package discord

import (
	"context"
	"fmt"
	"time"

	"github.com/distribution/reference"

	"github.com/DragonDrew/ddum/internal/bot/domain"
	"github.com/DragonDrew/ddum/internal/bot/store"
	"github.com/DragonDrew/ddum/internal/protocol"
)

// entityStackUpdates is the discord_messages entity type of a stack's
// update message, edited when its updates change.
const entityStackUpdates = "stack_updates"

// updateLine is one update with everything needed to display it.
type updateLine struct {
	U      store.Update
	C      store.Container
	Policy domain.EffectivePolicy
	State  string
	Reason string // why Held
}

// scopeView is the pending updates of one scope: a stack, or
// an update group, which may span several stacks.
type scopeView struct {
	Kind     string // stack | group
	Stack    store.Stack
	Group    store.Group
	Host     store.Host // the stack's host, or the group's when it has one
	Lines    []updateLine
	Headline string
	// Group scope only.
	HeldReason string // why the whole group is held
	ManualOK   bool   // a user may approve the whole group
}

// ScopeID is the stack or group ID.
func (v scopeView) ScopeID() string {
	if v.Kind == "group" {
		return v.Group.ID
	}
	return v.Stack.ID
}

// Name is how the scope is shown.
func (v scopeView) Name() string {
	if v.Kind == "group" {
		return "group " + v.Group.DisplayName
	}
	return v.Stack.DisplayName
}

// approvable reports whether Approve can do anything: some update is pending
// approval (manual, not held, not snoozed). Automatic, held, protected, and
// snoozed members alone leave the button disabled.
func (v scopeView) approvable() bool {
	if v.Kind == "group" {
		// A group is approved whole, whatever its members' modes.
		return v.ManualOK && len(v.offerLines()) > 0
	}
	for _, l := range v.Lines {
		if l.State == domain.StatePending {
			return true
		}
	}
	return false
}

// snoozable returns the lines a Snooze button may act on.
func (v scopeView) snoozable() []updateLine {
	var out []updateLine
	for _, l := range v.Lines {
		if !l.U.Snoozed() && l.U.Status == store.StatusDetected {
			out = append(out, l)
		}
	}
	return out
}

// facts builds the displayed-state inputs for one update.
func facts(u store.Update, c store.Container, eff domain.EffectivePolicy) domain.UpdateFacts {
	f := domain.UpdateFacts{
		Kind: u.Kind, Mode: eff.Mode, Snoozed: u.Snoozed(), Protected: c.Protected,
		DeploySupported: c.StackDeploySupported, DeployReason: c.StackDeployReason,
		HostApproved: c.HostApproval == domain.ApprovalApproved, HostOnline: c.HostConnectivity == domain.Online,
		AutoFailed: u.AutoFailed,
	}
	if c.TagChange != nil {
		f.TagChangeSupported, f.TagChangeReason = c.TagChange.Supported, c.TagChange.Reason
	}
	return f
}

// lineFor resolves policy and state for one update.
func lineFor(u store.Update, c store.Container, policies map[store.PolicyKey]domain.PolicyFields) updateLine {
	eff := domain.ResolvePolicy(store.Chain(policies, c.HostID, c.StackID, c.GroupID, c.ID), domain.LabelIgnore(c.DdumLabels))
	state, reason := domain.UpdateState(facts(u, c, eff))
	if u.Status == store.StatusApproved || u.Status == store.StatusDeploying {
		state, reason = domain.StateDeploying, ""
	}
	return updateLine{U: u, C: c, Policy: eff, State: state, Reason: reason}
}

// loadLines returns display lines for detected (or, with f.Statuses, other),
// visible updates matching f. Updates of ignored containers are left out.
func loadLines(ctx context.Context, db *store.Store, f store.UpdateFilter, now time.Time) ([]updateLine, error) {
	if len(f.Statuses) == 0 {
		f.Statuses = []string{store.StatusDetected}
	}
	f.Visible = true
	ups, err := db.Updates(ctx, f, now)
	if err != nil {
		return nil, err
	}
	policies, err := db.Policies(ctx)
	if err != nil {
		return nil, err
	}
	var out []updateLine
	for _, u := range ups {
		c, err := db.Container(ctx, u.ContainerID)
		if err != nil {
			return nil, err
		}
		l := lineFor(u, c, policies)
		if l.State == domain.StateIgnored {
			continue
		}
		out = append(out, l)
	}
	return out, nil
}

