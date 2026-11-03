package store

import (
	"errors"
	"testing"
	"time"

	"github.com/DragonDrew/ddum/internal/bot/domain"
	"github.com/DragonDrew/ddum/internal/protocol"
)

func stackByFP(t *testing.T, s *Store, fp string) Stack {
	t.Helper()
	sts, _ := s.Stacks(bgCtx, StackFilter{IncludeMissing: true})
	for _, st := range sts {
		if st.Fingerprint == fp {
			return st
		}
	}
	t.Fatalf("no stack %s in %+v", fp, sts)
	return Stack{}
}

func inv(t *testing.T, s *Store, at time.Time, stacks ...protocol.Stack) {
	t.Helper()
	if _, err := s.ApplyInventory(bgCtx, agentA, protocol.Inventory{InventoryHash: at.String(), Stacks: stacks}, at); err != nil {
		t.Fatal(err)
	}
}

// TestMergeRenamedProject: a renamed Compose project appears as a new stack;
// merging keeps the old UUID, its policy, group, and update history.
func TestMergeRenamedProject(t *testing.T) {
	s := openTest(t)
	poll(t, s, agentA, "h", t0)
	inv(t, s, t0, stackOf("compose:old", compose, "web", "db"))
	old := stackByFP(t, s, "compose:old")
	auto := domain.ModeAutomatic
	s.SetPolicy(bgCtx, PolicyKey{domain.LevelStack, old.ID}, domain.PolicyFields{Mode: &auto}, owner, t0)
	cs, _ := s.Containers(bgCtx, ContainerFilter{StackID: old.ID})
	var web Container
	for _, c := range cs {
		if c.IdentityKey == "web" {
			web = c
		}
	}
	g, _ := s.CreateGroup(bgCtx, "g", owner, t0)
	s.AddMember(bgCtx, g.ID, web.ID, owner, t0)
	u := detectFor(t, s, web, "b")

	// The project is renamed: the old stack goes missing, a new one appears.
	inv(t, s, t0.Add(time.Hour), stackOf("compose:new", compose, "web", "db"))
	newer := stackByFP(t, s, "compose:new")
	if newer.ID == old.ID || stackByFP(t, s, "compose:old").Presence != "missing" {
		t.Fatal("setup: the rename should look like a new stack")
	}
	res, err := s.MergeStacks(bgCtx, newer.ID, old.ID, owner, t0.Add(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if res.Survivor.ID != old.ID || res.Survivor.Fingerprint != "compose:new" || res.Survivor.Presence != "present" || res.Matched != 2 {
		t.Fatalf("merge result = %+v", res)
	}
	all, _ := s.Policies(bgCtx)
	if p := all[PolicyKey{domain.LevelStack, old.ID}]; p.Mode == nil || *p.Mode != auto {
		t.Fatal("the survivor keeps its policy")
	}
	if got, _ := s.Container(bgCtx, web.ID); got.GroupID != g.ID || got.Presence != "present" {
		t.Fatalf("the survivor's container keeps its group and is present: %+v", got)
	}
	if got, _ := s.GetUpdate(bgCtx, u.ID, t0); got.ContainerID != web.ID || got.StackID != old.ID {
		t.Fatal("update history stays with the surviving UUIDs")
	}
	// The next inventory of compose:new lands on the survivor.
	inv(t, s, t0.Add(3*time.Hour), stackOf("compose:new", compose, "web", "db"))
	if st := stackByFP(t, s, "compose:new"); st.ID != old.ID {
		t.Fatalf("rediscovery created a new stack: %s", st.ID)
	}
	if cs, _ := s.Containers(bgCtx, ContainerFilter{StackID: old.ID}); len(cs) != 2 {
		t.Fatalf("containers after rediscovery = %d, want 2", len(cs))
	}
	if auditCount(t, s, "stack.merged", old.ID) != 1 {
		t.Fatal("stack.merged audit missing")
	}
}

func detectFor(t *testing.T, s *Store, c Container, d string) Update {
	t.Helper()
	r, err := s.RecordDetection(bgCtx, Detection{ContainerID: c.ID, StackID: c.StackID, HostID: c.HostID, Kind: "digest",
		CurrentRef: "nginx:1.27", TargetRef: "docker.io/library/nginx:1.27@" + digest(d), TargetDigest: digest(d)}, t0)
	if err != nil {
		t.Fatal(err)
	}
	return r.Update
}

// TestMergeBackAndForth checks that merging back and forth
// never violates UNIQUE (host_id, fingerprint).
func TestMergeBackAndForth(t *testing.T) {
	s := openTest(t)
	poll(t, s, agentA, "h", t0)
	inv(t, s, t0, stackOf("compose:a", compose, "web"))
	survivor := stackByFP(t, s, "compose:a").ID
	at := t0
	for i, fp := range []string{"compose:b", "compose:a", "compose:b", "compose:a"} {
		at = at.Add(time.Hour)
		inv(t, s, at, stackOf(fp, compose, "web")) // the project is renamed again
		newer := stackByFP(t, s, fp)
		if newer.ID == survivor {
			t.Fatalf("round %d: the rename should first look like a new stack", i)
		}
		res, err := s.MergeStacks(bgCtx, survivor, newer.ID, owner, at)
		if err != nil {
			t.Fatalf("round %d: %v", i, err)
		}
		if res.Survivor.ID != survivor || res.Survivor.Fingerprint != fp {
			t.Fatalf("round %d: %+v", i, res)
		}
	}
	var dupes int
	s.Reader().QueryRow(`SELECT count(*) FROM (SELECT host_id, fingerprint FROM stacks GROUP BY 1, 2 HAVING count(*) > 1)`).Scan(&dupes)
	var retired int
	s.Reader().QueryRow(`SELECT count(*) FROM stacks WHERE retired_at IS NOT NULL AND fingerprint LIKE '%#retired:%'`).Scan(&retired)
	if dupes != 0 || retired != 4 {
		t.Fatalf("duplicates = %d, retired rows = %d (want 0 and 4)", dupes, retired)
	}
	// A retired stack cannot be merged again.
	var oneRetired string
	s.Reader().QueryRow(`SELECT id FROM stacks WHERE retired_at IS NOT NULL LIMIT 1`).Scan(&oneRetired)
	if _, err := s.MergeStacks(bgCtx, survivor, oneRetired, owner, at); !errors.Is(err, ErrStackRetired) {
		t.Fatalf("merging a retired stack: %v", err)
	}
	if _, err := s.MergeStacks(bgCtx, survivor, survivor, owner, at); !errors.Is(err, ErrSameStack) {
		t.Fatalf("self-merge: %v", err)
	}
}

// TestMergeAcrossHosts: a workload moved to another host keeps its UUID.
func TestMergeAcrossHosts(t *testing.T) {
	s := openTest(t)
	poll(t, s, agentA, "h1", t0)
	poll(t, s, agentB, "h2", t0)
	inv(t, s, t0, stackOf("compose:app", compose, "web"))
	old := stackByFP(t, s, "compose:app")
	inv(t, s, t0.Add(time.Hour)) // gone from host 1
	if _, err := s.ApplyInventory(bgCtx, agentB, protocol.Inventory{InventoryHash: "b",
		Stacks: []protocol.Stack{stackOf("compose:app", compose, "web")}}, t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	h2, _ := s.HostByAgent(bgCtx, agentB)
	moved, _ := s.Stacks(bgCtx, StackFilter{HostID: h2.ID})
	res, err := s.MergeStacks(bgCtx, old.ID, moved[0].ID, owner, t0.Add(2*time.Hour))
	if err != nil || res.Survivor.ID != old.ID || res.Survivor.HostID != h2.ID {
		t.Fatalf("host move: %+v %v", res, err)
	}
}

// TestPortainerToComposeKeepsIdentity checks that a stack moved
// from Portainer to Compose keeps its UUID, policy, group, and history.
func TestPortainerToComposeKeepsIdentity(t *testing.T) {
	s := openTest(t)
	poll(t, s, agentA, "h", t0)
	inv(t, s, t0, stackOf("compose:immich", protocol.ProviderPortainer, "server"))
	st := stackByFP(t, s, "compose:immich")
	cs, _ := s.Containers(bgCtx, ContainerFilter{StackID: st.ID})
	auto := domain.ModeAutomatic
	s.SetPolicy(bgCtx, PolicyKey{domain.LevelStack, st.ID}, domain.PolicyFields{Mode: &auto}, owner, t0)
	g, _ := s.CreateGroup(bgCtx, "photos", owner, t0)
	s.AddMember(bgCtx, g.ID, cs[0].ID, owner, t0)
	u := detectFor(t, s, cs[0], "b")

	inv(t, s, t0.Add(time.Hour), stackOf("compose:immich", compose, "server"))
	after := stackByFP(t, s, "compose:immich")
	c, _ := s.Container(bgCtx, cs[0].ID)
	all, _ := s.Policies(bgCtx)
	upd, _ := s.GetUpdate(bgCtx, u.ID, t0)
	if after.ID != st.ID || after.Provider != compose || c.GroupID != g.ID || all[PolicyKey{domain.LevelStack, st.ID}].Mode == nil ||
		upd.StackID != st.ID {
		t.Fatalf("identity lost: stack=%+v container=%+v update=%+v", after, c, upd)
	}
	if auditCount(t, s, "stack.provider_changed", st.ID) != 1 {
		t.Fatal("stack.provider_changed audit missing")
	}
}

// TestMergeSuggestedForRename: a new stack sharing most services with one
// that just went missing is suggested for merging; unrelated ones are not.
func TestMergeSuggestedForRename(t *testing.T) {
	s := openTest(t)
	poll(t, s, agentA, "h", t0)
	inv(t, s, t0, stackOf("compose:old", compose, "web", "db", "cache", "worker", "cron"), stackOf("compose:other", compose, "x"))
	old := stackByFP(t, s, "compose:old")

	res, err := s.ApplyInventory(bgCtx, agentA, protocol.Inventory{InventoryHash: "2", Stacks: []protocol.Stack{
		stackOf("compose:new", compose, "web", "db", "cache", "worker", "proxy"), // 4 of 5 shared
		stackOf("compose:unrelated", compose, "a", "b"),
		stackOf("compose:other", compose, "x"),
	}}, t0.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	newer := stackByFP(t, s, "compose:new")
	want := []MergeSuggestion{{NewStackID: newer.ID, MissingStackID: old.ID, Matched: 4, Total: 5}}
	if len(res.MergeSuggestions) != 1 || res.MergeSuggestions[0] != want[0] {
		t.Fatalf("suggestions = %+v, want %+v", res.MergeSuggestions, want)
	}

	// The same snapshot again creates nothing, so suggests nothing.
	res, _ = s.ApplyInventory(bgCtx, agentA, protocol.Inventory{InventoryHash: "3", Stacks: []protocol.Stack{
		stackOf("compose:new", compose, "web", "db", "cache", "worker", "proxy"),
		stackOf("compose:unrelated", compose, "a", "b"),
		stackOf("compose:other", compose, "x"),
	}}, t0.Add(2*time.Hour))
	if len(res.MergeSuggestions) != 0 {
		t.Fatalf("repeated suggestions: %+v", res.MergeSuggestions)
	}
}

func TestSharesServicesThreshold(t *testing.T) {
	if n, ok := domain.SharesServices([]string{"a", "b", "c", "d"}, []string{"a", "b", "c"}, 0.8); ok || n != 3 {
		t.Errorf("3 of 4 = %d %v, want below 0.8", n, ok)
	}
	if _, ok := domain.SharesServices(nil, []string{"a"}, 0.8); ok {
		t.Error("an empty stack matches nothing")
	}
}
