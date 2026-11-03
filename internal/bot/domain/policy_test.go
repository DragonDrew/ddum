package domain

import (
	"reflect"
	"testing"
	"time"
)

func s(v string) *string { return &v }
func b(v bool) *bool     { return &v }
func i(v int) *int       { return &v }

func TestResolvePolicyPerField(t *testing.T) {
	chain := []PolicyLevel{
		{LevelGlobal, PolicyFields{Mode: s(ModeManual), TagTrack: s(TrackSameMajor), VerifyTimeout: i(120), VerifyStable: i(30)}},
		{LevelHost, PolicyFields{TagTrack: s(TrackAny)}},
		{LevelStack, PolicyFields{Mode: s(ModeAutomatic)}},
		{LevelContainer, PolicyFields{VerifyStable: i(5)}},
	}
	e := ResolvePolicy(chain, false)
	if e.Mode != ModeAutomatic || e.TagTrack != TrackAny || e.VerifyTimeout != 120 || e.VerifyStable != 5 {
		t.Fatalf("effective = %+v", e)
	}
	want := map[string]string{FieldMode: LevelStack, FieldTagTrack: LevelHost, FieldAutoRollback: SourceFromMode,
		FieldVerifyTimeout: LevelGlobal, FieldVerifyStable: LevelContainer}
	if !reflect.DeepEqual(e.Source, want) {
		t.Fatalf("sources = %v, want %v", e.Source, want)
	}
	if !e.AutoRollback {
		t.Fatal("auto_rollback unset must default to true for automatic mode")
	}
}

func TestResolvePolicyAutoRollbackAndLabel(t *testing.T) {
	e := ResolvePolicy([]PolicyLevel{{LevelGlobal, PolicyFields{Mode: s(ModeManual)}}}, false)
	if e.AutoRollback {
		t.Fatal("auto_rollback defaults to false for manual")
	}
	e = ResolvePolicy([]PolicyLevel{
		{LevelGlobal, PolicyFields{Mode: s(ModeAutomatic)}},
		{LevelContainer, PolicyFields{AutoRollback: b(false), Mode: s(ModeAutomatic)}},
	}, false)
	if e.AutoRollback || e.Source[FieldAutoRollback] != LevelContainer {
		t.Fatalf("explicit auto_rollback: %+v", e)
	}
	e = ResolvePolicy([]PolicyLevel{{LevelContainer, PolicyFields{Mode: s(ModeAutomatic)}}}, true)
	if e.Mode != ModeIgnore || e.Source[FieldMode] != SourceIgnoreLabel || e.AutoRollback {
		t.Fatalf("ddum.ignore must force ignore over Discord policy: %+v", e)
	}
	e = ResolvePolicy(nil, false)
	if e.Mode != ModeManual || e.TagTrack != TrackSameMajor || e.Source[FieldMode] != SourceDefault {
		t.Fatalf("built-in defaults: %+v", e)
	}
}

func TestClassifyTracking(t *testing.T) {
	cases := []struct {
		name    string
		ref     string
		digests []string
		ignored bool
		mode    string
		tag     string
	}{
		{"version tag", "nginx:1.27-alpine", []string{"nginx@sha256:" + hex64("a")}, false, TrackingVersion, "1.27-alpine"},
		{"latest implicit", "nginx", []string{"nginx@sha256:" + hex64("a")}, false, TrackingDigest, "latest"},
		{"floating major", "redis:7-alpine", []string{"redis@sha256:" + hex64("b")}, false, TrackingDigest, "7-alpine"},
		{"pinned digest", "nginx@sha256:" + hex64("a"), []string{"nginx@sha256:" + hex64("a")}, false, TrackingUntracked, ""},
		{"local build", "myapp:dev", nil, false, TrackingUntracked, "dev"},
		{"digest of other repo", "ghcr.io/a/b:1.0", []string{"docker.io/a/b@sha256:" + hex64("c")}, false, TrackingUntracked, "1.0"},
		{"ignored", "nginx:1.27", []string{"nginx@sha256:" + hex64("a")}, true, TrackingUntracked, ""},
	}
	for _, c := range cases {
		got := ClassifyTracking(c.ref, c.digests, c.ignored)
		if got.Mode != c.mode || got.Tag != c.tag {
			t.Errorf("%s: %+v", c.name, got)
		}
		if got.Mode == TrackingUntracked && got.Reason == "" {
			t.Errorf("%s: untracked without a reason", c.name)
		}
	}
	got := ClassifyTracking("nginx:1.27", []string{"docker.io/library/nginx@sha256:" + hex64("d")}, false)
	if got.Repository != "docker.io/library/nginx" || len(got.Digests) != 1 || got.Digests[0] != "sha256:"+hex64("d") {
		t.Errorf("normalization: %+v", got)
	}
}

func hex64(c string) string {
	out := ""
	for len(out) < 64 {
		out += c
	}
	return out
}

func TestSnoozeActive(t *testing.T) {
	now := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	if !SnoozeActive(time.Time{}, false, now) {
		t.Error("indefinite snooze is active")
	}
	if !SnoozeActive(now.Add(time.Hour), false, now) || SnoozeActive(now, false, now) || SnoozeActive(now.Add(-time.Hour), false, now) {
		t.Error("timed snooze is active only until it expires")
	}
	if SnoozeActive(time.Time{}, true, now) {
		t.Error("removed snooze is not active")
	}
}

func TestUpdateStatePrecedence(t *testing.T) {
	ok := UpdateFacts{Kind: "digest", Mode: ModeManual, DeploySupported: true, TagChangeSupported: true, HostApproved: true, HostOnline: true}
	cases := []struct {
		name  string
		edit  func(*UpdateFacts)
		state string
	}{
		{"pending", func(*UpdateFacts) {}, StatePending},
		{"automatic", func(f *UpdateFacts) { f.Mode = ModeAutomatic }, StateAutomatic},
		{"ignored", func(f *UpdateFacts) { f.Mode = ModeIgnore; f.Protected = true }, StateIgnored},
		{"snoozed", func(f *UpdateFacts) { f.Snoozed = true }, StateSnoozed},
		{"held beats snoozed", func(f *UpdateFacts) { f.Snoozed = true; f.Protected = true }, StateHeld},
		{"offline", func(f *UpdateFacts) { f.HostOnline = false }, StateHeld},
		{"standalone", func(f *UpdateFacts) { f.DeploySupported = false; f.DeployReason = "standalone_manual" }, StateHeld},
		{"version tag source unsupported", func(f *UpdateFacts) { f.Kind = "version"; f.TagChangeSupported = false }, StateHeld},
		{"digest ignores tag source", func(f *UpdateFacts) { f.TagChangeSupported = false }, StatePending},
	}
	for _, c := range cases {
		f := ok
		c.edit(&f)
		if got, _ := UpdateState(f); got != c.state {
			t.Errorf("%s: %q, want %q", c.name, got, c.state)
		}
	}
}

func TestScopeState(t *testing.T) {
	cases := []struct {
		in   []string
		want string
	}{
		{[]string{StatePending, StateHeld}, StateHeld},
		{[]string{StateSnoozed, StateSnoozed, StateIgnored}, StateSnoozed},
		{[]string{StateSnoozed, StatePending}, StatePending},
		{[]string{StateAutomatic, StatePending}, StateAutomatic},
		{[]string{StateIgnored}, StateIgnored},
		{[]string{StateHeld, StateDeploying, StateSnoozed}, StateDeploying},
	}
	for _, c := range cases {
		if got := ScopeState(c.in); got != c.want {
			t.Errorf("ScopeState(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}
