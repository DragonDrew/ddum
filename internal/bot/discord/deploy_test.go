package discord

import (
	"context"
	"strings"
	"testing"

	"github.com/DragonDrew/ddum/internal/bot/store"
	"github.com/DragonDrew/ddum/internal/protocol"
)

// deployHost registers a deploy-capable approved host with stack "app":
// web (manual), proxy (protected); each gets a detected update.
func (e *env) deployHost(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	res, err := e.db.RecordPoll(ctx, store.Poll{AgentID: agentA, HostName: "host-3", PollInterval: 15,
		Capabilities: []string{protocol.CapActionDeploy, protocol.CapTagChangeEdit},
		Docker:       protocol.DockerInfo{EngineVersion: "29.6.0", OS: "linux", Arch: "amd64"}, At: e.now})
	if err != nil {
		t.Fatal(err)
	}
	e.db.ApproveHost(ctx, res.Host.ID, store.Actor{Kind: "user", DiscordID: ownerID}, e.now)
	st := protocol.Stack{Fingerprint: "compose:app", DisplayName: "app", Provider: "compose", Deploy: protocol.Support{Supported: true}}
	for _, n := range []string{"web", "proxy"} {
		st.Containers = append(st.Containers, protocol.Container{IdentityKey: n, ImageRef: "nginx:1.27", State: "running",
			RepoDigests: []string{"nginx@sha256:" + strings.Repeat("a", 64)}, Protected: n == "proxy",
			TagChange: &protocol.TagChange{Supported: true, Source: "image"}})
	}
	e.db.ApplyInventory(ctx, agentA, protocol.Inventory{InventoryHash: "x", Stacks: []protocol.Stack{st}}, e.now)
	e.gw.Sent, e.gw.Edits = nil, nil
	cs, _ := e.db.Containers(ctx, store.ContainerFilter{})
	for _, c := range cs {
		d := "sha256:" + strings.Repeat("8", 64)
		if _, err := e.db.RecordDetection(ctx, store.Detection{ContainerID: c.ID, StackID: c.StackID, HostID: c.HostID, Kind: "version",
			CurrentRef: "nginx:1.27", CurrentDigest: "sha256:" + strings.Repeat("a", 64),
			TargetRef: "docker.io/library/nginx:1.28@" + d, TargetDigest: d, TargetVersion: "1.28"}, e.now); err != nil {
			t.Fatal(err)
		}
	}
}

func (e *env) approveID(t *testing.T, m Message) string {
	t.Helper()
	for id, disabled := range buttons(m) {
		if strings.HasPrefix(id, "v1:"+actApprove+":") {
			if disabled {
				t.Fatalf("Approve is disabled: %s", text(Response{Message: m}))
			}
			return id
		}
	}
	t.Fatalf("no Approve button: %v", buttons(m))
	return ""
}

// TestApproveTwiceCreatesOneOperation checks, from the
// notification's button.
func TestApproveTwiceCreatesOneOperation(t *testing.T) {
	e := newEnv(t)
	e.deployHost(t)
	e.notify.NotifyNewUpdates(context.Background())
	notice := e.gw.Last()
	s := text(Response{Message: notice.Message})
	if !hasRow(s, "proxy", "1.27", "1.28", "Held") || !hasRow(s, "web", "1.27", "1.28", "Pending") || !strings.Contains(s, "Pending approval") {
		t.Fatalf("a protected member is held without holding the stack:\n%s", s)
	}
	id := e.approveID(t, notice.Message)

	r := e.do(click(id, notice.MessageID))
	if r.Kind != RespUpdate || !strings.Contains(text(r), "Deploy queued · app") ||
		!strings.Contains(text(r), "proxy: protected") {
		t.Fatalf("approve: %q", text(r))
	}
	if _, ok := buttons(r.Message)["v1:"+actOpCancel+":"]; ok {
		t.Fatal("cancel button must carry the operation ID")
	}
	second := e.do(Interaction{Kind: KindComponent, CustomID: id, MessageID: notice.MessageID, UserID: ownerID, GuildID: guild})
	if !strings.Contains(text(second), "Already approved by <@"+ownerID+">") {
		t.Fatalf("second click: %q", text(second))
	}
	ops, _ := e.db.Operations(context.Background(), store.OperationFilter{})
	if len(ops) != 1 {
		t.Fatalf("operations = %d, want 1", len(ops))
	}
	// The card became the operation's card: refreshing the stack must not
	// turn it back into the update list.
	if len(e.gw.Edits) != 0 {
		t.Fatalf("the operation card was overwritten: %s", text(Response{Message: e.gw.Edits[0].Message}))
	}
	if msgs, _ := e.db.DiscordMessages(context.Background(), entityOperation, ops[0].ID); len(msgs) != 1 || msgs[0].MessageID != notice.MessageID {
		t.Fatalf("the clicked card should be bound to the operation: %+v", msgs)
	}
}

