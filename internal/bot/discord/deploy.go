package discord

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/DragonDrew/ddum/internal/bot/store"
	"github.com/DragonDrew/ddum/internal/ids"
	"github.com/DragonDrew/ddum/internal/protocol"
)

// entityOperation is the discord_messages entity type of an operation's
// result message.
const entityOperation = "operation"

// approveButton approves an offer. The offer, not the scope,
// decides what is approved; a stale offer approves nothing and returns a
// fresh one.
func (r *Router) approveButton(ctx context.Context, it Interaction, offerID string) Response {
	return r.approveAt(ctx, it, offerID, time.Time{})
}

// approveAt approves an offer to deploy now, or at a scheduled time.
func (r *Router) approveAt(ctx context.Context, it Interaction, offerID string, at time.Time) Response {
	res, err := r.Store.Approve(ctx, store.ApproveRequest{OfferID: offerID, By: r.actor(it), At: r.Now(),
		ExpiryMinutes: r.ExpiryMinutes, NotBefore: at})
	var already *store.AlreadyApprovedError
	var nothing *store.NothingError
	switch {
	case errors.As(err, &already):
		by := "the policy engine"
		if already.ByDiscordID != "" {
			by = "<@" + already.ByDiscordID + ">"
		}
		return ephemeral("Already approved by " + by + ".")
	case errors.Is(err, store.ErrStaleOffer):
		return r.staleOffer(ctx, offerID)
	case errors.As(err, &nothing):
		return ephemeral("Nothing here can be approved:\n" + skippedText(nothing.Skipped))
	case errors.Is(err, store.ErrNotFound):
		return ephemeral("This button is no longer valid. Use /update list.")
	case err != nil:
		return r.internalError("approve", err)
	}
	r.Log.Info("operation approved", "operation_id", res.Operation.ID, "by", it.UserID, "updates", len(res.Approved))
	// The card becomes the operation's card before its stack's card is
	// refreshed, so the refresh does not overwrite it.
	r.claim(ctx, it, entityOperation, res.Operation.ID)
	r.refresh(ctx, res.Operation.ScopeID)
	m := r.approvedMessage(ctx, res)
	if it.Ephemeral {
		m.Embeds[0].Description += "\nThe result is posted in <#" + r.ChannelID + ">."
	}
	return updateTo(m)
}

// approvedMessage is the card of a just-approved operation.
func (r *Router) approvedMessage(ctx context.Context, res store.ApproveResult) Message {
	when := "Starts on the agent's next poll."
	if !res.Operation.NotBefore.IsZero() {
		when = "Deploys " + discordTime(res.Operation.NotBefore) + "."
	} else if res.QueuePosition > 0 {
		when = fmt.Sprintf("Waits for %s on this host first.", plural(res.QueuePosition, "other operation"))
	}
	m := queuedCard(res.Operation, when)
	e := m.Embeds[0]
	if len(res.Skipped) > 0 {
		e.Fields = append(e.Fields, field(iconHeld+" Left out", truncate(skippedText(res.Skipped), maxFieldValue), false))
	}
	if on, _ := r.Store.Maintenance(ctx); on {
		e.Fields = append(e.Fields, field(iconMaint+" Maintenance mode is on", "Manual approvals still deploy.", false))
	}
	return m
}

// queuedCard shows an operation waiting for its agent, with a Cancel button.
func queuedCard(op store.Operation, when string) Message {
	title := "⏳ Deploy queued · "
	switch {
	case !op.NotBefore.IsZero() && op.State == protocol.OpQueued:
		title = iconClock + " Scheduled · "
	case op.Kind == protocol.CommandRollback:
		title = iconRollback + " Rollback queued · "
	case op.Payload.SelfUpdate:
		title = iconBot + " Bot update queued · "
	}
	by := "the policy engine " + iconBot
	if op.InitiatorDiscordID != "" {
		by = "<@" + op.InitiatorDiscordID + ">"
	}
	e := &discordgo.MessageEmbed{
		Title:       title + escape(op.StackName),
		Author:      author(op.HostName, ""),
		Description: "Approved by " + by + " · " + when,
		Color:       colorInfo,
		Timestamp:   stamp(op.CreatedAt),
		Footer:      footer("This card shows the result when it finishes"),
	}
	if op.Kind == protocol.CommandRollback {
		e.Description = "Requested by " + by + " · " + when
	}
	e.Description += "\n" + operationTable(op, 1800)
	return Message{Embeds: []*discordgo.MessageEmbed{e}, Components: buttonRow(
		button("Cancel", "", discordgo.SecondaryButton, customID(actOpCancel, op.ID)))}
}

func skippedText(sk []store.Skipped) string {
	var b strings.Builder
	for _, s := range sk {
		fmt.Fprintf(&b, "• %s: %s\n", escape(s.Update.ContainerName), s.Reason)
	}
	return b.String()
}

