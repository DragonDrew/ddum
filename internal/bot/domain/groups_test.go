package domain

import (
	"strings"
	"testing"
)

func TestEvaluateGroup(t *testing.T) {
	m := func(name, mode string) MemberFacts { return MemberFacts{Name: name, Mode: mode, HostID: "h1"} }
	cases := []struct {
		name       string
		members    []MemberFacts
		held       string // prefix
		auto       bool
		manualOK   bool
		conflictIn string
	}{
		{"all automatic", []MemberFacts{m("app", ModeAutomatic), m("worker", ModeAutomatic)}, "", true, true, ""},
		{"all manual", []MemberFacts{m("app", ModeManual), m("worker", ModeManual)}, "", false, true, ""},
		{"mixed modes hold and name the member", []MemberFacts{m("app", ModeAutomatic), m("worker", ModeManual)},
			HoldPolicyConflict, false, true, "worker (manual)"},
		{"ignored member does not conflict", []MemberFacts{m("app", ModeAutomatic), m("db", ModeIgnore)}, "", true, true, ""},
		{"protected member does not conflict", []MemberFacts{m("app", ModeAutomatic), {Name: "proxy", Mode: ModeManual, Protected: true}}, "", true, true, ""},
		{"snoozed member holds auto-deploy", []MemberFacts{m("app", ModeAutomatic), {Name: "w", Mode: ModeAutomatic, HostID: "h1", Snoozed: true}},
			"", false, true, ""},
		{"spans hosts", []MemberFacts{m("app", ModeManual), {Name: "w", Mode: ModeManual, HostID: "h2"}}, HoldSpansHosts, false, false, ""},
		{"held member holds the group", []MemberFacts{m("app", ModeManual), {Name: "w", Mode: ModeManual, HostID: "h1", Held: "manual update required"}},
			"a member cannot be deployed", false, false, ""},
	}
	for _, c := range cases {
		ev := EvaluateGroup(c.members)
		if !strings.HasPrefix(ev.Held, c.held) || (c.held == "" && ev.Held != "") {
			t.Errorf("%s: held = %q, want prefix %q", c.name, ev.Held, c.held)
		}
		if ev.AutoDeploy != c.auto || ev.ManualApprovalAllowed() != c.manualOK {
			t.Errorf("%s: auto=%v manual=%v", c.name, ev.AutoDeploy, ev.ManualApprovalAllowed())
		}
		if c.conflictIn != "" && !strings.Contains(strings.Join(ev.Conflict, ","), c.conflictIn) {
			t.Errorf("%s: conflict = %v, want %s named", c.name, ev.Conflict, c.conflictIn)
		}
	}
}
