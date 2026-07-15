package projection

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/openclaw/discrawl/internal/store"
)

type Config struct {
	GuildID               string
	PollEvery             time.Duration
	BindingsEvery         time.Duration
	RepairEvery           time.Duration
	RepairLookback        time.Duration
	InitialLookback       time.Duration
	InitialRowsPerBinding int
	BatchSize             int
	StatePath             string
	OperationTimeout      time.Duration
	StatusEvery           time.Duration
}

type Binding struct {
	ID        string
	GuildID   string
	ChannelID string
	ThreadID  string
}

func (b Binding) TargetID() string {
	if b.ThreadID != "" {
		return b.ThreadID
	}
	return b.ChannelID
}

type Archive interface {
	ListProjectionMessages(context.Context, string, store.ProjectionCursor, int) ([]store.ProjectionMessage, error)
	ListRecentProjectionMessages(context.Context, string, string, time.Time, int) ([]store.ProjectionMessage, error)
	ListProjectionTombstones(context.Context, string, time.Time, string, int) ([]store.ProjectionTombstone, error)
	ListRecentProjectionTombstones(context.Context, string, string, time.Time, int) ([]store.ProjectionTombstone, error)
}

type Sink interface {
	Bindings(context.Context, string) ([]Binding, error)
	ApplyMessages(context.Context, []Binding, []store.ProjectionMessage) (int, error)
	ApplyTombstones(context.Context, []Binding, []store.ProjectionTombstone) (int, error)
	SanitizeAttachmentURLs(context.Context, string, int) (string, int, bool, error)
	Status(context.Context, Status) error
	Close() error
}

type State struct {
	Version                    int               `json:"version"`
	Initialized                bool              `json:"initialized"`
	MessageUpdatedAt           time.Time         `json:"message_updated_at"`
	MessageID                  string            `json:"message_id"`
	TombstoneDeletedAt         time.Time         `json:"tombstone_deleted_at"`
	TombstoneMessageID         string            `json:"tombstone_message_id"`
	TombstoneSweepStarted      bool              `json:"tombstone_sweep_started,omitempty"`
	TombstoneSweepComplete     bool              `json:"tombstone_sweep_complete,omitempty"`
	AttachmentURLSweepCursor   string            `json:"attachment_url_sweep_cursor,omitempty"`
	AttachmentURLSweepComplete bool              `json:"attachment_url_sweep_complete,omitempty"`
	SeededBindings             map[string]string `json:"seeded_bindings,omitempty"`
	RepairUpdatedAt            time.Time         `json:"repair_updated_at,omitempty"`
	RepairMessageID            string            `json:"repair_message_id,omitempty"`
}

type Status struct {
	State                      string    `firestore:"state"`
	GuildID                    string    `firestore:"guildId"`
	TombstoneSweepComplete     bool      `firestore:"tombstoneSweepComplete"`
	AttachmentURLSweepComplete bool      `firestore:"attachmentUrlSweepComplete"`
	LastSuccessAt              time.Time `firestore:"lastSuccessAt,omitempty"`
	LastFailureAt              time.Time `firestore:"lastFailureAt"`
	FailureCode                string    `firestore:"failureCode"`
	BindingCount               int       `firestore:"bindingCount"`
	ProjectedChanges           int       `firestore:"projectedChanges"`
	SchemaVersion              int       `firestore:"schemaVersion"`
}

type Projector struct {
	cfg             Config
	archive         Archive
	sink            Sink
	logger          *slog.Logger
	now             func() time.Time
	state           State
	bindings        []Binding
	bindingsValid   bool
	lastStatusAt    time.Time
	lastStatusState string
}

const maxBatchesPerPass = 10

func New(cfg Config, archive Archive, sink Sink, logger *slog.Logger) (*Projector, error) {
	if archive == nil || sink == nil || cfg.GuildID == "" || cfg.BatchSize < 1 || cfg.BatchSize > 250 ||
		cfg.InitialRowsPerBinding < 1 || cfg.InitialRowsPerBinding > 250 ||
		cfg.PollEvery <= 0 || cfg.BindingsEvery < 5*time.Minute || cfg.RepairEvery < time.Hour ||
		cfg.RepairLookback <= 0 || cfg.InitialLookback <= 0 || cfg.OperationTimeout <= 0 || cfg.StatusEvery < 5*time.Minute {
		return nil, errors.New("invalid projection configuration")
	}
	if !filepath.IsAbs(cfg.StatePath) {
		return nil, errors.New("projection state path must be absolute")
	}
	if logger == nil {
		logger = slog.Default()
	}
	state, err := LoadState(cfg.StatePath)
	if err != nil {
		return nil, err
	}
	if state.Initialized && !state.TombstoneSweepStarted {
		// Upgrade an early projection state that incorrectly started at "now".
		// Restarting from zero is idempotent and closes the pre-cutover gap.
		state.TombstoneDeletedAt, state.TombstoneMessageID = time.Time{}, ""
		state.TombstoneSweepStarted = true
		if err := SaveState(cfg.StatePath, state); err != nil {
			return nil, err
		}
	}
	return &Projector{cfg: cfg, archive: archive, sink: sink, logger: logger, now: time.Now, state: state}, nil
}

