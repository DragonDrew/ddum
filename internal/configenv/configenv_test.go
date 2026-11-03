package configenv

import (
	"errors"
	"io/fs"
	"strings"
	"testing"
)

func fakeFiles(files map[string]string) func(string) ([]byte, error) {
	return func(p string) ([]byte, error) {
		if v, ok := files[p]; ok {
			return []byte(v), nil
		}
		return nil, fs.ErrNotExist
	}
}

func TestResolve(t *testing.T) {
	files := fakeFiles(map[string]string{
		"/run/secrets/token": "file-token\n",
		"/run/secrets/key":   "file-key\r\n",
	})
	environ := []string{
		"DISCORD_TOKEN=env-token",
		"DISCORD_TOKEN_FILE=/run/secrets/token",
		"AGENT_SHARED_KEY_FILE=/run/secrets/key",
		"PLAIN=a=b",
		"EMPTY_SECRET_FILE=",
		"EMPTY_SECRET=kept",
	}
	env, err := Resolve(environ, []string{"DISCORD_TOKEN", "AGENT_SHARED_KEY", "EMPTY_SECRET", "UNSET"}, files)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"DISCORD_TOKEN":    "file-token", // _FILE wins over the plain variable
		"AGENT_SHARED_KEY": "file-key",   // CRLF trimmed
		"PLAIN":            "a=b",        // only the first '=' splits
		"EMPTY_SECRET":     "kept",       // empty _FILE is ignored
	}
	for k, v := range want {
		if env[k] != v {
			t.Errorf("%s = %q, want %q", k, env[k], v)
		}
	}
	if _, ok := env["UNSET"]; ok {
		t.Error("UNSET should stay unset")
	}
}

func TestResolveMissingFile(t *testing.T) {
	_, err := Resolve([]string{"DISCORD_TOKEN_FILE=/nope"}, []string{"DISCORD_TOKEN"}, fakeFiles(nil))
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("got %v, want not-exist", err)
	}
	if !strings.Contains(err.Error(), "DISCORD_TOKEN_FILE") {
		t.Fatalf("error should name the variable: %v", err)
	}
}
