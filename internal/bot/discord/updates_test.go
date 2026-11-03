package discord

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/DragonDrew/ddum/internal/bot/store"
	"github.com/DragonDrew/ddum/internal/protocol"
)

// hostWithStacks registers and approves a host whose inventory has the given
// stacks, each with one container "web" running nginx:1.27.
func (e *env) hostWithStacks(t *testing.T, names ...string) {
	t.Helper()
	h := e.register(t, agentA, "host-3", "amd64")
	if _, _, err := e.db.ApproveHost(context.Background(), h.ID, store.Actor{Kind: "user", DiscordID: ownerID}, e.now); err != nil {
		t.Fatal(err)
	}
	var stacks []protocol.Stack
	for _, n := range names {
		stacks = append(stacks, protocol.Stack{Fingerprint: "compose:" + n, DisplayName: n, Provider: "compose",
			Deploy: protocol.Support{Supported: true},
			Containers: []protocol.Container{{IdentityKey: "web", ImageRef: "nginx:1.27", State: "running",
				RepoDigests: []string{"nginx@sha256:" + strings.Repeat("a", 64)},
				TagChange:   &protocol.TagChange{Supported: true, Source: "image"}}}})
	}
	if _, err := e.db.ApplyInventory(context.Background(), agentA, protocol.Inventory{InventoryHash: "x", Stacks: stacks}, e.now); err != nil {
		t.Fatal(err)
	}
	e.gw.Sent, e.gw.Edits = nil, nil // drop the registration notice
}

// detect records an update for the "web" container of a stack.
func (e *env) detect(t *testing.T, stack, version string) store.Update {
	t.Helper()
	cs, _ := e.db.Containers(context.Background(), store.ContainerFilter{})
	for _, c := range cs {
		if c.StackName == stack {
			d := "sha256:" + strings.Repeat(version[len(version)-1:], 64)
			r, err := e.db.RecordDetection(context.Background(), store.Detection{ContainerID: c.ID, StackID: c.StackID, HostID: c.HostID,
				Kind: "version", CurrentRef: "nginx:1.27", CurrentDigest: "sha256:" + strings.Repeat("a", 64),
				TargetRef: "docker.io/library/nginx:" + version + "@" + d, TargetDigest: d, TargetVersion: version}, e.now)
			if err != nil {
				t.Fatal(err)
			}
			return r.Update
		}
	}
	t.Fatalf("stack %s not found", stack)
	return store.Update{}
}

func TestOneMessagePerStackThenNoRepeat(t *testing.T) {
	e := newEnv(t)
	e.hostWithStacks(t, "app", "db")
	e.detect(t, "app", "1.28")
	e.detect(t, "db", "1.28")
	e.notify.NotifyNewUpdates(context.Background())
	if e.gw.SentCount() != 2 {
		t.Fatalf("messages = %d, want one per stack", e.gw.SentCount())
	}
	msg := text(Response{Message: e.gw.Sent[0].Message})
	if !hasRow(msg, "CONTAINER", "FROM", "TO") || !hasRow(msg, "web", "1.27", "1.28") {
		t.Errorf("one state for every row, so no STATE column:\n%s", msg)
	}
	for _, want := range []string{"Updates for app", "Pending approval", "1 update"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message lacks %q:\n%s", want, msg)
		}
	}
	if _, ok := buttons(e.gw.Sent[0].Message)["v1:snooze:"]; ok {
		t.Error("snooze button must carry an offer ID")
	}
	e.notify.NotifyNewUpdates(context.Background())
	if e.gw.SentCount() != 2 {
		t.Fatal("already-notified updates were posted again")
	}
}

