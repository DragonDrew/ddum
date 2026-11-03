package executor

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/DragonDrew/ddum/internal/agent/docker"
	"github.com/DragonDrew/ddum/internal/ids"
	"github.com/DragonDrew/ddum/internal/protocol"
)

func rollbackCommand(id, original string, snapshots ...string) protocol.Command {
	p := protocol.RollbackPayload{OperationID: id, OriginalOperationID: original, SnapshotIDs: snapshots,
		ExpiresAt: t0.Add(time.Hour).Format(time.RFC3339), Verify: protocol.VerifySettings{TimeoutSeconds: 1}}
	b, _ := json.Marshal(p)
	return protocol.Command{OperationID: id, Type: protocol.CommandRollback, Payload: b}
}

// deployLiteral runs a 1.27 → 1.28 literal tag change and returns the
// operation ID and its snapshot.
func (h *harness) deployLiteral(t *testing.T) (string, string) {
	t.Helper()
	pendingTags["web"] = "docker.io/library/nginx:1.28"
	op := ids.V7{}.New()
	evs := h.run(command(op, webChange("1.28", dg("b"))))
	if last(evs).Type != protocol.EventCompleted {
		t.Fatalf("deploy: %s %+v", types(evs), last(evs))
	}
	return op, last(evs).Result.Snapshots[0].SnapshotID
}

// TestRollbackRestoresDigestAndFiles checks that rollback
// restores the previous digest and file content, and pruning the old tag
// does not break it.
func TestRollbackRestoresDigestAndFiles(t *testing.T) {
	h := newHarness(t, literalCompose, "")
	op, snap := h.deployLiteral(t)
	if b, _ := os.ReadFile(filepath.Join(h.dir, "compose.yaml")); !strings.Contains(string(b), "nginx:1.28") {
		t.Fatal("deploy did not edit the file")
	}
	if got := h.x.AvailableSnapshots(context.Background())["compose:app"]; len(got) != 1 || got[0] != snap {
		t.Fatalf("available snapshots = %v", got)
	}
	// "Prune": the old tag disappears, but the pin keeps the image. Docker also
	// drops the repo@digest record with the last tag, so RepoDigests is empty.
	h.engine.Untag(context.Background(), "nginx:1.27")
	h.engine.Update(func(l []docker.Container, imgs map[string]docker.Image) ([]docker.Container, map[string]docker.Image) {
		for k, img := range imgs {
			if img.ID == "img-old" {
				img.RepoDigests = nil
				imgs[k] = img
			}
		}
		return l, imgs
	})
	h.runner.pullErr = errors.New("registry unreachable") // the digest record cannot be restored either

	pendingTags["web"] = "nginx:1.27"
	evs := h.run(rollbackCommand(ids.V7{}.New(), op, snap))
	got := last(evs)
	if got.Type != protocol.EventCompleted || got.Result.Containers[0].Outcome != protocol.OutcomeUpdated ||
		got.Result.Containers[0].NewDigest != dg("a") {
		t.Fatalf("rollback: %s %+v", types(evs), got)
	}
	if b, _ := os.ReadFile(filepath.Join(h.dir, "compose.yaml")); string(b) != literalCompose {
		t.Fatalf("compose.yaml after rollback = %q", b)
	}
	if !strings.Contains(strings.Join(h.engine.Tags, "\n"), "ddum-rollback/") {
		t.Fatal("the pin tag must be retagged to the original reference")
	}
}

func TestRollbackRestoresEnvFile(t *testing.T) {
	compose := "services:\n  web:\n    image: nginx:${NGINX_TAG}\n  db:\n    image: postgres:16\n"
	env := "# keep me\nNGINX_TAG=1.27\n"
	h := newHarness(t, compose, env)
	op, snap := h.deployLiteral(t)
	if b, _ := os.ReadFile(filepath.Join(h.dir, ".env")); string(b) == env {
		t.Fatal("deploy did not edit .env")
	}
	pendingTags["web"] = "nginx:1.27"
	if evs := h.run(rollbackCommand(ids.V7{}.New(), op, snap)); last(evs).Type != protocol.EventCompleted {
		t.Fatalf("rollback: %s %+v", types(evs), last(evs))
	}
	if b, _ := os.ReadFile(filepath.Join(h.dir, ".env")); string(b) != env {
		t.Fatalf(".env after rollback = %q", b)
	}
}