// staleOffer answers a click on an outdated offer with the current state and
// a new offer, and edits the stack's message.
func (r *Router) staleOffer(ctx context.Context, offerID string) Response {
	offer, err := r.Store.GetOffer(ctx, offerID)
	if err != nil {
		return ephemeral("This button is no longer valid. Use /update list.")
	}
	r.refresh(ctx, offer.ScopeID)
	v, err := loadAnyScope(ctx, r.Store, offer.ScopeID, r.Now())
	if err != nil {
		return ephemeral("Those updates changed and the stack no longer exists.")
	}
	fresh, err := offerFor(ctx, r.Store, v, "command", r.Now())
	if err != nil {
		return r.internalError("create offer", err)
	}
	m := scopeMessage(v, fresh)
	m.Embeds[0].Description = iconWarning + " These updates changed since the card was shown, so nothing was approved. This is the current state.\n" +
		m.Embeds[0].Description
	return updateTo(m)
}

// updateApprove shows a confirmation for one update. A snoozed update may be approved this
// way; approving removes the snooze.
func (r *Router) updateApprove(ctx context.Context, it Interaction) Response {
	u, errResp := r.resolveUpdate(ctx, it.Options[optUpdate])
	if errResp != nil {
		return *errResp
	}
	if u.Status != store.StatusDetected {
		return ephemeral("That update is " + u.Status + ", not pending.")
	}
	c, err := r.Store.Container(ctx, u.ContainerID)
	if err != nil {
		return r.internalError("load container", err)
	}
	var offer string
	if c.Self == protocol.SelfBot {
		offer, err = r.Store.CreateSelfUpdateOffer(ctx, u, r.Now())
	} else {
		offer, err = r.Store.CreateOffer(ctx, "stack", u.StackID, []store.OfferItem{{UpdateID: u.ID, TargetDigest: u.TargetDigest}}, "command", r.Now())
	}
	if err != nil {
		return r.internalError("create offer", err)
	}
	e := &discordgo.MessageEmbed{
		Title:       iconPending + " Approve " + escape(u.ContainerName) + " · " + escape(u.StackName) + "?",
		Author:      author(u.HostName, ""),
		Description: change(u.CurrentRef, u.CurrentDigest, u.TargetRef, u.TargetDigest),
		Color:       colorWarn,
		Fields:      []*discordgo.MessageEmbedField{field("Kind", u.Kind, true), field("Target digest", code(shortDigest(u.TargetDigest)), true)},
	}
	if u.Snoozed() {
		e.Fields = append(e.Fields, field(iconSnoozed+" Snoozed", "Approving removes the snooze.", false))
	}
	if u.ReleaseURL != "" {
		e.Description += "\n[" + iconNotes + " Release notes](" + u.ReleaseURL + ")"
	}
	if c.Self == protocol.SelfBot {
		return reply(Message{Embeds: []*discordgo.MessageEmbed{e}, Components: buttonRow(
			button("Approve", "✅", discordgo.SuccessButton, customID(actSelfUpdate, offer)),
			button("Cancel", "", discordgo.SecondaryButton, customID(actCancel)))})
	}
	return reply(Message{Embeds: []*discordgo.MessageEmbed{e}, Components: buttonRow(
		button("Approve", "✅", discordgo.SuccessButton, customID(actApprove, offer)),
		button("Schedule", "⏰", discordgo.PrimaryButton, customID(actSchedule, offer)),
		button("Cancel", "", discordgo.SecondaryButton, customID(actCancel)))})
}

func (r *Router) cancelOperation(ctx context.Context, it Interaction, opID string) Response {
	if _, err := ids.Parse(opID); err != nil {
		return ephemeral("This button is malformed.")
	}
	op, err := r.Store.CancelOperation(ctx, opID, r.actor(it), r.Now())
	switch {
	case errors.Is(err, store.ErrNotCancellable):
		return ephemeral("Too late to cancel: the agent has already started. This card shows the result when it finishes.")
	case errors.Is(err, store.ErrNotFound):
		return ephemeral("That operation no longer exists.")
	case err != nil:
		return r.internalError("cancel", err)
	}
	note := "✖️ <@" + it.UserID + "> cancelled the deploy. These updates are pending again."
	if op.Kind == protocol.CommandRollback {
		r.refresh(ctx, op.ScopeID)
		return updateCard("✖️", "Rollback cancelled · "+escape(op.StackName), "Cancelled by <@"+it.UserID+">. Nothing was changed.", colorMuted)
	}
	resp := r.backToScope(ctx, it, op.ScopeID, note)
	r.refresh(ctx, op.ScopeID)
	return resp
}

