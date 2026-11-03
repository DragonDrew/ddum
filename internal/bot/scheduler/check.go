package scheduler

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"math/rand/v2"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/DragonDrew/ddum/internal/bot/domain"
	"github.com/DragonDrew/ddum/internal/bot/registry"
	"github.com/DragonDrew/ddum/internal/bot/store"
)

// Registry is what detection needs from the registry client.
type Registry interface {
	Digest(ctx context.Context, repo, tag string) (string, error)
	Tags(ctx context.Context, repo string) ([]string, error)
	TagsAfter(ctx context.Context, repo, last string) ([]string, error)
	PushOrdered(repo string) bool
	Metadata(ctx context.Context, repo, tag string) (map[string]string, error)
	ManifestExists(ctx context.Context, repo, digest string) (bool, error)
	Platforms(ctx context.Context, repo, digest string) ([]string, error)
	TakeRequests() map[string]int
}

// CheckNotifier posts what a check cycle found.
type CheckNotifier interface {
	// NotifyNewUpdates posts notifications for updates not yet notified.
	NotifyNewUpdates(ctx context.Context)
	// RefreshStacks edits the stored update messages of these stacks.
	RefreshStacks(ctx context.Context, stackIDs []string)
	// SnoozeExpired posts one notice per expired snooze.
	SnoozeExpired(ctx context.Context, expired []store.ExpiredSnooze)
	// ChecksFailing reports containers whose checks have long failed.
	ChecksFailing(ctx context.Context, cs []store.Container)
	// ChecksRecovered reports containers from ChecksFailing that work again.
	ChecksRecovered(ctx context.Context, cs []store.Container)
}

// ErrBusy is returned when a check is already running.
var ErrBusy = errors.New("a registry check is already running")

// Scope narrows an on-demand check. Empty means everything.
type Scope struct {
	HostID  string
	StackID string
}

// CheckResult summarizes one cycle.
type CheckResult struct {
	Started    time.Time
	Finished   time.Time
	Checked    int // containers with a tracked image
	Untracked  int
	NewUpdates int
	Errors     int
	RateLimits int
	Requests   map[string]int // registry host → HTTP requests sent
}

// lastCheckKey is the system_settings key holding the last CheckResult.
const lastCheckKey = "last_registry_check"

// LastCheck returns the most recent completed full or scoped check.
func LastCheck(ctx context.Context, s *store.Store) (CheckResult, bool, error) {
	var r CheckResult
	found, err := s.Setting(ctx, lastCheckKey, &r)
	return r, found, err
}

// Checker runs registry checks every Interval and on demand.
type Checker struct {
	Store      *store.Store
	Registry   Registry
	Notify     CheckNotifier
	Log        *slog.Logger
	Now        func() time.Time
	Interval   time.Duration // REGISTRY_CHECK_INTERVAL
	GraceHours int           // STACK_MISSING_GRACE_HOURS
	// InventoryWait overrides inventoryWait (tests).
	InventoryWait time.Duration
	// LinkCheck reports whether a release link resolves; nil accepts the
	// first candidate unchecked.
	LinkCheck func(ctx context.Context, url string) bool
	// AfterCheck, if set, runs after each cycle (the auto-update engine).
	AfterCheck func()
	// Sleep waits out a rate limit within a cycle; nil uses a timer.
	Sleep func(ctx context.Context, d time.Duration) error

	running     atomic.Bool
	lastRefresh time.Time

	metaMu sync.Mutex
	meta   map[string]metaEntry // repository → release metadata
}

type metaEntry struct {
	meta map[string]string
	at   time.Time
}

// metadataTTL is how long release metadata is cached per repository.
const metadataTTL = 24 * time.Hour

// tagListMaxAge is how long a repository's tag list is reused while its
// latest tag does not move, and how long an unused list is kept.
const tagListMaxAge = 24 * time.Hour

// checkFailureNoticeAfter is how long a container's registry checks must
// fail without a break before they are reported in Discord.
const checkFailureNoticeAfter = 24 * time.Hour

// maxRateLimitWait is the longest a cycle waits for a rate-limited registry
// before checking the containers it skipped once more. A longer
// Retry-After leaves them to the next cycle.
const maxRateLimitWait = 15 * time.Minute

