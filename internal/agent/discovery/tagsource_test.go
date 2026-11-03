package discovery

import (
	"strings"
	"testing"

	"github.com/DragonDrew/ddum/internal/protocol"
)

func classifyOne(t *testing.T, compose []string, env string) map[string]protocol.TagChange {
	t.Helper()
	var files []SourceFile
	for i, c := range compose {
		files = append(files, SourceFile{Path: "f" + string(rune('0'+i)), Data: []byte(c)})
	}
	var envFiles []SourceFile
	if env != "" {
		envFiles = []SourceFile{{Path: ".env", Data: []byte(env)}}
	}
	got, err := ClassifyTagSources(files, envFiles)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func want(t *testing.T, got map[string]protocol.TagChange, svc string, supported bool, source, variable, reason string) {
	t.Helper()
	tc, ok := got[svc]
	if !ok {
		t.Fatalf("%s: not classified (%v)", svc, got)
	}
	if tc.Supported != supported || tc.Source != source || tc.Variable != variable || tc.Reason != reason {
		t.Errorf("%s: got %+v, want supported=%v source=%q variable=%q reason=%q", svc, tc, supported, source, variable, reason)
	}
}

func TestTagSourceForms(t *testing.T) {
	got := classifyOne(t, []string{`
services:
  literal:
    image: nginx:1.27-alpine
  defined:
    image: ghcr.io/immich-app/immich-server:${IMMICH_VERSION:-release}
  defaultonly:
    image: redis:${REDIS_TAG:-7}
  shellonly:
    image: postgres:${PG_TAG}
  plainvar:
    image: valkey/valkey:${VALKEY_TAG}
  repotemplated:
    image: ${REGISTRY}/app:1.0
  partial:
    image: app:${MAJOR}.${MINOR}
  pinned:
    image: nginx@sha256:0000000000000000000000000000000000000000000000000000000000000000
  built:
    build: .
`}, "# comment\nIMMICH_VERSION=v1.140.1\nexport VALKEY_TAG=\"8\"\n")

	want(t, got, "literal", true, TagSourceImage, "", "")
	want(t, got, "defined", true, TagSourceEnvFile, "IMMICH_VERSION", "")
	want(t, got, "defaultonly", false, "", "", TagReasonFromDefault)
	want(t, got, "shellonly", false, "", "", TagReasonTemplated)
	want(t, got, "plainvar", true, TagSourceEnvFile, "VALKEY_TAG", "")
	want(t, got, "repotemplated", false, "", "", TagReasonTemplated)
	want(t, got, "partial", false, "", "", TagReasonTemplated)
	want(t, got, "pinned", false, TagSourceImage, "", TagReasonPinnedDigest)
	if _, ok := got["built"]; ok {
		t.Error("a service without image: has no tag source")
	}
}

func TestTagSourceSharedVariable(t *testing.T) {
	got := classifyOne(t, []string{`
services:
  server:
    image: ghcr.io/immich-app/immich-server:${IMMICH_VERSION}
  ml:
    image: ghcr.io/immich-app/immich-machine-learning:${IMMICH_VERSION}-cuda
  worker:
    image: app:${APP_TAG}
    environment:
      VERSION: $APP_TAG
  other:
    image: app2:${APP2_TAG}
`}, "IMMICH_VERSION=v1.140.1\nAPP_TAG=1.0\nAPP2_TAG=2.0\n")
	want(t, got, "server", false, TagSourceEnvFile, "IMMICH_VERSION", TagReasonSharedVariable)
	want(t, got, "ml", false, "", "", TagReasonTemplated) // the variable is not the whole tag
	want(t, got, "other", true, TagSourceEnvFile, "APP2_TAG", "")
	want(t, got, "worker", true, TagSourceEnvFile, "APP_TAG", "") // its own use does not count
}

func TestTagSourceAnchors(t *testing.T) {
	got := classifyOne(t, []string{`
x-base: &base
  image: myorg/app:1.0
  restart: unless-stopped
services:
  a:
    <<: *base
  b:
    <<: *base
  c:
    image: &solo nginx:1.27
  d:
    image: &shared redis:7.2
  e:
    image: *shared
`}, "")
	want(t, got, "a", false, TagSourceImage, "", TagReasonSharedAnchor)
	want(t, got, "b", false, TagSourceImage, "", TagReasonSharedAnchor)
	want(t, got, "c", true, TagSourceImage, "", "") // anchored but used once
	want(t, got, "d", false, TagSourceImage, "", TagReasonSharedAnchor)
	want(t, got, "e", false, TagSourceImage, "", TagReasonSharedAnchor)
}

func TestTagSourceOverrideFiles(t *testing.T) {
	base := `
services:
  web:
    image: nginx:${NGINX_TAG}
  db:
    image: postgres:16.4
`
	override := `
services:
  web:
    image: nginx:1.27-alpine
  db:
    environment:
      POSTGRES_DB: app
`
	got := classifyOne(t, []string{base, override}, "")
	// The last file that sets image: wins; db keeps the base file's literal.
	want(t, got, "web", true, TagSourceImage, "", "")
	want(t, got, "db", true, TagSourceImage, "", "")
}

func TestTagChangeInInventory(t *testing.T) {
	fileData["/opt/stacks/app/compose.yaml"] = "services:\n  web:\n    image: nginx:${NGINX_TAG}\n"
	fileData["/opt/stacks/app/.env"] = "NGINX_TAG=1.27-alpine\n"
	defer func() {
		delete(fileData, "/opt/stacks/app/compose.yaml")
		delete(fileData, "/opt/stacks/app/.env")
	}()
	files := okFiles("app")
	files["/opt/stacks/app/.env"] = "/opt/stacks/app/.env"
	s := Discover([]Observed{composeCtr("c", "app", "web", "1", nil)},
		Options{EnabledProviders: allProviders, ComposeAllowedRoots: []string{"/opt/stacks"}, Files: files})[0]
	tc := s.Containers[0].TagChange
	if tc == nil || !tc.Supported || tc.Source != TagSourceEnvFile || tc.Variable != "NGINX_TAG" {
		t.Fatalf("tag_change = %+v", tc)
	}

	// Inaccessible or standalone stacks report the deploy reason.
	s = Discover([]Observed{plainCtr("x", "solo", nil)}, Options{EnabledProviders: allProviders})[0]
	if tc := s.Containers[0].TagChange; tc == nil || tc.Supported || tc.Reason != protocol.ReasonStandaloneManual {
		t.Fatalf("standalone tag_change = %+v", tc)
	}
}

func TestPortainerStacks(t *testing.T) {
	portainerCtr := func(project, service string) Observed {
		return composeCtr("p-"+service, project, service, "1", map[string]string{
			LabelComposeConfigFiles: "/data/compose/7/docker-compose.yml", LabelComposeWorkingDir: "/data/compose/7"})
	}
	opt := Options{EnabledProviders: []string{"portainer", "compose", "docker"}, Portainer: map[string]PortainerStack{
		"immich": {ID: 7, EndpointID: 2, Content: "services:\n  server:\n    image: ghcr.io/x/server:${IMMICH_VERSION}\n  db:\n    image: postgres:16\n",
			Env: []string{"IMMICH_VERSION=v1.140.1"}},
		"gitapp": {ID: 9, EndpointID: 2, Git: true},
	}}
	stacks := Discover([]Observed{portainerCtr("immich", "server"), portainerCtr("immich", "db"), portainerCtr("gitapp", "web"),
		portainerCtr("elsewhere", "x")}, opt)
	im := find(t, stacks, "compose:immich")
	if im.Provider != "portainer" || !im.Deploy.Supported || !strings.Contains(string(im.ProviderRef), `"portainer_stack_id":7`) ||
		!strings.Contains(string(im.ProviderRef), `"portainer_stack_type":"file"`) {
		t.Fatalf("file stack: provider=%s deploy=%+v ref=%s", im.Provider, im.Deploy, im.ProviderRef)
	}
	for _, c := range im.Containers {
		switch c.IdentityKey {
		case "server":
			if c.TagChange == nil || !c.TagChange.Supported || c.TagChange.Source != TagSourcePortainerEnv || c.TagChange.Variable != "IMMICH_VERSION" {
				t.Errorf("server tag change = %+v", c.TagChange)
			}
		case "db":
			if c.TagChange == nil || !c.TagChange.Supported || c.TagChange.Source != TagSourceImage {
				t.Errorf("db tag change = %+v", c.TagChange)
			}
		}
	}
	g := find(t, stacks, "compose:gitapp")
	if !g.Deploy.Supported || g.Containers[0].TagChange.Supported || g.Containers[0].TagChange.Reason != TagReasonGitManaged {
		t.Fatalf("git stack: %+v %+v", g.Deploy, g.Containers[0].TagChange)
	}
	other := find(t, stacks, "compose:elsewhere")
	if other.Provider != "portainer" || other.Deploy.Supported || other.Deploy.Reason != "not a stack of the configured Portainer environment" {
		t.Fatalf("unmatched Portainer stack: %+v", other.Deploy)
	}
}
