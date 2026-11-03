package store

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/DragonDrew/ddum/internal/bot/domain"
	"github.com/DragonDrew/ddum/internal/protocol"
)

var deployCaps = []string{protocol.CapProviderCompose, protocol.CapActionDeploy, protocol.CapTagChangeEdit}

func digest(c string) string { return "sha256:" + strings.Repeat(c, 64) }

// opEnv is an approved, deploy-capable host with stack "app": web, db
// (ddum.ignore), and proxy (protected), each with a detected update.
type opEnv struct {
	s       *Store
	host    Host
	stackID string
	ups     map[string]Update // by container name
}

func newOpEnv(t *testing.T) *opEnv {
	t.Helper()
	s := openTest(t)
	res, err := s.RecordPoll(bgCtx, Poll{AgentID: agentA, HostName: "h", PollInterval: 15, Capabilities: deployCaps, Docker: amd64, At: t0})
	if err != nil {
		t.Fatal(err)
	}
	s.ApproveHost(bgCtx, res.Host.ID, owner, t0)
	st := protocol.Stack{Fingerprint: "compose:app", DisplayName: "app", Provider: compose, Deploy: protocol.Support{Supported: true}}
	for _, n := range []string{"web", "db", "proxy"} {
		c := protocol.Container{IdentityKey: n, ImageRef: "nginx:1.27", RepoDigests: []string{"nginx@" + digest("a")},
			State: "running", TagChange: &protocol.TagChange{Supported: true, Source: "image"}, DdumLabels: map[string]string{}}
		if n == "db" {
			c.DdumLabels["ddum.ignore"] = "true"
		}
		if n == "proxy" {
			c.Protected = true
		}
		st.Containers = append(st.Containers, c)
	}
	mustInventory(t, s, agentA, st)
	e := &opEnv{s: s, host: res.Host, ups: map[string]Update{}}
	cs, _ := s.Containers(bgCtx, ContainerFilter{})
	for _, c := range cs {
		e.stackID = c.StackID
		r, err := s.RecordDetection(bgCtx, Detection{ContainerID: c.ID, StackID: c.StackID, HostID: c.HostID, Kind: "version",
			CurrentRef: "nginx:1.27", CurrentDigest: digest("a"), TargetRef: "docker.io/library/nginx:1.28@" + digest("b"),
			TargetDigest: digest("b"), TargetVersion: "1.28"}, t0)
		if err != nil {
			t.Fatal(err)
		}
		e.ups[c.IdentityKey] = r.Update
	}
	return e
}

func (e *opEnv) offer(t *testing.T, names ...string) string {
	t.Helper()
	var items []OfferItem
	for _, n := range names {
		items = append(items, OfferItem{UpdateID: e.ups[n].ID, TargetDigest: e.ups[n].TargetDigest})
	}
	id, err := e.s.CreateOffer(bgCtx, "stack", e.stackID, items, "notification", t0)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func (e *opEnv) approve(t *testing.T, offer string) (ApproveResult, error) {
	t.Helper()
	return e.s.Approve(bgCtx, ApproveRequest{OfferID: offer, By: owner, At: t0, ExpiryMinutes: 60})
}

func TestApproveCreatesOneOperation(t *testing.T) {
	e := newOpEnv(t)
	offer := e.offer(t, "web", "db", "proxy")
	res, err := e.approve(t, offer)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Approved) != 1 || res.Approved[0].ContainerName != "web" || res.Operation.State != protocol.OpQueued {
		t.Fatalf("approve: %+v", res)
	}
	reasons := map[string]string{}
	for _, sk := range res.Skipped {
		reasons[sk.Update.ContainerName] = sk.Reason
	}
	if reasons["db"] != "ignored by policy" || !strings.HasPrefix(reasons["proxy"], "protected") {
		t.Fatalf("skipped = %v; ignored and protected members are left out without holding the stack", reasons)
	}
	p := res.Operation.Payload
	if len(p.Targets) != 1 || p.Targets[0].StackFingerprint != "compose:app" || len(p.Targets[0].Services) != 1 {
		t.Fatalf("payload = %+v", p)
	}
	svc := p.Targets[0].Services[0]
	if svc.Desired.ImageRef != "docker.io/library/nginx:1.28" || svc.Desired.Digest != digest("b") || p.Verify.TimeoutSeconds != 120 {
		t.Fatalf("service = %+v verify=%+v", svc, p.Verify)
	}
	if status(t, e.s, e.ups["web"].ID) != StatusApproved || status(t, e.s, e.ups["proxy"].ID) != StatusDetected {
		t.Fatal("statuses after approval")
	}

	// A second click, by anyone, creates nothing.
	_, err = e.approve(t, offer)
	var already *AlreadyApprovedError
	if !errors.As(err, &already) || already.OperationID != res.Operation.ID || already.ByDiscordID != owner.DiscordID {
		t.Fatalf("second approve err = %v", err)
	}
	ops, _ := e.s.Operations(bgCtx, OperationFilter{})
	if len(ops) != 1 {
		t.Fatalf("operations = %d, want 1", len(ops))
	}
	if auditCount(t, e.s, "update.approved", e.ups["web"].ID) != 1 {
		t.Fatal("approval audit missing")
	}
}

