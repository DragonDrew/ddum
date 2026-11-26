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

// versionTagRE matches a version tag. Its parts are:
//   - a prefix: any run of words joined by - or _, such as release-, version-,
//     nightly-version-, or an architecture tag's amd64-;
//   - an optional "v";
//   - one to four numeric parts;
//   - an optional suffix, which is split into a stem and a build counter.
var versionTagRE = regexp.MustCompile(`^((?:[A-Za-z][A-Za-z0-9]*[-_])*)(v?)(\d+(?:\.\d+){0,3})([-_.][A-Za-z0-9._-]+)?$`)

// dateTagRE matches a date written with dashes, such as 2026-11-20. It is
// checked first, because versionTagRE would read it as the number 2026 with a
// stem. Dotted and undotted dates (2026.11.20, 20261120) match versionTagRE.
var dateTagRE = regexp.MustCompile(`^(\d{4})-(\d{2})-(\d{2})$`)

// buildCounterRE matches a trailing build counter. Only ls, r, and build are
// recognized, so base-image variants such as -alpine3.20 stay in the stem.
var buildCounterRE = regexp.MustCompile(`[-_.](ls|r|build)(\d+)$`)

// hashRE matches a commit-hash suffix, such as Plex's -b3b13e506. Its build
// hashes change with every release, so it is dropped from the stem. A suffix
// is only a hash if it also has a digit and a letter (see isHash).
var hashRE = regexp.MustCompile(`^-[0-9a-f]{7,40}$`)

var preReleaseWords = []string{"alpha", "beta", "rc", "dev", "develop", "nightly", "pre", "snapshot", "testing", "unstable"}

// VersionTag is a parsed version tag.
type VersionTag struct {
	Raw      string
	Prefix   string // word prefix plus "v", such as "v", "release-" or ""
	Numbers  []int
	Stem     string // suffix without the build counter or commit hash
	Build    int
	HasBuild bool
}

// ParseVersionTag parses tag, reporting false if it is not a version tag.
func ParseVersionTag(tag string) (VersionTag, bool) {
	if m := dateTagRE.FindStringSubmatch(tag); m != nil {
		v := VersionTag{Raw: tag}
		for _, p := range m[1:] {
			n, _ := strconv.Atoi(p) // four and two digits: always fits
			v.Numbers = append(v.Numbers, n)
		}
		return v, true
	}
	m := versionTagRE.FindStringSubmatch(tag)
	if m == nil {
		return VersionTag{}, false
	}
	v := VersionTag{Raw: tag, Prefix: m[1] + m[2]}
	for _, p := range strings.Split(m[3], ".") {
		n, err := strconv.Atoi(p)
		if err != nil {
			return VersionTag{}, false // too large for int
		}
		v.Numbers = append(v.Numbers, n)
	}
	// A word and one bare number, such as pr-12, is an ID, not a version.
	if m[1] != "" && len(v.Numbers) == 1 {
		return VersionTag{}, false
	}
	suffix := m[4]
	if b := buildCounterRE.FindStringSubmatchIndex(suffix); b != nil {
		n, err := strconv.Atoi(suffix[b[4]:b[5]])
		if err == nil {
			v.Build, v.HasBuild = n, true
			suffix = suffix[:b[0]]
		}
	}
	if isHash(suffix) {
		suffix = ""
	}
	v.Stem = suffix
	return v, true
}

// isHash reports whether suffix is a commit-hash-like part such as -b3b13e506:
// hex of 7 to 40 characters with at least one digit and one letter, so that
// a word such as -cafe or a number such as -1234567 is not mistaken for one.
func isHash(suffix string) bool {
	if !hashRE.MatchString(suffix) {
		return false
	}
	return strings.ContainsAny(suffix, "0123456789") && strings.ContainsAny(suffix, "abcdef")
}

// IsPreRelease reports whether the tag's prefix or stem marks a pre-release.
func (v VersionTag) IsPreRelease() bool {
	s := strings.ToLower(v.Prefix + v.Stem)
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
		return partAt(cand.Numbers, 0) == partAt(cur.Numbers, 0) && partAt(cand.Numbers, 1) == partAt(cur.Numbers, 1)
	case TrackSameMajor:
		return partAt(cand.Numbers, 0) == partAt(cur.Numbers, 0)
	case TrackAny:
		return true
	}
	return false
}

// SelectVersion picks the newest tag that may replace current under track.
// It returns false when there is none.
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
