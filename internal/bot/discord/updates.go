package discord

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/DragonDrew/ddum/internal/bot/domain"
	"github.com/DragonDrew/ddum/internal/bot/store"
	"github.com/DragonDrew/ddum/internal/ids"
)

// ErrCheckBusy is what StartCheck returns when a check is already running.
var ErrCheckBusy = errors.New("a registry check is already running")

// snoozeUntil maps a duration choice to an end time; zero is indefinite.
func snoozeUntil(choice string, now time.Time) (time.Time, bool) {
	switch choice {
	case "1 day":
		return now.Add(24 * time.Hour), true
	case "1 week":
		return now.Add(7 * 24 * time.Hour), true
	case "1 month":
		return now.Add(30 * 24 * time.Hour), true
	case "indefinitely":
		return time.Time{}, true
	}
	return time.Time{}, false
}

func describeUntil(until time.Time) string {
	if until.IsZero() {
		return "indefinitely (for these updates only)"
	}
	return "until " + relTime(until)
}

func (r *Router) resolveUpdate(ctx context.Context, value string) (store.Update, *Response) {
	id, err := ids.Parse(value)
	if err == nil {
		if u, err := r.Store.GetUpdate(ctx, id, r.Now()); err == nil {
			return u, nil
		}
	}
	resp := ephemeral("No update matches " + code(value) + ". Pick one from the list as you type.")
	return store.Update{}, &resp
}

// scopeFilter reads the optional host and stack options.
func (r *Router) scopeFilter(ctx context.Context, it Interaction) (hostID, stackID, label string, errResp *Response) {
	label = "all hosts"
	if v := it.Options[optHost]; v != "" {
		h, er := r.resolveHost(ctx, v)
		if er != nil {
			return "", "", "", er
		}
		hostID, label = h.ID, "host **"+escape(h.DisplayName)+"**"
	}
	if v := it.Options[optStack]; v != "" {
		s, er := r.resolveStack(ctx, v)
		if er != nil {
			return "", "", "", er
		}
		stackID, label = s.ID, "stack **"+escape(s.DisplayName)+"**"
	}
	return hostID, stackID, label, nil
}

func (r *Router) updateList(ctx context.Context, it Interaction) Response {
	hostID, stackID, label, errResp := r.scopeFilter(ctx, it)
	if errResp != nil {
		return *errResp
	}
	pending, err := loadLines(ctx, r.Store, store.UpdateFilter{HostID: hostID, StackID: stackID}, r.Now())
	if err != nil {
		return r.internalError("list updates", err)
	}
	withSnoozed := it.Options[optIncludeSnoozed] == "true"
	var ls []string
	lastStack, shown, hidden := "", 0, 0
	for _, l := range pending {
		if l.U.Snoozed() && !withSnoozed {
			hidden++
			continue
		}
		if l.U.StackID != lastStack {
			ls = append(ls, fmt.Sprintf("\n%s **%s** · %s", iconUpdates, escape(l.U.StackName), escape(l.U.HostName)))
			lastStack = l.U.StackID
		}
		ls = append(ls, fmt.Sprintf("%s %s %s", stateIcon(l.State), escape(l.C.DisplayName),
			change(l.U.CurrentRef, l.U.CurrentDigest, l.U.TargetRef, l.U.TargetDigest)))
		shown++
	}
	if shown == 0 {
		msg := "Nothing is waiting for approval."
		if hidden > 0 {
			msg += fmt.Sprintf(" %s hidden; add include_snoozed to see them.", plural(hidden, "snoozed update"))
		}
		return replyCard(iconOK, "Up to date · "+strings.ReplaceAll(label, "**", ""), msg, colorOK)
	}
	desc := lines(ls, maxDescription, "Narrow it with the host or stack option.")
	e := &discordgo.MessageEmbed{Title: fmt.Sprintf("%s Pending updates · %s", iconUpdates, strings.ReplaceAll(label, "**", "")),
		Description: desc, Color: colorWarn}
	if hidden > 0 {
		e.Footer = footer(plural(hidden, "snoozed update") + " hidden")
	}
	return reply(Message{Embeds: []*discordgo.MessageEmbed{e}})
}

