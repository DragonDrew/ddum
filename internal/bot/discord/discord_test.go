package discord

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/DragonDrew/ddum/internal/bot/store"
	"github.com/DragonDrew/ddum/internal/ids"
	"github.com/DragonDrew/ddum/internal/protocol"
)

const (
	guild      = "100000000000000011"
	channel    = "100000000000000012"
	ownerID    = "100000000000000010"
	strangerID = "999999999999999999"
	agentA     = "0195f2c4-0000-7000-8000-00000000000a"
	agentB     = "0195f2c4-0000-7000-8000-00000000000b"
)

var t0 = time.Date(2026, 10, 1, 2, 0, 0, 0, time.UTC)

type env struct {
	db     *store.Store
	gw     *FakeGateway
	router *Router
	notify *Notifier
	now    time.Time
}

func newEnv(t *testing.T) *env {
	t.Helper()
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "ddum.db"), ids.V7{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	e := &env{db: db, gw: &FakeGateway{}, now: t0}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	now := func() time.Time { return e.now }
	e.router = NewRouter(Router{Store: db, Gateway: e.gw, Log: log, Now: now, Started: t0.Add(-time.Hour),
		Version: "test", GuildID: guild, ChannelID: channel}, []string{ownerID})
	e.notify = NewNotifier(Notifier{Gateway: e.gw, Store: db, ChannelID: channel, Log: log, Now: now}, 0)
	e.router.Notify = e.notify
	return e
}

// register makes an agent poll, and posts the registration notice like the
// HTTP API does.
func (e *env) register(t *testing.T, agentID, name, arch string) store.Host {
	t.Helper()
	res, err := e.db.RecordPoll(context.Background(), store.Poll{AgentID: agentID, HostName: name, PollInterval: 30,
		Docker: protocol.DockerInfo{EngineVersion: "29.6.0", OS: "linux", Arch: arch}, At: e.now})
	if err != nil {
		t.Fatal(err)
	}
	e.notify.HostRegistered(context.Background(), res.Host, res.Collision)
	return res.Host
}

func (e *env) do(it Interaction) Response {
	if it.ChannelID == "" {
		it.ChannelID = channel
	}
	if it.GuildID == "" {
		it.GuildID = guild
	}
	if it.UserID == "" {
		it.UserID = ownerID
		it.UserName = "drew"
	}
	if it.Options == nil {
		it.Options = map[string]string{}
	}
	return e.router.Handle(context.Background(), it)
}

func cmd(command, sub string, opts map[string]string) Interaction {
	return Interaction{Kind: KindCommand, Command: command, Subcommand: sub, Options: opts}
}

func click(customID, messageID string) Interaction {
	return Interaction{Kind: KindComponent, CustomID: customID, MessageID: messageID}
}

// buttons returns the custom IDs and disabled flags of a message's buttons.
func buttons(m Message) map[string]bool {
	out := map[string]bool{}
	for _, row := range m.Components {
		ar, ok := row.(discordgo.ActionsRow)
		if !ok {
			continue
		}
		for _, c := range ar.Components {
			if b, ok := c.(discordgo.Button); ok {
				out[b.CustomID] = b.Disabled
			}
		}
	}
	return out
}

func text(r Response) string {
	var b strings.Builder
	b.WriteString(r.Message.Content)
	for _, e := range r.Message.Embeds {
		b.WriteString("\n" + e.Title + "\n" + e.Description)
		for _, f := range e.Fields {
			b.WriteString("\n" + f.Name + ": " + f.Value)
		}
		if e.Footer != nil {
			b.WriteString("\n" + e.Footer.Text)
		}
	}
	return b.String()
}

