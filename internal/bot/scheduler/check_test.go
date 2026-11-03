package scheduler

import (
	"context"
	"errors"
	"io"
	"log"
	"log/slog"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	ggcrregistry "github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/DragonDrew/ddum/internal/bot/domain"
	"github.com/DragonDrew/ddum/internal/bot/registry"
	"github.com/DragonDrew/ddum/internal/bot/store"
	"github.com/DragonDrew/ddum/internal/ids"
	"github.com/DragonDrew/ddum/internal/protocol"
)

const agentID = "0195f2c4-0000-7000-8000-00000000000a"

var owner = store.Actor{Kind: "user", DiscordID: "100000000000000010", Name: "owner"}

type countingNotifier struct {
	notify    int
	refresh   [][]string
	expired   int
	failing   [][]string // container names per ChecksFailing call
	recovered [][]string
	versions  []string // "host agent bot" per AgentVersionDiffers call
}

func names(cs []store.Container) []string {
	var out []string
	for _, c := range cs {
		out = append(out, c.DisplayName)
	}
	return out
}

func (n *countingNotifier) ChecksFailing(_ context.Context, cs []store.Container) {
	n.failing = append(n.failing, names(cs))
}
func (n *countingNotifier) ChecksRecovered(_ context.Context, cs []store.Container) {
	n.recovered = append(n.recovered, names(cs))
}
func (n *countingNotifier) AgentVersionDiffers(_ context.Context, h store.Host, bot string) {
	n.versions = append(n.versions, h.DisplayName+" "+h.AgentVersion+" "+bot)
}

func (n *countingNotifier) NotifyNewUpdates(context.Context) { n.notify++ }
func (n *countingNotifier) RefreshStacks(_ context.Context, ids []string) {
	n.refresh = append(n.refresh, ids)
}
func (n *countingNotifier) SnoozeExpired(_ context.Context, e []store.ExpiredSnooze) {
	n.expired += len(e)
}

type env struct {
	t     *testing.T
	host  string
	db    *store.Store
	check *Checker
	now   time.Time
	notes *countingNotifier
}

func newEnv(t *testing.T) *env {
	t.Helper()
	srv := httptest.NewServer(ggcrregistry.New(ggcrregistry.Logger(log.New(io.Discard, "", 0))))
	t.Cleanup(srv.Close)
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "ddum.db"), ids.V7{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	e := &env{t: t, host: strings.TrimPrefix(srv.URL, "http://"), db: db,
		now: time.Date(2026, 10, 1, 2, 0, 0, 0, time.UTC), notes: &countingNotifier{}}
	e.check = &Checker{Store: db, Registry: registry.New(registry.Config{Insecure: true, RequestGap: time.Microsecond}), Notify: e.notes,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Now: func() time.Time { return e.now },
		Interval: 6 * time.Hour, GraceHours: 24}
	return e
}

// push writes a random image to repo:tag and returns its digest. A source
// label makes release links resolvable.
func (e *env) push(repoTag string) string {
	e.t.Helper()
	img, err := random.Image(64, 1)
	if err != nil {
		e.t.Fatal(err)
	}
	cf, _ := img.ConfigFile()
	cf = cf.DeepCopy()
	cf.Config.Labels = map[string]string{"org.opencontainers.image.source": "https://github.com/example/" + strings.Split(repoTag, ":")[0]}
	img, _ = mutate.ConfigFile(img, cf)
	ref, _ := name.NewTag(e.host+"/"+repoTag, name.Insecure)
	if err := remote.Write(ref, img); err != nil {
		e.t.Fatal(err)
	}
	d, _ := img.Digest()
	return d.String()
}

