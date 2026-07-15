package projection

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/openclaw/discrawl/internal/store"
	"github.com/stretchr/testify/require"
)

const (
	testGuild   = "11111111111111111"
	testChannel = "22222222222222222"
)

type fakeArchive struct {
	recentCalls int
	messageFn   func(store.ProjectionCursor, int) []store.ProjectionMessage
	tombstoneFn func(time.Time, string, int) []store.ProjectionTombstone
}

func (f *fakeArchive) ListProjectionMessages(_ context.Context, _ string, cursor store.ProjectionCursor, limit int) ([]store.ProjectionMessage, error) {
	if f.messageFn == nil {
		return nil, nil
	}
	return f.messageFn(cursor, limit), nil
}
func (f *fakeArchive) ListRecentProjectionMessages(context.Context, string, string, time.Time, int) ([]store.ProjectionMessage, error) {
	f.recentCalls++
	return nil, nil
}
func (f *fakeArchive) ListProjectionTombstones(_ context.Context, _ string, after time.Time, id string, limit int) ([]store.ProjectionTombstone, error) {
	if f.tombstoneFn == nil {
		return nil, nil
	}
	return f.tombstoneFn(after, id, limit), nil
}
func (f *fakeArchive) ListRecentProjectionTombstones(context.Context, string, string, time.Time, int) ([]store.ProjectionTombstone, error) {
	return nil, nil
}

type fakeSink struct {
	bindings                                                 []Binding
	bindingsErr                                              error
	messageCalls, tombstoneCalls, sanitizeCalls, statusCalls int
	statuses                                                 []Status
	sanitizeFn                                               func(string, int) (string, int, bool, error)
}

func (s *fakeSink) Bindings(context.Context, string) ([]Binding, error) {
	if s.bindingsErr != nil {
		return nil, s.bindingsErr
	}
	return append([]Binding(nil), s.bindings...), nil
}
func (s *fakeSink) ApplyMessages(_ context.Context, _ []Binding, _ []store.ProjectionMessage) (int, error) {
	s.messageCalls++
	return 0, nil
}
func (s *fakeSink) ApplyTombstones(_ context.Context, _ []Binding, rows []store.ProjectionTombstone) (int, error) {
	s.tombstoneCalls++
	return len(rows), nil
}
func (s *fakeSink) SanitizeAttachmentURLs(_ context.Context, cursor string, limit int) (string, int, bool, error) {
	s.sanitizeCalls++
	if s.sanitizeFn != nil {
		return s.sanitizeFn(cursor, limit)
	}
	return cursor, 0, true, nil
}
func (s *fakeSink) Status(_ context.Context, status Status) error {
	s.statusCalls++
	s.statuses = append(s.statuses, status)
	return nil
}
func (s *fakeSink) Close() error { return nil }

func testConfig(t *testing.T) Config {
	return Config{
		GuildID: testGuild, PollEvery: time.Second, BindingsEvery: 5 * time.Minute,
		RepairEvery: time.Hour, RepairLookback: 24 * time.Hour, InitialLookback: 30 * 24 * time.Hour,
		InitialRowsPerBinding: 250, BatchSize: 1, StatePath: filepath.Join(t.TempDir(), "projection.json"),
		OperationTimeout: time.Second, StatusEvery: 5 * time.Minute,
	}
}

func TestDisabledThenReenabledBindingIsSeededAgain(t *testing.T) {
	archive, sink := &fakeArchive{}, &fakeSink{}
	p, err := New(testConfig(t), archive, sink, slog.Default())
	require.NoError(t, err)
	p.state = State{Version: 1, Initialized: true, SeededBindings: map[string]string{"general": testGuild + "/" + testChannel + "/"}}
	require.NoError(t, p.refreshBindings(context.Background()))
	require.Empty(t, p.state.SeededBindings)
	sink.bindings = []Binding{{ID: "general", GuildID: testGuild, ChannelID: testChannel}}
	require.NoError(t, p.refreshBindings(context.Background()))
	_, err = p.seedPendingBindings(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, archive.recentCalls)
}