func (r *Router) updateShow(ctx context.Context, it Interaction) Response {
	u, errResp := r.resolveUpdate(ctx, it.Options[optUpdate])
	if errResp != nil {
		return *errResp
	}
	c, err := r.Store.Container(ctx, u.ContainerID)
	if err != nil {
		return r.internalError("load container", err)
	}
	policies, err := r.Store.Policies(ctx)
	if err != nil {
		return r.internalError("load policies", err)
	}
	l := lineFor(u, c, policies)
	state := displayState(l.State)
	if u.Status != store.StatusDetected {
		state = u.Status
	}
	e := &discordgo.MessageEmbed{
		Title:       stateIcon(l.State) + " " + escape(c.DisplayName) + " · " + escape(u.StackName),
		Author:      author(u.HostName, ""),
		Description: change(u.CurrentRef, u.CurrentDigest, u.TargetRef, u.TargetDigest) + "\n**" + state + "**",
		Color:       stateColor(l.State),
		Fields: []*discordgo.MessageEmbedField{
			field("Kind", u.Kind, true),
			field("Status", u.Status, true),
			field("Detected", relTime(u.DetectedAt), true),
			field("Current", code(familiar(u.CurrentRef))+"\n"+code(u.CurrentDigest), false),
			field("Target", code(familiar(u.TargetRef))+"\n"+code(u.TargetDigest), false),
			field("Policy", fmt.Sprintf("%s (set at %s), tag_track %s (set at %s)", l.Policy.Mode, l.Policy.Source[domain.FieldMode],
				l.Policy.TagTrack, l.Policy.Source[domain.FieldTagTrack]), false),
		},
	}
	if u.ReleaseURL != "" {
		e.Fields = append(e.Fields, field("Release notes", u.ReleaseURL, false))
	}
	if l.Reason != "" {
		e.Fields = append(e.Fields, field("Held", l.Reason, false))
	}
	if u.Snoozed() {
		e.Fields = append(e.Fields, field("Snoozed", describeUntil(u.SnoozeUntil), false))
	}
	m := Message{Embeds: []*discordgo.MessageEmbed{e}}
	if u.Status == store.StatusDetected && !u.Snoozed() {
		offer, err := r.Store.CreateOffer(ctx, "stack", u.StackID, []store.OfferItem{{UpdateID: u.ID, TargetDigest: u.TargetDigest}}, "command", r.Now())
		if err != nil {
			return r.internalError("create offer", err)
		}
		m.Components = []discordgo.MessageComponent{discordgo.ActionsRow{Components: []discordgo.MessageComponent{
			discordgo.Button{Label: "Approve", Style: discordgo.SuccessButton, Emoji: &discordgo.ComponentEmoji{Name: "✅"},
				CustomID: customID(actApprove, offer), Disabled: l.Reason != ""},
			discordgo.Button{Label: "Schedule", Style: discordgo.PrimaryButton, Emoji: &discordgo.ComponentEmoji{Name: "⏰"},
				CustomID: customID(actSchedule, offer), Disabled: l.Reason != ""},
			discordgo.Button{Label: "Snooze", Style: discordgo.SecondaryButton, Emoji: &discordgo.ComponentEmoji{Name: "💤"}, CustomID: customID(actSnooze, offer)},
		}}}
	}
	return reply(m)
}

func (r *Router) updateCheck(ctx context.Context, it Interaction) Response {
	hostID, stackID, label, errResp := r.scopeFilter(ctx, it)
	if errResp != nil {
		return *errResp
	}
	if r.StartCheck == nil {
		return ephemeral("Registry checks are not available.")
	}
	plain := strings.ReplaceAll(label, "**", "")
	checkID := ids.V7{}.New()
	live := it.ChannelID == r.ChannelID // a private reply cannot be edited later
	var done func(CheckSummary)
	if live {
		r.checks.start(checkID, hostID, stackID, plain)
		done = func(sum CheckSummary) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			r.checks.finished(ctx, r, checkID, sum)
		}
	}
	if err := r.StartCheck(hostID, stackID, done); errors.Is(err, ErrCheckBusy) {
		r.checks.drop(checkID)
		return ephemeral("A registry check is already running. New updates are posted in <#" + r.ChannelID + "> when it finishes.")
	} else if err != nil {
		r.checks.drop(checkID)
		return r.internalError("start check", err)
	}
	desc := "Asking the registries about every tracked image. This card shows what is waiting for approval when it finishes."
	if !live {
		desc = "New updates are posted in <#" + r.ChannelID + "> when it finishes."
	}
	resp := replyCard("🔎", "Checking "+plain+"…", desc, colorInfo)
	resp.Bind = &Binding{Entity: entityCheck, ID: checkID}
	return resp
}

