package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/distribution/reference"
	"gopkg.in/yaml.v3"

	"github.com/DragonDrew/ddum/internal/agent/discovery"
	"github.com/DragonDrew/ddum/internal/agent/docker"
	"github.com/DragonDrew/ddum/internal/ids"
	"github.com/DragonDrew/ddum/internal/protocol"
)

var t0 = time.Date(2026, 10, 1, 2, 0, 0, 0, time.UTC)

func dg(c string) string { return "sha256:" + strings.Repeat(c, 64) }

// fakeRunner emulates the docker CLI against a docker.Fake: pull adds the
// image, compose up recreates the named services on the pulled image.
type fakeRunner struct {
	mu      sync.Mutex
	engine  *docker.Fake
	calls   [][]string
	pullErr error
	// pullFails makes the next pullFails pulls fail, like a registry that
	// rate-limits for a moment.
	pullFails int
	upErr     error
	upOutput  string
	badImage  string // containers recreated on this image ID become unhealthy
	mismatch  bool   // pull stores the wrong digest
	// pullErrFor makes pulls of references containing it fail.
	pullErrFor string
	// hang makes commands with this first argument wait for their context.
	hang string
	n    int
	// configErr fails docker compose config; hashes and rendered are its answers.
	configErr error
	hashes    map[string]string
	rendered  string
}

func (r *fakeRunner) configAnswer(args []string) ([]byte, error) {
	if r.configErr != nil {
		return []byte("env file /elsewhere/app.env not found"), r.configErr
	}
	if slices.Contains(args, "--hash") {
		var out strings.Builder
		for name, h := range r.hashes {
			fmt.Fprintln(&out, name, h)
		}
		return []byte(out.String()), nil
	}
	return []byte(r.rendered), nil
}

func (r *fakeRunner) Run(ctx context.Context, dir string, args ...string) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, append([]string{dir}, args...))
	if r.hang == args[0] && !slices.Contains(args, "config") {
		<-ctx.Done()
		return []byte("hung"), ctx.Err()
	}
	switch args[0] {
	case "pull":
		if r.pullErr != nil {
			return []byte("Error response from daemon: manifest unknown"), r.pullErr
		}
		if r.pullErrFor != "" && strings.Contains(args[1], r.pullErrFor) {
			return []byte("manifest unknown"), errors.New("exit status 1")
		}
		if r.pullFails > 0 {
			r.pullFails--
			return []byte("error from registry: retry-after: 649µs, allowed: 44000/minute"), errors.New("exit status 1")
		}
		ref := args[1]
		repo, d, _ := strings.Cut(ref, "@")
		if r.mismatch {
			d = dg("f")
		}
		r.engine.Update(func(l []docker.Container, imgs map[string]docker.Image) ([]docker.Container, map[string]docker.Image) {
			imgs[ref] = docker.Image{ID: "img-" + d[7:15], RepoDigests: []string{repo + "@" + d}}
			imgs["img-"+d[7:15]] = imgs[ref]
			return l, imgs
		})
	case "compose":
		if slices.Contains(args, "config") {
			return r.configAnswer(args)
		}
		if r.upErr != nil {
			if r.upOutput != "" {
				return []byte(r.upOutput), r.upErr
			}
			return []byte("service web: error"), r.upErr
		}
		var services []string
		for i, a := range args {
			if a == "never" {
				services = args[i+1:]
			}
		}
		r.n++
		n := r.n
		r.engine.Update(func(l []docker.Container, imgs map[string]docker.Image) ([]docker.Container, map[string]docker.Image) {
			for i, c := range l {
				for _, s := range services {
					if c.Labels[discovery.LabelComposeService] == s {
						// The recreated container runs what the compose file now names.
						img := lookupImage(imgs, composeImage(dir, s))
						l[i].ID = fmt.Sprintf("new-%s-%d", s, n)
						l[i].ImageID = img.ID
						l[i].Health = "none"
						if img.ID != "" && img.ID == r.badImage {
							l[i].Health = "unhealthy"
						}
					}
				}
			}
			return l, imgs
		})
	}
	return []byte("ok"), nil
}

// pendingTags is kept so older tests read naturally; images now come from
// the compose file itself.
var pendingTags = map[string]string{}

