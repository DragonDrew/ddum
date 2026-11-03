//go:build !windows

package executor

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/DragonDrew/ddum/internal/ids"
	"github.com/DragonDrew/ddum/internal/protocol"
)

// The agent runs as root. A compose file or .env it edits must stay its
// owner's, or the owner can no longer read a chmod 600 .env.
func TestEditsKeepTheOwner(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	h := newHarness(t, literalCompose, "")
	path := filepath.Join(h.dir, "compose.yaml")
	if err := os.Chown(path, 1234, 2345); err != nil {
		t.Fatal(err)
	}
	os.Chmod(path, 0o600)
	evs := h.run(command(ids.V7{}.New(), webChange("1.28", dg("b"))))
	if got := last(evs); got.Type != protocol.EventCompleted {
		t.Fatalf("events = %s; last = %+v", types(evs), got)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	s := st.Sys().(*syscall.Stat_t)
	if s.Uid != 1234 || s.Gid != 2345 || st.Mode().Perm() != 0o600 {
		t.Fatalf("owner = %d:%d, mode = %o", s.Uid, s.Gid, st.Mode().Perm())
	}
}

// docker runs the compose plugin as a child. If only docker dies, the plugin
// still holds the output pipe, which must not keep Run waiting.
func TestRunnerDoesNotWaitForOrphansHoldingItsOutput(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	// sh ignores the interrupt and is killed at WaitDelay; its child lives on.
	_, err := ExecRunner{Binary: "sh"}.Run(ctx, "", "-c", "trap '' INT; sleep 120 & wait")
	if err == nil {
		t.Fatal("expected an error")
	}
	if d := time.Since(start); d > 30*time.Second {
		t.Fatalf("Run took %s", d)
	}
}
