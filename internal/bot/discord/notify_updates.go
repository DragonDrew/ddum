package discord

import (
	"context"
	"sort"

	"github.com/DragonDrew/ddum/internal/bot/domain"
	"github.com/DragonDrew/ddum/internal/bot/store"
	"github.com/DragonDrew/ddum/internal/protocol"
)

// NotifyNewUpdates posts notifications for detected updates that no
// notification has covered yet. Up to Limit stacks get one
// message each; more get one summary with a Review button. While maintenance
// mode is on, the updates are marked notified without posting.
func (n *Notifier) NotifyNewUpdates(ctx context.Context) {
	n.enqueue(ctx, n.notifyNewUpdates)
}

func (n *Notifier) notifyNewUpdates(ctx context.Context) {
	now := n.Now()
	fresh, err := n.Store.Updates(ctx, store.UpdateFilter{Statuses: []string{store.StatusDetected}, Visible: true, Unnotified: true}, now)
	if err != nil {
		n.Log.Error("load new updates", "err", err)
		return
	}
	if len(fresh) == 0 {
		return
	}
	ids := make([]string, 0, len(fresh))
	scopes := map[string]bool{} // stack or group IDs with new, unsnoozed updates
	for _, u := range fresh {
		ids = append(ids, u.ID)
		if u.Snoozed() {
			continue
		}
		if c, err := n.Store.Container(ctx, u.ContainerID); err == nil && c.GroupID != "" {
			scopes[c.GroupID] = true // group members are notified with their group
		} else {
			scopes[u.StackID] = true
		}
	}
	maintenance, err := n.Store.Maintenance(ctx)
	if err != nil {
		n.Log.Error("read maintenance mode", "err", err)
		return
	}
	if maintenance {
		n.Log.Info("maintenance mode: update notifications suppressed", "updates", len(ids))
		n.markNotified(ctx, ids)
		return
	}

	// Bot updates get their own message with a restart confirmation.
	if err := n.notifyBotUpdates(ctx, fresh); err != nil {
		n.Log.Error("notify bot update", "err", err)
	}

	var views []scopeView
	for scopeID := range scopes {
		v, err := loadAnyScope(ctx, n.Store, scopeID, now)
		if err != nil {
			n.Log.Error("load updates", "scope_id", scopeID, "err", err)
			continue
		}
		if len(v.Lines) > 0 {
			views = append(views, v)
		}
	}
	sort.Slice(views, func(i, j int) bool { return views[i].Name() < views[j].Name() })

	limit := n.Limit
	if limit < 1 {
		limit = 5
	}
	if len(views) > limit {
		if _, ok := n.post(ctx, "update summary", summaryMessage("Updates available", views, true)); !ok {
			return // retried after the next check
		}
	} else {
		for _, v := range views {
			if !n.postScope(ctx, v) {
				return
			}
		}
	}
	n.markNotified(ctx, ids)
}

// postScope posts one scope's update message and records it for editing.
func (n *Notifier) postScope(ctx context.Context, v scopeView) bool {
	offer, err := offerFor(ctx, n.Store, v, "notification", n.Now())
	if err != nil {
		n.Log.Error("create offer", "scope_id", v.ScopeID(), "err", err)
		return false
	}
	id, ok := n.post(ctx, v.Kind+" updates", scopeMessage(v, offer))
	if !ok {
		return false
	}
	if err := n.Store.AddDiscordMessage(ctx, scopeEntity(v.Kind), v.ScopeID(), n.ChannelID, id, n.Now()); err != nil {
		n.Log.Error("record update message", "scope_id", v.ScopeID(), "err", err)
	}
	return true
}

func (n *Notifier) markNotified(ctx context.Context, ids []string) {
	if err := n.Store.MarkNotified(ctx, ids, n.Now()); err != nil {
		n.Log.Error("mark updates notified", "err", err)
	}
}