var varRE = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(:-([^}]*))?\}`)

// composeImage reads a service's image: from dir/compose.yaml, expanding
// ${VAR} from dir/.env the way Compose does.
func composeImage(dir, service string) string {
	data, err := os.ReadFile(filepath.Join(dir, "compose.yaml"))
	if err != nil {
		return ""
	}
	var doc struct {
		Services map[string]struct {
			Image string `yaml:"image"`
		} `yaml:"services"`
	}
	yaml.Unmarshal(data, &doc)
	env := map[string]string{}
	if b, err := os.ReadFile(filepath.Join(dir, ".env")); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok && !strings.HasPrefix(k, "#") {
				env[k] = strings.Trim(v, `"'`)
			}
		}
	}
	return varRE.ReplaceAllStringFunc(doc.Services[service].Image, func(m string) string {
		p := varRE.FindStringSubmatch(m)
		if v, ok := env[p[1]]; ok {
			return v
		}
		return p[3]
	})
}

// lookupImage finds an image by reference as written or normalized.
func lookupImage(imgs map[string]docker.Image, ref string) docker.Image {
	if img, ok := imgs[ref]; ok {
		return img
	}
	if n, err := reference.ParseNormalizedNamed(ref); err == nil {
		if img, ok := imgs[reference.TagNameOnly(n).String()]; ok {
			return img
		}
		if img, ok := imgs[reference.FamiliarString(reference.TagNameOnly(n))]; ok {
			return img
		}
	}
	return docker.Image{}
}

type harness struct {
	t      *testing.T
	dir    string
	engine *docker.Fake
	runner *fakeRunner
	x      *Executor
	stacks []protocol.Stack
}

func newHarness(t *testing.T, compose, env string) *harness {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "stacks", "app")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte(compose), 0o644)
	if env != "" {
		os.WriteFile(filepath.Join(dir, ".env"), []byte(env), 0o644)
	}
	h := &harness{t: t, dir: dir, engine: &docker.Fake{}}
	h.runner = &fakeRunner{engine: h.engine}
	labels := func(svc string) map[string]string {
		return map[string]string{discovery.LabelComposeProject: "app", discovery.LabelComposeService: svc,
			discovery.LabelComposeContainerNumber: "1"}
	}
	h.engine.Set([]docker.Container{
		{ID: "old-web", Name: "app-web-1", Labels: labels("web"), ImageRef: "nginx:1.27", ImageID: "img-old", State: "running", Health: "none"},
		{ID: "old-db", Name: "app-db-1", Labels: labels("db"), ImageRef: "postgres:16", ImageID: "img-db", State: "running", Health: "none"},
	}, map[string]docker.Image{
		"img-old": {ID: "img-old", RepoDigests: []string{"nginx@" + dg("a")}},
		"img-db":  {ID: "img-db", RepoDigests: []string{"postgres@" + dg("d")}},
	})
	files := []discovery.SourceFile{{Path: filepath.Join(dir, "compose.yaml"), Data: []byte(compose)}}
	var envFiles []discovery.SourceFile
	if env != "" {
		envFiles = append(envFiles, discovery.SourceFile{Path: filepath.Join(dir, ".env"), Data: []byte(env)})
	}
	tags, _ := discovery.ClassifyTagSources(files, envFiles)
	webTC, dbTC := tags["web"], tags["db"]
	h.stacks = []protocol.Stack{{
		Fingerprint: "compose:app", DisplayName: "app", Provider: "compose", Deploy: protocol.Support{Supported: true},
		Compose: &protocol.ComposeInfo{Project: "app", ConfigFiles: []string{filepath.Join(dir, "compose.yaml")}, WorkingDir: dir, FilesAccessible: true},
		Containers: []protocol.Container{
			{IdentityKey: "web", DockerID: "old-web", ImageID: "img-old", ImageRef: "nginx:1.27", RepoDigests: []string{"nginx@" + dg("a")}, TagChange: &webTC},
			{IdentityKey: "db", DockerID: "old-db", ImageID: "img-db", ImageRef: "postgres:16", RepoDigests: []string{"postgres@" + dg("d")}, TagChange: &dbTC},
		},
	}}
	j, err := OpenJournal(filepath.Join(root, "data"))
	if err != nil {
		t.Fatal(err)
	}
	h.x = &Executor{Journal: j, Engine: h.engine, Runner: h.runner, Files: discovery.OSFiles{},
		AllowedRoots: []string{filepath.ToSlash(resolve(t, filepath.Join(root, "stacks")))}, DataDir: filepath.Join(root, "data"),
		Inventory: func() []protocol.Stack { return h.stacks }, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now: func() time.Time { return t0 }, VerifyPoll: time.Millisecond, NewID: ids.V7{}.New,
		PullRetryDelays: []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}}
	return h
}

