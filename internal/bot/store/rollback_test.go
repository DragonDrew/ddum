package store

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/DragonDrew/ddum/internal/protocol"
)

var rollbackCaps = []string{protocol.CapProviderCompose, protocol.CapActionDeploy, protocol.CapActionRollback, protocol.CapTagChangeEdit}

// deployed approves "web", runs it to the given terminal event with snapshot
// "snap-1", and returns the operation.
func (e *opEnv) deployed(t *testing.T, final protocol.Event) Operation {
	t.Helper()
	e.s.RecordPoll(bgCtx, Poll{AgentID: agentA, HostName: "h", PollInterval: 15, Capabilities: rollbackCaps, Docker: amd64, At: t0})
	res, err := e.approve(t, e.offer(t, "web"))
	if err != nil {
		t.Fatal(err)
	}
	e.s.DispatchCommands(bgCtx, e.host.ID, 120, t0)
	final.Seq = 2
	if final.Result == nil {
		final.Result = &protocol.Result{}
	}
	final.Result.Snapshots = []protocol.SnapshotRef{{SnapshotID: "00000000-0000-7000-8000-0000000005a1", StackID: e.stackID, Created: true}}
	if _, _, err := e.s.RecordEvents(bgCtx, agentA, res.Operation.ID, []protocol.Event{{Seq: 1, Type: protocol.EventAccepted}, final}, t0); err != nil {
		t.Fatal(err)
	}
	op, _ := e.s.GetOperation(bgCtx, res.Operation.ID)
	return op
}

var completedWeb = protocol.Event{Type: protocol.EventCompleted, Result: &protocol.Result{Containers: []protocol.ContainerOutcome{
	{IdentityKey: "web", Outcome: protocol.OutcomeUpdated, NewDigest: digest("b")}}}}

func TestRollbackLifecycle(t *testing.T) {
	e := newOpEnv(t)
	orig := e.deployed(t, completedWeb)
	snaps, _ := e.s.Snapshots(bgCtx, orig.ID)
	if len(snaps) != 1 || snaps[0].Status != SnapshotAvailable {
		t.Fatalf("snapshots = %+v", snaps)
	}
	cands, _ := e.s.RollbackCandidates(bgCtx)
	if len(cands) != 1 || cands[0].ID != orig.ID {
		t.Fatalf("candidates = %+v", cands)
	}

	rb, err := e.s.CreateRollback(bgCtx, orig.ID, owner, t0, 60)
	if err != nil {
		t.Fatal(err)
	}
	var p protocol.RollbackPayload
	if err := jsonUnmarshal(rb.PayloadJSON, &p); err != nil || p.OriginalOperationID != orig.ID || p.SnapshotIDs[0] != snaps[0].ID {
		t.Fatalf("payload = %+v %v", p, err)
	}
	svc := p.Targets[0].Services[0]
	if svc.Desired.Digest != digest("a") || svc.Current.Digest != digest("b") {
		t.Fatalf("rollback must restore the previous digest: %+v", svc)
	}
	// It is not rollbackable twice.
	if _, err := e.s.CreateRollback(bgCtx, orig.ID, owner, t0, 60); !errors.Is(err, ErrNotLatest) {
		t.Fatalf("second rollback err = %v", err)
	}

	e.s.DispatchCommands(bgCtx, e.host.ID, 120, t0)
	_, tr, _ := e.s.RecordEvents(bgCtx, agentA, rb.ID, []protocol.Event{{Seq: 1, Type: protocol.EventAccepted},
		{Seq: 2, Type: protocol.EventCompleted, Result: &protocol.Result{Containers: []protocol.ContainerOutcome{
			{IdentityKey: "web", Outcome: protocol.OutcomeUpdated, NewDigest: digest("a")}}}}}, t0)
	if tr.Operation.State != protocol.OpSucceeded {
		t.Fatalf("rollback state = %s", tr.Operation.State)
	}
	if status(t, e.s, e.ups["web"].ID) != StatusDetected {
		t.Fatal("after a rollback the update is pending again")
	}
	if snaps, _ := e.s.Snapshots(bgCtx, orig.ID); snaps[0].Status != SnapshotConsumed {
		t.Fatalf("snapshot = %s, want consumed", snaps[0].Status)
	}
	var reached string
	e.s.Reader().QueryRow(`SELECT image_digest FROM desired_states WHERE container_id = ? AND status = 'reached'
		ORDER BY set_at DESC, id DESC LIMIT 1`, e.ups["web"].ContainerID).Scan(&reached)
	if reached != digest("a") {
		t.Fatalf("desired state after rollback = %s, want the restored digest", reached)
	}
}