func TestStaleOfferApprovesNothing(t *testing.T) {
	e := newOpEnv(t)
	offer := e.offer(t, "web")
	// A newer update supersedes the offered one.
	cs, _ := e.s.Containers(bgCtx, ContainerFilter{})
	for _, c := range cs {
		if c.IdentityKey == "web" {
			e.s.RecordDetection(bgCtx, Detection{ContainerID: c.ID, StackID: c.StackID, HostID: c.HostID, Kind: "version",
				CurrentRef: "nginx:1.27", TargetRef: "docker.io/library/nginx:1.29@" + digest("c"), TargetDigest: digest("c")}, t0)
		}
	}
	if _, err := e.approve(t, offer); !errors.Is(err, ErrStaleOffer) {
		t.Fatalf("err = %v, want ErrStaleOffer", err)
	}
	if ops, _ := e.s.Operations(bgCtx, OperationFilter{}); len(ops) != 0 {
		t.Fatal("a stale offer created an operation")
	}
}

func TestApproveRefusals(t *testing.T) {
	e := newOpEnv(t)
	// Only protected and ignored members: nothing to approve, and it says why.
	_, err := e.approve(t, e.offer(t, "proxy", "db"))
	var nothing *NothingError
	if !errors.As(err, &nothing) || len(nothing.Skipped) != 2 {
		t.Fatalf("err = %v", err)
	}
	// An automatic member is left to the policy engine.
	auto := domain.ModeAutomatic
	e.s.SetPolicy(bgCtx, PolicyKey{domain.LevelStack, e.stackID}, domain.PolicyFields{Mode: &auto}, owner, t0)
	if _, err := e.approve(t, e.offer(t, "web")); !errors.As(err, &nothing) || nothing.Skipped[0].Reason != "policy is automatic" {
		t.Fatalf("automatic for a user: %v", err)
	}
	res, err := e.s.Approve(bgCtx, ApproveRequest{OfferID: e.offer(t, "web"), By: Actor{Kind: "policy"}, At: t0, ExpiryMinutes: 60})
	if err != nil || len(res.Approved) != 1 || res.Operation.InitiatorKind != "policy" {
		t.Fatalf("policy approval: %+v %v", res, err)
	}
	// An agent that cannot deploy gets nothing queued.
	e2 := newOpEnv(t)
	e2.s.RecordPoll(bgCtx, Poll{AgentID: agentA, HostName: "h", PollInterval: 15, Capabilities: []string{"provider:compose"}, Docker: amd64, At: t0})
	if _, err := e2.approve(t, e2.offer(t, "web")); !errors.As(err, &nothing) || !strings.Contains(nothing.Skipped[0].Reason, "action:deploy") {
		t.Fatalf("no capability: %v", err)
	}
}