func resolve(t *testing.T, p string) string {
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func command(id string, svc protocol.ServiceChange) protocol.Command {
	p := protocol.DeployPayload{OperationID: id, Scope: protocol.Scope{Kind: "stack", ID: "s1"},
		ExpiresAt: t0.Add(time.Hour).Format(time.RFC3339), Verify: protocol.VerifySettings{TimeoutSeconds: 1, StableSeconds: 0},
		Targets: []protocol.DeployTarget{{StackID: "s1", StackFingerprint: "compose:app", Services: []protocol.ServiceChange{svc}}}}
	b, _ := json.Marshal(p)
	return protocol.Command{OperationID: id, Type: protocol.CommandDeploy, Payload: b}
}

func webChange(tag, digest string) protocol.ServiceChange {
	return protocol.ServiceChange{ContainerID: "c-web", IdentityKey: "web",
		Current: protocol.ImageState{ImageRef: "nginx:1.27", Digest: dg("a")},
		Desired: protocol.ImageState{ImageRef: "docker.io/library/nginx:" + tag, Digest: digest}}
}

// run handles one command and executes it synchronously.
func (h *harness) run(cmd protocol.Command) []protocol.Event {
	h.t.Helper()
	h.x.init()
	h.x.Handle([]protocol.Command{cmd})
	select {
	case id := <-h.x.queue:
		h.x.execute(context.Background(), id)
	default:
	}
	return h.x.Pending()[cmd.OperationID]
}

func last(evs []protocol.Event) protocol.Event { return evs[len(evs)-1] }

func types(evs []protocol.Event) string {
	var s []string
	for _, e := range evs {
		t := e.Type
		if e.Step != "" {
			t += ":" + e.Step
		}
		s = append(s, t)
	}
	return strings.Join(s, ",")
}

const literalCompose = "services:\n  web:\n    image: nginx:1.27   # web server\n  db:\n    image: postgres:16\n"

func TestDigestUpdateSameTag(t *testing.T) {
	h := newHarness(t, literalCompose, "")
	pendingTags["web"] = "docker.io/library/nginx:1.27"
	id := ids.V7{}.New()
	evs := h.run(command(id, webChange("1.27", dg("b"))))
	if got := last(evs); got.Type != protocol.EventCompleted {
		t.Fatalf("events = %s; last = %+v", types(evs), got)
	}
	want := "accepted,progress:validating,progress:snapshotting,progress:pulling,progress:deploying,progress:verifying,progress:finalising,completed"
	if types(evs) != want {
		t.Fatalf("events = %s", types(evs))
	}
	r := last(evs).Result
	if r.Containers[0].Outcome != protocol.OutcomeUpdated || r.Containers[0].NewDigest != dg("b") || r.Containers[0].DockerID != "new-web-1" {
		t.Fatalf("result = %+v", r.Containers)
	}
	if b, _ := os.ReadFile(filepath.Join(h.dir, "compose.yaml")); string(b) != literalCompose {
		t.Fatal("a digest update must not edit files")
	}
	// The pull is by digest, the tag local, and compose names only web with --no-deps --pull never.
	calls := fmt.Sprint(h.runner.calls)
	if !strings.Contains(calls, "pull docker.io/library/nginx@"+dg("b")) ||
		!strings.Contains(calls, "up -d --no-deps --pull never web]") || strings.Contains(calls, " db") {
		t.Fatalf("calls = %v", h.runner.calls)
	}
	if len(h.engine.Tags) != 2 || !strings.HasPrefix(h.engine.Tags[0], "img-old -> ddum-rollback/") ||
		h.engine.Tags[1] != "docker.io/library/nginx@"+dg("b")+" -> docker.io/library/nginx:1.27" {
		t.Fatalf("tags = %v", h.engine.Tags)
	}
	if len(r.Snapshots) != 1 {
		t.Fatal("a snapshot must be taken before changing anything")
	}
	snap, err := os.ReadFile(filepath.Join(h.x.DataDir, "snapshots", r.Snapshots[0].SnapshotID, "snapshot.json"))
	if err != nil || !strings.Contains(string(snap), `"stack_fingerprint": "compose:app"`) {
		t.Fatalf("snapshot.json = %s %v", snap, err)
	}
}

func TestVersionUpdateLiteralImage(t *testing.T) {
	h := newHarness(t, literalCompose, "")
	pendingTags["web"] = "docker.io/library/nginx:1.28"
	evs := h.run(command(ids.V7{}.New(), webChange("1.28", dg("b"))))
	if last(evs).Type != protocol.EventCompleted {
		t.Fatalf("events = %s %+v", types(evs), last(evs))
	}
	b, _ := os.ReadFile(filepath.Join(h.dir, "compose.yaml"))
	if string(b) != "services:\n  web:\n    image: nginx:1.28   # web server\n  db:\n    image: postgres:16\n" {
		t.Fatalf("compose.yaml = %q", b)
	}
}

func TestVersionUpdateEnvVariable(t *testing.T) {
	compose := "services:\n  web:\n    image: nginx:${NGINX_TAG:-1.27}\n  db:\n    image: postgres:16\n"
	h := newHarness(t, compose, "# tag\nNGINX_TAG=1.27\n")
	pendingTags["web"] = "docker.io/library/nginx:1.28"
	evs := h.run(command(ids.V7{}.New(), webChange("1.28", dg("b"))))
	if last(evs).Type != protocol.EventCompleted {
		t.Fatalf("events = %s %+v", types(evs), last(evs))
	}
	env, _ := os.ReadFile(filepath.Join(h.dir, ".env"))
	if string(env) != "# tag\nNGINX_TAG=1.28\n" {
		t.Fatalf(".env = %q", env)
	}
	if b, _ := os.ReadFile(filepath.Join(h.dir, "compose.yaml")); string(b) != compose {
		t.Fatal("the compose file must not change for an env-file tag source")
	}
}

func TestDuplicateCommandRunsOnce(t *testing.T) {
	h := newHarness(t, literalCompose, "")
	pendingTags["web"] = "docker.io/library/nginx:1.27"
	cmd := command(ids.V7{}.New(), webChange("1.27", dg("b")))
	h.run(cmd)
	n := len(h.runner.calls)
	h.run(cmd) // the bot re-sends it
	if len(h.runner.calls) != n {
		t.Fatalf("a duplicate command was executed again: %v", h.runner.calls)
	}
	// Its events are re-sent until acknowledged, then dropped.
	evs := h.x.Pending()[cmd.OperationID]
	h.x.Acked(cmd.OperationID, last(evs).Seq)
	if len(h.x.Pending()[cmd.OperationID]) != 0 {
		t.Fatal("acknowledged events must be dropped")
	}
}

func TestCrashYieldsInterrupted(t *testing.T) {
	h := newHarness(t, literalCompose, "")
	id := ids.V7{}.New()
	h.x.Handle([]protocol.Command{command(id, webChange("1.27", dg("b")))}) // accepted, never executed: the agent "crashes"

	restarted := &Executor{Journal: h.x.Journal, Log: h.x.Log, Now: h.x.Now}
	restarted.Recover()
	evs := restarted.Pending()[id]
	if got := last(evs); got.Type != protocol.EventInterrupted || got.Code != protocol.FailInterrupted {
		t.Fatalf("after restart: %s", types(evs))
	}
	restarted.Recover()
	if n := len(restarted.Pending()[id]); n != len(evs) {
		t.Fatal("an interrupted operation must be reported once")
	}
}

func TestSafetyRules(t *testing.T) {
	cases := []struct {
		name   string
		change protocol.ServiceChange
		edit   func(h *harness)
		code   string
	}{
		{"repository change", protocol.ServiceChange{IdentityKey: "web", Desired: protocol.ImageState{ImageRef: "evil/nginx:1.27", Digest: dg("b")}},
			nil, protocol.FailUnsafe},
		{"no digest", protocol.ServiceChange{IdentityKey: "web", Desired: protocol.ImageState{ImageRef: "nginx:1.27"}}, nil, protocol.FailUnsafe},
		{"unknown service", protocol.ServiceChange{IdentityKey: "ghost", Desired: protocol.ImageState{ImageRef: "nginx:1.27", Digest: dg("b")}},
			nil, protocol.FailUnsafe},
		{"protected", webChange("1.27", dg("b")), func(h *harness) { h.stacks[0].Containers[0].Protected = true }, protocol.FailUnsafe},
		{"outside allowed roots", webChange("1.28", dg("b")), func(h *harness) { h.x.AllowedRoots = []string{"/elsewhere"} }, protocol.FailUnsafe},
		{"not in inventory", webChange("1.27", dg("b")), func(h *harness) { h.stacks[0].Fingerprint = "compose:other" }, protocol.FailUnsafe},
		{"portainer stack", webChange("1.27", dg("b")), func(h *harness) { h.stacks[0].Provider = "portainer" }, protocol.FailUnsupported},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t, literalCompose, "")
			if c.edit != nil {
				c.edit(h)
			}
			evs := h.run(command(ids.V7{}.New(), c.change))
			if len(evs) != 1 || evs[0].Type != protocol.EventRejected || evs[0].Code != c.code {
				t.Fatalf("events = %s %+v; want rejected %s", types(evs), evs, c.code)
			}
			if len(h.runner.calls) != 0 {
				t.Fatal("a rejected operation must not run anything")
			}
		})
	}
}

