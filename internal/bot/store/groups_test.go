package store

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/DragonDrew/ddum/internal/bot/domain"
	"github.com/DragonDrew/ddum/internal/protocol"
)

// groupEnv: an approved host with stacks "a" (app) and "b" (worker), both
// labelled ddum.group=pair, plus "c" (lone) with no label. Each has an update.
type groupEnv struct {
	s     *Store
	host  Host
	group Group
	ups   map[string]Update
	ctrs  map[string]Container
}

func groupStack(fp, key string, labels map[string]string) protocol.Stack {
	return protocol.Stack{Fingerprint: fp, DisplayName: fp[len("compose:"):], Provider: compose, Deploy: protocol.Support{Supported: true},
		Containers: []protocol.Container{{IdentityKey: key, ImageRef: "nginx:1.27", RepoDigests: []string{"nginx@" + digest("a")},
			State: "running", TagChange: &protocol.TagChange{Supported: true, Source: "image"}, DdumLabels: labels}}}
}

func newGroupEnv(t *testing.T) *groupEnv {
	t.Helper()
	s := openTest(t)
	res, _ := s.RecordPoll(bgCtx, Poll{AgentID: agentA, HostName: "h", PollInterval: 15, Capabilities: rollbackCaps, Docker: amd64, At: t0})
	s.ApproveHost(bgCtx, res.Host.ID, owner, t0)
	label := map[string]string{"ddum.group": "pair"}
	mustInventory(t, s, agentA, groupStack("compose:a", "app", label), groupStack("compose:b", "worker", label),
		groupStack("compose:c", "lone", nil))
	e := &groupEnv{s: s, host: res.Host, ups: map[string]Update{}, ctrs: map[string]Container{}}
	groups, _ := s.Groups(bgCtx)
	if len(groups) != 1 || groups[0].DisplayName != "pair" || groups[0].Members != 2 || groups[0].Source != "label" {
		t.Fatalf("label seeding: %+v", groups)
	}
	e.group = groups[0]
	cs, _ := s.Containers(bgCtx, ContainerFilter{})
	for _, c := range cs {
		e.ctrs[c.IdentityKey] = c
		e.ups[c.IdentityKey] = e.detect(t, c, "b")
	}
	return e
}

func (e *groupEnv) detect(t *testing.T, c Container, d string) Update {
	t.Helper()
	r, err := e.s.RecordDetection(bgCtx, Detection{ContainerID: c.ID, StackID: c.StackID, HostID: c.HostID, Kind: "digest",
		CurrentRef: "nginx:1.27", CurrentDigest: digest("a"), TargetRef: "docker.io/library/nginx:1.27@" + digest(d), TargetDigest: digest(d)}, t0)
	if err != nil {
		t.Fatal(err)
	}
	return r.Update
}