func TestUnauthorizedUsersAreRejectedEverywhere(t *testing.T) {
	e := newEnv(t)
	h := e.register(t, agentA, "host-3", "amd64")
	cases := []Interaction{
		cmd("host", "list", nil),
		cmd("system", "status", nil),
		click(customID(actHostApprove, h.ID), "msg-1"),
		click(customID(actHostRemove, h.ID), "msg-1"),
	}
	for _, it := range cases {
		it.UserID, it.UserName = strangerID, "mallory"
		r := e.do(it)
		if r.Kind != RespMessage || !r.Ephemeral || text(r) != NotAuthorized || len(r.Message.Embeds) != 0 {
			t.Errorf("%s %s %s: got %+v, want only an ephemeral %q", it.Command, it.Subcommand, it.CustomID, r, NotAuthorized)
		}
	}
	ac := Interaction{Kind: KindAutocomplete, Command: "host", Subcommand: "show", Focused: optHost,
		Options: map[string]string{optHost: ""}, UserID: strangerID}
	if r := e.do(ac); r.Kind != RespChoices || len(r.Choices) != 0 {
		t.Errorf("unauthorized autocomplete leaked choices: %+v", r.Choices)
	}
	// An authorized user in another guild is also refused.
	if r := e.do(Interaction{Kind: KindCommand, Command: "host", Subcommand: "list", GuildID: "111111111111111111"}); text(r) != NotAuthorized {
		t.Errorf("other guild: %+v", r)
	}
	got, _ := e.db.Host(context.Background(), h.ID)
	if got.Approval != "pending" || got.Removed() {
		t.Fatalf("an unauthorized click changed the host: %+v", got)
	}
}

func TestApproveFromRegistrationNotice(t *testing.T) {
	e := newEnv(t)
	h := e.register(t, agentA, "host-3", "amd64")
	if e.gw.SentCount() != 1 {
		t.Fatalf("registration notices = %d, want 1", e.gw.SentCount())
	}
	notice := e.gw.Last()
	if notice.ChannelID != channel || !strings.Contains(text(Response{Message: notice.Message}), "register: host-3") {
		t.Fatalf("notice = %+v", notice)
	}
	b := buttons(notice.Message)
	if disabled, ok := b[customID(actHostApprove, h.ID)]; !ok || disabled {
		t.Fatalf("Approve button missing or disabled: %v", b)
	}
	if _, ok := b[customID(actHostReject, h.ID)]; !ok {
		t.Fatalf("Reject button missing: %v", b)
	}

	r := e.do(click(customID(actHostApprove, h.ID), notice.MessageID))
	if r.Kind != RespUpdate || len(buttons(r.Message)) != 0 || !strings.Contains(text(r), "Approved by <@"+ownerID+">") {
		t.Fatalf("approve click response: %+v", r)
	}
	got, _ := e.db.Host(context.Background(), h.ID)
	if got.Approval != "approved" {
		t.Fatalf("host not approved: %+v", got)
	}
	// A second click (or a second user) changes nothing and says so.
	r = e.do(click(customID(actHostApprove, h.ID), notice.MessageID))
	if r.Kind != RespUpdate || !strings.Contains(text(r), "Already approved") {
		t.Fatalf("second click: %+v", text(r))
	}
}

func TestApproveCommandEditsTheNotice(t *testing.T) {
	e := newEnv(t)
	h := e.register(t, agentA, "host-3", "amd64")
	r := e.do(cmd("host", "approve", map[string]string{optHost: h.ID}))
	if r.Ephemeral || !strings.Contains(text(r), "Approved") {
		t.Fatalf("approve: %+v", text(r))
	}
	if len(e.gw.Edits) != 1 || e.gw.Edits[0].MessageID != e.gw.Sent[0].MessageID || len(buttons(e.gw.Edits[0].Message)) != 0 {
		t.Fatalf("the registration notice should be edited to drop its buttons: %+v", e.gw.Edits)
	}
}

func TestUnsupportedArchitectureCannotBeApproved(t *testing.T) {
	e := newEnv(t)
	h := e.register(t, agentA, "old-pi", "arm")
	if disabled := buttons(e.gw.Last().Message)[customID(actHostApprove, h.ID)]; !disabled {
		t.Fatal("Approve must be disabled for 32-bit arm")
	}
	if !strings.Contains(text(Response{Message: e.gw.Last().Message}), "unsupported architecture") {
		t.Fatal("the notice must say why Approve is disabled")
	}
	r := e.do(cmd("host", "approve", map[string]string{optHost: h.ID}))
	if !strings.Contains(text(r), "unsupported architecture") {
		t.Fatalf("approve arm: %+v", r)
	}
}

