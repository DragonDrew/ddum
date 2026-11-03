package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/DragonDrew/ddum/internal/protocol"
)

var (
	t0      = time.Date(2026, 10, 1, 2, 0, 0, 0, time.UTC)
	agentA  = "0195f2c4-0000-7000-8000-00000000000a"
	agentB  = "0195f2c4-0000-7000-8000-00000000000b"
	owner   = Actor{Kind: "user", DiscordID: "100000000000000010", Name: "owner"}
	amd64   = protocol.DockerInfo{EngineVersion: "29.6.0", APIVersion: "1.55", OS: "linux", Arch: "amd64"}
	arm32   = protocol.DockerInfo{EngineVersion: "29.6.0", OS: "linux", Arch: "arm"}
	bgCtx   = context.Background()
	compose = protocol.ProviderCompose
)

func poll(t *testing.T, s *Store, agent, name string, at time.Time) PollResult {
	t.Helper()
	res, err := s.RecordPoll(bgCtx, Poll{AgentID: agent, AgentVersion: "dev", ProtocolVersion: 1,
		Capabilities: []string{"provider:compose"}, PollInterval: 30, HostName: name, Docker: amd64, At: at})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func auditCount(t *testing.T, s *Store, action, target string) int {
	t.Helper()
	var n int
	if err := s.Reader().QueryRow(`SELECT count(*) FROM audit_events WHERE action = ? AND target_id = ?`, action, target).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestRegistrationCreatesPendingHost(t *testing.T) {
	s := openTest(t)
	r := poll(t, s, agentA, "host-3", t0)
	if !r.Registered || r.Host.Approval != "pending" || r.Host.Connectivity != "online" || r.Host.DisplayName != "host-3" {
		t.Fatalf("first poll: %+v", r)
	}
	if r.Host.Docker.Arch != "amd64" || r.Host.PollInterval != 30 {
		t.Fatalf("agent details not stored: %+v", r.Host)
	}
	if auditCount(t, s, "host.registered", r.Host.ID) != 1 {
		t.Fatal("host.registered audit missing")
	}
	again := poll(t, s, agentA, "host-3", t0.Add(30*time.Second))
	if again.Registered || again.Host.ID != r.Host.ID {
		t.Fatalf("second poll re-registered: %+v", again)
	}
	if !again.Host.LastSeenAt.Equal(t0.Add(30 * time.Second)) {
		t.Fatalf("last_seen_at = %s", again.Host.LastSeenAt)
	}

	other := poll(t, s, agentB, "host-3", t0)
	if !other.Registered || !other.Collision || other.Host.ID == r.Host.ID {
		t.Fatalf("duplicate name should register a separate host and flag the collision: %+v", other)
	}
}

func TestApproveHost(t *testing.T) {
	s := openTest(t)
	h := poll(t, s, agentA, "a", t0).Host
	got, already, err := s.ApproveHost(bgCtx, h.ID, owner, t0)
	if err != nil || already || got.Approval != "approved" || got.ApprovedBy != owner.DiscordID {
		t.Fatalf("approve: %+v already=%v err=%v", got, already, err)
	}
	if _, already, _ := s.ApproveHost(bgCtx, h.ID, owner, t0); !already {
		t.Fatal("second approve should report alreadyApproved")
	}
	if auditCount(t, s, "host.approved", h.ID) != 1 {
		t.Fatal("want exactly one host.approved audit")
	}

	res, _ := s.RecordPoll(bgCtx, Poll{AgentID: agentB, HostName: "pi", Docker: arm32, At: t0, PollInterval: 30})
	if _, _, err := s.ApproveHost(bgCtx, res.Host.ID, owner, t0); !errors.Is(err, ErrUnsupportedArch) {
		t.Fatalf("arm approve err = %v, want ErrUnsupportedArch", err)
	}
}

func TestRemovedHostIsRefused(t *testing.T) {
	s := openTest(t)
	h := poll(t, s, agentA, "a", t0).Host
	if _, err := s.RemoveHost(bgCtx, h.ID, "host.rejected", owner, t0); err != nil {
		t.Fatal(err)
	}
	if r := poll(t, s, agentA, "a", t0.Add(time.Minute)); !r.Removed || r.Registered {
		t.Fatalf("poll after reject: %+v", r)
	}
	if _, err := s.ApplyInventory(bgCtx, agentA, protocol.Inventory{}, t0); !errors.Is(err, ErrHostRemoved) {
		t.Fatalf("inventory after reject err = %v", err)
	}
	if _, _, err := s.ApproveHost(bgCtx, h.ID, owner, t0); !errors.Is(err, ErrHostRemoved) {
		t.Fatalf("approving a removed host err = %v", err)
	}
	if auditCount(t, s, "host.rejected", h.ID) != 1 {
		t.Fatal("host.rejected audit missing")
	}
}

func TestReassignAgent(t *testing.T) {
	s := openTest(t)
	h := poll(t, s, agentA, "nas", t0).Host
	s.ApproveHost(bgCtx, h.ID, owner, t0)
	mustInventory(t, s, agentA, stackOf("compose:app", compose, "web"))

	// The replacement agent registers its own pending host first.
	pending := poll(t, s, agentB, "nas", t0.Add(time.Minute)).Host
	res, err := s.ReassignAgent(bgCtx, h.ID, agentB, owner, t0.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if res.Host.ID != h.ID || res.Host.AgentID != agentB || res.Host.Approval != "approved" || res.RetiredHost != pending.ID {
		t.Fatalf("reassign: %+v", res)
	}
	if r := poll(t, s, agentA, "nas", t0.Add(3*time.Minute)); !r.Removed {
		t.Fatal("old agent must be refused after reassignment")
	}
	r := poll(t, s, agentB, "nas", t0.Add(3*time.Minute))
	if r.Removed || r.Host.ID != h.ID {
		t.Fatalf("new agent should now poll as the original host: %+v", r)
	}
	// The new agent's inventory lands on the original host and stack UUIDs.
	before, _ := s.Stacks(bgCtx, StackFilter{HostID: h.ID})
	mustInventory(t, s, agentB, stackOf("compose:app", compose, "web"))
	after, _ := s.Stacks(bgCtx, StackFilter{HostID: h.ID})
	if len(after) != 1 || after[0].ID != before[0].ID {
		t.Fatalf("stack UUID changed across reassignment: %+v → %+v", before, after)
	}
	if auditCount(t, s, "host.agent_reassigned", h.ID) != 1 {
		t.Fatal("reassign audit missing")
	}

	// An agent that has never polled can be pre-assigned.
	const agentC = "0195f2c4-0000-7000-8000-00000000000c"
	if _, err := s.ReassignAgent(bgCtx, h.ID, agentC, owner, t0); err != nil {
		t.Fatal(err)
	}
	if r := poll(t, s, agentC, "nas", t0.Add(4*time.Minute)); r.Registered || r.Host.ID != h.ID {
		t.Fatalf("pre-assigned agent should not register a new host: %+v", r)
	}

	// An agent bound to another approved host is refused.
	d := poll(t, s, "0195f2c4-0000-7000-8000-00000000000d", "d", t0).Host
	s.ApproveHost(bgCtx, d.ID, owner, t0)
	if _, err := s.ReassignAgent(bgCtx, h.ID, d.AgentID, owner, t0); !errors.Is(err, ErrAgentInUse) {
		t.Fatalf("err = %v, want ErrAgentInUse", err)
	}
}

func TestMarkOfflineHosts(t *testing.T) {
	s := openTest(t)
	h := poll(t, s, agentA, "a", t0).Host
	if got, _ := s.MarkOfflineHosts(bgCtx, t0.Add(90*time.Second)); len(got) != 0 {
		t.Fatalf("offline too early: %+v", got)
	}
	got, err := s.MarkOfflineHosts(bgCtx, t0.Add(91*time.Second))
	if err != nil || len(got) != 1 || got[0].ID != h.ID {
		t.Fatalf("offline: %+v err=%v", got, err)
	}
	if again, _ := s.MarkOfflineHosts(bgCtx, t0.Add(200*time.Second)); len(again) != 0 {
		t.Fatal("an offline host must only be reported once")
	}
	if r := poll(t, s, agentA, "a", t0.Add(300*time.Second)); !r.CameOnline || r.Host.Connectivity != "online" {
		t.Fatalf("return: %+v", r)
	}
}