func TestFirstRunFloodIsOneSummary(t *testing.T) {
	e := newEnv(t)
	var names []string
	for i := 0; i < 7; i++ {
		names = append(names, fmt.Sprintf("stack%d", i))
	}
	e.hostWithStacks(t, names...)
	for _, n := range names {
		e.detect(t, n, "1.28")
	}
	e.notify.NotifyNewUpdates(context.Background())
	if e.gw.SentCount() != 1 {
		t.Fatalf("messages = %d, want one summary above the limit of 5", e.gw.SentCount())
	}
	s := text(Response{Message: e.gw.Sent[0].Message})
	if !strings.Contains(s, "Updates available") || !strings.Contains(s, "host-3") || !hasRow(s, "stack3", "1", "Pending") {
		t.Fatalf("summary:\n%s", s)
	}
	// One host: the menu opens a stack directly.
	menu := menuOf(t, e.gw.Sent[0].Message)
	if menu.CustomID != customID(actReviewPick) || len(menu.Options) != 7 {
		t.Fatalf("menu = %s with %d options, want the 7 stacks", menu.CustomID, len(menu.Options))
	}
	pick := click(menu.CustomID, e.gw.Sent[0].MessageID)
	pick.Values = []string{menu.Options[0].Value}
	r := e.do(pick)
	if r.Kind != RespUpdate || !strings.Contains(text(r), "Updates for stack") || e.approveID(t, r.Message) == "" {
		t.Fatalf("pick: %+v", text(r))
	}
	if _, ok := buttons(r.Message)[customID(actHostView, e.hostID(t))]; !ok {
		t.Fatalf("the stack card needs a way back to its host: %v", buttons(r.Message))
	}
}

func TestSnoozedNotRenotifiedNewerIs(t *testing.T) {
	e := newEnv(t)
	e.hostWithStacks(t, "app")
	first := e.detect(t, "app", "1.28")
	e.notify.NotifyNewUpdates(context.Background())
	notice := e.gw.Last()

	// Snooze through the message's button and the duration menu.
	var snoozeID string
	for id := range buttons(notice.Message) {
		if strings.HasPrefix(id, "v1:"+actSnooze+":") {
			snoozeID = id
		}
	}
	r := e.do(click(snoozeID, notice.MessageID))
	menu := r.Message.Components[0].(discordgo.ActionsRow).Components[0].(discordgo.SelectMenu)
	pick := click(menu.CustomID, "eph")
	pick.Values = []string{"1 week"}
	r = e.do(pick)
	if r.Kind != RespUpdate || !strings.Contains(text(r), "snoozed 1 update") {
		t.Fatalf("snooze: %+v", r)
	}
	if u, _ := e.db.GetUpdate(context.Background(), first.ID, e.now); !u.Snoozed() {
		t.Fatal("update not snoozed")
	}
	if len(e.gw.Edits) == 0 || !strings.Contains(text(Response{Message: e.gw.Edits[len(e.gw.Edits)-1].Message}), "snoozed until") {
		t.Fatal("the notification should be edited to show the snooze")
	}

	// Re-detecting the snoozed update notifies nothing.
	sent := e.gw.SentCount()
	e.detect(t, "app", "1.28")
	e.notify.NotifyNewUpdates(context.Background())
	if e.gw.SentCount() != sent {
		t.Fatal("a snoozed update was notified again")
	}

	// A newer update is a new record and is notified.
	e.detect(t, "app", "1.29")
	e.notify.NotifyNewUpdates(context.Background())
	if e.gw.SentCount() != sent+1 || !strings.Contains(text(Response{Message: e.gw.Last().Message}), "1.29") {
		t.Fatalf("newer update not notified: sent=%d", e.gw.SentCount())
	}
}

func TestMaintenanceSuppressesThenSummarizes(t *testing.T) {
	e := newEnv(t)
	e.hostWithStacks(t, "app")
	if r := e.do(cmd("maintenance", "on", nil)); r.Ephemeral || !strings.Contains(text(r), "Maintenance mode on") {
		t.Fatalf("on: %+v", r)
	}
	e.gw.Sent = nil
	e.detect(t, "app", "1.28")
	e.notify.NotifyNewUpdates(context.Background())
	if e.gw.SentCount() != 0 {
		t.Fatal("maintenance mode must suppress update notifications")
	}
	if r := e.do(cmd("maintenance", "status", nil)); !strings.Contains(text(r), "Maintenance mode is on") {
		t.Fatalf("status: %+v", r)
	}
	if r := e.do(cmd("maintenance", "off", nil)); !strings.Contains(text(r), "Maintenance mode off") {
		t.Fatalf("off: %s", text(r))
	}
	if e.gw.SentCount() != 1 || !hasRow(text(Response{Message: e.gw.Last().Message}), "app", "1", "Pending") {
		t.Fatalf("turning maintenance off should post one summary (the reply is the notice): %+v", e.gw.Sent)
	}
	// Suppressed updates are not replayed afterwards.
	e.notify.NotifyNewUpdates(context.Background())
	if e.gw.SentCount() != 1 {
		t.Fatal("suppressed notifications were replayed")
	}
}