func TestRejectNeedsConfirmation(t *testing.T) {
	e := newEnv(t)
	h := e.register(t, agentA, "rogue", "amd64")
	r := e.do(click(customID(actHostReject, h.ID), e.gw.Last().MessageID))
	if r.Kind != RespUpdate || buttons(r.Message)[customID(actHostRemove, h.ID)] {
		t.Fatalf("reject should ask for confirmation: %+v", r)
	}
	if got, _ := e.db.Host(context.Background(), h.ID); got.Removed() {
		t.Fatal("removed before confirmation")
	}
	if r := e.do(click(customID(actCancel), "eph")); r.Kind != RespUpdate || !strings.Contains(text(r), "Cancelled") {
		t.Fatalf("cancel: %+v", r)
	}
	r = e.do(click(customID(actHostRemove, h.ID), "eph"))
	if r.Kind != RespUpdate || !strings.Contains(text(r), "Rejected") {
		t.Fatalf("confirm: %+v", r)
	}
	got, _ := e.db.Host(context.Background(), h.ID)
	if !got.Removed() {
		t.Fatal("host not removed")
	}
	if len(e.gw.Edits) != 1 || !strings.Contains(text(Response{Message: e.gw.Edits[0].Message}), "Rejected by") {
		t.Fatalf("notice not edited: %+v", e.gw.Edits)
	}
	// The old Approve button on the notice is now stale.
	if r := e.do(click(customID(actHostApprove, h.ID), e.gw.Sent[0].MessageID)); !strings.Contains(text(r), "removed") {
		t.Fatalf("stale approve: %+v", r)
	}
}

func TestRemoveAndReassignViaCommands(t *testing.T) {
	e := newEnv(t)
	h := e.register(t, agentA, "nas", "amd64")
	e.do(cmd("host", "approve", map[string]string{optHost: h.ID}))

	r := e.do(cmd("host", "reassign-agent", map[string]string{optHost: h.ID, optAgentID: "not-a-uuid"}))
	if !strings.Contains(text(r), "not a valid agent UUID") {
		t.Fatalf("bad uuid: %+v", r)
	}
	r = e.do(cmd("host", "reassign-agent", map[string]string{optHost: h.ID, optAgentID: agentB}))
	id := customID(actHostReassign, h.ID, agentB)
	if _, ok := buttons(r.Message)[id]; !ok || len(id) > 100 {
		t.Fatalf("reassign confirm button %q missing or too long: %+v", id, r)
	}
	r = e.do(click(id, "eph"))
	if !strings.Contains(text(r), "Now uses agent") {
		t.Fatalf("reassign: %+v", r)
	}
	got, _ := e.db.Host(context.Background(), h.ID)
	if got.AgentID != agentB || got.Approval != "approved" {
		t.Fatalf("after reassign: %+v", got)
	}

	r = e.do(cmd("host", "remove", map[string]string{optHost: "NAS"})) // by name, case-insensitive
	if _, ok := buttons(r.Message)[customID(actHostRemove, h.ID)]; !ok || !strings.Contains(text(r), "agent is refused from now on") {
		t.Fatalf("remove confirm: %+v", r)
	}
	e.do(click(customID(actHostRemove, h.ID), "eph"))
	if got, _ := e.db.Host(context.Background(), h.ID); !got.Removed() {
		t.Fatal("host not removed")
	}
	var n int
	e.db.Reader().QueryRow(`SELECT count(*) FROM audit_events WHERE action = 'host.removed' AND actor_discord_id = ?`, ownerID).Scan(&n)
	if n != 1 {
		t.Fatalf("host.removed audit with actor = %d, want 1", n)
	}
}

