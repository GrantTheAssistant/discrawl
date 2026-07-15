package syncer

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/stretchr/testify/require"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return fn(req) }

func TestBuildMessageMutationIncludesAttachmentTextAndMentions(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("stack trace line one\nstack trace line two"))
	}))
	defer server.Close()

	previousClient := attachmentHTTPClient
	previousPolicy := attachmentURLAllowed
	attachmentHTTPClient = server.Client()
	attachmentURLAllowed = func(raw string) bool { return raw == server.URL }
	t.Cleanup(func() {
		attachmentHTTPClient = previousClient
		attachmentURLAllowed = previousPolicy
	})

	now := time.Now().UTC()
	mutation, err := buildMessageMutation(context.Background(), &discordgo.Message{
		ID:        "m1",
		GuildID:   "g1",
		ChannelID: "c1",
		Content:   "",
		Timestamp: now,
		Author:    &discordgo.User{ID: "u1", Username: "peter"},
		Attachments: []*discordgo.MessageAttachment{{
			ID:          "a1",
			Filename:    "trace.txt",
			ContentType: "text/plain",
			URL:         server.URL,
			Size:        64,
		}},
		Mentions: []*discordgo.User{{
			ID:         "u2",
			Username:   "shadow",
			GlobalName: "Shadow",
		}},
		MentionRoles: []string{"r1"},
	}, "maintainers", "", false, true)
	require.NoError(t, err)
	require.Len(t, mutation.Attachments, 1)
	require.Equal(t, "trace.txt", mutation.Attachments[0].Filename)
	require.Contains(t, mutation.Attachments[0].TextContent, "stack trace")
	require.Contains(t, mutation.Record.NormalizedContent, "trace.txt")
	require.Contains(t, mutation.Record.NormalizedContent, "stack trace line one")
	require.Len(t, mutation.Mentions, 2)
	require.Equal(t, "user", mutation.Mentions[0].TargetType)
	require.Equal(t, "u2", mutation.Mentions[0].TargetID)
	require.Equal(t, "Shadow", mutation.Mentions[0].TargetName)
	require.Equal(t, "role", mutation.Mentions[1].TargetType)
	require.Equal(t, "r1", mutation.Mentions[1].TargetID)
}

func TestBuildMessageMutationFallsBackToChannelGuildID(t *testing.T) {
	now := time.Now().UTC()
	mutation, err := buildMessageMutation(context.Background(), &discordgo.Message{
		ID:        "m1",
		ChannelID: "c1",
		Content:   "missing guild id from channel history",
		Timestamp: now,
		Author:    &discordgo.User{ID: "u1", Username: "peter"},
		Attachments: []*discordgo.MessageAttachment{{
			ID:       "a1",
			Filename: "trace.txt",
		}},
		Mentions: []*discordgo.User{{ID: "u2", Username: "shadow"}},
		MentionRoles: []string{
			"r1",
		},
	}, "maintainers", "g1", false, false)
	require.NoError(t, err)
	require.Equal(t, "g1", mutation.Record.GuildID)
	require.Equal(t, "g1", mutation.Attachments[0].GuildID)
	require.Equal(t, "g1", mutation.Mentions[0].GuildID)
	require.Equal(t, "g1", mutation.Mentions[1].GuildID)
}

func TestShouldFetchAttachmentText(t *testing.T) {
	t.Parallel()

	require.True(t, shouldFetchAttachmentText(&discordgo.MessageAttachment{Filename: "trace.txt"}))
	require.True(t, shouldFetchAttachmentText(&discordgo.MessageAttachment{Filename: "payload.json"}))
	require.True(t, shouldFetchAttachmentText(&discordgo.MessageAttachment{ContentType: "text/plain"}))
	require.False(t, shouldFetchAttachmentText(&discordgo.MessageAttachment{Filename: "script.sh", ContentType: "text/plain"}))
	require.False(t, shouldFetchAttachmentText(&discordgo.MessageAttachment{Filename: "photo.png", ContentType: "image/png"}))
}

