package store

import (
	"testing"

	"github.com/DragonDrew/ddum/internal/protocol"
)

// TestLostOperationEnds: an agent that lost its data volume never reports the
// end of an operation, and an accepted operation cannot be cancelled. When to
// give up is decided in httpapi (TestLostOperationIsNotWaitedForForever).
func TestLostOperationEnds(t *testing.T) {
	e := newOpEnv(t)
	res, err := e.approve(t, e.offer(t, "web"))
	if err != nil {
		t.Fatal(err)
	}
	e.s.DispatchCommands(bgCtx, e.host.ID, 120, t0)
	e.s.RecordEvents(bgCtx, agentA, res.Operation.ID, []protocol.Event{{Seq: 1, Type: protocol.EventAccepted}}, t0)
	if ids, _ := e.s.RunningOperationIDs(bgCtx, e.host.ID); len(ids) != 1 || ids[0] != res.Operation.ID {
		t.Fatalf("running = %v", ids)
	}
	op, ended, err := e.s.InterruptLostOperation(bgCtx, res.Operation.ID, t0)
	if err != nil || !ended || op.State != protocol.OpInterrupted {
		t.Fatalf("op = %s, ended = %v, err = %v", op.State, ended, err)
	}
	if got := status(t, e.s, e.ups["web"].ID); got != StatusFailed {
		t.Fatalf("update = %s", got)
	}
	// The host can run the next operation, and a late event changes nothing.
	if ids, _ := e.s.RunningOperationIDs(bgCtx, e.host.ID); len(ids) != 0 {
		t.Fatalf("running = %v", ids)
	}
	acked, _, err := e.s.RecordEvents(bgCtx, agentA, op.ID, []protocol.Event{{Seq: 2, Type: protocol.EventCompleted}}, t0)
	if err != nil || acked != 2 {
		t.Fatalf("acked = %d, %v", acked, err)
	}
	if got, _ := e.s.GetOperation(bgCtx, op.ID); got.State != protocol.OpInterrupted {
		t.Fatalf("state after the late event = %s", got.State)
	}
	if _, ended, _ := e.s.InterruptLostOperation(bgCtx, op.ID, t0); ended {
		t.Fatal("an operation that ended cannot end twice")
	}
}

// TestRemovingAHostReleasesItsOperations: otherwise a stack moved off a dead
// host could never be merged, since a merge waits for pending operations.
func TestRemovingAHostReleasesItsOperations(t *testing.T) {
	e := newOpEnv(t)
	res, err := e.approve(t, e.offer(t, "web"))
	if err != nil {
		t.Fatal(err)
	}
	e.s.DispatchCommands(bgCtx, e.host.ID, 120, t0)
	e.s.RecordEvents(bgCtx, agentA, res.Operation.ID, []protocol.Event{{Seq: 1, Type: protocol.EventAccepted}}, t0)
	if _, err := e.s.RemoveHost(bgCtx, e.host.ID, "host.removed", owner, t0); err != nil {
		t.Fatal(err)
	}
	if op, _ := e.s.GetOperation(bgCtx, res.Operation.ID); !op.Terminal() {
		t.Fatalf("operation = %s", op.State)
	}
}
