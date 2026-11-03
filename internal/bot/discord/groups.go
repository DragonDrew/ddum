package discord

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/bwmarrin/discordgo"

	"github.com/DragonDrew/ddum/internal/bot/store"
	"github.com/DragonDrew/ddum/internal/ids"
)

func (r *Router) resolveGroup(ctx context.Context, value string) (store.Group, *Response) {
	if id, err := ids.Parse(value); err == nil {
		if g, err := r.Store.GetGroup(ctx, id); err == nil {
			return g, nil
		}
	}
	groups, err := r.Store.Groups(ctx)
	if err != nil {
		resp := r.internalError("list groups", err)
		return store.Group{}, &resp
	}
	var match []store.Group
	for _, g := range groups {
		if strings.EqualFold(g.DisplayName, value) {
			match = append(match, g)
		}
	}
	if len(match) == 1 {
		return match[0], nil
	}
	resp := ephemeral("No single group matches " + code(value) + ". Pick one from the list as you type.")
	return store.Group{}, &resp
}

func (r *Router) groupList(ctx context.Context) Response {
	groups, err := r.Store.Groups(ctx)
	if err != nil {
		return r.internalError("list groups", err)
	}
	if len(groups) == 0 {
		return replyCard("🔗", "No update groups yet", "Create one with /group create, or label containers `ddum.group=<name>`.", colorMuted)
	}
	var ls []string
	for _, g := range groups {
		src := ""
		if g.Source == "label" {
			src = " · from label"
		}
		ls = append(ls, fmt.Sprintf("**%s** · %d members%s", escape(g.DisplayName), g.Members, src))
	}
	return reply(Message{Embeds: []*discordgo.MessageEmbed{{
		Title: fmt.Sprintf("🔗 Update groups · %d", len(groups)), Description: lines(ls, maxDescription, ""), Color: colorInfo,
		Footer: footer("Members of a group are deployed and rolled back together"),
	}}})
}

func (r *Router) groupShow(ctx context.Context, it Interaction) Response {
	g, errResp := r.resolveGroup(ctx, it.Options[optGroup])
	if errResp != nil {
		return *errResp
	}
	members, err := r.Store.GroupMembers(ctx, g.ID)
	if err != nil {
		return r.internalError("list members", err)
	}
	var ls []string
	hosts := map[string]bool{}
	for _, c := range members {
		hosts[c.HostID] = true
		l := fmt.Sprintf("• **%s** in %s on %s · %s", escape(c.DisplayName), escape(c.StackName), escape(c.HostName), code(c.ImageRef))
		if c.Presence != "present" {
			l += " · *missing*"
		}
		ls = append(ls, l)
	}
	e := &discordgo.MessageEmbed{Title: "🔗 Group " + escape(g.DisplayName), Color: colorInfo,
		Description: fmt.Sprintf("Created %s (%s)", relTime(g.CreatedAt), g.Source),
		Fields: []*discordgo.MessageEmbedField{
			field(fmt.Sprintf("Members (%d)", len(members)), lines(ls, maxFieldValue, ""), false),
		}}
	if len(hosts) > 1 {
		e.Color = colorProblem
		e.Fields = append(e.Fields, field("Held", "group_spans_hosts: its members are on more than one host, so it cannot be deployed", false))
	}
	if v, err := loadGroupScope(ctx, r.Store, g.ID, r.Now()); err == nil && len(v.Lines) > 0 {
		e.Fields = append(e.Fields, field("Pending updates", fmt.Sprintf("%d · **%s**. See /update list.", len(v.Lines), displayState(v.Headline)), false))
		if v.HeldReason != "" {
			e.Fields = append(e.Fields, field("Held", v.HeldReason, false))
		}
	}
	return reply(Message{Embeds: []*discordgo.MessageEmbed{e}})
}

func (r *Router) groupCreate(ctx context.Context, it Interaction) Response {
	name := strings.TrimSpace(it.Options[optName])
	if name == "" {
		return ephemeral("The name cannot be empty.")
	}
	g, err := r.Store.CreateGroup(ctx, name, r.actor(it), r.Now())
	if err != nil {
		return r.internalError("create group", err)
	}
	return replyCard("🔗", "Group created · "+escape(g.DisplayName), "Add containers with /group add.", colorOK)
}

func (r *Router) groupRename(ctx context.Context, it Interaction) Response {
	g, errResp := r.resolveGroup(ctx, it.Options[optGroup])
	if errResp != nil {
		return *errResp
	}
	name := strings.TrimSpace(it.Options[optName])
	if name == "" {
		return ephemeral("The name cannot be empty.")
	}
	if _, err := r.Store.RenameGroup(ctx, g.ID, name, r.actor(it), r.Now()); err != nil {
		return r.internalError("rename group", err)
	}
	return replyCard("🔗", "Group renamed", fmt.Sprintf("**%s** is now **%s**.", escape(g.DisplayName), escape(name)), colorOK)
}

func (r *Router) groupDelete(ctx context.Context, it Interaction) Response {
	g, errResp := r.resolveGroup(ctx, it.Options[optGroup])
	if errResp != nil {
		return *errResp
	}
	m := notice("🔗", "Delete group "+escape(g.DisplayName)+"?", fmt.Sprintf("Its %s go back to being updated with their stacks, "+
		"and its group policy is removed. Labelled members are not re-added by their label.", plural(g.Members, "member")), colorWarn)
	m.Components = buttonRow(button("Delete group", "", discordgo.DangerButton, customID(actGroupDelete, g.ID)),
		button("Cancel", "", discordgo.SecondaryButton, customID(actCancel)))
	return reply(m)
}

