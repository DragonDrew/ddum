package domain

import (
	"reflect"
	"slices"
	"testing"
)

func TestParseVersionTag(t *testing.T) {
	cases := []struct {
		tag      string
		ok       bool
		prefix   string
		numbers  []int
		stem     string
		build    int
		hasBuild bool
	}{
		{"v1.2.3", true, "v", []int{1, 2, 3}, "", 0, false},
		{"4.0.10.2544-ls257", true, "", []int{4, 0, 10, 2544}, "", 257, true},
		{"1.27.2-alpine-r2", true, "", []int{1, 27, 2}, "-alpine", 2, true},
		{"1.2.3-alpine3.20", true, "", []int{1, 2, 3}, "-alpine3.20", 0, false},
		{"16.4", true, "", []int{16, 4}, "", 0, false},
		{"1.27-alpine", true, "", []int{1, 27}, "-alpine", 0, false},
		{"2.0.0-build15", true, "", []int{2, 0, 0}, "", 15, true},
		{"16", false, "", nil, "", 0, false},
		{"latest", false, "", nil, "", 0, false},
		{"7-alpine", false, "", nil, "", 0, false},
		{"v", false, "", nil, "", 0, false},
	}
	for _, c := range cases {
		v, ok := ParseVersionTag(c.tag)
		if ok != c.ok {
			t.Errorf("%s: ok = %v, want %v", c.tag, ok, c.ok)
			continue
		}
		if !ok {
			continue
		}
		if v.Prefix != c.prefix || !reflect.DeepEqual(v.Numbers, c.numbers) || v.Stem != c.stem ||
			v.Build != c.build || v.HasBuild != c.hasBuild {
			t.Errorf("%s: got %+v", c.tag, v)
		}
	}
}

func TestSelectVersion(t *testing.T) {
	cases := []struct {
		name    string
		current string
		tags    []string
		track   string
		want    string
	}{
		{"newest same-major", "1.2.3", []string{"1.2.4", "1.3.0", "2.0.0", "1.2.2"}, TrackSameMajor, "1.3.0"},
		{"same-minor", "1.2.3", []string{"1.2.4", "1.3.0"}, TrackSameMinor, "1.2.4"},
		{"any", "1.2.3", []string{"1.2.4", "2.0.0"}, TrackAny, "2.0.0"},
		{"pinned offers nothing", "1.2.3", []string{"1.2.4"}, TrackPinned, ""},
		{"prefix must match", "v1.2.3", []string{"1.9.0", "v1.3.0"}, TrackAny, "v1.3.0"},
		{"part count must match", "16.4", []string{"16.5", "16.5.1", "17"}, TrackSameMajor, "16.5"},
		{"two-part same-minor is pinned for versions", "1.27", []string{"1.28"}, TrackSameMinor, ""},
		{"two-part next minor", "1.27", []string{"1.28", "1.27.3"}, TrackSameMajor, "1.28"},
		{"stem locked", "1.2.3-alpine", []string{"1.2.4", "1.2.5-alpine", "1.2.6-bookworm"}, TrackAny, "1.2.5-alpine"},
		{"alpine3.19 never gets alpine3.20", "1.2.3-alpine3.19", []string{"1.2.4-alpine3.20", "1.2.3-alpine3.20"}, TrackAny, ""},
		{"alpine3.19 to next version same base", "1.2.3-alpine3.19", []string{"1.2.4-alpine3.19"}, TrackAny, "1.2.4-alpine3.19"},
		{"build counter bump", "4.0.10.2544-ls257", []string{"4.0.10.2544-ls258", "4.0.10.2544-ls256"}, TrackSameMinor, "4.0.10.2544-ls258"},
		{"build counter: newer numbers win", "4.0.10.2544-ls257", []string{"4.0.10.2544-ls300", "4.0.11.2600-ls1"}, TrackSameMajor, "4.0.11.2600-ls1"},
		{"build counter required both ways", "4.0.10.2544-ls257", []string{"4.0.11.2600"}, TrackAny, ""},
		{"no build counter on current", "1.2.3", []string{"1.2.4-r1"}, TrackAny, ""},
		{"r counter with stem", "1.27.2-alpine-r2", []string{"1.27.2-alpine-r3", "1.27.3-r1"}, TrackSameMinor, "1.27.2-alpine-r3"},
		{"build bump not allowed when pinned", "1.2.3-ls5", []string{"1.2.3-ls6"}, TrackPinned, ""},
		{"pre-releases excluded", "1.2.3", []string{"1.3.0-rc1", "1.3.0-beta", "1.2.4"}, TrackAny, "1.2.4"},
		{"pre-release current may move to pre-release", "1.3.0-rc", []string{"1.3.1-rc"}, TrackAny, "1.3.1-rc"},
		{"not a version tag", "latest", []string{"1.0.0"}, TrackAny, ""},
		{"nothing newer", "2.0.0", []string{"1.0.0", "2.0.0"}, TrackAny, ""},
		{"numeric not lexical", "1.9.0", []string{"1.10.0"}, TrackAny, "1.10.0"},
	}
	for _, c := range cases {
		got, ok := SelectVersion(c.current, c.tags, c.track, false)
		if got != c.want || ok != (c.want != "") {
			t.Errorf("%s: SelectVersion(%s) = %q,%v, want %q", c.name, c.current, got, ok, c.want)
		}
	}
}