// deploy registers and approves the host with one compose stack whose
// containers run the given image:digest pairs.
func (e *env) deploy(images map[string]string, labels map[string]map[string]string) {
	e.t.Helper()
	ctx := context.Background()
	res, err := e.db.RecordPoll(ctx, store.Poll{AgentID: agentID, HostName: "h", PollInterval: 30,
		Docker: protocol.DockerInfo{Arch: "amd64"}, At: e.now})
	if err != nil {
		e.t.Fatal(err)
	}
	if _, _, err := e.db.ApproveHost(ctx, res.Host.ID, owner, e.now); err != nil && !errors.Is(err, store.ErrHostRemoved) {
		e.t.Fatal(err)
	}
	st := protocol.Stack{Fingerprint: "compose:app", DisplayName: "app", Provider: "compose", Deploy: protocol.Support{Supported: true}}
	for svc, image := range images {
		ref, digest, _ := strings.Cut(image, "@")
		repo := ref[:strings.LastIndex(ref, ":")]
		st.Containers = append(st.Containers, protocol.Container{IdentityKey: svc, ImageRef: e.host + "/" + ref,
			RepoDigests: []string{e.host + "/" + repo + "@" + digest}, State: "running", DdumLabels: labels[svc]})
	}
	if _, err := e.db.ApplyInventory(ctx, agentID, protocol.Inventory{InventoryHash: "x", Stacks: []protocol.Stack{st}}, e.now); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) run() CheckResult {
	e.t.Helper()
	r, err := e.check.Check(context.Background(), Scope{})
	if err != nil {
		e.t.Fatal(err)
	}
	return r
}

func (e *env) detected() map[string]store.Update {
	e.t.Helper()
	ups, err := e.db.Updates(context.Background(), store.UpdateFilter{Statuses: []string{store.StatusDetected}}, e.now)
	if err != nil {
		e.t.Fatal(err)
	}
	out := map[string]store.Update{}
	for _, u := range ups {
		out[u.ContainerName] = u
	}
	return out
}

// TestDetectsEachKindExactlyOnce checks that a moved latest
// tag, a newer version tag, and an -ls257 → -ls258 build are each detected
// exactly once.
func TestDetectsEachKindExactlyOnce(t *testing.T) {
	e := newEnv(t)
	dLatest := e.push("app:latest")
	dVer := e.push("ver:1.2.3")
	dLs := e.push("arr:4.0.10.2544-ls257")
	e.push("arr:4.0.10.2544-alpine-ls300") // different stem: never offered
	e.deploy(map[string]string{
		"web":    "app:latest@" + dLatest,
		"api":    "ver:1.2.3@" + dVer,
		"sonarr": "arr:4.0.10.2544-ls257@" + dLs,
	}, nil)

	if r := e.run(); r.NewUpdates != 0 || r.Checked != 3 || r.Errors != 0 {
		t.Fatalf("up to date: %+v", r)
	}

	newLatest := e.push("app:latest")
	newVer := e.push("ver:1.2.4")
	e.push("ver:2.0.0") // other major: excluded by same-major
	newLs := e.push("arr:4.0.10.2544-ls258")
	e.now = e.now.Add(6 * time.Hour)
	if r := e.run(); r.NewUpdates != 3 {
		t.Fatalf("new updates = %d, want 3", r.NewUpdates)
	}
	got := e.detected()
	if u := got["web"]; u.Kind != "digest" || u.TargetDigest != newLatest {
		t.Errorf("moved latest: %+v", u)
	}
	if u := got["api"]; u.Kind != "version" || u.TargetVersion != "1.2.4" || u.TargetDigest != newVer {
		t.Errorf("version: %+v", u)
	}
	if u := got["sonarr"]; u.Kind != "version" || u.TargetVersion != "4.0.10.2544-ls258" || u.TargetDigest != newLs {
		t.Errorf("build counter: %+v", u)
	}
	if u := got["api"]; u.ReleaseURL != "https://github.com/example/ver/releases/tag/1.2.4" {
		t.Errorf("release link = %q", u.ReleaseURL)
	}

	e.now = e.now.Add(6 * time.Hour)
	if r := e.run(); r.NewUpdates != 0 {
		t.Fatalf("second cycle new updates = %d, want 0 (exactly once)", r.NewUpdates)
	}
	if len(e.detected()) != 3 {
		t.Fatal("updates must stay detected across cycles")
	}
	if e.notes.notify != 3 {
		t.Fatalf("notifier called %d times, want once per cycle", e.notes.notify)
	}
}