// RefreshStacks edits the latest update message of each stack to show its
// current updates.
func (n *Notifier) RefreshStacks(ctx context.Context, stackIDs []string) {
	if len(stackIDs) == 0 {
		return
	}
	ids := append([]string(nil), stackIDs...)
	n.enqueue(ctx, func(ctx context.Context) {
		groups := map[string]bool{}
		for _, id := range ids {
			n.refreshStack(ctx, id)
			// Groups with members in this stack show its updates too.
			if cs, err := n.Store.Containers(ctx, store.ContainerFilter{StackID: id}); err == nil {
				for _, c := range cs {
					if c.GroupID != "" && !groups[c.GroupID] {
						groups[c.GroupID] = true
						n.refreshStack(ctx, c.GroupID)
					}
				}
			}
		}
	})
}

// refreshStack edits the latest update message of a stack or group.
func (n *Notifier) refreshStack(ctx context.Context, stackID string) {
	kind := "stack"
	if _, err := n.Store.GetGroup(ctx, stackID); err == nil {
		kind = "group"
	}
	msgs, err := n.Store.DiscordMessages(ctx, scopeEntity(kind), stackID)
	if err != nil || len(msgs) == 0 {
		return
	}
	v, err := loadAnyScope(ctx, n.Store, stackID, n.Now())
	if err != nil {
		n.Log.Error("load stack updates", "stack_id", stackID, "err", err)
		return
	}
	offer, err := offerFor(ctx, n.Store, v, "notification", n.Now())
	if err != nil {
		n.Log.Error("create offer", "stack_id", stackID, "err", err)
		return
	}
	last := msgs[len(msgs)-1]
	if err := n.Gateway.Edit(ctx, last.ChannelID, last.MessageID, scopeMessage(v, offer)); err != nil {
		n.Log.Warn("edit update message", "stack_id", stackID, "message_id", last.MessageID, "err", err)
	}
}

// MaintenanceSummary posts one concise summary of the currently actionable
// updates when maintenance mode ends.
func (n *Notifier) MaintenanceSummary(ctx context.Context) {
	n.enqueue(ctx, func(ctx context.Context) {
		lines, err := loadLines(ctx, n.Store, store.UpdateFilter{}, n.Now())
		if err != nil {
			n.Log.Error("load updates", "err", err)
			return
		}
		scopes := map[string]bool{}
		for _, l := range lines {
			if !l.U.Snoozed() {
				scopes[scopeOf(l)] = true
			}
		}
		var views []scopeView
		for id := range scopes {
			if v, err := loadAnyScope(ctx, n.Store, id, n.Now()); err == nil && len(v.Lines) > 0 {
				views = append(views, v)
			}
		}
		if len(views) == 0 {
			n.post(ctx, "maintenance summary", notice(iconOK, "Maintenance mode is off", "No updates are pending.", colorOK))
			return
		}
		n.post(ctx, "maintenance summary", summaryMessage("Maintenance is over · pending updates", views, true))
	})
}

// notifyBotUpdates posts one message per new, unsnoozed bot update.
func (n *Notifier) notifyBotUpdates(ctx context.Context, fresh []store.Update) error {
	policies, err := n.Store.Policies(ctx)
	if err != nil {
		return err
	}
	for _, u := range fresh {
		if u.Snoozed() {
			continue
		}
		c, err := n.Store.Container(ctx, u.ContainerID)
		if err != nil || c.Self != protocol.SelfBot {
			continue
		}
		l := lineFor(u, c, policies)
		if l.State == domain.StateIgnored {
			continue
		}
		offer, err := n.Store.CreateSelfUpdateOffer(ctx, u, n.Now())
		if err != nil {
			return err
		}
		n.post(ctx, "bot update", selfUpdateMessage(l, offer))
	}
	return nil
}

// entityGroupUpdates is the discord_messages entity type of a group's
// update message.
const entityGroupUpdates = "group_updates"

func scopeEntity(kind string) string {
	if kind == "group" {
		return entityGroupUpdates
	}
	return entityStackUpdates
}
