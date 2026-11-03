package discord

import (
	"context"
	"fmt"
	"sync"

	"github.com/bwmarrin/discordgo"
)

// Cards: every reply is a self-contained card. A command run in the
// notification channel answers with a card posted there; clicking its
// buttons edits that same card, and the bot keeps editing it as things
// happen (a check finishing, a deploy's result). Cards are tracked in
// discord_messages like notifications, so this survives restarts. Errors and
// refusals stay private, and so does everything when a command is run in
// another channel, so inventory never leaks into a channel ddum does not own.

// entityCheck binds a card to an on-demand registry check. Checks are not
// stored, so these bindings live in memory only.
const entityCheck = "check"

// reply is a public card answering a command.
func reply(m Message) Response {
	return Response{Kind: RespMessage, Message: m}
}

// replyCard is a public one-embed card.
func replyCard(icon, title, desc string, color int) Response {
	return reply(notice(icon, title, desc, color))
}

// updateTo replaces the card a button belongs to.
func updateTo(m Message) Response {
	if m.Components == nil {
		m.Components = []discordgo.MessageComponent{}
	}
	return Response{Kind: RespUpdate, Message: m}
}

// updateCard replaces the card a button belongs to with a one-embed card.
func updateCard(icon, title, desc string, color int) Response {
	return updateTo(notice(icon, title, desc, color))
}

// ephemeral is a private note: an error, a refusal, or a hint.
func ephemeral(text string) Response {
	return Response{Kind: RespMessage, Ephemeral: true, Private: true, Message: Message{Embeds: []*discordgo.MessageEmbed{{
		Description: text, Color: colorMuted,
	}}}}
}

// cancelledCard ends a confirmation that changed nothing.
func cancelledCard() Response {
	return updateCard("✖️", "Cancelled", "Nothing was changed.", colorMuted)
}

// buttons builds one action row.
func buttonRow(bs ...discordgo.Button) []discordgo.MessageComponent {
	row := discordgo.ActionsRow{}
	for _, b := range bs {
		row.Components = append(row.Components, b)
	}
	return []discordgo.MessageComponent{row}
}

func button(label, emoji string, style discordgo.ButtonStyle, id string) discordgo.Button {
	b := discordgo.Button{Label: label, Style: style, CustomID: id}
	if emoji != "" {
		b.Emoji = &discordgo.ComponentEmoji{Name: emoji}
	}
	return b
}

// visibility decides who sees a reply: public cards only for commands run in
// the notification channel, and never for private notes.
func (r *Router) visibility(it Interaction, resp Response) Response {
	if resp.Kind != RespMessage {
		return resp
	}
	resp.Ephemeral = resp.Private || it.Kind != KindCommand || it.ChannelID != r.ChannelID
	if resp.Ephemeral {
		resp.Bind = nil
	}
	return resp
}

// claim makes the card a button belongs to represent entity/id from now on,
// so later events edit it. It must run before anything that refreshes the
// card's old entity. Private cards cannot be edited later and are skipped.
func (r *Router) claim(ctx context.Context, it Interaction, entity, id string) {
	if it.Kind != KindComponent || it.Ephemeral || it.MessageID == "" {
		return
	}
	if err := r.Store.BindDiscordMessage(ctx, entity, id, it.ChannelID, it.MessageID, r.Now()); err != nil {
		r.Log.Warn("bind card", "entity", entity, "id", id, "err", err)
	}
}

// Delivered records where a new public card landed. The Discord session
// calls it once the reply exists; the fake gateway does the same.
func (r *Router) Delivered(ctx context.Context, resp Response, channelID, messageID string) {
	if resp.Bind == nil || messageID == "" {
		return
	}
	if resp.Bind.Entity == entityCheck {
		r.checks.delivered(ctx, r, resp.Bind.ID, channelID, messageID)
		return
	}
	if err := r.Store.BindDiscordMessage(ctx, resp.Bind.Entity, resp.Bind.ID, channelID, messageID, r.Now()); err != nil {
		r.Log.Warn("bind card", "entity", resp.Bind.Entity, "err", err)
	}
}

// CheckSummary is what an on-demand registry check found.
type CheckSummary struct {
	Checked    int
	NewUpdates int
	Errors     int
	Err        error
}

// checkCards joins a check card with its check's result, whichever comes
// first: the check can finish before Discord says where the card is.
type checkCards struct {
	mu   sync.Mutex
	byID map[string]*checkCard
}

type checkCard struct {
	hostID, stackID, label string
	channelID, messageID   string
	done                   *CheckSummary
}

func (c *checkCards) start(id, hostID, stackID, label string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.byID[id] = &checkCard{hostID: hostID, stackID: stackID, label: label}
}

func (c *checkCards) delivered(ctx context.Context, r *Router, id, channelID, messageID string) {
	c.mu.Lock()
	card, ok := c.byID[id]
	if ok {
		card.channelID, card.messageID = channelID, messageID
	}
	ready := ok && card.done != nil
	if ready {
		delete(c.byID, id)
	}
	c.mu.Unlock()
	if ready {
		r.showCheckResult(ctx, card)
	}
}

func (c *checkCards) finished(ctx context.Context, r *Router, id string, sum CheckSummary) {
	c.mu.Lock()
	card, ok := c.byID[id]
	if ok {
		card.done = &sum
	}
	ready := ok && card.messageID != ""
	if ready {
		delete(c.byID, id)
	}
	c.mu.Unlock()
	if ready {
		r.showCheckResult(ctx, card)
	}
}

// showCheckResult turns a check card into its result.
func (r *Router) showCheckResult(ctx context.Context, card *checkCard) {
	m, err := r.checkResultMessage(ctx, card)
	if err != nil {
		r.Log.Error("render check result", "err", err)
		return
	}
	if err := r.Gateway.Edit(ctx, card.channelID, card.messageID, m); err != nil {
		r.Log.Warn("edit check card", "message_id", card.messageID, "err", err)
	}
}

func (r *Router) checkResultMessage(ctx context.Context, card *checkCard) (Message, error) {
	sum := *card.done
	if sum.Err != nil {
		return notice(iconFailed, "Check failed · "+card.label, "The registry check stopped early. The error has been logged.", colorProblem), nil
	}
	views, err := pendingViews(ctx, r.Store, card.hostID, r.Now())
	if err != nil {
		return Message{}, err
	}
	if card.stackID != "" {
		var only []scopeView
		for _, v := range views {
			for _, l := range v.Lines {
				if l.U.StackID == card.stackID {
					only = append(only, v)
					break
				}
			}
		}
		views = only
	}
	pending := 0
	for _, v := range views {
		pending += len(v.Lines)
	}
	stats := fmt.Sprintf("Checked **%d** images · **%d** pending · **%d** new", sum.Checked, pending, sum.NewUpdates)
	if sum.Errors > 0 {
		stats += fmt.Sprintf("\n%s %d could not be checked (see /container show)", iconWarning, sum.Errors)
	}
	if len(views) == 0 {
		m := notice(iconOK, "Up to date · "+card.label, stats+"\nNothing is waiting for approval.", colorOK)
		m.Embeds[0].Timestamp = stamp(r.Now())
		return m, nil
	}
	m := summaryMessage("Check finished · "+card.label, views, true)
	m.Embeds[0].Title = "🔎 Check finished · " + escape(card.label)
	m.Embeds[0].Description = stats
	m.Embeds[0].Timestamp = stamp(r.Now())
	return m, nil
}

func (c *checkCards) drop(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.byID, id)
}
