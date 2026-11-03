package domain

import (
	"strings"
	"time"

	"github.com/distribution/reference"
)

// Policy modes.
const (
	ModeManual    = "manual"
	ModeAutomatic = "automatic"
	ModeIgnore    = "ignore"
)

// Policy levels, least to most specific.
const (
	LevelGlobal    = "global"
	LevelHost      = "host"
	LevelStack     = "stack"
	LevelGroup     = "group"
	LevelContainer = "container"
)

// Policy field names, as shown by /policy show.
const (
	FieldMode          = "mode"
	FieldTagTrack      = "tag_track"
	FieldAutoRollback  = "auto_rollback"
	FieldVerifyTimeout = "verify_timeout_seconds"
	FieldVerifyStable  = "verify_stable_seconds"
)

// Sources reported for fields not set by any policy row.
const (
	SourceDefault     = "default"
	SourceFromMode    = "default (follows mode)"
	SourceIgnoreLabel = "label ddum.ignore=true"
)

// PolicyFields is one policy row; nil fields are unset and inherit.
type PolicyFields struct {
	Mode          *string
	TagTrack      *string
	AutoRollback  *bool
	VerifyTimeout *int
	VerifyStable  *int
}

// PolicyLevel is the policy row (possibly empty) at one level.
type PolicyLevel struct {
	Level  string
	Fields PolicyFields
}

// EffectivePolicy is a resolved policy with the level that set each field.
type EffectivePolicy struct {
	Mode          string
	TagTrack      string
	AutoRollback  bool
	VerifyTimeout int
	VerifyStable  int
	Source        map[string]string
}

// ResolvePolicy resolves fields independently: for each field the most
// specific level that sets it wins. chain is ordered
// from least to most specific. labelIgnore applies ddum.ignore=true, which
// forces mode ignore and cannot be overridden from Discord.
func ResolvePolicy(chain []PolicyLevel, labelIgnore bool) EffectivePolicy {
	e := EffectivePolicy{
		Mode: ModeManual, TagTrack: TrackSameMajor, VerifyTimeout: 120, VerifyStable: 30,
		Source: map[string]string{
			FieldMode: SourceDefault, FieldTagTrack: SourceDefault, FieldAutoRollback: SourceFromMode,
			FieldVerifyTimeout: SourceDefault, FieldVerifyStable: SourceDefault,
		},
	}
	var autoRollback *bool
	for _, l := range chain {
		f := l.Fields
		if f.Mode != nil {
			e.Mode, e.Source[FieldMode] = *f.Mode, l.Level
		}
		if f.TagTrack != nil {
			e.TagTrack, e.Source[FieldTagTrack] = *f.TagTrack, l.Level
		}
		if f.AutoRollback != nil {
			autoRollback, e.Source[FieldAutoRollback] = f.AutoRollback, l.Level
		}
		if f.VerifyTimeout != nil {
			e.VerifyTimeout, e.Source[FieldVerifyTimeout] = *f.VerifyTimeout, l.Level
		}
		if f.VerifyStable != nil {
			e.VerifyStable, e.Source[FieldVerifyStable] = *f.VerifyStable, l.Level
		}
	}
	if labelIgnore {
		e.Mode, e.Source[FieldMode] = ModeIgnore, SourceIgnoreLabel
	}
	if autoRollback != nil {
		e.AutoRollback = *autoRollback
	} else {
		e.AutoRollback = e.Mode == ModeAutomatic
	}
	return e
}

// LabelIgnore reports whether ddum labels contain ddum.ignore=true.
func LabelIgnore(labels map[string]string) bool {
	return strings.EqualFold(labels["ddum.ignore"], "true")
}

// Tracking modes.
const (
	TrackingDigest    = "digest"
	TrackingVersion   = "version"
	TrackingUntracked = "untracked"
	// ReasonNotInRegistry explains a container no check has ever found.
	ReasonNotInRegistry = "not found in its registry: a local build, or a private image the bot has no login for"
)

// Tracking is how a container's image is checked for updates.
type Tracking struct {
	Mode       string
	Reason     string   // why untracked
	Repository string   // normalized, e.g. docker.io/library/nginx
	Tag        string   // tag in use (latest when omitted)
	Digests    []string // running digests for Repository, e.g. sha256:…
}

