// Package config parses and validates workspace-brain runtime configuration.
package config

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// LookupFunc returns an environment value for key.
type LookupFunc func(key string) string

// Config contains typed process configuration for the runtime entrypoint.
type Config struct {
	Addr               string
	APIToken           string
	SlackSigningSecret string
	AdminUsers         []string
	PublicBaseURL      string
	CommandName        string
	SlackAppName       string
	DataPath           string
	ReadinessRequired  bool
	ReadHeaderTimeout  time.Duration
	ReadTimeout        time.Duration
	WriteTimeout       time.Duration
	IdleTimeout        time.Duration
	ShutdownTimeout    time.Duration
	// OpenAI embedding fields — all optional. When OpenAIAPIKey and
	// OpenAIEmbeddingModel are both non-empty the server uses the
	// OpenAI-backed embedder; otherwise the local FNV embedder is used.
	OpenAIAPIKey         string
	OpenAIBaseURL        string
	OpenAIEmbeddingModel string
	OpenAIOrganizationID string
	OpenAIProjectID      string
}

const (
	defaultAddr              = ":8080"
	defaultCommandName       = "brain"
	defaultSlackAppName      = "workspace-brain"
	defaultReadHeaderTimeout = 5 * time.Second
	defaultReadTimeout       = 15 * time.Second
	defaultWriteTimeout      = 30 * time.Second
	defaultIdleTimeout       = 60 * time.Second
	defaultShutdownTimeout   = 10 * time.Second
)

// FromEnv parses Config from getenv-compatible lookup.
func FromEnv(getenv LookupFunc) (Config, error) {
	if getenv == nil {
		getenv = func(string) string { return "" }
	}
	cfg := Config{
		Addr:                 envOr(getenv, "ADDR", defaultAddr),
		APIToken:             strings.TrimSpace(getenv("API_TOKEN")),
		SlackSigningSecret:   strings.TrimSpace(getenv("SLACK_SIGNING_SECRET")),
		AdminUsers:           splitList(getenv("ADMIN_USERS")),
		PublicBaseURL:        strings.TrimRight(strings.TrimSpace(getenv("PUBLIC_BASE_URL")), "/"),
		CommandName:          envOr(getenv, "COMMAND_NAME", defaultCommandName),
		SlackAppName:         envOr(getenv, "SLACK_APP_NAME", defaultSlackAppName),
		DataPath:             strings.TrimSpace(getenv("DATA_PATH")),
		OpenAIAPIKey:         strings.TrimSpace(getenv("OPENAI_API_KEY")),
		OpenAIBaseURL:        strings.TrimSpace(getenv("OPENAI_BASE_URL")),
		OpenAIEmbeddingModel: strings.TrimSpace(getenv("OPENAI_EMBEDDING_MODEL")),
		OpenAIOrganizationID: strings.TrimSpace(getenv("OPENAI_ORG_ID")),
		OpenAIProjectID:      strings.TrimSpace(getenv("OPENAI_PROJECT_ID")),
	}
	var err error
	if cfg.ReadinessRequired, err = parseBool(getenv, "READINESS_REQUIRED", false); err != nil {
		return Config{}, err
	}
	if cfg.ReadHeaderTimeout, err = parseDuration(getenv, "HTTP_READ_HEADER_TIMEOUT", defaultReadHeaderTimeout); err != nil {
		return Config{}, err
	}
	if cfg.ReadTimeout, err = parseDuration(getenv, "HTTP_READ_TIMEOUT", defaultReadTimeout); err != nil {
		return Config{}, err
	}
	if cfg.WriteTimeout, err = parseDuration(getenv, "HTTP_WRITE_TIMEOUT", defaultWriteTimeout); err != nil {
		return Config{}, err
	}
	if cfg.IdleTimeout, err = parseDuration(getenv, "HTTP_IDLE_TIMEOUT", defaultIdleTimeout); err != nil {
		return Config{}, err
	}
	if cfg.ShutdownTimeout, err = parseDuration(getenv, "HTTP_SHUTDOWN_TIMEOUT", defaultShutdownTimeout); err != nil {
		return Config{}, err
	}
	return cfg, cfg.Validate()
}

// Validate checks deploy-time configuration invariants.
func (c Config) Validate() error {
	if strings.TrimSpace(c.Addr) == "" {
		return fmt.Errorf("ADDR is required")
	}
	if c.SlackSigningSecret == "" && c.APIToken == "" {
		return fmt.Errorf("no frontend adapter configured; set SLACK_SIGNING_SECRET for Slack or API_TOKEN for JSON HTTP API, or use `go run ./cmd/workspace-brain demo`")
	}
	if strings.TrimSpace(c.CommandName) == "" {
		return fmt.Errorf("COMMAND_NAME is required")
	}
	if strings.ContainsAny(c.CommandName, " \t\n\r/") {
		return fmt.Errorf("COMMAND_NAME must be a slash-command name without whitespace or leading slash")
	}
	if strings.TrimSpace(c.SlackAppName) == "" {
		return fmt.Errorf("SLACK_APP_NAME is required")
	}
	if c.PublicBaseURL != "" {
		u, err := url.Parse(c.PublicBaseURL)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return fmt.Errorf("PUBLIC_BASE_URL must be an absolute URL")
		}
	}
	for name, value := range map[string]time.Duration{
		"HTTP_READ_HEADER_TIMEOUT": c.ReadHeaderTimeout,
		"HTTP_READ_TIMEOUT":        c.ReadTimeout,
		"HTTP_WRITE_TIMEOUT":       c.WriteTimeout,
		"HTTP_IDLE_TIMEOUT":        c.IdleTimeout,
		"HTTP_SHUTDOWN_TIMEOUT":    c.ShutdownTimeout,
	} {
		if value <= 0 {
			return fmt.Errorf("%s must be positive", name)
		}
	}
	return nil
}

// AdminUserSet returns admin users as a lookup map for adapters.
func (c Config) AdminUserSet() map[string]bool {
	admins := make(map[string]bool, len(c.AdminUsers))
	for _, user := range c.AdminUsers {
		admins[user] = true
	}
	return admins
}

func envOr(getenv LookupFunc, key, fallback string) string {
	if value := strings.TrimSpace(getenv(key)); value != "" {
		return value
	}
	return fallback
}

func splitList(raw string) []string {
	parts := strings.Split(raw, ",")
	items := make([]string, 0, len(parts))
	seen := make(map[string]bool, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" && !seen[part] {
			items = append(items, part)
			seen[part] = true
		}
	}
	return items
}

func parseBool(getenv LookupFunc, key string, fallback bool) (bool, error) {
	raw := strings.TrimSpace(getenv(key))
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("%s must be a boolean: %w", key, err)
	}
	return value, nil
}

func parseDuration(getenv LookupFunc, key string, fallback time.Duration) (time.Duration, error) {
	raw := strings.TrimSpace(getenv(key))
	if raw == "" {
		return fallback, nil
	}
	value, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("%s must be a duration: %w", key, err)
	}
	return value, nil
}
