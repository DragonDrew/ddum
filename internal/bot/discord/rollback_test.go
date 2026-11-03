package discord

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/DragonDrew/ddum/internal/bot/store"
	"github.com/DragonDrew/ddum/internal/protocol"
)

// finishedDeploy approves "web" in deployHost's stack and finishes it with
// the given event plus a snapshot.
func (e *env) finishedDeploy(t *testing.T, final protocol.Event) store.Operation {
	t.Helper()
	ctx := context.Background()
	e.db.RecordPoll(ctx, store.Poll{AgentID: agentA, HostName: "host-3", PollInterval: 15,
		Capabilities: []string{protocol.CapActionDeploy, protocol.CapActionRollback, protocol.CapTagChangeEdit},
		Docker:       protocol.DockerInfo{Arch: "amd64"}, At: e.now})
	e.do(click(e.approveIDFor(t, "web"), "eph"))
	ops, _ := e.db.Operations(ctx, store.OperationFilter{})
	h, _ := e.db.Hosts(ctx, false)
	e.db.DispatchCommands(ctx, h[0].ID, 120, e.now)
	final.Seq = 2
	if final.Result == nil {
		final.Result = &protocol.Result{}
	}
	final.Result.Snapshots = []protocol.SnapshotRef{{SnapshotID: "00000000-0000-7000-8000-0000000005a1", StackID: ops[0].ScopeID}}
	_, tr, err := e.db.RecordEvents(ctx, agentA, ops[0].ID, []protocol.Event{{Seq: 1, Type: protocol.EventAccepted}, final}, e.now)
	if err != nil {
		t.Fatal(err)
	}
	return tr.Operation
}

func TestRollBackFromTheResultMessage(t *testing.T) {
	e := newEnv(t)
	e.deployHost(t)
	op := e.finishedDeploy(t, protocol.Event{Type: protocol.EventFailed, Code: protocol.FailVerify, Message: "unhealthy"})
	e.notify.OperationFinished(context.Background(), op)
	res := e.opCard(t, op.ID)
	if !strings.Contains(text(Response{Message: res.Message}), "Use **Roll back** below") {
		t.Fatalf("failure message:\n%s", text(Response{Message: res.Message}))
	}
	id := customID(actRollback, op.ID)
	if disabled, ok := buttons(res.Message)[id]; !ok || disabled {
		t.Fatalf("Roll back button missing: %v", buttons(res.Message))
	}
	r := e.do(click(id, res.MessageID))
	if !strings.Contains(text(r), "Roll back app?") || !hasRow(text(r), "web", "1.28", "1.27") {
		t.Fatalf("confirmation: %q", text(r))
	}
	ops, _ := e.db.Operations(context.Background(), store.OperationFilter{})
	if len(ops) != 1 {
		t.Fatal("the confirmation itself must not create anything")
	}
	r = e.do(click(customID(actRollbackConfirm, op.ID), "eph"))
	if r.Kind != RespUpdate || !strings.Contains(text(r), "Rollback queued") {
		t.Fatalf("confirmed: %+v", r)
	}
	ops, _ = e.db.Operations(context.Background(), store.OperationFilter{})
	if len(ops) != 2 || ops[0].Kind != protocol.CommandRollback {
		t.Fatalf("operations = %+v", ops)
	}
	// Only the latest deployment can be rolled back.
	if r := e.do(click(id, res.MessageID)); !strings.Contains(text(r), "a later operation changed this stack") {
		t.Fatalf("second rollback: %q", text(r))
	}
}

