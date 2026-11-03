package orchestrator

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/DragonDrew/ddum/internal/bot/domain"
	"github.com/DragonDrew/ddum/internal/bot/store"
	"github.com/DragonDrew/ddum/internal/ids"
	"github.com/DragonDrew/ddum/internal/protocol"
)

const agentID = "0195f2c4-0000-7000-8000-00000000000a"

var t0 = time.Date(2026, 10, 1, 2, 0, 0, 0, time.UTC)

type recorder struct {
	queued  []store.ApproveResult
	expired int
}

func (r *recorder) AutoQueued(_ context.Context, res store.ApproveResult) {
	r.queued = append(r.queued, res)
}
func (r *recorder) OperationFinished(context.Context, store.Operation)         {}
func (r *recorder) OperationsExpired(_ context.Context, ops []store.Operation) { r.expired += len(ops) }

func setup(t *testing.T) (*store.Store, *AutoDeployer, *recorder) {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "ddum.db"), ids.V7{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	res, _ := db.RecordPoll(ctx, store.Poll{AgentID: agentID, HostName: "h", PollInterval: 15, Docker: protocol.DockerInfo{Arch: "amd64"},
		Capabilities: []string{protocol.CapActionDeploy, protocol.CapTagChangeEdit}, At: t0})
	db.ApproveHost(ctx, res.Host.ID, store.Actor{Kind: "user", DiscordID: "1"}, t0)
	st := protocol.Stack{Fingerprint: "compose:app", DisplayName: "app", Provider: "compose", Deploy: protocol.Support{Supported: true}}
	for _, n := range []string{"app", "db"} {
		c := protocol.Container{IdentityKey: n, ImageRef: "nginx:1.27", RepoDigests: []string{"nginx@sha256:" + strings.Repeat("a", 64)},
			TagChange: &protocol.TagChange{Supported: true, Source: "image"}, DdumLabels: map[string]string{}}
		st.Containers = append(st.Containers, c)
	}
	db.ApplyInventory(ctx, agentID, protocol.Inventory{InventoryHash: "x", Stacks: []protocol.Stack{st}}, t0)
	cs, _ := db.Containers(ctx, store.ContainerFilter{})
	for _, c := range cs {
		d := "sha256:" + strings.Repeat("b", 64)
		db.RecordDetection(ctx, store.Detection{ContainerID: c.ID, StackID: c.StackID, HostID: c.HostID, Kind: "digest",
			CurrentRef: "nginx:1.27", TargetRef: "docker.io/library/nginx:1.27@" + d, TargetDigest: d}, t0)
		mode := domain.ModeAutomatic
		if c.IdentityKey == "db" {
			mode = domain.ModeIgnore
		}
		db.SetPolicy(ctx, store.PolicyKey{Level: domain.LevelContainer, ID: c.ID}, domain.PolicyFields{Mode: &mode}, store.Actor{Kind: "user"}, t0)
	}
	rec := &recorder{}
	a := &AutoDeployer{Store: db, Notify: rec, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now: func() time.Time { return t0 }, ExpiryMinutes: 60}
	return db, a, rec
}

// TestAutomaticAppIgnoredDatabase checks that an automatic app
// next to an ignored database auto-updates the app, leaves the database, and
// is not Held.
func TestAutomaticAppIgnoredDatabase(t *testing.T) {
	db, a, rec := setup(t)
	out := a.Evaluate(context.Background())
	if len(out) != 1 || len(out[0].Approved) != 1 || out[0].Approved[0].ContainerName != "app" {
		t.Fatalf("auto result = %+v", out)
	}
	if out[0].Operation.InitiatorKind != "policy" || len(rec.queued) != 1 {
		t.Fatal("the policy engine must be the initiator, and the queueing notified")
	}
	ups, _ := db.Updates(context.Background(), store.UpdateFilter{}, t0)
	for _, u := range ups {
		if u.ContainerName == "db" && u.Status != store.StatusDetected {
			t.Fatalf("ignored database was touched: %s", u.Status)
		}
	}
	// A second evaluation queues nothing new.
	if again := a.Evaluate(context.Background()); len(again) != 0 {
		t.Fatalf("re-evaluation queued %d operations", len(again))
	}
}