func TestDispatchLeaseAndExpiry(t *testing.T) {
	e := newOpEnv(t)
	res, _ := e.approve(t, e.offer(t, "web"))
	id := res.Operation.ID

	got, err := e.s.DispatchCommands(bgCtx, e.host.ID, 120, t0)
	if err != nil || len(got) != 1 || got[0].ID != id || got[0].State != protocol.OpDispatched || got[0].Attempt != 1 {
		t.Fatalf("dispatch: %+v %v", got, err)
	}
	if status(t, e.s, e.ups["web"].ID) != StatusDeploying {
		t.Fatal("dispatched update should be deploying")
	}
	// Polling again within the lease re-sends the same command.
	if again, _ := e.s.DispatchCommands(bgCtx, e.host.ID, 120, t0.Add(10*time.Second)); len(again) != 1 || again[0].Attempt != 1 {
		t.Fatalf("re-send: %+v", again)
	}
	// Lease expiry requeues, up to three attempts, then fails.
	at := t0
	for attempt := 1; attempt <= MaxDispatchAttempts; attempt++ {
		at = at.Add(3 * time.Minute)
		sw, _ := e.s.SweepOperations(bgCtx, at)
		if attempt < MaxDispatchAttempts {
			if len(sw.Requeued) != 1 {
				t.Fatalf("attempt %d: %+v", attempt, sw)
			}
			e.s.DispatchCommands(bgCtx, e.host.ID, 120, at)
		} else if len(sw.Failed) != 1 || sw.Failed[0].Outcome.Code != protocol.FailNotAcknowledged {
			t.Fatalf("final attempt: %+v", sw)
		}
	}
	if status(t, e.s, e.ups["web"].ID) != StatusFailed {
		t.Fatal("unacknowledged operation should fail its updates")
	}

	// A queued operation past expires_at expires; its update is pending again.
	e2 := newOpEnv(t)
	r2, _ := e2.approve(t, e2.offer(t, "web"))
	sw, _ := e2.s.SweepOperations(bgCtx, t0.Add(61*time.Minute))
	if len(sw.Expired) != 1 || sw.Expired[0].ID != r2.Operation.ID || status(t, e2.s, e2.ups["web"].ID) != StatusDetected {
		t.Fatalf("expiry: %+v", sw)
	}
	if got, _ := e2.s.DispatchCommands(bgCtx, e2.host.ID, 120, t0.Add(62*time.Minute)); len(got) != 0 {
		t.Fatal("an expired operation was dispatched")
	}
}

func TestOneActiveOperationPerHostFIFO(t *testing.T) {
	e := newOpEnv(t)
	first, _ := e.approve(t, e.offer(t, "web"))
	e.s.SetPolicy(bgCtx, PolicyKey{domain.LevelContainer, e.ups["db"].ContainerID}, domain.PolicyFields{Mode: ptrS(domain.ModeManual)}, owner, t0)
	mustExec(t, e.s, `UPDATE containers SET ddum_labels_json = '{}' WHERE id = ?`, e.ups["db"].ContainerID)
	second, err := e.approve(t, e.offer(t, "db"))
	if err != nil || second.QueuePosition != 1 {
		t.Fatalf("second: %+v %v", second, err)
	}
	got, _ := e.s.DispatchCommands(bgCtx, e.host.ID, 120, t0)
	e.s.RecordEvents(bgCtx, agentA, got[0].ID, []protocol.Event{{Seq: 1, Type: protocol.EventAccepted}}, t0)
	if got, _ := e.s.DispatchCommands(bgCtx, e.host.ID, 120, t0); len(got) != 0 {
		t.Fatal("a second operation was dispatched while one is active")
	}
	e.s.RecordEvents(bgCtx, agentA, first.Operation.ID, []protocol.Event{{Seq: 2, Type: protocol.EventCompleted,
		Result: &protocol.Result{Containers: []protocol.ContainerOutcome{{IdentityKey: "web", Outcome: protocol.OutcomeUpdated}}}}}, t0)
	if got, _ := e.s.DispatchCommands(bgCtx, e.host.ID, 120, t0); len(got) != 1 || got[0].ID != second.Operation.ID {
		t.Fatalf("next in line: %+v", got)
	}
}

func ptrS(s string) *string { return &s }