func TestNewerUpdateSupersedesAndPinnedTrack(t *testing.T) {
	e := newEnv(t)
	d := e.push("ver:1.2.3")
	e.deploy(map[string]string{"api": "ver:1.2.3@" + d}, nil)
	e.push("ver:1.2.4")
	e.run()
	first := e.detected()["api"]
	e.push("ver:1.3.0")
	e.now = e.now.Add(time.Hour)
	e.run()
	second := e.detected()["api"]
	if second.ID == first.ID || second.TargetVersion != "1.3.0" {
		t.Fatalf("newer version should supersede: %+v", second)
	}

	// tag_track pinned: only rebuilds of the current tag are offered.
	pinned := domain.TrackPinned
	cs, _ := e.db.Containers(context.Background(), store.ContainerFilter{})
	e.db.SetPolicy(context.Background(), store.PolicyKey{Level: domain.LevelContainer, ID: cs[0].ID},
		domain.PolicyFields{TagTrack: &pinned}, owner, e.now)
	e.push("ver:1.2.3") // rebuild of the running tag
	e.now = e.now.Add(time.Hour)
	e.run()
	if u := e.detected()["api"]; u.Kind != "digest" || !strings.Contains(u.TargetRef, ":1.2.3@") {
		t.Fatalf("pinned should offer only the rebuild of 1.2.3: %+v", u)
	}
}

func TestIgnoredAndUntracked(t *testing.T) {
	e := newEnv(t)
	d := e.push("app:latest")
	e.deploy(map[string]string{"web": "app:latest@" + d, "db": "app:latest@" + d},
		map[string]map[string]string{"db": {"ddum.ignore": "true"}})
	e.push("app:latest")
	r := e.run()
	if r.NewUpdates != 1 || r.Untracked != 1 {
		t.Fatalf("result = %+v; the ignored container must be untracked", r)
	}
	cs, _ := e.db.Containers(context.Background(), store.ContainerFilter{})
	for _, c := range cs {
		if c.IdentityKey == "db" && (c.Tracking != domain.TrackingUntracked || c.UntrackedReason != "ignored by policy") {
			t.Fatalf("db tracking = %s (%s)", c.Tracking, c.UntrackedReason)
		}
	}
}

func TestRegistryFailureDoesNotFailTheCycle(t *testing.T) {
	e := newEnv(t)
	d := e.push("app:latest")
	e.deploy(map[string]string{"web": "app:latest@" + d, "gone": "missing:1.0@" + d}, nil)
	e.foundBefore("gone") // an image never found is a local build
	e.push("app:latest")
	r := e.run()
	if r.Errors != 1 || r.NewUpdates != 1 {
		t.Fatalf("result = %+v; one failed repository must not stop the others", r)
	}
	cs, _ := e.db.Containers(context.Background(), store.ContainerFilter{})
	for _, c := range cs {
		if c.IdentityKey == "gone" && (c.LastCheckError == "" || c.Tracking == domain.TrackingUntracked) {
			t.Fatalf("failed check: tracking=%s err=%q", c.Tracking, c.LastCheckError)
		}
	}
	if last, found, _ := LastCheck(context.Background(), e.db); !found || last.Errors != 1 {
		t.Fatalf("last check = %+v", last)
	}
}

