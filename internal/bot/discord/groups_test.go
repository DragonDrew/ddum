package discord

import (
	"context"
	"strings"
	"testing"

	"github.com/DragonDrew/ddum/internal/bot/store"
	"github.com/DragonDrew/ddum/internal/protocol"
)

// groupHost: approved host with stacks "a" (app) and "b" (worker), both
// labelled ddum.group=pair, and "c" (lone), each with an update.
func (e *env) groupHost(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	res, _ := e.db.RecordPoll(ctx, store.Poll{AgentID: agentA, HostName: "host-3", PollInterval: 15,
		Capabilities: []string{protocol.CapActionDeploy, protocol.CapActionRollback, protocol.CapTagChangeEdit},
		Docker:       protocol.DockerInfo{Arch: "amd64"}, At: e.now})
	e.db.ApproveHost(ctx, res.Host.ID, store.Actor{Kind: "user"}, e.now)
	mk := func(name, key string, label bool) protocol.Stack {
		c := protocol.Container{IdentityKey: key, ImageRef: "nginx:1.27", State: "running",
			RepoDigests: []string{"nginx@sha256:" + strings.Repeat("a", 64)}, TagChange: &protocol.TagChange{Supported: true, Source: "image"}}
		if label {
			c.DdumLabels = map[string]string{"ddum.group": "pair"}
		}
		return protocol.Stack{Fingerprint: "compose:" + name, DisplayName: name, Provider: "compose",
			Deploy: protocol.Support{Supported: true}, Containers: []protocol.Container{c}}
	}
	e.db.ApplyInventory(ctx, agentA, protocol.Inventory{InventoryHash: "x", Stacks: []protocol.Stack{
		mk("a", "app", true), mk("b", "worker", true), mk("c", "lone", false)}}, e.now)
	cs, _ := e.db.Containers(ctx, store.ContainerFilter{})
	for _, c := range cs {
		d := "sha256:" + strings.Repeat("b", 64)
		e.db.RecordDetection(ctx, store.Detection{ContainerID: c.ID, StackID: c.StackID, HostID: c.HostID, Kind: "digest",
			CurrentRef: "nginx:1.27", CurrentDigest: "sha256:" + strings.Repeat("a", 64), TargetRef: "docker.io/library/nginx:1.27@" + d, TargetDigest: d}, e.now)
	}
	e.gw.Sent, e.gw.Edits = nil, nil
}

func (e *env) message(t *testing.T, contains string) SentMessage {
	t.Helper()
	for _, m := range e.gw.Sent {
		if strings.Contains(text(Response{Message: m.Message}), contains) {
			return m
		}
	}
	var all []string
	for _, m := range e.gw.Sent {
		all = append(all, text(Response{Message: m.Message}))
	}
	t.Fatalf("no message contains %q:\n%s", contains, strings.Join(all, "\n----\n"))
	return SentMessage{}
}

func TestGroupNotifiedAsOneScope(t *testing.T) {
	e := newEnv(t)
	e.groupHost(t)
	e.notify.NotifyNewUpdates(context.Background())
	if e.gw.SentCount() != 2 {
		t.Fatalf("messages = %d, want the group and stack c", e.gw.SentCount())
	}
	g := e.message(t, "Updates for group pair")
	s := text(Response{Message: g.Message})
	if !strings.Contains(s, "app (a)") || !strings.Contains(s, "worker (b)") || !strings.Contains(s, "new build") || !strings.Contains(s, "deploy and roll back together") {
		t.Fatalf("group message:\n%s", s)
	}
	for _, m := range e.gw.Sent {
		if strings.Contains(text(Response{Message: m.Message}), "Updates for a\n") {
			t.Fatal("group members must not get stack messages")
		}
	}
	r := e.do(click(e.approveID(t, g.Message), g.MessageID))
	if !strings.Contains(text(r), "Deploy queued · group pair") {
		t.Fatalf("group approve: %q", text(r))
	}
	ops, _ := e.db.Operations(context.Background(), store.OperationFilter{})
	if ops[0].ScopeType != "group" || len(ops[0].Payload.Targets) != 2 {
		t.Fatalf("operation = %+v", ops[0])
	}
}