func TestPolicyShowNamesTheLevel(t *testing.T) {
	e := newEnv(t)
	e.hostWithStacks(t, "app")
	stacks, _ := e.db.Stacks(context.Background(), store.StackFilter{})
	hosts, _ := e.db.Hosts(context.Background(), false)
	cs, _ := e.db.Containers(context.Background(), store.ContainerFilter{})

	e.do(cmd("policy", "set", map[string]string{optScope: "host", optTarget: hosts[0].ID, optTagTrack: "any"}))
	r := e.do(cmd("policy", "set", map[string]string{optScope: "stack", optTarget: stacks[0].ID, optMode: "automatic"}))
	if !strings.Contains(text(r), "Saved") {
		t.Fatalf("set: %s", text(r))
	}
	show := text(e.do(cmd("policy", "show", map[string]string{optScope: "container", optTarget: cs[0].ID})))
	for _, want := range []string{"mode: **automatic**\nset at: stack", "tag_track: **any**\nset at: host",
		"auto_rollback: **true**\nset at: default (follows mode)", "verify_timeout_seconds: **120 s**\nset at: global"} {
		if !strings.Contains(show, want) {
			t.Errorf("policy show lacks %q:\n%s", want, show)
		}
	}
	r = e.do(cmd("policy", "set", map[string]string{optScope: "global"}))
	if !strings.Contains(text(r), "at least one field") {
		t.Fatalf("empty set: %+v", r)
	}
	r = e.do(cmd("policy", "clear", map[string]string{optScope: "stack", optTarget: stacks[0].ID}))
	if !strings.Contains(text(r), "mode: **manual**\nset at: global") {
		t.Fatalf("clear:\n%s", text(r))
	}
	// Target autocomplete follows the chosen level.
	ac := e.do(Interaction{Kind: KindAutocomplete, Command: "policy", Subcommand: "show", Focused: optTarget,
		Options: map[string]string{optScope: "stack", optTarget: ""}})
	if len(ac.Choices) != 1 || ac.Choices[0].Value != stacks[0].ID {
		t.Fatalf("target autocomplete = %+v", ac.Choices)
	}
}

func TestUpdateCommands(t *testing.T) {
	e := newEnv(t)
	e.hostWithStacks(t, "app")
	u := e.detect(t, "app", "1.28")

	list := text(e.do(cmd("update", "list", nil)))
	if !strings.Contains(list, "**app** · host-3") || !strings.Contains(list, "`1.27` → **`1.28`**") {
		t.Fatalf("list:\n%s", list)
	}
	show := e.do(cmd("update", "show", map[string]string{optUpdate: u.ID}))
	if !strings.Contains(text(show), "nginx:1.28") || len(buttons(show.Message)) != 3 {
		t.Fatalf("show: %s", text(show))
	}
	r := e.do(cmd("update", "snooze", map[string]string{optUpdate: u.ID, optDuration: "indefinitely"}))
	if !strings.Contains(text(r), "indefinitely") {
		t.Fatalf("snooze: %+v", r)
	}
	if l := text(e.do(cmd("update", "list", nil))); !strings.Contains(l, "1 snoozed") {
		t.Fatalf("list should hide snoozed: %s", l)
	}
	if s := text(e.do(cmd("update", "snoozed", nil))); !strings.Contains(s, "web") {
		t.Fatalf("snoozed: %s", s)
	}
	ac := e.do(Interaction{Kind: KindAutocomplete, Command: "update", Subcommand: "unsnooze", Focused: optUpdate,
		Options: map[string]string{optUpdate: ""}})
	if len(ac.Choices) != 1 || ac.Choices[0].Value != u.ID {
		t.Fatalf("unsnooze autocomplete = %+v", ac.Choices)
	}
	if r := e.do(cmd("update", "unsnooze", map[string]string{optUpdate: u.ID})); !strings.Contains(text(r), "Snooze removed") {
		t.Fatalf("unsnooze: %+v", r)
	}
	e.do(cmd("update", "snooze", map[string]string{optUpdate: u.ID, optDuration: "1 day"}))
	r = e.do(cmd("update", "unsnooze-all", nil))
	if _, ok := buttons(r.Message)[customID(actUnsnoozeAll)]; !ok {
		t.Fatalf("unsnooze-all must confirm: %+v", r)
	}
	if r := e.do(click(customID(actUnsnoozeAll), "eph")); !strings.Contains(text(r), "Removed 1 snooze.") {
		t.Fatalf("unsnooze-all: %+v", r)
	}

	var started []string
	e.router.StartCheck = func(h, s string, _ func(CheckSummary)) error { started = append(started, h+"|"+s); return nil }
	stacks, _ := e.db.Stacks(context.Background(), store.StackFilter{})
	if r := e.do(cmd("update", "check", map[string]string{optStack: stacks[0].ID})); !strings.Contains(text(r), "Checking stack app") {
		t.Fatalf("check: %+v", r)
	}
	if len(started) != 1 || !strings.HasSuffix(started[0], stacks[0].ID) {
		t.Fatalf("check scope = %v", started)
	}
	e.router.StartCheck = func(string, string, func(CheckSummary)) error { return ErrCheckBusy }
	if r := e.do(cmd("update", "check", nil)); !strings.Contains(text(r), "already running") {
		t.Fatalf("busy: %+v", r)
	}
	e.router.StartCheck = func(string, string, func(CheckSummary)) error { return errors.New("boom") }
	if r := e.do(cmd("update", "check", nil)); !strings.Contains(text(r), "went wrong") {
		t.Fatalf("error: %+v", r)
	}
}

