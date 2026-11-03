package discovery

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/DragonDrew/ddum/internal/agent/docker"
	"github.com/DragonDrew/ddum/internal/protocol"
)

// mapFiles is a FileChecker over a fixed set of paths. A value other than
// the key models a symlink.
type mapFiles map[string]string

func (m mapFiles) Resolve(p string) (string, error) {
	if r, ok := m[p]; ok {
		return r, nil
	}
	return "", errors.New("no such file")
}

var allProviders = []string{"compose", "docker"}

func composeCtr(id, project, service, number string, extra map[string]string) Observed {
	l := map[string]string{
		LabelComposeProject:         project,
		LabelComposeService:         service,
		LabelComposeContainerNumber: number,
		LabelComposeOneoff:          "False",
		LabelComposeConfigFiles:     "/opt/stacks/" + project + "/compose.yaml",
		LabelComposeWorkingDir:      "/opt/stacks/" + project,
	}
	for k, v := range extra {
		l[k] = v
	}
	return Observed{
		Container: docker.Container{ID: id, Name: project + "-" + service + "-" + number, Labels: l,
			ImageRef: "nginx:1.27", ImageID: "sha256:img", State: "running", Health: "none"},
		Image: &docker.Image{ID: "sha256:img", RepoDigests: []string{"nginx@sha256:aaa"}, OS: "linux", Arch: "amd64"},
	}
}

func plainCtr(id, name string, labels map[string]string) Observed {
	return Observed{Container: docker.Container{ID: id, Name: name, Labels: labels, ImageRef: "redis", ImageID: "sha256:r", State: "running"}}
}

func okFiles(projects ...string) mapFiles {
	m := mapFiles{}
	for _, p := range projects {
		m["/opt/stacks/"+p] = "/opt/stacks/" + p
		m["/opt/stacks/"+p+"/compose.yaml"] = "/opt/stacks/" + p + "/compose.yaml"
	}
	return m
}

func find(t *testing.T, stacks []protocol.Stack, fp string) protocol.Stack {
	t.Helper()
	for _, s := range stacks {
		if s.Fingerprint == fp {
			return s
		}
	}
	t.Fatalf("stack %s not found in %+v", fp, stacks)
	return protocol.Stack{}
}

func TestFingerprintsAndIdentityKeys(t *testing.T) {
	obs := []Observed{
		composeCtr("c1", "immich", "server", "1", nil),
		composeCtr("c2", "immich", "db", "1", nil),
		plainCtr("c3", "media-a", map[string]string{LabelStack: "media"}),
		plainCtr("c4", "media-b", map[string]string{LabelStack: "media"}),
		plainCtr("c5", "portainer", map[string]string{LabelProtected: "true"}),
	}
	stacks := Discover(obs, Options{EnabledProviders: allProviders, ComposeAllowedRoots: []string{"/opt/stacks"}, Files: okFiles("immich")})

	if len(stacks) != 3 {
		t.Fatalf("got %d stacks, want 3: %+v", len(stacks), stacks)
	}
	im := find(t, stacks, "compose:immich")
	if im.Provider != "compose" || im.DisplayName != "immich" {
		t.Errorf("immich: provider=%s name=%s", im.Provider, im.DisplayName)
	}
	if got := keys(im); got != "db,server" {
		t.Errorf("immich identity keys = %s, want db,server (service names)", got)
	}
	if !im.Deploy.Supported || !im.Compose.FilesAccessible {
		t.Errorf("immich should be deployable: %+v", im.Deploy)
	}

	media := find(t, stacks, "label:media")
	if media.Provider != "docker" || keys(media) != "media-a,media-b" {
		t.Errorf("label stack: provider=%s keys=%s", media.Provider, keys(media))
	}
	if media.Deploy.Supported || media.Deploy.Reason != protocol.ReasonStandaloneManual {
		t.Errorf("label stack deploy = %+v, want standalone_manual", media.Deploy)
	}

	p := find(t, stacks, "container:portainer")
	if keys(p) != "portainer" || !p.Containers[0].Protected {
		t.Errorf("standalone portainer: keys=%s protected=%v", keys(p), p.Containers[0].Protected)
	}
	if p.Deploy.Reason != protocol.ReasonStandaloneManual {
		t.Errorf("standalone deploy reason = %q", p.Deploy.Reason)
	}
}

func TestIdentitySurvivesRecreate(t *testing.T) {
	opt := Options{EnabledProviders: allProviders, ComposeAllowedRoots: []string{"/opt/stacks"}, Files: okFiles("app")}
	before := Discover([]Observed{composeCtr("old-id", "app", "web", "1", nil)}, opt)
	after := Discover([]Observed{composeCtr("new-id", "app", "web", "1", nil)}, opt)
	if before[0].Fingerprint != after[0].Fingerprint || before[0].Containers[0].IdentityKey != after[0].Containers[0].IdentityKey {
		t.Fatalf("identity changed across recreate: %+v → %+v", before[0], after[0])
	}
	if after[0].Containers[0].DockerID != "new-id" {
		t.Fatalf("docker ID should be the new runtime ID")
	}
}

