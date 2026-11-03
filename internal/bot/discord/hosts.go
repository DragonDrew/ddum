package discord

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/bwmarrin/discordgo"

	"github.com/DragonDrew/ddum/internal/bot/domain"
	"github.com/DragonDrew/ddum/internal/bot/store"
	"github.com/DragonDrew/ddum/internal/ids"
	"github.com/DragonDrew/ddum/internal/protocol"
)

// entityRegistration is the discord_messages entity type of a host's
// "wants to register" notice.
const entityRegistration = "host_registration"

// resolveHost finds the host an option names: normally a UUID chosen through
// autocomplete, or else an exact (case-insensitive) display name.
func (r *Router) resolveHost(ctx context.Context, value string) (store.Host, *Response) {
	if id, err := ids.Parse(value); err == nil {
		h, err := r.Store.Host(ctx, id)
		if err == nil && !h.Removed() {
			return h, nil
		}
	}
	hosts, err := r.Store.Hosts(ctx, false)
	if err != nil {
		resp := r.internalError("list hosts", err)
		return store.Host{}, &resp
	}
	var match []store.Host
	for _, h := range hosts {
		if strings.EqualFold(h.DisplayName, value) {
			match = append(match, h)
		}
	}
	switch len(match) {
	case 1:
		return match[0], nil
	case 0:
		resp := ephemeral("No host matches " + code(value) + ". Pick one from the list as you type.")
		return store.Host{}, &resp
	}
	resp := ephemeral("Several hosts are named " + code(value) + ". Pick one from the list as you type.")
	return store.Host{}, &resp
}

func (r *Router) hostList(ctx context.Context) Response {
	hosts, err := r.Store.Hosts(ctx, false)
	if err != nil {
		return r.internalError("list hosts", err)
	}
	if len(hosts) == 0 {
		return replyCard(iconHost, "No hosts yet", "Start an agent; it asks to register in <#"+r.ChannelID+"> on its first poll.", colorMuted)
	}
	stacks, err := r.Store.Stacks(ctx, store.StackFilter{})
	if err != nil {
		return r.internalError("list stacks", err)
	}
	count := map[string]int{}
	for _, s := range stacks {
		count[s.HostID]++
	}
	var ls []string
	for _, h := range hosts {
		ls = append(ls, fmt.Sprintf("**%s** — %s — %d stacks — inventory %s",
			escape(h.DisplayName), hostState(h), count[h.ID], relTime(h.LastInventoryAt)))
	}
	return reply(Message{Embeds: []*discordgo.MessageEmbed{{
		Title:       fmt.Sprintf("%s Hosts · %d", iconHost, len(hosts)),
		Description: lines(ls, maxDescription, "Use /host show for details."),
		Color:       colorInfo,
	}}})
}

func (r *Router) hostShow(ctx context.Context, it Interaction) Response {
	h, errResp := r.resolveHost(ctx, it.Options[optHost])
	if errResp != nil {
		return *errResp
	}
	stacks, err := r.Store.Stacks(ctx, store.StackFilter{HostID: h.ID})
	if err != nil {
		return r.internalError("list stacks", err)
	}
	var sl []string
	for _, s := range stacks {
		sl = append(sl, fmt.Sprintf("%s (%s, %d)", escape(s.DisplayName), s.Provider, s.ContainerCount))
	}
	var providers []string
	for _, p := range h.Providers {
		line := p.Type + ": " + p.Status
		if p.Detail != "" {
			line += " — " + p.Detail
		}
		providers = append(providers, line)
	}
	e := &discordgo.MessageEmbed{
		Title:       iconHost + " " + escape(h.DisplayName),
		Description: hostState(h) + "\n" + inventoryAge(h),
		Color:       hostColor(h),
		Fields: []*discordgo.MessageEmbedField{
			field("Agent ID", code(h.AgentID), false),
			field("Agent key", agentKeyText(h), false),
			field("Docker", fmt.Sprintf("%s (API %s)", h.Docker.EngineVersion, h.Docker.APIVersion), true),
			field("Platform", h.Docker.OS+"/"+h.Docker.Arch, true),
			field("Agent version", h.AgentVersion, true),
			field("Last poll", relTime(h.LastSeenAt), true),
			field("Poll interval", fmt.Sprintf("%d s", h.PollInterval), true),
			field("Capabilities", strings.Join(h.Capabilities, ", "), false),
			field("Providers", strings.Join(providers, "\n"), false),
			field(fmt.Sprintf("Stacks (%d)", len(stacks)), lines(sl, maxFieldValue, "Use /stack list."), false),
		},
	}
	if h.Approval == domain.ApprovalPending {
		e.Footer = &discordgo.MessageEmbedFooter{Text: "Waiting for approval. It receives no operations until approved: /host approve"}
	}
	return reply(Message{Embeds: []*discordgo.MessageEmbed{e}})
}

