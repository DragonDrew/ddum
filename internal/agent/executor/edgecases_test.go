package executor

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/DragonDrew/ddum/internal/agent/discovery"
	"github.com/DragonDrew/ddum/internal/agent/docker"
	"github.com/DragonDrew/ddum/internal/ids"
	"github.com/DragonDrew/ddum/internal/protocol"
)

// A container changed by hand after the approval is not moved: this is what
// took a manually updated 0.11.1 back to an approved 0.10.
func TestChangedSinceApprovalIsRefused(t *testing.T) {
	h := newHarness(t, literalCompose, "")
	h.stacks[0].Containers[0].RepoDigests = []string{"nginx@" + dg("e")} // neither a nor b
	evs := h.run(command(ids.V7{}.New(), webChange("1.28", dg("b"))))
	if len(evs) != 1 || evs[0].Type != protocol.EventRejected || evs[0].Code != protocol.FailIneligible ||
		!strings.Contains(evs[0].Message, "changed after") {
		t.Fatalf("events = %+v", evs)
	}
	if len(h.runner.calls) != 0 {
		t.Fatal("a refused operation must not run anything")
	}
}

// Docker can lose the digest of a running image when its tag moves. That is no
// sign of a change, and the update must still go through.
func TestLostDigestRecordIsNotAChange(t *testing.T) {
	h := newHarness(t, literalCompose, "")
	h.stacks[0].Containers[0].RepoDigests = []string{}
	evs := h.run(command(ids.V7{}.New(), webChange("1.28", dg("b"))))
	if got := last(evs); got.Type != protocol.EventCompleted {
		t.Fatalf("events = %s; last = %+v", types(evs), got)
	}
}

// addStack adds a second Compose project whose service has the same name.
func (h *harness) addStack(t *testing.T) {
	t.Helper()
	dir := filepath.Join(filepath.Dir(h.dir), "other")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte("services:\n  web:\n    image: redis:7\n"), 0o644)
	h.engine.Update(func(l []docker.Container, imgs map[string]docker.Image) ([]docker.Container, map[string]docker.Image) {
		imgs["img-redis"] = docker.Image{ID: "img-redis", RepoDigests: []string{"redis@" + dg("c")}}
		return append(l, docker.Container{ID: "other-web", Name: "other-web-1", ImageRef: "redis:7", ImageID: "img-redis", State: "running", Health: "none",
			Labels: map[string]string{discovery.LabelComposeProject: "other", discovery.LabelComposeService: "web", discovery.LabelComposeContainerNumber: "1"}}), imgs
	})
	tc := protocol.TagChange{Supported: true, Source: "image"}
	h.stacks = append(h.stacks, protocol.Stack{Fingerprint: "compose:other", DisplayName: "other", Provider: "compose", Deploy: protocol.Support{Supported: true},
		Compose:    &protocol.ComposeInfo{Project: "other", ConfigFiles: []string{filepath.Join(dir, "compose.yaml")}, WorkingDir: dir, FilesAccessible: true},
		Containers: []protocol.Container{{IdentityKey: "web", DockerID: "other-web", ImageID: "img-redis", ImageRef: "redis:7", RepoDigests: []string{"redis@" + dg("c")}, TagChange: &tc}}})
}

