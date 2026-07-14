package archiveapi

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Config is deliberately single-tenant. One process, database, guild, Google
// Cloud project, and caller identity are the isolation boundary; multi-tenant
// dispatch is not supported by this service.
type Config struct {
	Listen                      string           `json:"listen"`
	DBPath                      string           `json:"db_path"`
	GuildID                     string           `json:"guild_id"`
	Audience                    string           `json:"audience"`
	AllowedCallerServiceAccount string           `json:"allowed_caller_service_account"`
	QueryTimeout                string           `json:"query_timeout"`
	MaxConcurrentQueries        int              `json:"max_concurrent_queries"`
	RequestsPerSecond           int              `json:"requests_per_second"`
	RequestBurst                int              `json:"request_burst"`
	MaxResponseBytes            int              `json:"max_response_bytes"`
	MaxRequestURIBytes          int              `json:"max_request_uri_bytes"`
	StaleAfter                  string           `json:"stale_after"`
	ExposeAttachmentURLs        bool             `json:"expose_attachment_urls"`
	Projection                  ProjectionConfig `json:"projection"`
}

type ProjectionConfig struct {
	Enabled               bool   `json:"enabled"`
	ProjectID             string `json:"project_id"`
	OrgID                 string `json:"org_id"`
	DatabaseURL           string `json:"database_url"`
	PollEvery             string `json:"poll_every"`
	BindingsEvery         string `json:"bindings_every"`
	RepairEvery           string `json:"repair_every"`
	RepairLookback        string `json:"repair_lookback"`
	InitialLookback       string `json:"initial_lookback"`
	OperationTimeout      string `json:"operation_timeout"`
	StatusEvery           string `json:"status_every"`
	InitialRowsPerBinding int    `json:"initial_rows_per_binding"`
	BatchSize             int    `json:"batch_size"`
	StatePath             string `json:"state_path"`
}

func LoadConfig(path string) (Config, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	var cfg Config
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("parse archive API config: %w", err)
	}
	if err := cfg.normalize(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c *Config) normalize() error {
	c.Listen = strings.TrimSpace(c.Listen)
	c.DBPath = strings.TrimSpace(c.DBPath)
	c.GuildID = strings.TrimSpace(c.GuildID)
	c.Audience = strings.TrimSpace(c.Audience)
	c.AllowedCallerServiceAccount = strings.ToLower(strings.TrimSpace(c.AllowedCallerServiceAccount))
	if c.Listen == "" {
		c.Listen = "0.0.0.0:8787"
	}
	if c.QueryTimeout == "" {
		c.QueryTimeout = "5s"
	}
	if c.StaleAfter == "" {
		c.StaleAfter = "2h"
	}
	if c.MaxConcurrentQueries == 0 {
		c.MaxConcurrentQueries = 4
	}
	if c.MaxResponseBytes == 0 {
		c.MaxResponseBytes = 1 << 20
	}
	if c.RequestsPerSecond == 0 {
		c.RequestsPerSecond = 20
	}
	if c.RequestBurst == 0 {
		c.RequestBurst = 40
	}
	if c.MaxRequestURIBytes == 0 {
		c.MaxRequestURIBytes = 4096
	}
	if c.DBPath == "" || c.GuildID == "" || c.Audience == "" || c.AllowedCallerServiceAccount == "" {
		return fmt.Errorf("db_path, guild_id, audience, and allowed_caller_service_account are required")
	}
	if !isDiscordSnowflake(c.GuildID) {
		return fmt.Errorf("guild_id must be a Discord snowflake")
	}
	if !strings.Contains(c.AllowedCallerServiceAccount, "@") || !strings.HasSuffix(c.AllowedCallerServiceAccount, ".gserviceaccount.com") {
		return fmt.Errorf("allowed_caller_service_account must be a Google service-account email")
	}
	if parsed, err := url.Parse(c.Audience); err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil {
		return fmt.Errorf("audience must be an exact HTTPS URL")
	}
	if _, err := time.ParseDuration(c.QueryTimeout); err != nil {
		return fmt.Errorf("invalid query_timeout: %w", err)
	}
	if stale, err := time.ParseDuration(c.StaleAfter); err != nil || stale < time.Minute {
		return fmt.Errorf("stale_after must be a duration of at least one minute")
	}
	if c.MaxConcurrentQueries < 1 || c.MaxConcurrentQueries > 32 {
		return fmt.Errorf("max_concurrent_queries must be between 1 and 32")
	}
	if c.RequestsPerSecond < 1 || c.RequestsPerSecond > 100 || c.RequestBurst < 1 || c.RequestBurst > 200 {
		return fmt.Errorf("requests_per_second must be 1..100 and request_burst must be 1..200")
	}
	if c.MaxResponseBytes < 4096 || c.MaxResponseBytes > 2<<20 {
		return fmt.Errorf("max_response_bytes must be between 4096 and 2097152")
	}
	if c.MaxRequestURIBytes < 512 || c.MaxRequestURIBytes > 16<<10 {
		return fmt.Errorf("max_request_uri_bytes must be between 512 and 16384")
	}
	return c.Projection.normalize(c.GuildID)
}

