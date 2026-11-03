// Package config loads and validates the agent's bootstrap configuration
// .
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path"
	"strings"

	"github.com/DragonDrew/ddum/internal/configenv"
	"github.com/DragonDrew/ddum/internal/ids"
	"github.com/DragonDrew/ddum/internal/logging"
	"github.com/DragonDrew/ddum/internal/protocol"
)

// Poll interval bounds.
const (
	MinPollIntervalSeconds = 3
	MaxPollIntervalSeconds = 300
)

// Provider names accepted in ENABLED_PROVIDERS.
const (
	ProviderPortainer = "portainer"
	ProviderCompose   = "compose"
	ProviderDocker    = "docker"
)

// Secrets lists the variables that also accept a <NAME>_FILE variant.
var Secrets = []string{"AGENT_SHARED_KEY", "PORTAINER_TOKEN", "GHCR_TOKEN"}

// Config is the agent's bootstrap configuration.
type Config struct {
	BotURL          string `env:"BOT_URL,required,notEmpty"`
	BotPublicKey    string `env:"BOT_PUBLIC_KEY"` // the bot's key, if not to be learned on first contact
	AgentSharedKey  string `env:"AGENT_SHARED_KEY,required,notEmpty"`
	AgentID         string `env:"AGENT_ID"`
	HostDisplayName string `env:"HOST_DISPLAY_NAME"`

	PollIntervalSeconds      int    `env:"POLL_INTERVAL_SECONDS" envDefault:"30"`
	InventoryIntervalSeconds int    `env:"INVENTORY_INTERVAL_SECONDS" envDefault:"300"`
	DataDir                  string `env:"DATA_DIR" envDefault:"/data"`
	DockerHost               string `env:"DOCKER_HOST" envDefault:"unix:///var/run/docker.sock"`

	EnabledProviders    []string `env:"ENABLED_PROVIDERS" envDefault:"portainer,compose,docker" envSeparator:","`
	ComposeAllowedRoots []string `env:"COMPOSE_ALLOWED_ROOTS" envSeparator:","`

	PortainerURL         string `env:"PORTAINER_URL"`
	PortainerToken       string `env:"PORTAINER_TOKEN"`
	PortainerEndpointID  int    `env:"PORTAINER_ENDPOINT_ID"`
	PortainerInsecureTLS bool   `env:"PORTAINER_INSECURE_TLS" envDefault:"false"`
	PortainerCAFile      string `env:"PORTAINER_CA_FILE"`

	GHCRToken string `env:"GHCR_TOKEN"` // GitHub token (read:packages) for pulls from ghcr.io and lscr.io

	SnapshotRetentionCount int    `env:"SNAPSHOT_RETENTION_COUNT" envDefault:"3"`
	LogLevel               string `env:"LOG_LEVEL" envDefault:"info"`

	// Derived during validation.
	Level slog.Level `env:"-"`
	// Warnings are non-fatal adjustments (clamping, disabled providers) to
	// log once the logger exists.
	Warnings []string `env:"-"`
}

// Load reads the configuration from the process environment. hostname is
// the default for HOST_DISPLAY_NAME.
func Load(hostname string) (*Config, error) {
	m, err := configenv.FromOS(Secrets)
	if err != nil {
		return nil, err
	}
	return FromMap(m, hostname)
}