func (r *Router) resolveOperation(ctx context.Context, value string) (store.Operation, *Response) {
	if id, err := ids.Parse(value); err == nil {
		if op, err := r.Store.GetOperation(ctx, id); err == nil {
			return op, nil
		}
	}
	resp := ephemeral("No operation matches " + code(value) + ". Pick one from the list as you type.")
	return store.Operation{}, &resp
}

func opLabel(op store.Operation) string {
	return fmt.Sprintf("%s on %s · %s · %s", op.StackName, op.HostName, op.State, op.CreatedAt.UTC().Format("Jan 2 15:04 UTC"))
}

func (r *Router) historyList(ctx context.Context, it Interaction) Response {
	hostID, stackID, label, errResp := r.scopeFilter(ctx, it)
	if errResp != nil {
		return *errResp
	}
	ops, err := r.Store.Operations(ctx, store.OperationFilter{HostID: hostID, ScopeID: stackID, Limit: 25})
	if err != nil {
		return r.internalError("list operations", err)
	}
	if len(ops) == 0 {
		return replyCard("📜", "No history yet", "Nothing has been deployed for "+label+".", colorMuted)
	}
	var ls []string
	for _, op := range ops {
		by := "policy"
		if op.InitiatorDiscordID != "" {
			by = "<@" + op.InitiatorDiscordID + ">"
		}
		ls = append(ls, fmt.Sprintf("%s **%s** on %s · %s · %s by %s", opIcon(op), escape(op.StackName),
			escape(op.HostName), strings.ReplaceAll(op.State, "_", " "), relTime(op.CreatedAt), by))
	}
	return reply(Message{Embeds: []*discordgo.MessageEmbed{{
		Title: "📜 History · " + strings.ReplaceAll(label, "**", ""), Description: lines(ls, maxDescription, "Narrow it with host or stack."),
		Color: colorInfo, Footer: footer("Details: /history show"),
	}}})
}

func (r *Router) historyShow(ctx context.Context, it Interaction) Response {
	op, errResp := r.resolveOperation(ctx, it.Options[optOperation])
	if errResp != nil {
		return *errResp
	}
	e := operationEmbed(op)
	if evs, err := r.Store.OperationEvents(ctx, op.ID); err == nil && len(evs) > 0 {
		var ls []string
		for _, ev := range evs {
			l := fmt.Sprintf("%d. %s", ev.Seq, ev.Type)
			if ev.Step != "" {
				l += " · " + ev.Step
			}
			if ev.Message != "" {
				l += " · " + truncate(ev.Message, 120)
			}
			ls = append(ls, l)
		}
		e.Fields = append(e.Fields, field("Events", lines(ls, maxFieldValue, ""), false))
	}
	if tail := op.Outcome.OutputTail; tail != "" {
		e.Description += "\n**Output (end):**\n```\n" + strings.ReplaceAll(lastBytes(tail, 1500), "```", "'''") + "\n```"
	}
	return reply(Message{Embeds: []*discordgo.MessageEmbed{e}})
}

func lastBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}

func opColor(state string) int {
	switch state {
	case protocol.OpSucceeded:
		return colorOK
	case protocol.OpFailed, protocol.OpPartiallyFailed, protocol.OpInterrupted:
		return colorProblem
	case protocol.OpExpired, protocol.OpCancelled:
		return colorMuted
	}
	return colorInfo
}

// OperationFinished posts an operation's result and edits its stack's
// update message.
func (n *Notifier) OperationFinished(ctx context.Context, op store.Operation) {
	n.enqueue(ctx, func(ctx context.Context) {
		n.showOperation(ctx, op, n.resultMessage(ctx, op))
		n.refreshStack(ctx, op.ScopeID)
	})
}

// resultMessage is a finished operation's card, with Roll back or Clear
// hold when they apply.
func (n *Notifier) resultMessage(ctx context.Context, op store.Operation) Message {
	comps := n.resultComponents(ctx, op)
	e := operationEmbed(op)
	if hasButton(comps, customID(actRollback, op.ID)) {
		for _, f := range e.Fields {
			if f.Name == "Next" {
				f.Value = "Use **Roll back** below to restore the previous images, or fix the cause and run /update check."
			}
		}
	}
	return Message{Embeds: []*discordgo.MessageEmbed{e}, Components: comps}
}

// showOperation edits the operation's card, or posts one if it has none
// (or the card is gone).
func (n *Notifier) showOperation(ctx context.Context, op store.Operation, m Message) {
	if msgs, err := n.Store.DiscordMessages(ctx, entityOperation, op.ID); err == nil && len(msgs) > 0 {
		last := msgs[len(msgs)-1]
		if err := n.Gateway.Edit(ctx, last.ChannelID, last.MessageID, m); err == nil {
			return
		} else {
			n.Log.Warn("edit operation card; posting a new one", "operation_id", op.ID, "err", err)
		}
	}
	if id, ok := n.post(ctx, "operation result", m); ok {
		if err := n.Store.BindDiscordMessage(ctx, entityOperation, op.ID, n.ChannelID, id, n.Now()); err != nil {
			n.Log.Error("record operation message", "operation_id", op.ID, "err", err)
		}
	}
}

