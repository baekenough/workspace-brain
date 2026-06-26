package config

import (
	"strings"
	"testing"
	"time"
)

func TestFromEnvDefaultsAndTypedValues(t *testing.T) {
	t.Parallel()
	cfg, err := FromEnv(func(key string) string {
		switch key {
		case "API_TOKEN":
			return " token "
		case "ADMIN_USERS":
			return " U1, U2, U1 ,, "
		case "PUBLIC_BASE_URL":
			return "https://example.com/"
		case "DATA_PATH":
			return "/data/workspace-brain"
		case "READINESS_REQUIRED":
			return "true"
		case "HTTP_READ_TIMEOUT":
			return "3s"
		case "HTTP_READ_HEADER_TIMEOUT":
			return "2s"
		case "HTTP_WRITE_TIMEOUT":
			return "4s"
		case "HTTP_IDLE_TIMEOUT":
			return "5s"
		case "HTTP_SHUTDOWN_TIMEOUT":
			return "6s"
		default:
			return ""
		}
	})
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if cfg.Addr != ":8080" || cfg.CommandName != "brain" || cfg.SlackAppName != "workspace-brain" {
		t.Fatalf("defaults not applied: %+v", cfg)
	}
	if cfg.APIToken != "token" || cfg.PublicBaseURL != "https://example.com" || cfg.DataPath != "/data/workspace-brain" || !cfg.ReadinessRequired {
		t.Fatalf("typed values not parsed: %+v", cfg)
	}
	if got := strings.Join(cfg.AdminUsers, ","); got != "U1,U2" {
		t.Fatalf("AdminUsers = %q", got)
	}
	if cfg.ReadHeaderTimeout != 2*time.Second || cfg.ReadTimeout != 3*time.Second || cfg.WriteTimeout != 4*time.Second || cfg.IdleTimeout != 5*time.Second || cfg.ShutdownTimeout != 6*time.Second {
		t.Fatalf("timeouts = %+v", cfg)
	}
	admins := cfg.AdminUserSet()
	if !admins["U1"] || !admins["U2"] || admins[""] {
		t.Fatalf("AdminUserSet = %+v", admins)
	}
}

func TestFromEnvValidationErrors(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		env  map[string]string
		want string
	}{
		{"adapter", map[string]string{}, "no frontend adapter configured"},
		{"bool", map[string]string{"API_TOKEN": "t", "READINESS_REQUIRED": "sometimes"}, "READINESS_REQUIRED must be a boolean"},
		{"duration", map[string]string{"API_TOKEN": "t", "HTTP_READ_TIMEOUT": "soon"}, "HTTP_READ_TIMEOUT must be a duration"},
		{"command", map[string]string{"API_TOKEN": "t", "COMMAND_NAME": "/brain"}, "COMMAND_NAME must be"},
		{"url", map[string]string{"API_TOKEN": "t", "PUBLIC_BASE_URL": "://bad"}, "PUBLIC_BASE_URL must be"},
		{"positive", map[string]string{"API_TOKEN": "t", "HTTP_IDLE_TIMEOUT": "0s"}, "HTTP_IDLE_TIMEOUT must be positive"},
	} {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := FromEnv(func(key string) string { return tt.env[key] })
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err=%v, want %q", err, tt.want)
			}
		})
	}
}

func TestFromEnvUsesDefaultsWhenLookupIsNil(t *testing.T) {
	t.Parallel()
	_, err := FromEnv(nil)
	if err == nil || !strings.Contains(err.Error(), "no frontend adapter configured") {
		t.Fatalf("err=%v", err)
	}
}

func TestFromEnvReturnsEachDurationParseError(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		key  string
	}{
		{"read header", "HTTP_READ_HEADER_TIMEOUT"},
		{"write", "HTTP_WRITE_TIMEOUT"},
		{"idle", "HTTP_IDLE_TIMEOUT"},
		{"shutdown", "HTTP_SHUTDOWN_TIMEOUT"},
	} {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := FromEnv(func(key string) string {
				if key == "API_TOKEN" {
					return "token"
				}
				if key == tt.key {
					return "soon"
				}
				return ""
			})
			if err == nil || !strings.Contains(err.Error(), tt.key+" must be a duration") {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

func TestFromEnvParsesOpenAIFields(t *testing.T) {
	t.Parallel()
	cfg, err := FromEnv(func(key string) string {
		switch key {
		case "API_TOKEN":
			return "token"
		case "OPENAI_API_KEY":
			return " sk-test "
		case "OPENAI_BASE_URL":
			return " https://api.example.com/v1 "
		case "OPENAI_EMBEDDING_MODEL":
			return " text-embedding-3-small "
		case "OPENAI_ORG_ID":
			return " org-123 "
		case "OPENAI_PROJECT_ID":
			return " proj-456 "
		default:
			return ""
		}
	})
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if cfg.OpenAIAPIKey != "sk-test" {
		t.Fatalf("OpenAIAPIKey = %q", cfg.OpenAIAPIKey)
	}
	if cfg.OpenAIBaseURL != "https://api.example.com/v1" {
		t.Fatalf("OpenAIBaseURL = %q", cfg.OpenAIBaseURL)
	}
	if cfg.OpenAIEmbeddingModel != "text-embedding-3-small" {
		t.Fatalf("OpenAIEmbeddingModel = %q", cfg.OpenAIEmbeddingModel)
	}
	if cfg.OpenAIOrganizationID != "org-123" {
		t.Fatalf("OpenAIOrganizationID = %q", cfg.OpenAIOrganizationID)
	}
	if cfg.OpenAIProjectID != "proj-456" {
		t.Fatalf("OpenAIProjectID = %q", cfg.OpenAIProjectID)
	}
}

func TestFromEnvOpenAIFieldsDefaultToEmpty(t *testing.T) {
	t.Parallel()
	cfg, err := FromEnv(func(key string) string {
		if key == "API_TOKEN" {
			return "token"
		}
		return ""
	})
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if cfg.OpenAIAPIKey != "" || cfg.OpenAIBaseURL != "" || cfg.OpenAIEmbeddingModel != "" || cfg.OpenAIOrganizationID != "" || cfg.OpenAIProjectID != "" {
		t.Fatalf("expected empty OpenAI fields, got %+v", cfg)
	}
}

func TestValidateRequiresNonEmptyFields(t *testing.T) {
	t.Parallel()
	base := Config{APIToken: "token", Addr: ":8080", CommandName: "brain", SlackAppName: "workspace-brain", ReadHeaderTimeout: time.Second, ReadTimeout: time.Second, WriteTimeout: time.Second, IdleTimeout: time.Second, ShutdownTimeout: time.Second}
	for _, tt := range []struct {
		name string
		mut  func(*Config)
		want string
	}{
		{"addr", func(c *Config) { c.Addr = " \t" }, "ADDR is required"},
		{"command", func(c *Config) { c.CommandName = "\n" }, "COMMAND_NAME is required"},
		{"slack app", func(c *Config) { c.SlackAppName = " " }, "SLACK_APP_NAME is required"},
	} {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := base
			tt.mut(&cfg)
			if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err=%v, want %q", err, tt.want)
			}
		})
	}
}
