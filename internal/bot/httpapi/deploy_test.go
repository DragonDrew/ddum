package httpapi_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/DragonDrew/ddum/internal/agent/discovery"
	"github.com/DragonDrew/ddum/internal/agent/docker"
	"github.com/DragonDrew/ddum/internal/agent/executor"
	"github.com/DragonDrew/ddum/internal/bot/domain"
	"github.com/DragonDrew/ddum/internal/bot/store"
	"github.com/DragonDrew/ddum/internal/ids"
	"github.com/DragonDrew/ddum/internal/protocol"
)

// cli emulates `docker pull` and `docker compose up` on a docker.Fake.
type cli struct {
	engine *docker.Fake
	gate   chan struct{} // when set, compose up waits for it
}

func (c *cli) Run(_ context.Context, _ string, args ...string) ([]byte, error) {
	switch args[0] {
	case "pull":
		ref := args[1]
		c.engine.Update(func(l []docker.Container, imgs map[string]docker.Image) ([]docker.Container, map[string]docker.Image) {
			imgs[ref] = docker.Image{ID: "img-new", RepoDigests: []string{ref}, OS: "linux", Arch: "amd64"}
			imgs["img-new"] = imgs[ref]
			return l, imgs
		})
	case "compose":
		if c.gate != nil {
			<-c.gate
		}
		c.engine.Update(func(l []docker.Container, imgs map[string]docker.Image) ([]docker.Container, map[string]docker.Image) {
			for i := range l {
				if l[i].Labels[discovery.LabelComposeService] == "web" {
					l[i].ID, l[i].ImageID = "web-recreated", "img-new"
				}
			}
			return l, imgs
		})
	}
	return []byte("done"), nil
}

func TestDeployEndToEndInProcess(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	stacks := t.TempDir()
	dir := filepath.Join(stacks, "app")
	os.MkdirAll(dir, 0o755)
	compose := filepath.Join(dir, "compose.yaml")
	os.WriteFile(compose, []byte("services:\n  web:\n    image: nginx:1.27\n"), 0o644)

	oldDigest, newDigest := "sha256:"+strings.Repeat("a", 64), "sha256:"+strings.Repeat("b", 64)
	fake := &docker.Fake{EngineInfo: protocol.DockerInfo{EngineVersion: "29.6.0", OS: "linux", Arch: "amd64"}}
	fake.Set([]docker.Container{{ID: "web-1", Name: "app-web-1", ImageRef: "nginx:1.27", ImageID: "img-old", State: "running", Health: "none",
		Labels: map[string]string{discovery.LabelComposeProject: "app", discovery.LabelComposeService: "web",
			discovery.LabelComposeContainerNumber: "1", discovery.LabelComposeConfigFiles: compose, discovery.LabelComposeWorkingDir: dir}}},
		map[string]docker.Image{"img-old": {ID: "img-old", RepoDigests: []string{"nginx@" + oldDigest}, OS: "linux", Arch: "amd64"}})

	roots := []string{filepath.ToSlash(mustResolve(t, stacks))}
	loop := r.agent(agentA, "host-3", fake)
	loop.Collector.(*discovery.Collector).Options = discovery.Options{EnabledProviders: []string{"compose"},
		ComposeAllowedRoots: roots, Files: discovery.OSFiles{}}
	loop.Capabilities = []string{protocol.CapProviderCompose, protocol.CapActionDeploy, protocol.CapTagChangeEdit}
	journal, _ := executor.OpenJournal(t.TempDir())
	runner := &cli{engine: fake, gate: make(chan struct{})}
	x := &executor.Executor{Journal: journal, Engine: fake, Runner: runner, Files: discovery.OSFiles{}, AllowedRoots: roots,
		DataDir: t.TempDir(), Inventory: loop.LatestStacks, Log: quiet, Now: r.clock.Now, VerifyPoll: time.Millisecond, NewID: ids.V7{}.New}
	loop.Ops = x
	xctx, stop := context.WithCancel(ctx)
	defer stop()
	go x.Run(xctx)

	loop.Step(ctx) // register and send inventory
	h, _ := r.db.HostByAgent(ctx, agentA)
	r.db.ApproveHost(ctx, h.ID, owner, r.clock.Now())
	cs, _ := r.db.Containers(ctx, store.ContainerFilter{})
	zero := 0
	r.db.SetPolicy(ctx, store.PolicyKey{Level: domain.LevelContainer, ID: cs[0].ID}, domain.PolicyFields{VerifyStable: &zero}, owner, r.clock.Now())
	det, err := r.db.RecordDetection(ctx, store.Detection{ContainerID: cs[0].ID, StackID: cs[0].StackID, HostID: h.ID, Kind: "digest",
		CurrentRef: "nginx:1.27", CurrentDigest: oldDigest, TargetRef: "docker.io/library/nginx:1.27@" + newDigest, TargetDigest: newDigest}, r.clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	offer, _ := r.db.CreateOffer(ctx, "stack", cs[0].StackID, []store.OfferItem{{UpdateID: det.Update.ID, TargetDigest: newDigest}}, "notification", r.clock.Now())
	approved, err := r.db.Approve(ctx, store.ApproveRequest{OfferID: offer, By: owner, At: r.clock.Now(), ExpiryMinutes: 60})
	if err != nil {
		var nothing *store.NothingError
		if errors.As(err, &nothing) {
			t.Fatalf("not approvable: %+v", nothing.Skipped)
		}
		t.Fatal(err)
	}
	opID := approved.Operation.ID

	// The poll dispatches the command; the agent accepts it in the same step.
	r.clock.Add(15 * time.Second)
	loop.Step(ctx)
	op, _ := r.db.GetOperation(ctx, opID)
	if op.State != protocol.OpAccepted && op.State != protocol.OpRunning {
		t.Fatalf("after dispatch: %s", op.State)
	}

	// The bot restarts while the operation runs: nothing is lost.
	r.restart()
	close(runner.gate)
	deadline := time.Now().Add(5 * time.Second)
	for {
		time.Sleep(20 * time.Millisecond)
		r.clock.Add(time.Second)
		loop.Step(ctx)
		op, _ = r.db.GetOperation(ctx, opID)
		if op.Terminal() || time.Now().After(deadline) {
			break
		}
	}
	if op.State != protocol.OpSucceeded {
		evs, _ := r.db.OperationEvents(ctx, opID)
		t.Fatalf("final state %s (%+v); events %+v", op.State, op.Outcome, evs)
	}
	u, _ := r.db.GetUpdate(ctx, det.Update.ID, r.clock.Now())
	if u.Status != store.StatusApplied {
		t.Fatalf("update = %s", u.Status)
	}
	// The fresh inventory after the deploy shows the recreated container.
	loop.Step(ctx)
	c, _ := r.db.Container(ctx, cs[0].ID)
	if c.DockerID != "web-recreated" || !strings.Contains(strings.Join(c.RunningDigests, ","), newDigest) {
		t.Fatalf("container after deploy: %+v", c)
	}
	// Every event reached the bot exactly once.
	evs, _ := r.db.OperationEvents(ctx, opID)
	for i, e := range evs {
		if e.Seq != i+1 {
			t.Fatalf("events out of order or duplicated: %+v", evs)
		}
	}
	if len(x.Pending()) != 0 {
		t.Fatal("acknowledged events are still pending on the agent")
	}
	if len(r.notes.finished) != 1 {
		t.Fatalf("finished notices = %d, want 1", len(r.notes.finished))
	}
}

func mustResolve(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