// TestCheckCardShowsTheResult: /update check answers with a card that the
// bot edits into the result, whichever comes first: the check finishing or
// Discord saying where the card is.
func TestCheckCardShowsTheResult(t *testing.T) {
	for _, finishFirst := range []bool{false, true} {
		e := newEnv(t)
		e.hostWithStacks(t, "app", "db")
		e.detect(t, "app", "1.28")
		var done func(CheckSummary)
		e.router.StartCheck = func(_, _ string, d func(CheckSummary)) error { done = d; return nil }
		r := e.do(cmd("update", "check", nil))
		if r.Ephemeral || r.Bind == nil || r.Bind.Entity != entityCheck || !strings.Contains(text(r), "Checking all hosts") {
			t.Fatalf("check card: %+v", r)
		}
		ctx := context.Background()
		if finishFirst {
			done(CheckSummary{Checked: 2, NewUpdates: 1})
			e.router.Delivered(ctx, r, channel, "card-1")
		} else {
			e.router.Delivered(ctx, r, channel, "card-1")
			if len(e.gw.Edits) != 0 {
				t.Fatal("edited before the check finished")
			}
			done(CheckSummary{Checked: 2, NewUpdates: 1})
		}
		card := e.current("card-1")
		s := text(Response{Message: card.Message})
		if !strings.Contains(s, "Check finished · all hosts") || !strings.Contains(s, "Checked **2** images · **1** pending · **1** new") || !hasRow(s, "app", "1", "Pending") {
			t.Fatalf("finishFirst=%v: result card:\n%s", finishFirst, s)
		}
		// Pick → the same card becomes the stack's card with Approve.
		menu := menuOf(t, card.Message)
		pick := click(menu.CustomID, "card-1")
		pick.Values = []string{menu.Options[0].Value}
		if p := e.do(pick); p.Kind != RespUpdate || e.approveID(t, p.Message) == "" {
			t.Fatalf("picked: %+v", p)
		}
	}
	// A check run outside the notification channel answers privately, and
	// its result goes to the channel as usual.
	e := newEnv(t)
	e.router.StartCheck = func(_, _ string, d func(CheckSummary)) error {
		if d != nil {
			t.Fatal("a private card cannot be edited later")
		}
		return nil
	}
	it := cmd("update", "check", nil)
	it.ChannelID = "elsewhere"
	if r := e.do(it); !r.Ephemeral || r.Bind != nil {
		t.Fatalf("private check: %+v", r)
	}
}