func TestBindingRefreshFailureSuspendsProjectionScope(t *testing.T) {
	archive := &fakeArchive{}
	sink := &fakeSink{bindingsErr: errors.New("control plane unavailable")}
	p, err := New(testConfig(t), archive, sink, slog.Default())
	require.NoError(t, err)
	p.bindings = []Binding{{ID: "stale", GuildID: testGuild, ChannelID: testChannel}}
	p.bindingsValid = true
	require.Error(t, p.refreshBindings(context.Background()))
	require.False(t, p.bindingsValid)
	require.Empty(t, p.bindings)
	require.ErrorContains(t, p.projectDeltas(context.Background()), "bindings unavailable")
}

func TestBusyMessageStreamStillProcessesTombstones(t *testing.T) {
	now := time.Now().UTC()
	archive := &fakeArchive{
		messageFn: func(cursor store.ProjectionCursor, _ int) []store.ProjectionMessage {
			return []store.ProjectionMessage{{ArchiveMessage: store.ArchiveMessage{MessageID: "33333333333333333", GuildID: testGuild, ChannelID: testChannel}, UpdatedAt: cursor.UpdatedAt.Add(time.Nanosecond)}}
		},
		tombstoneFn: func(_ time.Time, id string, _ int) []store.ProjectionTombstone {
			if id != "" {
				return nil
			}
			return []store.ProjectionTombstone{{MessageID: "44444444444444444", GuildID: testGuild, ChannelID: testChannel, DeletedAt: now}}
		},
	}
	sink := &fakeSink{bindings: []Binding{{ID: "general", GuildID: testGuild, ChannelID: testChannel}}}
	p, err := New(testConfig(t), archive, sink, slog.Default())
	require.NoError(t, err)
	p.bindings, p.bindingsValid, p.state = sink.bindings, true, State{Version: 1, Initialized: true}
	require.NoError(t, p.projectDeltas(context.Background()))
	require.Equal(t, maxBatchesPerPass, sink.messageCalls)
	require.GreaterOrEqual(t, sink.tombstoneCalls, 1)
}

func TestRepairCursorEventuallyCoversBeyondPerPassCap(t *testing.T) {
	now := time.Now().UTC()
	archive := &fakeArchive{messageFn: func(cursor store.ProjectionCursor, _ int) []store.ProjectionMessage {
		current, _ := strconv.Atoi(cursor.MessageID)
		if current >= 25 {
			return nil
		}
		id := current + 1
		return []store.ProjectionMessage{{ArchiveMessage: store.ArchiveMessage{MessageID: fmt.Sprint(id), GuildID: testGuild, ChannelID: testChannel}, UpdatedAt: now}}
	}}
	sink := &fakeSink{}
	p, err := New(testConfig(t), archive, sink, slog.Default())
	require.NoError(t, err)
	p.now = func() time.Time { return now }
	p.state = State{Version: 1, Initialized: true}
	p.bindingsValid = true
	for i := 0; i < 3; i++ {
		require.NoError(t, p.repair(context.Background()))
	}
	require.Equal(t, 25, sink.messageCalls)
	require.True(t, p.state.RepairUpdatedAt.IsZero())
}

func TestEmptyPollsRespectStatusHeartbeatBudget(t *testing.T) {
	archive, sink := &fakeArchive{}, &fakeSink{}
	p, err := New(testConfig(t), archive, sink, slog.Default())
	require.NoError(t, err)
	now := time.Now().UTC()
	p.now = func() time.Time { return now }
	p.lastStatusAt, p.lastStatusState = now, "healthy"
	p.bindingsValid = true
	for i := 0; i < 100; i++ {
		require.NoError(t, p.projectDeltas(context.Background()))
	}
	require.Zero(t, sink.statusCalls)
	now = now.Add(5 * time.Minute)
	require.NoError(t, p.projectDeltas(context.Background()))
	require.Equal(t, 1, sink.statusCalls)
}

func TestProjectionFailureLogIncludesUnderlyingCause(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	p, err := New(testConfig(t), &fakeArchive{}, &fakeSink{}, logger)
	require.NoError(t, err)

	p.reportFailure(context.Background(), "delta", errors.New("firestore transaction denied"))

	require.Contains(t, logs.String(), `"msg":"projection pass failed"`)
	require.Contains(t, logs.String(), `"code":"delta"`)
	require.Contains(t, logs.String(), `"error":"firestore transaction denied"`)
}

