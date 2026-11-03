package executor

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// ghcrHosts are the registries GHCR_TOKEN logs in to. lscr.io serves
// LinuxServer images with ghcr.io tokens, so the same login works there.
var ghcrHosts = []string{"ghcr.io", "lscr.io"}

// RegistryLogin writes a docker CLI config.json into dir: the config at base
// (the one docker would otherwise read; it may not exist) with a GHCR_TOKEN
// login added for ghcr.io and lscr.io. Pointing DOCKER_CONFIG at dir makes
// the agent's pulls and compose runs use it. ghcr.io checks only the token;
// the user name is not used.
func RegistryLogin(dir, base, token string) error {
	cfg := map[string]any{}
	data, err := os.ReadFile(base)
	switch {
	case err == nil:
		if err := json.Unmarshal(data, &cfg); err != nil {
			return fmt.Errorf("read %s: %w", base, err)
		}
	case !errors.Is(err, fs.ErrNotExist):
		return err
	}
	auths, _ := cfg["auths"].(map[string]any)
	if auths == nil {
		auths = map[string]any{}
	}
	helpers, _ := cfg["credHelpers"].(map[string]any)
	login := base64.StdEncoding.EncodeToString([]byte("ddum:" + token))
	for _, h := range ghcrHosts {
		auths[h] = map[string]any{"auth": login}
		delete(helpers, h) // a credential helper would win over auths
	}
	cfg["auths"] = auths

	out, err := json.MarshalIndent(cfg, "", "\t")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "config.json"), out, 0o600)
}