func (r *Router) updateSnooze(ctx context.Context, it Interaction) Response {
	u, errResp := r.resolveUpdate(ctx, it.Options[optUpdate])
	if errResp != nil {
		return *errResp
	}
	until, ok := snoozeUntil(it.Options[optDuration], r.Now())
	if !ok {
		return ephemeral("Pick a duration from the list.")
	}
	done, err := r.Store.SnoozeUpdates(ctx, []string{u.ID}, until, r.actor(it), r.Now())
	if err != nil {
		return r.internalError("snooze", err)
	}
	if len(done) == 0 {
		return ephemeral("That update is no longer pending (" + u.Status + "), so there is nothing to snooze.")
	}
	r.refresh(ctx, u.StackID)
	return replyCard(iconSnoozed, "Snoozed · "+escape(u.ContainerName), fmt.Sprintf("%s in **%s**, %s.",
		change(u.CurrentRef, u.CurrentDigest, u.TargetRef, u.TargetDigest), escape(u.StackName), describeUntil(until)), colorMuted)
}

func (r *Router) updateSnoozed(ctx context.Context) Response {
	pending, err := loadLines(ctx, r.Store, store.UpdateFilter{}, r.Now())
	if err != nil {
		return r.internalError("list updates", err)
	}
	var ls []string
	for _, l := range pending {
		if l.U.Snoozed() {
			ls = append(ls, fmt.Sprintf("• **%s** (%s on %s) %s → %s · %s", escape(l.C.DisplayName), escape(l.U.StackName),
				escape(l.U.HostName), code(tagOf(l.U.CurrentRef)), code(tagOf(l.U.TargetRef)), describeUntil(l.U.SnoozeUntil)))
		}
	}
	if len(ls) == 0 {
		return replyCard(iconSnoozed, "Nothing snoozed", "Every pending update is shown in its stack's card.", colorMuted)
	}
	return reply(Message{Embeds: []*discordgo.MessageEmbed{{
		Title: iconSnoozed + " Snoozed updates · " + fmt.Sprint(len(ls)), Description: lines(ls, maxDescription, ""), Color: colorMuted,
	}}})
}

func (r *Router) updateUnsnooze(ctx context.Context, it Interaction) Response {
	u, errResp := r.resolveUpdate(ctx, it.Options[optUpdate])
	if errResp != nil {
		return *errResp
	}
	ok, err := r.Store.Unsnooze(ctx, u.ID, r.actor(it), r.Now())
	if err != nil {
		return r.internalError("unsnooze", err)
	}
	if !ok {
		return ephemeral("That update is not snoozed.")
	}
	r.refresh(ctx, u.StackID)
	return replyCard(iconPending, "Snooze removed · "+escape(u.ContainerName), fmt.Sprintf("%s in **%s** is pending again.",
		change(u.CurrentRef, u.CurrentDigest, u.TargetRef, u.TargetDigest), escape(u.StackName)), colorWarn)
}

func (r *Router) updateUnsnoozeAll(ctx context.Context) Response {
	lines, err := loadLines(ctx, r.Store, store.UpdateFilter{}, r.Now())
	if err != nil {
		return r.internalError("list updates", err)
	}
	n := 0
	for _, l := range lines {
		if l.U.Snoozed() {
			n++
		}
	}
	if n == 0 {
		return replyCard(iconSnoozed, "Nothing snoozed", "There is nothing to unsnooze.", colorMuted)
	}
	m := notice(iconSnoozed, "Unsnooze everything?", fmt.Sprintf("All %s show as pending again.", plural(n, "snoozed update")), colorWarn)
	m.Components = buttonRow(button("Unsnooze all", "", discordgo.DangerButton, customID(actUnsnoozeAll)),
		button("Cancel", "", discordgo.SecondaryButton, customID(actCancel)))
	return reply(m)
}