func TestOneoffIgnoredAndReplicas(t *testing.T) {
	run := composeCtr("x", "app", "web", "1", map[string]string{LabelComposeOneoff: "True"})
	obs := []Observed{
		composeCtr("w2", "app", "web", "2", nil),
		composeCtr("w1", "app", "web", "1", nil),
		run,
	}
	stacks := Discover(obs, Options{EnabledProviders: allProviders, ComposeAllowedRoots: []string{"/opt/stacks"}, Files: okFiles("app")})
	s := find(t, stacks, "compose:app")
	if len(s.Containers) != 1 {
		t.Fatalf("got %d containers, want 1 (replicas reported once, oneoff ignored)", len(s.Containers))
	}
	c := s.Containers[0]
	if c.DockerID != "w1" {
		t.Errorf("reported replica %s, want the lowest-numbered w1", c.DockerID)
	}
	if len(c.Notes) != 1 || c.Notes[0] != protocol.NoteReplicasUnsupported {
		t.Errorf("notes = %v, want replicas_unsupported", c.Notes)
	}
	if s.Deploy.Supported || !strings.HasPrefix(s.Deploy.Reason, protocol.ReasonReplicasUnsupported) {
		t.Errorf("deploy = %+v, want replicas_unsupported", s.Deploy)
	}
}

func TestLabels(t *testing.T) {
	obs := []Observed{
		plainCtr("a", "agent", map[string]string{LabelSelf: "agent", "other.label": "x"}),
		plainCtr("b", "bot", map[string]string{LabelSelf: "bot", "ddum.ignore": "true"}),
		plainCtr("c", "weird", map[string]string{LabelSelf: "something"}),
	}
	stacks := Discover(obs, Options{EnabledProviders: allProviders})
	agent := find(t, stacks, "container:agent").Containers[0]
	if agent.Self != "agent" || !agent.Protected {
		t.Errorf("agent: self=%q protected=%v, want agent/true", agent.Self, agent.Protected)
	}
	if _, leaked := agent.DdumLabels["other.label"]; leaked || agent.DdumLabels[LabelSelf] != "agent" {
		t.Errorf("only ddum.* labels may be reported: %v", agent.DdumLabels)
	}
	bot := find(t, stacks, "container:bot").Containers[0]
	if bot.Self != "bot" || bot.Protected {
		t.Errorf("bot: self=%q protected=%v, want bot/false", bot.Self, bot.Protected)
	}
	if bot.DdumLabels["ddum.ignore"] != "true" {
		t.Errorf("ddum.ignore not reported: %v", bot.DdumLabels)
	}
	if w := find(t, stacks, "container:weird").Containers[0]; w.Self != "" || w.Protected {
		t.Errorf("unknown ddum.self value: self=%q protected=%v", w.Self, w.Protected)
	}
}

func TestPortainerHeuristic(t *testing.T) {
	o := composeCtr("p1", "immich", "server", "1", map[string]string{
		LabelComposeConfigFiles: "/data/compose/12/docker-compose.yml",
		LabelComposeWorkingDir:  "/data/compose/12",
	})
	stacks := Discover([]Observed{o}, Options{EnabledProviders: allProviders, ComposeAllowedRoots: []string{"/opt/stacks"}})
	s := find(t, stacks, "compose:immich")
	if s.Provider != "portainer" || s.Deploy.Supported || s.Deploy.Reason != protocol.ReasonPortainerNotEnabled {
		t.Fatalf("got provider=%s deploy=%+v", s.Provider, s.Deploy)
	}
	if s.Fingerprint != "compose:immich" {
		t.Fatalf("provider must not change the fingerprint")
	}
}

