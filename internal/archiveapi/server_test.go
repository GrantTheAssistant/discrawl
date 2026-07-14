package archiveapi

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/openclaw/discrawl/internal/store"
	"github.com/stretchr/testify/require"
)

const (
	apiGuild    = "11111111111111111"
	apiChannel  = "33333333333333333"
	apiCaller   = "brennos-app@brennos.iam.gserviceaccount.com"
	apiAudience = "https://discrawl.internal.brennos.example"
)

type fixedVerifier struct {
	claims tokenClaims
	err    error
}

func (v fixedVerifier) Verify(context.Context, string, string) (tokenClaims, error) {
	return v.claims, v.err
}

func validClaims() tokenClaims {
	return tokenClaims{Issuer: expectedIssuer, Audience: apiAudience, Email: apiCaller, Verified: true}
}

func seedAPIDatabase(t *testing.T, count int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "archive.db")
	s, err := store.Open(context.Background(), path)
	require.NoError(t, err)
	require.NoError(t, s.UpsertGuild(context.Background(), store.GuildRecord{ID: apiGuild, Name: "Guild", RawJSON: `{}`}))
	require.NoError(t, s.UpsertChannel(context.Background(), store.ChannelRecord{ID: apiChannel, GuildID: apiGuild, Kind: "text", Name: "general", RawJSON: `{}`}))
	for i := 0; i < count; i++ {
		id := "6666666666666666" + string(rune('0'+i))
		require.NoError(t, s.UpsertMessage(context.Background(), store.MessageRecord{
			ID: id, GuildID: apiGuild, ChannelID: apiChannel, AuthorID: "55555555555555555",
			AuthorName: "Author", ChannelName: "general", Content: strings.Repeat("x", 2000),
			NormalizedContent: strings.Repeat("x", 2000), CreatedAt: time.Now().UTC().Format(time.RFC3339Nano), RawJSON: `{}`,
		}))
	}
	require.NoError(t, s.SetSyncState(context.Background(), "tail:heartbeat", "ok"))
	require.NoError(t, s.Close())
	return path
}

func testConfig(path string) Config {
	return Config{
		Listen: "127.0.0.1:0", DBPath: path, GuildID: apiGuild,
		Audience: apiAudience, AllowedCallerServiceAccount: apiCaller,
		QueryTimeout: "2s", StaleAfter: "2h", MaxConcurrentQueries: 1,
		RequestsPerSecond: 100, RequestBurst: 100, MaxResponseBytes: 1 << 20,
		MaxRequestURIBytes: 4096,
	}
}

func newTestServer(t *testing.T, cfg Config, verifier tokenVerifier) *Server {
	t.Helper()
	server, err := newServer(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), verifier)
	require.NoError(t, err)
	t.Cleanup(func() { _ = server.Close() })
	return server
}

func apiRequest(method, target string) *http.Request {
	req := httptest.NewRequest(method, target, nil)
	req.Header.Set("Authorization", "Bearer signed-token")
	return req
}

func TestOIDCRequiresExactIssuerAudienceAndVerifiedServiceAccount(t *testing.T) {
	path := seedAPIDatabase(t, 0)
	cases := []struct {
		name   string
		claims tokenClaims
		err    error
		want   int
	}{
		{name: "valid", claims: validClaims(), want: http.StatusOK},
		{name: "wrong issuer", claims: tokenClaims{Issuer: "accounts.google.com", Audience: apiAudience, Email: apiCaller, Verified: true}, want: http.StatusUnauthorized},
		{name: "wrong audience", claims: tokenClaims{Issuer: expectedIssuer, Audience: "https://other.example", Email: apiCaller, Verified: true}, want: http.StatusUnauthorized},
		{name: "wrong service account", claims: tokenClaims{Issuer: expectedIssuer, Audience: apiAudience, Email: "other@brennos.iam.gserviceaccount.com", Verified: true}, want: http.StatusUnauthorized},
		{name: "unverified email", claims: tokenClaims{Issuer: expectedIssuer, Audience: apiAudience, Email: apiCaller}, want: http.StatusUnauthorized},
		{name: "verification error", err: errors.New("bad signature"), want: http.StatusUnauthorized},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := newTestServer(t, testConfig(path), fixedVerifier{claims: tc.claims, err: tc.err})
			response := httptest.NewRecorder()
			server.Handler().ServeHTTP(response, apiRequest(http.MethodGet, "/v1/status"))
			require.Equal(t, tc.want, response.Code)
		})
	}
}

