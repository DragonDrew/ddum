// Package orchestrator runs the bot's decisions that span components: the
// automatic-update engine and the operations sweep.
package orchestrator

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/DragonDrew/ddum/internal/bot/domain"
	"github.com/DragonDrew/ddum/internal/bot/store"
)

// Notifier posts what the orchestrator did.
type Notifier interface {
	AutoQueued(ctx context.Context, res store.ApproveResult)
	OperationFinished(ctx context.Context, op store.Operation)
	OperationsExpired(ctx context.Context, ops []store.Operation)
}

// AutoDeployer queues operations for updates whose containers resolve to
// automatic. It runs after each registry check and each
// applied inventory, and never while maintenance mode is on.
type AutoDeployer struct {
	Store         *store.Store
	Notify        Notifier
	Log           *slog.Logger
	Now           func() time.Time
	ExpiryMinutes int

	trigger     chan struct{}
	triggerOnce sync.Once
}

func (a *AutoDeployer) triggers() chan struct{} {
	a.triggerOnce.Do(func() { a.trigger = make(chan struct{}, 1) })
	return a.trigger
}

// Trigger asks for an evaluation soon; it never blocks.
func (a *AutoDeployer) Trigger() {
	select {
	case a.triggers() <- struct{}{}:
	default:
	}
}

// Run evaluates on each trigger, at most every two seconds, until ctx ends.
func (a *AutoDeployer) Run(ctx context.Context) {
	trigger := a.triggers()
	for {
		select {
		case <-ctx.Done():
			return
		case <-trigger:
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second): // debounce bursts of inventories
		}
		a.Evaluate(ctx)
	}
}

// Evaluate creates at most one operation per stack: every actionable,
// unsnoozed update whose container resolves to automatic and is not protected
// or held. Manual members of the same stack stay pending.
// Each operation goes through the same offer and approval checks as a click,
// with the policy engine as the actor.
func (a *AutoDeployer) Evaluate(ctx context.Context) []store.ApproveResult {
	now := a.Now()
	if on, err := a.Store.Maintenance(ctx); err != nil || on {
		return nil
	}
	ups, err := a.Store.Updates(ctx, store.UpdateFilter{Statuses: []string{store.StatusDetected}, Visible: true}, now)
	if err != nil {
		a.Log.Error("auto-update: load updates", "err", err)
		return nil
	}
	policies, err := a.Store.Policies(ctx)
	if err != nil {
		a.Log.Error("auto-update: load policies", "err", err)
		return nil
	}
	byStack := map[string][]store.OfferItem{}
	var order []string
	for _, u := range ups {
		if u.Snoozed() || u.AutoFailed {
			continue // never auto-deployed: a snooze, or a failed attempt
		}
		c, err := a.Store.Container(ctx, u.ContainerID)
		if err != nil {
			continue
		}
		eff := domain.ResolvePolicy(store.Chain(policies, c.HostID, c.StackID, c.GroupID, c.ID), domain.LabelIgnore(c.DdumLabels))
		if c.GroupID != "" {
			continue // group members deploy with their group, below
		}
		if eff.Mode != domain.ModeAutomatic || c.Protected || c.Self != "" {
			continue // the bot is never updated automatically
		}
		if _, ok := byStack[u.StackID]; !ok {
			order = append(order, u.StackID)
		}
		byStack[u.StackID] = append(byStack[u.StackID], store.OfferItem{UpdateID: u.ID, TargetDigest: u.TargetDigest})
	}
	var out []store.ApproveResult
	out = append(out, a.evaluateGroups(ctx, ups, policies)...)
	for _, stackID := range order {
		offer, err := a.Store.CreateOffer(ctx, "stack", stackID, byStack[stackID], "policy", now)
		if err != nil {
			a.Log.Error("auto-update: create offer", "stack_id", stackID, "err", err)
			continue
		}
		res, err := a.Store.Approve(ctx, store.ApproveRequest{OfferID: offer, By: store.Actor{Kind: "policy"}, At: now,
			ExpiryMinutes: a.ExpiryMinutes})
		var nothing *store.NothingError
		switch {
		case errors.As(err, &nothing):
			a.Log.Debug("auto-update: nothing deployable", "stack_id", stackID, "skipped", len(nothing.Skipped))
			continue
		case err != nil:
			a.Log.Error("auto-update: approve", "stack_id", stackID, "err", err)
			continue
		}
		a.Log.Info("auto-update queued", "operation_id", res.Operation.ID, "stack_id", stackID, "updates", len(res.Approved))
		if a.Notify != nil {
			a.Notify.AutoQueued(ctx, res)
		}
		out = append(out, res)
	}
	return out
}

// Sweeper applies lease and command expiry every Every.
type Sweeper struct {
	Store  *store.Store
	Notify Notifier
	Log    *slog.Logger
	Now    func() time.Time
	Every  time.Duration
}

// Run sweeps until ctx ends.
func (s *Sweeper) Run(ctx context.Context) {
	t := time.NewTicker(s.Every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.Sweep(ctx)
		}
	}
}