// approve approves h and edits its registration notices. skipMessageID is the
// notice the click came from, which the caller updates itself.
func (r *Router) approve(ctx context.Context, it Interaction, h store.Host, skipMessageID string) (store.Host, string, error) {
	approved, already, err := r.Store.ApproveHost(ctx, h.ID, r.actor(it), r.Now())
	switch {
	case errors.Is(err, store.ErrHostRemoved):
		return h, "**" + escape(h.DisplayName) + "** has been removed and cannot be approved.", err
	case errors.Is(err, store.ErrUnsupportedArch):
		return h, "**" + escape(h.DisplayName) + "** cannot be approved: unsupported architecture " + code(h.Docker.Arch) +
			". Supported: " + domain.SupportedArches + ".", err
	case err != nil:
		return h, "", err
	}
	if already {
		return approved, "**" + escape(h.DisplayName) + "** was already approved.", nil
	}
	r.Log.Info("host approved", "host_id", h.ID, "by", it.UserID)
	r.editRegistrations(ctx, approved, false, "Approved by <@"+it.UserID+">", skipMessageID)
	return approved, "Approved **" + escape(h.DisplayName) + "**.", nil
}

func (r *Router) hostApprove(ctx context.Context, it Interaction) Response {
	h, errResp := r.resolveHost(ctx, it.Options[optHost])
	if errResp != nil {
		return *errResp
	}
	approved, msg, err := r.approve(ctx, it, h, "")
	if err != nil {
		if msg == "" {
			return r.internalError("approve host", err)
		}
		return ephemeral(msg)
	}
	return reply(registrationMessage(approved, false, strings.TrimSuffix(msg, ".")))
}

func (r *Router) hostApproveButton(ctx context.Context, it Interaction, hostID string) Response {
	h, err := r.Store.Host(ctx, hostID)
	if err != nil {
		return ephemeral("That host no longer exists.")
	}
	approved, msg, err := r.approve(ctx, it, h, it.MessageID)
	if err != nil {
		if msg == "" {
			return r.internalError("approve host", err)
		}
		return ephemeral(msg)
	}
	status := "Approved by <@" + it.UserID + ">"
	if strings.Contains(msg, "already") {
		status = "Already approved"
		if approved.ApprovedBy != "" {
			status += " by <@" + approved.ApprovedBy + ">"
		}
	}
	return updateTo(registrationMessage(approved, false, status))
}

// hostRejectButton asks for confirmation before rejecting (removing) a host.
func (r *Router) hostRejectButton(ctx context.Context, hostID string) Response {
	h, err := r.Store.Host(ctx, hostID)
	if err != nil || h.Removed() {
		return ephemeral("That host has already been removed.")
	}
	m := confirmRemove(h).Message
	m.Components = buttonRow(button("Reject host", "✖️", discordgo.DangerButton, customID(actHostRemove, h.ID)),
		button("Back", "", discordgo.SecondaryButton, customID(actHostBack, h.ID)))
	return updateTo(m)
}

// hostBack puts a host's registration card back.
func (r *Router) hostBack(ctx context.Context, hostID string) Response {
	h, err := r.Store.Host(ctx, hostID)
	if err != nil {
		return ephemeral("That host no longer exists.")
	}
	status := ""
	if h.Approval == domain.ApprovalApproved || h.Removed() {
		status = hostState(h)
	}
	return updateTo(registrationMessage(h, false, status))
}

func (r *Router) hostRemove(ctx context.Context, it Interaction) Response {
	h, errResp := r.resolveHost(ctx, it.Options[optHost])
	if errResp != nil {
		return *errResp
	}
	return confirmRemove(h)
}

func confirmRemove(h store.Host) Response {
	verb := "Remove"
	if h.Approval == domain.ApprovalPending {
		verb = "Reject"
	}
	m := notice(iconOffline, verb+" "+escape(h.DisplayName)+"?",
		"Its agent is refused from now on. Its inventory and history are kept.", colorWarn)
	m.Embeds[0].Footer = footer("Agent " + h.AgentID)
	m.Components = buttonRow(button(verb+" host", "✖️", discordgo.DangerButton, customID(actHostRemove, h.ID)),
		button("Cancel", "", discordgo.SecondaryButton, customID(actCancel)))
	return reply(m)
}

