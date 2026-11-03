package executor

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func readConfig(t *testing.T, dir string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestRegistryLoginWithoutBaseConfig(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "out")
	if err := RegistryLogin(dir, filepath.Join(t.TempDir(), "missing.json"), "ghp_x"); err != nil {
		t.Fatal(err)
	}
	auths := readConfig(t, dir)["auths"].(map[string]any)
	want := base64.StdEncoding.EncodeToString([]byte("ddum:ghp_x"))
	for _, h := range []string{"ghcr.io", "lscr.io"} {
		if got := auths[h].(map[string]any)["auth"]; got != want {
			t.Errorf("%s auth = %v, want %s", h, got, want)
		}
	}
}

func TestRegistryLoginKeepsOtherLogins(t *testing.T) {
	base := filepath.Join(t.TempDir(), "config.json")
	os.WriteFile(base, []byte(`{"auths":{"registry.example":{"auth":"b3RoZXI="}},
		"credHelpers":{"ghcr.io":"desktop","gcr.io":"gcloud"},"psFormat":"table"}`), 0o600)
	dir := t.TempDir()
	if err := RegistryLogin(dir, base, "ghp_x"); err != nil {
		t.Fatal(err)
	}
	cfg := readConfig(t, dir)
	auths := cfg["auths"].(map[string]any)
	if auths["registry.example"] == nil || auths["ghcr.io"] == nil || auths["lscr.io"] == nil {
		t.Fatalf("auths = %v", auths)
	}
	helpers := cfg["credHelpers"].(map[string]any)
	if _, ok := helpers["ghcr.io"]; ok || helpers["gcr.io"] != "gcloud" {
		t.Fatalf("credHelpers = %v; want ghcr.io dropped, others kept", helpers)
	}
	if cfg["psFormat"] != "table" {
		t.Fatal("other settings must be kept")
	}
	if fi, _ := os.Stat(filepath.Join(dir, "config.json")); fi.Mode().Perm()&0o077 != 0 && os.PathSeparator == '/' {
		t.Fatalf("config.json mode = %v, want owner-only", fi.Mode().Perm())
	}
}
