// Package executor runs operations on the agent: the journal,
// safety rules, the Compose deploy steps, verification, and event reporting.
package executor

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/DragonDrew/ddum/internal/ids"
	"github.com/DragonDrew/ddum/internal/protocol"
)

// Journal states.
const (
	JournalRunning = "running"
	JournalDone    = "done"
)

// JournalRetention is how long finished entries are kept.
const JournalRetention = 7 * 24 * time.Hour

// Entry is one operation's journal file.
type Entry struct {
	OperationID string           `json:"operation_id"`
	Type        string           `json:"type"`
	State       string           `json:"state"` // running | done
	NextSeq     int              `json:"next_seq"`
	Unacked     []protocol.Event `json:"unacked"`
	SnapshotIDs []string         `json:"snapshot_ids,omitempty"`
	Payload     json.RawMessage  `json:"payload,omitempty"`
	CreatedAt   time.Time        `json:"created_at"`
	FinishedAt  time.Time        `json:"finished_at,omitzero"`
}

// Journal stores entries in DATA_DIR/journal/<operation_id>.json, written
// atomically (write, sync, rename). It is safe for concurrent use.
type Journal struct {
	dir string
	mu  sync.Mutex
}

// OpenJournal creates the journal directory if needed.
func OpenJournal(dataDir string) (*Journal, error) {
	dir := filepath.Join(dataDir, "journal")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, err
	}
	return &Journal{dir: dir}, nil
}

func (j *Journal) path(id string) (string, error) {
	if _, err := ids.Parse(id); err != nil {
		return "", fmt.Errorf("operation ID: %w", err)
	}
	return filepath.Join(j.dir, id+".json"), nil
}

// Get returns an entry, or false if the operation is unknown.
func (j *Journal) Get(id string) (*Entry, bool, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.get(id)
}

func (j *Journal) get(id string) (*Entry, bool, error) {
	p, err := j.path(id)
	if err != nil {
		return nil, false, err
	}
	b, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	var e Entry
	if err := json.Unmarshal(b, &e); err != nil {
		return nil, false, fmt.Errorf("journal %s is corrupt: %w", p, err)
	}
	return &e, true, nil
}

// Create stores a new entry; it fails if the operation is already journaled,
// which is what makes a duplicate command a no-op.
func (j *Journal) Create(e *Entry) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if _, ok, err := j.get(e.OperationID); err != nil || ok {
		if ok {
			return fmt.Errorf("operation %s is already journaled", e.OperationID)
		}
		return err
	}
	return j.save(e)
}

// Update loads an entry, applies fn, and saves it.
func (j *Journal) Update(id string, fn func(*Entry)) (*Entry, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	e, ok, err := j.get(id)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("operation %s is not journaled", id)
	}
	fn(e)
	return e, j.save(e)
}

// Append adds events to an entry, numbering them, and returns them.
func (j *Journal) Append(id string, at time.Time, events ...protocol.Event) ([]protocol.Event, error) {
	var out []protocol.Event
	_, err := j.Update(id, func(e *Entry) {
		for _, ev := range events {
			if e.NextSeq < 1 {
				e.NextSeq = 1
			}
			ev.Seq = e.NextSeq
			e.NextSeq++
			if ev.At == "" {
				ev.At = at.UTC().Format(time.RFC3339)
			}
			if protocol.IsTerminalEvent(ev.Type) {
				e.State, e.FinishedAt = JournalDone, at
			}
			e.Unacked = append(e.Unacked, ev)
			out = append(out, ev)
		}
	})
	return out, err
}

// Ack drops events up to and including seq.
func (j *Journal) Ack(id string, seq int) error {
	_, err := j.Update(id, func(e *Entry) {
		kept := e.Unacked[:0]
		for _, ev := range e.Unacked {
			if ev.Seq > seq {
				kept = append(kept, ev)
			}
		}
		e.Unacked = kept
	})
	return err
}

// List returns every entry, oldest first.
func (j *Journal) List() ([]*Entry, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	names, err := os.ReadDir(j.dir)
	if err != nil {
		return nil, err
	}
	var out []*Entry
	for _, n := range names {
		if !strings.HasSuffix(n.Name(), ".json") {
			continue
		}
		e, ok, err := j.get(strings.TrimSuffix(n.Name(), ".json"))
		if err != nil || !ok {
			continue
		}
		out = append(out, e)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].CreatedAt.Before(out[b].CreatedAt) })
	return out, nil
}

// Prune removes finished, fully acknowledged entries older than retention.
func (j *Journal) Prune(now time.Time) {
	entries, err := j.List()
	if err != nil {
		return
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	for _, e := range entries {
		if e.State == JournalDone && len(e.Unacked) == 0 && now.Sub(e.FinishedAt) > JournalRetention {
			if p, err := j.path(e.OperationID); err == nil {
				os.Remove(p)
			}
		}
	}
}

func (j *Journal) save(e *Entry) error {
	p, err := j.path(e.OperationID)
	if err != nil {
		return err
	}
	b, err := json.MarshalIndent(e, "", "  ")
	if err != nil {
		return err
	}
	return WriteFileAtomic(p, b, 0o640)
}

// WriteFileAtomic writes data to a temporary file in the same directory,
// syncs it, and renames it over path, so readers never see a partial file.
func WriteFileAtomic(path string, data []byte, perm fs.FileMode) error {
	return writeAtomic(path, data, perm, nil)
}

// writeAtomic is WriteFileAtomic with a chance to adjust the temporary file
// before it takes the place of path.
func writeAtomic(path string, data []byte, perm fs.FileMode, prepare func(*os.File)) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if prepare != nil {
		prepare(tmp)
	}
	_ = tmp.Chmod(perm) // best effort: Windows cannot chmod an open file
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