func seedInventory(t *testing.T, e *env, agentID string) {
	t.Helper()
	stacks := []protocol.Stack{{
		Fingerprint: "compose:immich", DisplayName: "immich", Provider: "compose",
		Compose: &protocol.ComposeInfo{Project: "immich", ConfigFiles: []string{"/opt/stacks/immich/compose.yaml"}, WorkingDir: "/opt/stacks/immich"},
		Deploy:  protocol.Support{Supported: true},
		Containers: []protocol.Container{
			{IdentityKey: "immich-server", DockerName: "immich_server", DockerID: "3f9c0123456789", ImageRef: "ghcr.io/immich-app/immich-server:v1.140.1",
				RepoDigests: []string{"ghcr.io/immich-app/immich-server@sha256:0123456789abcdef0123"}, Platform: "linux/amd64", State: "running", Health: "healthy",
				DdumLabels: map[string]string{"ddum.ignore": "false"}},
			{IdentityKey: "database", DockerName: "immich_postgres", ImageRef: "postgres:16", State: "running"},
		},
	}, {
		Fingerprint: "container:portainer", DisplayName: "portainer", Provider: "docker",
		Deploy:     protocol.Support{Reason: protocol.ReasonStandaloneManual},
		Containers: []protocol.Container{{IdentityKey: "portainer", ImageRef: "portainer/portainer-ce:lts", State: "running", Protected: true}},
	}}
	if _, err := e.db.ApplyInventory(context.Background(), agentID,
		protocol.Inventory{InventoryHash: "sha256:x", Stacks: stacks}, e.now); err != nil {
		t.Fatal(err)
	}
}

func TestReadOnlyCommands(t *testing.T) {
	e := newEnv(t)
	h := e.register(t, agentA, "host-3", "amd64")
	seedInventory(t, e, agentA)

	list := text(e.do(cmd("host", "list", nil)))
	if !strings.Contains(list, "host-3") || !strings.Contains(list, "pending approval") || !strings.Contains(list, "2 stacks") {
		t.Errorf("host list:\n%s", list)
	}
	show := text(e.do(cmd("host", "show", map[string]string{optHost: h.ID})))
	for _, want := range []string{agentA, "29.6.0", "linux/amd64", "immich", "Inventory from <t:", "/host approve"} {
		if !strings.Contains(show, want) {
			t.Errorf("host show lacks %q:\n%s", want, show)
		}
	}

	stacks, _ := e.db.Stacks(context.Background(), store.StackFilter{})
	var immich store.Stack
	for _, s := range stacks {
		if s.Fingerprint == "compose:immich" {
			immich = s
		}
	}
	st := text(e.do(cmd("stack", "show", map[string]string{optStack: immich.ID})))
	for _, want := range []string{"compose:immich", "compose.yaml", "immich-server", "(healthy)", "Inventory from"} {
		if !strings.Contains(st, want) {
			t.Errorf("stack show lacks %q:\n%s", want, st)
		}
	}
	if sl := text(e.do(cmd("stack", "list", map[string]string{optHost: h.ID}))); !strings.Contains(sl, "portainer") {
		t.Errorf("stack list:\n%s", sl)
	}
	cl := text(e.do(cmd("container", "list", map[string]string{optStack: immich.ID})))
	if !strings.Contains(cl, "database") || strings.Contains(cl, "portainer") {
		t.Errorf("container list by stack:\n%s", cl)
	}
	cs, _ := e.db.Containers(context.Background(), store.ContainerFilter{StackID: immich.ID})
	var server store.Container
	for _, c := range cs {
		if c.IdentityKey == "immich-server" {
			server = c
		}
	}
	cshow := text(e.do(cmd("container", "show", map[string]string{optContainer: server.ID})))
	for _, want := range []string{"ghcr.io/immich-app/immich-server", "sha256:0123456789ab", "ddum.ignore=false", "linux/amd64"} {
		if !strings.Contains(cshow, want) {
			t.Errorf("container show lacks %q:\n%s", want, cshow)
		}
	}

	status := text(e.do(cmd("system", "status", nil)))
	for _, want := range []string{"test", "1h0m0s", "0 online · 0 offline · 1 pending approval", "Maintenance mode: off"} {
		if !strings.Contains(status, want) {
			t.Errorf("system status lacks %q:\n%s", want, status)
		}
	}
}