func TestTombstoneSweepGatesHealthyStatusUntilExhausted(t *testing.T) {
	remaining := maxBatchesPerPass
	archive := &fakeArchive{tombstoneFn: func(after time.Time, _ string, _ int) []store.ProjectionTombstone {
		if remaining == 0 {
			return nil
		}
		remaining--
		return []store.ProjectionTombstone{{
			MessageID: fmt.Sprintf("%017d", 50000000000000000+remaining),
			GuildID:   testGuild, ChannelID: testChannel, DeletedAt: after.Add(time.Nanosecond),
		}}
	}}
	sink := &fakeSink{}
	p, err := New(testConfig(t), archive, sink, slog.Default())
	require.NoError(t, err)
	p.bindingsValid = true
	p.state = State{Version: 1, Initialized: true, TombstoneSweepStarted: true}
	require.NoError(t, p.projectDeltas(context.Background()))
	require.False(t, p.state.TombstoneSweepComplete)
	require.Equal(t, "seeding", sink.statuses[len(sink.statuses)-1].State)
	require.NoError(t, p.projectDeltas(context.Background()))
	require.True(t, p.state.TombstoneSweepComplete)
	require.Equal(t, "healthy", sink.statuses[len(sink.statuses)-1].State)
}

func TestAttachmentURLSweepIsCursorBackedAndGatesHealthyStatus(t *testing.T) {
	call := 0
	sink := &fakeSink{sanitizeFn: func(cursor string, _ int) (string, int, bool, error) {
		call++
		if call <= maxBatchesPerPass {
			return fmt.Sprintf("%017d", 60000000000000000+call), 1, false, nil
		}
		return cursor, 0, true, nil
	}}
	p, err := New(testConfig(t), &fakeArchive{}, sink, slog.Default())
	require.NoError(t, err)
	p.bindingsValid = true
	p.state = State{Version: 1, Initialized: true, TombstoneSweepStarted: true, TombstoneSweepComplete: true}
	require.NoError(t, p.projectDeltas(context.Background()))
	require.False(t, p.state.AttachmentURLSweepComplete)
	require.NotEmpty(t, p.state.AttachmentURLSweepCursor)
	require.Equal(t, "seeding", sink.statuses[len(sink.statuses)-1].State)
	require.NoError(t, p.projectDeltas(context.Background()))
	require.True(t, p.state.AttachmentURLSweepComplete)
	require.Equal(t, "healthy", sink.statuses[len(sink.statuses)-1].State)
}

func TestLegacyProjectionStateRestartsTombstoneSweepFromZero(t *testing.T) {
	cfg := testConfig(t)
	legacyCursor := time.Now().UTC()
	require.NoError(t, SaveState(cfg.StatePath, State{
		Version: 1, Initialized: true, TombstoneDeletedAt: legacyCursor,
		TombstoneMessageID: "99999999999999999",
	}))
	p, err := New(cfg, &fakeArchive{}, &fakeSink{}, slog.Default())
	require.NoError(t, err)
	require.True(t, p.state.TombstoneSweepStarted)
	require.False(t, p.state.TombstoneSweepComplete)
	require.True(t, p.state.TombstoneDeletedAt.IsZero())
	require.Empty(t, p.state.TombstoneMessageID)
	reloaded, err := LoadState(cfg.StatePath)
	require.NoError(t, err)
	require.True(t, reloaded.TombstoneDeletedAt.IsZero())
}