// operationTitle names an outcome: what happened, then the scope.
func operationTitle(op store.Operation) string {
	verb := map[string][2]string{
		protocol.OpSucceeded:       {iconOK, "Deployed"},
		protocol.OpPartiallyFailed: {iconWarning, "Partly deployed"},
		protocol.OpFailed:          {iconFailed, "Deploy failed"},
		protocol.OpInterrupted:     {iconFailed, "Deploy interrupted"},
		protocol.OpExpired:         {iconExpired, "Deploy expired"},
		protocol.OpCancelled:       {"✖️", "Deploy cancelled"},
	}[op.State]
	if verb[1] == "" {
		verb = [2]string{iconDeploying, "Deploy " + op.State}
	}
	switch {
	case op.Kind == protocol.CommandRollback && op.State == protocol.OpSucceeded:
		verb = [2]string{iconRollback, "Rolled back"}
	case op.Kind == protocol.CommandRollback:
		verb[1] = strings.Replace(verb[1], "Deploy", "Rollback", 1)
	case op.Payload.SelfUpdate && op.Outcome.Result != nil && op.Outcome.Result.RolledBack:
		verb = [2]string{iconRollback, "Bot update failed, previous version restored"}
	case op.Payload.SelfUpdate && op.State == protocol.OpSucceeded:
		verb = [2]string{iconBot, "Bot updated"}
	case op.Payload.SelfUpdate:
		verb[1] = strings.Replace(verb[1], "Deploy", "Bot update", 1)
	}
	return verb[0] + " " + verb[1] + " · " + escape(op.StackName)
}

// hasButton reports whether comps has a button with this custom ID.
func hasButton(comps []discordgo.MessageComponent, id string) bool {
	for _, c := range comps {
		if row, ok := c.(discordgo.ActionsRow); ok {
			for _, b := range row.Components {
				if b, ok := b.(discordgo.Button); ok && b.CustomID == id {
					return true
				}
			}
		}
	}
	return false
}

// operationEmbed is an operation's result.
func operationEmbed(op store.Operation) *discordgo.MessageEmbed {
	by := "the policy engine 🤖"
	if op.InitiatorDiscordID != "" {
		by = "<@" + op.InitiatorDiscordID + ">"
	}
	desc := "Approved by " + by
	if op.Kind == protocol.CommandRollback {
		desc = "Requested by " + by
	}
	if !op.FinishedAt.IsZero() && op.FinishedAt.After(op.CreatedAt) {
		desc += " · took " + op.FinishedAt.Sub(op.CreatedAt).Round(time.Second).String()
	}
	e := &discordgo.MessageEmbed{
		Title:       operationTitle(op),
		Author:      author(op.HostName, ""),
		Description: desc,
		Color:       opColor(op.State),
		Timestamp:   stamp(op.FinishedAt),
		Footer:      footer("Full details: /history show"),
	}
	e.Description += "\n" + operationTable(op, 1800)
	if op.Outcome.Result != nil {
		var failed []string
		for _, c := range op.Outcome.Result.Containers {
			if c.Outcome == protocol.OutcomeFailed && c.Message != "" {
				failed = append(failed, iconFailed+" **"+escape(c.IdentityKey)+"** · "+truncate(c.Message, 200))
			}
		}
		if len(failed) > 0 {
			e.Description += "\n" + lines(failed, 1200, "/history show")
		}
	}
	if op.Outcome.Code != "" || op.Outcome.Message != "" {
		reason := strings.TrimSpace(strings.TrimPrefix(op.Outcome.Code+": "+op.Outcome.Message, ": "))
		e.Fields = append(e.Fields, field("Why", "```\n"+truncate(strings.ReplaceAll(reason, "```", "'''"), 900)+"\n```", false))
	}
	if op.Outcome.Result != nil && len(op.Outcome.Result.Warnings) > 0 {
		e.Fields = append(e.Fields, field(iconWarning+" Warnings", strings.Join(op.Outcome.Result.Warnings, "\n"), false))
	}
	switch op.State {
	case protocol.OpFailed, protocol.OpPartiallyFailed, protocol.OpInterrupted:
		next := "Roll back below if offered, or fix the cause and run /update check."
		if op.Kind == protocol.CommandRollback {
			next = "This stack is held until you check it and clear the hold."
			if store.ChangedNothing(op.State, op.Outcome.Code) {
				next = "Nothing was changed. Fix the cause, then try again."
			}
		}
		e.Fields = append(e.Fields, field("Next", next, false))
	}
	return e
}