func TestGroupConflictShownButManualApprovalWorks(t *testing.T) {
	e := newEnv(t)
	e.groupHost(t)
	cs, _ := e.db.Containers(context.Background(), store.ContainerFilter{})
	for _, c := range cs {
		if c.IdentityKey == "app" {
			e.do(cmd("policy", "set", map[string]string{optScope: "container", optTarget: c.ID, optMode: "automatic"}))
		}
	}
	e.notify.NotifyNewUpdates(context.Background())
	g := e.message(t, "Updates for group pair")
	s := text(Response{Message: g.Message})
	if !strings.Contains(s, "policy conflict") || !strings.Contains(s, "worker (manual)") ||
		!strings.Contains(s, "You can still approve the whole group") {
		t.Fatalf("conflict message:\n%s", s)
	}
	if r := e.do(click(e.approveID(t, g.Message), g.MessageID)); !strings.Contains(text(r), "Deploy queued") {
		t.Fatalf("manual approval of a conflicted group: %q", text(r))
	}
}

func TestGroupCommands(t *testing.T) {
	e := newEnv(t)
	e.groupHost(t)
	if l := text(e.do(cmd("group", "list", nil))); !strings.Contains(l, "**pair** · 2 members · from label") {
		t.Fatalf("list:\n%s", l)
	}
	if r := e.do(cmd("group", "create", map[string]string{optName: "web"})); !strings.Contains(text(r), "Group created · web") {
		t.Fatalf("create: %q", text(r))
	}
	groups, _ := e.db.Groups(context.Background())
	var web, pair store.Group
	for _, g := range groups {
		if g.DisplayName == "web" {
			web = g
		} else {
			pair = g
		}
	}
	cs, _ := e.db.Containers(context.Background(), store.ContainerFilter{})
	ids := map[string]string{}
	for _, c := range cs {
		ids[c.IdentityKey] = c.ID
	}
	if r := e.do(cmd("group", "add", map[string]string{optGroup: web.ID, optContainer: ids["app"]})); !strings.Contains(text(r), "already in group **pair**") {
		t.Fatalf("add to a second group: %q", text(r))
	}
	if r := e.do(cmd("group", "add", map[string]string{optGroup: web.ID, optContainer: ids["lone"]})); !strings.Contains(text(r), "**lone**") || !strings.Contains(text(r), "Added to web") {
		t.Fatalf("add: %q", text(r))
	}
	if r := e.do(cmd("group", "remove", map[string]string{optContainer: ids["worker"]})); !strings.Contains(text(r), "label will not add it back") {
		t.Fatalf("remove: %q", text(r))
	}
	show := text(e.do(cmd("group", "show", map[string]string{optGroup: pair.ID})))
	if !strings.Contains(show, "Members (1)") || !strings.Contains(show, "app") {
		t.Fatalf("show:\n%s", show)
	}
	r := e.do(cmd("group", "delete", map[string]string{optGroup: web.ID}))
	if _, ok := buttons(r.Message)[customID(actGroupDelete, web.ID)]; !ok {
		t.Fatalf("delete must confirm: %+v", r)
	}
	if r := e.do(click(customID(actGroupDelete, web.ID), "eph")); !strings.Contains(text(r), "Group deleted · web") {
		t.Fatalf("delete: %q", text(r))
	}
	if p := text(e.do(cmd("policy", "set", map[string]string{optScope: "group", optTarget: pair.ID, optMode: "automatic"}))); !strings.Contains(p, "mode: **automatic**\nset at: group") {
		t.Fatalf("group policy:\n%s", p)
	}
}