func TestProjectionFingerprintCanonicalizesFirestoreArrayShapes(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Microsecond)
	message := store.ProjectionMessage{ArchiveMessage: store.ArchiveMessage{
		MessageID: "33333333333333333", GuildID: testGuild, ChannelID: testChannel,
		Content: "hello", CreatedAt: now, Attachments: []store.ArchiveAttachment{{
			ID: "a", Filename: "a.txt", URL: "https://cdn.discordapp.com/attachments/c/m/a.txt?ex=secret",
			ProxyURL: "https://media.discordapp.net/attachments/c/m/a.txt?hm=secret",
		}},
	}, UpdatedAt: now}
	doc := projectionDocument(Binding{ID: "general", GuildID: testGuild, ChannelID: testChannel}, message, nil)
	roundTripped := cloneMap(doc)
	roundTripped["attachments"] = []any{map[string]any{"id": "a", "filename": "a.txt", "contentType": "", "size": int64(0)}}
	equal, err := mapsEqual(doc, roundTripped)
	require.NoError(t, err)
	require.True(t, equal)
	descriptors := doc["attachments"].([]map[string]any)
	require.Len(t, descriptors, 1)
	require.NotContains(t, descriptors[0], "url")
	require.NotContains(t, descriptors[0], "proxyUrl")

	legacy := cloneMap(doc)
	legacy["attachments"] = []any{map[string]any{
		"id": "a", "filename": "a.txt", "contentType": "", "size": int64(0),
		"url":      "https://cdn.discordapp.com/attachments/c/m/a.txt?ex=secret",
		"proxyUrl": "https://media.discordapp.net/attachments/c/m/a.txt?hm=secret",
	}}
	equal, err = mapsEqual(doc, legacy)
	require.NoError(t, err)
	require.False(t, equal, "replay must replace historical projected CDN URLs")
}

func TestProjectionFingerprintFailsClosedOnUnsupportedFirestoreValue(t *testing.T) {
	existing := map[string]any{
		"authorPhoto": math.NaN(),
		"content":     "must be deleted",
		"deleted":     false,
	}
	deleted := cloneMap(existing)
	deleted["content"] = ""
	deleted["deleted"] = true

	equal, err := mapsEqual(existing, deleted)
	require.Error(t, err)
	require.False(t, equal)
}

func TestSanitizeProjectedAttachmentsRemovesEveryURLVariantAndPreservesMetadata(t *testing.T) {
	raw := []any{map[string]any{
		"id": "33333333333333333", "filename": "report.pdf", "contentType": "application/pdf",
		"size": int64(42), "width": int64(100), "height": int64(200),
		"url": "secret-a", "proxyUrl": "secret-b", "proxy_url": "secret-c", "proxyURL": "secret-d",
	}}
	sanitized, changed := sanitizeProjectedAttachments(raw)
	require.True(t, changed)
	rows := sanitized.([]any)
	require.Equal(t, map[string]any{
		"id": "33333333333333333", "filename": "report.pdf", "contentType": "application/pdf",
		"size": int64(42), "width": int64(100), "height": int64(200),
	}, rows[0])
	replayed, changed := sanitizeProjectedAttachments(sanitized)
	require.False(t, changed)
	require.Equal(t, sanitized, replayed)

	direct, changed := sanitizeProjectedAttachments(map[string]any{"id": "a", "filename": "a.txt", "url": "secret"})
	require.True(t, changed)
	require.Equal(t, map[string]any{"id": "a", "filename": "a.txt"}, direct)
	keyed, changed := sanitizeProjectedAttachments(map[string]any{
		"one": map[string]any{"id": "a", "proxyURL": "secret", "nested": map[string]any{"proxy_url": "secret"}},
	})
	require.True(t, changed)
	require.Equal(t, map[string]any{"one": map[string]any{"id": "a", "nested": map[string]any{}}}, keyed)
}

func TestAttachmentSweepCursorIsOpaqueAndAcceptsLegacyWhitespaceIDs(t *testing.T) {
	require.True(t, validAttachmentSweepCursor("  legacy document id  "))
	require.False(t, validAttachmentSweepCursor("nested/document"))
	require.False(t, validAttachmentSweepCursor(strings.Repeat("x", 1501)))
}

