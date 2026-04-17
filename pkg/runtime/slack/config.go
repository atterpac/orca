package slack

import (
	"errors"
	"os"

	"gopkg.in/yaml.v3"
)

// Config carries the slack runtime's settings. Loaded from env (and an
// optional YAML routing file) by LoadConfig, or set directly by callers
// embedding the runtime.
type Config struct {
	// Slack auth + secrets.
	SigningSecret string // required for HTTP Events API mode (NOT Socket Mode)
	BotToken      string // xoxb-... required for posting
	// AppToken enables Socket Mode when set. xapp-... with `connections:write`.
	// Strongly recommended — no public URL needed for inbound events.
	AppToken string

	// Per-post identity override. Requires chat:write.customize bot scope.
	Username  string
	IconEmoji string
	IconURL   string

	// Routing rules (loaded from yaml when ORCA_SLACK_CONFIG is set).
	Routing RoutingConfig

	// HTTP listen address for inbound events when Socket Mode is NOT in use.
	// Default ":7880". Ignored when AppToken is set.
	HTTPListen string
}

// UseSocketMode reports whether the runtime should use Slack's Socket Mode
// transport for inbound events.
func (c *Config) UseSocketMode() bool { return c.AppToken != "" }

type RoutingConfig struct {
	DefaultInboundKind string           `yaml:"default_inbound_kind"`
	Channels           map[string]Route `yaml:"channels,omitempty"`
	DMToBot            *Route           `yaml:"dm_to_bot,omitempty"`
	Outbound           OutboundConfig   `yaml:"outbound,omitempty"`
	Progress           ProgressConfig   `yaml:"progress,omitempty"`
	Decisions          DecisionsConfig  `yaml:"decisions,omitempty"`
}

type Route struct {
	RouteToID   string   `yaml:"route_to_id,omitempty"`
	RouteToTags []string `yaml:"route_to_tags,omitempty"`
	RouteMode   string   `yaml:"route_mode,omitempty"` // any|all
}

type OutboundConfig struct {
	DefaultChannel    string `yaml:"default_channel,omitempty"`
	ResponseToChannel bool   `yaml:"response_to_channel,omitempty"`
}

type ProgressConfig struct {
	EventKinds         []string `yaml:"event_kinds,omitempty"`
	ThrottlePerChannel string   `yaml:"throttle_per_channel,omitempty"`
}

type DecisionsConfig struct {
	DefaultChannel string `yaml:"default_channel,omitempty"`
}

// LoadConfig reads env vars (and the optional ORCA_SLACK_CONFIG yaml).
func LoadConfig() (*Config, error) {
	cfg := &Config{
		SigningSecret: os.Getenv("SLACK_SIGNING_SECRET"),
		BotToken:      os.Getenv("SLACK_BOT_TOKEN"),
		AppToken:      os.Getenv("SLACK_APP_TOKEN"),
		Username:      os.Getenv("ORCA_SLACK_USERNAME"),
		IconEmoji:     os.Getenv("ORCA_SLACK_ICON_EMOJI"),
		IconURL:       os.Getenv("ORCA_SLACK_ICON_URL"),
		HTTPListen:    getenvDefault("ORCA_SLACK_LISTEN", ":7880"),
	}

	if cfg.BotToken == "" {
		return nil, errors.New("SLACK_BOT_TOKEN required")
	}
	if !cfg.UseSocketMode() && cfg.SigningSecret == "" {
		return nil, errors.New("SLACK_SIGNING_SECRET required (verifies inbound events) — or enable Socket Mode by setting SLACK_APP_TOKEN=xapp-...")
	}

	if p := os.Getenv("ORCA_SLACK_CONFIG"); p != "" {
		if err := cfg.Routing.loadYAML(p); err != nil {
			return nil, err
		}
	}
	if cfg.Routing.DefaultInboundKind == "" {
		cfg.Routing.DefaultInboundKind = "discussion"
	}
	return cfg, nil
}

func (r *RoutingConfig) loadYAML(path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return yaml.Unmarshal(b, r)
}

func getenvDefault(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// ResolvedChannelForDecisions picks the channel where decisions post.
// Precedence: decisions.default_channel → outbound.default_channel → "#orca-decisions".
func (c *Config) ResolvedChannelForDecisions() string {
	if c.Routing.Decisions.DefaultChannel != "" {
		return c.Routing.Decisions.DefaultChannel
	}
	if c.Routing.Outbound.DefaultChannel != "" {
		return c.Routing.Outbound.DefaultChannel
	}
	return "#orca-decisions"
}
