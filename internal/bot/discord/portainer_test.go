package discord

import (
	"context"
	"strings"
	"testing"

	"github.com/DragonDrew/ddum/internal/bot/store"
	"github.com/DragonDrew/ddum/internal/protocol"
)

func TestGitStackTagChangeHeldWithReleaseLink(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	res, _ := e.db.RecordPoll(ctx, store.Poll{AgentID: agentA, HostName: "host-3", PollInterval: 15, Docker: protocol.DockerInfo{Arch: "amd64"},
		Capabilities: []string{protocol.CapProviderPortainer, protocol.CapActionDeploy, protocol.CapTagChangeEdit}, At: e.now})
	e.db.ApproveHost(ctx, res.Host.ID, store.Actor{Kind: "user"}, e.now)
	e.db.ApplyInventory(ctx, agentA, protocol.Inventory{InventoryHash: "x", Stacks: []protocol.Stack{{
		Fingerprint: "compose:gitapp", DisplayName: "gitapp", Provider: protocol.ProviderPortainer,
		ProviderRef: []byte(`{"portainer_stack_id":9,"portainer_stack_type":"git"}`), Deploy: protocol.Support{Supported: true},
		Containers: []protocol.Container{{IdentityKey: "web", ImageRef: "ghcr.io/x/web:1.0.0", State: "running",
			RepoDigests: []string{"ghcr.io/x/web@sha256:" + strings.Repeat("a", 64)},
			TagChange:   &protocol.TagChange{Reason: "git_managed"}}},
	}}}, e.now)
	cs, _ := e.db.Containers(ctx, store.ContainerFilter{})
	d := "sha256:" + strings.Repeat("b", 64)
	r, _ := e.db.RecordDetection(ctx, store.Detection{ContainerID: cs[0].ID, StackID: cs[0].StackID, HostID: cs[0].HostID, Kind: "version",
		CurrentRef: "ghcr.io/x/web:1.0.0", TargetRef: "ghcr.io/x/web:1.1.0@" + d, TargetDigest: d, TargetVersion: "1.1.0"}, e.now)
	e.db.SetReleaseURL(ctx, r.Update.ID, "https://github.com/x/web/releases/tag/1.1.0")
	e.gw.Sent = nil
	e.notify.NotifyNewUpdates(ctx)
	m := e.message(t, "Updates for gitapp")
	s := text(Response{Message: m.Message})
	if !strings.Contains(s, "⛔ **web** · manual update required (git_managed)") || !strings.Contains(s, "(https://github.com/x/web/releases/tag/1.1.0)") {
		t.Fatalf("git stack message:\n%s", s)
	}
	for id, disabled := range buttons(m.Message) {
		if strings.HasPrefix(id, "v1:"+actApprove) && !disabled {
			t.Fatal("Approve must be disabled: the tag change is held")
		}
	}
}

func TestStackMergeCommand(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.register(t, agentA, "host-3", "amd64")
	mk := func(fp string) protocol.Stack {
		return protocol.Stack{Fingerprint: fp, DisplayName: fp[8:], Provider: "compose",
			Containers: []protocol.Container{{IdentityKey: "web", ImageRef: "nginx:1.27"}}}
	}
	e.db.ApplyInventory(ctx, agentA, protocol.Inventory{InventoryHash: "1", Stacks: []protocol.Stack{mk("compose:old")}}, e.now)
	e.now = e.now.Add(1e9)
	e.db.ApplyInventory(ctx, agentA, protocol.Inventory{InventoryHash: "2", Stacks: []protocol.Stack{mk("compose:new")}}, e.now)
	stacks, _ := e.db.Stacks(ctx, store.StackFilter{IncludeMissing: true})
	ids := map[string]string{}
	for _, s := range stacks {
		ids[s.Fingerprint] = s.ID
	}
	ac := e.do(Interaction{Kind: KindAutocomplete, Command: "stack", Subcommand: "merge", Focused: optSource, Options: map[string]string{optSource: "old"}})
	if len(ac.Choices) != 1 || !strings.Contains(ac.Choices[0].Name, "(missing)") {
		t.Fatalf("merge autocomplete must offer missing stacks: %+v", ac.Choices)
	}
	r := e.do(cmd("stack", "merge", map[string]string{optSource: ids["compose:new"], optInto: ids["compose:old"]}))
	id := customID(actStackMerge, ids["compose:new"], ids["compose:old"])
	if _, ok := buttons(r.Message)[id]; !ok || len(id) > 100 || !strings.Contains(text(r), "cannot be undone") {
		t.Fatalf("merge must confirm: %+v", r)
	}
	r = e.do(click(id, "eph"))
	if !strings.Contains(text(r), "Merged · old") || !strings.Contains(text(r), "now `compose:new`") {
		t.Fatalf("merge: %q", text(r))
	}
	if r := e.do(click(id, "eph")); !strings.Contains(text(r), "already merged") {
		t.Fatalf("second merge: %q", text(r))
	}
}