func (r *Router) unsnoozeAllConfirmed(ctx context.Context, it Interaction) Response {
	before, _ := loadLines(ctx, r.Store, store.UpdateFilter{}, r.Now())
	n, err := r.Store.UnsnoozeAll(ctx, r.actor(it), r.Now())
	if err != nil {
		return r.internalError("unsnooze all", err)
	}
	stacks := map[string]bool{}
	for _, l := range before {
		if l.U.Snoozed() && !stacks[l.U.StackID] {
			stacks[l.U.StackID] = true
			r.refresh(ctx, l.U.StackID)
		}
	}
	return updateCard(iconPending, "Unsnoozed", fmt.Sprintf("Removed %s. Those updates are pending again.", plural(n, "snooze")), colorWarn)
}

// snoozeButton turns the card into a duration picker for an offer.
func (r *Router) snoozeButton(ctx context.Context, offerID string) Response {
	offer, err := r.Store.GetOffer(ctx, offerID)
	if err != nil {
		return ephemeral("This button is no longer valid. Use /update list.")
	}
	var opts []discordgo.SelectMenuOption
	for _, c := range snoozeChoices {
		opts = append(opts, discordgo.SelectMenuOption{Label: c, Value: c})
	}
	m := notice(iconSnoozed, "Snooze for how long?", fmt.Sprintf("%s on this card. A newer update is still announced.",
		plural(len(offer.Items), "update")), colorMuted)
	m.Components = []discordgo.MessageComponent{
		discordgo.ActionsRow{Components: []discordgo.MessageComponent{
			discordgo.SelectMenu{CustomID: customID(actSnoozePick, offerID), Placeholder: "Duration", Options: opts},
		}},
		discordgo.ActionsRow{Components: []discordgo.MessageComponent{
			button("Back", "", discordgo.SecondaryButton, customID(actBack, offer.ScopeID)),
		}},
	}
	return updateTo(m)
}

// backToScope shows a stack's or group's current updates on the card.
func (r *Router) backToScope(ctx context.Context, it Interaction, scopeID, note string) Response {
	if _, err := ids.Parse(scopeID); err != nil {
		return ephemeral("This button is malformed.")
	}
	v, err := loadAnyScope(ctx, r.Store, scopeID, r.Now())
	if err != nil {
		return updateCard("✖️", "Gone", "That stack or group no longer exists.", colorMuted)
	}
	offer, err := offerFor(ctx, r.Store, v, "command", r.Now())
	if err != nil {
		return r.internalError("create offer", err)
	}
	r.claim(ctx, it, scopeEntity(v.Kind), scopeID)
	m := scopeMessage(v, offer)
	if note != "" {
		m.Embeds[0].Description = note + "\n" + m.Embeds[0].Description
	}
	return updateTo(m)
}

// snoozePicked snoozes the offer's updates that are still exactly as shown.
func (r *Router) snoozePicked(ctx context.Context, it Interaction, offerID string) Response {
	offer, err := r.Store.GetOffer(ctx, offerID)
	if err != nil {
		return ephemeral("This menu is no longer valid. Use /update list.")
	}
	if len(it.Values) != 1 {
		return ephemeral("Pick one duration.")
	}
	until, ok := snoozeUntil(it.Values[0], r.Now())
	if !ok {
		return ephemeral("Unknown duration.")
	}
	var ids []string
	for _, item := range offer.Items {
		u, err := r.Store.GetUpdate(ctx, item.UpdateID, r.Now())
		if err == nil && u.Status == store.StatusDetected && u.TargetDigest == item.TargetDigest {
			ids = append(ids, u.ID)
		}
	}
	done, err := r.Store.SnoozeUpdates(ctx, ids, until, r.actor(it), r.Now())
	if err != nil {
		return r.internalError("snooze", err)
	}
	note := iconSnoozed + " Nothing to snooze: those updates are no longer pending."
	if len(done) > 0 {
		note = fmt.Sprintf("%s <@%s> snoozed %s %s.", iconSnoozed, it.UserID, plural(len(done), "update"), describeUntil(until))
	}
	resp := r.backToScope(ctx, it, offer.ScopeID, note)
	r.refresh(ctx, offer.ScopeID)
	return resp
}

// refresh edits a stack's stored update message after a change.
func (r *Router) refresh(ctx context.Context, stackID string) {
	if r.Notify != nil {
		r.Notify.RefreshStacks(ctx, []string{stackID})
	}
}