func (r *Router) hostRemoveConfirmed(ctx context.Context, it Interaction, hostID string) Response {
	h, err := r.Store.Host(ctx, hostID)
	if err != nil {
		return ephemeral("That host no longer exists.")
	}
	action, verb := "host.removed", "Removed"
	if h.Approval == domain.ApprovalPending {
		action, verb = "host.rejected", "Rejected"
	}
	removed, err := r.Store.RemoveHost(ctx, hostID, action, r.actor(it), r.Now())
	if errors.Is(err, store.ErrHostRemoved) {
		return updateCard(iconMaint, escape(h.DisplayName)+" was already removed", "", colorMuted)
	}
	if err != nil {
		return r.internalError("remove host", err)
	}
	r.Log.Info("host removed", "host_id", hostID, "action", action, "by", it.UserID)
	status := verb + " by <@" + it.UserID + ">"
	if card, err := r.Store.DiscordMessageAt(ctx, it.ChannelID, it.MessageID); err == nil && card.EntityType == entityRegistration {
		// The click came from the registration card itself.
		r.editRegistrations(ctx, removed, false, status, it.MessageID)
		return updateTo(registrationMessage(removed, false, status))
	}
	r.editRegistrations(ctx, removed, false, status, "")
	return updateCard("✖️", verb+" · "+escape(h.DisplayName), "Its agent is refused from now on. Its inventory and history are kept.", colorMuted)
}

func (r *Router) hostRename(ctx context.Context, it Interaction) Response {
	h, errResp := r.resolveHost(ctx, it.Options[optHost])
	if errResp != nil {
		return *errResp
	}
	name := strings.TrimSpace(it.Options[optName])
	if name == "" {
		return ephemeral("The name cannot be empty.")
	}
	if _, err := r.Store.RenameHost(ctx, h.ID, name, r.actor(it), r.Now()); err != nil {
		return r.internalError("rename host", err)
	}
	return replyCard(iconHost, "Host renamed", fmt.Sprintf("**%s** is now **%s**.", escape(h.DisplayName), escape(name)), colorOK)
}

func (r *Router) hostReassign(ctx context.Context, it Interaction) Response {
	h, errResp := r.resolveHost(ctx, it.Options[optHost])
	if errResp != nil {
		return *errResp
	}
	agentID, err := ids.Parse(strings.TrimSpace(it.Options[optAgentID]))
	if err != nil {
		return ephemeral("That is not a valid agent UUID. Use the replacement agent's AGENT_ID (generate one with `ddum-agent gen-id`).")
	}
	if agentID == h.AgentID {
		return ephemeral("**" + escape(h.DisplayName) + "** already uses that agent.")
	}
	m := notice(iconHost, "Move "+escape(h.DisplayName)+" to a new agent?", fmt.Sprintf("From %s to %s. The current agent is refused from then on. "+
		"If the new agent already registered as a pending host, that pending host is removed.", code(h.AgentID), code(agentID)), colorWarn)
	m.Components = buttonRow(button("Reassign agent", "", discordgo.DangerButton, customID(actHostReassign, h.ID, agentID)),
		button("Cancel", "", discordgo.SecondaryButton, customID(actCancel)))
	return reply(m)
}

func (r *Router) hostReassignConfirmed(ctx context.Context, it Interaction, hostID, agentID string) Response {
	if _, err := ids.Parse(agentID); err != nil {
		return ephemeral("This button is malformed.")
	}
	res, err := r.Store.ReassignAgent(ctx, hostID, agentID, r.actor(it), r.Now())
	switch {
	case errors.Is(err, store.ErrNotFound), errors.Is(err, store.ErrHostRemoved):
		return updateCard(iconFailed, "Not reassigned", "That host no longer exists.", colorProblem)
	case errors.Is(err, store.ErrAgentInUse):
		return updateCard(iconFailed, "Not reassigned", "That agent belongs to an approved host. "+
			strings.TrimPrefix(err.Error(), store.ErrAgentInUse.Error()+" "), colorProblem)
	case err != nil:
		return r.internalError("reassign agent", err)
	}
	if res.RetiredHost != "" {
		if retired, err := r.Store.Host(ctx, res.RetiredHost); err == nil {
			r.editRegistrations(ctx, retired, false, "Merged into "+escape(res.Host.DisplayName)+" by <@"+it.UserID+">", "")
		}
	}
	if r.Notify != nil {
		for _, op := range res.Released {
			r.Notify.OperationFinished(ctx, op)
		}
	}
	return updateCard(iconOK, "Agent reassigned · "+escape(res.Host.DisplayName), "Now uses agent "+code(agentID)+".", colorOK)
}

// agentKeyText describes a host's agent key on /host show.
func agentKeyText(h store.Host) string {
	if h.AgentKey == "" {
		return "none yet: this agent runs an older version, and relies on the shared key alone"
	}
	s := code(protocol.KeyFingerprint(h.AgentKey))
	if h.PendingAgentKey != "" {
		s += "\n" + iconWarning + " presenting a different key: " + code(protocol.KeyFingerprint(h.PendingAgentKey))
	}
	return s
}

