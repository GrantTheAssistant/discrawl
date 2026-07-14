package store

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const (
	testGuildA   = "11111111111111111"
	testGuildB   = "22222222222222222"
	testChannelA = "33333333333333333"
	testChannelB = "44444444444444444"
)

func openArchiveServiceStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(context.Background(), filepath.Join(t.TempDir(), "archive.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	require.NoError(t, s.UpsertGuild(context.Background(), GuildRecord{
		ID: testGuildA, Name: "Guild A",
		RawJSON: `{"roles":[{"id":"role-low","color":255,"position":1},{"id":"role-high","color":16711680,"position":9}]}`,
	}))
	require.NoError(t, s.UpsertChannel(context.Background(), ChannelRecord{
		ID: testChannelA, GuildID: testGuildA, Kind: "text", Name: "general", RawJSON: `{}`,
	}))
	return s
}

func archiveRecord(id, channel, content string) MessageRecord {
	now := time.Now().UTC()
	return MessageRecord{
		ID: id, GuildID: testGuildA, ChannelID: channel, AuthorID: "55555555555555555",
		AuthorName: "Author", ChannelName: "general", Content: content,
		NormalizedContent: content, CreatedAt: now.Format(time.RFC3339Nano),
		RawJSON: `{"author":{"id":"55555555555555555","username":"Author","avatar":"avatar","bot":true},"member":{"nick":"Author","roles":["role-low","role-high"]}}`,
	}
}

func TestArchiveTombstoneNeverReturnsBodyOrAttachmentsButKeepsIdentity(t *testing.T) {
	ctx := context.Background()
	s := openArchiveServiceStore(t)
	id := "66666666666666666"
	require.NoError(t, s.UpsertMessages(ctx, []MessageMutation{{
		Record: archiveRecord(id, testChannelA, "private body"),
		Attachments: []AttachmentRecord{{
			AttachmentID: "77777777777777777", MessageID: id, GuildID: testGuildA,
			ChannelID: testChannelA, Filename: "secret.txt", URL: "https://cdn.discordapp.com/attachments/c/f",
		}},
	}}))
	require.NoError(t, s.MarkMessageDeleted(ctx, testGuildA, testChannelA, id, map[string]any{"content": "private body"}))

	page, err := s.ListArchiveMessages(ctx, ArchivePageOptions{
		GuildID: testGuildA, ChannelID: testChannelA, Limit: 10, ExposeAttachmentURLs: true,
	})
	require.NoError(t, err)
	require.Len(t, page.Messages, 1)
	message := page.Messages[0]
	require.True(t, message.Deleted)
	require.Empty(t, message.Content)
	require.Empty(t, message.Attachments)
	require.Equal(t, "Author", message.AuthorName)
	require.True(t, message.Bot)
	require.Equal(t, "#ff0000", message.RoleColor)

	projected, err := s.ListProjectionMessages(ctx, testGuildA, ProjectionCursor{}, 10)
	require.NoError(t, err)
	require.Len(t, projected, 1)
	require.True(t, projected[0].Deleted)
	require.Empty(t, projected[0].Content)
	require.Empty(t, projected[0].Attachments)
	require.Equal(t, "Author", projected[0].AuthorName)
}

func TestArchiveAndProjectionNeverRenderNormalizedAttachmentTextAsBody(t *testing.T) {
	ctx := context.Background()
	s := openArchiveServiceStore(t)
	record := archiveRecord("66666666666666665", testChannelA, "")
	record.NormalizedContent = "secret filename.txt\nextracted attachment contents"
	require.NoError(t, s.UpsertMessage(ctx, record))
	page, err := s.ListArchiveMessages(ctx, ArchivePageOptions{GuildID: testGuildA, ChannelID: testChannelA, Limit: 10})
	require.NoError(t, err)
	require.Len(t, page.Messages, 1)
	require.Empty(t, page.Messages[0].Content)
	projected, err := s.ListProjectionMessages(ctx, testGuildA, ProjectionCursor{}, 10)
	require.NoError(t, err)
	require.Len(t, projected, 1)
	require.Empty(t, projected[0].Content)
}