func TestRollbackRefusedWithoutSnapshot(t *testing.T) {
	e := newEnv(t)
	e.deployHost(t)
	op := e.finishedDeploy(t, completedEvent())
	if err := e.db.WriteTx(context.Background(), func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE rollback_snapshots SET status = 'unavailable'`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	r := e.do(cmd("rollback", "run", map[string]string{optOperation: op.ID}))
	if !strings.Contains(text(r), "no usable snapshot") {
		t.Fatalf("rollback without snapshot: %q", text(r))
	}
	if l := text(e.do(cmd("rollback", "list", nil))); !strings.Contains(l, "Nothing to roll back") {
		t.Fatalf("list: %s", l)
	}
}

func completedEvent() protocol.Event {
	return protocol.Event{Type: protocol.EventCompleted, Result: &protocol.Result{Containers: []protocol.ContainerOutcome{
		{IdentityKey: "web", Outcome: protocol.OutcomeUpdated}}}}
}

func TestFailedRollbackOffersClearHold(t *testing.T) {
	e := newEnv(t)
	e.deployHost(t)
	op := e.finishedDeploy(t, completedEvent())
	e.do(click(customID(actRollbackConfirm, op.ID), "eph"))
	ops, _ := e.db.Operations(context.Background(), store.OperationFilter{})
	rb := ops[0]
	h, _ := e.db.Hosts(context.Background(), false)
	e.db.DispatchCommands(context.Background(), h[0].ID, 120, e.now)
	_, tr, _ := e.db.RecordEvents(context.Background(), agentA, rb.ID, []protocol.Event{{Seq: 1, Type: protocol.EventAccepted},
		{Seq: 2, Type: protocol.EventFailed, Code: protocol.FailDeploy, Message: "compose failed"}}, e.now)
	e.notify.OperationFinished(context.Background(), tr.Operation)
	res := e.opCard(t, rb.ID)
	s := text(Response{Message: res.Message})
	if !strings.Contains(s, "Rollback failed · app") || !strings.Contains(s, "held until you check it") {
		t.Fatalf("rollback failure:\n%s", s)
	}
	hold := customID(actHoldClear, rb.ScopeID)
	if _, ok := buttons(res.Message)[hold]; !ok {
		t.Fatalf("Clear hold missing: %v", buttons(res.Message))
	}
	if l := text(e.do(cmd("rollback", "list", nil))); !strings.Contains(l, "Held after a failed rollback") {
		t.Fatalf("list: %s", l)
	}
	if r := e.do(click(hold, res.MessageID)); !strings.Contains(text(r), "Hold cleared") {
		t.Fatalf("clear: %q", text(r))
	}
}

func TestBotUpdateHasItsOwnMessage(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	res, _ := e.db.RecordPoll(ctx, store.Poll{AgentID: agentA, HostName: "host-3", PollInterval: 15,
		Capabilities: []string{protocol.CapActionDeploy, protocol.CapTagChangeEdit}, Docker: protocol.DockerInfo{Arch: "amd64"}, At: e.now})
	e.db.ApproveHost(ctx, res.Host.ID, store.Actor{Kind: "user"}, e.now)
	st := protocol.Stack{Fingerprint: "compose:ddum", DisplayName: "ddum", Provider: "compose", Deploy: protocol.Support{Supported: true}}
	st.Containers = []protocol.Container{
		{IdentityKey: "ddum-bot", ImageRef: "ghcr.io/x/ddum-bot:1.0.0", Self: protocol.SelfBot, RepoDigests: []string{"ghcr.io/x/ddum-bot@sha256:" + strings.Repeat("a", 64)},
			TagChange: &protocol.TagChange{Supported: true, Source: "image"}},
		{IdentityKey: "ddum-agent", ImageRef: "ghcr.io/x/ddum-agent:1.0.0", Self: protocol.SelfAgent, Protected: true},
	}
	e.db.ApplyInventory(ctx, agentA, protocol.Inventory{InventoryHash: "x", Stacks: []protocol.Stack{st}}, e.now)
	e.gw.Sent = nil
	cs, _ := e.db.Containers(ctx, store.ContainerFilter{})
	for _, c := range cs {
		if c.Self == protocol.SelfBot {
			d := "sha256:" + strings.Repeat("9", 64)
			e.db.RecordDetection(ctx, store.Detection{ContainerID: c.ID, StackID: c.StackID, HostID: c.HostID, Kind: "version",
				CurrentRef: c.ImageRef, TargetRef: "ghcr.io/x/ddum-bot:1.1.0@" + d, TargetDigest: d, TargetVersion: "1.1.0"}, e.now)
		}
	}
	e.notify.NotifyNewUpdates(ctx)
	var botMsg SentMessage
	for _, m := range e.gw.Sent {
		if strings.Contains(text(Response{Message: m.Message}), "ddum bot update available") {
			botMsg = m
		}
		if s := text(Response{Message: m.Message}); strings.Contains(s, "Updates for ddum") && strings.Contains(s, "ddum-bot") {
			t.Fatalf("the bot must not be in its stack's message:\n%s", s)
		}
	}
	if botMsg.MessageID == "" {
		t.Fatalf("no bot update message: %+v", e.gw.Sent)
	}
	var selfID string
	for id := range buttons(botMsg.Message) {
		if strings.HasPrefix(id, "v1:"+actSelfUpdate+":") {
			selfID = id
		}
	}
	r := e.do(click(selfID, botMsg.MessageID))
	if !strings.Contains(text(r), "This restarts the bot") {
		t.Fatalf("confirmation: %q", text(r))
	}
	r = e.do(click(e.approveID(t, r.Message), "eph"))
	if !strings.Contains(text(r), "Bot update queued") {
		t.Fatalf("approve: %q", text(r))
	}
	ops, _ := e.db.Operations(ctx, store.OperationFilter{})
	if !ops[0].Payload.SelfUpdate {
		t.Fatal("the operation must be a self-update")
	}
}