func (r *Router) groupDeleteConfirmed(ctx context.Context, it Interaction, id string) Response {
	g, err := r.Store.DeleteGroup(ctx, id, r.actor(it), r.Now())
	if errors.Is(err, store.ErrNotFound) {
		return updateCard("🔗", "Already gone", "That group no longer exists.", colorMuted)
	}
	if err != nil {
		return r.internalError("delete group", err)
	}
	return updateCard("🔗", "Group deleted · "+escape(g.DisplayName), "Deleted by <@"+it.UserID+">.", colorMuted)
}

func (r *Router) groupAdd(ctx context.Context, it Interaction) Response {
	g, errResp := r.resolveGroup(ctx, it.Options[optGroup])
	if errResp != nil {
		return *errResp
	}
	c, errResp := r.resolveContainer(ctx, it.Options[optContainer])
	if errResp != nil {
		return *errResp
	}
	err := r.Store.AddMember(ctx, g.ID, c.ID, r.actor(it), r.Now())
	switch {
	case errors.Is(err, store.ErrInOtherGroup):
		return ephemeral(fmt.Sprintf("**%s** is already in group **%s**. A container belongs to one group; remove it there first.",
			escape(c.DisplayName), escape(c.GroupName)))
	case err != nil:
		return r.internalError("add member", err)
	}
	msg := fmt.Sprintf("**%s** (%s on %s) is now in **%s**.", escape(c.DisplayName), escape(c.StackName), escape(c.HostName), escape(g.DisplayName))
	if members, err := r.Store.GroupMembers(ctx, g.ID); err == nil {
		for _, m := range members {
			if m.HostID != c.HostID {
				msg += "\n⚠️ The group now spans more than one host, so it is held (group_spans_hosts) until that changes."
				break
			}
		}
	}
	r.refresh(ctx, c.StackID)
	return replyCard("🔗", "Added to "+escape(g.DisplayName), msg, colorOK)
}

func (r *Router) groupRemove(ctx context.Context, it Interaction) Response {
	c, errResp := r.resolveContainer(ctx, it.Options[optContainer])
	if errResp != nil {
		return *errResp
	}
	if _, err := r.Store.RemoveMember(ctx, c.ID, r.actor(it), r.Now()); errors.Is(err, store.ErrNotMember) {
		return ephemeral("**" + escape(c.DisplayName) + "** is not in a group.")
	} else if err != nil {
		return r.internalError("remove member", err)
	}
	r.refresh(ctx, c.StackID)
	msg := fmt.Sprintf("**%s** is updated with its stack again.", escape(c.DisplayName))
	if c.DdumLabels["ddum.group"] != "" {
		msg += " Its ddum.group label will not add it back."
	}
	return replyCard("🔗", "Removed from "+escape(c.GroupName), msg, colorOK)
}

// stackMerge confirms a merge first: it cannot be undone.
func (r *Router) stackMerge(ctx context.Context, it Interaction) Response {
	a, errResp := r.resolveStack(ctx, it.Options[optSource])
	if errResp != nil {
		return *errResp
	}
	b, errResp := r.resolveStack(ctx, it.Options[optInto])
	if errResp != nil {
		return *errResp
	}
	if a.ID == b.ID {
		return ephemeral("Pick two different stacks.")
	}
	old, newer := a, b
	if b.CreatedAt.Before(a.CreatedAt) {
		old, newer = b, a
	}
	desc := func(s store.Stack) string {
		return fmt.Sprintf("on %s · %s · %s", escape(s.HostName), code(s.Fingerprint), s.Presence)
	}
	m := notice(iconDrift, "Merge "+escape(newer.DisplayName)+" into "+escape(old.DisplayName)+"?",
		"The older stack keeps its identity: policy, groups, snoozes, and history. It takes over the newer one's name, host, provider, "+
			"and containers, and the newer one is retired. This cannot be undone.", colorWarn)
	m.Embeds[0].Fields = []*discordgo.MessageEmbedField{
		field("Keeps its identity · "+escape(old.DisplayName), desc(old), false),
		field("Retired · "+escape(newer.DisplayName), desc(newer), false),
	}
	m.Components = buttonRow(button("Merge", "🔀", discordgo.DangerButton, customID(actStackMerge, a.ID, b.ID)),
		button("Cancel", "", discordgo.SecondaryButton, customID(actCancel)))
	if it.Kind == KindComponent {
		return updateTo(m)
	}
	return reply(m)
}

func (r *Router) stackMergeConfirmed(ctx context.Context, it Interaction, a, b string) Response {
	res, err := r.Store.MergeStacks(ctx, a, b, r.actor(it), r.Now())
	switch {
	case errors.Is(err, store.ErrStackRetired), errors.Is(err, store.ErrNotFound):
		return updateCard(iconFailed, "Not merged", "One of the stacks was already merged or no longer exists.", colorProblem)
	case errors.Is(err, store.ErrMergeBusy):
		return updateCard(iconFailed, "Not merged", "An operation is in progress for one of the stacks. Try again when it finishes.", colorProblem)
	case err != nil:
		return r.internalError("merge stacks", err)
	}
	m := notice(iconDrift, "Merged · "+escape(res.Survivor.DisplayName), fmt.Sprintf(
		"Keeps its identity and is now %s on **%s**. %s re-bound, %d moved over. Merged by <@%s>.",
		code(res.Survivor.Fingerprint), escape(res.Survivor.HostName), plural(res.Matched, "container"), res.Moved, it.UserID), colorOK)
	return updateTo(m)
}