func TestHeldReasonsShown(t *testing.T) {
	e := newEnv(t)
	h := e.register(t, agentA, "host-3", "amd64")
	e.db.ApproveHost(context.Background(), h.ID, store.Actor{Kind: "user"}, e.now)
	e.db.ApplyInventory(context.Background(), agentA, protocol.Inventory{InventoryHash: "x", Stacks: []protocol.Stack{
		{Fingerprint: "container:solo", DisplayName: "solo", Provider: "docker", Deploy: protocol.Support{Reason: "standalone_manual"},
			Containers: []protocol.Container{{IdentityKey: "web", ImageRef: "nginx:1.27", State: "running"}}},
	}}, e.now)
	e.gw.Sent = nil
	e.detect(t, "solo", "1.28")
	e.notify.NotifyNewUpdates(context.Background())
	s := text(Response{Message: e.gw.Last().Message})
	if !strings.Contains(s, "⛔ **web** · manual update required (standalone_manual)") {
		t.Fatalf("held reason missing:\n%s", s)
	}
}

func TestSnoozeExpiredNotice(t *testing.T) {
	e := newEnv(t)
	e.hostWithStacks(t, "app")
	u := e.detect(t, "app", "1.28")
	e.notify.SnoozeExpired(context.Background(), []store.ExpiredSnooze{{Update: u, Until: e.now}})
	if s := text(Response{Message: e.gw.Last().Message}); !strings.Contains(s, "Snooze ended") || !strings.Contains(s, "pending again") {
		t.Fatalf("notice:\n%s", s)
	}
}

// TestNavigateHostsThenStacks: with several hosts, a summary lists each
// host's stacks in its own field and its menu opens a host, then a stack,
// all on the same card.
func TestNavigateHostsThenStacks(t *testing.T) {
	e := newEnv(t)
	e.hostWithStacks(t, "plex", "immich")
	ctx := context.Background()
	h2 := e.register(t, agentB, "host-0", "amd64")
	e.db.ApproveHost(ctx, h2.ID, store.Actor{Kind: "user", DiscordID: ownerID}, e.now)
	e.db.ApplyInventory(ctx, agentB, protocol.Inventory{InventoryHash: "b", Stacks: []protocol.Stack{{Fingerprint: "compose:nas",
		DisplayName: "nas", Provider: "compose", Deploy: protocol.Support{Supported: true},
		Containers: []protocol.Container{{IdentityKey: "web", ImageRef: "nginx:1.27", State: "running",
			RepoDigests: []string{"nginx@sha256:" + strings.Repeat("a", 64)}, TagChange: &protocol.TagChange{Supported: true, Source: "image"}}}}}}, e.now)
	for _, s := range []string{"plex", "immich", "nas"} {
		e.detect(t, s, "1.28")
	}
	r := e.do(click(customID(actReview), "card"))
	if len(r.Message.Embeds) == 0 || len(r.Message.Embeds[0].Fields) != 2 {
		t.Fatalf("want one field per host: %s", text(r))
	}
	s := text(r)
	if !strings.Contains(s, "🖥️ host-3: "+"```") || !hasRow(s, "STACK", "/", "GROUP", "#", "STATE") || !hasRow(s, "immich", "1", "Pending") || !hasRow(s, "plex", "1", "Pending") || !hasRow(s, "nas", "1", "Pending") {
		t.Fatalf("fields:\n%s", s)
	}
	menu := menuOf(t, r.Message)
	if menu.CustomID != customID(actHostPick) || len(menu.Options) != 2 {
		t.Fatalf("with two hosts the menu picks a host: %+v", menu)
	}
	pick := click(menu.CustomID, "card")
	pick.Values = []string{e.hostID(t)}
	for _, o := range menu.Options {
		if o.Label == "host-3" {
			pick.Values = []string{o.Value}
		}
	}
	hv := e.do(pick)
	if hv.Kind != RespUpdate || !hasRow(text(hv), "plex", "1", "Pending") || strings.Contains(text(hv), "nas") {
		t.Fatalf("host view:\n%s", text(hv))
	}
	if _, ok := buttons(hv.Message)[customID(actReview)]; !ok {
		t.Fatal("the host view needs a way back to all hosts")
	}
	sm := menuOf(t, hv.Message)
	sp := click(sm.CustomID, "card")
	sp.Values = []string{sm.Options[0].Value}
	if st := e.do(sp); !strings.Contains(text(st), "Updates for immich") || e.approveID(t, st.Message) == "" {
		t.Fatalf("stack:\n%s", text(st))
	}
}