func (p *Projector) Close() error { return p.sink.Close() }

func (p *Projector) Run(ctx context.Context) error {
	if err := p.refreshBindings(ctx); err != nil {
		return err
	}
	if !p.state.Initialized {
		if err := p.seed(ctx); err != nil {
			return err
		}
	} else if _, err := p.seedPendingBindings(ctx); err != nil {
		return err
	}
	poll := time.NewTicker(p.cfg.PollEvery)
	bindings := time.NewTicker(p.cfg.BindingsEvery)
	repair := time.NewTicker(p.cfg.RepairEvery)
	defer poll.Stop()
	defer bindings.Stop()
	defer repair.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-bindings.C:
			if err := p.refreshBindings(ctx); err != nil {
				p.reportFailure(ctx, "bindings", err)
			} else if _, err := p.seedPendingBindings(ctx); err != nil {
				p.reportFailure(ctx, "seed", err)
			}
		case <-repair.C:
			if err := p.repair(ctx); err != nil {
				p.reportFailure(ctx, "repair", err)
			}
		case <-poll.C:
			if err := p.projectDeltas(ctx); err != nil {
				p.reportFailure(ctx, "delta", err)
			}
		}
	}
}

func (p *Projector) refreshBindings(ctx context.Context) error {
	// Bindings are the authorization/scope control plane. A failed refresh
	// invalidates the snapshot so disabled or retargeted scopes cannot continue
	// receiving rows during a control-plane outage.
	p.bindings = nil
	p.bindingsValid = false
	opctx, cancel := p.operationContext(ctx)
	bindings, err := p.sink.Bindings(opctx, p.cfg.GuildID)
	cancel()
	if err != nil {
		return err
	}
	seenTargets := map[string]string{}
	for _, binding := range bindings {
		if binding.GuildID != p.cfg.GuildID || !validBindingID(binding.ID) || !validSnowflake(binding.ChannelID) ||
			(binding.ThreadID != "" && !validSnowflake(binding.ThreadID)) {
			return errors.New("projection binding escaped configured guild or is incomplete")
		}
		if previous, ok := seenTargets[binding.TargetID()]; ok {
			return fmt.Errorf("ambiguous projection target shared by bindings %s and %s", previous, binding.ID)
		}
		seenTargets[binding.TargetID()] = binding.ID
	}
	sort.Slice(bindings, func(i, j int) bool { return bindings[i].ID < bindings[j].ID })
	p.bindings = bindings
	p.bindingsValid = true
	if p.state.Initialized && len(p.state.SeededBindings) > 0 {
		active := make(map[string]struct{}, len(bindings))
		for _, binding := range bindings {
			active[binding.ID] = struct{}{}
		}
		changed := false
		for id := range p.state.SeededBindings {
			if _, ok := active[id]; !ok {
				delete(p.state.SeededBindings, id)
				changed = true
			}
		}
		if changed {
			return SaveState(p.cfg.StatePath, p.state)
		}
	}
	return nil
}

func (p *Projector) seed(ctx context.Context) error {
	started := p.now().UTC()
	p.state = State{
		Version: 1, Initialized: true, MessageUpdatedAt: started,
		// Deletions are never horizon-bounded: the global cursor exhaustively
		// sweeps legacy tombstones after startup so disabled/pre-cutover bindings
		// cannot retain a stale body in Firestore.
		TombstoneDeletedAt: time.Time{}, TombstoneSweepStarted: true,
		TombstoneSweepComplete: false, SeededBindings: map[string]string{},
	}
	changes, err := p.seedPendingBindings(ctx)
	if err != nil {
		return err
	}
	if err := SaveState(p.cfg.StatePath, p.state); err != nil {
		return err
	}
	return p.emitStatus(ctx, Status{State: "seeding", LastSuccessAt: p.now(), BindingCount: len(p.bindings), ProjectedChanges: changes, SchemaVersion: 2}, true)
}