func TestDeleteBeforeCreateRemainsTombstonedAndUnsearchable(t *testing.T) {
	ctx := context.Background()
	s := openArchiveServiceStore(t)
	id := "66666666666666667"
	require.NoError(t, s.MarkMessageDeleted(ctx, testGuildA, testChannelA, id, map[string]any{"bulk": true}))
	require.NoError(t, s.UpsertMessages(ctx, []MessageMutation{{
		Record:      archiveRecord(id, testChannelA, "must never surface"),
		Attachments: []AttachmentRecord{{AttachmentID: "77777777777777778", MessageID: id, GuildID: testGuildA, ChannelID: testChannelA, Filename: "retained.txt"}},
	}}))

	results, err := s.SearchMessages(ctx, SearchOptions{Query: "surface", GuildIDs: []string{testGuildA}, ChannelIDExact: testChannelA, Limit: 10})
	require.NoError(t, err)
	require.Empty(t, results)
	projected, err := s.ListProjectionMessages(ctx, testGuildA, ProjectionCursor{}, 10)
	require.NoError(t, err)
	require.Len(t, projected, 1)
	require.True(t, projected[0].Deleted)
	require.Empty(t, projected[0].Content)
	require.Empty(t, projected[0].Attachments)
	tombstones, err := s.ListProjectionTombstones(ctx, testGuildA, time.Time{}, "", 10)
	require.NoError(t, err)
	require.Len(t, tombstones, 1)
	require.Equal(t, id, tombstones[0].MessageID)
}

func TestArchiveExactChannelSearchCannotMatchAnotherChannelName(t *testing.T) {
	ctx := context.Background()
	s := openArchiveServiceStore(t)
	require.NoError(t, s.UpsertChannel(ctx, ChannelRecord{
		ID: testChannelB, GuildID: testGuildA, Kind: "text", Name: "private-" + testChannelA, RawJSON: `{}`,
	}))
	require.NoError(t, s.UpsertMessage(ctx, archiveRecord("66666666666666668", testChannelA, "shared sentinel")))
	require.NoError(t, s.UpsertMessage(ctx, archiveRecord("66666666666666669", testChannelB, "shared sentinel")))
	results, err := s.SearchMessages(ctx, SearchOptions{
		Query: "sentinel", GuildIDs: []string{testGuildA}, ChannelIDExact: testChannelA, Limit: 10,
	})
	require.NoError(t, err)
	require.Len(t, results, 1)
	require.Equal(t, testChannelA, results[0].ChannelID)
}

func TestArchivePaginationAcceptsAbsentWebsiteSnowflakeBoundary(t *testing.T) {
	ctx := context.Background()
	s := openArchiveServiceStore(t)
	for _, id := range []string{"66666666666666660", "66666666666666670"} {
		require.NoError(t, s.UpsertMessage(ctx, archiveRecord(id, testChannelA, "message "+id)))
	}
	page, err := s.ListArchiveMessages(ctx, ArchivePageOptions{
		GuildID: testGuildA, ChannelID: testChannelA, BeforeID: "66666666666666665", Limit: 10,
	})
	require.NoError(t, err)
	require.Len(t, page.Messages, 1)
	require.Equal(t, "66666666666666660", page.Messages[0].MessageID)
}

func TestRecentProjectionReturnsMoreThan250RowsWithSharedTimestamp(t *testing.T) {
	ctx := context.Background()
	s := openArchiveServiceStore(t)
	created := time.Now().UTC().Add(-time.Minute)
	for i := 0; i < 300; i++ {
		id := fmt.Sprintf("%017d", 70000000000000000+i)
		record := archiveRecord(id, testChannelA, "recent")
		record.CreatedAt = created.Format(time.RFC3339Nano)
		require.NoError(t, s.UpsertMessage(ctx, record))
	}
	rows, err := s.ListRecentProjectionMessages(ctx, testGuildA, testChannelA, created.Add(-time.Second), 500)
	require.NoError(t, err)
	require.Len(t, rows, 300)
}

func TestSingleGuildValidationRejectsSharedDatabase(t *testing.T) {
	ctx := context.Background()
	s := openArchiveServiceStore(t)
	require.NoError(t, s.ValidateSingleGuild(ctx, testGuildA))
	require.NoError(t, s.UpsertGuild(ctx, GuildRecord{ID: testGuildB, Name: "Guild B", RawJSON: `{}`}))
	require.Error(t, s.ValidateSingleGuild(ctx, testGuildA))
}