// FromMap parses and validates configuration from an environment map.
func FromMap(m map[string]string, hostname string) (*Config, error) {
	var c Config
	if err := configenv.Parse(&c, m); err != nil {
		return nil, err
	}
	if c.HostDisplayName == "" {
		c.HostDisplayName = hostname
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) validate() error {
	var errs []error
	add := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }
	warn := func(format string, a ...any) { c.Warnings = append(c.Warnings, fmt.Sprintf(format, a...)) }

	if u, err := url.Parse(c.BotURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		add("BOT_URL must be an http:// URL such as http://192.168.1.10:8080, got %q", c.BotURL)
	} else {
		c.BotURL = strings.TrimRight(c.BotURL, "/")
	}
	if len(c.AgentSharedKey) < protocol.MinSharedKeyLength {
		add("AGENT_SHARED_KEY must be at least %d characters (got %d)", protocol.MinSharedKeyLength, len(c.AgentSharedKey))
	}
	if c.AgentID != "" {
		id, err := ids.Parse(c.AgentID)
		if err != nil {
			add("AGENT_ID: %v (generate one with `ddum-agent gen-id`)", err)
		}
		c.AgentID = id
	}
	if strings.TrimSpace(c.HostDisplayName) == "" {
		add("HOST_DISPLAY_NAME is empty and the hostname could not be determined")
	}

	switch {
	case c.PollIntervalSeconds < MinPollIntervalSeconds:
		warn("POLL_INTERVAL_SECONDS=%d is below the minimum; using %d", c.PollIntervalSeconds, MinPollIntervalSeconds)
		c.PollIntervalSeconds = MinPollIntervalSeconds
	case c.PollIntervalSeconds > MaxPollIntervalSeconds:
		warn("POLL_INTERVAL_SECONDS=%d is above the maximum; using %d", c.PollIntervalSeconds, MaxPollIntervalSeconds)
		c.PollIntervalSeconds = MaxPollIntervalSeconds
	}
	if c.InventoryIntervalSeconds < 30 {
		add("INVENTORY_INTERVAL_SECONDS must be at least 30, got %d", c.InventoryIntervalSeconds)
	}
	if c.DataDir == "" {
		add("DATA_DIR must not be empty")
	}
	if c.DockerHost == "" {
		add("DOCKER_HOST must not be empty")
	}
	if c.SnapshotRetentionCount < 1 {
		add("SNAPSHOT_RETENTION_COUNT must be at least 1, got %d", c.SnapshotRetentionCount)
	}
	level, err := logging.ParseLevel(c.LogLevel)
	if err != nil {
		add("LOG_LEVEL: %v", err)
	}
	c.Level = level

	// Providers.
	var providers []string
	seen := map[string]bool{}
	for _, p := range c.EnabledProviders {
		p = strings.ToLower(strings.TrimSpace(p))
		if p == "" || seen[p] {
			continue
		}
		switch p {
		case ProviderPortainer, ProviderCompose, ProviderDocker:
		default:
			add("ENABLED_PROVIDERS: unknown provider %q (want portainer, compose, docker)", p)
			continue
		}
		seen[p] = true
		providers = append(providers, p)
	}
	if seen[ProviderPortainer] && c.PortainerURL == "" {
		warn("Portainer provider disabled: PORTAINER_URL is not set")
		providers = remove(providers, ProviderPortainer)
		seen[ProviderPortainer] = false
	}
	c.EnabledProviders = providers
	if seen[ProviderPortainer] {
		if u, err := url.Parse(c.PortainerURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			add("PORTAINER_URL must be an http(s) URL, got %q", c.PortainerURL)
		}
		if c.PortainerToken == "" {
			add("PORTAINER_TOKEN (or PORTAINER_TOKEN_FILE) is required when the Portainer provider is enabled")
		}
		if c.PortainerEndpointID < 1 {
			add("PORTAINER_ENDPOINT_ID is required when the Portainer provider is enabled")
		}
	}

	// Allowed roots: absolute, cleaned, no duplicates. These are paths on the
	// Linux host, so they are handled with forward slashes on every platform.
	var roots []string
	for _, r := range c.ComposeAllowedRoots {
		r = strings.TrimSpace(r)
		if r == "" {
			continue
		}
		if !path.IsAbs(r) {
			add("COMPOSE_ALLOWED_ROOTS: %q is not an absolute path", r)
			continue
		}
		r = path.Clean(r)
		if r == "/" {
			add("COMPOSE_ALLOWED_ROOTS: \"/\" is not allowed; list the directories that hold Compose files")
			continue
		}
		roots = append(roots, r)
	}
	c.ComposeAllowedRoots = roots
	if seen[ProviderCompose] && len(roots) == 0 {
		warn("COMPOSE_ALLOWED_ROOTS is not set: Compose stacks will be discovered but not deployable")
	}

	return errors.Join(errs...)
}

// HasProvider reports whether a provider is enabled.
func (c *Config) HasProvider(p string) bool {
	for _, x := range c.EnabledProviders {
		if x == p {
			return true
		}
	}
	return false
}

// LogValue lets the config be logged at startup with secrets omitted.
func (c *Config) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("bot_url", c.BotURL),
		slog.String("host_display_name", c.HostDisplayName),
		slog.Bool("agent_id_pinned", c.AgentID != ""),
		slog.Int("poll_interval_seconds", c.PollIntervalSeconds),
		slog.Int("inventory_interval_seconds", c.InventoryIntervalSeconds),
		slog.String("data_dir", c.DataDir),
		slog.String("docker_host", c.DockerHost),
		slog.Any("enabled_providers", c.EnabledProviders),
		slog.Any("compose_allowed_roots", c.ComposeAllowedRoots),
		slog.String("portainer_url", c.PortainerURL),
		slog.Bool("ghcr_token_set", c.GHCRToken != ""),
		slog.String("log_level", c.Level.String()),
	)
}

// Hostname returns os.Hostname or "" on error.
func Hostname() string {
	h, err := os.Hostname()
	if err != nil {
		return ""
	}
	return h
}

func remove(list []string, v string) []string {
	out := list[:0]
	for _, x := range list {
		if x != v {
			out = append(out, x)
		}
	}
	return out
}
