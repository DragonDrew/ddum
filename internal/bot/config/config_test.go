package config

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func validEnv() map[string]string {
	return map[string]string{
		"DISCORD_TOKEN":           "discord-secret-token",
		"DISCORD_APPLICATION_ID":  "100000000000000001",
		"DISCORD_GUILD_ID":        "100000000000000002",
		"NOTIFICATION_CHANNEL_ID": "100000000000000003",
		"AUTHORIZED_DISCORD_IDS":  "123456789012345678, 223456789012345678",
		"AGENT_SHARED_KEY":        "0123456789abcdef0123456789abcdef",
	}
}

func TestDefaults(t *testing.T) {
	c, err := FromMap(validEnv())
	if err != nil {
		t.Fatal(err)
	}
	if c.DatabasePath != "/data/ddum.db" || c.Addr() != "0.0.0.0:8080" {
		t.Errorf("unexpected defaults: db=%q addr=%q", c.DatabasePath, c.Addr())
	}
	if c.RegistryCheckInterval != 6*time.Hour || c.CommandLeaseSeconds != 120 ||
		c.CommandExpiryMinutes != 60 || c.StackMissingGraceHours != 24 ||
		c.NotifyIndividualLimit != 5 || c.RegistryConcurrencyPerHost != 4 {
		t.Errorf("unexpected numeric defaults: %+v", c)
	}
	if c.Level != slog.LevelInfo || c.Location.String() != "UTC" {
		t.Errorf("level=%v tz=%v", c.Level, c.Location)
	}
	if got := strings.Join(c.AuthorizedDiscordIDs, ","); got != "123456789012345678,223456789012345678" {
		t.Errorf("authorized IDs = %q", got)
	}
}

func TestMissingRequired(t *testing.T) {
	for _, name := range []string{"DISCORD_TOKEN", "DISCORD_APPLICATION_ID", "DISCORD_GUILD_ID",
		"NOTIFICATION_CHANNEL_ID", "AUTHORIZED_DISCORD_IDS", "AGENT_SHARED_KEY"} {
		t.Run(name, func(t *testing.T) {
			m := validEnv()
			delete(m, name)
			_, err := FromMap(m)
			if err == nil || !strings.Contains(err.Error(), name) {
				t.Fatalf("error should name %s, got %v", name, err)
			}
		})
	}
}

func TestInvalidValues(t *testing.T) {
	cases := map[string]struct{ key, value, want string }{
		"short key":        {"AGENT_SHARED_KEY", "tooshort", "AGENT_SHARED_KEY must be at least 32"},
		"bad guild":        {"DISCORD_GUILD_ID", "my-server", "DISCORD_GUILD_ID must be a Discord ID"},
		"bad user id":      {"AUTHORIZED_DISCORD_IDS", "123456789012345678,drew", `"drew" is not a Discord user ID`},
		"only commas":      {"AUTHORIZED_DISCORD_IDS", " , ", "at least one Discord user ID"},
		"bad level":        {"LOG_LEVEL", "loud", "LOG_LEVEL"},
		"bad port":         {"LISTEN_PORT", "70000", "LISTEN_PORT must be 1-65535"},
		"bad address":      {"LISTEN_ADDRESS", "localhost", "LISTEN_ADDRESS must be an IP"},
		"interval too low": {"REGISTRY_CHECK_INTERVAL", "30s", "REGISTRY_CHECK_INTERVAL must be at least 5m"},
		"bad duration":     {"REGISTRY_CHECK_INTERVAL", "six hours", "REGISTRY_CHECK_INTERVAL"},
		"zero lease":       {"COMMAND_LEASE_SECONDS", "0", "COMMAND_LEASE_SECONDS must be at least 1"},
		"bad tz":           {"TZ", "Mars/Olympus", "unknown time zone"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			m := validEnv()
			m[c.key] = c.value
			_, err := FromMap(m)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("got %v, want error containing %q", err, c.want)
			}
		})
	}
}

func TestLogValueOmitsSecrets(t *testing.T) {
	c, err := FromMap(validEnv())
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("config", "config", c)
	out := buf.String()
	for _, secret := range []string{c.DiscordToken, c.AgentSharedKey} {
		if strings.Contains(out, secret) {
			t.Fatalf("log output contains a secret: %s", out)
		}
	}
}
