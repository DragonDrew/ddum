package domain

import (
	"sort"
	"strings"
)

// Group-level hold reasons.
const (
	HoldPolicyConflict = "policy conflict"
	HoldSpansHosts     = "group_spans_hosts"
)

// MemberFacts describes one group member with a pending update.
type MemberFacts struct {
	Name      string
	Mode      string // effective mode
	HostID    string
	Snoozed   bool
	Protected bool
	Held      string // why this member's update is held, or ""
}

// GroupEvaluation is the outcome of the group-scope rules.
type GroupEvaluation struct {
	// Held is why the group cannot be deployed at all ("" if it can).
	Held string
	// Conflict names the members whose modes disagree; manual approval of
	// the whole group stays possible.
	Conflict []string
	// Mode is the members' common mode when there is no conflict.
	Mode string
	// AutoDeploy reports whether the policy engine may deploy the group.
	AutoDeploy bool
	// Snoozed reports that some member's update is snoozed, which holds the
	// group's automatic deployment (state Snoozed).
	Snoozed bool
}

// EvaluateGroup applies the coupling rules of a group scope. Ignored and
// protected members are left out and never hold the group.
func EvaluateGroup(members []MemberFacts) GroupEvaluation {
	var ev GroupEvaluation
	hosts := map[string]bool{}
	modes := map[string][]string{}
	var held []string
	for _, m := range members {
		if m.Mode == ModeIgnore || m.Protected {
			continue
		}
		hosts[m.HostID] = true
		modes[m.Mode] = append(modes[m.Mode], m.Name)
		if m.Snoozed {
			ev.Snoozed = true
		}
		if m.Held != "" {
			held = append(held, m.Name+": "+m.Held)
		}
	}
	switch {
	case len(hosts) > 1:
		ev.Held = HoldSpansHosts + ": the group's members are on more than one host"
	case len(held) > 0:
		// Deploying part of a coupled group is not allowed.
		sort.Strings(held)
		ev.Held = "a member cannot be deployed: " + strings.Join(held, "; ")
	}
	if len(modes) > 1 {
		for mode, names := range modes {
			for _, n := range names {
				ev.Conflict = append(ev.Conflict, n+" ("+mode+")")
			}
		}
		sort.Strings(ev.Conflict)
		if ev.Held == "" {
			ev.Held = HoldPolicyConflict + ": " + strings.Join(ev.Conflict, ", ")
		}
	} else {
		for mode := range modes {
			ev.Mode = mode
		}
	}
	ev.AutoDeploy = ev.Held == "" && ev.Mode == ModeAutomatic && !ev.Snoozed
	return ev
}

// ManualApprovalAllowed reports whether a user may approve the whole group:
// everything except a cross-host group or a held member; a policy conflict
// alone does not prevent it.
func (ev GroupEvaluation) ManualApprovalAllowed() bool {
	return ev.Held == "" || (len(ev.Conflict) > 0 && strings.HasPrefix(ev.Held, HoldPolicyConflict))
}