func TestStaleOfferReturnsFreshOne(t *testing.T) {
	e := newEnv(t)
	e.deployHost(t)
	e.notify.NotifyNewUpdates(context.Background())
	notice := e.gw.Last()
	id := e.approveID(t, notice.Message)
	e.detect(t, "app", "1.29") // supersedes the offered 1.28

	r := e.do(click(id, notice.MessageID))
	if !strings.Contains(text(r), "nothing was approved") || !strings.Contains(text(r), "1.29") {
		t.Fatalf("stale click: %s", text(r))
	}
	if ops, _ := e.db.Operations(context.Background(), store.OperationFilter{}); len(ops) != 0 {
		t.Fatal("a stale offer created an operation")
	}
	fresh := e.approveID(t, r.Message)
	if fresh == id {
		t.Fatal("the reply must carry a new offer")
	}
	if r := e.do(click(fresh, "eph")); !strings.Contains(text(r), "Deploy queued") {
		t.Fatalf("fresh offer: %q", text(r))
	}
}

func TestProtectedCannotBeApproved(t *testing.T) {
	e := newEnv(t)
	e.deployHost(t)
	ups, _ := e.db.Updates(context.Background(), store.UpdateFilter{}, e.now)
	var proxy store.Update
	for _, u := range ups {
		if u.ContainerName == "proxy" {
			proxy = u
		}
	}
	r := e.do(cmd("update", "approve", map[string]string{optUpdate: proxy.ID}))
	r = e.do(click(e.approveID(t, r.Message), "eph"))
	if !strings.Contains(text(r), "Nothing here can be approved") || !strings.Contains(text(r), "protected") {
		t.Fatalf("protected approve: %q", text(r))
	}
	// Only protected members: Approve is disabled on the message.
	e.do(click(e.approveIDFor(t, "web"), "eph"))
	e.gw.Sent = nil
	v, _ := loadScope(context.Background(), e.db, proxy.StackID, e.now)
	m := scopeMessage(v, "offer")
	for id, disabled := range buttons(m) {
		if strings.HasPrefix(id, "v1:"+actApprove) && !disabled {
			t.Fatal("Approve must be disabled when only protected members are pending")
		}
	}
}

func (e *env) approveIDFor(t *testing.T, container string) string {
	t.Helper()
	ups, _ := e.db.Updates(context.Background(), store.UpdateFilter{}, e.now)
	for _, u := range ups {
		if u.ContainerName == container {
			r := e.do(cmd("update", "approve", map[string]string{optUpdate: u.ID}))
			return e.approveID(t, r.Message)
		}
	}
	t.Fatalf("no update for %s", container)
	return ""
}

func TestCancelHistoryAndResult(t *testing.T) {
	e := newEnv(t)
	e.deployHost(t)
	r := e.do(click(e.approveIDFor(t, "web"), "eph"))
	var cancelID string
	for id := range buttons(r.Message) {
		if strings.HasPrefix(id, "v1:"+actOpCancel+":") {
			cancelID = id
		}
	}
	if c := e.do(click(cancelID, "eph")); c.Kind != RespUpdate || !strings.Contains(text(c), "cancelled the deploy") || e.approveID(t, c.Message) == "" {
		t.Fatalf("cancel: %+v", c)
	}
	ops, _ := e.db.Operations(context.Background(), store.OperationFilter{})
	if ops[0].State != protocol.OpCancelled {
		t.Fatalf("state = %s", ops[0].State)
	}
	if h := text(e.do(cmd("history", "list", nil))); !strings.Contains(h, "cancelled") {
		t.Fatalf("history list:\n%s", h)
	}
	// Approve again and finish with a failure: the result replaces the card.
	e.do(click(e.approveIDFor(t, "web"), "eph"))
	ops, _ = e.db.Operations(context.Background(), store.OperationFilter{})
	h, _ := e.db.Hosts(context.Background(), false)
	e.db.DispatchCommands(context.Background(), h[0].ID, 120, e.now)
	_, tr, _ := e.db.RecordEvents(context.Background(), agentA, ops[0].ID, []protocol.Event{{Seq: 1, Type: protocol.EventAccepted},
		{Seq: 2, Type: protocol.EventFailed, Code: protocol.FailPull, Message: "manifest unknown", OutputTail: "Error: manifest unknown"}}, e.now)
	e.notify.OperationFinished(context.Background(), tr.Operation)
	res := text(Response{Message: e.opCard(t, ops[0].ID).Message})
	if !strings.Contains(res, "Deploy failed · app") || !strings.Contains(res, "pull_failed: manifest unknown") {
		t.Fatalf("result:\n%s", res)
	}
	show := text(e.do(cmd("history", "show", map[string]string{optOperation: ops[0].ID})))
	if !strings.Contains(show, "Error: manifest unknown") || !strings.Contains(show, "2. failed") {
		t.Fatalf("history show:\n%s", show)
	}
}

func TestMaintenanceWarnsOnManualApproval(t *testing.T) {
	e := newEnv(t)
	e.deployHost(t)
	e.do(cmd("maintenance", "on", nil))
	r := e.do(click(e.approveIDFor(t, "web"), "eph"))
	if !strings.Contains(text(r), "Maintenance mode is on") {
		t.Fatalf("approval during maintenance: %q", text(r))
	}
}