func TestPendingHostIsNotChecked(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	d := e.push("app:latest")
	e.db.RecordPoll(ctx, store.Poll{AgentID: agentID, HostName: "h", PollInterval: 30, Docker: protocol.DockerInfo{Arch: "amd64"}, At: e.now})
	e.db.ApplyInventory(ctx, agentID, protocol.Inventory{InventoryHash: "x", Stacks: []protocol.Stack{{
		Fingerprint: "compose:app", Provider: "compose", Containers: []protocol.Container{{IdentityKey: "web",
			ImageRef: e.host + "/app:latest", RepoDigests: []string{e.host + "/app@" + d}}}}}}, e.now)
	e.push("app:latest")
	if r := e.run(); r.Checked != 0 || r.NewUpdates != 0 {
		t.Fatalf("pending host was checked: %+v", r)
	}
}

func TestBusy(t *testing.T) {
	e := newEnv(t)
	e.check.running.Store(true)
	if _, err := e.check.Check(context.Background(), Scope{}); !errors.Is(err, ErrBusy) {
		t.Fatalf("err = %v, want ErrBusy", err)
	}
	if err := e.check.Start(Scope{}, nil); !errors.Is(err, ErrBusy) {
		t.Fatalf("Start err = %v, want ErrBusy", err)
	}
}

// countingRegistry counts full and incremental tag listings.
type countingRegistry struct {
	Registry
	tags, after int
}

func (r *countingRegistry) Tags(ctx context.Context, repo string) ([]string, error) {
	r.tags++
	return r.Registry.Tags(ctx, repo)
}

func (r *countingRegistry) TagsAfter(ctx context.Context, repo, last string) ([]string, error) {
	r.after++
	return r.Registry.TagsAfter(ctx, repo, last)
}

func TestLongFailingChecksAreReportedOnceAndOnRecovery(t *testing.T) {
	e := newEnv(t)
	d := e.push("app:1.0.0")
	e.deploy(map[string]string{"web": "app:1.0.0@" + d, "gone": "missing:1.0@" + d}, nil)
	e.foundBefore("gone") // an image never found is a local build

	// Failing for less than a day: nothing yet.
	for range 4 {
		e.run()
		e.now = e.now.Add(6 * time.Hour)
	}
	if len(e.notes.failing) != 0 {
		t.Fatalf("reported after 18 hours: %v", e.notes.failing)
	}
	e.run() // 24 hours after the first failure
	e.now = e.now.Add(6 * time.Hour)
	e.run()
	if len(e.notes.failing) != 1 || strings.Join(e.notes.failing[0], ",") != "gone" {
		t.Fatalf("failing notices = %v, want one for gone", e.notes.failing)
	}
	if ctrs, _ := e.db.Containers(context.Background(), store.ContainerFilter{}); len(ctrs) == 2 {
		for _, c := range ctrs {
			if (c.DisplayName == "gone") == c.CheckFailingSince.IsZero() {
				t.Errorf("%s: failing since %v", c.DisplayName, c.CheckFailingSince)
			}
		}
	}

	// Once the check works again, that is reported once too.
	e.push("missing:1.0")
	e.now = e.now.Add(6 * time.Hour)
	e.run()
	e.now = e.now.Add(6 * time.Hour)
	e.run()
	if len(e.notes.recovered) != 1 || strings.Join(e.notes.recovered[0], ",") != "gone" || len(e.notes.failing) != 1 {
		t.Fatalf("recovered = %v, failing = %v", e.notes.recovered, e.notes.failing)
	}
}