// A group spans stacks, and identity keys are unique per stack only.
func TestOutcomesOfSameNamedServicesStayApart(t *testing.T) {
	h := newHarness(t, literalCompose, "")
	h.addStack(t)
	h.runner.pullErrFor = "redis"
	id := ids.V7{}.New()
	p := protocol.DeployPayload{OperationID: id, Scope: protocol.Scope{Kind: "group", ID: "g1"},
		ExpiresAt: t0.Add(time.Hour).Format(time.RFC3339), Verify: protocol.VerifySettings{TimeoutSeconds: 1},
		Targets: []protocol.DeployTarget{
			{StackID: "s1", StackFingerprint: "compose:app", Services: []protocol.ServiceChange{webChange("1.27", dg("b"))}},
			{StackID: "s2", StackFingerprint: "compose:other", Services: []protocol.ServiceChange{{ContainerID: "c2", IdentityKey: "web",
				Current: protocol.ImageState{ImageRef: "redis:7", Digest: dg("c")},
				Desired: protocol.ImageState{ImageRef: "docker.io/library/redis:7", Digest: dg("e")}}}},
		}}
	b, _ := json.Marshal(p)
	evs := h.run(protocol.Command{OperationID: id, Type: protocol.CommandDeploy, Payload: b})
	got := last(evs)
	if got.Type != protocol.EventFailed || got.Code != protocol.FailPull {
		t.Fatalf("events = %s; last = %+v", types(evs), got)
	}
	outcome := map[string]protocol.ContainerOutcome{}
	for _, c := range got.Result.Containers {
		outcome[c.StackID] = c
	}
	if outcome["s1"].Outcome != protocol.OutcomeUpdated || outcome["s1"].NewDigest != dg("b") {
		t.Errorf("the stack that deployed: %+v", outcome["s1"])
	}
	if outcome["s2"].Outcome != protocol.OutcomeFailed {
		t.Errorf("the stack that failed: %+v", outcome["s2"])
	}
}

func TestEditWritesThroughASymlink(t *testing.T) {
	h := newHarness(t, literalCompose, "")
	link := filepath.Join(h.dir, "compose.yaml")
	real := filepath.Join(h.dir, "real.yaml")
	os.Rename(link, real)
	if err := os.Symlink("real.yaml", link); err != nil {
		os.Rename(real, link)
		t.Skip("symlinks are not available here:", err)
	}
	evs := h.run(command(ids.V7{}.New(), webChange("1.28", dg("b"))))
	if got := last(evs); got.Type != protocol.EventCompleted {
		t.Fatalf("events = %s; last = %+v", types(evs), got)
	}
	st, err := os.Lstat(link)
	if err != nil || st.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the link was replaced by a copy: %v, %v", st, err)
	}
	if b, _ := os.ReadFile(real); !strings.Contains(string(b), "nginx:1.28") {
		t.Fatalf("real file = %q", b)
	}
}

// Docker's containerd store drops an image once its last tag moves, even while
// a container runs it. The update must still work, and can still be undone.
func TestDeployWhenTheRunningImageIsGone(t *testing.T) {
	gone := func(h *harness) {
		h.engine.Update(func(l []docker.Container, imgs map[string]docker.Image) ([]docker.Container, map[string]docker.Image) {
			delete(imgs, "img-old")
			return l, imgs
		})
	}
	t.Run("pulled again for the rollback", func(t *testing.T) {
		h := newHarness(t, literalCompose, "")
		gone(h)
		evs := h.run(command(ids.V7{}.New(), webChange("1.28", dg("b"))))
		got := last(evs)
		if got.Type != protocol.EventCompleted || len(got.Result.Snapshots) != 1 {
			t.Fatalf("events = %s; last = %+v", types(evs), got)
		}
		if avail := h.x.AvailableSnapshots(context.Background())["compose:app"]; len(avail) != 1 {
			t.Fatalf("the snapshot must stay usable: %v", avail)
		}
	})
	t.Run("not on the registry any more", func(t *testing.T) {
		h := newHarness(t, literalCompose, "")
		gone(h)
		h.runner.pullErrFor = dg("a")
		evs := h.run(command(ids.V7{}.New(), webChange("1.28", dg("b"))))
		if got := last(evs); got.Type != protocol.EventCompleted {
			t.Fatalf("the update must not depend on it: %s; last = %+v", types(evs), got)
		}
		if avail := h.x.AvailableSnapshots(context.Background())["compose:app"]; len(avail) != 0 {
			t.Fatalf("this snapshot cannot be restored: %v", avail)
		}
	})
}