// TestScheduleDeploy: Schedule asks for a time, then the card becomes the
// scheduled deploy, which waits for its time.
func TestScheduleDeploy(t *testing.T) {
	e := newEnv(t)
	e.router.Location = time.FixedZone("AEST", 10*3600)
	e.deployHost(t)
	e.notify.NotifyNewUpdates(context.Background())
	card := e.gw.Last()
	var sched string
	for id := range buttons(card.Message) {
		if strings.HasPrefix(id, "v1:"+actSchedule+":") {
			sched = id
		}
	}
	if sched == "" {
		t.Fatalf("no Schedule button: %v", buttons(card.Message))
	}
	r := e.do(click(sched, card.MessageID))
	menu := menuOf(t, r.Message)
	if r.Kind != RespUpdate || len(menu.Options) != 24 || !strings.Contains(text(r), "AEST") {
		t.Fatalf("picker: %d options\n%s", len(menu.Options), text(r))
	}
	// t0 is 2026-10-01 02:00 UTC = 12:00 AEST; pick 03:00 tomorrow AEST.
	var value string
	for _, o := range menu.Options {
		if o.Label == "03:00 tomorrow" {
			value = o.Value
		}
	}
	if value == "" {
		t.Fatalf("no 03:00 slot: %+v", menu.Options)
	}
	pick := click(menu.CustomID, card.MessageID)
	pick.Values = []string{value}
	r = e.do(pick)
	ops, _ := e.db.Operations(context.Background(), store.OperationFilter{})
	if len(ops) != 1 || ops[0].NotBefore.IsZero() || ops[0].NotBefore.In(e.router.Location).Hour() != 3 {
		t.Fatalf("operation: %+v", ops)
	}
	if !strings.Contains(text(r), "Scheduled · app") || !strings.Contains(text(r), fmt.Sprintf("<t:%d:f>", ops[0].NotBefore.Unix())) {
		t.Fatalf("scheduled card:\n%s", text(r))
	}
	if _, ok := buttons(r.Message)[customID(actOpCancel, ops[0].ID)]; !ok {
		t.Fatal("a scheduled deploy can be cancelled")
	}
	h, _ := e.db.Hosts(context.Background(), false)
	if got, _ := e.db.DispatchCommands(context.Background(), h[0].ID, 120, e.now); len(got) != 0 {
		t.Fatal("dispatched before its time")
	}
	// A stale pick (the time has passed) is refused.
	pick.Values = []string{fmt.Sprint(e.now.Add(-time.Hour).Unix())}
	if r := e.do(pick); !strings.Contains(text(r), "passed") {
		t.Fatalf("past time: %s", text(r))
	}
}

// TestReleaseNotesAreLinksOnly: release notes appear once, as links whose
// text is not Markdown-escaped, and not again as buttons.
func TestReleaseNotesAreLinksOnly(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	h := e.register(t, agentA, "host-1", "amd64")
	e.db.ApproveHost(ctx, h.ID, store.Actor{Kind: "user", DiscordID: ownerID}, e.now)
	e.db.ApplyInventory(ctx, agentA, protocol.Inventory{InventoryHash: "x", Stacks: []protocol.Stack{{Fingerprint: "compose:arr",
		DisplayName: "arr", Provider: "compose", Deploy: protocol.Support{Supported: true},
		Containers: []protocol.Container{{IdentityKey: "portainer_agent", ImageRef: "nginx:1.27", State: "running",
			RepoDigests: []string{"nginx@sha256:" + strings.Repeat("a", 64)}, TagChange: &protocol.TagChange{Supported: true, Source: "image"}}}}}}, e.now)
	e.gw.Sent = nil
	u := e.detect(t, "arr", "1.28")
	e.db.SetReleaseURL(ctx, u.ID, "https://github.com/x/web/releases/tag/1.28")
	e.notify.NotifyNewUpdates(context.Background())
	m := e.gw.Last().Message
	s := text(Response{Message: m})
	if !strings.Contains(s, "[portainer_agent](https://github.com/x/web/releases/tag/1.28)") {
		t.Fatalf("link text must not be escaped:\n%s", s)
	}
	for _, row := range m.Components {
		for _, c := range row.(discordgo.ActionsRow).Components {
			if b, ok := c.(discordgo.Button); ok && b.Style == discordgo.LinkButton {
				t.Fatalf("release notes must not repeat as buttons: %+v", b)
			}
		}
	}
}
