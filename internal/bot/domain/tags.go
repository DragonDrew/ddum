package domain

import (
	"cmp"
	"regexp"
	"strconv"
	"strings"
)

// Tag-track policy values.
const (
	TrackPinned    = "pinned"
	TrackSameMinor = "same-minor"
	TrackSameMajor = "same-major"
	TrackAny       = "any"
)

// versionTagRE matches a version tag: optional "v", two to four numeric
// parts, and an optional suffix.
var versionTagRE = regexp.MustCompile(`^(v?)(\d+(?:\.\d+){1,3})([-_.][A-Za-z0-9._-]+)?$`)

// buildCounterRE matches a trailing build counter. Only ls, r, and build are
// recognized, so base-image variants such as -alpine3.20 stay in the stem.
var buildCounterRE = regexp.MustCompile(`[-_.](ls|r|build)(\d+)$`)

var preReleaseWords = []string{"alpha", "beta", "rc", "dev", "nightly", "pre", "snapshot"}

// VersionTag is a parsed version tag.
type VersionTag struct {
	Raw      string
	Prefix   string // "v" or ""
	Numbers  []int
	Stem     string // suffix without the build counter
	Build    int
	HasBuild bool
}

// ParseVersionTag parses tag, reporting false if it is not a version tag.
func ParseVersionTag(tag string) (VersionTag, bool) {
	m := versionTagRE.FindStringSubmatch(tag)
	if m == nil {
		return VersionTag{}, false
	}
	v := VersionTag{Raw: tag, Prefix: m[1]}
	for _, p := range strings.Split(m[2], ".") {
		n, err := strconv.Atoi(p)
		if err != nil {
			return VersionTag{}, false // too large for int
		}
		v.Numbers = append(v.Numbers, n)
	}
	suffix := m[3]
	if b := buildCounterRE.FindStringSubmatchIndex(suffix); b != nil {
		n, err := strconv.Atoi(suffix[b[4]:b[5]])
		if err == nil {
			v.Build, v.HasBuild = n, true
			suffix = suffix[:b[0]]
		}
	}
	v.Stem = suffix
	return v, true
}

// IsPreRelease reports whether the stem marks a pre-release.
func (v VersionTag) IsPreRelease() bool {
	s := strings.ToLower(v.Stem)
	for _, w := range preReleaseWords {
		if strings.Contains(s, w) {
			return true
		}
	}
	return false
}

// comparable reports whether cand is in the same family as cur: same prefix,
// same number of numeric parts, exactly the same stem, and a build counter
// if and only if cur has one.
func (cur VersionTag) comparable(cand VersionTag) bool {
	return cur.Prefix == cand.Prefix && len(cur.Numbers) == len(cand.Numbers) &&
		cur.Stem == cand.Stem && cur.HasBuild == cand.HasBuild
}

// CompareVersions orders two tags of the same family by numbers, then build.
func CompareVersions(a, b VersionTag) int {
	if c := compareNumbers(a, b); c != 0 {
		return c
	}
	return cmp.Compare(a.Build, b.Build)
}

// compareNumbers orders two tags by their numeric parts only.
func compareNumbers(a, b VersionTag) int {
	for i := 0; i < len(a.Numbers) && i < len(b.Numbers); i++ {
		if c := cmp.Compare(a.Numbers[i], b.Numbers[i]); c != 0 {
			return c
		}
	}
	return cmp.Compare(len(a.Numbers), len(b.Numbers))
}

// IsDowngrade reports whether desired is an older release than current. Both
// must be version tags with the same prefix and stem. The numeric parts are
// compared, with a missing part counting as zero, so 0.10 is older than
// 0.11.1. A build counter is not compared, since its order depends on how
// the publisher numbers it.
func IsDowngrade(current, desired string) bool {
	c, ok := compareTags(current, desired)
	return ok && c < 0
}

// IsUpgrade reports whether desired is a newer release than current, judged
// like IsDowngrade: by the numeric parts of two tags of one family.
func IsUpgrade(current, desired string) bool {
	c, ok := compareTags(current, desired)
	return ok && c > 0
}

// compareTags orders desired against current by their numeric parts. It
// reports false unless both are version tags with the same prefix and stem.
func compareTags(current, desired string) (int, bool) {
	cur, okCur := ParseVersionTag(current)
	des, okDes := ParseVersionTag(desired)
	if !okCur || !okDes || cur.Prefix != des.Prefix || cur.Stem != des.Stem {
		return 0, false
	}
	for i := range max(len(cur.Numbers), len(des.Numbers)) {
		if c := cmp.Compare(partAt(des.Numbers, i), partAt(cur.Numbers, i)); c != 0 {
			return c, true
		}
	}
	return 0, true
}

// partAt returns the i-th numeric part, or zero past the end.
func partAt(numbers []int, i int) int {
	if i < len(numbers) {
		return numbers[i]
	}
	return 0
}

// satisfiesTrack reports whether moving from cur to cand is allowed by the
// tag_track policy. "major" is the first number and "minor" the second; a
// build-counter bump with equal numbers satisfies everything except pinned.
func satisfiesTrack(cur, cand VersionTag, track string) bool {
	switch track {
	case TrackPinned:
		return false
	case TrackSameMinor:
		return cand.Numbers[0] == cur.Numbers[0] && cand.Numbers[1] == cur.Numbers[1]
	case TrackSameMajor:
		return cand.Numbers[0] == cur.Numbers[0]
	case TrackAny:
		return true
	}
	return false
}

// SelectVersion picks the newest tag that may replace current under track
// . It returns false when there is none.
//
// pushOrdered says tags are listed in push order, oldest first (ghcr.io,
// lscr.io). Two tags with the same numbers are then ordered by position, not
// by build counter, so a publisher restarting its counter is followed: when
// linuxserver went from sonarr 4.0.20.3014-ls329 to 4.0.20.3014-ls1, counters
// alone offered ls329 to ls1 forever and never offered ls2 to ls329.
// A tag missing from the list falls back to the counter.
func SelectVersion(current string, tags []string, track string, pushOrdered bool) (string, bool) {
	cur, ok := ParseVersionTag(current)
	if !ok || track == TrackPinned {
		return "", false
	}
	var pos map[string]int
	if pushOrdered {
		pos = make(map[string]int, len(tags))
		for i, t := range tags {
			pos[t] = i // a tag listed twice counts where it was listed last
		}
	}
	newer := func(a, b VersionTag) int {
		if c := compareNumbers(a, b); c != 0 {
			return c
		}
		pa, okA := pos[a.Raw]
		pb, okB := pos[b.Raw]
		if okA && okB {
			return cmp.Compare(pa, pb)
		}
		return cmp.Compare(a.Build, b.Build)
	}
	curPre := cur.IsPreRelease()
	var best VersionTag
	found := false
	for _, t := range tags {
		cand, ok := ParseVersionTag(t)
		if !ok || !cur.comparable(cand) {
			continue
		}
		if cand.IsPreRelease() && !curPre {
			continue
		}
		if newer(cand, cur) <= 0 || !satisfiesTrack(cur, cand, track) {
			continue
		}
		if !found || newer(cand, best) > 0 {
			best, found = cand, true
		}
	}
	return best.Raw, found
}