func TestTombstoneUsesExistingScopeWithoutActiveBindingAndKeepsIdentity(t *testing.T) {
	now := time.Now().UTC()
	existing := map[string]any{
		"bindingId": "disabled", "guildId": testGuild, "channelId": testChannel, "threadId": "",
		"content": "must disappear", "attachments": []any{map[string]any{"url": "private"}},
		"websiteOrigin": true, "authorName": "Brendan", "actorUid": "private-uid",
	}
	candidate, bindingID, ok := tombstoneDocument(existing, store.ProjectionTombstone{
		MessageID: "33333333333333333", GuildID: testGuild, ChannelID: testChannel, DeletedAt: now,
	})
	require.True(t, ok)
	require.Equal(t, "disabled", bindingID)
	require.Empty(t, candidate["content"])
	require.Empty(t, candidate["attachments"])
	require.Equal(t, "Brendan", candidate["authorName"])
	require.Equal(t, "private-uid", candidate["actorUid"])
}

func TestProjectionNeverResurrectsExistingOrDurableTombstone(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Microsecond)
	deletedAt := now.Add(-time.Hour)
	message := store.ProjectionMessage{ArchiveMessage: store.ArchiveMessage{
		MessageID: "33333333333333333", GuildID: testGuild, ChannelID: testChannel,
		Content: "restored stale body", CreatedAt: now,
	}, UpdatedAt: now}
	binding := Binding{ID: "general", GuildID: testGuild, ChannelID: testChannel}
	existing := map[string]any{
		"deleted": true, "deletedAt": deletedAt, "updatedAt": deletedAt,
		"websiteOrigin": true, "authorName": "Website Identity",
	}
	doc := projectionDocument(binding, message, existing)
	require.Equal(t, true, doc["deleted"])
	require.Empty(t, doc["content"])
	require.Empty(t, doc["attachments"])
	require.Equal(t, deletedAt, doc["deletedAt"])
	require.Equal(t, "Website Identity", doc["authorName"])

	ledger, changed, err := durableTombstoneDocument(nil, store.ProjectionTombstone{
		MessageID: message.MessageID, GuildID: testGuild, ChannelID: testChannel, DeletedAt: deletedAt,
	})
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, map[string]any{
		"discordMessageId": message.MessageID, "guildId": testGuild,
		"channelId": testChannel, "deletedAt": deletedAt,
	}, ledger)
	replayed := preserveTerminalDelete(projectionDocument(binding, message, nil), nil, ledger)
	require.Equal(t, true, replayed["deleted"])
	require.Empty(t, replayed["content"])
	require.Empty(t, replayed["attachments"])
}

func TestDurableTombstoneIsAppendOnlyScopeCheckedAndChronologicallyEarliest(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Microsecond)
	existing := map[string]any{
		"discordMessageId": "33333333333333333", "guildId": testGuild,
		"channelId": testChannel, "deletedAt": now,
	}
	row := store.ProjectionTombstone{
		MessageID: "33333333333333333", GuildID: testGuild, ChannelID: testChannel, DeletedAt: now.Add(time.Hour),
	}
	ledger, changed, err := durableTombstoneDocument(existing, row)
	require.NoError(t, err)
	require.False(t, changed)
	require.Equal(t, now, ledger["deletedAt"])
	row.ChannelID = "99999999999999999"
	_, _, err = durableTombstoneDocument(existing, row)
	require.ErrorContains(t, err, "scope mismatch")
	row = store.ProjectionTombstone{MessageID: "bad/id", GuildID: testGuild, ChannelID: testChannel, DeletedAt: now}
	_, _, err = durableTombstoneDocument(nil, row)
	require.ErrorContains(t, err, "invalid durable tombstone")
	row = store.ProjectionTombstone{MessageID: "33333333333333333", GuildID: testGuild, ChannelID: testChannel}
	_, _, err = durableTombstoneDocument(nil, row)
	require.ErrorContains(t, err, "invalid durable tombstone")
}

func TestNewRejectsUnsafeCadenceAndOversizedSeed(t *testing.T) {
	cfg := testConfig(t)
	cfg.BindingsEvery = time.Second
	_, err := New(cfg, &fakeArchive{}, &fakeSink{}, nil)
	require.Error(t, err)
	cfg = testConfig(t)
	cfg.InitialRowsPerBinding = 251
	_, err = New(cfg, &fakeArchive{}, &fakeSink{}, nil)
	require.Error(t, err)
}