func (p *ProjectionConfig) normalize(_ string) error {
	if !p.Enabled {
		return nil
	}
	p.ProjectID = strings.TrimSpace(p.ProjectID)
	p.OrgID = strings.TrimSpace(p.OrgID)
	p.DatabaseURL = strings.TrimSpace(p.DatabaseURL)
	p.StatePath = strings.TrimSpace(p.StatePath)
	if p.PollEvery == "" {
		p.PollEvery = "2s"
	}
	if p.BindingsEvery == "" {
		p.BindingsEvery = "5m"
	}
	if p.RepairEvery == "" {
		p.RepairEvery = "6h"
	}
	if p.RepairLookback == "" {
		p.RepairLookback = "24h"
	}
	if p.InitialLookback == "" {
		p.InitialLookback = "720h"
	}
	if p.OperationTimeout == "" {
		p.OperationTimeout = "15s"
	}
	if p.StatusEvery == "" {
		p.StatusEvery = "10m"
	}
	if p.InitialRowsPerBinding == 0 {
		p.InitialRowsPerBinding = 250
	}
	if p.BatchSize == 0 {
		p.BatchSize = 100
	}
	if p.StatePath == "" {
		return fmt.Errorf("projection state_path is required and must live on persistent disk")
	}
	if p.ProjectID == "" || p.OrgID == "" || p.DatabaseURL == "" {
		return fmt.Errorf("projection project_id, org_id, and database_url are required when projection is enabled")
	}
	if strings.ContainsAny(p.ProjectID+p.OrgID, "/\\") {
		return fmt.Errorf("projection project_id and org_id may not contain slashes")
	}
	parsed, err := url.Parse(p.DatabaseURL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.Path != "" {
		return fmt.Errorf("projection database_url must be an origin-only HTTPS URL")
	}
	expectedDatabaseURL := "https://" + p.ProjectID + "-default-rtdb.firebaseio.com"
	if p.DatabaseURL != expectedDatabaseURL {
		return fmt.Errorf("projection database_url must exactly equal %s", expectedDatabaseURL)
	}
	for name, item := range map[string]struct {
		raw     string
		minimum time.Duration
	}{
		"poll_every": {p.PollEvery, time.Second}, "bindings_every": {p.BindingsEvery, 5 * time.Minute},
		"repair_every": {p.RepairEvery, time.Hour}, "repair_lookback": {p.RepairLookback, time.Second},
		"initial_lookback": {p.InitialLookback, time.Second}, "operation_timeout": {p.OperationTimeout, time.Second},
		"status_every": {p.StatusEvery, 5 * time.Minute},
	} {
		value, err := time.ParseDuration(item.raw)
		if err != nil || value < item.minimum {
			return fmt.Errorf("projection %s must be a duration of at least %s", name, item.minimum)
		}
	}
	if p.BatchSize < 1 || p.BatchSize > 250 {
		return fmt.Errorf("projection batch_size must be between 1 and 250")
	}
	if p.InitialRowsPerBinding < 1 || p.InitialRowsPerBinding > 250 {
		return fmt.Errorf("projection initial_rows_per_binding must be between 1 and 250")
	}
	if !filepath.IsAbs(p.StatePath) {
		return fmt.Errorf("projection state_path must be absolute")
	}
	return nil
}

func (c Config) queryDuration() time.Duration {
	d, _ := time.ParseDuration(c.QueryTimeout)
	return d
}

func (c Config) staleDuration() time.Duration {
	d, _ := time.ParseDuration(c.StaleAfter)
	return d
}

func isDiscordSnowflake(value string) bool {
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
