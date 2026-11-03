package discord

import (
	"fmt"
	"sort"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/DragonDrew/ddum/internal/bot/domain"
	"github.com/DragonDrew/ddum/internal/bot/store"
)

func stateColor(state string) int {
	switch state {
	case domain.StateHeld:
		return colorProblem
	case domain.StateSnoozed:
		return colorMuted
	case domain.StateAutomatic, domain.StateDeploying:
		return colorInfo
	}
	return colorWarn
}

// displayState renders a state for people; "Automatic" says honestly what
// happens next.
func displayState(state string) string {
	if state == domain.StateAutomatic {
		return "Automatic: deploys on its own"
	}
	return stateLabel(state)
}

// scopeMessage renders a stack's or group's update message. offerID backs
// its buttons; empty means no buttons.
func scopeMessage(v scopeView, offerID string) Message {
	var latest time.Time
	for _, l := range v.Lines {
		if l.U.DetectedAt.After(latest) {
			latest = l.U.DetectedAt
		}
	}
	e := &discordgo.MessageEmbed{
		Title:     iconUpdates + " Updates for " + escape(v.Name()),
		Author:    author(v.Host.DisplayName, v.Stack.Provider),
		Color:     stateColor(v.Headline),
		Timestamp: stamp(latest),
	}
	headline := stateIcon(v.Headline) + " **" + displayState(v.Headline) + "**"
	if len(v.Lines) > 0 {
		headline += " · " + plural(len(v.Lines), "update")
	}
	e.Description = headline
	if v.Kind == "group" {
		e.Author = author(v.Host.DisplayName, "")
		e.Footer = footer("Members deploy and roll back together")
		if v.HeldReason != "" {
			e.Description += "\n" + iconHeld + " " + v.HeldReason
			if v.ManualOK {
				e.Description += "\nYou can still approve the whole group."
			}
		}
	}
	if len(v.Lines) == 0 {
		e.Description = iconOK + " Nothing pending. Everything here is up to date or was handled."
		e.Color = colorOK
		return Message{Embeds: []*discordgo.MessageEmbed{e}, Components: []discordgo.MessageComponent{}}
	}
	e.Description += "\n" + updatesTable(v, 1800)
	if notes := lineNotes(v); len(notes) > 0 {
		e.Description += "\n" + lines(notes, 1500, "/update show")
	}
	if links := releaseLinks(v); links != "" {
		e.Description += "\n" + truncate(links, 600)
	}
	m := Message{Embeds: []*discordgo.MessageEmbed{e}, Components: []discordgo.MessageComponent{}}
	if offerID == "" {
		return m
	}
	row := discordgo.ActionsRow{}
	if len(v.offerLines()) > 0 {
		row.Components = append(row.Components,
			discordgo.Button{Label: "Approve", Style: discordgo.SuccessButton, Emoji: &discordgo.ComponentEmoji{Name: "✅"},
				CustomID: customID(actApprove, offerID), Disabled: !v.approvable()},
			discordgo.Button{Label: "Schedule", Style: discordgo.PrimaryButton, Emoji: &discordgo.ComponentEmoji{Name: "⏰"},
				CustomID: customID(actSchedule, offerID), Disabled: !v.approvable()})
	}
	if len(v.snoozable()) > 0 {
		row.Components = append(row.Components, discordgo.Button{Label: "Snooze", Style: discordgo.SecondaryButton,
			Emoji: &discordgo.ComponentEmoji{Name: "💤"}, CustomID: customID(actSnooze, offerID)})
	}
	if len(row.Components) > 0 {
		m.Components = []discordgo.MessageComponent{row}
	}
	return m
}

// summaryMessage groups many scopes by host: one field per host
// listing its waiting stacks and groups. With nav, a menu opens a host (or,
// with one host, a stack or group) on the same card.
func summaryMessage(title string, views []scopeView, nav bool) Message {
	sort.Slice(views, func(i, j int) bool {
		if views[i].Host.DisplayName != views[j].Host.DisplayName {
			return views[i].Host.DisplayName < views[j].Host.DisplayName
		}
		return views[i].Name() < views[j].Name()
	})
	byHost := map[string][]scopeView{}
	var hosts []string
	for _, v := range views {
		h := v.Host.DisplayName
		if _, ok := byHost[h]; !ok {
			hosts = append(hosts, h)
		}
		byHost[h] = append(byHost[h], v)
	}
	e := &discordgo.MessageEmbed{Title: iconUpdates + " " + title, Color: colorWarn}
	if nav {
		e.Footer = footer("Pick one below to see it here")
	}
	for i, h := range hosts {
		if i == 24 {
			e.Fields = append(e.Fields, field("…", "more hosts · /update list", false))
			break
		}
		e.Fields = append(e.Fields, field(iconHost+" "+escape(h), scopesTable(byHost[h], maxFieldValue), false))
	}
	m := Message{Embeds: []*discordgo.MessageEmbed{e}, Components: []discordgo.MessageComponent{}}
	if nav {
		m.Components = navMenu(views)
	}
	return m
}

// mergeSuggestionMessage proposes /stack merge for a renamed stack.
func mergeSuggestionMessage(newer, missing store.Stack, matched, total int) Message {
	e := &discordgo.MessageEmbed{
		Title:  iconDrift + " Was " + escape(missing.DisplayName) + " renamed?",
		Author: author(newer.HostName, newer.Provider),
		Description: fmt.Sprintf("New stack **%s** shares %d of %d services with **%s**, which went missing %s.\n"+
			"If it is the same app, merge them so it keeps its policies, groups, and history.",
			escape(newer.DisplayName), matched, total, escape(missing.DisplayName), relTime(missing.MissingSince)),
		Color:  colorInfo,
		Footer: footer("Nothing changes unless you merge"),
	}
	return Message{Embeds: []*discordgo.MessageEmbed{e}, Components: []discordgo.MessageComponent{discordgo.ActionsRow{
		Components: []discordgo.MessageComponent{discordgo.Button{Label: "Merge…", Style: discordgo.PrimaryButton,
			CustomID: customID(actMergeAsk, newer.ID, missing.ID)}},
	}}}
}