// Sweep runs once.
func (s *Sweeper) Sweep(ctx context.Context) {
	res, err := s.Store.SweepOperations(ctx, s.Now())
	if err != nil {
		if ctx.Err() == nil {
			s.Log.Error("operations sweep", "err", err)
		}
		return
	}
	for _, op := range res.Requeued {
		s.Log.Warn("lease expired; command re-queued", "operation_id", op.ID, "attempt", op.Attempt)
	}
	if s.Notify == nil {
		return
	}
	for _, op := range res.Failed {
		s.Notify.OperationFinished(ctx, op)
	}
	if len(res.Expired) > 0 {
		s.Notify.OperationsExpired(ctx, res.Expired)
	}
}

// RollbackNotifier posts automatic rollbacks.
type RollbackNotifier interface {
	RollbackQueued(ctx context.Context, op store.Operation, auto bool)
}

// AutoRollback queues a rollback for a deployment that failed, partially
// failed, or was interrupted when auto_rollback resolves true for any
// container it changed and its snapshot is available.
// A bot self-update is never rolled back this way: the agent already did.
type AutoRollback struct {
	Store         *store.Store
	Notify        RollbackNotifier
	Log           *slog.Logger
	Now           func() time.Time
	ExpiryMinutes int
}

// OperationFinished is called for every finished operation.
func (a *AutoRollback) OperationFinished(ctx context.Context, op store.Operation) {
	if op.Kind != "deploy" || op.Payload.SelfUpdate {
		return
	}
	switch op.State {
	case "failed", "partially_failed", "interrupted":
	default:
		return
	}
	if store.ChangedNothing(op.State, op.Outcome.Code) {
		return // nothing to restore, and restoring would touch the stack for nothing
	}
	policies, err := a.Store.Policies(ctx)
	if err != nil {
		a.Log.Error("auto-rollback: load policies", "err", err)
		return
	}
	want := false
	for _, t := range op.Payload.Targets {
		for _, svc := range t.Services {
			c, err := a.Store.Container(ctx, svc.ContainerID)
			if err != nil {
				continue
			}
			if domain.ResolvePolicy(store.Chain(policies, c.HostID, c.StackID, c.GroupID, c.ID), domain.LabelIgnore(c.DdumLabels)).AutoRollback {
				want = true
			}
		}
	}
	if !want {
		return
	}
	rb, err := a.Store.CreateRollback(ctx, op.ID, store.Actor{Kind: "policy"}, a.Now(), a.ExpiryMinutes)
	if err != nil {
		a.Log.Warn("auto-rollback not possible", "operation_id", op.ID, "err", err)
		return
	}
	a.Log.Info("auto-rollback queued", "operation_id", rb.ID, "original_operation_id", op.ID)
	if a.Notify != nil {
		a.Notify.RollbackQueued(ctx, rb, true)
	}
}

// evaluateGroups queues one operation per group that may be deployed
// automatically: every non-ignored member automatic, nothing held, nothing
// snoozed.
func (a *AutoDeployer) evaluateGroups(ctx context.Context, ups []store.Update, policies map[store.PolicyKey]domain.PolicyFields) []store.ApproveResult {
	type group struct {
		members []domain.MemberFacts
		items   []store.OfferItem
		bot     bool
		failed  bool // a member's automatic deployment failed
	}
	groups := map[string]*group{}
	var order []string
	for _, u := range ups {
		c, err := a.Store.Container(ctx, u.ContainerID)
		if err != nil || c.GroupID == "" {
			continue
		}
		g := groups[c.GroupID]
		if g == nil {
			g = &group{}
			groups[c.GroupID] = g
			order = append(order, c.GroupID)
		}
		m, actionable := store.MemberFactsFor(u, c, policies)
		g.members = append(g.members, m)
		if actionable {
			g.items = append(g.items, store.OfferItem{UpdateID: u.ID, TargetDigest: u.TargetDigest})
			g.bot = g.bot || c.Self != ""
			g.failed = g.failed || u.AutoFailed
		}
	}
	var out []store.ApproveResult
	for _, id := range order {
		g := groups[id]
		if len(g.items) == 0 || g.bot || g.failed || !domain.EvaluateGroup(g.members).AutoDeploy {
			continue
		}
		offer, err := a.Store.CreateOffer(ctx, "group", id, g.items, "policy", a.Now())
		if err != nil {
			a.Log.Error("auto-update: create group offer", "group_id", id, "err", err)
			continue
		}
		res, err := a.Store.Approve(ctx, store.ApproveRequest{OfferID: offer, By: store.Actor{Kind: "policy"}, At: a.Now(),
			ExpiryMinutes: a.ExpiryMinutes})
		if err != nil {
			a.Log.Debug("auto-update: group not deployed", "group_id", id, "err", err)
			continue
		}
		a.Log.Info("auto-update queued for group", "operation_id", res.Operation.ID, "group_id", id, "updates", len(res.Approved))
		if a.Notify != nil {
			a.Notify.AutoQueued(ctx, res)
		}
		out = append(out, res)
	}
	return out
}
