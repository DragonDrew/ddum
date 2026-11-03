package config

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

func validEnv() map[string]string {
	return map[string]string{
		"BOT_URL":          "http://192.168.1.10:8080/",
		"AGENT_SHARED_KEY": "0123456789abcdef0123456789abcdef",
	}
}

func TestDefaults(t *testing.T) {
	c, err := FromMap(validEnv(), "host-3")
	if err != nil {
		t.Fatal(err)
	}
	if c.BotURL != "http://192.168.1.10:8080" {
		t.Errorf("trailing slash not trimmed: %q", c.BotURL)
	}
	if c.HostDisplayName != "host-3" || c.PollIntervalSeconds != 30 || c.InventoryIntervalSeconds != 300 ||
		c.DataDir != "/data" || c.SnapshotRetentionCount != 3 {
		t.Errorf("unexpected defaults: %+v", c)
	}
	// Portainer is in the default list but has no URL, so it is dropped with a warning.
	if got := strings.Join(c.EnabledProviders, ","); got != "compose,docker" {
		t.Errorf("providers = %q", got)
	}
	if !hasWarning(c, "Portainer provider disabled") || !hasWarning(c, "COMPOSE_ALLOWED_ROOTS is not set") {
		t.Errorf("missing warnings: %v", c.Warnings)
	}
}

func TestPollIntervalClamped(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want int
	}{{"1", 3}, {"3", 3}, {"300", 300}, {"900", 300}} {
		m := validEnv()
		m["POLL_INTERVAL_SECONDS"] = tc.in
		c, err := FromMap(m, "h")
		if err != nil {
			t.Fatal(err)
		}
		if c.PollIntervalSeconds != tc.want {
			t.Errorf("POLL_INTERVAL_SECONDS=%s -> %d, want %d", tc.in, c.PollIntervalSeconds, tc.want)
		}
		clamped := tc.in == "1" || tc.in == "900"
		if clamped != hasWarning(c, "POLL_INTERVAL_SECONDS") {
			t.Errorf("POLL_INTERVAL_SECONDS=%s: warning mismatch: %v", tc.in, c.Warnings)
		}
	}
}

func TestAgentIDNormalized(t *testing.T) {
	m := validEnv()
	m["AGENT_ID"] = "0195F2C4-0000-7000-8000-000000000001"
	c, err := FromMap(m, "h")
	if err != nil {
		t.Fatal(err)
	}
	if c.AgentID != "0195f2c4-0000-7000-8000-000000000001" {
		t.Errorf("AgentID = %q", c.AgentID)
	}
}

func TestPortainerEnabled(t *testing.T) {
	m := validEnv()
	m["PORTAINER_URL"] = "https://portainer.lan:9443"
	m["PORTAINER_TOKEN"] = "ptr_secret"
	m["PORTAINER_ENDPOINT_ID"] = "2"
	m["COMPOSE_ALLOWED_ROOTS"] = "/opt/stacks/, /srv/compose"
	c, err := FromMap(m, "h")
	if err != nil {
		t.Fatal(err)
	}
	if !c.HasProvider(ProviderPortainer) {
		t.Error("portainer should be enabled")
	}
	if got := strings.Join(c.ComposeAllowedRoots, ","); got != "/opt/stacks,/srv/compose" {
		t.Errorf("roots = %q", got)
	}
}

func TestInvalid(t *testing.T) {
	cases := map[string]struct {
		set  map[string]string
		want string
	}{
		"missing bot url":   {map[string]string{"BOT_URL": ""}, "BOT_URL"},
		"bot url no scheme": {map[string]string{"BOT_URL": "192.168.1.10:8080"}, "BOT_URL must be an http:// URL"},
		"short key":         {map[string]string{"AGENT_SHARED_KEY": "short"}, "AGENT_SHARED_KEY must be at least 32"},
		"bad agent id":      {map[string]string{"AGENT_ID": "host-3"}, "AGENT_ID"},
		"unknown provider":  {map[string]string{"ENABLED_PROVIDERS": "compose,swarm"}, `unknown provider "swarm"`},
		"relative root":     {map[string]string{"COMPOSE_ALLOWED_ROOTS": "stacks"}, "not an absolute path"},
		"root slash":        {map[string]string{"COMPOSE_ALLOWED_ROOTS": "/"}, `"/" is not allowed`},
		"portainer no token": {map[string]string{
			"PORTAINER_URL": "https://p:9443", "PORTAINER_ENDPOINT_ID": "2"}, "PORTAINER_TOKEN"},
		"portainer no endpoint": {map[string]string{
			"PORTAINER_URL": "https://p:9443", "PORTAINER_TOKEN": "t"}, "PORTAINER_ENDPOINT_ID is required"},
		"bad retention": {map[string]string{"SNAPSHOT_RETENTION_COUNT": "0"}, "SNAPSHOT_RETENTION_COUNT"},
		"bad level":     {map[string]string{"LOG_LEVEL": "verbose"}, "LOG_LEVEL"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			m := validEnv()
			for k, v := range tc.set {
				if v == "" {
					delete(m, k)
				} else {
					m[k] = v
				}
			}
			_, err := FromMap(m, "h")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want error containing %q", err, tc.want)
			}
		})
	}
}

func TestLogValueOmitsSecrets(t *testing.T) {
	m := validEnv()
	m["PORTAINER_URL"] = "https://p:9443"
	m["PORTAINER_TOKEN"] = "ptr_very_secret_token"
	m["PORTAINER_ENDPOINT_ID"] = "2"
	c, err := FromMap(m, "h")
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("config", "config", c)
	for _, secret := range []string{c.AgentSharedKey, c.PortainerToken} {
		if strings.Contains(buf.String(), secret) {
			t.Fatalf("log output contains a secret: %s", buf.String())
		}
	}
}

func hasWarning(c *Config, substr string) bool {
	for _, w := range c.Warnings {
		if strings.Contains(w, substr) {
			return true
		}
	}
	return false
}