func TestEventsStateMachineAndDedup(t *testing.T) {
	e := newOpEnv(t)
	res, _ := e.approve(t, e.offer(t, "web"))
	id := res.Operation.ID
	e.s.DispatchCommands(bgCtx, e.host.ID, 120, t0)

	acked, tr, err := e.s.RecordEvents(bgCtx, agentA, id, []protocol.Event{
		{Seq: 1, Type: protocol.EventAccepted}, {Seq: 2, Type: protocol.EventProgress, Step: protocol.StepPulling},
	}, t0)
	if err != nil || acked != 2 || tr == nil || tr.Operation.State != protocol.OpRunning {
		t.Fatalf("first batch: acked=%d tr=%+v err=%v", acked, tr, err)
	}
	// Duplicate delivery plus a gap: acked stops before the gap.
	acked, _, _ = e.s.RecordEvents(bgCtx, agentA, id, []protocol.Event{
		{Seq: 2, Type: protocol.EventProgress, Step: protocol.StepPulling}, {Seq: 4, Type: protocol.EventProgress, Step: protocol.StepVerifying},
	}, t0)
	if acked != 2 {
		t.Fatalf("acked with a gap = %d, want 2", acked)
	}
	acked, tr, _ = e.s.RecordEvents(bgCtx, agentA, id, []protocol.Event{
		{Seq: 3, Type: protocol.EventProgress, Step: protocol.StepDeploying},
		{Seq: 5, Type: protocol.EventCompleted, Result: &protocol.Result{Containers: []protocol.ContainerOutcome{
			{IdentityKey: "web", Outcome: protocol.OutcomeUpdated, NewDigest: digest("b")}}}},
	}, t0)
	if acked != 5 || tr == nil || !tr.Finished || tr.Operation.State != protocol.OpSucceeded {
		t.Fatalf("completion: acked=%d tr=%+v", acked, tr)
	}
	if status(t, e.s, e.ups["web"].ID) != StatusApplied {
		t.Fatal("update should be applied")
	}
	var reached int
	e.s.Reader().QueryRow(`SELECT count(*) FROM desired_states WHERE status = 'reached' AND image_digest = ?`, digest("b")).Scan(&reached)
	if reached != 1 {
		t.Fatal("desired state should be reached")
	}
	if evs, _ := e.s.OperationEvents(bgCtx, id); len(evs) != 5 {
		t.Fatalf("events stored = %d, want 5 (duplicates dropped)", len(evs))
	}
	// Another agent cannot report on this operation.
	if _, _, err := e.s.RecordEvents(bgCtx, agentB, id, []protocol.Event{{Seq: 9, Type: protocol.EventFailed}}, t0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign agent err = %v", err)
	}
}

func TestFailureAndInterruption(t *testing.T) {
	e := newOpEnv(t)
	res, _ := e.approve(t, e.offer(t, "web"))
	e.s.DispatchCommands(bgCtx, e.host.ID, 120, t0)
	_, tr, _ := e.s.RecordEvents(bgCtx, agentA, res.Operation.ID, []protocol.Event{
		{Seq: 1, Type: protocol.EventAccepted},
		{Seq: 2, Type: protocol.EventFailed, Code: protocol.FailPull, Message: "manifest unknown", OutputTail: "error pulling"},
	}, t0)
	if tr.Operation.State != protocol.OpFailed || tr.Operation.Outcome.Code != protocol.FailPull || tr.Operation.Outcome.OutputTail != "error pulling" {
		t.Fatalf("failed: %+v", tr.Operation)
	}
	if status(t, e.s, e.ups["web"].ID) != StatusFailed {
		t.Fatal("update should be failed")
	}

	e2 := newOpEnv(t)
	r2, _ := e2.approve(t, e2.offer(t, "web"))
	e2.s.DispatchCommands(bgCtx, e2.host.ID, 120, t0)
	_, tr, _ = e2.s.RecordEvents(bgCtx, agentA, r2.Operation.ID, []protocol.Event{{Seq: 1, Type: protocol.EventAccepted},
		{Seq: 2, Type: protocol.EventInterrupted}}, t0)
	if tr.Operation.State != protocol.OpInterrupted {
		t.Fatalf("interrupted: %+v", tr.Operation)
	}
}

func TestCancel(t *testing.T) {
	e := newOpEnv(t)
	res, _ := e.approve(t, e.offer(t, "web"))
	op, err := e.s.CancelOperation(bgCtx, res.Operation.ID, owner, t0)
	if err != nil || op.State != protocol.OpCancelled || status(t, e.s, e.ups["web"].ID) != StatusDetected {
		t.Fatalf("cancel: %+v %v", op, err)
	}
	e2 := newOpEnv(t)
	r2, _ := e2.approve(t, e2.offer(t, "web"))
	e2.s.DispatchCommands(bgCtx, e2.host.ID, 120, t0)
	e2.s.RecordEvents(bgCtx, agentA, r2.Operation.ID, []protocol.Event{{Seq: 1, Type: protocol.EventAccepted}}, t0)
	if _, err := e2.s.CancelOperation(bgCtx, r2.Operation.ID, owner, t0); !errors.Is(err, ErrNotCancellable) {
		t.Fatalf("cancel accepted op: %v", err)
	}
}