func TestRenames(t *testing.T) {
	e := newEnv(t)
	h := e.register(t, agentA, "host-3", "amd64")
	seedInventory(t, e, agentA)
	if r := e.do(cmd("host", "rename", map[string]string{optHost: h.ID, optName: "NAS"})); !strings.Contains(text(r), "is now **NAS**") {
		t.Fatalf("host rename: %+v", r)
	}
	stacks, _ := e.db.Stacks(context.Background(), store.StackFilter{})
	if r := e.do(cmd("stack", "rename", map[string]string{optStack: stacks[0].ID, optName: "Photos"})); !strings.Contains(text(r), "Photos") {
		t.Fatalf("stack rename: %+v", r)
	}
	got, _ := e.db.Host(context.Background(), h.ID)
	st, _ := e.db.Stack(context.Background(), stacks[0].ID)
	if got.DisplayName != "NAS" || st.DisplayName != "Photos" {
		t.Fatalf("names: %q %q", got.DisplayName, st.DisplayName)
	}
	if r := e.do(cmd("host", "rename", map[string]string{optHost: h.ID, optName: "  "})); !strings.Contains(text(r), "cannot be empty") {
		t.Fatalf("empty rename: %+v", r)
	}
}

func TestAutocomplete(t *testing.T) {
	e := newEnv(t)
	for i := 0; i < 30; i++ {
		e.register(t, fmt.Sprintf("0195f2c4-0000-7000-8000-%012d", i), fmt.Sprintf("host-%02d", i), "amd64")
	}
	approved := e.register(t, agentA, "approved-one", "amd64")
	e.do(cmd("host", "approve", map[string]string{optHost: approved.ID}))

	ac := func(command, sub, typed string) []Choice {
		return e.do(Interaction{Kind: KindAutocomplete, Command: command, Subcommand: sub, Focused: optHost,
			Options: map[string]string{optHost: typed}}).Choices
	}
	all := ac("host", "show", "")
	if len(all) != 25 {
		t.Fatalf("choices = %d, want the 25 maximum", len(all))
	}
	for _, c := range all {
		if _, err := ids.Parse(c.Value); err != nil || len(c.Name) > 100 {
			t.Fatalf("choice %+v: value must be a UUID and name ≤ 100", c)
		}
	}
	if got := ac("host", "show", "HOST-1"); len(got) != 10 {
		t.Fatalf("filter 'HOST-1' = %d choices, want 10 (host-10..host-19)", len(got))
	}
	for _, c := range ac("host", "approve", "approved") {
		t.Fatalf("approve autocomplete offered a non-pending host: %+v", c)
	}
}