func TestAgentVersionNotices(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	d := e.push("app:1.0.0")
	e.deploy(map[string]string{"web": "app:1.0.0@" + d}, nil) // agent on h, version unset
	started := e.now
	poll := func(version string) {
		t.Helper()
		if _, err := e.db.RecordPoll(ctx, store.Poll{AgentID: agentID, AgentVersion: version, HostName: "h",
			PollInterval: 30, At: e.now}); err != nil {
			t.Fatal(err)
		}
	}
	v := &AgentVersions{Store: e.db, Notify: e.notes, Log: e.check.Log, Version: "0.9.8", Started: started}

	poll("0.9.7")
	v.Sweep(ctx)
	v.Sweep(ctx)
	if strings.Join(e.notes.versions, ";") != "h 0.9.7 0.9.8" {
		t.Fatalf("notices = %v, want one for h", e.notes.versions)
	}

	// Matching versions say nothing; a later mismatch is reported again.
	poll("0.9.8")
	v.Sweep(ctx)
	poll("0.9.6")
	v.Sweep(ctx)
	if strings.Join(e.notes.versions, ";") != "h 0.9.7 0.9.8;h 0.9.6 0.9.8" {
		t.Fatalf("notices = %v", e.notes.versions)
	}

	// Versions recorded before the bot started, and development agents, are
	// not reported.
	e.notes.versions = nil
	v.Version, v.Started = "0.9.9", e.now.Add(time.Minute)
	v.Sweep(ctx)
	e.now = e.now.Add(2 * time.Minute)
	poll("dev")
	v.Sweep(ctx)
	if len(e.notes.versions) != 0 {
		t.Fatalf("notices = %v, want none", e.notes.versions)
	}
}

// limitingRegistry answers the first limit lookups with a 429 that lasts
// retryAfter.
type limitingRegistry struct {
	Registry
	now        func() time.Time
	limit      int
	retryAfter time.Duration
}

func (r *limitingRegistry) refuse() error {
	if r.limit == 0 {
		return nil
	}
	r.limit--
	return &registry.RateLimitError{Host: "ghcr.io", Until: r.now().Add(r.retryAfter), RetryAfter: r.retryAfter}
}

func (r *limitingRegistry) Digest(ctx context.Context, repo, tag string) (string, error) {
	if err := r.refuse(); err != nil {
		return "", err
	}
	return r.Registry.Digest(ctx, repo, tag)
}

func (r *limitingRegistry) Tags(ctx context.Context, repo string) ([]string, error) {
	if err := r.refuse(); err != nil {
		return nil, err
	}
	return r.Registry.Tags(ctx, repo)
}

func TestRateLimitedContainersAreRetriedInTheSameCycle(t *testing.T) {
	e := newEnv(t)
	reg := &limitingRegistry{Registry: e.check.Registry, now: func() time.Time { return e.now }, limit: 1, retryAfter: 5 * time.Minute}
	e.check.Registry = reg
	var slept []time.Duration
	e.check.Sleep = func(_ context.Context, d time.Duration) error {
		slept = append(slept, d)
		e.now = e.now.Add(d)
		return nil
	}
	d := e.push("ver:1.0.0")
	e.push("ver:1.0.1")
	e.deploy(map[string]string{"api": "ver:1.0.0@" + d}, nil)
	r := e.run()
	if len(slept) != 1 || slept[0] != 5*time.Minute {
		t.Fatalf("slept %v, want one 5m wait", slept)
	}
	if r.Errors != 0 || r.RateLimits != 0 || r.NewUpdates != 1 || e.detected()["api"].TargetVersion != "1.0.1" {
		t.Fatalf("result %+v, update %+v; want the retry to find 1.0.1", r, e.detected()["api"])
	}

	// A Retry-After beyond maxRateLimitWait is left to the next cycle.
	reg.limit, reg.retryAfter = 1, time.Hour
	e.push("ver:1.0.2")
	e.now = e.now.Add(6 * time.Hour)
	if r := e.run(); len(slept) != 1 || r.RateLimits != 1 || r.Errors != 1 {
		t.Fatalf("long Retry-After: result %+v, slept %v; want the failure recorded without waiting", r, slept)
	}
}