// TestSelectVersionPushOrdered lists tags oldest first, as ghcr.io and lscr.io
// do. The sonarr tags are the real ones from when linuxserver restarted its
// build counter.
func TestSelectVersionPushOrdered(t *testing.T) {
	reset := []string{"4.0.19.2979-ls324", "4.0.20.3014-ls328", "4.0.20.3014-ls329", "4.0.20.3014-ls1"}
	cases := []struct {
		name    string
		current string
		tags    []string
		track   string
		want    string
	}{
		{"counter reset: older high counter not offered", "4.0.20.3014-ls1", reset, TrackSameMajor, ""},
		{"counter reset: later low counter offered", "4.0.20.3014-ls329", reset, TrackSameMajor, "4.0.20.3014-ls1"},
		{"counter reset: builds after the reset followed", "4.0.20.3014-ls1",
			append(slices.Clone(reset), "4.0.20.3014-ls2", "4.0.20.3014-ls3"), TrackSameMajor, "4.0.20.3014-ls3"},
		{"counter reset: newer version still wins", "4.0.20.3014-ls329",
			append(slices.Clone(reset), "4.0.21.3100-ls2"), TrackSameMajor, "4.0.21.3100-ls2"},
		{"ordinary bump", "4.0.10.2544-ls257", []string{"4.0.10.2544-ls256", "4.0.10.2544-ls257", "4.0.10.2544-ls258"}, TrackSameMinor, "4.0.10.2544-ls258"},
		{"same position rules leave versions alone", "1.2.3", []string{"1.3.0", "1.2.4"}, TrackSameMajor, "1.3.0"},
		{"pinned still offers nothing", "4.0.20.3014-ls329", reset, TrackPinned, ""},
		{"current not listed: counter against current, position between candidates", "4.0.20.3014-ls5",
			[]string{"4.0.20.3014-ls329", "4.0.20.3014-ls6"}, TrackSameMajor, "4.0.20.3014-ls6"},
		{"current not listed: older counter not offered", "4.0.20.3014-ls5", []string{"4.0.20.3014-ls4"}, TrackSameMajor, ""},
	}
	for _, c := range cases {
		got, ok := SelectVersion(c.current, c.tags, c.track, true)
		if got != c.want || ok != (c.want != "") {
			t.Errorf("%s: SelectVersion(%s) = %q,%v, want %q", c.name, c.current, got, ok, c.want)
		}
	}
}

func TestIsDowngrade(t *testing.T) {
	cases := []struct {
		name            string
		current, target string
		want            bool
	}{
		{"the 240 case: 0.11.1 to 0.10", "0.11.1", "0.10", true},
		{"upgrade", "0.10", "0.11.1", false},
		{"same version", "1.2.3", "1.2.3", false},
		{"build counter is not compared", "4.0.10.2544-ls257", "4.0.10.2544-ls256", false},
		{"missing part counts as zero", "1.2.0", "1.2", false},
		{"lower build with lower numbers", "4.0.11.2600-ls1", "4.0.10.2544-ls900", true},
		{"other stem is never a downgrade", "1.2.3-alpine", "1.2.2", false},
		{"v prefix kept", "v1.3.0", "v1.2.9", true},
		{"latest is not a version", "latest", "1.0.0", false},
		{"target not a version", "1.2.3", "latest", false},
	}
	for _, c := range cases {
		if got := IsDowngrade(c.current, c.target); got != c.want {
			t.Errorf("%s: IsDowngrade(%s, %s) = %v, want %v", c.name, c.current, c.target, got, c.want)
		}
	}
}
