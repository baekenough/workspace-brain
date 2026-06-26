package slack

import (
	"encoding/json"
	"net/url"
	"strings"

	"github.com/sangyi/workspace-brain/pkg/brainapi"
)

// ManifestConfig contains the Slack app manifest values that vary per deployment.
type ManifestConfig struct {
	AppName          string
	CommandName      string
	CommandURL       string
	InteractivityURL string
}

// Manifest is the Slack app manifest shape emitted by ManifestConfig.Manifest.
type Manifest struct {
	DisplayInformation ManifestDisplayInformation `json:"display_information"`
	Features           ManifestFeatures           `json:"features"`
	Settings           ManifestSettings           `json:"settings"`
}

// ManifestDisplayInformation configures the app name shown in Slack.
type ManifestDisplayInformation struct {
	Name string `json:"name"`
}

// ManifestFeatures configures the Slack frontend surfaces this adapter owns.
type ManifestFeatures struct {
	BotUser       ManifestBotUser        `json:"bot_user"`
	SlashCommands []ManifestSlashCommand `json:"slash_commands"`
}

// ManifestBotUser configures the Slack bot display name.
type ManifestBotUser struct {
	DisplayName  string `json:"display_name"`
	AlwaysOnline bool   `json:"always_online"`
}

// ManifestSlashCommand configures one slash command endpoint.
type ManifestSlashCommand struct {
	Command      string `json:"command"`
	URL          string `json:"url"`
	Description  string `json:"description"`
	UsageHint    string `json:"usage_hint"`
	ShouldEscape bool   `json:"should_escape"`
}

// ManifestSettings configures app-level request routing settings.
type ManifestSettings struct {
	Interactivity        ManifestInteractivity `json:"interactivity"`
	OrgDeployEnabled     bool                  `json:"org_deploy_enabled"`
	SocketModeEnabled    bool                  `json:"socket_mode_enabled"`
	TokenRotationEnabled bool                  `json:"token_rotation_enabled"`
}

// ManifestInteractivity configures Slack interactivity request routing.
type ManifestInteractivity struct {
	IsEnabled  bool   `json:"is_enabled"`
	RequestURL string `json:"request_url"`
}

// Validate checks whether cfg can produce a Slack app manifest.
func (cfg ManifestConfig) Validate() error {
	const op = "slack_manifest_validate"
	if strings.TrimSpace(cfg.AppName) == "" {
		return brainapi.E(brainapi.KindInvalid, op, "app name is required", nil)
	}
	if normalizedCommandName(cfg.CommandName) == "" {
		return brainapi.E(brainapi.KindInvalid, op, "command name is required", nil)
	}
	if !validHTTPSURL(cfg.CommandURL) {
		return brainapi.E(brainapi.KindInvalid, op, "command URL must be an absolute HTTPS URL", nil)
	}
	if !validHTTPSURL(cfg.InteractivityURL) {
		return brainapi.E(brainapi.KindInvalid, op, "interactivity URL must be an absolute HTTPS URL", nil)
	}
	return nil
}

// Manifest builds a Slack app manifest without introducing another frontend adapter.
func (cfg ManifestConfig) Manifest() (Manifest, error) {
	if err := cfg.Validate(); err != nil {
		return Manifest{}, err
	}
	appName := strings.TrimSpace(cfg.AppName)
	return Manifest{
		DisplayInformation: ManifestDisplayInformation{Name: appName},
		Features: ManifestFeatures{
			BotUser: ManifestBotUser{DisplayName: appName, AlwaysOnline: false},
			SlashCommands: []ManifestSlashCommand{{
				Command:      "/" + normalizedCommandName(cfg.CommandName),
				URL:          strings.TrimSpace(cfg.CommandURL),
				Description:  appName + " command gateway",
				UsageHint:    "create|ingest|ask|discover|status|admin",
				ShouldEscape: false,
			}},
		},
		Settings: ManifestSettings{
			Interactivity: ManifestInteractivity{IsEnabled: true, RequestURL: strings.TrimSpace(cfg.InteractivityURL)},
		},
	}, nil
}

// ManifestJSON returns a deterministic JSON Slack app manifest for cfg.
func (cfg ManifestConfig) ManifestJSON() ([]byte, error) {
	manifest, err := cfg.Manifest()
	if err != nil {
		return nil, err
	}
	return json.MarshalIndent(manifest, "", "  ")
}

func normalizedCommandName(command string) string {
	return strings.TrimPrefix(strings.TrimSpace(command), "/")
}

func validHTTPSURL(raw string) bool {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	return err == nil && parsed.Scheme == "https" && parsed.Host != ""
}