// Run schedules checks until ctx ends. The first check runs shortly after
// startup if the last one is older than Interval; later ones every
// Interval ±10 %. Snooze expiry is swept every minute.
func (c *Checker) Run(ctx context.Context) {
	next := c.Now().Add(30 * time.Second)
	if last, found, err := LastCheck(ctx, c.Store); err == nil && found {
		if due := last.Started.Add(c.Interval); due.After(next) {
			next = due
		}
	}
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	c.lastRefresh = c.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		c.sweepSnoozes(ctx)
		if c.Now().Before(next) {
			continue
		}
		if _, err := c.Check(ctx, Scope{}); err != nil && !errors.Is(err, ErrBusy) && ctx.Err() == nil {
			c.Log.Error("registry check", "err", err)
		}
		jitter := time.Duration((rand.Float64()*0.2 - 0.1) * float64(c.Interval))
		next = c.Now().Add(c.Interval + jitter)
	}
}

// Start runs an on-demand check in the background (/update check). It lists
// every repository's tags afresh instead of reusing kept lists, so it also
// finds releases that did not move latest. It returns ErrBusy if a check is
// running.
func (c *Checker) Start(scope Scope, done func(CheckResult, error)) error {
	if c.running.Load() {
		return ErrBusy
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()
		res, err := c.check(ctx, scope, true)
		if err != nil && !errors.Is(err, ErrBusy) {
			c.Log.Error("on-demand registry check", "err", err)
		}
		if done != nil && !errors.Is(err, ErrBusy) {
			done(res, err)
		}
	}()
	return nil
}

func (c *Checker) sweepSnoozes(ctx context.Context) {
	expired, err := c.Store.TakeExpiredSnoozes(ctx, c.Now())
	if err != nil {
		c.Log.Error("snooze expiry", "err", err)
		return
	}
	if len(expired) > 0 && c.Notify != nil {
		c.Notify.SnoozeExpired(ctx, expired)
	}
}

// cycle caches registry answers for one check, so each repository:tag is
// asked once however many containers use it.
type cycle struct {
	digests   map[string]result[string]
	tags      map[string]result[[]string]
	freshTags bool // list tags even where a kept list could be reused
}

type result[T any] struct {
	v   T
	err error
}

// Check runs one detection pass over the containers in scope, reusing kept
// tag lists where latest has not moved.
func (c *Checker) Check(ctx context.Context, scope Scope) (CheckResult, error) {
	return c.check(ctx, scope, false)
}