func TestTagListsSurviveARestartAndKeepVersionTagsOnly(t *testing.T) {
	e := newEnv(t)
	cfg := registry.Config{Insecure: true, RequestGap: time.Microsecond, PushOrderedHosts: []string{e.host}}
	reg := &countingRegistry{Registry: registry.New(cfg)}
	e.check.Registry = reg
	d := e.push("ver:1.0.0")
	e.push("ver:commit-abc")
	e.push("ver:pr-12")
	e.deploy(map[string]string{"api": "ver:1.0.0@" + d}, nil)
	r := e.run()
	if r.Requests[e.host] == 0 {
		t.Errorf("requests = %v, want this registry counted", r.Requests)
	}
	l, ok, err := e.db.TagList(context.Background(), e.host+"/ver")
	if err != nil || !ok || strings.Join(l.Tags, ",") != "1.0.0" || strings.Join(l.Tail, ",") != "commit-abc,pr-12" {
		t.Fatalf("kept list %+v, %v, %v; want version tags only and the raw tail", l, ok, err)
	}

	// A new Checker on the same database, as after a restart, extends the
	// kept list instead of listing again.
	reg = &countingRegistry{Registry: registry.New(cfg)}
	e.check = &Checker{Store: e.db, Registry: reg, Notify: e.notes, Log: e.check.Log, Now: e.check.Now,
		Interval: 6 * time.Hour, GraceHours: 24}
	e.push("ver:pr-13")
	e.push("ver:v9") // not a version tag either
	e.now = e.now.Add(6 * time.Hour)
	if e.run(); reg.tags != 0 || reg.after != 1 {
		t.Fatalf("after restart: listings %d, extensions %d; want one extension", reg.tags, reg.after)
	}
	if l, _, _ := e.db.TagList(context.Background(), e.host+"/ver"); strings.Join(l.Tags, ",") != "1.0.0" ||
		strings.Join(l.Tail, ",") != "pr-13,v9" {
		t.Fatalf("extended list %+v", l)
	}

	// Lists unused for tagListMaxAge are dropped.
	e.deploy(map[string]string{}, nil)
	e.now = e.now.Add(tagListMaxAge + time.Hour)
	e.run()
	if _, ok, _ := e.db.TagList(context.Background(), e.host+"/ver"); ok {
		t.Fatal("unused list was not pruned")
	}
}

// TestPushOrderedTagListIsExtended runs against a registry treated as push
// ordered; its tags are pushed in lexical order, which is the order the
// in-process registry lists them in.
func TestPushOrderedTagListIsExtended(t *testing.T) {
	e := newEnv(t)
	reg := &countingRegistry{Registry: registry.New(registry.Config{Insecure: true, RequestGap: time.Microsecond,
		PushOrderedHosts: []string{e.host}})}
	e.check.Registry = reg
	d := e.push("ver:1.0.0")
	e.push("ver:1.0.1")
	e.deploy(map[string]string{"api": "ver:1.0.0@" + d}, nil)
	if e.run(); reg.tags != 1 || reg.after != 0 {
		t.Fatalf("first cycle: listings %d, extensions %d; want one full listing", reg.tags, reg.after)
	}

	// A release without latest is found at once, by extending the kept list.
	e.push("ver:1.0.2")
	e.now = e.now.Add(6 * time.Hour)
	if r := e.run(); r.NewUpdates != 1 || reg.tags != 1 || reg.after != 1 || e.detected()["api"].TargetVersion != "1.0.2" {
		t.Fatalf("release: result %+v, listings %d, extensions %d, update %+v", r, reg.tags, reg.after, e.detected()["api"])
	}

	// The list does not age out while it is extended.
	for range 6 {
		e.now = e.now.Add(6 * time.Hour)
		e.run()
	}
	if reg.tags != 1 || reg.after != 7 {
		t.Fatalf("quiet days: listings %d, extensions %d; want 1 and 7", reg.tags, reg.after)
	}

	// A deleted kept tag breaks the chain, so the repository is listed afresh.
	ref, _ := name.NewTag(e.host+"/ver:1.0.2", name.Insecure)
	if err := remote.Delete(ref); err != nil {
		t.Fatal(err)
	}
	e.now = e.now.Add(6 * time.Hour)
	if e.run(); reg.tags != 2 || reg.after != 8 {
		t.Fatalf("deleted tag: listings %d, extensions %d; want a fresh listing", reg.tags, reg.after)
	}
}