func TestNoAutoDeployInMaintenance(t *testing.T) {
	db, a, _ := setup(t)
	db.SetMaintenance(context.Background(), true, store.Actor{Kind: "user"}, t0)
	if out := a.Evaluate(context.Background()); len(out) != 0 {
		t.Fatal("maintenance mode must pause automatic deployments")
	}
}

func TestSnoozedNotAutoDeployed(t *testing.T) {
	db, a, _ := setup(t)
	ups, _ := db.Updates(context.Background(), store.UpdateFilter{}, t0)
	for _, u := range ups {
		db.SnoozeUpdates(context.Background(), []string{u.ID}, time.Time{}, store.Actor{Kind: "user"}, t0)
	}
	if out := a.Evaluate(context.Background()); len(out) != 0 {
		t.Fatal("a snoozed update must not be auto-deployed")
	}
}

type rbRecorder struct{ queued []store.Operation }

func (r *rbRecorder) RollbackQueued(_ context.Context, op store.Operation, _ bool) {
	r.queued = append(r.queued, op)
}

func TestAutoRollbackOnFailure(t *testing.T) {
	db, a, _ := setup(t)
	ctx := context.Background()
	db.RecordPoll(ctx, store.Poll{AgentID: agentID, HostName: "h", PollInterval: 15, Docker: protocol.DockerInfo{Arch: "amd64"},
		Capabilities: []string{protocol.CapActionDeploy, protocol.CapActionRollback, protocol.CapTagChangeEdit}, At: t0})
	out := a.Evaluate(ctx) // app is automatic, so auto_rollback resolves true
	op := out[0].Operation
	h, _ := db.Hosts(ctx, false)
	db.DispatchCommands(ctx, h[0].ID, 120, t0)
	_, tr, _ := db.RecordEvents(ctx, agentID, op.ID, []protocol.Event{{Seq: 1, Type: protocol.EventAccepted},
		{Seq: 2, Type: protocol.EventFailed, Code: protocol.FailVerify, Result: &protocol.Result{
			Snapshots: []protocol.SnapshotRef{{SnapshotID: "00000000-0000-7000-8000-0000000005a1", StackID: op.ScopeID}}}}}, t0)

	rec := &rbRecorder{}
	ar := &AutoRollback{Store: db, Notify: rec, Log: a.Log, Now: a.Now, ExpiryMinutes: 60}
	ar.OperationFinished(ctx, tr.Operation)
	if len(rec.queued) != 1 || rec.queued[0].Kind != "rollback" || rec.queued[0].InitiatorKind != "policy" {
		t.Fatalf("auto-rollback = %+v", rec.queued)
	}
	// A success is never rolled back, and a self-update never automatically.
	ar.OperationFinished(ctx, store.Operation{Kind: "deploy", State: "succeeded"})
	ar.OperationFinished(ctx, store.Operation{Kind: "deploy", State: "failed", Payload: protocol.DeployPayload{SelfUpdate: true}})
	if len(rec.queued) != 1 {
		t.Fatal("unexpected rollback")
	}
}

