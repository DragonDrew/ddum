package domain

import (
	"reflect"
	"testing"
	"time"

	"github.com/DragonDrew/ddum/internal/protocol"
)

func TestOfflineAfter(t *testing.T) {
	cases := []struct {
		interval int
		want     time.Duration
	}{
		{3, 90 * time.Second},
		{30, 90 * time.Second},
		{31, 93 * time.Second},
		{300, 900 * time.Second},
	}
	for _, c := range cases {
		if got := OfflineAfter(c.interval); got != c.want {
			t.Errorf("OfflineAfter(%d) = %s, want %s", c.interval, got, c.want)
		}
	}
	seen := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	if IsOffline(seen, seen.Add(90*time.Second), 30) {
		t.Error("exactly at the threshold is still online")
	}
	if !IsOffline(seen, seen.Add(91*time.Second), 30) {
		t.Error("past the threshold is offline")
	}
}

func stack(fp, provider string, keys ...string) protocol.Stack {
	s := protocol.Stack{Fingerprint: fp, Provider: provider}
	for _, k := range keys {
		s.Containers = append(s.Containers, protocol.Container{IdentityKey: k})
	}
	return s
}

func TestPlanInventoryNewAndExisting(t *testing.T) {
	existing := []ExistingStack{{
		ID: "s1", Fingerprint: "compose:app", Provider: "compose", Presence: Present,
		Containers: []ExistingContainer{
			{ID: "c1", IdentityKey: "web", Presence: Present},
			{ID: "c2", IdentityKey: "db", Presence: Present},
			{ID: "c3", IdentityKey: "old", Presence: Missing},
		},
	}}
	plan := PlanInventory(existing, []protocol.Stack{
		stack("compose:app", "compose", "web", "cache"),
		stack("container:redis", "docker", "redis"),
	})
	if len(plan.Stacks) != 2 || len(plan.MissingStackIDs) != 0 {
		t.Fatalf("plan = %+v", plan)
	}
	app := plan.Stacks[0]
	if app.ExistingID != "s1" || app.ProviderChanged || app.Reappeared {
		t.Errorf("app change = %+v", app)
	}
	if app.Containers[0].ExistingID != "c1" || app.Containers[1].ExistingID != "" {
		t.Errorf("web must reuse c1 and cache must be new: %+v", app.Containers)
	}
	if !reflect.DeepEqual(app.MissingContainerIDs, []string{"c2"}) {
		t.Errorf("missing containers = %v, want [c2] (c3 is already missing)", app.MissingContainerIDs)
	}
	if plan.Stacks[1].ExistingID != "" {
		t.Errorf("redis should be new")
	}
}

func TestPlanInventoryMissingAndReappear(t *testing.T) {
	existing := []ExistingStack{
		{ID: "s1", Fingerprint: "compose:a", Provider: "compose", Presence: Present},
		{ID: "s2", Fingerprint: "compose:b", Provider: "compose", Presence: Missing,
			Containers: []ExistingContainer{{ID: "c9", IdentityKey: "x", Presence: Missing}}},
	}
	plan := PlanInventory(existing, []protocol.Stack{stack("compose:b", "compose", "x")})
	if !reflect.DeepEqual(plan.MissingStackIDs, []string{"s1"}) {
		t.Errorf("missing stacks = %v, want [s1]", plan.MissingStackIDs)
	}
	b := plan.Stacks[0]
	if !b.Reappeared || !b.Containers[0].Reappeared || b.Containers[0].ExistingID != "c9" {
		t.Errorf("reappeared stack/container not detected: %+v", b)
	}
}

func TestPlanInventoryProviderChangeKeepsIdentity(t *testing.T) {
	existing := []ExistingStack{{ID: "s1", Fingerprint: "compose:immich", Provider: "portainer", Presence: Present,
		Containers: []ExistingContainer{{ID: "c1", IdentityKey: "server", Presence: Present}}}}
	plan := PlanInventory(existing, []protocol.Stack{stack("compose:immich", "compose", "server")})
	ch := plan.Stacks[0]
	if ch.ExistingID != "s1" || !ch.ProviderChanged || ch.OldProvider != "portainer" {
		t.Fatalf("change = %+v", ch)
	}
	if ch.Containers[0].ExistingID != "c1" {
		t.Fatalf("container identity lost on provider change")
	}
}

func TestPlanInventoryEmptySnapshotIsIdempotent(t *testing.T) {
	existing := []ExistingStack{{ID: "s1", Fingerprint: "compose:a", Presence: Missing}}
	plan := PlanInventory(existing, nil)
	if len(plan.MissingStackIDs) != 0 || len(plan.Stacks) != 0 {
		t.Fatalf("an already-missing stack must not be marked again: %+v", plan)
	}
}
