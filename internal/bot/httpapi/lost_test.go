package httpapi_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/DragonDrew/ddum/internal/bot/httpapi"
	"github.com/DragonDrew/ddum/internal/bot/store"
	"github.com/DragonDrew/ddum/internal/protocol"
)

// reportingOps is an executor that reports one operation as running, or none.
type reportingOps struct{ active string }

func (o *reportingOps) Active() []protocol.ActiveOperation {
	if o.active == "" {
		return []protocol.ActiveOperation{}
	}
	return []protocol.ActiveOperation{{OperationID: o.active, State: protocol.OpRunning}}
}
func (o *reportingOps) Handle([]protocol.Command)            {}
func (o *reportingOps) Pending() map[string][]protocol.Event { return nil }
func (o *reportingOps) Acked(string, int)                    {}
func (o *reportingOps) TakeInventoryRequest() bool           { return false }

// TestLostOperationIsNotWaitedForForever: an agent that lost its data volume
// never reports the end of an operation, and an accepted operation cannot be
// cancelled, so nothing else would unblock its host. The wait counts from the
// first poll that leaves it out, so an operation that finished while the bot
// was down still has time to report.
func TestLostOperationIsNotWaitedForForever(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	ops := &reportingOps{}
	loop := r.agent(agentA, "host", fakeDocker(composeContainer("c1", "app", "web")))
	loop.Capabilities = []string{protocol.CapProviderCompose, protocol.CapActionDeploy, protocol.CapTagChangeEdit}
	loop.Ops = ops
	loop.Step(ctx)
	host, err := r.db.HostByAgent(ctx, agentA)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.db.ApproveHost(ctx, host.ID, owner, r.clock.Now()); err != nil {
		t.Fatal(err)
	}
	loop.Step(ctx)
	cs, _ := r.db.Containers(ctx, store.ContainerFilter{})
	if len(cs) != 1 {
		t.Fatalf("containers = %+v", cs)
	}
	d := "sha256:" + strings.Repeat("b", 64)
	det, err := r.db.RecordDetection(ctx, store.Detection{ContainerID: cs[0].ID, StackID: cs[0].StackID, HostID: cs[0].HostID, Kind: "digest",
		CurrentRef: "nginx:1.27", TargetRef: "docker.io/library/nginx:1.27@" + d, TargetDigest: d}, r.clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	// Deploy support comes from the agent's files, which this one lacks.
	if err := r.db.WriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE stacks SET deploy_supported = 1`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	offer, _ := r.db.CreateOffer(ctx, "stack", cs[0].StackID, []store.OfferItem{{UpdateID: det.Update.ID, TargetDigest: d}}, "notification", r.clock.Now())
	res, err := r.db.Approve(ctx, store.ApproveRequest{OfferID: offer, By: owner, At: r.clock.Now(), ExpiryMinutes: 60})
	if err != nil {
		t.Fatal(err)
	}
	opID := res.Operation.ID
	r.db.DispatchCommands(ctx, host.ID, 120, r.clock.Now())
	r.db.RecordEvents(ctx, agentA, opID, []protocol.Event{{Seq: 1, Type: protocol.EventAccepted}}, r.clock.Now())
	state := func() string {
		op, _ := r.db.GetOperation(ctx, opID)
		return op.State
	}

	// Reported as running, it may take as long as it takes.
	ops.active = opID
	loop.Step(ctx)
	r.clock.Add(httpapi.LostOperationAfter * 3)
	loop.Step(ctx)
	if state() != protocol.OpAccepted {
		t.Fatalf("a reported operation was ended: %s", state())
	}

	// Long since its last event, but the first poll that leaves it out only
	// starts the wait: its final event may be about to arrive.
	ops.active = ""
	loop.Step(ctx)
	if state() != protocol.OpAccepted {
		t.Fatalf("ended on the first poll without it: %s", state())
	}
	r.clock.Add(httpapi.LostOperationAfter - time.Minute)
	loop.Step(ctx)
	if state() != protocol.OpAccepted {
		t.Fatalf("ended too soon: %s", state())
	}
	r.clock.Add(2 * time.Minute)
	loop.Step(ctx)
	if state() != protocol.OpInterrupted {
		t.Fatalf("state = %s, want interrupted", state())
	}
	if len(r.notes.finished) != 1 || r.notes.finished[0].ID != opID {
		t.Fatalf("the owner must be told: %+v", r.notes.finished)
	}
	if ids, _ := r.db.RunningOperationIDs(ctx, host.ID); len(ids) != 0 {
		t.Fatalf("the host is still blocked: %v", ids)
	}
}
