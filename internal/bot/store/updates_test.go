package store

import (
	"testing"
	"time"

	"github.com/DragonDrew/ddum/internal/bot/domain"
	"github.com/DragonDrew/ddum/internal/protocol"
)

// seedContainer registers agentA with one compose stack "app" holding "web",
// and returns the web container.
func seedContainer(t *testing.T, s *Store) Container {
	t.Helper()
	poll(t, s, agentA, "a", t0)
	mustInventory(t, s, agentA, stackOf("compose:app", compose, "web"))
	cs, err := s.Containers(bgCtx, ContainerFilter{})
	if err != nil || len(cs) != 1 {
		t.Fatalf("containers = %v, %v", cs, err)
	}
	return cs[0]
}

func detect(t *testing.T, s *Store, c Container, digest string, at time.Time) DetectionResult {
	t.Helper()
	r, err := s.RecordDetection(bgCtx, Detection{ContainerID: c.ID, StackID: c.StackID, HostID: c.HostID, Kind: "digest",
		CurrentRef: "nginx:1.27", CurrentDigest: "sha256:aaa", TargetRef: "docker.io/library/nginx:1.27@" + digest,
		TargetDigest: digest}, at)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func status(t *testing.T, s *Store, id string) string {
	t.Helper()
	u, err := s.GetUpdate(bgCtx, id, t0)
	if err != nil {
		t.Fatal(err)
	}
	return u.Status
}

func TestRecordDetectionLifecycle(t *testing.T) {
	s := openTest(t)
	c := seedContainer(t, s)

	first := detect(t, s, c, "sha256:bbb", t0)
	if !first.New || first.Update.Status != StatusDetected {
		t.Fatalf("first: %+v", first)
	}
	again := detect(t, s, c, "sha256:bbb", t0.Add(time.Hour))
	if again.New || again.Update.ID != first.Update.ID {
		t.Fatalf("re-detecting the same digest must refresh, not create: %+v", again)
	}

	newer := detect(t, s, c, "sha256:ccc", t0.Add(2*time.Hour))
	if !newer.New || newer.Superseded != 1 || status(t, s, first.Update.ID) != StatusSuperseded {
		t.Fatalf("newer detection must supersede the older unapproved one: %+v", newer)
	}

	// Approved updates are never superseded.
	mustExec(t, s, `UPDATE updates SET status = 'approved' WHERE id = ?`, newer.Update.ID)
	detect(t, s, c, "sha256:ddd", t0.Add(3*time.Hour))
	if status(t, s, newer.Update.ID) != StatusApproved {
		t.Fatal("an approved update was superseded")
	}

	// A superseded digest seen again is reopened, flagged, and audited.
	mustExec(t, s, `UPDATE updates SET notified_at = ? WHERE id = ?`, now, first.Update.ID)
	re := detect(t, s, c, "sha256:bbb", t0.Add(4*time.Hour))
	if !re.New || !re.Reopened || re.Update.ID != first.Update.ID || re.Update.Status != StatusDetected || !re.Update.NotifiedAt.IsZero() {
		t.Fatalf("reopen: %+v", re)
	}
	if auditCount(t, s, "update.reopened", first.Update.ID) != 1 {
		t.Fatal("update.reopened audit missing")
	}
}

func TestAppliedExternallyThroughInventory(t *testing.T) {
	s := openTest(t)
	c := seedContainer(t, s)
	d := "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	r := detect(t, s, c, d, t0)

	st := stackOf("compose:app", compose, "web")
	st.Containers[0].RepoDigests = []string{"nginx@" + d}
	mustInventory(t, s, agentA, st)
	if status(t, s, r.Update.ID) != StatusApplied {
		t.Fatal("running the target digest must mark the update applied")
	}
	if auditCount(t, s, "update.applied_externally", r.Update.ID) != 1 {
		t.Fatal("update.applied_externally audit missing")
	}
}

func TestUpdatesFilterAndVisibility(t *testing.T) {
	s := openTest(t)
	c := seedContainer(t, s)
	detect(t, s, c, "sha256:bbb", t0)
	all, _ := s.Updates(bgCtx, UpdateFilter{Statuses: []string{StatusDetected}}, t0)
	vis, _ := s.Updates(bgCtx, UpdateFilter{Statuses: []string{StatusDetected}, Visible: true}, t0)
	if len(all) != 1 || len(vis) != 0 {
		t.Fatalf("pending host updates must be hidden: all=%d visible=%d", len(all), len(vis))
	}
	s.ApproveHost(bgCtx, c.HostID, owner, t0)
	vis, _ = s.Updates(bgCtx, UpdateFilter{Statuses: []string{StatusDetected}, Visible: true, Unnotified: true}, t0)
	if len(vis) != 1 || vis[0].ContainerName != "web" || vis[0].StackName != "app" {
		t.Fatalf("visible = %+v", vis)
	}
	s.MarkNotified(bgCtx, []string{vis[0].ID}, t0)
	if un, _ := s.Updates(bgCtx, UpdateFilter{Unnotified: true}, t0); len(un) != 0 {
		t.Fatal("notified update still unnotified")
	}
}

func TestSnoozes(t *testing.T) {
	s := openTest(t)
	c := seedContainer(t, s)
	u := detect(t, s, c, "sha256:bbb", t0).Update

	ids, err := s.SnoozeUpdates(bgCtx, []string{u.ID}, t0.Add(24*time.Hour), owner, t0)
	if err != nil || len(ids) != 1 {
		t.Fatalf("snooze: %v %v", ids, err)
	}
	got, _ := s.GetUpdate(bgCtx, u.ID, t0.Add(time.Hour))
	if !got.Snoozed() || !got.SnoozeUntil.Equal(t0.Add(24*time.Hour)) {
		t.Fatalf("snoozed update: %+v", got)
	}
	if got, _ := s.GetUpdate(bgCtx, u.ID, t0.Add(25*time.Hour)); got.Snoozed() {
		t.Fatal("expired snooze still active")
	}

	exp, _ := s.TakeExpiredSnoozes(bgCtx, t0.Add(25*time.Hour))
	if len(exp) != 1 || exp[0].Update.ID != u.ID {
		t.Fatalf("expired = %+v", exp)
	}
	if again, _ := s.TakeExpiredSnoozes(bgCtx, t0.Add(26*time.Hour)); len(again) != 0 {
		t.Fatal("expiry must be announced once")
	}

	s.SnoozeUpdates(bgCtx, []string{u.ID}, time.Time{}, owner, t0.Add(30*time.Hour))
	if got, _ := s.GetUpdate(bgCtx, u.ID, t0.Add(9999*time.Hour)); !got.Snoozed() {
		t.Fatal("indefinite snooze must stay active")
	}
	if ok, _ := s.Unsnooze(bgCtx, u.ID, owner, t0.Add(31*time.Hour)); !ok {
		t.Fatal("unsnooze found nothing")
	}
	if ok, _ := s.Unsnooze(bgCtx, u.ID, owner, t0.Add(31*time.Hour)); ok {
		t.Fatal("second unsnooze should find nothing")
	}
	s.SnoozeUpdates(bgCtx, []string{u.ID}, time.Time{}, owner, t0.Add(32*time.Hour))
	if n, _ := s.UnsnoozeAll(bgCtx, owner, t0.Add(33*time.Hour)); n != 1 {
		t.Fatalf("unsnooze-all removed %d, want 1", n)
	}
	if auditCount(t, s, "snooze.created", u.ID) != 3 || auditCount(t, s, "snooze.removed", u.ID) != 2 {
		t.Fatal("snooze audits missing")
	}
	// Only detected updates can be snoozed.
	mustExec(t, s, `UPDATE updates SET status = 'applied' WHERE id = ?`, u.ID)
	if ids, _ := s.SnoozeUpdates(bgCtx, []string{u.ID}, time.Time{}, owner, t0); len(ids) != 0 {
		t.Fatal("applied update was snoozed")
	}
}

func TestPolicies(t *testing.T) {
	s := openTest(t)
	c := seedContainer(t, s)
	auto, any := domain.ModeAutomatic, domain.TrackAny
	if err := s.SetPolicy(bgCtx, PolicyKey{domain.LevelStack, c.StackID}, domain.PolicyFields{Mode: &auto}, owner, t0); err != nil {
		t.Fatal(err)
	}
	if err := s.SetPolicy(bgCtx, PolicyKey{domain.LevelHost, c.HostID}, domain.PolicyFields{TagTrack: &any}, owner, t0); err != nil {
		t.Fatal(err)
	}
	all, _ := s.Policies(bgCtx)
	e := domain.ResolvePolicy(Chain(all, c.HostID, c.StackID, c.GroupID, c.ID), false)
	if e.Mode != auto || e.Source[domain.FieldMode] != domain.LevelStack || e.TagTrack != any || e.Source[domain.FieldVerifyTimeout] != domain.LevelGlobal {
		t.Fatalf("effective = %+v", e)
	}
	if auditCount(t, s, "policy.set", c.StackID) != 1 {
		t.Fatal("policy.set audit missing")
	}

	if ok, _ := s.ClearPolicy(bgCtx, PolicyKey{domain.LevelStack, c.StackID}, "", owner, t0); !ok {
		t.Fatal("clear found nothing")
	}
	all, _ = s.Policies(bgCtx)
	if e := domain.ResolvePolicy(Chain(all, c.HostID, c.StackID, c.GroupID, c.ID), false); e.Mode != domain.ModeManual || e.Source[domain.FieldMode] != domain.LevelGlobal {
		t.Fatalf("after clear: %+v", e)
	}
	if ok, _ := s.ClearPolicy(bgCtx, PolicyKey{domain.LevelContainer, c.ID}, "", owner, t0); ok {
		t.Fatal("clearing a level with no row should report false")
	}

	// Global clear restores the built-in default rather than leaving it unset.
	s.SetPolicy(bgCtx, PolicyKey{Level: domain.LevelGlobal}, domain.PolicyFields{Mode: &auto}, owner, t0)
	s.ClearPolicy(bgCtx, PolicyKey{Level: domain.LevelGlobal}, domain.FieldMode, owner, t0)
	all, _ = s.Policies(bgCtx)
	if g := all[PolicyKey{Level: domain.LevelGlobal}]; g.Mode == nil || *g.Mode != domain.ModeManual {
		t.Fatalf("global mode after clear = %v", g.Mode)
	}
}

func TestMaintenanceAndOffers(t *testing.T) {
	s := openTest(t)
	if on, _ := s.Maintenance(bgCtx); on {
		t.Fatal("maintenance defaults to off")
	}
	if changed, _ := s.SetMaintenance(bgCtx, true, owner, t0); !changed {
		t.Fatal("enable should change state")
	}
	if changed, _ := s.SetMaintenance(bgCtx, true, owner, t0); changed {
		t.Fatal("enabling twice should not change state")
	}
	if on, _ := s.Maintenance(bgCtx); !on {
		t.Fatal("maintenance not on")
	}
	if auditCount(t, s, "maintenance.enabled", "maintenance") != 1 {
		t.Fatal("maintenance audit missing")
	}

	id, err := s.CreateOffer(bgCtx, "stack", "s1", []OfferItem{{UpdateID: "u1", TargetDigest: "sha256:x"}}, "notification", t0)
	if err != nil {
		t.Fatal(err)
	}
	o, err := s.GetOffer(bgCtx, id)
	if err != nil || len(o.Items) != 1 || o.Items[0].TargetDigest != "sha256:x" {
		t.Fatalf("offer = %+v, %v", o, err)
	}
}

func TestSupersedeMissingStacks(t *testing.T) {
	s := openTest(t)
	c := seedContainer(t, s)
	u := detect(t, s, c, "sha256:bbb", t0).Update
	mustInventory(t, s, agentA) // the stack disappears at t0
	if n, _ := s.SupersedeMissingStacks(bgCtx, t0.Add(-time.Hour), t0); n != 0 {
		t.Fatal("inside the grace period nothing is superseded")
	}
	if n, _ := s.SupersedeMissingStacks(bgCtx, t0.Add(time.Hour), t0.Add(25*time.Hour)); n != 1 || status(t, s, u.ID) != StatusSuperseded {
		t.Fatalf("after the grace period: n=%d status=%s", n, status(t, s, u.ID))
	}
}

func TestTagChangeStored(t *testing.T) {
	s := openTest(t)
	poll(t, s, agentA, "a", t0)
	st := stackOf("compose:app", compose, "web")
	st.Containers[0].TagChange = &protocol.TagChange{Supported: true, Source: "env_file", Variable: "WEB_TAG"}
	mustInventory(t, s, agentA, st)
	cs, _ := s.Containers(bgCtx, ContainerFilter{})
	if tc := cs[0].TagChange; tc == nil || tc.Variable != "WEB_TAG" {
		t.Fatalf("tag change = %+v", tc)
	}
	if cs[0].HostApproval != "pending" || cs[0].StackPresence != "present" {
		t.Fatalf("joined host/stack facts: %+v", cs[0])
	}
}

func TestSupersedeCurrentClearsOnlyUnapprovedUpdates(t *testing.T) {
	s := openTest(t)
	c := seedContainer(t, s)
	approved := detect(t, s, c, "sha256:ccc", t0)
	mustExec(t, s, `UPDATE updates SET status = 'approved' WHERE id = ?`, approved.Update.ID)
	open := detect(t, s, c, "sha256:bbb", t0.Add(time.Hour))

	n, err := s.SupersedeCurrent(bgCtx, c.ID, t0.Add(2*time.Hour))
	if err != nil || n != 1 || status(t, s, open.Update.ID) != StatusSuperseded {
		t.Fatalf("supersede current: n=%d err=%v status=%s", n, err, status(t, s, open.Update.ID))
	}
	if status(t, s, approved.Update.ID) != StatusApproved {
		t.Fatal("an approved update must not be cleared")
	}
	// A superseded update the container is still behind is offered again.
	if re := detect(t, s, c, "sha256:bbb", t0.Add(3*time.Hour)); !re.Reopened {
		t.Fatalf("a still-pending update must reopen: %+v", re)
	}
}
