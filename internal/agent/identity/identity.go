// Package identity resolves the agent's persistent ID.
package identity

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/DragonDrew/ddum/internal/ids"
)

// FileName is the file in DATA_DIR that stores a generated agent ID.
const FileName = "agent_id"

// Source says where the resolved ID came from.
type Source string

const (
	SourceConfig    Source = "AGENT_ID"
	SourceFile      Source = "data_dir"
	SourceGenerated Source = "generated"
)

// Result is the resolved agent ID. Warning is non-empty when AGENT_ID
// disagrees with a previously stored ID.
type Result struct {
	ID      string
	Source  Source
	Warning string
}

// Resolve returns the agent ID: AGENT_ID when set, otherwise the ID stored in
// dataDir, otherwise a new UUIDv7 that is persisted atomically to dataDir.
func Resolve(dataDir, configured string, gen ids.Generator) (Result, error) {
	path := filepath.Join(dataDir, FileName)
	stored, err := readStored(path)
	if err != nil {
		return Result{}, err
	}

	if configured != "" {
		r := Result{ID: configured, Source: SourceConfig}
		if stored != "" && stored != configured {
			r.Warning = fmt.Sprintf("AGENT_ID %s differs from the ID stored in %s (%s); using AGENT_ID", configured, path, stored)
		}
		return r, nil
	}
	if stored != "" {
		return Result{ID: stored, Source: SourceFile}, nil
	}

	id := gen.New()
	if err := writeAtomic(path, id+"\n"); err != nil {
		return Result{}, fmt.Errorf("persist agent ID: %w", err)
	}
	return Result{ID: id, Source: SourceGenerated}, nil
}

func readStored(path string) (string, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	id, err := ids.Parse(strings.TrimSpace(string(data)))
	if err != nil {
		return "", fmt.Errorf("%s is corrupt: %w (delete it or set AGENT_ID)", path, err)
	}
	return id, nil
}

// writeAtomic writes via a temp file and rename so a crash never leaves a
// half-written ID behind.
func writeAtomic(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".agent_id-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(content); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
