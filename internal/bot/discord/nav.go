package discord

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/DragonDrew/ddum/internal/bot/store"
	"github.com/DragonDrew/ddum/internal/ids"
)

// Navigation on summary cards: all hosts → one host → one stack or group,
// all on the same card. A summary lists each host's waiting stacks and
// groups; its menu opens a host (or, with one host, a stack or group).

// pendingViews loads every scope with unsnoozed pending updates, optionally
// on one host.
func pendingViews(ctx context.Context, db *store.Store, hostID string, now time.Time) ([]scopeView, error) {
	lines, err := loadLines(ctx, db, store.UpdateFilter{HostID: hostID}, now)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var views []scopeView
	for _, l := range lines {
		if l.U.Snoozed() || seen[scopeOf(l)] {
			continue
		}
		seen[scopeOf(l)] = true
		if v, err := loadAnyScope(ctx, db, scopeOf(l), now); err == nil && len(v.Lines) > 0 {
			views = append(views, v)
		}
	}
	sort.Slice(views, func(i, j int) bool {
		if views[i].Host.DisplayName != views[j].Host.DisplayName {
			return views[i].Host.DisplayName < views[j].Host.DisplayName
		}
		return views[i].Name() < views[j].Name()
	})
	return views, nil
}

// navMenu is the menu under a summary: hosts when there are several,
// otherwise the stacks and groups.
func navMenu(views []scopeView) []discordgo.MessageComponent {
	hosts := map[string]int{}
	var order []scopeView
	for _, v := range views {
		if hosts[v.Host.ID] == 0 {
			order = append(order, v)
		}
		hosts[v.Host.ID] += len(v.Lines)
	}
	if len(order) > 1 {
		var opts []discordgo.SelectMenuOption
		for _, v := range order {
			if len(opts) == maxChoices {
				break
			}
			opts = append(opts, discordgo.SelectMenuOption{Label: truncate(v.Host.DisplayName, maxChoiceName), Value: v.Host.ID,
				Emoji: &discordgo.ComponentEmoji{Name: "🖥️"}, Description: plural(hosts[v.Host.ID], "update") + " waiting"})
		}
		return []discordgo.MessageComponent{discordgo.ActionsRow{Components: []discordgo.MessageComponent{
			discordgo.SelectMenu{CustomID: customID(actHostPick), Placeholder: "Open a host…", Options: opts},
		}}}
	}
	return scopeMenu(views)
}

// scopeMenu picks one stack or group.
func scopeMenu(views []scopeView) []discordgo.MessageComponent {
	var opts []discordgo.SelectMenuOption
	for _, v := range views {
		if len(opts) == maxChoices {
			break
		}
		opts = append(opts, discordgo.SelectMenuOption{Label: truncate(v.Name(), maxChoiceName), Value: v.ScopeID(),
			Emoji:       &discordgo.ComponentEmoji{Name: stateIcon(v.Headline)},
			Description: truncate(plural(len(v.Lines), "update")+" · "+displayState(v.Headline), maxChoiceName)})
	}
	if len(opts) == 0 {
		return []discordgo.MessageComponent{}
	}
	return []discordgo.MessageComponent{discordgo.ActionsRow{Components: []discordgo.MessageComponent{
		discordgo.SelectMenu{CustomID: customID(actReviewPick), Placeholder: "Open a stack or group…", Options: opts},
	}}}
}

// allPending shows the summary of everything waiting on the card.
func (r *Router) allPending(ctx context.Context) Response {
	views, err := pendingViews(ctx, r.Store, "", r.Now())
	if err != nil {
		return r.internalError("list updates", err)
	}
	if len(views) == 0 {
		return updateCard(iconOK, "Up to date", "Nothing is waiting for approval.", colorOK)
	}
	return updateTo(summaryMessage("Waiting for approval", views, true))
}

// hostView shows one host's waiting stacks and groups on the card.
func (r *Router) hostView(ctx context.Context, hostID string) Response {
	if _, err := ids.Parse(hostID); err != nil {
		return ephemeral("Unknown host.")
	}
	h, err := r.Store.Host(ctx, hostID)
	if err != nil {
		return ephemeral("That host no longer exists.")
	}
	views, err := pendingViews(ctx, r.Store, hostID, r.Now())
	if err != nil {
		return r.internalError("list updates", err)
	}
	back := button("All hosts", "◀️", discordgo.SecondaryButton, customID(actReview))
	if len(views) == 0 {
		m := notice(iconOK, escape(h.DisplayName)+" is up to date", "Nothing is waiting for approval here.", colorOK)
		m.Components = buttonRow(back)
		return updateTo(m)
	}
	e := &discordgo.MessageEmbed{
		Title:       iconHost + " " + escape(h.DisplayName),
		Description: scopesTable(views, 3000),
		Color:       colorWarn,
		Footer:      footer("Pick one below to see it here"),
	}
	comps := scopeMenu(views)
	comps = append(comps, discordgo.ActionsRow{Components: []discordgo.MessageComponent{back}})
	return updateTo(Message{Embeds: []*discordgo.MessageEmbed{e}, Components: comps})
}