// agentKeyMessage asks whether to accept a key an agent now presents.
func agentKeyMessage(h store.Host, fingerprint string) Message {
	m := notice(iconWarning, escape(h.DisplayName)+" presents a different key",
		"Its requests are signed with a key other than the one the bot pinned, so they are refused until someone decides. "+
			"The agent's data volume being lost or recreated causes this. So would someone else who has the shared key posing as this host.\n"+
			"New key: "+code(fingerprint)+"\nCheck that it is the fingerprint in the log of the agent on that host (`agent key` line) before you accept it.", colorWarn)
	m.Components = buttonRow(button("Accept new key", "", discordgo.SuccessButton, customID(actKeyAccept, h.ID)),
		button("Keep the old key", "", discordgo.SecondaryButton, customID(actKeyReject, h.ID)))
	return m
}

// agentKeyDecision applies the answer to agentKeyMessage.
func (r *Router) agentKeyDecision(ctx context.Context, it Interaction, hostID string, accept bool) Response {
	decide, what := r.Store.RejectAgentKey, "kept the old key"
	if accept {
		decide, what = r.Store.AcceptAgentKey, "accepted the new key"
	}
	h, err := decide(ctx, hostID, r.actor(it), r.Now())
	if errors.Is(err, store.ErrNotFound) {
		return ephemeral("That has already been decided.")
	}
	if err != nil {
		return r.internalError("decide agent key", err)
	}
	return updateCard(iconOK, escape(h.DisplayName)+": "+what, "Decided by <@"+it.UserID+">. Key now pinned: "+code(protocol.KeyFingerprint(h.AgentKey))+".", colorOK)
}

// registrationMessage is the "new host wants to register" notice.
// With an empty status it carries Approve and Reject buttons; otherwise the
// buttons are removed and the status is shown.
func registrationMessage(h store.Host, collision bool, status string) Message {
	e := &discordgo.MessageEmbed{
		Title:       iconHost + " New host wants to register: " + escape(h.DisplayName),
		Description: "An approved host's containers can be updated from this channel.",
		Color:       colorWarn,
		Fields: []*discordgo.MessageEmbedField{
			field("Docker", h.Docker.EngineVersion, true),
			field("OS", h.Docker.OS, true),
			field("Arch", h.Docker.Arch, true),
		},
		Timestamp: stamp(h.CreatedAt),
		Footer:    footer("Approve only if you started this agent"),
	}
	if h.AgentKey != "" {
		e.Fields = append(e.Fields, field("Agent key", code(protocol.KeyFingerprint(h.AgentKey))+" (the agent logs its own at startup)", false))
	}
	supported := domain.ArchSupported(h.Docker.Arch)
	var warnings []string
	if collision {
		warnings = append(warnings, "Another host already uses this name. Names are only labels, but check this is the host you expect.")
	}
	if !supported {
		warnings = append(warnings, "Approve is disabled: unsupported architecture. Supported: "+domain.SupportedArches+".")
	}
	if len(warnings) > 0 {
		e.Fields = append(e.Fields, field(iconWarning+" Warning", strings.Join(warnings, "\n"), false))
	}
	m := Message{Embeds: []*discordgo.MessageEmbed{e}}
	if status != "" {
		e.Color = colorMuted
		if h.Approval == domain.ApprovalApproved && !h.Removed() {
			e.Color = colorOK
		}
		e.Fields = append(e.Fields, field("Status", status, false))
		m.Components = []discordgo.MessageComponent{}
		return m
	}
	m.Components = []discordgo.MessageComponent{discordgo.ActionsRow{Components: []discordgo.MessageComponent{
		discordgo.Button{Label: "Approve", Style: discordgo.SuccessButton, Emoji: &discordgo.ComponentEmoji{Name: "✅"}, CustomID: customID(actHostApprove, h.ID), Disabled: !supported},
		discordgo.Button{Label: "Reject", Style: discordgo.DangerButton, Emoji: &discordgo.ComponentEmoji{Name: "✖️"}, CustomID: customID(actHostReject, h.ID)},
	}}}
	return m
}

// editRegistrations rewrites every stored registration notice of h to show
// status and drop its buttons.
func (r *Router) editRegistrations(ctx context.Context, h store.Host, collision bool, status, skipMessageID string) {
	msgs, err := r.Store.DiscordMessages(ctx, entityRegistration, h.ID)
	if err != nil {
		r.Log.Warn("load registration notices", "host_id", h.ID, "err", err)
		return
	}
	for _, m := range msgs {
		if m.MessageID == skipMessageID {
			continue
		}
		if err := r.Gateway.Edit(ctx, m.ChannelID, m.MessageID, registrationMessage(h, collision, status)); err != nil {
			r.Log.Warn("edit registration notice", "host_id", h.ID, "message_id", m.MessageID, "err", err)
		}
	}
}