func TestRollbackRefusedWhenSnapshotUnavailable(t *testing.T) {
	e := newOpEnv(t)
	orig := e.deployed(t, completedWeb)
	// The next inventory no longer lists the snapshot: its pinned image is gone.
	st := protocol.Stack{Fingerprint: "compose:app", DisplayName: "app", Provider: compose, Deploy: protocol.Support{Supported: true}}
	st.Containers = []protocol.Container{{IdentityKey: "web", ImageRef: "nginx:1.28", RepoDigests: []string{"nginx@" + digest("b")}}}
	if _, err := e.s.ApplyInventory(bgCtx, agentA, protocol.Inventory{InventoryHash: "y", Stacks: []protocol.Stack{st}}, t0); err != nil {
		t.Fatal(err)
	}
	if snaps, _ := e.s.Snapshots(bgCtx, orig.ID); snaps[0].Status != SnapshotUnavailable {
		t.Fatalf("snapshot = %s, want unavailable", snaps[0].Status)
	}
	if _, err := e.s.CreateRollback(bgCtx, orig.ID, owner, t0, 60); !errors.Is(err, ErrSnapshotUnavailable) {
		t.Fatalf("err = %v, want ErrSnapshotUnavailable", err)
	}
	// It becomes available again when the agent reports it.
	st.Rollback = protocol.RollbackInfo{Available: true, SnapshotIDs: []string{"00000000-0000-7000-8000-0000000005a1"}}
	e.s.ApplyInventory(bgCtx, agentA, protocol.Inventory{InventoryHash: "z", Stacks: []protocol.Stack{st}}, t0)
	if err := e.s.CanRollBack(bgCtx, orig.ID); err != nil {
		t.Fatalf("after the agent reports it again: %v", err)
	}
}

func TestFailedRollbackHoldsStack(t *testing.T) {
	e := newOpEnv(t)
	orig := e.deployed(t, completedWeb)
	rb, _ := e.s.CreateRollback(bgCtx, orig.ID, owner, t0, 60)
	e.s.DispatchCommands(bgCtx, e.host.ID, 120, t0)
	e.s.RecordEvents(bgCtx, agentA, rb.ID, []protocol.Event{{Seq: 1, Type: protocol.EventAccepted},
		{Seq: 2, Type: protocol.EventFailed, Code: protocol.FailDeploy, Message: "boom"}}, t0)
	if _, held, _ := e.s.RollbackHold(bgCtx, e.stackID); !held {
		t.Fatal("a failed rollback must hold the stack")
	}
	// A new update in the held stack cannot be approved.
	cs, _ := e.s.Containers(bgCtx, ContainerFilter{})
	for _, c := range cs {
		if c.IdentityKey == "web" {
			d, _ := e.s.RecordDetection(bgCtx, Detection{ContainerID: c.ID, StackID: c.StackID, HostID: c.HostID, Kind: "digest",
				CurrentRef: "nginx:1.28", TargetRef: "docker.io/library/nginx:1.28@" + digest("c"), TargetDigest: digest("c")}, t0)
			e.ups["web"] = d.Update
		}
	}
	var nothing *NothingError
	if _, err := e.approve(t, e.offer(t, "web")); !errors.As(err, &nothing) {
		t.Fatalf("approval in a held stack: %v", err)
	}
	if ok, _ := e.s.ClearRollbackHold(bgCtx, e.stackID, owner, t0); !ok {
		t.Fatal("clear hold")
	}
	if _, err := e.approve(t, e.offer(t, "web")); err != nil {
		t.Fatalf("after clearing the hold: %v", err)
	}
	if auditCount(t, e.s, "rollback.hold_cleared", e.stackID) != 1 {
		t.Fatal("hold clearing must be audited")
	}
}

func TestSelfUpdateOffers(t *testing.T) {
	e := newOpEnv(t)
	e.s.RecordPoll(bgCtx, Poll{AgentID: agentA, HostName: "h", PollInterval: 15, Capabilities: rollbackCaps, Docker: amd64, At: t0})
	mustExec(t, e.s, `UPDATE containers SET self_role = 'bot' WHERE id = ?`, e.ups["web"].ContainerID)

	// In a stack offer the bot is left out.
	var nothing *NothingError
	if _, err := e.approve(t, e.offer(t, "web")); !errors.As(err, &nothing) || nothing.Skipped[0].Reason != "the ddum bot is updated from its own message" {
		t.Fatalf("stack offer with the bot: %v", err)
	}
	u, _ := e.s.GetUpdate(bgCtx, e.ups["web"].ID, t0)
	offer, err := e.s.CreateSelfUpdateOffer(bgCtx, u, t0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.Approve(bgCtx, ApproveRequest{OfferID: offer, By: Actor{Kind: "policy"}, At: t0, ExpiryMinutes: 60}); err == nil {
		t.Fatal("the policy engine must never update the bot")
	}
	offer, _ = e.s.CreateSelfUpdateOffer(bgCtx, u, t0)
	res, err := e.approve(t, offer)
	if err != nil || !res.Operation.Payload.SelfUpdate || len(res.Operation.Payload.Targets[0].Services) != 1 {
		t.Fatalf("self-update: %+v %v", res.Operation.Payload, err)
	}
}

func TestInterruptedRecordsSnapshot(t *testing.T) {
	e := newOpEnv(t)
	orig := e.deployed(t, protocol.Event{Type: protocol.EventInterrupted})
	if orig.State != protocol.OpInterrupted {
		t.Fatalf("state = %s", orig.State)
	}
	if err := e.s.CanRollBack(bgCtx, orig.ID); err != nil {
		t.Fatalf("an interrupted deploy with a snapshot is rollbackable: %v", err)
	}
}

func jsonUnmarshal(s string, v any) error { return json.Unmarshal([]byte(s), v) }