func (p *Projector) seedPendingBindings(ctx context.Context) (int, error) {
	if p.state.SeededBindings == nil {
		p.state.SeededBindings = map[string]string{}
	}
	changes := 0
	cutoff := p.now().UTC().Add(-p.cfg.InitialLookback)
	for _, binding := range p.bindings {
		fingerprint := binding.GuildID + "/" + binding.ChannelID + "/" + binding.ThreadID
		if p.state.SeededBindings[binding.ID] == fingerprint {
			continue
		}
		opctx, cancel := p.operationContext(ctx)
		messages, err := p.archive.ListRecentProjectionMessages(opctx, p.cfg.GuildID, binding.TargetID(), cutoff, p.cfg.InitialRowsPerBinding)
		cancel()
		if err != nil {
			return changes, err
		}
		opctx, cancel = p.operationContext(ctx)
		changed, err := p.sink.ApplyMessages(opctx, []Binding{binding}, messages)
		cancel()
		if err != nil {
			return changes, err
		}
		changes += changed
		opctx, cancel = p.operationContext(ctx)
		tombstones, err := p.archive.ListRecentProjectionTombstones(opctx, p.cfg.GuildID, binding.TargetID(), cutoff, p.cfg.InitialRowsPerBinding)
		cancel()
		if err != nil {
			return changes, err
		}
		opctx, cancel = p.operationContext(ctx)
		changed, err = p.sink.ApplyTombstones(opctx, []Binding{binding}, tombstones)
		cancel()
		if err != nil {
			return changes, err
		}
		changes += changed
		p.state.SeededBindings[binding.ID] = fingerprint
		if err := SaveState(p.cfg.StatePath, p.state); err != nil {
			return changes, err
		}
	}
	return changes, nil
}

func (p *Projector) projectDeltas(ctx context.Context) error {
	if !p.bindingsValid {
		return errors.New("projection bindings unavailable")
	}
	changes := 0
	tombstoneExhausted := false
	for batch := 0; batch < maxBatchesPerPass; batch++ {
		opctx, cancel := p.operationContext(ctx)
		rows, err := p.archive.ListProjectionMessages(opctx, p.cfg.GuildID, store.ProjectionCursor{
			UpdatedAt: p.state.MessageUpdatedAt, MessageID: p.state.MessageID,
		}, p.cfg.BatchSize)
		cancel()
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			break
		}
		opctx, cancel = p.operationContext(ctx)
		changed, err := p.sink.ApplyMessages(opctx, p.bindings, rows)
		cancel()
		if err != nil {
			return err
		}
		changes += changed
		last := rows[len(rows)-1]
		p.state.MessageUpdatedAt, p.state.MessageID = last.UpdatedAt, last.MessageID
		if err := SaveState(p.cfg.StatePath, p.state); err != nil {
			return err
		}
		if len(rows) < p.cfg.BatchSize {
			break
		}
	}
	for batch := 0; batch < maxBatchesPerPass; batch++ {
		opctx, cancel := p.operationContext(ctx)
		rows, err := p.archive.ListProjectionTombstones(opctx, p.cfg.GuildID, p.state.TombstoneDeletedAt, p.state.TombstoneMessageID, p.cfg.BatchSize)
		cancel()
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			tombstoneExhausted = true
			break
		}
		opctx, cancel = p.operationContext(ctx)
		changed, err := p.sink.ApplyTombstones(opctx, p.bindings, rows)
		cancel()
		if err != nil {
			return err
		}
		changes += changed
		last := rows[len(rows)-1]
		p.state.TombstoneDeletedAt, p.state.TombstoneMessageID = last.DeletedAt, last.MessageID
		if err := SaveState(p.cfg.StatePath, p.state); err != nil {
			return err
		}
		if len(rows) < p.cfg.BatchSize {
			tombstoneExhausted = true
			break
		}
	}
	if tombstoneExhausted && !p.state.TombstoneSweepComplete {
		p.state.TombstoneSweepComplete = true
		if err := SaveState(p.cfg.StatePath, p.state); err != nil {
			return err
		}
	}
	if !p.state.AttachmentURLSweepComplete {
		for batch := 0; batch < maxBatchesPerPass; batch++ {
			opctx, cancel := p.operationContext(ctx)
			next, changed, complete, err := p.sink.SanitizeAttachmentURLs(
				opctx, p.state.AttachmentURLSweepCursor, p.cfg.BatchSize,
			)
			cancel()
			if err != nil {
				return err
			}
			changes += changed
			if !complete && (next == "" || next == p.state.AttachmentURLSweepCursor) {
				return errors.New("attachment URL sanitation sweep did not advance")
			}
			p.state.AttachmentURLSweepCursor = next
			p.state.AttachmentURLSweepComplete = complete
			if err := SaveState(p.cfg.StatePath, p.state); err != nil {
				return err
			}
			if complete {
				break
			}
		}
	}
	statusState := "seeding"
	if p.state.TombstoneSweepComplete && p.state.AttachmentURLSweepComplete {
		statusState = "healthy"
	}
	return p.emitStatus(ctx, Status{State: statusState, LastSuccessAt: p.now(), BindingCount: len(p.bindings), ProjectedChanges: changes, SchemaVersion: 2}, false)
}

