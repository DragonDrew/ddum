package store

import (
	"errors"
	"testing"
	"time"

	"github.com/DragonDrew/ddum/internal/protocol"
)

func (e *groupEnv) approveGroup(t *testing.T, names ...string) (ApproveResult, error) {
	t.Helper()
	return e.s.Approve(bgCtx, ApproveRequest{OfferID: e.groupOffer(t, names...), By: owner, At: t0, ExpiryMinutes: 60})
}

// TestGroupApprovalIsAllOrNothing: a coupled group never deploys part of
// itself, so a member that cannot go now stops the others.
func TestGroupApprovalIsAllOrNothing(t *testing.T) {
	label := map[string]string{"ddum.group": "pair"}
	t.Run("a member changed by hand", func(t *testing.T) {
		e := newGroupEnv(t)
		changed := groupStack("compose:b", "worker", label)
		changed.Containers[0].RepoDigests = []string{"nginx@" + digest("c")}
		mustInventory(t, e.s, agentA, groupStack("compose:a", "app", label), changed, groupStack("compose:c", "lone", nil))
		var nothing *NothingError
		if res, err := e.approveGroup(t, "app", "worker"); !errors.As(err, &nothing) {
			t.Fatalf("approved %d of the group: %v", len(res.Approved), err)
		}
		if status(t, e.s, e.ups["app"].ID) != StatusDetected {
			t.Fatal("the other member must stay pending")
		}
	})
	t.Run("a member busy in another operation", func(t *testing.T) {
		e := newGroupEnv(t)
		mustExec(t, e.s, `INSERT INTO desired_states (id, container_id, image_ref, image_digest, status, set_at)
			VALUES ('00000000-0000-7000-8000-0000000000d1', ?, 'nginx:1.27', ?, 'pending', ?)`, e.ctrs["worker"].ID, digest("b"), now)
		var nothing *NothingError
		if res, err := e.approveGroup(t, "app", "worker"); !errors.As(err, &nothing) {
			t.Fatalf("approved %d of the group: %v", len(res.Approved), err)
		}
	})
}

// TestGroupOutcomesKeepSameNamedServicesApart: identity keys are unique per
// stack only, and a group spans stacks.
func TestGroupOutcomesKeepSameNamedServicesApart(t *testing.T) {
	s := openTest(t)
	res, _ := s.RecordPoll(bgCtx, Poll{AgentID: agentA, HostName: "h", PollInterval: 15, Capabilities: rollbackCaps, Docker: amd64, At: t0})
	s.ApproveHost(bgCtx, res.Host.ID, owner, t0)
	label := map[string]string{"ddum.group": "pair"}
	mustInventory(t, s, agentA, groupStack("compose:a", "app", label), groupStack("compose:b", "app", label))
	cs, _ := s.Containers(bgCtx, ContainerFilter{})
	if len(cs) != 2 {
		t.Fatalf("containers = %d", len(cs))
	}
	groups, _ := s.Groups(bgCtx)
	e := &groupEnv{s: s, host: res.Host, group: groups[0], ups: map[string]Update{}}
	a, b := cs[0], cs[1]
	e.ups["a"], e.ups["b"] = e.detect(t, a, "b"), e.detect(t, b, "b")
	ar, err := e.approveGroup(t, "a", "b")
	if err != nil {
		t.Fatal(err)
	}
	s.DispatchCommands(bgCtx, res.Host.ID, 120, t0)
	_, tr, err := s.RecordEvents(bgCtx, agentA, ar.Operation.ID, []protocol.Event{{Seq: 1, Type: protocol.EventAccepted},
		{Seq: 2, Type: protocol.EventFailed, Code: protocol.FailPull, Result: &protocol.Result{Containers: []protocol.ContainerOutcome{
			{IdentityKey: "app", StackID: a.StackID, Outcome: protocol.OutcomeUpdated, NewDigest: digest("b")},
			{IdentityKey: "app", StackID: b.StackID, Outcome: protocol.OutcomeFailed}}}}}, t0)
	if err != nil {
		t.Fatal(err)
	}
	if tr.Operation.State != protocol.OpPartiallyFailed {
		t.Fatalf("state = %s", tr.Operation.State)
	}
	if got := status(t, s, e.ups["a"].ID); got != StatusApplied {
		t.Errorf("the service that updated is %s", got)
	}
	if got := status(t, s, e.ups["b"].ID); got != StatusFailed {
		t.Errorf("the service that failed is %s; it must not count as applied", got)
	}
}

