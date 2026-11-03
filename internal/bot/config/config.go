// Package config loads and validates the bot's bootstrap configuration
// . Everything else is managed from Discord.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/DragonDrew/ddum/internal/configenv"
	"github.com/DragonDrew/ddum/internal/logging"
	"github.com/DragonDrew/ddum/internal/protocol"
)

// Secrets lists the variables that also accept a <NAME>_FILE variant.
var Secrets = []string{"DISCORD_TOKEN", "AGENT_SHARED_KEY", "GHCR_TOKEN"}

// Config is the bot's bootstrap configuration.
type Config struct {
	DiscordToken          string   `env:"DISCORD_TOKEN,required,notEmpty"`
	DiscordApplicationID  string   `env:"DISCORD_APPLICATION_ID,required,notEmpty"`
	DiscordGuildID        string   `env:"DISCORD_GUILD_ID,required,notEmpty"`
	NotificationChannelID string   `env:"NOTIFICATION_CHANNEL_ID,required,notEmpty"`
	AuthorizedDiscordIDs  []string `env:"AUTHORIZED_DISCORD_IDS,required,notEmpty" envSeparator:","`
	AgentSharedKey        string   `env:"AGENT_SHARED_KEY,required,notEmpty"`

	DatabasePath  string `env:"DATABASE_PATH" envDefault:"/data/ddum.db"`
	ListenAddress string `env:"LISTEN_ADDRESS" envDefault:"0.0.0.0"`
	ListenPort    int    `env:"LISTEN_PORT" envDefault:"8080"`
	LogLevel      string `env:"LOG_LEVEL" envDefault:"info"`

	RegistryCheckInterval      time.Duration `env:"REGISTRY_CHECK_INTERVAL" envDefault:"6h"`
	RegistryConcurrencyPerHost int           `env:"REGISTRY_CONCURRENCY_PER_HOST" envDefault:"4"`
	DockerConfig               string        `env:"DOCKER_CONFIG"`
	GHCRToken                  string        `env:"GHCR_TOKEN"` // GitHub token (read:packages) for ghcr.io and lscr.io

	CommandLeaseSeconds    int `env:"COMMAND_LEASE_SECONDS" envDefault:"120"`
	CommandExpiryMinutes   int `env:"COMMAND_EXPIRY_MINUTES" envDefault:"60"`
	StackMissingGraceHours int `env:"STACK_MISSING_GRACE_HOURS" envDefault:"24"`
	NotifyIndividualLimit  int `env:"NOTIFY_INDIVIDUAL_LIMIT" envDefault:"5"`

	TZ string `env:"TZ" envDefault:"UTC"`

	// DiscordMode is "discord", or "fake" for the end-to-end test rig only: no
	// Discord connection, messages kept in memory, and signed test endpoints
	// under /test/v1/.
	DiscordMode string `env:"DISCORD_MODE" envDefault:"discord"`

	// Derived during validation.
	Level    slog.Level     `env:"-"`
	Location *time.Location `env:"-"`
}

// Load reads the configuration from the process environment.
func Load() (*Config, error) {
	m, err := configenv.FromOS(Secrets)
	if err != nil {
		return nil, err
	}
	return FromMap(m)
}

// FromMap parses and validates configuration from an environment map.
func FromMap(m map[string]string) (*Config, error) {
	var c Config
	if err := configenv.Parse(&c, m); err != nil {
		return nil, err
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) validate() error {
	var errs []error
	add := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	for _, f := range []struct{ name, v string }{
		{"DISCORD_APPLICATION_ID", c.DiscordApplicationID},
		{"DISCORD_GUILD_ID", c.DiscordGuildID},
		{"NOTIFICATION_CHANNEL_ID", c.NotificationChannelID},
	} {
		if !isSnowflake(f.v) {
			add("%s must be a Discord ID (digits only), got %q", f.name, f.v)
		}
	}
	ids := c.AuthorizedDiscordIDs[:0]
	for _, id := range c.AuthorizedDiscordIDs {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if !isSnowflake(id) {
			add("AUTHORIZED_DISCORD_IDS: %q is not a Discord user ID", id)
		}
		ids = append(ids, id)
	}
	c.AuthorizedDiscordIDs = ids
	if len(ids) == 0 {
		add("AUTHORIZED_DISCORD_IDS must list at least one Discord user ID")
	}
	if len(c.AgentSharedKey) < protocol.MinSharedKeyLength {
		add("AGENT_SHARED_KEY must be at least %d characters (got %d)", protocol.MinSharedKeyLength, len(c.AgentSharedKey))
	}
	if c.DiscordMode != "discord" && c.DiscordMode != "fake" {
		add("DISCORD_MODE must be discord or fake, got %q", c.DiscordMode)
	}
	if c.DatabasePath == "" {
		add("DATABASE_PATH must not be empty")
	}
	if net.ParseIP(c.ListenAddress) == nil {
		add("LISTEN_ADDRESS must be an IP address, got %q", c.ListenAddress)
	}
	if c.ListenPort < 1 || c.ListenPort > 65535 {
		add("LISTEN_PORT must be 1-65535, got %d", c.ListenPort)
	}
	level, err := logging.ParseLevel(c.LogLevel)
	if err != nil {
		add("LOG_LEVEL: %v", err)
	}
	c.Level = level
	if c.RegistryCheckInterval < 5*time.Minute {
		add("REGISTRY_CHECK_INTERVAL must be at least 5m, got %s", c.RegistryCheckInterval)
	}
	for _, f := range []struct {
		name string
		v    int
	}{
		{"REGISTRY_CONCURRENCY_PER_HOST", c.RegistryConcurrencyPerHost},
		{"COMMAND_LEASE_SECONDS", c.CommandLeaseSeconds},
		{"COMMAND_EXPIRY_MINUTES", c.CommandExpiryMinutes},
		{"STACK_MISSING_GRACE_HOURS", c.StackMissingGraceHours},
		{"NOTIFY_INDIVIDUAL_LIMIT", c.NotifyIndividualLimit},
	} {
		if f.v < 1 {
			add("%s must be at least 1, got %d", f.name, f.v)
		}
	}
	loc, err := time.LoadLocation(c.TZ)
	if err != nil {
		add("TZ: unknown time zone %q", c.TZ)
	}
	c.Location = loc

	return errors.Join(errs...)
}

// Addr is the HTTP listen address.
func (c *Config) Addr() string {
	return net.JoinHostPort(c.ListenAddress, strconv.Itoa(c.ListenPort))
}

// LogValue lets the config be logged at startup with secrets omitted.
func (c *Config) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("database_path", c.DatabasePath),
		slog.String("listen", c.Addr()),
		slog.String("log_level", c.Level.String()),
		slog.String("guild_id", c.DiscordGuildID),
		slog.String("notification_channel_id", c.NotificationChannelID),
		slog.Int("authorized_users", len(c.AuthorizedDiscordIDs)),
		slog.String("registry_check_interval", c.RegistryCheckInterval.String()),
		slog.Bool("docker_config_set", c.DockerConfig != ""),
		slog.Bool("ghcr_token_set", c.GHCRToken != ""),
		slog.String("tz", c.TZ),
	)
}

// isSnowflake reports whether s looks like a Discord ID: 17-20 digits.
func isSnowflake(s string) bool {
	if len(s) < 17 || len(s) > 20 {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
