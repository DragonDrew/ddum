package scheduler

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/DragonDrew/ddum/internal/bot/store"
	"github.com/DragonDrew/ddum/internal/ids"
	"github.com/DragonDrew/ddum/internal/protocol"
)

type recorder struct{ offline []store.Host }

func (r *recorder) HostOffline(_ context.Context, h store.Host) { r.offline = append(r.offline, h) }

// TestKilledAgentPostsOneNotice checks that killing an agent
// marks the host offline and posts one notice.
func TestKilledAgentPostsOneNotice(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "ddum.db"), ids.V7{})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	t0 := time.Date(2026, 10, 1, 2, 0, 0, 0, time.UTC)
	res, err := db.RecordPoll(ctx, store.Poll{AgentID: "0195f2c4-0000-7000-8000-00000000000a", HostName: "h",
		PollInterval: 30, Docker: protocol.DockerInfo{Arch: "amd64"}, At: t0})
	if err != nil {
		t.Fatal(err)
	}

	now := t0
	rec := &recorder{}
	sw := &OfflineSweeper{Store: db, Notify: rec, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Now: func() time.Time { return now }}
	// The agent stops polling. Sweeps run every 15 s for ten minutes.
	for i := 0; i < 40; i++ {
		now = now.Add(15 * time.Second)
		sw.Sweep(ctx)
	}
	if len(rec.offline) != 1 || rec.offline[0].ID != res.Host.ID {
		t.Fatalf("offline notices = %+v, want exactly one for %s", rec.offline, res.Host.ID)
	}
	h, _ := db.Host(ctx, res.Host.ID)
	if h.Connectivity != "offline" {
		t.Fatalf("connectivity = %s", h.Connectivity)
	}
}