func TestCrossHostGroupApproveDisabled(t *testing.T) {
	e := newEnv(t)
	e.groupHost(t)
	ctx := context.Background()
	res, _ := e.db.RecordPoll(ctx, store.Poll{AgentID: agentB, HostName: "far", PollInterval: 15, Docker: protocol.DockerInfo{Arch: "amd64"},
		Capabilities: []string{protocol.CapActionDeploy}, At: e.now})
	e.db.ApproveHost(ctx, res.Host.ID, store.Actor{Kind: "user"}, e.now)
	e.db.ApplyInventory(ctx, agentB, protocol.Inventory{InventoryHash: "y", Stacks: []protocol.Stack{{Fingerprint: "compose:far",
		DisplayName: "far", Provider: "compose", Deploy: protocol.Support{Supported: true},
		Containers: []protocol.Container{{IdentityKey: "far", ImageRef: "nginx:1.27", DdumLabels: map[string]string{"ddum.group": "pair"},
			RepoDigests: []string{"nginx@sha256:" + strings.Repeat("a", 64)}}}}}}, e.now)
	cs, _ := e.db.Containers(ctx, store.ContainerFilter{HostID: res.Host.ID})
	d := "sha256:" + strings.Repeat("b", 64)
	e.db.RecordDetection(ctx, store.Detection{ContainerID: cs[0].ID, StackID: cs[0].StackID, HostID: res.Host.ID, Kind: "digest",
		CurrentRef: "nginx:1.27", TargetRef: "docker.io/library/nginx:1.27@" + d, TargetDigest: d}, e.now)
	e.gw.Sent = nil
	e.notify.NotifyNewUpdates(ctx)
	g := e.message(t, "Updates for group pair")
	if s := text(Response{Message: g.Message}); !strings.Contains(s, "group_spans_hosts") {
		t.Fatalf("cross-host group:\n%s", s)
	}
	for id, disabled := range buttons(g.Message) {
		if strings.HasPrefix(id, "v1:"+actApprove) && !disabled {
			t.Fatal("Approve must be disabled for a cross-host group")
		}
	}
}

// TestMergeSuggestionLeadsToConfirmation: a suggested rename posts a notice
// whose button opens the usual merge confirmation, which merges nothing yet.
func TestMergeSuggestionLeadsToConfirmation(t *testing.T) {
	e := newEnv(t)
	e.hostWithStacks(t, "old_app")
	ctx := context.Background()
	stack := func(name string) protocol.Stack {
		return protocol.Stack{Fingerprint: "compose:" + name, DisplayName: name, Provider: "compose",
			Containers: []protocol.Container{{IdentityKey: "web", ImageRef: "nginx:1.27", State: "running"}}}
	}
	res, err := e.db.ApplyInventory(ctx, agentA, protocol.Inventory{InventoryHash: "y", Stacks: []protocol.Stack{stack("new_app")}}, e.now)
	if err != nil || len(res.MergeSuggestions) != 1 {
		t.Fatalf("suggestions = %+v, %v", res.MergeSuggestions, err)
	}
	e.notify.MergeSuggested(ctx, res.MergeSuggestions)
	m := e.gw.Last()
	s := text(Response{Message: m.Message})
	if !strings.Contains(s, `Was old\_app renamed?`) || !strings.Contains(s, "shares 1 of 1 services") {
		t.Fatalf("suggestion:\n%s", s)
	}
	sg := res.MergeSuggestions[0]
	id := customID(actMergeAsk, sg.NewStackID, sg.MissingStackID)
	if _, ok := buttons(m.Message)[id]; !ok {
		t.Fatalf("Merge button missing: %v", buttons(m.Message))
	}
	r := e.do(click(id, m.MessageID))
	if r.Kind != RespUpdate || !strings.Contains(text(r), "This cannot be undone") {
		t.Fatalf("confirmation: %+v", r)
	}
	if _, ok := buttons(r.Message)[customID(actStackMerge, sg.NewStackID, sg.MissingStackID)]; !ok {
		t.Fatalf("confirm button missing: %v", buttons(r.Message))
	}
	if sts, _ := e.db.Stacks(ctx, store.StackFilter{IncludeMissing: true}); len(sts) != 2 {
		t.Fatalf("stacks = %d: the suggestion itself must not merge", len(sts))
	}
}
