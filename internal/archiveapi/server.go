package archiveapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/openclaw/discrawl/internal/store"
	"google.golang.org/api/idtoken"
)

const (
	maxSearchQueryBytes = 500
	expectedIssuer      = "https://accounts.google.com"
)

var errResponseTooLarge = errors.New("archive response exceeds configured byte limit")

type tokenClaims struct {
	Issuer   string
	Audience string
	Email    string
	Verified bool
}

type tokenVerifier interface {
	Verify(context.Context, string, string) (tokenClaims, error)
}

type googleTokenVerifier struct{}

func (googleTokenVerifier) Verify(ctx context.Context, rawToken, audience string) (tokenClaims, error) {
	payload, err := idtoken.Validate(ctx, rawToken, audience)
	if err != nil {
		return tokenClaims{}, err
	}
	email, _ := payload.Claims["email"].(string)
	verified, _ := payload.Claims["email_verified"].(bool)
	return tokenClaims{
		Issuer: payload.Issuer, Audience: payload.Audience,
		Email: strings.ToLower(strings.TrimSpace(email)), Verified: verified,
	}, nil
}

type metrics struct {
	requests     atomic.Uint64
	authFailures atomic.Uint64
	busy         atomic.Uint64
	rateLimited  atomic.Uint64
	errors       atomic.Uint64
}

type tokenBucket struct {
	mu       sync.Mutex
	tokens   float64
	last     time.Time
	rate     float64
	capacity float64
}