func TestDrift(t *testing.T) {
	e := newOpEnv(t)
	res, _ := e.approve(t, e.offer(t, "web"))
	e.s.DispatchCommands(bgCtx, e.host.ID, 120, t0)
	e.s.RecordEvents(bgCtx, agentA, res.Operation.ID, []protocol.Event{{Seq: 1, Type: protocol.EventAccepted},
		{Seq: 2, Type: protocol.EventCompleted, Result: &protocol.Result{Containers: []protocol.ContainerOutcome{
			{IdentityKey: "web", Outcome: protocol.OutcomeUpdated}}}}}, t0)

	inv := func(webDigest string) InventoryResult {
		st := protocol.Stack{Fingerprint: "compose:app", DisplayName: "app", Provider: compose, Deploy: protocol.Support{Supported: true}}
		st.Containers = []protocol.Container{{IdentityKey: "web", ImageRef: "nginx:1.28", RepoDigests: []string{"nginx@" + webDigest}}}
		r, err := e.s.ApplyInventory(bgCtx, agentA, protocol.Inventory{InventoryHash: "x", Stacks: []protocol.Stack{st}}, t0)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	if r := inv(digest("b")); len(r.Drifts) != 0 {
		t.Fatalf("running the desired digest is not drift: %+v", r.Drifts)
	}
	r := inv(digest("e"))
	if len(r.Drifts) != 1 || r.Drifts[0].Expected != digest("b") || r.Drifts[0].Observed != digest("e") {
		t.Fatalf("drift: %+v", r.Drifts)
	}
	if r := inv(digest("e")); len(r.Drifts) != 0 {
		t.Fatal("drift must be reported once, then re-based")
	}
	if auditCount(t, e.s, "container.drifted", e.ups["web"].ContainerID) != 1 {
		t.Fatal("drift audit missing")
	}
}

// TestScheduledOperationWaitsWithoutBlocking: a scheduled deploy is not
// dispatched before its time, does not hold up later ones, and expires
// relative to its time.
func TestScheduledOperationWaitsWithoutBlocking(t *testing.T) {
	e := newOpEnv(t)
	at := t0.Add(5 * time.Hour)
	sched, err := e.s.Approve(bgCtx, ApproveRequest{OfferID: e.offer(t, "web"), By: owner, At: t0, ExpiryMinutes: 60, NotBefore: at})
	if err != nil {
		t.Fatal(err)
	}
	if !sched.Operation.NotBefore.Equal(at) || !sched.Operation.ExpiresAt.Equal(at.Add(time.Hour)) || sched.Operation.Payload.ExpiresAt != at.Add(time.Hour).UTC().Format(time.RFC3339) {
		t.Fatalf("scheduled op: not_before=%v expires=%v payload=%s", sched.Operation.NotBefore, sched.Operation.ExpiresAt, sched.Operation.Payload.ExpiresAt)
	}
	if u, _ := e.s.GetUpdate(bgCtx, e.ups["web"].ID, t0); !u.ScheduledFor.Equal(at) {
		t.Fatalf("update scheduled for %v", u.ScheduledFor)
	}
	// Two hours later: still waiting, and the sweeper leaves it alone.
	if got, _ := e.s.DispatchCommands(bgCtx, e.host.ID, 120, t0.Add(2*time.Hour)); len(got) != 0 {
		t.Fatal("dispatched before its time")
	}
	if res, _ := e.s.SweepOperations(bgCtx, t0.Add(2*time.Hour)); len(res.Expired) != 0 {
		t.Fatal("a scheduled operation expired before its time")
	}
	// An immediate deploy approved later goes first.
	e.s.SetPolicy(bgCtx, PolicyKey{domain.LevelContainer, e.ups["db"].ContainerID}, domain.PolicyFields{Mode: ptrS(domain.ModeManual)}, owner, t0)
	mustExec(t, e.s, `UPDATE containers SET ddum_labels_json = '{}' WHERE id = ?`, e.ups["db"].ContainerID)
	now, err := e.s.Approve(bgCtx, ApproveRequest{OfferID: e.offer(t, "db"), By: owner, At: t0.Add(2 * time.Hour), ExpiryMinutes: 60})
	if err != nil || now.QueuePosition != 0 {
		t.Fatalf("immediate: %+v %v", now, err)
	}
	got, _ := e.s.DispatchCommands(bgCtx, e.host.ID, 120, t0.Add(2*time.Hour))
	if len(got) != 1 || got[0].ID != now.Operation.ID {
		t.Fatalf("the immediate deploy should not wait behind the scheduled one: %+v", got)
	}
	e.s.RecordEvents(bgCtx, agentA, now.Operation.ID, []protocol.Event{{Seq: 1, Type: protocol.EventAccepted}, {Seq: 2, Type: protocol.EventCompleted,
		Result: &protocol.Result{Containers: []protocol.ContainerOutcome{{IdentityKey: "db", Outcome: protocol.OutcomeUpdated}}}}}, t0.Add(2*time.Hour))
	// At its time, it goes.
	if got, _ := e.s.DispatchCommands(bgCtx, e.host.ID, 120, at); len(got) != 1 || got[0].ID != sched.Operation.ID {
		t.Fatalf("at its time: %+v", got)
	}
}

// TestApprovalSkipsUpdatesThatNoLongerFit covers a container changed by hand
// after its update was found.
func TestApprovalSkipsUpdatesThatNoLongerFit(t *testing.T) {
	reason := func(t *testing.T, e *opEnv) string {
		t.Helper()
		var nothing *NothingError
		if _, err := e.approve(t, e.offer(t, "web")); !errors.As(err, &nothing) || len(nothing.Skipped) != 1 {
			t.Fatalf("approval: %v", err)
		}
		return nothing.Skipped[0].Reason
	}
	// A digest update of an unchanged tag: only the registry can say which of
	// two digests is newer, so it stays open and cannot be approved.
	digestUpdate := func(t *testing.T, e *opEnv) {
		t.Helper()
		cs, _ := e.s.Containers(bgCtx, ContainerFilter{})
		for _, c := range cs {
			if c.IdentityKey == "web" {
				r, err := e.s.RecordDetection(bgCtx, Detection{ContainerID: c.ID, StackID: c.StackID, HostID: c.HostID, Kind: "digest",
					CurrentRef: "nginx:1.27", CurrentDigest: digest("a"), TargetRef: "docker.io/library/nginx:1.27@" + digest("c"), TargetDigest: digest("c")}, t0)
				if err != nil {
					t.Fatal(err)
				}
				e.ups["web"] = r.Update
			}
		}
	}

	t.Run("runs another digest", func(t *testing.T) {
		e := newOpEnv(t)
		digestUpdate(t, e)
		e.webRunning(t, "nginx:1.27", digest("e"))
		if r := reason(t, e); !strings.Contains(r, "changed after this update was found") {
			t.Fatalf("reason = %q", r)
		}
	})
	t.Run("a downgrade", func(t *testing.T) {
		e := newOpEnv(t)
		e.webRunning(t, "nginx:1.29", digest("a"))
		if r := reason(t, e); !strings.Contains(r, "older release") {
			t.Fatalf("reason = %q", r)
		}
	})
	// Docker's containerd store forgets a running image once its tag moves, so
	// no digest at all is no sign of a change, and must not block the update.
	t.Run("no digest known", func(t *testing.T) {
		e := newOpEnv(t)
		digestUpdate(t, e)
		st := protocol.Stack{Fingerprint: "compose:app", DisplayName: "app", Provider: compose, Deploy: protocol.Support{Supported: true},
			Containers: []protocol.Container{{IdentityKey: "web", ImageRef: "nginx:1.27", RepoDigests: []string{}, State: "running", DdumLabels: map[string]string{}}}}
		mustInventory(t, e.s, agentA, st)
		if _, err := e.approve(t, e.offer(t, "web")); err != nil {
			t.Fatalf("approval: %v", err)
		}
	})
}