// A rollback changes the tag and nothing else, whatever else changed since the
// deploy, and snapshots hold no copies of the files.
func TestRollbackKeepsLaterEdits(t *testing.T) {
	compose := "services:\n  web:\n    image: nginx:${NGINX_TAG}\n  db:\n    image: postgres:16\n"
	h := newHarness(t, compose, "NGINX_TAG=1.27\nAPI_KEY=original\n")
	op, snap := h.deployLiteral(t)
	if _, err := os.Stat(filepath.Join(h.x.snapshotDir(snap), "files")); !os.IsNotExist(err) {
		t.Fatalf("the snapshot keeps file copies: %v", err)
	}
	os.WriteFile(filepath.Join(h.dir, ".env"), []byte("NGINX_TAG=1.28\nAPI_KEY=rotated\nNEW=1\n"), 0o644)
	os.WriteFile(filepath.Join(h.dir, "compose.yaml"), []byte(compose+"# added later\n"), 0o644)
	pendingTags["web"] = "nginx:1.27"
	if evs := h.run(rollbackCommand(ids.V7{}.New(), op, snap)); last(evs).Type != protocol.EventCompleted {
		t.Fatalf("rollback: %s %+v", types(evs), last(evs))
	}
	if b, _ := os.ReadFile(filepath.Join(h.dir, ".env")); string(b) != "NGINX_TAG=1.27\nAPI_KEY=rotated\nNEW=1\n" {
		t.Fatalf(".env = %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(h.dir, "compose.yaml")); string(b) != compose+"# added later\n" {
		t.Fatalf("compose.yaml = %q", b)
	}
}

func TestRollbackRefusesAFileEditedToAnotherTag(t *testing.T) {
	h := newHarness(t, literalCompose, "")
	op, snap := h.deployLiteral(t)
	path := filepath.Join(h.dir, "compose.yaml")
	edited := strings.Replace(literalCompose, "nginx:1.27", "nginx:1.30", 1)
	os.WriteFile(path, []byte(edited), 0o644)
	evs := h.run(rollbackCommand(ids.V7{}.New(), op, snap))
	if evs[0].Type != protocol.EventRejected || evs[0].Code != protocol.FailIneligible || !strings.Contains(evs[0].Message, "1.30") {
		t.Fatalf("%s %+v", types(evs), evs[0])
	}
	if b, _ := os.ReadFile(path); string(b) != edited {
		t.Fatalf("the edit was overwritten: %q", b)
	}
}

func TestRollbackRefusesADeletedFile(t *testing.T) {
	h := newHarness(t, literalCompose, "")
	op, snap := h.deployLiteral(t)
	os.Remove(filepath.Join(h.dir, "compose.yaml"))
	evs := h.run(rollbackCommand(ids.V7{}.New(), op, snap))
	if evs[0].Type != protocol.EventRejected {
		t.Fatalf("%s %+v", types(evs), evs[0])
	}
}

func TestOldSnapshotFileCopiesAreRemoved(t *testing.T) {
	h := newHarness(t, literalCompose, "")
	_, snap := h.deployLiteral(t)
	files := filepath.Join(h.x.snapshotDir(snap), "files")
	os.MkdirAll(files, 0o750)
	os.WriteFile(filepath.Join(files, "1-.env"), []byte("API_KEY=old\n"), 0o640)
	h.x.Recover()
	if _, err := os.Stat(files); !os.IsNotExist(err) {
		t.Fatalf("old copies are still there: %v", err)
	}
	if got := h.x.AvailableSnapshots(context.Background())["compose:app"]; len(got) != 1 {
		t.Fatalf("the snapshot stopped being usable: %v", got)
	}
}

func TestRollbackRefusedWhenSnapshotUnavailable(t *testing.T) {
	h := newHarness(t, literalCompose, "")
	op, snap := h.deployLiteral(t)
	m, _ := h.x.loadSnapshot(snap)
	h.engine.Untag(context.Background(), m.Services[0].Pin) // the pinned image was deleted outside ddum
	if got := h.x.AvailableSnapshots(context.Background()); len(got) != 0 {
		t.Fatalf("a snapshot without its pinned image must be unavailable: %v", got)
	}
	evs := h.run(rollbackCommand(ids.V7{}.New(), op, snap))
	if evs[0].Type != protocol.EventRejected || evs[0].Code != protocol.FailIneligible {
		t.Fatalf("events = %s %+v", types(evs), evs)
	}
	// A snapshot of another operation is refused as unsafe.
	evs = h.run(rollbackCommand(ids.V7{}.New(), ids.V7{}.New(), snap))
	if evs[0].Type != protocol.EventRejected {
		t.Fatalf("foreign snapshot: %+v", evs)
	}
}

func TestSnapshotRetention(t *testing.T) {
	h := newHarness(t, literalCompose, "")
	h.x.RetentionCount = 3
	var snaps []string
	for i := 0; i < 4; i++ {
		pendingTags["web"] = "docker.io/library/nginx:1.27"
		evs := h.run(command(ids.V7{}.New(), webChange("1.27", dg(string(rune('b'+i))))))
		if last(evs).Type != protocol.EventCompleted {
			t.Fatalf("deploy %d: %+v", i, last(evs))
		}
		snaps = append(snaps, last(evs).Result.Snapshots[0].SnapshotID)
		h.now(time.Minute)
		// Each deploy starts from the image the previous one left running.
		h.stacks[0].Containers[0].ImageID = "img-" + strings.Repeat(string(rune('b'+i)), 8)
	}
	got := h.x.AvailableSnapshots(context.Background())["compose:app"]
	if len(got) != 3 || got[0] != snaps[3] {
		t.Fatalf("kept snapshots = %v, want the newest three", got)
	}
	if _, err := os.Stat(h.x.snapshotDir(snaps[0])); !os.IsNotExist(err) {
		t.Fatal("the oldest snapshot directory must be removed")
	}
	if !strings.Contains(strings.Join(h.engine.Tags, "\n"), "untag "+PinTag(snaps[0], "web")) {
		t.Fatal("the oldest pin tag must be removed")
	}
}

func (h *harness) now(d time.Duration) {
	cur := h.x.Now()
	h.x.Now = func() time.Time { return cur.Add(d) }
}

func selfUpdateCommand(id string, digest string) protocol.Command {
	p := protocol.DeployPayload{OperationID: id, Scope: protocol.Scope{Kind: "stack", ID: "s1"}, SelfUpdate: true,
		ExpiresAt: t0.Add(time.Hour).Format(time.RFC3339), Verify: protocol.VerifySettings{TimeoutSeconds: 1},
		Targets: []protocol.DeployTarget{{StackID: "s1", StackFingerprint: "compose:app", Services: []protocol.ServiceChange{webChange("1.28", digest)}}}}
	b, _ := json.Marshal(p)
	return protocol.Command{OperationID: id, Type: protocol.CommandDeploy, Payload: b}
}

// TestFailedSelfUpdateRollsBackLocally checks that a bot update
// whose new image fails verification is rolled back by the agent.
func TestFailedSelfUpdateRollsBackLocally(t *testing.T) {
	h := newHarness(t, literalCompose, "")
	h.stacks[0].Containers[0].Self = protocol.SelfBot // "web" is the bot here
	pendingTags["web"] = "docker.io/library/nginx:1.28"
	h.runner.badImage = "img-bbbbbbbb"
	evs := h.run(selfUpdateCommand(ids.V7{}.New(), dg("b")))
	got := last(evs)
	if got.Type != protocol.EventFailed || got.Code != protocol.FailVerify || got.Result == nil || !got.Result.RolledBack ||
		!strings.Contains(got.Message, "rolled the bot back") {
		t.Fatalf("self-update: %s %+v", types(evs), got)
	}
	if b, _ := os.ReadFile(filepath.Join(h.dir, "compose.yaml")); string(b) != literalCompose {
		t.Fatalf("compose.yaml after local rollback = %q", b)
	}
}

func TestBotOnlyBySelfUpdate(t *testing.T) {
	h := newHarness(t, literalCompose, "")
	h.stacks[0].Containers[0].Self = protocol.SelfBot
	evs := h.run(command(ids.V7{}.New(), webChange("1.28", dg("b"))))
	if evs[0].Type != protocol.EventRejected || evs[0].Code != protocol.FailUnsafe {
		t.Fatalf("plain deploy of the bot: %+v", evs)
	}
	h.stacks[0].Containers[0].Self = ""
	evs = h.run(selfUpdateCommand(ids.V7{}.New(), dg("b")))
	if evs[0].Type != protocol.EventRejected {
		t.Fatalf("self-update of a non-bot container: %+v", evs)
	}
}

func TestInterruptedReportsSnapshot(t *testing.T) {
	h := newHarness(t, literalCompose, "")
	id := ids.V7{}.New()
	h.x.Handle([]protocol.Command{command(id, webChange("1.27", dg("b")))})
	h.x.Journal.Update(id, func(e *Entry) { e.SnapshotIDs = []string{"snap-1"} })
	restarted := &Executor{Journal: h.x.Journal, Log: h.x.Log, Now: h.x.Now, Engine: &docker.Fake{}}
	restarted.Recover()
	got := last(restarted.Pending()[id])
	if got.Type != protocol.EventInterrupted || got.Result == nil || got.Result.Snapshots[0].SnapshotID != "snap-1" ||
		got.Result.Snapshots[0].StackID != "s1" {
		t.Fatalf("interrupted event = %+v", got)
	}
}

func TestRollbackRejectsPathLikeSnapshotIDs(t *testing.T) {
	h := newHarness(t, literalCompose, "")
	for _, bad := range []string{"../../etc", "..", "/etc/passwd", "a/b"} {
		evs := h.run(rollbackCommand(ids.V7{}.New(), ids.V7{}.New(), bad))
		if evs[0].Type != protocol.EventRejected {
			t.Fatalf("snapshot ID %q: %+v", bad, evs)
		}
	}
}
