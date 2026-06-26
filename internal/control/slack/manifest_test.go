package slack

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/sangyi/workspace-brain/internal/control/gateway"
)

func TestManifestConfigManifestBuildsSlackManifest(t *testing.T) {
	t.Parallel()
	manifest, err := validManifestConfig().Manifest()
	if err != nil {
		t.Fatalf("Manifest() error = %v", err)
	}
	if manifest.DisplayInformation.Name != "Workspace Brain" {
		t.Fatalf("app name = %q", manifest.DisplayInformation.Name)
	}
	if manifest.Features.BotUser.DisplayName != "Workspace Brain" || manifest.Features.BotUser.AlwaysOnline {
		t.Fatalf("bot user = %+v", manifest.Features.BotUser)
	}
	if len(manifest.Features.SlashCommands) != 1 {
		t.Fatalf("slash command count = %d", len(manifest.Features.SlashCommands))
	}
	command := manifest.Features.SlashCommands[0]
	if command.Command != "/brain" || command.URL != "https://example.com/slack/command" {
		t.Fatalf("slash command = %+v", command)
	}
	if command.Description != "Workspace Brain command gateway" || command.UsageHint != "create|ingest|ask|discover|status|admin" || command.ShouldEscape {
		t.Fatalf("slash command metadata = %+v", command)
	}
	if !manifest.Settings.Interactivity.IsEnabled || manifest.Settings.Interactivity.RequestURL != "https://example.com/slack/interactivity" {
		t.Fatalf("interactivity = %+v", manifest.Settings.Interactivity)
	}
	if manifest.Settings.OrgDeployEnabled || manifest.Settings.SocketModeEnabled || manifest.Settings.TokenRotationEnabled {
		t.Fatalf("settings should keep optional Slack modes disabled: %+v", manifest.Settings)
	}
}

func TestManifestConfigManifestTrimsValuesAndNormalizesCommandSlash(t *testing.T) {
	t.Parallel()
	manifest, err := (ManifestConfig{
		AppName:          "  Workspace Brain  ",
		CommandName:      "  /brain  ",
		CommandURL:       "  https://example.com/slack/command  ",
		InteractivityURL: "  https://example.com/slack/interactivity  ",
	}).Manifest()
	if err != nil {
		t.Fatalf("Manifest() error = %v", err)
	}
	if manifest.DisplayInformation.Name != "Workspace Brain" {
		t.Fatalf("app name = %q", manifest.DisplayInformation.Name)
	}
	if manifest.Features.SlashCommands[0].Command != "/brain" || manifest.Features.SlashCommands[0].URL != "https://example.com/slack/command" {
		t.Fatalf("slash command = %+v", manifest.Features.SlashCommands[0])
	}
	if manifest.Settings.Interactivity.RequestURL != "https://example.com/slack/interactivity" {
		t.Fatalf("request URL = %q", manifest.Settings.Interactivity.RequestURL)
	}
}

func TestManifestConfigManifestJSON(t *testing.T) {
	t.Parallel()
	body, err := validManifestConfig().ManifestJSON()
	if err != nil {
		t.Fatalf("ManifestJSON() error = %v", err)
	}
	if !strings.Contains(string(body), "\n  \"display_information\":") {
		t.Fatalf("ManifestJSON() was not indented: %s", body)
	}
	var decoded Manifest
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("json decode: %v body=%s", err, body)
	}
	if decoded.Features.SlashCommands[0].Command != "/brain" || decoded.Settings.Interactivity.RequestURL == "" {
		t.Fatalf("decoded manifest = %+v", decoded)
	}
}

func TestManifestConfigManifestJSONReturnsValidationError(t *testing.T) {
	t.Parallel()
	body, err := (ManifestConfig{}).ManifestJSON()
	if err == nil {
		t.Fatalf("ManifestJSON() error = nil body=%s", body)
	}
	if body != nil {
		t.Fatalf("ManifestJSON() body = %s, want nil", body)
	}
}

func TestManifestConfigValidateRejectsInvalidConfig(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		cfg  ManifestConfig
		want string
	}{
		{"missing app", ManifestConfig{CommandName: "brain", CommandURL: "https://example.com/slack/command", InteractivityURL: "https://example.com/slack/interactivity"}, "app name is required"},
		{"missing command", ManifestConfig{AppName: "Workspace Brain", CommandURL: "https://example.com/slack/command", InteractivityURL: "https://example.com/slack/interactivity"}, "command name is required"},
		{"bad command URL", ManifestConfig{AppName: "Workspace Brain", CommandName: "brain", CommandURL: "http://example.com/slack/command", InteractivityURL: "https://example.com/slack/interactivity"}, "command URL must be an absolute HTTPS URL"},
		{"bad command URL parse", ManifestConfig{AppName: "Workspace Brain", CommandName: "brain", CommandURL: "https://%", InteractivityURL: "https://example.com/slack/interactivity"}, "command URL must be an absolute HTTPS URL"},
		{"bad interactivity URL", ManifestConfig{AppName: "Workspace Brain", CommandName: "brain", CommandURL: "https://example.com/slack/command", InteractivityURL: "/slack/interactivity"}, "interactivity URL must be an absolute HTTPS URL"},
		{"bad interactivity URL parse", ManifestConfig{AppName: "Workspace Brain", CommandName: "brain", CommandURL: "https://example.com/slack/command", InteractivityURL: "https://%"}, "interactivity URL must be an absolute HTTPS URL"},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := tt.cfg.Validate()
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Validate() error = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestManifestConfigValidateAcceptsValidConfig(t *testing.T) {
	t.Parallel()
	if err := validManifestConfig().Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
}

func validManifestConfig() ManifestConfig {
	return ManifestConfig{
		AppName:          "Workspace Brain",
		CommandName:      "brain",
		CommandURL:       "https://example.com/slack/command",
		InteractivityURL: "https://example.com/slack/interactivity",
	}
}

func (f *fakeGateway) SetProjectState(_ context.Context, cmd gateway.SetProjectStateCommand) (gateway.SetProjectStateResult, error) {
	if f.err != nil {
		return gateway.SetProjectStateResult{}, f.err
	}
	return gateway.SetProjectStateResult{TenantID: cmd.TenantID, State: cmd.State}, nil
}