func (p *Projector) repair(ctx context.Context) error {
	if !p.bindingsValid {
		return errors.New("projection bindings unavailable")
	}
	lookback := p.now().Add(-p.cfg.RepairLookback)
	cursor := store.ProjectionCursor{UpdatedAt: p.state.RepairUpdatedAt, MessageID: p.state.RepairMessageID}
	if cursor.UpdatedAt.IsZero() || cursor.UpdatedAt.Before(lookback) {
		cursor = store.ProjectionCursor{UpdatedAt: lookback}
	}
	for batch := 0; batch < maxBatchesPerPass; batch++ {
		opctx, cancel := p.operationContext(ctx)
		rows, err := p.archive.ListProjectionMessages(opctx, p.cfg.GuildID, cursor, p.cfg.BatchSize)
		cancel()
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			p.state.RepairUpdatedAt, p.state.RepairMessageID = time.Time{}, ""
			return SaveState(p.cfg.StatePath, p.state)
		}
		opctx, cancel = p.operationContext(ctx)
		_, err = p.sink.ApplyMessages(opctx, p.bindings, rows)
		cancel()
		if err != nil {
			return err
		}
		last := rows[len(rows)-1]
		cursor = store.ProjectionCursor{UpdatedAt: last.UpdatedAt, MessageID: last.MessageID}
		p.state.RepairUpdatedAt, p.state.RepairMessageID = cursor.UpdatedAt, cursor.MessageID
		if err := SaveState(p.cfg.StatePath, p.state); err != nil {
			return err
		}
		if len(rows) < p.cfg.BatchSize {
			p.state.RepairUpdatedAt, p.state.RepairMessageID = time.Time{}, ""
			return SaveState(p.cfg.StatePath, p.state)
		}
	}
	return nil
}

func (p *Projector) reportFailure(ctx context.Context, code string, cause error) {
	p.logger.Error("projection pass failed", "code", code, "error", cause)
	_ = p.emitStatus(ctx, Status{State: "degraded", LastFailureAt: p.now(), FailureCode: code, BindingCount: len(p.bindings), SchemaVersion: 2}, false)
}

func (p *Projector) operationContext(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, p.cfg.OperationTimeout)
}

func (p *Projector) emitStatus(ctx context.Context, status Status, force bool) error {
	status.GuildID = p.cfg.GuildID
	status.TombstoneSweepComplete = p.state.TombstoneSweepComplete
	status.AttachmentURLSweepComplete = p.state.AttachmentURLSweepComplete
	now := p.now()
	if !force && status.State == p.lastStatusState && !p.lastStatusAt.IsZero() && now.Sub(p.lastStatusAt) < p.cfg.StatusEvery {
		return nil
	}
	opctx, cancel := p.operationContext(ctx)
	err := p.sink.Status(opctx, status)
	cancel()
	if err == nil {
		p.lastStatusAt, p.lastStatusState = now, status.State
	}
	return err
}

func validBindingID(value string) bool {
	if len(value) < 1 || len(value) > 80 || value[0] == '-' || value[0] == '_' {
		return false
	}
	for _, char := range value {
		if !((char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') ||
			(char >= '0' && char <= '9') || char == '-' || char == '_') {
			return false
		}
	}
	return true
}

func validSnowflake(value string) bool {
	if len(value) < 17 || len(value) > 20 {
		return false
	}
	for _, char := range value {
		if char < '0' || char > '9' {
			return false
		}
	}
	return true
}

func LoadState(path string) (State, error) {
	body, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return State{Version: 1}, nil
	}
	if err != nil {
		return State{}, err
	}
	var state State
	if err := json.Unmarshal(body, &state); err != nil {
		return State{}, fmt.Errorf("parse projection state: %w", err)
	}
	if state.Version != 1 {
		return State{}, errors.New("unsupported projection state version")
	}
	return state, nil
}

func SaveState(path string, state State) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	body, err := json.Marshal(state)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".projection-state-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	directory, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer func() { _ = directory.Close() }()
	return directory.Sync()
}
