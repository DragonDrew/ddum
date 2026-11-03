package identity

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/DragonDrew/ddum/internal/protocol"
)

// Files in DATA_DIR: this agent's private key, and the bot's public key once
// pinned.
const (
	KeyFileName    = "agent_key"
	BotKeyFileName = "bot_key"
)

// LoadKey returns the agent's private key, creating it on first start. Its
// file is readable by root only.
func LoadKey(dataDir string) (ed25519.PrivateKey, error) {
	path := filepath.Join(dataDir, KeyFileName)
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		b, err := base64.RawStdEncoding.DecodeString(strings.TrimSpace(string(data)))
		if err != nil || len(b) != ed25519.PrivateKeySize {
			return nil, fmt.Errorf("%s is corrupt (delete it to generate a new key, then accept the new key in Discord)", path)
		}
		return ed25519.PrivateKey(b), nil
	case !errors.Is(err, fs.ErrNotExist):
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	priv, err := protocol.NewKey()
	if err != nil {
		return nil, err
	}
	if err := writeAtomic(path, base64.RawStdEncoding.EncodeToString(priv)+"\n"); err != nil {
		return nil, fmt.Errorf("persist agent key: %w", err)
	}
	return priv, nil
}

// LoadBotKey returns the pinned bot key, or "" if none is pinned.
func LoadBotKey(dataDir string) (string, error) {
	data, err := os.ReadFile(filepath.Join(dataDir, BotKeyFileName))
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

// SaveBotKey pins the bot's key.
func SaveBotKey(dataDir, key string) error {
	return writeAtomic(filepath.Join(dataDir, BotKeyFileName), key+"\n")
}
