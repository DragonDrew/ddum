package discord

import (
	"context"
	"errors"
	"fmt"

	"github.com/bwmarrin/discordgo"

	"github.com/DragonDrew/ddum/internal/bot/store"
	"github.com/DragonDrew/ddum/internal/ids"
	"github.com/DragonDrew/ddum/internal/protocol"
)

// botRestartWarning is shown before a bot self-update.
const botRestartWarning = "This restarts the bot. Discord commands will not respond until it is back. " +
	"The result is posted when it returns. If the new version fails its checks, the agent restores the current one on its own."

// rollbackButton asks for confirmation before a rollback. From a
// result card the question replaces the card, and Keep puts the result back.
func (r *Router) rollbackButton(ctx context.Context, it Interaction, opID string) Response {
	if _, err := ids.Parse(opID); err != nil {
		return ephemeral("This button is malformed.")
	}
	op, err := r.Store.GetOperation(ctx, opID)
	if err != nil {
		return ephemeral("That operation no longer exists.")
	}
	if err := r.Store.CanRollBack(ctx, opID); err != nil {
		return ephemeral("Cannot roll back this deploy: " + rollbackReason(err))
	}
	e := &discordgo.MessageEmbed{
		Title:       iconRollback + " Roll back " + escape(op.StackName) + "?",
		Author:      author(op.HostName, ""),
		Description: "Back to the state before this deploy. Only the image tags change in your files, and the updates show as pending again.",
		Color:       colorWarn,
	}
	var rows [][]string
	for _, t := range op.Payload.Targets {
		for _, s := range t.Services {
			rows = append(rows, []string{s.IdentityKey, tagOf(s.Desired.ImageRef), tagOf(s.Current.ImageRef)})
		}
	}
	e.Description += "\n" + table([]column{{head: "CONTAINER", max: 18}, {head: "NOW", max: 12}, {head: "BACK TO", max: 12}}, rows, 1800)
	if op.Payload.SelfUpdate {
		e.Fields = append(e.Fields, field(iconBot+" The bot restarts", botRestartWarning, false))
	}
	back := button("Cancel", "", discordgo.SecondaryButton, customID(actCancel))
	if it.Kind == KindComponent {
		back = button("Keep", "", discordgo.SecondaryButton, customID(actResultBack, op.ID))
	}
	m := Message{Embeds: []*discordgo.MessageEmbed{e}, Components: buttonRow(
		button("Roll back", "↩️", discordgo.DangerButton, customID(actRollbackConfirm, op.ID)), back)}
	if it.Kind == KindComponent {
		return updateTo(m)
	}
	return reply(m)
}

// resultBack puts an operation's result back on its card.
func (r *Router) resultBack(ctx context.Context, opID string) Response {
	if _, err := ids.Parse(opID); err != nil {
		return ephemeral("This button is malformed.")
	}
	op, err := r.Store.GetOperation(ctx, opID)
	if err != nil {
		return ephemeral("That operation no longer exists.")
	}
	return updateTo(r.Notify.resultMessage(ctx, op))
}

func rollbackReason(err error) string {
	switch {
	case errors.Is(err, store.ErrSnapshotUnavailable):
		return "no usable snapshot (" + err.Error() + "). The previous image or saved files are gone."
	case errors.Is(err, store.ErrNotLatest):
		return "a later operation changed this stack, so only the latest one can be rolled back."
	case errors.Is(err, store.ErrChangedOutside):
		return err.Error() + ". Rolling back would undo that change, so change it back by hand if you want the old version."
	}
	return err.Error()
}

func (r *Router) rollbackConfirmed(ctx context.Context, it Interaction, opID string) Response {
	rb, err := r.Store.CreateRollback(ctx, opID, r.actor(it), r.Now(), r.ExpiryMinutes)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return ephemeral("That operation no longer exists.")
		}
		var known bool
		for _, e := range []error{store.ErrSnapshotUnavailable, store.ErrNotLatest, store.ErrNotRollbackable, store.ErrChangedOutside} {
			known = known || errors.Is(err, e)
		}
		if !known {
			return r.internalError("create rollback", err)
		}
		return updateCard(iconFailed, "Not rolled back", rollbackReason(err), colorProblem)
	}
	r.Log.Info("rollback queued", "operation_id", rb.ID, "original_operation_id", opID, "by", it.UserID)
	r.claim(ctx, it, entityOperation, rb.ID)
	r.refresh(ctx, rb.ScopeID)
	m := queuedCard(rb, "Starts on the agent's next poll.")
	if it.Ephemeral {
		m.Embeds[0].Description += "\nThe result is posted in <#" + r.ChannelID + ">."
	}
	return updateTo(m)
}

func (r *Router) rollbackList(ctx context.Context) Response {
	ops, err := r.Store.RollbackCandidates(ctx)
	if err != nil {
		return r.internalError("list rollbacks", err)
	}
	stacks, err := r.Store.Stacks(ctx, store.StackFilter{IncludeMissing: true})
	if err != nil {
		return r.internalError("list stacks", err)
	}
	var holds []string
	for _, s := range stacks {
		if opID, held, _ := r.Store.RollbackHold(ctx, s.ID); held {
			_ = opID
			holds = append(holds, fmt.Sprintf("• **%s** on %s: a rollback failed. Check it, then /rollback clear-hold.",
				escape(s.DisplayName), escape(s.HostName)))
		}
	}
	if len(ops) == 0 && len(holds) == 0 {
		return replyCard(iconRollback, "Nothing to roll back", "Only a stack's latest deployment can be rolled back, while its snapshot is available.", colorMuted)
	}
	var ls []string
	for _, op := range ops {
		ls = append(ls, fmt.Sprintf("%s **%s** on %s · deployed %s", opIcon(op), escape(op.StackName), escape(op.HostName),
			relTime(op.CreatedAt)))
	}
	e := &discordgo.MessageEmbed{Title: iconRollback + " Rollbacks", Color: colorInfo, Footer: footer("Roll back with /rollback run")}
	if len(ls) > 0 {
		e.Fields = append(e.Fields, field("Can be rolled back", lines(ls, maxFieldValue, ""), false))
	}
	if len(holds) > 0 {
		e.Fields = append(e.Fields, field("Held after a failed rollback", lines(holds, maxFieldValue, ""), false))
		e.Color = colorProblem
	}
	return reply(Message{Embeds: []*discordgo.MessageEmbed{e}})
}