func TestDigestMismatchRecreatesNothing(t *testing.T) {
	h := newHarness(t, literalCompose, "")
	h.runner.mismatch = true
	evs := h.run(command(ids.V7{}.New(), webChange("1.27", dg("b"))))
	if got := last(evs); got.Type != protocol.EventFailed || got.Code != protocol.FailDigestMismatch {
		t.Fatalf("events = %s %+v", types(evs), got)
	}
	if h.ran("up") {
		t.Fatal("compose up ran after a digest mismatch")
	}
}

func TestVerifyFailsOnUnhealthy(t *testing.T) {
	h := newHarness(t, literalCompose, "")
	pendingTags["web"] = "docker.io/library/nginx:1.27"
	h.runner.badImage = "img-bbbbbbbb"
	evs := h.run(command(ids.V7{}.New(), webChange("1.27", dg("b"))))
	if got := last(evs); got.Type != protocol.EventFailed || got.Code != protocol.FailVerify || got.Result.Containers[0].Outcome != protocol.OutcomeFailed {
		t.Fatalf("events = %s %+v", types(evs), got)
	}
}

func TestPullFailureCarriesOutput(t *testing.T) {
	h := newHarness(t, literalCompose, "")
	h.runner.pullErr = errors.New("exit status 1")
	evs := h.run(command(ids.V7{}.New(), webChange("1.27", dg("b"))))
	got := last(evs)
	if got.Code != protocol.FailPull || !strings.Contains(got.OutputTail, "manifest unknown") {
		t.Fatalf("failed event = %+v", got)
	}
}

