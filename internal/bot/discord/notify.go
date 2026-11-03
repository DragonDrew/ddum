package discord

import (
	"context"
	"log/slog"
	"time"

	"github.com/DragonDrew/ddum/internal/bot/store"
)

// Notifier posts system notices to the notification channel.
// Notices are queued and sent by Run, so an agent's poll never waits on
// Discord. A Notifier built without Run (Queue 0) sends synchronously.
type Notifier struct {
	Gateway   Gateway
	Store     *store.Store
	ChannelID string
	Log       *slog.Logger
	Now       func() time.Time
	// Limit is NOTIFY_INDIVIDUAL_LIMIT: above this many stacks with new
	// updates, one summary is posted instead.
	Limit int

	queue chan func(context.Context)
}

// NewNotifier returns a notifier with a queue of the given size; call Run.
func NewNotifier(n Notifier, queue int) *Notifier {
	if queue > 0 {
		n.queue = make(chan func(context.Context), queue)
	}
	return &n
}

// Run sends queued notices until ctx is cancelled, then sends what is left
// within a few seconds.
func (n *Notifier) Run(ctx context.Context) {
	for {
		select {
		case f := <-n.queue:
			n.send(context.Background(), f)
		case <-ctx.Done():
			drain, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			for {
				select {
				case f := <-n.queue:
					n.send(drain, f)
				default:
					return
				}
			}
		}
	}
}

func (n *Notifier) send(ctx context.Context, f func(context.Context)) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	f(ctx)
}

func (n *Notifier) enqueue(ctx context.Context, f func(context.Context)) {
	if n.queue == nil {
		f(ctx)
		return
	}
	select {
	case n.queue <- f:
	default:
		n.Log.Error("notification queue full; dropping notice")
	}
}

func (n *Notifier) post(ctx context.Context, what string, m Message) (string, bool) {
	id, err := n.Gateway.Send(ctx, n.ChannelID, m)
	if err != nil {
		n.Log.Error("post notice", "notice", what, "err", err)
		return "", false
	}
	return id, true
}

// HostRegistered posts the approval request for a new host and
// records the message so it can be edited when the host is decided.
func (n *Notifier) HostRegistered(ctx context.Context, h store.Host, collision bool) {
	n.enqueue(ctx, func(ctx context.Context) {
		id, ok := n.post(ctx, "host registered", registrationMessage(h, collision, ""))
		if !ok {
			return
		}
		if err := n.Store.AddDiscordMessage(ctx, entityRegistration, h.ID, n.ChannelID, id, n.Now()); err != nil {
			n.Log.Error("record registration notice", "host_id", h.ID, "err", err)
		}
	})
}

// publicChannelSetting records which channel was already warned about.
const publicChannelSetting = "public_channel_warning"

// WarnPublicChannel posts the one-time warning that the notification channel
// is visible to @everyone. It is posted once per channel.
func (n *Notifier) WarnPublicChannel(ctx context.Context) {
	var warned struct {
		ChannelID string `json:"channel_id"`
	}
	if _, err := n.Store.Setting(ctx, publicChannelSetting, &warned); err != nil {
		n.Log.Error("read public channel setting", "err", err)
		return
	}
	if warned.ChannelID == n.ChannelID {
		return
	}
	m := notice(iconPublic, "This channel is public",
		"Everyone in the server can see it, and ddum posts host, stack, image, and digest details here.\n"+
			"**Fix:** deny *View Channel* to @everyone, then allow only the bot and the people who approve updates.", colorProblem)
	m.Embeds[0].Footer = footer("Shown once per channel")
	_, ok := n.post(ctx, "public channel", m)
	if ok {
		warned.ChannelID = n.ChannelID
		if err := n.Store.SetSetting(ctx, publicChannelSetting, warned, n.Now()); err != nil {
			n.Log.Error("record public channel warning", "err", err)
		}
	}
}

// MergeSuggested posts one suggestion per new stack that looks like a
// renamed missing one. It never merges by itself.
func (n *Notifier) MergeSuggested(ctx context.Context, suggestions []store.MergeSuggestion) {
	list := append([]store.MergeSuggestion(nil), suggestions...)
	n.enqueue(ctx, func(ctx context.Context) {
		for _, s := range list {
			newer, err1 := n.Store.Stack(ctx, s.NewStackID)
			missing, err2 := n.Store.Stack(ctx, s.MissingStackID)
			if err1 != nil || err2 != nil {
				continue
			}
			n.post(ctx, "merge suggestion", mergeSuggestionMessage(newer, missing, s.Matched, s.Total))
		}
	})
}