func TestComposeFileAccess(t *testing.T) {
	roots := []string{"/opt/stacks"}
	cases := []struct {
		name    string
		opt     Options
		labels  map[string]string
		ok      bool
		wantMsg string
	}{
		{"accessible", Options{ComposeAllowedRoots: roots, Files: okFiles("app")}, nil, true, ""},
		{"no roots", Options{Files: okFiles("app")}, nil, false, "COMPOSE_ALLOWED_ROOTS is not set"},
		{"outside roots", Options{ComposeAllowedRoots: []string{"/srv"}, Files: okFiles("app")}, nil, false, "not under COMPOSE_ALLOWED_ROOTS"},
		{"not mounted", Options{ComposeAllowedRoots: roots, Files: mapFiles{}}, nil, false, "mount it at the same path"},
		{"symlink escapes", Options{ComposeAllowedRoots: roots, Files: mapFiles{
			"/opt/stacks/app": "/opt/stacks/app", "/opt/stacks/app/compose.yaml": "/etc/compose.yaml"}}, nil, false, "outside COMPOSE_ALLOWED_ROOTS"},
		{"prefix is not a parent", Options{ComposeAllowedRoots: []string{"/opt/stack"}, Files: okFiles("app")}, nil, false, "not under"},
		{"env file checked", Options{ComposeAllowedRoots: roots, Files: okFiles("app")},
			map[string]string{LabelComposeEnvironmentFile: "/opt/stacks/app/.env"}, false, ".env is not readable"},
		{"compose disabled", Options{EnabledProviders: []string{"docker"}, ComposeAllowedRoots: roots, Files: okFiles("app")}, nil, false, "compose provider not enabled"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.opt.EnabledProviders == nil {
				c.opt.EnabledProviders = allProviders
			}
			s := Discover([]Observed{composeCtr("c", "app", "web", "1", c.labels)}, c.opt)[0]
			if s.Deploy.Supported != c.ok || s.Compose.FilesAccessible != c.ok {
				t.Fatalf("deploy=%+v files_accessible=%v, want supported=%v", s.Deploy, s.Compose.FilesAccessible, c.ok)
			}
			if !strings.Contains(s.Deploy.Reason, c.wantMsg) {
				t.Fatalf("reason %q does not contain %q", s.Deploy.Reason, c.wantMsg)
			}
		})
	}
}

func TestImageDetails(t *testing.T) {
	o := composeCtr("c", "app", "web", "1", nil)
	o.Image = &docker.Image{RepoDigests: []string{"b@sha256:2", "a@sha256:1"}, OS: "linux", Arch: "arm", Variant: "v7"}
	gone := plainCtr("g", "gone", nil) // image removed: no digests, no platform
	stacks := Discover([]Observed{o, gone}, Options{EnabledProviders: allProviders})
	c := find(t, stacks, "compose:app").Containers[0]
	if c.Platform != "linux/arm/v7" || strings.Join(c.RepoDigests, " ") != "a@sha256:1 b@sha256:2" {
		t.Errorf("platform=%s digests=%v", c.Platform, c.RepoDigests)
	}
	g := find(t, stacks, "container:gone").Containers[0]
	if g.Platform != "" || g.RepoDigests == nil || len(g.RepoDigests) != 0 {
		t.Errorf("missing image: platform=%q digests=%#v", g.Platform, g.RepoDigests)
	}
}

func TestCollectorHashStableAndErrors(t *testing.T) {
	now := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	fake := &docker.Fake{EngineInfo: protocol.DockerInfo{EngineVersion: "29.6.0", OS: "linux", Arch: "amd64"}}
	o := composeCtr("c1", "app", "web", "1", nil)
	fake.Set([]docker.Container{o.Container}, map[string]docker.Image{"sha256:img": *o.Image})
	col := &Collector{Engine: fake, Options: Options{EnabledProviders: allProviders}, Now: func() time.Time { return now }}

	a, err := col.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// A restart changes only volatile fields: the hash must not change.
	o.Container.RestartCount = 3
	o.Container.CreatedAt = now
	fake.Set([]docker.Container{o.Container}, map[string]docker.Image{"sha256:img": *o.Image})
	b, _ := col.Collect(context.Background())
	if a.Hash != b.Hash {
		t.Fatalf("hash changed on a volatile field")
	}
	// A state change must change the hash.
	o.Container.State = "exited"
	fake.Set([]docker.Container{o.Container}, map[string]docker.Image{"sha256:img": *o.Image})
	c, _ := col.Collect(context.Background())
	if c.Hash == a.Hash {
		t.Fatalf("hash unchanged after a state change")
	}

	fake.Err = errors.New("daemon down")
	d, err := col.Collect(context.Background())
	if err == nil || len(d.Stacks) != 0 || d.Providers[0].Status != "error" {
		t.Fatalf("docker error: err=%v stacks=%d providers=%+v", err, len(d.Stacks), d.Providers)
	}
}

func keys(s protocol.Stack) string {
	var k []string
	for _, c := range s.Containers {
		k = append(k, c.IdentityKey)
	}
	return strings.Join(k, ",")
}

// ReadFile implements FileChecker; content is looked up in fileData.
func (m mapFiles) ReadFile(p string) ([]byte, error) {
	if d, ok := fileData[p]; ok {
		return []byte(d), nil
	}
	if _, ok := m[p]; ok {
		return []byte("services: {}\n"), nil
	}
	return nil, errors.New("no such file")
}

// fileData holds the content served by mapFiles.ReadFile in tests.
var fileData = map[string]string{}