// TestFailedAutoDeployDoesNotLoop: a rollback returns the update to detected,
// and the engine runs after every inventory, so a bad image must not be
// deployed and rolled back again and again.
func TestFailedAutoDeployDoesNotLoop(t *testing.T) {
	db, a, _ := setup(t)
	ctx := context.Background()
	db.RecordPoll(ctx, store.Poll{AgentID: agentID, HostName: "h", PollInterval: 15, Docker: protocol.DockerInfo{Arch: "amd64"},
		Capabilities: []string{protocol.CapActionDeploy, protocol.CapActionRollback, protocol.CapTagChangeEdit}, At: t0})
	out := a.Evaluate(ctx)
	if len(out) != 1 {
		t.Fatalf("first evaluation queued %d", len(out))
	}
	op := out[0].Operation
	h, _ := db.Hosts(ctx, false)
	db.DispatchCommands(ctx, h[0].ID, 120, t0)
	_, tr, _ := db.RecordEvents(ctx, agentID, op.ID, []protocol.Event{{Seq: 1, Type: protocol.EventAccepted},
		{Seq: 2, Type: protocol.EventFailed, Code: protocol.FailVerify, Result: &protocol.Result{
			Snapshots: []protocol.SnapshotRef{{SnapshotID: "00000000-0000-7000-8000-0000000005a1", StackID: op.ScopeID}}}}}, t0)
	rec := &rbRecorder{}
	(&AutoRollback{Store: db, Notify: rec, Log: a.Log, Now: a.Now, ExpiryMinutes: 60}).OperationFinished(ctx, tr.Operation)
	if len(rec.queued) != 1 {
		t.Fatalf("auto-rollback = %+v", rec.queued)
	}
	rb := rec.queued[0]
	db.DispatchCommands(ctx, h[0].ID, 120, t0)
	_, tr, _ = db.RecordEvents(ctx, agentID, rb.ID, []protocol.Event{{Seq: 1, Type: protocol.EventAccepted},
		{Seq: 2, Type: protocol.EventCompleted, Result: &protocol.Result{Containers: []protocol.ContainerOutcome{
			{IdentityKey: "app", Outcome: protocol.OutcomeUpdated}}}}}, t0)
	if tr.Operation.State != protocol.OpSucceeded {
		t.Fatalf("rollback = %s", tr.Operation.State)
	}
	if again := a.Evaluate(ctx); len(again) != 0 {
		t.Fatalf("the failed update was deployed again: %d operation(s)", len(again))
	}
}

// A newer release is a new update, and is deployed despite the earlier failure.
func TestNewerReleaseDeploysAfterAFailedAutoDeploy(t *testing.T) {
	db, a, _ := setup(t)
	ctx := context.Background()
	out := a.Evaluate(ctx)
	op := out[0].Operation
	h, _ := db.Hosts(ctx, false)
	db.DispatchCommands(ctx, h[0].ID, 120, t0)
	db.RecordEvents(ctx, agentID, op.ID, []protocol.Event{{Seq: 1, Type: protocol.EventAccepted},
		{Seq: 2, Type: protocol.EventFailed, Code: protocol.FailVerify}}, t0)
	cs, _ := db.Containers(ctx, store.ContainerFilter{StackID: op.ScopeID})
	for _, c := range cs {
		if c.IdentityKey != "app" {
			continue
		}
		// The same digest found again stays held back; a new digest does not.
		same := "sha256:" + strings.Repeat("b", 64)
		db.RecordDetection(ctx, store.Detection{ContainerID: c.ID, StackID: c.StackID, HostID: c.HostID, Kind: "digest",
			CurrentRef: "nginx:1.27", TargetRef: "docker.io/library/nginx:1.27@" + same, TargetDigest: same}, t0)
		if again := a.Evaluate(ctx); len(again) != 0 {
			t.Fatalf("the failed digest was deployed again: %d", len(again))
		}
		d := "sha256:" + strings.Repeat("c", 64)
		db.RecordDetection(ctx, store.Detection{ContainerID: c.ID, StackID: c.StackID, HostID: c.HostID, Kind: "digest",
			CurrentRef: "nginx:1.27", TargetRef: "docker.io/library/nginx:1.27@" + d, TargetDigest: d}, t0)
	}
	if out := a.Evaluate(ctx); len(out) != 1 {
		t.Fatalf("a newer release must deploy: %d", len(out))
	}
}