// A hung docker command must not block the host for good: an accepted
// operation cannot be cancelled.
func TestHungDockerCommandsGiveUp(t *testing.T) {
	for _, c := range []struct{ hang, code string }{{"pull", protocol.FailPull}, {"compose", protocol.FailDeploy}} {
		t.Run(c.hang, func(t *testing.T) {
			h := newHarness(t, literalCompose, "")
			h.x.PullTimeout, h.x.ComposeTimeout = 20*time.Millisecond, 20*time.Millisecond
			h.runner.hang = c.hang
			evs := h.run(command(ids.V7{}.New(), webChange("1.27", dg("b"))))
			got := last(evs)
			if got.Type != protocol.EventFailed || got.Code != c.code || !strings.Contains(got.Message, "gave up") {
				t.Fatalf("events = %s; last = %+v", types(evs), got)
			}
		})
	}
}

// A Compose file edited but not applied is someone's pending change.
func TestPendingFileEditIsNotOverwritten(t *testing.T) {
	edited := strings.Replace(literalCompose, "nginx:1.27", "nginx:1.29", 1)
	h := newHarness(t, edited, "")
	evs := h.run(command(ids.V7{}.New(), webChange("1.28", dg("b"))))
	if len(evs) != 1 || evs[0].Type != protocol.EventRejected || evs[0].Code != protocol.FailIneligible {
		t.Fatalf("events = %+v", evs)
	}
	if b, _ := os.ReadFile(filepath.Join(h.dir, "compose.yaml")); string(b) != edited {
		t.Fatalf("file = %q", b)
	}
}

// A failed deploy leaves the file on the new tag, and retrying must work.
func TestRetryAfterAFailedDeployIsNotAPendingEdit(t *testing.T) {
	h := newHarness(t, strings.Replace(literalCompose, "nginx:1.27", "nginx:1.28", 1), "")
	evs := h.run(command(ids.V7{}.New(), webChange("1.28", dg("b"))))
	if got := last(evs); got.Type != protocol.EventCompleted {
		t.Fatalf("events = %s; last = %+v", types(evs), got)
	}
}

// The agent restarts in the middle of a bot update. If the new bot does not
// start, nobody else can bring it back.
func TestInterruptedSelfUpdateIsRestored(t *testing.T) {
	h := newHarness(t, literalCompose, "")
	h.stacks[0].Containers[0].Self = protocol.SelfBot
	id := ids.V7{}.New()
	cmd := selfUpdateCommand(id, dg("b"))
	h.x.init()
	h.x.Handle([]protocol.Command{cmd})
	var p protocol.DeployPayload
	json.Unmarshal(cmd.Payload, &p)
	plans, err := h.x.plan(p)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := h.x.snapshot(context.Background(), id, "s1", plans[0])
	if err != nil {
		t.Fatal(err)
	}
	h.x.Journal.Update(id, func(e *Entry) { e.SnapshotIDs = []string{snap} })
	// The file already names the new version when the agent dies.
	path := filepath.Join(h.dir, "compose.yaml")
	os.WriteFile(path, []byte(strings.Replace(literalCompose, "nginx:1.27", "nginx:1.28", 1)), 0o644)

	restarted := &Executor{Journal: h.x.Journal, Engine: h.x.Engine, Runner: h.x.Runner, Files: h.x.Files, AllowedRoots: h.x.AllowedRoots,
		DataDir: h.x.DataDir, Inventory: h.x.Inventory, Log: h.x.Log, Now: h.x.Now, VerifyPoll: time.Millisecond, NewID: h.x.NewID,
		PullRetryDelays: h.x.PullRetryDelays}
	restarted.Recover()
	for _, ev := range restarted.Pending()[id] {
		if ev.Type == protocol.EventInterrupted {
			t.Fatal("reported before anything was restored")
		}
	}
	restarted.restoreSelfUpdates(context.Background())
	got := last(restarted.Pending()[id])
	if got.Type != protocol.EventInterrupted || got.Result == nil || !got.Result.RolledBack {
		t.Fatalf("event = %+v", got)
	}
	if b, _ := os.ReadFile(path); string(b) != literalCompose {
		t.Fatalf("compose.yaml = %q", b)
	}
}