// ClassifyTracking decides how a container is tracked.
func ClassifyTracking(imageRef string, repoDigests []string, ignored bool) Tracking {
	if ignored {
		return Tracking{Mode: TrackingUntracked, Reason: "ignored by policy"}
	}
	named, err := reference.ParseNormalizedNamed(imageRef)
	if err != nil {
		return Tracking{Mode: TrackingUntracked, Reason: "image reference does not parse: " + imageRef}
	}
	t := Tracking{Repository: named.Name()}
	if _, pinned := named.(reference.Digested); pinned {
		t.Mode, t.Reason = TrackingUntracked, "pinned by digest"
		return t
	}
	t.Tag = reference.TagNameOnly(named).(reference.Tagged).Tag()
	t.Digests = RunningDigests(t.Repository, repoDigests)
	if len(t.Digests) == 0 {
		t.Mode, t.Reason = TrackingUntracked, "image has no registry digest (built or loaded locally)"
		return t
	}
	if _, ok := ParseVersionTag(t.Tag); ok {
		t.Mode = TrackingVersion
	} else {
		t.Mode = TrackingDigest
	}
	return t
}

// RunningDigests returns the digests in repoDigests that belong to repo.
func RunningDigests(repo string, repoDigests []string) []string {
	var out []string
	for _, rd := range repoDigests {
		n, err := reference.ParseNormalizedNamed(rd)
		if err != nil {
			continue
		}
		if c, ok := n.(reference.Canonical); ok && n.Name() == repo {
			out = append(out, c.Digest().String())
		}
	}
	return out
}

// SnoozeActive reports whether a snooze is in force: not removed, and either
// indefinite (zero until) or not yet expired.
func SnoozeActive(until time.Time, removed bool, now time.Time) bool {
	return !removed && (until.IsZero() || until.After(now))
}

// Displayed states.
const (
	StateDeploying  = "Deploying"
	StateHeld       = "Held"
	StateSnoozed    = "Snoozed"
	StatePending    = "Pending approval"
	StateAutomatic  = "Automatic"
	StateIgnored    = ""
	stateRankDeploy = 2
	stateRankHeld   = 4
	stateRankSnooze = 5
	stateRankOther  = 6
)

// UpdateFacts are the inputs to an update's displayed state.
type UpdateFacts struct {
	Kind               string // digest | version
	Mode               string // effective mode of the container
	Snoozed            bool
	Protected          bool
	TagChangeSupported bool
	TagChangeReason    string
	DeploySupported    bool
	DeployReason       string
	HostApproved       bool
	HostOnline         bool
	// AutoFailed: an automatic deployment of this update failed, so it now
	// waits for a user.
	AutoFailed bool
}

// UpdateState returns the displayed state of one update and, when Held, why.
func UpdateState(f UpdateFacts) (state, reason string) {
	switch {
	case f.Mode == ModeIgnore:
		return StateIgnored, ""
	case f.Protected:
		return StateHeld, "protected: update this component manually, then re-run /update check"
	case !f.HostApproved:
		return StateHeld, "host pending approval"
	case !f.HostOnline:
		return StateHeld, "agent offline"
	case !f.DeploySupported:
		return StateHeld, "manual update required (" + f.DeployReason + ")"
	case f.Kind == "version" && !f.TagChangeSupported:
		r := f.TagChangeReason
		if r == "" {
			r = "the tag source could not be classified"
		}
		return StateHeld, "manual update required (" + r + ")"
	case f.Snoozed:
		return StateSnoozed, ""
	case f.Mode == ModeAutomatic && !f.AutoFailed:
		return StateAutomatic, ""
	}
	return StatePending, ""
}

// ScopeState is a stack's headline state: Held if any update is Held,
// Snoozed if every non-ignored update is snoozed, otherwise the first
// remaining state.
func ScopeState(states []string) string {
	best, rank := "", 99
	allSnoozed, any := true, false
	for _, s := range states {
		if s == StateIgnored {
			continue
		}
		any = true
		r := stateRankOther
		switch s {
		case StateDeploying:
			r = stateRankDeploy
			allSnoozed = false
		case StateHeld:
			r = stateRankHeld
		case StateSnoozed:
			r = stateRankSnooze
		default:
			allSnoozed = false
		}
		if s != StateSnoozed && r < rank {
			best, rank = s, r
		}
	}
	switch {
	case !any:
		return StateIgnored
	case rank == stateRankDeploy:
		return StateDeploying
	case rank == stateRankHeld:
		return StateHeld
	case allSnoozed:
		return StateSnoozed
	}
	return best
}