func (r *Router) rollbackRun(ctx context.Context, it Interaction) Response {
	op, errResp := r.resolveOperation(ctx, it.Options[optOperation])
	if errResp != nil {
		return *errResp
	}
	return r.rollbackButton(ctx, it, op.ID)
}

func (r *Router) rollbackClearHold(ctx context.Context, it Interaction, stackValue string) Response {
	s, errResp := r.resolveStack(ctx, stackValue)
	if errResp != nil {
		return *errResp
	}
	ok, err := r.Store.ClearRollbackHold(ctx, s.ID, r.actor(it), r.Now())
	if err != nil {
		return r.internalError("clear hold", err)
	}
	if !ok {
		return ephemeral("**" + escape(s.DisplayName) + "** is not held.")
	}
	r.refresh(ctx, s.ID)
	if it.Kind == KindComponent {
		// Show the failed rollback again, now without the hold.
		if card, err := r.Store.DiscordMessageAt(ctx, it.ChannelID, it.MessageID); err == nil && card.EntityType == entityOperation {
			if op, err := r.Store.GetOperation(ctx, card.EntityID); err == nil {
				m := r.Notify.resultMessage(ctx, op)
				m.Embeds[0].Fields = append(m.Embeds[0].Fields, field(iconOK+" Hold cleared", "by <@"+it.UserID+">. Its updates can be approved again.", false))
				return updateTo(m)
			}
		}
		return updateCard(iconOK, "Hold cleared · "+escape(s.DisplayName), "Its updates can be approved again.", colorOK)
	}
	return replyCard(iconOK, "Hold cleared · "+escape(s.DisplayName), "Its updates can be approved again.", colorOK)
}

// selfUpdateButton shows the restart warning before approving a bot update.
func (r *Router) selfUpdateButton(ctx context.Context, it Interaction, offerID string) Response {
	offer, err := r.Store.GetOffer(ctx, offerID)
	if err != nil || len(offer.Items) != 1 {
		return ephemeral("This button is no longer valid. Use /update list.")
	}
	u, err := r.Store.GetUpdate(ctx, offer.Items[0].UpdateID, r.Now())
	if err != nil {
		return ephemeral("That update no longer exists.")
	}
	e := &discordgo.MessageEmbed{
		Title:       iconBot + " Update the ddum bot?",
		Author:      author(u.HostName, ""),
		Description: change(u.CurrentRef, u.CurrentDigest, u.TargetRef, u.TargetDigest),
		Color:       colorWarn,
		Fields:      []*discordgo.MessageEmbedField{field(iconWarning+" The bot restarts", botRestartWarning, false)},
	}
	back := button("Cancel", "", discordgo.SecondaryButton, customID(actCancel))
	if it.Kind == KindComponent {
		back = button("Back", "", discordgo.SecondaryButton, customID(actSelfBack, offerID))
	}
	return updateTo(Message{Embeds: []*discordgo.MessageEmbed{e}, Components: buttonRow(
		button("Update the bot", "🤖", discordgo.DangerButton, customID(actApprove, offerID)), back)})
}

// selfBack puts the bot update card back.
func (r *Router) selfBack(ctx context.Context, offerID string) Response {
	offer, err := r.Store.GetOffer(ctx, offerID)
	if err != nil || len(offer.Items) != 1 {
		return ephemeral("This button is no longer valid. Use /update list.")
	}
	u, err := r.Store.GetUpdate(ctx, offer.Items[0].UpdateID, r.Now())
	if err != nil {
		return ephemeral("That update no longer exists.")
	}
	c, err := r.Store.Container(ctx, u.ContainerID)
	if err != nil {
		return r.internalError("load container", err)
	}
	policies, err := r.Store.Policies(ctx)
	if err != nil {
		return r.internalError("load policies", err)
	}
	return updateTo(selfUpdateMessage(lineFor(u, c, policies), offerID))
}

// resultComponents adds Roll back or Clear hold to a result message.
func (n *Notifier) resultComponents(ctx context.Context, op store.Operation) []discordgo.MessageComponent {
	var buttons []discordgo.MessageComponent
	switch {
	case op.Kind == protocol.CommandDeploy:
		if n.Store.CanRollBack(ctx, op.ID) == nil {
			buttons = append(buttons, discordgo.Button{Label: "Roll back", Style: discordgo.DangerButton, CustomID: customID(actRollback, op.ID)})
		}
	case op.Kind == protocol.CommandRollback && op.State != protocol.OpSucceeded:
		if _, held, _ := n.Store.RollbackHold(ctx, op.ScopeID); held {
			buttons = append(buttons, discordgo.Button{Label: "Clear hold", Style: discordgo.SecondaryButton, CustomID: customID(actHoldClear, op.ScopeID)})
		}
	}
	if len(buttons) == 0 {
		return []discordgo.MessageComponent{}
	}
	return []discordgo.MessageComponent{discordgo.ActionsRow{Components: buttons}}
}