func (c *Checker) check(ctx context.Context, scope Scope, freshTags bool) (CheckResult, error) {
	if !c.running.CompareAndSwap(false, true) {
		return CheckResult{}, ErrBusy
	}
	defer c.running.Store(false)

	res := CheckResult{Started: c.Now()}
	stale, err := c.awaitInventories(ctx)
	if err != nil {
		return res, err
	}
	containers, err := c.Store.Containers(ctx, store.ContainerFilter{HostID: scope.HostID, StackID: scope.StackID})
	if err != nil {
		return res, err
	}
	policies, err := c.Store.Policies(ctx)
	if err != nil {
		return res, err
	}
	if n, err := c.Store.PruneTagLists(ctx, c.Now().Add(-tagListMaxAge)); err != nil {
		c.Log.Error("prune tag lists", "err", err)
	} else if n > 0 {
		c.Log.Debug("pruned unused tag lists", "count", n)
	}
	c.Registry.TakeRequests() // count this cycle's requests only
	cy := &cycle{digests: map[string]result[string]{}, tags: map[string]result[[]string]{}, freshTags: freshTags}
	changed := map[string]bool{}
	var limited []tracked // skipped by a 429, checked again once it passes
	var retryAt time.Time

	for _, ctr := range containers {
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		// Registry checks start once a host is approved.
		if ctr.HostApproval != domain.ApprovalApproved || ctr.StackPresence != domain.Present || stale[ctr.HostID] {
			continue
		}
		eff := domain.ResolvePolicy(store.Chain(policies, ctr.HostID, ctr.StackID, ctr.GroupID, ctr.ID), domain.LabelIgnore(ctr.DdumLabels))
		tr := domain.ClassifyTracking(ctr.ImageRef, ctr.RunningDigests, eff.Mode == domain.ModeIgnore)
		if tr.Mode == domain.TrackingUntracked {
			res.Untracked++
			c.setTracking(ctx, ctr.ID, tr.Mode, tr.Reason, "")
			// Moved to a pinned digest or a local build, it is no longer checked,
			// so nothing else would close its updates. Ignoring only hides them.
			if eff.Mode != domain.ModeIgnore {
				if err := c.supersedeCurrent(ctx, changed, ctr); err != nil {
					return res, err
				}
			}
			continue
		}
		res.Checked++
		t := tracked{ctr: ctr, tr: tr, track: eff.TagTrack}
		det, err := c.detect(ctx, cy, ctr, tr, eff.TagTrack)
		var rl *registry.RateLimitError
		if errors.As(err, &rl) {
			t.err = err
			limited = append(limited, t)
			if rl.Until.After(retryAt) {
				retryAt = rl.Until
			}
			continue
		}
		if err := c.record(ctx, &res, changed, t, det, err); err != nil {
			return res, err
		}
	}
	if err := c.retryLimited(ctx, cy, &res, changed, limited, retryAt); err != nil {
		return res, err
	}

	grace := time.Duration(c.GraceHours) * time.Hour
	if n, err := c.Store.SupersedeMissingStacks(ctx, c.Now().Add(-grace), c.Now()); err != nil {
		c.Log.Error("supersede updates of missing stacks", "err", err)
	} else if n > 0 {
		c.Log.Info("superseded updates of long-missing stacks", "count", n)
	}
	c.reportFailingChecks(ctx)

	res.Finished = c.Now()
	res.Requests = c.Registry.TakeRequests()
	if err := c.Store.SetSetting(ctx, lastCheckKey, res, res.Finished); err != nil {
		c.Log.Error("record last check", "err", err)
	}
	c.Log.Info("registry check finished", "checked", res.Checked, "untracked", res.Untracked,
		"new_updates", res.NewUpdates, "errors", res.Errors, "rate_limited", res.RateLimits,
		"requests", res.Requests, "duration", res.Finished.Sub(res.Started).Round(time.Millisecond).String())

	if c.Notify != nil {
		c.Notify.NotifyNewUpdates(ctx)
		// Edit messages of stacks whose updates changed in this cycle or, by
		// outside changes, since the last cycle.
		since := c.lastRefresh
		c.lastRefresh = res.Started
		stacks, err := c.Store.StacksWithUpdateChanges(ctx, since)
		if err != nil {
			c.Log.Error("find changed stacks", "err", err)
		}
		for _, id := range stacks {
			changed[id] = true
		}
		var ids []string
		for id := range changed {
			ids = append(ids, id)
		}
		slices.Sort(ids)
		c.Notify.RefreshStacks(ctx, ids)
	}
	if c.AfterCheck != nil {
		c.AfterCheck()
	}
	return res, nil
}

// reportFailingChecks reports the containers whose checks have failed for
// checkFailureNoticeAfter, once per run of failures, and the reported ones
// whose checks work again.
func (c *Checker) reportFailingChecks(ctx context.Context) {
	failing, err := c.Store.TakeFailingChecks(ctx, c.Now().Add(-checkFailureNoticeAfter), c.Now())
	if err != nil {
		c.Log.Error("find failing checks", "err", err)
	}
	recovered, err := c.Store.TakeRecoveredChecks(ctx)
	if err != nil {
		c.Log.Error("find recovered checks", "err", err)
	}
	if len(failing) > 0 {
		c.Log.Warn("registry checks failing for a day", "containers", len(failing))
		if c.Notify != nil {
			c.Notify.ChecksFailing(ctx, failing)
		}
	}
	if len(recovered) > 0 {
		c.Log.Info("registry checks working again", "containers", len(recovered))
		if c.Notify != nil {
			c.Notify.ChecksRecovered(ctx, recovered)
		}
	}
}

// tracked is a container with a tracked image, and the error of its check
// while it waits for a rate limit to pass.
type tracked struct {
	ctr   store.Container
	tr    domain.Tracking
	track string
	err   error
}

