package projection

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"strconv"
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
	bindings                                  []Binding
	messageCalls, tombstoneCalls, statusCalls int
}

func (s *fakeSink) Bindings(context.Context, string) ([]Binding, error) {
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
func (s *fakeSink) Status(context.Context, Status) error { s.statusCalls++; return nil }
func (s *fakeSink) Close() error                         { return nil }

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
	p.bindings, p.state = sink.bindings, State{Version: 1, Initialized: true}
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
	for i := 0; i < 100; i++ {
		require.NoError(t, p.projectDeltas(context.Background()))
	}
	require.Zero(t, sink.statusCalls)
	now = now.Add(5 * time.Minute)
	require.NoError(t, p.projectDeltas(context.Background()))
	require.Equal(t, 1, sink.statusCalls)
}

func TestProjectionFingerprintCanonicalizesFirestoreArrayShapes(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Microsecond)
	message := store.ProjectionMessage{ArchiveMessage: store.ArchiveMessage{
		MessageID: "33333333333333333", GuildID: testGuild, ChannelID: testChannel,
		Content: "hello", CreatedAt: now, Attachments: []store.ArchiveAttachment{{ID: "a", Filename: "a.txt", URL: "https://cdn.discordapp.com/attachments/c/m/a.txt"}},
	}, UpdatedAt: now}
	doc := projectionDocument(Binding{ID: "general", GuildID: testGuild, ChannelID: testChannel}, message, nil)
	roundTripped := cloneMap(doc)
	roundTripped["attachments"] = []any{map[string]any{"id": "a", "filename": "a.txt", "contentType": "", "size": int64(0), "url": "https://cdn.discordapp.com/attachments/c/m/a.txt", "proxyUrl": ""}}
	require.True(t, mapsEqual(doc, roundTripped))
	require.Empty(t, safeHTTPSURL("https://tracker.example/attachments/c/m/a.txt"))
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