func TestTagListReusedWhileLatestStays(t *testing.T) {
	e := newEnv(t)
	reg := &countingRegistry{Registry: e.check.Registry}
	e.check.Registry = reg
	e.push("lsio:latest")
	d := e.push("lsio:1.0.0")
	e.deploy(map[string]string{"app": "lsio:1.0.0@" + d}, nil)

	e.run()
	e.now = e.now.Add(6 * time.Hour)
	if r := e.run(); r.NewUpdates != 0 || reg.tags != 1 {
		t.Fatalf("quiet repository: result %+v, listings %d; want the list reused", r, reg.tags)
	}

	// A release that moves latest is found at once.
	e.push("lsio:1.0.1")
	e.push("lsio:latest")
	e.now = e.now.Add(6 * time.Hour)
	if r := e.run(); r.NewUpdates != 1 || reg.tags != 2 || e.detected()["app"].TargetVersion != "1.0.1" {
		t.Fatalf("moved latest: result %+v, listings %d, update %+v", r, reg.tags, e.detected()["app"])
	}

	// One that leaves latest alone is found once the list is tagListMaxAge old.
	e.push("lsio:1.0.2")
	e.now = e.now.Add(6 * time.Hour)
	if e.run(); e.detected()["app"].TargetVersion != "1.0.1" || reg.tags != 2 {
		t.Fatalf("before max age: update %+v, listings %d", e.detected()["app"], reg.tags)
	}
	e.now = e.now.Add(tagListMaxAge)
	if e.run(); e.detected()["app"].TargetVersion != "1.0.2" || reg.tags != 3 {
		t.Fatalf("after max age: update %+v, listings %d", e.detected()["app"], reg.tags)
	}
}

func TestRepositoryWithoutLatestIsListedEveryCycle(t *testing.T) {
	e := newEnv(t)
	reg := &countingRegistry{Registry: e.check.Registry}
	e.check.Registry = reg
	d := e.push("ver:1.0.0")
	e.deploy(map[string]string{"api": "ver:1.0.0@" + d}, nil)
	e.run()
	e.push("ver:1.0.1")
	e.now = e.now.Add(6 * time.Hour)
	if r := e.run(); r.NewUpdates != 1 || reg.tags != 2 {
		t.Fatalf("result %+v, listings %d; want a fresh list and the update", r, reg.tags)
	}

	// Once latest appears it is used from the next listing on.
	e.push("ver:latest")
	e.now = e.now.Add(6 * time.Hour)
	e.run()
	e.now = e.now.Add(6 * time.Hour)
	e.run()
	if reg.tags != 3 {
		t.Fatalf("listings = %d, want 3 (latest adopted after it appeared)", reg.tags)
	}
}