// record stores the outcome of one container's check: the failure when err
// is set, otherwise its detection, if any. It returns only store errors.
func (c *Checker) record(ctx context.Context, res *CheckResult, changed map[string]bool, t tracked, det *store.Detection, err error) error {
	if err != nil {
		// Never found in any check, this is a local build or an image the bot
		// has no login for, not a failing check.
		if !t.ctr.CheckEverOK && registry.IsMissing(err) {
			res.Untracked++
			c.setTracking(ctx, t.ctr.ID, domain.TrackingUntracked, domain.ReasonNotInRegistry, "")
			return nil
		}
		res.Errors++
		if errors.Is(err, registry.ErrRateLimited) {
			res.RateLimits++
		}
		// A failed check never changes the tracking mode.
		c.setTracking(ctx, t.ctr.ID, t.tr.Mode, "", err.Error())
		c.Log.Warn("registry check failed", "container_id", t.ctr.ID, "image", t.ctr.ImageRef, "err", err)
		return nil
	}
	c.setTracking(ctx, t.ctr.ID, t.tr.Mode, "", "")
	if det == nil {
		// The container is current, so updates still waiting for it are
		// stale (a manual update got there first).
		return c.supersedeCurrent(ctx, changed, t.ctr)
	}
	r, err := c.Store.RecordDetection(ctx, *det, c.Now())
	if err != nil {
		return err
	}
	if r.New {
		c.checkOrigin(ctx, t, r.Update.ID)
	}
	if r.New || r.Superseded > 0 {
		changed[t.ctr.StackID] = true
	}
	if r.New {
		res.NewUpdates++
		c.attachReleaseURL(ctx, r.Update, t.tr.Repository, det)
	}
	return nil
}

// supersedeCurrent closes the open updates of a container that needs none.
func (c *Checker) supersedeCurrent(ctx context.Context, changed map[string]bool, ctr store.Container) error {
	n, err := c.Store.SupersedeCurrent(ctx, ctr.ID, c.Now())
	if err == nil && n > 0 {
		changed[ctr.StackID] = true
	}
	return err
}

// retryLimited checks the containers a 429 skipped once more, after waiting
// until retryAt, so they are not left unchecked for a whole interval.
// If retryAt is more than maxRateLimitWait away, or the registry refuses
// again, their failures are recorded instead.
func (c *Checker) retryLimited(ctx context.Context, cy *cycle, res *CheckResult, changed map[string]bool, limited []tracked, retryAt time.Time) error {
	if len(limited) == 0 {
		return nil
	}
	wait := retryAt.Sub(c.Now())
	if wait > maxRateLimitWait {
		for _, t := range limited {
			if err := c.record(ctx, res, changed, t, nil, t.err); err != nil {
				return err
			}
		}
		return nil
	}
	c.Log.Info("registry check waiting out a rate limit", "containers", len(limited),
		"wait", wait.Round(time.Second).String())
	if err := c.sleep(ctx, wait); err != nil {
		return err
	}
	cy.forgetRateLimited()
	for _, t := range limited {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		det, err := c.detect(ctx, cy, t.ctr, t.tr, t.track)
		if err := c.record(ctx, res, changed, t, det, err); err != nil {
			return err
		}
	}
	return nil
}