func (b *tokenBucket) allow(now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.last.IsZero() {
		b.last, b.tokens = now, b.capacity
	}
	elapsed := now.Sub(b.last).Seconds()
	if elapsed > 0 {
		b.tokens = min(b.capacity, b.tokens+elapsed*b.rate)
		b.last = now
	}
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

type Server struct {
	cfg             Config
	logger          *slog.Logger
	store           *store.Store
	verifier        tokenVerifier
	queryTimeout    time.Duration
	staleAfter      time.Duration
	semaphore       chan struct{}
	authSemaphore   chan struct{}
	healthSemaphore chan struct{}
	rateLimit       tokenBucket
	authRateLimit   tokenBucket
	healthRateLimit tokenBucket
	metrics         metrics
	now             func() time.Time
}

func NewServer(cfg Config, logger *slog.Logger) (*Server, error) {
	return newServer(cfg, logger, googleTokenVerifier{})
}

func newServer(cfg Config, logger *slog.Logger, verifier tokenVerifier) (*Server, error) {
	if err := cfg.normalize(); err != nil {
		return nil, err
	}
	if logger == nil {
		logger = slog.Default()
	}
	archiveStore, err := store.OpenReadOnly(context.Background(), cfg.DBPath)
	if err != nil {
		return nil, fmt.Errorf("open archive: %w", err)
	}
	if err := archiveStore.ValidateSingleGuild(context.Background(), cfg.GuildID); err != nil {
		_ = archiveStore.Close()
		return nil, fmt.Errorf("validate isolated archive: %w", err)
	}
	return &Server{
		cfg: cfg, logger: logger, store: archiveStore, verifier: verifier,
		queryTimeout: cfg.queryDuration(), staleAfter: cfg.staleDuration(),
		semaphore:     make(chan struct{}, cfg.MaxConcurrentQueries),
		authSemaphore: make(chan struct{}, cfg.MaxConcurrentQueries), now: time.Now,
		healthSemaphore: make(chan struct{}, 1),
		rateLimit:       tokenBucket{rate: float64(cfg.RequestsPerSecond), capacity: float64(cfg.RequestBurst)},
		authRateLimit:   tokenBucket{rate: float64(cfg.RequestsPerSecond), capacity: float64(cfg.RequestBurst)},
		healthRateLimit: tokenBucket{rate: 5, capacity: 10},
	}, nil
}

func (s *Server) Close() error {
	if s.store == nil {
		return nil
	}
	return s.store.Close()
}

func (s *Server) Handler() http.Handler { return http.HandlerFunc(s.serveHTTP) }

func (s *Server) serveHTTP(w http.ResponseWriter, r *http.Request) {
	started := s.now()
	s.metrics.requests.Add(1)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	if len(r.RequestURI) > s.cfg.MaxRequestURIBytes {
		s.writeError(w, http.StatusRequestURITooLong, "request_uri_too_large")
		return
	}
	if r.Method != http.MethodGet {
		s.writeError(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	if r.ContentLength != 0 || len(r.TransferEncoding) > 0 {
		s.writeError(w, http.StatusRequestEntityTooLarge, "request_body_not_allowed")
		return
	}

	switch r.URL.Path {
	case "/livez":
		s.writeSmallJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	case "/readyz":
		if !s.healthRateLimit.allow(s.now()) {
			s.writeError(w, http.StatusTooManyRequests, "rate_limited")
			return
		}
		select {
		case s.healthSemaphore <- struct{}{}:
			defer func() { <-s.healthSemaphore }()
		default:
			s.writeError(w, http.StatusServiceUnavailable, "health_busy")
			return
		}
		s.handleReady(w, r)
		return
	case "/metrics":
		s.handleMetrics(w, r)
		return
	}
	if !s.authRateLimit.allow(s.now()) {
		s.metrics.rateLimited.Add(1)
		w.Header().Set("Retry-After", "1")
		s.writeError(w, http.StatusTooManyRequests, "rate_limited")
		return
	}
	select {
	case s.authSemaphore <- struct{}{}:
	default:
		s.metrics.busy.Add(1)
		s.writeError(w, http.StatusServiceUnavailable, "auth_busy")
		return
	}
	authenticated := s.authenticate(r)
	<-s.authSemaphore
	if !authenticated {
		s.metrics.authFailures.Add(1)
		s.writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if !s.rateLimit.allow(s.now()) {
		s.metrics.rateLimited.Add(1)
		w.Header().Set("Retry-After", "1")
		s.writeError(w, http.StatusTooManyRequests, "rate_limited")
		return
	}

	select {
	case s.semaphore <- struct{}{}:
		defer func() { <-s.semaphore }()
	default:
		s.metrics.busy.Add(1)
		w.Header().Set("Retry-After", "1")
		s.writeError(w, http.StatusServiceUnavailable, "archive_busy")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.queryTimeout)
	defer cancel()
	var err error
	switch r.URL.Path {
	case "/v1/messages":
		err = s.handleMessages(ctx, w, r)
	case "/v1/search":
		err = s.handleSearch(ctx, w, r)
	case "/v1/status":
		err = s.handleStatus(ctx, w)
	default:
		s.writeError(w, http.StatusNotFound, "not_found")
		return
	}
	if err != nil {
		s.metrics.errors.Add(1)
		status, code := http.StatusInternalServerError, "archive_query_failed"
		switch {
		case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
			status, code = http.StatusGatewayTimeout, "archive_timeout"
		case errors.Is(err, errResponseTooLarge):
			status, code = http.StatusRequestEntityTooLarge, "archive_response_too_large"
		case errors.Is(err, store.ErrArchiveInvalidRequest):
			status, code = http.StatusBadRequest, "invalid_request"
		}
		s.logger.Error("archive request failed", "action", r.URL.Path, "code", code,
			"duration_ms", s.now().Sub(started).Milliseconds())
		s.writeError(w, status, code)
		return
	}
	s.logger.Info("archive request", "action", r.URL.Path,
		"duration_ms", s.now().Sub(started).Milliseconds())
}

func (s *Server) authenticate(r *http.Request) bool {
	header := strings.TrimSpace(r.Header.Get("Authorization"))
	if !strings.HasPrefix(header, "Bearer ") || len(header) <= len("Bearer ") {
		return false
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	claims, err := s.verifier.Verify(ctx, strings.TrimSpace(strings.TrimPrefix(header, "Bearer ")), s.cfg.Audience)
	if err != nil {
		return false
	}
	return claims.Issuer == expectedIssuer && claims.Audience == s.cfg.Audience && claims.Verified &&
		claims.Email == s.cfg.AllowedCallerServiceAccount
}

func (s *Server) handleMessages(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
	limit, err := boundedLimit(r.URL.Query().Get("limit"), 50, 100)
	if err != nil {
		return err
	}
	channelID := strings.TrimSpace(r.URL.Query().Get("channel_id"))
	beforeID := strings.TrimSpace(r.URL.Query().Get("before_id"))
	if !isDiscordSnowflake(channelID) || (beforeID != "" && !isDiscordSnowflake(beforeID)) {
		return fmt.Errorf("%w: channel_id and before_id must be Discord snowflakes", store.ErrArchiveInvalidRequest)
	}
	page, err := s.store.ListArchiveMessages(ctx, store.ArchivePageOptions{
		GuildID: s.cfg.GuildID, ChannelID: channelID, BeforeID: beforeID, Limit: limit,
		ExposeAttachmentURLs: s.cfg.ExposeAttachmentURLs,
	})
	if err != nil {
		return err
	}
	return s.writeBoundedJSON(w, http.StatusOK, page)
}

func (s *Server) handleSearch(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
	limit, err := boundedLimit(r.URL.Query().Get("limit"), 20, 100)
	if err != nil {
		return err
	}
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	if query == "" || len([]byte(query)) > maxSearchQueryBytes {
		return fmt.Errorf("%w: search query is required and must not exceed %d bytes", store.ErrArchiveInvalidRequest, maxSearchQueryBytes)
	}
	channel := strings.TrimSpace(r.URL.Query().Get("channel_id"))
	if !isDiscordSnowflake(channel) {
		return fmt.Errorf("%w: channel_id must be a Discord snowflake", store.ErrArchiveInvalidRequest)
	}
	author := strings.TrimSpace(r.URL.Query().Get("author"))
	if len([]byte(author)) > 100 {
		return fmt.Errorf("%w: author exceeds 100 bytes", store.ErrArchiveInvalidRequest)
	}
	results, err := s.store.SearchArchiveMessages(ctx, store.SearchOptions{
		Query: query, GuildIDs: []string{s.cfg.GuildID}, ChannelIDExact: channel, Author: author, Limit: limit,
	})
	if err != nil {
		return err
	}
	return s.writeBoundedJSON(w, http.StatusOK, map[string]any{"results": results})
}

func (s *Server) handleStatus(ctx context.Context, w http.ResponseWriter) error {
	status, err := s.store.ScopedArchiveStatus(ctx, s.cfg.GuildID, s.now(), s.staleAfter)
	if err != nil {
		return err
	}
	return s.writeBoundedJSON(w, http.StatusOK, status)
}

func boundedLimit(raw string, fallback, max int) (int, error) {
	if strings.TrimSpace(raw) == "" {
		return fallback, nil
	}
	limit, err := strconv.Atoi(raw)
	if err != nil || limit < 1 || limit > max {
		return 0, fmt.Errorf("%w: limit must be between 1 and %d", store.ErrArchiveInvalidRequest, max)
	}
	return limit, nil
}

func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), time.Second)
	defer cancel()
	ready, err := s.store.ArchiveReady(ctx, s.cfg.GuildID, s.now(), s.staleAfter)
	if err != nil || !ready {
		s.writeError(w, http.StatusServiceUnavailable, "not_ready")
		return
	}
	s.writeSmallJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	ip := net.ParseIP(host)
	if err != nil || ip == nil || !ip.IsLoopback() {
		s.writeError(w, http.StatusForbidden, "forbidden")
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	_, _ = fmt.Fprintf(w, "discrawl_archive_requests_total %d\n", s.metrics.requests.Load())
	_, _ = fmt.Fprintf(w, "discrawl_archive_auth_failures_total %d\n", s.metrics.authFailures.Load())
	_, _ = fmt.Fprintf(w, "discrawl_archive_busy_total %d\n", s.metrics.busy.Load())
	_, _ = fmt.Fprintf(w, "discrawl_archive_rate_limited_total %d\n", s.metrics.rateLimited.Load())
	_, _ = fmt.Fprintf(w, "discrawl_archive_errors_total %d\n", s.metrics.errors.Load())
}

func (s *Server) writeBoundedJSON(w http.ResponseWriter, status int, value any) error {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	if err := encoder.Encode(value); err != nil {
		return err
	}
	if buffer.Len() > s.cfg.MaxResponseBytes {
		return errResponseTooLarge
	}
	w.WriteHeader(status)
	_, err := w.Write(buffer.Bytes())
	return err
}

func (s *Server) writeSmallJSON(w http.ResponseWriter, status int, value any) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func (s *Server) writeError(w http.ResponseWriter, status int, code string) {
	s.writeSmallJSON(w, status, map[string]any{"error": code})
}
