package orchestrator

import (
	"context"
	"strings"
	"testing"

	"github.com/DragonDrew/ddum/internal/bot/store"
	"github.com/DragonDrew/ddum/internal/protocol"
)

// A pull that fails changed nothing: a registry outage must heal by itself,
// without a pointless rollback, and without waiting for a person.
func TestFailedPullIsRetriedByTheNextCheck(t *testing.T) {
	db, a, _ := setup(t)
	ctx := context.Background()
	db.RecordPoll(ctx, store.Poll{AgentID: agentID, HostName: "h", PollInterval: 15, Docker: protocol.DockerInfo{Arch: "amd64"},
		Capabilities: []string{protocol.CapActionDeploy, protocol.CapActionRollback, protocol.CapTagChangeEdit}, At: t0})
	op := a.Evaluate(ctx)[0].Operation
	h, _ := db.Hosts(ctx, false)
	db.DispatchCommands(ctx, h[0].ID, 120, t0)
	_, tr, _ := db.RecordEvents(ctx, agentID, op.ID, []protocol.Event{{Seq: 1, Type: protocol.EventAccepted},
		{Seq: 2, Type: protocol.EventFailed, Code: protocol.FailPull, Result: &protocol.Result{
			Snapshots: []protocol.SnapshotRef{{SnapshotID: "00000000-0000-7000-8000-0000000005a1", StackID: op.ScopeID}}}}}, t0)

	rec := &rbRecorder{}
	(&AutoRollback{Store: db, Notify: rec, Log: a.Log, Now: a.Now, ExpiryMinutes: 60}).OperationFinished(ctx, tr.Operation)
	if len(rec.queued) != 0 {
		t.Fatalf("nothing changed, so nothing is rolled back: %+v", rec.queued)
	}
	if out := a.Evaluate(ctx); len(out) != 0 {
		t.Fatalf("not before the next check: %d", len(out))
	}

	// The next check finds the same digest again.
	cs, _ := db.Containers(ctx, store.ContainerFilter{StackID: op.ScopeID})
	for _, c := range cs {
		if c.IdentityKey == "app" {
			d := "sha256:" + strings.Repeat("b", 64)
			db.RecordDetection(ctx, store.Detection{ContainerID: c.ID, StackID: c.StackID, HostID: c.HostID, Kind: "digest",
				CurrentRef: "nginx:1.27", TargetRef: "docker.io/library/nginx:1.27@" + d, TargetDigest: d}, t0)
		}
	}
	if out := a.Evaluate(ctx); len(out) != 1 {
		t.Fatalf("the update must deploy again: %d", len(out))
	}
}
