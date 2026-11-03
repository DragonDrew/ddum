package identity

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type fixedID string

func (f fixedID) New() string { return string(f) }

const (
	idA = "0195f2c4-0000-7000-8000-00000000000a"
	idB = "0195f2c4-0000-7000-8000-00000000000b"
)

func TestGeneratedThenReused(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")

	r, err := Resolve(dir, "", fixedID(idA))
	if err != nil {
		t.Fatal(err)
	}
	if r.ID != idA || r.Source != SourceGenerated {
		t.Fatalf("first start: %+v", r)
	}

	// A second start must reuse the stored ID, not generate another.
	r, err = Resolve(dir, "", fixedID(idB))
	if err != nil {
		t.Fatal(err)
	}
	if r.ID != idA || r.Source != SourceFile {
		t.Fatalf("second start: %+v", r)
	}
}

func TestConfiguredWins(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, FileName), []byte(idA+"\n"), 0o600)

	r, err := Resolve(dir, idB, fixedID("unused"))
	if err != nil {
		t.Fatal(err)
	}
	if r.ID != idB || r.Source != SourceConfig || !strings.Contains(r.Warning, "differs") {
		t.Fatalf("got %+v", r)
	}

	r, _ = Resolve(dir, idA, fixedID("unused"))
	if r.Warning != "" {
		t.Fatalf("matching IDs should not warn: %q", r.Warning)
	}
}

func TestCorruptFile(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, FileName), []byte("not-a-uuid"), 0o600)
	if _, err := Resolve(dir, "", fixedID(idA)); err == nil || !strings.Contains(err.Error(), "corrupt") {
		t.Fatalf("got %v, want corrupt-file error", err)
	}
}