// TestReassignAgentReleasesItsOperations: operations belong to the agent that
// was asked to run them, and the replacement knows nothing about them.
func TestReassignAgentReleasesItsOperations(t *testing.T) {
	const newAgent = "0195f2c4-0000-7000-8000-0000000000bb"
	t.Run("queued goes back to pending", func(t *testing.T) {
		e := newOpEnv(t)
		res, err := e.approve(t, e.offer(t, "web"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := e.s.ReassignAgent(bgCtx, e.host.ID, newAgent, owner, t0); err != nil {
			t.Fatal(err)
		}
		op, _ := e.s.GetOperation(bgCtx, res.Operation.ID)
		if op.State != protocol.OpCancelled {
			t.Fatalf("queued operation = %s, want cancelled", op.State)
		}
		if status(t, e.s, e.ups["web"].ID) != StatusDetected {
			t.Fatal("its update must be pending again")
		}
	})
	t.Run("running stops blocking the host", func(t *testing.T) {
		e := newOpEnv(t)
		res, err := e.approve(t, e.offer(t, "web"))
		if err != nil {
			t.Fatal(err)
		}
		e.s.DispatchCommands(bgCtx, e.host.ID, 120, t0)
		e.s.RecordEvents(bgCtx, agentA, res.Operation.ID, []protocol.Event{{Seq: 1, Type: protocol.EventAccepted}}, t0)
		if _, err := e.s.ReassignAgent(bgCtx, e.host.ID, newAgent, owner, t0); err != nil {
			t.Fatal(err)
		}
		op, _ := e.s.GetOperation(bgCtx, res.Operation.ID)
		if !op.Terminal() {
			t.Fatalf("operation = %s: the host would be blocked for good", op.State)
		}
	})
}

// TestApproveRefusesContainersThatAreGone: a card can outlive its container.
func TestApproveRefusesContainersThatAreGone(t *testing.T) {
	e := newOpEnv(t)
	st := protocol.Stack{Fingerprint: "compose:app", DisplayName: "app", Provider: compose, Deploy: protocol.Support{Supported: true},
		Containers: []protocol.Container{{IdentityKey: "db", ImageRef: "nginx:1.27", RepoDigests: []string{"nginx@" + digest("a")}, State: "running"}}}
	mustInventory(t, e.s, agentA, st) // web and proxy are no longer there
	var nothing *NothingError
	if _, err := e.approve(t, e.offer(t, "web")); !errors.As(err, &nothing) {
		t.Fatalf("approve = %v; the agent would only refuse it as unsafe", err)
	}
}

// webRunning reports the "web" container running imageRef at digest d.
func (e *opEnv) webRunning(t *testing.T, imageRef, d string, rollback ...string) {
	t.Helper()
	st := protocol.Stack{Fingerprint: "compose:app", DisplayName: "app", Provider: compose, Deploy: protocol.Support{Supported: true},
		Rollback: protocol.RollbackInfo{Available: len(rollback) > 0, SnapshotIDs: rollback},
		Containers: []protocol.Container{{IdentityKey: "web", ImageRef: imageRef, RepoDigests: []string{"nginx@" + d}, State: "running",
			TagChange: &protocol.TagChange{Supported: true, Source: "image"}, DdumLabels: map[string]string{}}}}
	mustInventory(t, e.s, agentA, st)
}

const snapshotA = "00000000-0000-7000-8000-0000000005a1"

// TestRollbackRefusedAfterAManualChange: restoring a snapshot would silently
// undo a version the owner moved to by hand, the same harm as approving a
// stale update.
func TestRollbackRefusedAfterAManualChange(t *testing.T) {
	e := newOpEnv(t)
	orig := e.deployed(t, completedWeb)
	e.webRunning(t, "nginx:1.28", digest("b"), snapshotA)
	if err := e.s.CanRollBack(bgCtx, orig.ID); err != nil {
		t.Fatalf("running what was deployed: %v", err)
	}
	e.webRunning(t, "nginx:1.30", digest("e"), snapshotA)
	if err := e.s.CanRollBack(bgCtx, orig.ID); !errors.Is(err, ErrChangedOutside) {
		t.Fatalf("after a manual update: %v, want ErrChangedOutside", err)
	}
	if _, err := e.s.CreateRollback(bgCtx, orig.ID, owner, t0, 60); !errors.Is(err, ErrChangedOutside) {
		t.Fatalf("create = %v", err)
	}
}

// TestLaterGroupOperationBlocksStackRollback: a snapshot restores whole files,
// so it would also undo what a later group operation changed in the stack.
func TestLaterGroupOperationBlocksStackRollback(t *testing.T) {
	s := openTest(t)
	res, _ := s.RecordPoll(bgCtx, Poll{AgentID: agentA, HostName: "h", PollInterval: 15, Capabilities: rollbackCaps, Docker: amd64, At: t0})
	s.ApproveHost(bgCtx, res.Host.ID, owner, t0)
	label := map[string]string{"ddum.group": "pair"}
	a := groupStack("compose:a", "app", label)
	a.Containers = append(a.Containers, groupStack("compose:a", "solo", nil).Containers[0])
	mustInventory(t, s, agentA, a, groupStack("compose:b", "worker", label))
	groups, _ := s.Groups(bgCtx)
	e := &groupEnv{s: s, host: res.Host, group: groups[0], ups: map[string]Update{}, ctrs: map[string]Container{}}
	cs, _ := s.Containers(bgCtx, ContainerFilter{})
	for _, c := range cs {
		e.ctrs[c.IdentityKey] = c
		e.ups[c.IdentityKey] = e.detect(t, c, "b")
	}
	offer, _ := s.CreateOffer(bgCtx, "stack", e.ctrs["solo"].StackID,
		[]OfferItem{{UpdateID: e.ups["solo"].ID, TargetDigest: e.ups["solo"].TargetDigest}}, "notification", t0)
	first, err := s.Approve(bgCtx, ApproveRequest{OfferID: offer, By: owner, At: t0, ExpiryMinutes: 60})
	if err != nil {
		t.Fatal(err)
	}
	s.DispatchCommands(bgCtx, res.Host.ID, 120, t0)
	if _, _, err := s.RecordEvents(bgCtx, agentA, first.Operation.ID, []protocol.Event{{Seq: 1, Type: protocol.EventAccepted},
		{Seq: 2, Type: protocol.EventCompleted, Result: &protocol.Result{
			Containers: []protocol.ContainerOutcome{{IdentityKey: "solo", StackID: e.ctrs["solo"].StackID, Outcome: protocol.OutcomeUpdated, NewDigest: digest("b")}},
			Snapshots:  []protocol.SnapshotRef{{SnapshotID: snapshotA, StackID: e.ctrs["solo"].StackID, Created: true}}}}}, t0); err != nil {
		t.Fatal(err)
	}
	if err := s.CanRollBack(bgCtx, first.Operation.ID); err != nil {
		t.Fatalf("before the group deploys: %v", err)
	}
	later, err := s.Approve(bgCtx, ApproveRequest{OfferID: e.groupOffer(t, "app", "worker"), By: owner, At: t0.Add(time.Minute), ExpiryMinutes: 60})
	if err != nil {
		t.Fatal(err)
	}
	if later.Operation.ScopeType != "group" {
		t.Fatalf("scope = %s", later.Operation.ScopeType)
	}
	if err := s.CanRollBack(bgCtx, first.Operation.ID); !errors.Is(err, ErrNotLatest) {
		t.Fatalf("after the group operation: %v, want ErrNotLatest", err)
	}
}

// TestInventoryClosesOvertakenUpdates: a manual update shows within one poll,
// not at the next registry check.
func TestInventoryClosesOvertakenUpdates(t *testing.T) {
	t.Run("past the target", func(t *testing.T) {
		e := newOpEnv(t) // web: 1.27 → 1.28
		e.webRunning(t, "nginx:1.29", digest("e"))
		if got := status(t, e.s, e.ups["web"].ID); got != StatusSuperseded {
			t.Fatalf("status = %s, want superseded", got)
		}
	})
	t.Run("still behind the target", func(t *testing.T) {
		e := newOpEnv(t)
		e.webRunning(t, "nginx:1.27.5", digest("e"))
		u, _ := e.s.GetUpdate(bgCtx, e.ups["web"].ID, t0)
		if u.Status != StatusDetected || u.CurrentRef != "nginx:1.27.5" || u.CurrentDigest != digest("e") {
			t.Fatalf("update = %s, %s, %s; it must show what runs now", u.Status, u.CurrentRef, u.CurrentDigest)
		}
		if _, err := e.approve(t, e.offer(t, "web")); err != nil {
			t.Fatalf("approving what is still an upgrade: %v", err)
		}
	})
	t.Run("an inventory that changes nothing leaves it alone", func(t *testing.T) {
		e := newOpEnv(t)
		e.webRunning(t, "nginx:1.27", digest("a"))
		if got := status(t, e.s, e.ups["web"].ID); got != StatusDetected {
			t.Fatalf("status = %s", got)
		}
	})
	t.Run("reports the stack so its card can be edited", func(t *testing.T) {
		e := newOpEnv(t)
		st := protocol.Stack{Fingerprint: "compose:app", DisplayName: "app", Provider: compose, Deploy: protocol.Support{Supported: true},
			Containers: []protocol.Container{{IdentityKey: "web", ImageRef: "nginx:1.29", RepoDigests: []string{"nginx@" + digest("e")}, State: "running"}}}
		res := mustInventory(t, e.s, agentA, st)
		if len(res.ChangedStacks) != 1 || res.ChangedStacks[0] != e.stackID {
			t.Fatalf("changed stacks = %v", res.ChangedStacks)
		}
	})
}