func (e *groupEnv) groupOffer(t *testing.T, names ...string) string {
	t.Helper()
	var items []OfferItem
	for _, n := range names {
		items = append(items, OfferItem{UpdateID: e.ups[n].ID, TargetDigest: e.ups[n].TargetDigest})
	}
	id, err := e.s.CreateOffer(bgCtx, "group", e.group.ID, items, "notification", t0)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func (e *groupEnv) mode(t *testing.T, name, mode string) {
	t.Helper()
	if err := e.s.SetPolicy(bgCtx, PolicyKey{domain.LevelContainer, e.ctrs[name].ID}, domain.PolicyFields{Mode: &mode}, owner, t0); err != nil {
		t.Fatal(err)
	}
}

// TestGroupDeploysTwoStacksAsOneScope checks the deploy half of group scope.
func TestGroupDeploysTwoStacksAsOneScope(t *testing.T) {
	e := newGroupEnv(t)
	res, err := e.s.Approve(bgCtx, ApproveRequest{OfferID: e.groupOffer(t, "app", "worker"), By: owner, At: t0, ExpiryMinutes: 60})
	if err != nil {
		t.Fatal(err)
	}
	p := res.Operation.Payload
	if res.Operation.ScopeType != "group" || p.Scope.Kind != "group" || len(p.Targets) != 2 || len(res.Approved) != 2 {
		t.Fatalf("operation = %+v", res.Operation)
	}
	fps := p.Targets[0].StackFingerprint + "," + p.Targets[1].StackFingerprint
	if !strings.Contains(fps, "compose:a") || !strings.Contains(fps, "compose:b") {
		t.Fatalf("targets = %s", fps)
	}
	lone, _ := e.s.CreateOffer(bgCtx, "stack", e.ctrs["lone"].StackID,
		[]OfferItem{{UpdateID: e.ups["lone"].ID, TargetDigest: e.ups["lone"].TargetDigest}}, "notification", t0)
	if _, err := e.s.Approve(bgCtx, ApproveRequest{OfferID: lone, By: owner, At: t0, ExpiryMinutes: 60}); err != nil {
		t.Fatalf("a container outside the group is approved on its own: %v", err)
	}

	// Roll back as one scope: both stacks.
	e.s.DispatchCommands(bgCtx, e.host.ID, 120, t0)
	e.s.RecordEvents(bgCtx, agentA, res.Operation.ID, []protocol.Event{{Seq: 1, Type: protocol.EventAccepted},
		{Seq: 2, Type: protocol.EventCompleted, Result: &protocol.Result{
			Containers: []protocol.ContainerOutcome{{IdentityKey: "app", Outcome: protocol.OutcomeUpdated}, {IdentityKey: "worker", Outcome: protocol.OutcomeUpdated}},
			Snapshots: []protocol.SnapshotRef{{SnapshotID: "00000000-0000-7000-8000-0000000005a1", StackID: e.ctrs["app"].StackID},
				{SnapshotID: "00000000-0000-7000-8000-0000000005a2", StackID: e.ctrs["worker"].StackID}}}}}, t0)
	rb, err := e.s.CreateRollback(bgCtx, res.Operation.ID, owner, t0.Add(time.Minute), 60)
	if err != nil {
		t.Fatal(err)
	}
	var rp protocol.RollbackPayload
	jsonUnmarshal(rb.PayloadJSON, &rp)
	if rb.ScopeType != "group" || len(rp.SnapshotIDs) != 2 || len(rp.Targets) != 2 {
		t.Fatalf("group rollback = %+v %+v", rb, rp)
	}
}

// TestGroupPolicyConflict checks that a manual/automatic mix
// holds the group and names the member; an ignore member does not.
func TestGroupPolicyConflict(t *testing.T) {
	e := newGroupEnv(t)
	e.mode(t, "app", domain.ModeAutomatic)
	_, err := e.s.Approve(bgCtx, ApproveRequest{OfferID: e.groupOffer(t, "app", "worker"), By: Actor{Kind: "policy"}, At: t0, ExpiryMinutes: 60})
	var nothing *NothingError
	if !errors.As(err, &nothing) || !strings.Contains(nothing.Skipped[0].Reason, "worker (manual)") {
		t.Fatalf("conflict must hold auto-deploy and name the member: %v %+v", err, nothing)
	}
	// Manual approval of the whole group stays possible.
	if res, err := e.s.Approve(bgCtx, ApproveRequest{OfferID: e.groupOffer(t, "app", "worker"), By: owner, At: t0, ExpiryMinutes: 60}); err != nil || len(res.Approved) != 2 {
		t.Fatalf("manual approval despite conflict: %v", err)
	}
}

func TestGroupIgnoredMemberDoesNotConflict(t *testing.T) {
	e := newGroupEnv(t)
	e.mode(t, "app", domain.ModeAutomatic)
	e.mode(t, "worker", domain.ModeIgnore)
	res, err := e.s.Approve(bgCtx, ApproveRequest{OfferID: e.groupOffer(t, "app", "worker"), By: Actor{Kind: "policy"}, At: t0, ExpiryMinutes: 60})
	if err != nil || len(res.Approved) != 1 || res.Approved[0].ContainerName != "app" {
		t.Fatalf("ignore member: %+v %v", res, err)
	}
}

func TestGroupSnoozeHoldsAutoDeploy(t *testing.T) {
	e := newGroupEnv(t)
	e.mode(t, "app", domain.ModeAutomatic)
	e.mode(t, "worker", domain.ModeAutomatic)
	e.s.SnoozeUpdates(bgCtx, []string{e.ups["worker"].ID}, time.Time{}, owner, t0)
	_, err := e.s.Approve(bgCtx, ApproveRequest{OfferID: e.groupOffer(t, "app", "worker"), By: Actor{Kind: "policy"}, At: t0, ExpiryMinutes: 60})
	var nothing *NothingError
	if !errors.As(err, &nothing) || !strings.Contains(nothing.Skipped[0].Reason, "snoozed") {
		t.Fatalf("a snoozed member must hold the group's auto-deploy: %v", err)
	}
	// A manual group approval includes it and removes the snooze.
	res, err := e.s.Approve(bgCtx, ApproveRequest{OfferID: e.groupOffer(t, "app", "worker"), By: owner, At: t0, ExpiryMinutes: 60})
	if err != nil || len(res.Approved) != 2 {
		t.Fatalf("manual group approval: %+v %v", res, err)
	}
	if u, _ := e.s.GetUpdate(bgCtx, e.ups["worker"].ID, t0); u.Snoozed() {
		t.Fatal("the group approval must remove the snooze")
	}
}

func TestGroupOfferStaleOnNewMemberUpdate(t *testing.T) {
	e := newGroupEnv(t)
	offer := e.groupOffer(t, "app") // shows only app; worker's update is missing
	if _, err := e.s.Approve(bgCtx, ApproveRequest{OfferID: offer, By: owner, At: t0, ExpiryMinutes: 60}); !errors.Is(err, ErrStaleOffer) {
		t.Fatalf("err = %v, want ErrStaleOffer", err)
	}
	offer = e.groupOffer(t, "app", "worker")
	e.ups["worker"] = e.detect(t, e.ctrs["worker"], "c") // a newer member update appears
	if _, err := e.s.Approve(bgCtx, ApproveRequest{OfferID: offer, By: owner, At: t0, ExpiryMinutes: 60}); !errors.Is(err, ErrStaleOffer) {
		t.Fatalf("after a new member update: %v", err)
	}
}

func TestGroupSpansHosts(t *testing.T) {
	e := newGroupEnv(t)
	res, _ := e.s.RecordPoll(bgCtx, Poll{AgentID: agentB, HostName: "h2", PollInterval: 15, Capabilities: rollbackCaps, Docker: amd64, At: t0})
	e.s.ApproveHost(bgCtx, res.Host.ID, owner, t0)
	mustInventory(t, e.s, agentB, groupStack("compose:far", "far", map[string]string{"ddum.group": "pair"}))
	cs, _ := e.s.Containers(bgCtx, ContainerFilter{HostID: res.Host.ID})
	e.ctrs["far"] = cs[0]
	e.ups["far"] = e.detect(t, cs[0], "b")
	_, err := e.s.Approve(bgCtx, ApproveRequest{OfferID: e.groupOffer(t, "app", "worker", "far"), By: owner, At: t0, ExpiryMinutes: 60})
	var nothing *NothingError
	if !errors.As(err, &nothing) || !strings.Contains(nothing.Skipped[0].Reason, domain.HoldSpansHosts) {
		t.Fatalf("cross-host group: %v", err)
	}
}

func TestLabelNeverOverridesDiscord(t *testing.T) {
	e := newGroupEnv(t)
	label := map[string]string{"ddum.group": "pair"}
	inv := func() {
		mustInventory(t, e.s, agentA, groupStack("compose:a", "app", label), groupStack("compose:b", "worker", label),
			groupStack("compose:c", "lone", nil))
	}
	// Removing a labelled member is not undone by the label.
	if _, err := e.s.RemoveMember(bgCtx, e.ctrs["worker"].ID, owner, t0); err != nil {
		t.Fatal(err)
	}
	inv()
	if g, _ := e.s.GetGroup(bgCtx, e.group.ID); g.Members != 1 {
		t.Fatalf("members after removal + inventory = %d, want 1", g.Members)
	}
	// A container in another group stays there despite its label.
	other, _ := e.s.CreateGroup(bgCtx, "other", owner, t0)
	if err := e.s.AddMember(bgCtx, e.group.ID, e.ctrs["lone"].ID, owner, t0); err != nil {
		t.Fatal(err)
	}
	if err := e.s.AddMember(bgCtx, other.ID, e.ctrs["lone"].ID, owner, t0); !errors.Is(err, ErrInOtherGroup) {
		t.Fatalf("a container belongs to at most one group: %v", err)
	}
	// Re-adding in Discord wins over the earlier removal.
	if err := e.s.AddMember(bgCtx, e.group.ID, e.ctrs["worker"].ID, owner, t0); err != nil {
		t.Fatal(err)
	}
	if g, _ := e.s.GetGroup(bgCtx, e.group.ID); g.Members != 3 {
		t.Fatalf("members = %d, want 3", g.Members)
	}
	if _, err := e.s.DeleteGroup(bgCtx, e.group.ID, owner, t0); err != nil {
		t.Fatal(err)
	}
	inv()
	if gs, _ := e.s.Groups(bgCtx); len(gs) != 1 || gs[0].ID != other.ID {
		t.Fatalf("a deleted group must not come back from labels: %+v", gs)
	}
	if auditCount(t, e.s, "group.deleted", e.group.ID) != 1 {
		t.Fatal("group deletion audit missing")
	}
}

func TestGroupPolicyLevel(t *testing.T) {
	e := newGroupEnv(t)
	auto := domain.ModeAutomatic
	e.s.SetPolicy(bgCtx, PolicyKey{domain.LevelGroup, e.group.ID}, domain.PolicyFields{Mode: &auto}, owner, t0)
	all, _ := e.s.Policies(bgCtx)
	c := e.ctrs["app"]
	if eff := domain.ResolvePolicy(Chain(all, c.HostID, c.StackID, c.GroupID, c.ID), false); eff.Mode != auto || eff.Source[domain.FieldMode] != domain.LevelGroup {
		t.Fatalf("group level: %+v", eff)
	}
}