func TestAPIBoundsRequestURIRequestBodyRateConcurrencyAndResponse(t *testing.T) {
	path := seedAPIDatabase(t, 3)

	t.Run("URI", func(t *testing.T) {
		cfg := testConfig(path)
		cfg.MaxRequestURIBytes = 512
		server := newTestServer(t, cfg, fixedVerifier{claims: validClaims()})
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, apiRequest(http.MethodGet, "/v1/status?x="+strings.Repeat("a", 600)))
		require.Equal(t, http.StatusRequestURITooLong, response.Code)
	})

	t.Run("body and chunked body", func(t *testing.T) {
		server := newTestServer(t, testConfig(path), fixedVerifier{claims: validClaims()})
		for _, chunked := range []bool{false, true} {
			req := apiRequest(http.MethodGet, "/v1/status")
			req.Body = io.NopCloser(strings.NewReader("body"))
			if chunked {
				req.ContentLength = -1
				req.TransferEncoding = []string{"chunked"}
			} else {
				req.ContentLength = 4
			}
			response := httptest.NewRecorder()
			server.Handler().ServeHTTP(response, req)
			require.Equal(t, http.StatusRequestEntityTooLarge, response.Code)
		}
	})

	t.Run("rate", func(t *testing.T) {
		cfg := testConfig(path)
		cfg.RequestsPerSecond, cfg.RequestBurst = 1, 1
		server := newTestServer(t, cfg, fixedVerifier{claims: validClaims()})
		first, second := httptest.NewRecorder(), httptest.NewRecorder()
		server.Handler().ServeHTTP(first, apiRequest(http.MethodGet, "/v1/status"))
		server.Handler().ServeHTTP(second, apiRequest(http.MethodGet, "/v1/status"))
		require.Equal(t, http.StatusOK, first.Code)
		require.Equal(t, http.StatusTooManyRequests, second.Code)
		require.Equal(t, "1", second.Header().Get("Retry-After"))
	})

	t.Run("concurrency", func(t *testing.T) {
		server := newTestServer(t, testConfig(path), fixedVerifier{claims: validClaims()})
		server.semaphore <- struct{}{}
		defer func() { <-server.semaphore }()
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, apiRequest(http.MethodGet, "/v1/status"))
		require.Equal(t, http.StatusServiceUnavailable, response.Code)
	})

	t.Run("response", func(t *testing.T) {
		cfg := testConfig(path)
		cfg.MaxResponseBytes = 4096
		server := newTestServer(t, cfg, fixedVerifier{claims: validClaims()})
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, apiRequest(http.MethodGet, "/v1/messages?channel_id="+apiChannel+"&limit=100"))
		require.Equal(t, http.StatusRequestEntityTooLarge, response.Code)
		require.NotContains(t, response.Body.String(), strings.Repeat("x", 100))
	})
}

func TestAPIFailsClosedWithoutExactChannelScopeAndHasNoChannelCatalog(t *testing.T) {
	server := newTestServer(t, testConfig(seedAPIDatabase(t, 0)), fixedVerifier{claims: validClaims()})
	for _, target := range []string{"/v1/messages", "/v1/search?q=test"} {
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, apiRequest(http.MethodGet, target))
		require.Equal(t, http.StatusBadRequest, response.Code)
	}
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, apiRequest(http.MethodGet, "/v1/channels"))
	require.Equal(t, http.StatusNotFound, response.Code)
}

func TestServerRejectsSharedDatabase(t *testing.T) {
	path := seedAPIDatabase(t, 0)
	s, err := store.Open(context.Background(), path)
	require.NoError(t, err)
	require.NoError(t, s.UpsertGuild(context.Background(), store.GuildRecord{ID: "22222222222222222", Name: "other", RawJSON: `{}`}))
	require.NoError(t, s.Close())
	_, err = newServer(testConfig(path), nil, fixedVerifier{claims: validClaims()})
	require.Error(t, err)
}