func TestEveryoneCanView(t *testing.T) {
	view := int64(discordgo.PermissionViewChannel)
	everyone := func(allow, deny int64) []*discordgo.PermissionOverwrite {
		return []*discordgo.PermissionOverwrite{{ID: guild, Type: discordgo.PermissionOverwriteTypeRole, Allow: allow, Deny: deny}}
	}
	cases := []struct {
		name  string
		perms int64
		ow    []*discordgo.PermissionOverwrite
		want  bool
	}{
		{"default public", view, nil, true},
		{"denied on channel", view, everyone(0, view), false},
		{"denied server-wide", 0, nil, false},
		{"allowed on channel", 0, everyone(view, 0), true},
		{"other role overwrite ignored", view, []*discordgo.PermissionOverwrite{{ID: "123", Type: discordgo.PermissionOverwriteTypeRole, Deny: view}}, true},
		{"administrator", discordgo.PermissionAdministrator, everyone(0, view), true},
	}
	for _, c := range cases {
		if got := EveryoneCanView(guild, c.perms, c.ow); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

func TestPublicChannelWarningOnce(t *testing.T) {
	e := newEnv(t)
	e.notify.WarnPublicChannel(context.Background())
	e.notify.WarnPublicChannel(context.Background())
	if e.gw.SentCount() != 1 {
		t.Fatalf("warnings = %d, want 1", e.gw.SentCount())
	}
}

func TestNoticesText(t *testing.T) {
	e := newEnv(t)
	e.notify.ClockSkew(context.Background(), agentA, "host-3", -10*time.Minute)
	if s := text(Response{Message: e.gw.Last().Message}); !strings.Contains(s, "10m0s behind") || !strings.Contains(s, "host-3") {
		t.Errorf("clock skew notice:\n%s", s)
	}
	h := store.Host{DisplayName: "host_one", LastSeenAt: t0}
	e.notify.HostOffline(context.Background(), h)
	if s := text(Response{Message: e.gw.Last().Message}); !strings.Contains(s, `host\_one`) || !strings.Contains(s, "<t:") {
		t.Errorf("offline notice:\n%s", s)
	}

	failing := []store.Container{
		{DisplayName: "immich-server", StackName: "immich", HostName: "241", CheckFailingSince: t0,
			LastCheckError: "GET https://ghcr.io/token: DENIED: requested access to the resource is denied"},
		{DisplayName: "web", StackName: "app", HostName: "242", CheckFailingSince: t0, LastCheckError: "not found in registry"},
	}
	e.notify.ChecksFailing(context.Background(), failing)
	if s := text(Response{Message: e.gw.Last().Message}); !strings.Contains(s, "failing · 2 images") ||
		!strings.Contains(s, "**immich-server** · immich · 241") || !strings.Contains(s, "GHCR_TOKEN") {
		t.Errorf("failing checks notice:\n%s", s)
	}
	e.notify.ChecksFailing(context.Background(), failing[1:])
	if s := text(Response{Message: e.gw.Last().Message}); !strings.Contains(s, "failing · 1 image") || strings.Contains(s, "GHCR_TOKEN") {
		t.Errorf("failing checks notice without a refused login:\n%s", s)
	}
	e.notify.ChecksRecovered(context.Background(), failing[:1])
	if s := text(Response{Message: e.gw.Last().Message}); !strings.Contains(s, "working again · 1 image") {
		t.Errorf("recovered notice:\n%s", s)
	}

	e.notify.AgentVersionDiffers(context.Background(), store.Host{DisplayName: "241", AgentVersion: "0.9.7"}, "0.9.10")
	if s := text(Response{Message: e.gw.Last().Message}); !strings.Contains(s, "differs · 241") || !strings.Contains(s, "image tag to `0.9.10`") {
		t.Errorf("older agent notice:\n%s", s)
	}
	e.notify.AgentVersionDiffers(context.Background(), store.Host{DisplayName: "241", AgentVersion: "0.10.0"}, "0.9.10")
	if s := text(Response{Message: e.gw.Last().Message}); !strings.Contains(s, "newer than the bot") {
		t.Errorf("newer agent notice:\n%s", s)
	}
}

func TestNotifierQueue(t *testing.T) {
	e := newEnv(t)
	n := NewNotifier(Notifier{Gateway: e.gw, Store: e.db, ChannelID: channel, Log: e.router.Log, Now: e.router.Now}, 4)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { n.Run(ctx); close(done) }()
	n.HostOnline(context.Background(), store.Host{DisplayName: "a"})
	n.HostOnline(context.Background(), store.Host{DisplayName: "b"})
	cancel()
	<-done
	if e.gw.SentCount() != 2 {
		t.Fatalf("sent = %d, want 2 (queued notices drain on shutdown)", e.gw.SentCount())
	}
}

func TestCommandDefinitions(t *testing.T) {
	for _, c := range Commands() {
		if c.DefaultMemberPermissions == nil || *c.DefaultMemberPermissions != 0 {
			t.Errorf("/%s must be hidden by default (default_member_permissions 0)", c.Name)
		}
		b, _ := json.Marshal(c)
		if !strings.Contains(string(b), `"default_member_permissions":"0"`) {
			t.Errorf("/%s marshals without default_member_permissions \"0\": %s", c.Name, b)
		}
		for _, sub := range c.Options {
			if len(sub.Description) > 100 || len(sub.Options) > 25 {
				t.Errorf("/%s %s exceeds Discord limits", c.Name, sub.Name)
			}
		}
	}
}

func TestToInteraction(t *testing.T) {
	raw := `{"type":2,"guild_id":"` + guild + `","channel_id":"` + channel + `",
		"member":{"nick":"Drew","user":{"id":"` + ownerID + `","username":"drew"}},
		"data":{"name":"host","type":1,"options":[{"name":"rename","type":1,"options":[
			{"name":"host","type":3,"value":"` + agentA + `"},{"name":"name","type":3,"value":"NAS"}]}]}}`
	var i discordgo.Interaction
	if err := json.Unmarshal([]byte(raw), &i); err != nil {
		t.Fatal(err)
	}
	it, ok := toInteraction(&i)
	if !ok || it.Kind != KindCommand || it.Command != "host" || it.Subcommand != "rename" ||
		it.Options["name"] != "NAS" || it.UserID != ownerID || it.UserName != "Drew" || it.GuildID != guild {
		t.Fatalf("command: %+v", it)
	}

	raw = `{"type":3,"guild_id":"` + guild + `","message":{"id":"m1"},"member":{"user":{"id":"` + ownerID + `","username":"drew"}},
		"data":{"custom_id":"v1:host_approve:` + agentA + `","component_type":2}}`
	i = discordgo.Interaction{}
	if err := json.Unmarshal([]byte(raw), &i); err != nil {
		t.Fatal(err)
	}
	it, ok = toInteraction(&i)
	if !ok || it.Kind != KindComponent || it.CustomID != "v1:host_approve:"+agentA || it.MessageID != "m1" {
		t.Fatalf("component: %+v", it)
	}

	resp := toDiscordResponse(ephemeral("x"))
	if resp.Data.Flags != discordgo.MessageFlagsEphemeral || resp.Data.AllowedMentions == nil {
		t.Fatalf("ephemeral response: %+v", resp.Data)
	}
}

// hasRow reports whether s has a line with these cells, in order,
// separated only by spaces (an aligned table row).
func hasRow(s string, cells ...string) bool {
	for _, line := range strings.Split(s, "\n") {
		if strings.Join(strings.Fields(line), " ") == strings.Join(cells, " ") {
			return true
		}
	}
	return false
}

// menuOf returns a message's first select menu.
func menuOf(t *testing.T, m Message) discordgo.SelectMenu {
	t.Helper()
	for _, row := range m.Components {
		if ar, ok := row.(discordgo.ActionsRow); ok {
			for _, c := range ar.Components {
				if menu, ok := c.(discordgo.SelectMenu); ok {
					return menu
				}
			}
		}
	}
	t.Fatalf("no select menu: %+v", m.Components)
	return discordgo.SelectMenu{}
}

// hostID is the only host's ID.
func (e *env) hostID(t *testing.T) string {
	t.Helper()
	hs, err := e.db.Hosts(context.Background(), false)
	if err != nil || len(hs) == 0 {
		t.Fatal("no host")
	}
	return hs[0].ID
}

// current is a message as it looks now: its latest edit, or as sent.
func (e *env) current(messageID string) SentMessage {
	for i := len(e.gw.Edits) - 1; i >= 0; i-- {
		if e.gw.Edits[i].MessageID == messageID {
			return e.gw.Edits[i]
		}
	}
	for _, s := range e.gw.Sent {
		if s.MessageID == messageID {
			return s
		}
	}
	return SentMessage{MessageID: messageID}
}

// opCard is the card an operation is shown on, as it looks now.
func (e *env) opCard(t *testing.T, opID string) SentMessage {
	t.Helper()
	msgs, err := e.db.DiscordMessages(context.Background(), entityOperation, opID)
	if err != nil || len(msgs) == 0 {
		t.Fatalf("operation %s has no card", opID)
	}
	return e.current(msgs[len(msgs)-1].MessageID)
}