// openScope opens a stack or group picked from a summary, with a way back
// to its host.
func (r *Router) openScope(ctx context.Context, it Interaction, scopeID string) Response {
	resp := r.backToScope(ctx, it, scopeID, "")
	if resp.Kind != RespUpdate {
		return resp
	}
	if v, err := loadAnyScope(ctx, r.Store, scopeID, r.Now()); err == nil {
		resp.Message.Components = append(resp.Message.Components, discordgo.ActionsRow{Components: []discordgo.MessageComponent{
			button(v.Host.DisplayName, "◀️", discordgo.SecondaryButton, customID(actHostView, v.Host.ID)),
		}})
	}
	return resp
}

// Scheduling: Schedule on an update card asks for a time, then approves
// the same offer to start then.

// scheduleSlots are the next 24 whole hours in the bot's time zone.
func scheduleSlots(now time.Time, loc *time.Location) []time.Time {
	t := now.In(loc).Truncate(time.Hour).Add(time.Hour)
	if t.Sub(now) < 10*time.Minute { // too close to be worth scheduling
		t = t.Add(time.Hour)
	}
	slots := make([]time.Time, 0, 24)
	for i := 0; i < 24; i++ {
		slots = append(slots, t.Add(time.Duration(i)*time.Hour))
	}
	return slots
}

// slotLabel names a slot for the menu: "03:00 tomorrow".
func slotLabel(t, now time.Time, loc *time.Location) string {
	t, now = t.In(loc), now.In(loc)
	day := "today"
	if y, m, d := t.Date(); y != now.Year() || m != now.Month() || d != now.Day() {
		day = "tomorrow"
	}
	return t.Format("15:04") + " " + day
}

func (r *Router) location() *time.Location {
	if r.Location != nil {
		return r.Location
	}
	return time.Local
}

// scheduleButton turns the card into a time picker for an offer.
func (r *Router) scheduleButton(ctx context.Context, offerID string) Response {
	offer, err := r.Store.GetOffer(ctx, offerID)
	if err != nil {
		return ephemeral("This button is no longer valid. Use /update list.")
	}
	now, loc := r.Now(), r.location()
	var opts []discordgo.SelectMenuOption
	for _, t := range scheduleSlots(now, loc) {
		opts = append(opts, discordgo.SelectMenuOption{Label: slotLabel(t, now, loc), Value: strconv.FormatInt(t.Unix(), 10),
			Description: "in " + roundDuration(t.Sub(now))})
	}
	zone, _ := now.In(loc).Zone()
	m := notice(iconClock, "When should it deploy?", fmt.Sprintf("The approved %s will deploy at the time you pick (%s). "+
		"Until then you can cancel it.", plural(len(offer.Items), "update"), zone), colorInfo)
	m.Components = []discordgo.MessageComponent{
		discordgo.ActionsRow{Components: []discordgo.MessageComponent{
			discordgo.SelectMenu{CustomID: customID(actSchedulePick, offerID), Placeholder: "Pick a time…", Options: opts},
		}},
		discordgo.ActionsRow{Components: []discordgo.MessageComponent{
			button("Back", "", discordgo.SecondaryButton, customID(actBack, offer.ScopeID)),
		}},
	}
	return updateTo(m)
}

// schedulePicked approves the offer to start at the picked time.
func (r *Router) schedulePicked(ctx context.Context, it Interaction, offerID string) Response {
	if len(it.Values) != 1 {
		return ephemeral("Pick one time.")
	}
	sec, err := strconv.ParseInt(it.Values[0], 10, 64)
	if err != nil {
		return ephemeral("Unknown time.")
	}
	at := time.Unix(sec, 0).UTC()
	if !at.After(r.Now()) || at.Sub(r.Now()) > 48*time.Hour {
		return ephemeral("That time has passed. Press Schedule again for fresh times.")
	}
	return r.approveAt(ctx, it, offerID, at)
}

// roundDuration renders "5h" or "40m".
func roundDuration(d time.Duration) string {
	if d >= time.Hour {
		return fmt.Sprintf("%dh", int(d.Round(time.Hour)/time.Hour))
	}
	return fmt.Sprintf("%dm", int(d.Round(time.Minute)/time.Minute))
}

// discordTime renders t in each viewer's own time zone.
func discordTime(t time.Time) string {
	return fmt.Sprintf("<t:%d:f> (<t:%d:R>)", t.Unix(), t.Unix())
}