func TestAttachmentTextURLPolicyAndFinalRedirect(t *testing.T) {
	require.True(t, isAllowedAttachmentTextURL("https://cdn.discordapp.com/attachments/c/file.txt"))
	require.True(t, isAllowedAttachmentTextURL("https://media.discordapp.net/attachments/c/file.txt"))
	for _, raw := range []string{
		"http://cdn.discordapp.com/attachments/c/file.txt",
		"https://cdn.discordapp.com:444/attachments/c/file.txt",
		"https://cdn.discordapp.com.evil.test/attachments/c/file.txt",
		"https://169.254.169.254/attachments/c/file.txt",
		"https://cdn.discordapp.com/not-attachments/file.txt",
	} {
		require.False(t, isAllowedAttachmentTextURL(raw), raw)
	}

	previous := attachmentHTTPClient
	attachmentHTTPClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		final, err := http.NewRequest(http.MethodGet, "http://169.254.169.254/computeMetadata/v1/", nil)
		require.NoError(t, err)
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: http.NoBody, Request: final}, nil
	})}
	t.Cleanup(func() { attachmentHTTPClient = previous })
	_, err := fetchAttachmentText(context.Background(), "https://cdn.discordapp.com/attachments/c/file.txt")
	require.ErrorContains(t, err, "response URL denied")
}

func TestBuildMessageMutationSkipsBinaryResponseEvenWhenAttachmentLooksTextual(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write([]byte{0x7f, 'E', 'L', 'F', 0x02, 0x01})
	}))
	defer server.Close()

	previousClient := attachmentHTTPClient
	previousPolicy := attachmentURLAllowed
	attachmentHTTPClient = server.Client()
	attachmentURLAllowed = func(raw string) bool { return raw == server.URL }
	t.Cleanup(func() {
		attachmentHTTPClient = previousClient
		attachmentURLAllowed = previousPolicy
	})

	mutation, err := buildMessageMutation(context.Background(), &discordgo.Message{
		ID:        "m1",
		GuildID:   "g1",
		ChannelID: "c1",
		Timestamp: time.Now().UTC(),
		Author:    &discordgo.User{ID: "u1", Username: "peter"},
		Attachments: []*discordgo.MessageAttachment{{
			ID:       "a1",
			Filename: "trace.txt",
			URL:      server.URL,
		}},
	}, "maintainers", "", false, true)
	require.NoError(t, err)
	require.Len(t, mutation.Attachments, 1)
	require.Empty(t, mutation.Attachments[0].TextContent)
	require.Contains(t, mutation.Record.NormalizedContent, "trace.txt")
	require.NotContains(t, mutation.Record.NormalizedContent, "ELF")
}

func TestBuildMessageMutationSkipsOversizedAttachmentResponses(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("Content-Length", "999999")
		_, _ = w.Write([]byte("should not be indexed"))
	}))
	defer server.Close()

	previousClient := attachmentHTTPClient
	previousPolicy := attachmentURLAllowed
	attachmentHTTPClient = server.Client()
	attachmentURLAllowed = func(raw string) bool { return raw == server.URL }
	t.Cleanup(func() {
		attachmentHTTPClient = previousClient
		attachmentURLAllowed = previousPolicy
	})

	mutation, err := buildMessageMutation(context.Background(), &discordgo.Message{
		ID:        "m1",
		GuildID:   "g1",
		ChannelID: "c1",
		Timestamp: time.Now().UTC(),
		Author:    &discordgo.User{ID: "u1", Username: "peter"},
		Attachments: []*discordgo.MessageAttachment{{
			ID:          "a1",
			Filename:    "trace.txt",
			ContentType: "text/plain",
			URL:         server.URL,
		}},
	}, "maintainers", "", false, true)
	require.NoError(t, err)
	require.Len(t, mutation.Attachments, 1)
	require.Empty(t, mutation.Attachments[0].TextContent)
	require.NotContains(t, mutation.Record.NormalizedContent, "should not be indexed")
}

func TestBuildMessageMutationRespectsAttachmentTextOptOut(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("stack trace line one"))
	}))
	defer server.Close()

	previousClient := attachmentHTTPClient
	attachmentHTTPClient = server.Client()
	t.Cleanup(func() {
		attachmentHTTPClient = previousClient
	})

	mutation, err := buildMessageMutation(context.Background(), &discordgo.Message{
		ID:        "m1",
		GuildID:   "g1",
		ChannelID: "c1",
		Timestamp: time.Now().UTC(),
		Author:    &discordgo.User{ID: "u1", Username: "peter"},
		Attachments: []*discordgo.MessageAttachment{{
			ID:          "a1",
			Filename:    "trace.txt",
			ContentType: "text/plain",
			URL:         server.URL,
		}},
	}, "maintainers", "", false, false)
	require.NoError(t, err)
	require.Len(t, mutation.Attachments, 1)
	require.Empty(t, mutation.Attachments[0].TextContent)
	require.Contains(t, mutation.Record.NormalizedContent, "trace.txt")
	require.NotContains(t, mutation.Record.NormalizedContent, "stack trace line one")
}