func TestOnDemandCheckListsTagsAfresh(t *testing.T) {
	e := newEnv(t)
	reg := &countingRegistry{Registry: e.check.Registry}
	e.check.Registry = reg
	e.push("lsio:latest")
	d := e.push("lsio:1.0.0")
	e.deploy(map[string]string{"app": "lsio:1.0.0@" + d}, nil)
	e.run()

	// A release that leaves latest alone: a scheduled check reuses the list,
	// /update check does not.
	e.push("lsio:1.0.1")
	e.now = e.now.Add(time.Hour)
	if r := e.run(); r.NewUpdates != 0 || reg.tags != 1 {
		t.Fatalf("scheduled: result %+v, listings %d", r, reg.tags)
	}
	done := make(chan CheckResult, 1)
	if err := e.check.Start(Scope{}, func(r CheckResult, err error) {
		if err != nil {
			t.Error(err)
		}
		done <- r
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-done:
		if r.NewUpdates != 1 || reg.tags != 2 || e.detected()["app"].TargetVersion != "1.0.1" {
			t.Fatalf("on demand: result %+v, listings %d, update %+v", r, reg.tags, e.detected()["app"])
		}
	case <-time.After(10 * time.Second):
		t.Fatal("on-demand check did not finish")
	}

	// The fresh list is kept, so the next scheduled check reuses it.
	e.now = e.now.Add(time.Hour)
	if e.run(); reg.tags != 2 {
		t.Fatalf("after on demand: listings %d, want the fresh list reused", reg.tags)
	}
}

// TestManualUpdateClearsTheStaleOffer covers an update found at 0.10, after
// which the container is updated by hand to 0.11.1. Nothing is newer than
// 0.11.1, so the next check records nothing, and the 0.10 offer must not stay
// open to be approved as a downgrade.
func TestManualUpdateClearsTheStaleOffer(t *testing.T) {
	e := newEnv(t)
	d := e.push("ver:0.7.0")
	e.deploy(map[string]string{"api": "ver:0.7.0@" + d}, nil)
	e.push("ver:0.10.0")
	e.run()
	if u, ok := e.detected()["api"]; !ok || u.TargetVersion != "0.10.0" {
		t.Fatalf("0.10.0 should be offered: %+v", u)
	}

	d = e.push("ver:0.11.1")
	e.deploy(map[string]string{"api": "ver:0.11.1@" + d}, nil)
	e.now = e.now.Add(6 * time.Hour)
	if r := e.run(); r.NewUpdates != 0 {
		t.Fatalf("new updates = %d, want 0", r.NewUpdates)
	}
	if u, ok := e.detected()["api"]; ok {
		t.Fatalf("a stale offer is still open: %+v", u)
	}
}

// TestUntrackedContainerClearsItsStaleUpdate: a container moved to a pinned
// digest or a local build is no longer checked, so nothing else would close
// the update it had.
func TestUntrackedContainerClearsItsStaleUpdate(t *testing.T) {
	e := newEnv(t)
	d := e.push("ver:1.0.0")
	e.deploy(map[string]string{"api": "ver:1.0.0@" + d}, nil)
	e.push("ver:1.1.0")
	e.run()
	if _, ok := e.detected()["api"]; !ok {
		t.Fatal("1.1.0 should be offered")
	}
	st := protocol.Stack{Fingerprint: "compose:app", DisplayName: "app", Provider: "compose", Deploy: protocol.Support{Supported: true},
		Containers: []protocol.Container{{IdentityKey: "api", ImageRef: e.host + "/ver:1.0.0", RepoDigests: []string{}, State: "running"}}}
	if _, err := e.db.ApplyInventory(context.Background(), agentID, protocol.Inventory{InventoryHash: "local", Stacks: []protocol.Stack{st}}, e.now); err != nil {
		t.Fatal(err)
	}
	e.now = e.now.Add(6 * time.Hour)
	if r := e.run(); r.Untracked != 1 {
		t.Fatalf("result = %+v", r)
	}
	if u, ok := e.detected()["api"]; ok {
		t.Fatalf("a stale offer is still open: %+v", u)
	}
}

// TestIgnoredContainerKeepsItsUpdate: ignoring only hides an update, so it
// comes back unchanged when the ignore is lifted.
func TestIgnoredContainerKeepsItsUpdate(t *testing.T) {
	e := newEnv(t)
	d := e.push("ver:1.0.0")
	e.deploy(map[string]string{"api": "ver:1.0.0@" + d}, nil)
	e.push("ver:1.1.0")
	e.run()
	first := e.detected()["api"]
	mode := domain.ModeIgnore
	cs, _ := e.db.Containers(context.Background(), store.ContainerFilter{})
	e.db.SetPolicy(context.Background(), store.PolicyKey{Level: domain.LevelContainer, ID: cs[0].ID},
		domain.PolicyFields{Mode: &mode}, owner, e.now)
	e.now = e.now.Add(6 * time.Hour)
	e.run()
	if u := e.detected()["api"]; u.ID != first.ID {
		t.Fatalf("update = %+v", u)
	}
}