func TestExpiredCommandRejected(t *testing.T) {
	h := newHarness(t, literalCompose, "")
	h.x.Now = func() time.Time { return t0.Add(2 * time.Hour) }
	evs := h.run(command(ids.V7{}.New(), webChange("1.27", dg("b"))))
	if evs[0].Type != protocol.EventRejected || evs[0].Code != protocol.FailIneligible {
		t.Fatalf("events = %+v", evs)
	}
}

func TestRunRequestsInventory(t *testing.T) {
	h := newHarness(t, literalCompose, "")
	pendingTags["web"] = "docker.io/library/nginx:1.27"
	woke := make(chan struct{}, 10)
	h.x.Wake = func() { woke <- struct{}{} }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.x.Run(ctx)
	id := ids.V7{}.New()
	h.x.Handle([]protocol.Command{command(id, webChange("1.27", dg("b")))})
	deadline := time.After(5 * time.Second)
	for {
		evs := h.x.Pending()[id]
		if len(evs) > 0 && last(evs).Type == protocol.EventCompleted && h.x.TakeInventoryRequest() {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("operation did not finish: %s", types(h.x.Pending()[id]))
		case <-woke:
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// TestPullRetriesThroughARegistryBlip: a registry that refuses a pull for a
// moment (as lscr.io did with a rate-limit reply) costs a retry, not the
// deploy.
func TestPullRetriesThroughARegistryBlip(t *testing.T) {
	h := newHarness(t, literalCompose, "")
	h.runner.pullFails = 2
	evs := h.run(command(ids.V7{}.New(), webChange("1.27", dg("b"))))
	if got := last(evs); got.Type != protocol.EventCompleted {
		t.Fatalf("events = %s %+v", types(evs), got)
	}
	pulls := 0
	for _, c := range h.runner.calls {
		if len(c) > 1 && c[1] == "pull" {
			pulls++
		}
	}
	if pulls != 3 {
		t.Fatalf("pulls = %d, want 2 refused and 1 successful", pulls)
	}
}

// TestPullGivesUpAfterRetries: a pull that keeps failing still fails the
// deploy, after the retries, with the registry's output.
func TestPullGivesUpAfterRetries(t *testing.T) {
	h := newHarness(t, literalCompose, "")
	h.runner.pullFails = 10
	evs := h.run(command(ids.V7{}.New(), webChange("1.27", dg("b"))))
	got := last(evs)
	if got.Code != protocol.FailPull || !strings.Contains(got.OutputTail, "retry-after") {
		t.Fatalf("failed event = %+v", got)
	}
	if h.runner.pullFails != 6 {
		t.Fatalf("attempts = %d, want 4 (1 + 3 retries)", 10-h.runner.pullFails)
	}
}
