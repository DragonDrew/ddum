package discord

import (
	"context"
	"strings"
	"testing"

	"github.com/DragonDrew/ddum/internal/bot/domain"
	"github.com/DragonDrew/ddum/internal/bot/store"
	"github.com/DragonDrew/ddum/internal/protocol"
)

// Ending maintenance mode while operations wait asks first.
func TestMaintenanceOffConfirmsWhileOperationsAreQueued(t *testing.T) {
	e := newEnv(t)
	e.deployHost(t)
	e.do(cmd("maintenance", "on", nil))
	e.do(click(e.approveIDFor(t, "web"), "eph"))

	r := e.do(cmd("maintenance", "off", nil))
	if !strings.Contains(text(r), "Turn maintenance mode off?") || !strings.Contains(text(r), "1 operation") {
		t.Fatalf("expected a confirmation: %q", text(r))
	}
	if on, _ := e.db.Maintenance(context.Background()); !on {
		t.Fatal("asking must not change anything")
	}
	r = e.do(click(customID(actMaintenanceOff), "card"))
	if r.Kind != RespUpdate || !strings.Contains(text(r), "Maintenance mode off") {
		t.Fatalf("confirmed: %q", text(r))
	}
	if on, _ := e.db.Maintenance(context.Background()); on {
		t.Fatal("maintenance mode should be off")
	}
}

// The report that started this: a container updated by hand must not stay
// pending, and its card must say so without waiting for the next check.
func TestManualUpdateClearsTheCardAtOnce(t *testing.T) {
	e := newEnv(t)
	e.deployHost(t)
	ctx := context.Background()
	e.notify.NotifyNewUpdates(ctx)
	card := e.gw.Last()
	if !strings.Contains(text(Response{Message: card.Message}), "1.28") {
		t.Fatalf("card: %s", text(Response{Message: card.Message}))
	}
	approve := e.approveID(t, card.Message)

	st := protocol.Stack{Fingerprint: "compose:app", DisplayName: "app", Provider: "compose", Deploy: protocol.Support{Supported: true},
		Containers: []protocol.Container{
			{IdentityKey: "web", ImageRef: "nginx:1.30", State: "running", RepoDigests: []string{"nginx@sha256:" + strings.Repeat("e", 64)},
				TagChange: &protocol.TagChange{Supported: true, Source: "image"}},
			{IdentityKey: "proxy", ImageRef: "nginx:1.27", State: "running", Protected: true, RepoDigests: []string{"nginx@sha256:" + strings.Repeat("a", 64)},
				TagChange: &protocol.TagChange{Supported: true, Source: "image"}},
		}}
	res, err := e.db.ApplyInventory(ctx, agentA, protocol.Inventory{InventoryHash: "manual", Stacks: []protocol.Stack{st}}, e.now)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.ChangedStacks) != 1 {
		t.Fatalf("changed stacks = %v", res.ChangedStacks)
	}
	e.notify.RefreshStacks(ctx, res.ChangedStacks)
	edited := e.gw.Edits[len(e.gw.Edits)-1]
	s := text(Response{Message: edited.Message})
	if strings.Contains(s, "web") || !strings.Contains(s, "proxy") {
		t.Fatalf("the card must no longer offer web:\n%s", s)
	}

	// The button already on someone's screen approves nothing.
	r := e.do(click(approve, card.MessageID))
	if ops, _ := e.db.Operations(ctx, store.OperationFilter{}); len(ops) != 0 {
		t.Fatalf("operations = %+v; answer = %q", ops, text(r))
	}
}

// After its automatic deploy failed, an update waits for a person, and the card
// lets them act: an automatic container cannot otherwise be approved by hand.
func TestFailedAutoDeployFallsBackToApproval(t *testing.T) {
	e := newEnv(t)
	e.deployHost(t)
	ctx := context.Background()
	mode := domain.ModeAutomatic
	cs, _ := e.db.Containers(ctx, store.ContainerFilter{})
	var web store.Container
	for _, c := range cs {
		if c.IdentityKey == "web" {
			web = c
		}
	}
	if err := e.db.SetPolicy(ctx, store.PolicyKey{Level: domain.LevelContainer, ID: web.ID}, domain.PolicyFields{Mode: &mode},
		store.Actor{Kind: "user", DiscordID: ownerID}, e.now); err != nil {
		t.Fatal(err)
	}
	ups, _ := e.db.Updates(ctx, store.UpdateFilter{ContainerID: web.ID}, e.now)
	offer, _ := e.db.CreateOffer(ctx, "stack", web.StackID, []store.OfferItem{{UpdateID: ups[0].ID, TargetDigest: ups[0].TargetDigest}}, "policy", e.now)
	res, err := e.db.Approve(ctx, store.ApproveRequest{OfferID: offer, By: store.Actor{Kind: "policy"}, At: e.now, ExpiryMinutes: 60})
	if err != nil {
		t.Fatal(err)
	}
	host, _ := e.db.HostByAgent(ctx, agentA)
	e.db.DispatchCommands(ctx, host.ID, 120, e.now)
	if _, _, err := e.db.RecordEvents(ctx, agentA, res.Operation.ID, []protocol.Event{{Seq: 1, Type: protocol.EventAccepted},
		{Seq: 2, Type: protocol.EventFailed, Code: protocol.FailVerify, Message: "unhealthy"}}, e.now); err != nil {
		t.Fatal(err)
	}
	// The next check finds the same digest again and reopens the update.
	if _, err := e.db.RecordDetection(ctx, store.Detection{ContainerID: web.ID, StackID: web.StackID, HostID: web.HostID, Kind: ups[0].Kind,
		CurrentRef: ups[0].CurrentRef, CurrentDigest: ups[0].CurrentDigest, TargetRef: ups[0].TargetRef, TargetDigest: ups[0].TargetDigest,
		TargetVersion: ups[0].TargetVersion}, e.now); err != nil {
		t.Fatal(err)
	}
	v, err := loadScope(ctx, e.db, web.StackID, e.now)
	if err != nil {
		t.Fatal(err)
	}
	offerID, err := offerFor(ctx, e.db, v, "notification", e.now)
	if err != nil {
		t.Fatal(err)
	}
	m := scopeMessage(v, offerID)
	if s := text(Response{Message: m}); !strings.Contains(s, "failed its automatic deploy") {
		t.Fatalf("the card must say why:\n%s", s)
	}
	r := e.do(click(e.approveID(t, m), "card"))
	if !strings.Contains(text(r), "Deploy queued") {
		t.Fatalf("approve: %q", text(r))
	}
}