func (c *Checker) sleep(ctx context.Context, d time.Duration) error {
	if c.Sleep != nil {
		return c.Sleep(ctx, d)
	}
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// forgetRateLimited drops cached answers that were 429s, so a retry asks
// the registry again.
func (cy *cycle) forgetRateLimited() {
	maps.DeleteFunc(cy.digests, func(_ string, r result[string]) bool { return errors.Is(r.err, registry.ErrRateLimited) })
	maps.DeleteFunc(cy.tags, func(_ string, r result[[]string]) bool { return errors.Is(r.err, registry.ErrRateLimited) })
}

// inventoryWait is how long a check waits for hosts that just finished an
// operation to report what they now run.
const inventoryWait = 30 * time.Second

// awaitInventories waits for a fresh inventory from any host whose last
// operation finished after its last inventory, and returns those still waiting.
// Until it arrives the bot has the host's containers wrong. A check would see a
// container just rolled back as still on the new version, decide it needs no
// update, and close the update the rollback reopened, so a group could then be
// offered with a member missing. Their containers are left for the next
// check.
func (c *Checker) awaitInventories(ctx context.Context) (map[string]bool, error) {
	wait := c.InventoryWait
	if wait <= 0 {
		wait = inventoryWait
	}
	const step = time.Second
	for waited := time.Duration(0); ; waited += step {
		stale, err := c.Store.HostsAwaitingInventory(ctx)
		if err != nil {
			return nil, err
		}
		if len(stale) == 0 {
			return nil, nil
		}
		if waited >= wait {
			c.Log.Warn("skipping hosts that have not reported since their last operation", "hosts", len(stale))
			return stale, nil
		}
		if err := c.sleep(ctx, step); err != nil {
			return nil, err
		}
	}
}

// checkOrigin looks at a new update of a container on the containerd image
// store, where a local build has a digest too. If the repository does not hold
// the running image, the update could replace a local build with a stranger's
// image of the same name, so it waits for a person. A registry that
// pruned the old image looks the same, which costs one approval. Only this
// store is checked, because elsewhere a digest does show where an image came from.
func (c *Checker) checkOrigin(ctx context.Context, t tracked, updateID string) {
	h, err := c.Store.Host(ctx, t.ctr.HostID)
	if err != nil || !h.Docker.ImageIDIsDigest {
		return
	}
	found, err := c.Registry.ManifestExists(ctx, t.tr.Repository, t.tr.Digests[0])
	if err != nil {
		c.Log.Warn("could not check where the running image came from", "container_id", t.ctr.ID, "err", err)
		return
	}
	if !found {
		if err := c.Store.MarkUnverifiedOrigin(ctx, updateID, c.Now()); err != nil {
			c.Log.Error("mark update", "update_id", updateID, "err", err)
		}
	}
}

// maxPlatformTries limits how many versions, newest first, are looked at for
// one that a container's platform can run.
const maxPlatformTries = 5

// runsOn reports whether repo@digest has a build for a container's platform
// . amd64 hosts skip this: nearly every image has that build, the answer
// costs a manifest GET, and a missing one fails harmlessly at the pull.
func (c *Checker) runsOn(ctx context.Context, repo, digest string, ctr store.Container) (bool, error) {
	if ctr.Platform == "" || strings.HasPrefix(ctr.Platform, "linux/amd64") {
		return true, nil
	}
	platforms, ok, err := c.Store.ImagePlatforms(ctx, repo, digest)
	if err != nil {
		return false, err
	}
	if !ok {
		if platforms, err = c.Registry.Platforms(ctx, repo, digest); err != nil {
			return false, err
		}
		if err := c.Store.SetImagePlatforms(ctx, repo, digest, platforms, c.Now()); err != nil {
			return false, err
		}
	}
	return domain.PlatformRuns(ctr.Platform, platforms), nil
}

// noteNewerOutsideTrack records the newest version the container's tag_track
// leaves out, which otherwise nothing would mention.
func (c *Checker) noteNewerOutsideTrack(ctx context.Context, ctr store.Container, tag, best string, tags []string, pushOrdered bool) {
	newest, _ := domain.SelectVersion(tag, tags, domain.TrackAny, pushOrdered)
	if newest == best {
		newest = ""
	}
	if newest != ctr.NewerOutsideTrack {
		if err := c.Store.SetNewerOutsideTrack(ctx, ctr.ID, newest); err != nil {
			c.Log.Error("store newer version", "container_id", ctr.ID, "err", err)
		}
	}
}

// detect applies the detection algorithm to one container. It returns nil
// when the container is up to date.
func (c *Checker) detect(ctx context.Context, cy *cycle, ctr store.Container, tr domain.Tracking, track string) (*store.Detection, error) {
	det := &store.Detection{
		ContainerID: ctr.ID, StackID: ctr.StackID, HostID: ctr.HostID,
		CurrentRef: ctr.ImageRef, CurrentDigest: tr.Digests[0],
	}
	if tr.Mode == domain.TrackingVersion && track != domain.TrackPinned {
		tags, err := c.tags(ctx, cy, tr.Repository)
		if err != nil {
			return nil, err
		}
		ordered := c.Registry.PushOrdered(tr.Repository)
		best, ok := domain.SelectVersion(tr.Tag, tags, track, ordered)
		c.noteNewerOutsideTrack(ctx, ctr, tr.Tag, best, tags, ordered)
		for try := 0; ok && try < maxPlatformTries; try++ {
			d, err := c.digest(ctx, cy, tr.Repository, best)
			if err != nil {
				return nil, err
			}
			runs, err := c.runsOn(ctx, tr.Repository, d, ctr)
			if err != nil {
				return nil, err
			}
			if runs {
				det.Kind, det.TargetVersion = "version", best
				det.TargetRef, det.TargetDigest = tr.Repository+":"+best+"@"+d, d
				return det, nil
			}
			c.Log.Info("newest version has no build for this platform; trying the one before", "image", tr.Repository, "tag", best, "platform", ctr.Platform)
			skip := best
			tags = slices.DeleteFunc(slices.Clone(tags), func(t string) bool { return t == skip })
			best, ok = domain.SelectVersion(tr.Tag, tags, track, ordered)
		}
	}
	// Digest tracking, and rebuilds of the current version tag (step 6).
	d, err := c.digest(ctx, cy, tr.Repository, tr.Tag)
	if err != nil {
		return nil, err
	}
	if slices.Contains(tr.Digests, d) {
		return nil, nil
	}
	if runs, err := c.runsOn(ctx, tr.Repository, d, ctr); err != nil {
		return nil, err
	} else if !runs {
		return nil, nil // the tag moved to a build this host cannot run
	}
	det.Kind = "digest"
	det.TargetRef, det.TargetDigest = tr.Repository+":"+tr.Tag+"@"+d, d
	return det, nil
}

func (c *Checker) digest(ctx context.Context, cy *cycle, repo, tag string) (string, error) {
	key := repo + ":" + tag
	if r, ok := cy.digests[key]; ok {
		return r.v, r.err
	}
	d, err := c.Registry.Digest(ctx, repo, tag)
	cy.digests[key] = result[string]{d, err}
	return d, err
}

func (c *Checker) tags(ctx context.Context, cy *cycle, repo string) ([]string, error) {
	if r, ok := cy.tags[repo]; ok {
		return r.v, r.err
	}
	t, err := c.listTags(ctx, cy, repo)
	cy.tags[repo] = result[[]string]{t, err}
	return t, err
}

// listTags returns repo's tags. A list from an earlier cycle is reused while
// it is younger than tagListMaxAge and the repository's latest tag still has
// the digest it had then, so a quiet repository costs one HEAD instead of
// every page of its tag list. A release almost always moves latest;
// one that does not (a patch on an older line) is found within
// tagListMaxAge. A repository without latest is listed every cycle.
//
// On a registry that lists tags in push order (ghcr.io, lscr.io) a kept list
// is instead extended with the tags pushed since, every cycle and by
// /update check alike: one request, however long the list (immich's
// repositories have over 200,000 tags and no latest), and every release is
// found at once, whether it moves latest or not.
//
// Lists are kept in the database, so a restart does not list everything
// again, and hold version tags only, the only ones SelectVersion can pick
// .
func (c *Checker) listTags(ctx context.Context, cy *cycle, repo string) ([]string, error) {
	prev, ok, err := c.Store.TagList(ctx, repo)
	if err != nil {
		return nil, err
	}

	if ok && len(prev.Tail) == 2 && c.Registry.PushOrdered(repo) {
		l, extended, err := c.extendTags(ctx, repo, prev)
		if err != nil {
			return nil, err
		}
		if extended {
			if err := c.Store.SetTagList(ctx, repo, l); err != nil {
				return nil, err
			}
			return l.Tags, nil
		}
	}

	latest := ""
	if !ok || prev.Latest != "" {
		d, err := c.digest(ctx, cy, repo, "latest")
		if errors.Is(err, registry.ErrRateLimited) {
			return nil, err
		}
		latest = d // "" on any other error: no signal, so list
	}
	if !cy.freshTags && ok && latest != "" && latest == prev.Latest && c.Now().Sub(prev.At) < tagListMaxAge {
		return prev.Tags, nil
	}

	all, err := c.Registry.Tags(ctx, repo)
	if err != nil {
		return nil, err
	}
	if latest == "" && ok && slices.Contains(all, "latest") {
		// latest appeared since the last listing: start using it.
		latest, _ = c.digest(ctx, cy, repo, "latest")
	}
	l := store.TagList{Tags: versionTags(nil, all), Tail: lastTwo(all), Latest: latest, At: c.Now()}
	if err := c.Store.SetTagList(ctx, repo, l); err != nil {
		return nil, err
	}
	return l.Tags, nil
}

// extendTags extends prev, a push-ordered list, with the tags pushed since it
// was listed. It lists after the second-to-last tag, so the answer must start
// with the last one; if it does not, a kept tag has been deleted and
// extendTags reports false so the caller lists afresh.
func (c *Checker) extendTags(ctx context.Context, repo string, prev store.TagList) (store.TagList, bool, error) {
	more, err := c.Registry.TagsAfter(ctx, repo, prev.Tail[0])
	if err != nil {
		return prev, false, err
	}
	if len(more) == 0 || more[0] != prev.Tail[1] {
		return prev, false, nil
	}
	l := prev
	l.Tags = versionTags(slices.Clip(prev.Tags), more[1:])
	l.Tail = lastTwo(append(slices.Clone(prev.Tail), more[1:]...))
	l.At = c.Now()
	return l, true, nil
}

// versionTags appends the version tags among tags to dst. immich's
// repositories, for one, are almost all commit and pull-request tags.
func versionTags(dst, tags []string) []string {
	for _, t := range tags {
		if _, ok := domain.ParseVersionTag(t); ok {
			dst = append(dst, t)
		}
	}
	return dst
}

// lastTwo returns the last two of tags, which anchor the next extension of a
// push-ordered list.
func lastTwo(tags []string) []string {
	return slices.Clone(tags[max(0, len(tags)-2):])
}

// attachReleaseURL finds the release link for a new update, once, using
// metadata cached per repository. Failures only mean no link.
func (c *Checker) attachReleaseURL(ctx context.Context, u store.Update, repo string, det *store.Detection) {
	if u.ReleaseURL != "" {
		return
	}
	c.metaMu.Lock()
	if c.meta == nil {
		c.meta = map[string]metaEntry{}
	}
	e, ok := c.meta[repo]
	c.metaMu.Unlock()
	if !ok || c.Now().Sub(e.at) > metadataTTL {
		tag := det.TargetVersion
		if tag == "" {
			tag = tagOf(det.TargetRef, repo)
		}
		m, err := c.Registry.Metadata(ctx, repo, tag)
		if err != nil {
			c.Log.Debug("release metadata unavailable", "repository", repo, "err", err)
			m = map[string]string{}
		}
		e = metaEntry{meta: m, at: c.Now()}
		c.metaMu.Lock()
		c.meta[repo] = e
		c.metaMu.Unlock()
	}
	cands := registry.ReleaseCandidates(e.meta, det.Kind, det.TargetVersion)
	url := ""
	for i, u := range cands {
		if i == len(cands)-1 || c.LinkCheck == nil || c.LinkCheck(ctx, u) {
			url = u
			break
		}
	}
	if url != "" {
		if err := c.Store.SetReleaseURL(ctx, u.ID, url); err != nil {
			c.Log.Error("store release URL", "update_id", u.ID, "err", err)
		}
	}
}

// tagOf extracts the tag from "repo:tag@digest".
func tagOf(ref, repo string) string {
	rest := ref[len(repo):]
	if len(rest) > 0 && rest[0] == ':' {
		rest = rest[1:]
	}
	for i := 0; i < len(rest); i++ {
		if rest[i] == '@' {
			return rest[:i]
		}
	}
	return rest
}

func (c *Checker) setTracking(ctx context.Context, id, mode, reason, checkErr string) {
	if err := c.Store.SetTracking(ctx, id, mode, reason, checkErr, c.Now()); err != nil {
		c.Log.Error("store tracking", "container_id", id, "err", err)
	}
}