// loadScope returns the view of one stack.
func loadScope(ctx context.Context, db *store.Store, stackID string, now time.Time) (scopeView, error) {
	st, err := db.Stack(ctx, stackID)
	if err != nil {
		return scopeView{}, err
	}
	h, err := db.Host(ctx, st.HostID)
	if err != nil {
		return scopeView{}, err
	}
	lines, err := loadLines(ctx, db, store.UpdateFilter{StackID: stackID, NoGroup: true,
		Statuses: []string{store.StatusDetected, store.StatusApproved, store.StatusDeploying}}, now)
	if err != nil {
		return scopeView{}, err
	}
	// The bot's own update is offered in a separate message.
	var kept []updateLine
	for _, l := range lines {
		if l.C.Self != protocol.SelfBot || l.State == domain.StateDeploying {
			kept = append(kept, l)
		}
	}
	lines = kept
	v := scopeView{Kind: "stack", Stack: st, Host: h, Lines: lines}
	// Protected members and the bot are left out of the scope like ignored
	// ones: they never make the stack Held.
	var states []string
	for _, l := range lines {
		if l.C.Protected || l.C.Self == protocol.SelfBot {
			continue
		}
		states = append(states, l.State)
	}
	v.Headline = domain.ScopeState(states)
	if v.Headline == domain.StateIgnored && len(lines) > 0 {
		v.Headline = domain.StateHeld // only protected members are pending
	}
	return v, nil
}

// tagOf returns the tag of an image reference ("latest" if none).
func tagOf(ref string) string {
	n, err := reference.ParseNormalizedNamed(ref)
	if err != nil {
		return ref
	}
	if t, ok := n.(reference.Tagged); ok {
		return t.Tag()
	}
	return "latest"
}

// familiar shortens "docker.io/library/nginx:1.27@sha256:…" to "nginx:1.27".
func familiar(ref string) string {
	n, err := reference.ParseNormalizedNamed(ref)
	if err != nil {
		return ref
	}
	s := reference.FamiliarName(n)
	if t, ok := n.(reference.Tagged); ok {
		s += ":" + t.Tag()
	}
	return s
}

// offerLines are the updates an offer covers: for a stack its unsnoozed
// pending updates; for a group every actionable member update, snoozed ones
// included, since a group offer must show them all.
func (v scopeView) offerLines() []updateLine {
	if v.Kind != "group" {
		return v.snoozable()
	}
	var out []updateLine
	for _, l := range v.Lines {
		if l.U.Status == store.StatusDetected && !l.C.Protected {
			out = append(out, l)
		}
	}
	return out
}

// offerFor records an offer covering the view's updates.
func offerFor(ctx context.Context, db *store.Store, v scopeView, createdBy string, now time.Time) (string, error) {
	var items []store.OfferItem
	for _, l := range v.offerLines() {
		items = append(items, store.OfferItem{UpdateID: l.U.ID, TargetDigest: l.U.TargetDigest})
	}
	if len(items) == 0 {
		return "", nil
	}
	return db.CreateOffer(ctx, v.Kind, v.ScopeID(), items, createdBy, now)
}

// loadGroupScope returns the view of one update group: every pending member
// update, across its stacks, judged by the group rules.
func loadGroupScope(ctx context.Context, db *store.Store, groupID string, now time.Time) (scopeView, error) {
	g, err := db.GetGroup(ctx, groupID)
	if err != nil {
		return scopeView{}, err
	}
	lines, err := loadLines(ctx, db, store.UpdateFilter{GroupID: groupID,
		Statuses: []string{store.StatusDetected, store.StatusApproved, store.StatusDeploying}}, now)
	if err != nil {
		return scopeView{}, err
	}
	policies, err := db.Policies(ctx)
	if err != nil {
		return scopeView{}, err
	}
	v := scopeView{Kind: "group", Group: g, Lines: lines}
	var members []domain.MemberFacts
	var states []string
	hosts := map[string]bool{}
	deploying := false
	for _, l := range lines {
		if l.State == domain.StateDeploying {
			deploying = true
			continue
		}
		m, _ := store.MemberFactsFor(l.U, l.C, policies)
		members = append(members, m)
		if !l.C.Protected {
			states = append(states, l.State)
			hosts[l.C.HostID] = true
			if v.Host.ID == "" {
				v.Host, _ = db.Host(ctx, l.C.HostID)
			}
		}
	}
	ev := domain.EvaluateGroup(members)
	v.HeldReason, v.ManualOK = ev.Held, ev.ManualApprovalAllowed()
	switch {
	case deploying:
		v.Headline = domain.StateDeploying
	case ev.Held != "":
		v.Headline = domain.StateHeld
	case ev.Snoozed:
		v.Headline = domain.StateSnoozed // for a group, any snoozed member
	default:
		v.Headline = domain.ScopeState(states)
	}
	if len(hosts) > 1 {
		v.Host = store.Host{DisplayName: fmt.Sprintf("%d hosts", len(hosts))}
	}
	return v, nil
}

// loadAnyScope loads a stack or a group view by ID.
func loadAnyScope(ctx context.Context, db *store.Store, id string, now time.Time) (scopeView, error) {
	if _, err := db.GetGroup(ctx, id); err == nil {
		return loadGroupScope(ctx, db, id, now)
	}
	return loadScope(ctx, db, id, now)
}

// scopeOf is the scope an update belongs to: its group, else its stack.
func scopeOf(l updateLine) string {
	if l.C.GroupID != "" {
		return l.C.GroupID
	}
	return l.U.StackID
}
