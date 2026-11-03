package store

import (
	"testing"
	"time"

	"github.com/DragonDrew/ddum/internal/protocol"
)

func stackOf(fp, provider string, keys ...string) protocol.Stack {
	s := protocol.Stack{Fingerprint: fp, DisplayName: fp[len("compose:"):], Provider: provider,
		Deploy: protocol.Support{Supported: false, Reason: "x"}}
	for _, k := range keys {
		s.Containers = append(s.Containers, protocol.Container{
			IdentityKey: k, DockerID: "id-" + k, DockerName: "app-" + k + "-1",
			ImageRef: "nginx:1.27", RepoDigests: []string{"nginx@sha256:aaa"}, State: "running",
			DdumLabels: map[string]string{"ddum.ignore": "false"},
		})
	}
	return s
}

func mustInventory(t *testing.T, s *Store, agent string, stacks ...protocol.Stack) InventoryResult {
	t.Helper()
	res, err := s.ApplyInventory(bgCtx, agent, protocol.Inventory{
		ProtocolVersion: 1, AgentID: agent, InventoryHash: protocol.InventoryHash(nil, stacks), Stacks: stacks,
	}, t0)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func entityIDs(t *testing.T, s *Store, hostID string) (map[string]string, map[string]string) {
	t.Helper()
	stacks, err := s.Stacks(bgCtx, StackFilter{HostID: hostID, IncludeMissing: true})
	if err != nil {
		t.Fatal(err)
	}
	st := map[string]string{}
	for _, x := range stacks {
		st[x.Fingerprint] = x.ID
	}
	cs, err := s.Containers(bgCtx, ContainerFilter{HostID: hostID, IncludeMissing: true})
	if err != nil {
		t.Fatal(err)
	}
	ct := map[string]string{}
	for _, x := range cs {
		ct[x.IdentityKey] = x.ID
	}
	return st, ct
}

func TestInventoryKeepsUUIDs(t *testing.T) {
	s := openTest(t)
	h := poll(t, s, agentA, "a", t0).Host

	res := mustInventory(t, s, agentA, stackOf("compose:app", compose, "web", "db"))
	if res.StacksCreated != 1 || res.ContainersAdded != 2 {
		t.Fatalf("first inventory: %+v", res)
	}
	if res.Host.InventoryHash == "" || res.Host.LastInventoryAt.IsZero() {
		t.Fatalf("hash/time not stored: %+v", res.Host)
	}
	st1, ct1 := entityIDs(t, s, h.ID)

	// Recreate: new Docker IDs, same identity.
	recreated := stackOf("compose:app", compose, "web", "db")
	recreated.Containers[0].DockerID = "new-docker-id"
	mustInventory(t, s, agentA, recreated)
	st2, ct2 := entityIDs(t, s, h.ID)
	if st1["compose:app"] != st2["compose:app"] || ct1["web"] != ct2["web"] {
		t.Fatal("UUIDs changed across container recreation")
	}
	c, _ := s.Container(bgCtx, ct2["web"])
	if c.DockerID != "new-docker-id" || c.ImageRepository != "docker.io/library/nginx" {
		t.Fatalf("runtime fields not refreshed: %+v", c)
	}

	// Provider relabeling: Compose → Portainer keeps the UUID and is audited.
	mustInventory(t, s, agentA, stackOf("compose:app", protocol.ProviderPortainer, "web", "db"))
	st3, ct3 := entityIDs(t, s, h.ID)
	if st3["compose:app"] != st1["compose:app"] || ct3["db"] != ct1["db"] {
		t.Fatal("UUIDs changed across provider change")
	}
	if auditCount(t, s, "stack.provider_changed", st1["compose:app"]) != 1 {
		t.Fatal("stack.provider_changed audit missing")
	}
	// Applying the same snapshot again changes nothing (idempotent).
	mustInventory(t, s, agentA, stackOf("compose:app", protocol.ProviderPortainer, "web", "db"))
	if auditCount(t, s, "stack.provider_changed", st1["compose:app"]) != 1 {
		t.Fatal("re-applying an inventory must not re-audit")
	}
}

func TestInventoryMissingAndReappear(t *testing.T) {
	s := openTest(t)
	h := poll(t, s, agentA, "a", t0).Host
	mustInventory(t, s, agentA, stackOf("compose:app", compose, "web", "db"), stackOf("compose:other", compose, "x"))
	st, ct := entityIDs(t, s, h.ID)

	res := mustInventory(t, s, agentA, stackOf("compose:app", compose, "web"))
	if res.StacksMissing != 1 || res.ContainersMissed != 2 {
		t.Fatalf("missing: %+v", res)
	}
	visible, _ := s.Stacks(bgCtx, StackFilter{HostID: h.ID})
	if len(visible) != 1 || visible[0].Fingerprint != "compose:app" {
		t.Fatalf("missing stacks must be hidden from normal views: %+v", visible)
	}
	other, _ := s.Stack(bgCtx, st["compose:other"])
	if other.Presence != "missing" || other.MissingSince.IsZero() {
		t.Fatalf("other stack: %+v", other)
	}

	mustInventory(t, s, agentA, stackOf("compose:app", compose, "web", "db"), stackOf("compose:other", compose, "x"))
	st2, ct2 := entityIDs(t, s, h.ID)
	if st2["compose:other"] != st["compose:other"] || ct2["db"] != ct["db"] {
		t.Fatal("reappearing objects must keep their UUIDs")
	}
	other, _ = s.Stack(bgCtx, st["compose:other"])
	db, _ := s.Container(bgCtx, ct["db"])
	if other.Presence != "present" || !other.MissingSince.IsZero() || db.Presence != "present" {
		t.Fatalf("reappeared: stack=%+v container=%+v", other, db)
	}
}

// An agent that cannot read Docker sends no stacks with every provider in
// error. That records the error but marks nothing missing; a snapshot
// with a working provider and no stacks still does.
func TestInventoryDiscoveryFailure(t *testing.T) {
	s := openTest(t)
	h := poll(t, s, agentA, "a", t0).Host
	mustInventory(t, s, agentA, stackOf("compose:app", compose, "web"))

	failed := []protocol.ProviderStatus{
		{Type: protocol.ProviderDocker, Status: "error", Detail: "inspect x: No such container"},
		{Type: compose, Status: "error", Detail: "inspect x: No such container"},
	}
	res, err := s.ApplyInventory(bgCtx, agentA, protocol.Inventory{ProtocolVersion: 1, AgentID: agentA,
		InventoryHash: protocol.InventoryHash(failed, nil), Providers: failed}, t0.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if res.StacksMissing != 0 || res.ContainersMissed != 0 {
		t.Fatalf("a failed discovery marked objects missing: %+v", res)
	}
	if len(res.Host.Providers) != 2 || res.Host.Providers[0].Status != "error" || !res.Host.LastInventoryAt.Equal(t0.Add(time.Minute)) {
		t.Fatalf("host not updated: %+v", res.Host)
	}
	if visible, _ := s.Stacks(bgCtx, StackFilter{HostID: h.ID}); len(visible) != 1 {
		t.Fatalf("stack hidden after a failed discovery: %+v", visible)
	}

	ok := []protocol.ProviderStatus{{Type: protocol.ProviderDocker, Status: "ok"}, {Type: compose, Status: "error"}}
	res, err = s.ApplyInventory(bgCtx, agentA, protocol.Inventory{ProtocolVersion: 1, AgentID: agentA,
		InventoryHash: protocol.InventoryHash(ok, nil), Providers: ok}, t0.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if res.StacksMissing != 1 {
		t.Fatalf("an empty snapshot from a working provider must mark stacks missing: %+v", res)
	}
}

func TestInventoryKeepsDiscordNames(t *testing.T) {
	s := openTest(t)
	h := poll(t, s, agentA, "a", t0).Host
	mustInventory(t, s, agentA, stackOf("compose:app", compose, "web"))
	st, _ := entityIDs(t, s, h.ID)
	if _, err := s.RenameStack(bgCtx, st["compose:app"], "My App", owner, t0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RenameHost(bgCtx, h.ID, "NAS", owner, t0); err != nil {
		t.Fatal(err)
	}
	mustInventory(t, s, agentA, stackOf("compose:app", compose, "web"))
	poll(t, s, agentA, "a", t0.Add(time.Minute))
	got, _ := s.Stack(bgCtx, st["compose:app"])
	host, _ := s.Host(bgCtx, h.ID)
	if got.DisplayName != "My App" || host.DisplayName != "NAS" {
		t.Fatalf("names overwritten: stack=%q host=%q", got.DisplayName, host.DisplayName)
	}
	if auditCount(t, s, "stack.renamed", st["compose:app"]) != 1 || auditCount(t, s, "host.renamed", h.ID) != 1 {
		t.Fatal("rename audits missing")
	}
}

func TestInventoryPendingHostIsStored(t *testing.T) {
	s := openTest(t)
	h := poll(t, s, agentA, "a", t0).Host
	mustInventory(t, s, agentA, stackOf("compose:app", compose, "web"))
	if st, _ := s.Stacks(bgCtx, StackFilter{HostID: h.ID}); len(st) != 1 {
		t.Fatal("a pending host's inventory must be stored for /host show")
	}
}

func TestInventoryUnknownAgent(t *testing.T) {
	s := openTest(t)
	if _, err := s.ApplyInventory(bgCtx, agentA, protocol.Inventory{}, t0); err != ErrUnknownAgent {
		t.Fatalf("err = %v, want ErrUnknownAgent", err)
	}
}

func TestSettingsRoundTrip(t *testing.T) {
	s := openTest(t)
	var v struct{ On bool }
	if found, err := s.Setting(bgCtx, "maintenance", &v); found || err != nil {
		t.Fatalf("unset setting: found=%v err=%v", found, err)
	}
	s.SetSetting(bgCtx, "maintenance", struct{ On bool }{true}, t0)
	s.SetSetting(bgCtx, "maintenance", struct{ On bool }{false}, t0)
	if found, _ := s.Setting(bgCtx, "maintenance", &v); !found || v.On {
		t.Fatalf("setting = %+v found=%v", v, found)
	}
}
