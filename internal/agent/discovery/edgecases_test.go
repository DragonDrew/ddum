package discovery

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/DragonDrew/ddum/internal/agent/docker"
	"github.com/DragonDrew/ddum/internal/agent/portainer"
	"github.com/DragonDrew/ddum/internal/protocol"
)

// Docker's containerd store drops an image once its last tag moves, even while
// a container runs it. There the image ID is the digest, so the container
// stays tracked and is not mistaken for a local build.
func TestGoneImageKeepsItsDigestOnTheContainerdStore(t *testing.T) {
	d := "sha256:" + strings.Repeat("a", 64)
	ctr := docker.Container{ID: "c1", Name: "web", ImageRef: "ghcr.io/x/web:latest", ImageID: d, State: "running"}
	collect := func(t *testing.T, digestIDs bool) []string {
		t.Helper()
		fake := &docker.Fake{DigestIDs: digestIDs}
		fake.Set([]docker.Container{ctr}, map[string]docker.Image{})
		col := &Collector{Engine: fake, Options: Options{EnabledProviders: allProviders}, Now: time.Now}
		snap, err := col.Collect(context.Background())
		if err != nil || len(snap.Stacks) != 1 {
			t.Fatalf("snapshot = %+v, %v", snap, err)
		}
		return snap.Stacks[0].Containers[0].RepoDigests
	}
	if got := collect(t, true); len(got) != 1 || got[0] != "ghcr.io/x/web@"+d {
		t.Fatalf("containerd store: %v", got)
	}
	if got := collect(t, false); len(got) != 0 {
		t.Fatalf("an image ID is no digest on other stores: %v", got)
	}
}

// hangingPortainer waits for its context on every call once told to.
type hangingPortainer struct {
	hang   bool
	stacks []portainer.Stack
}

func (h *hangingPortainer) Stacks(ctx context.Context) ([]portainer.Stack, error) {
	if h.hang {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return h.stacks, nil
}

func (h *hangingPortainer) StackFile(context.Context, int) (string, error) {
	return "services:\n  web:\n    image: nginx:1.27\n", nil
}

// Discovery runs before every poll. A Portainer that hangs must not make the
// host look offline, nor its stacks look undeployable.
func TestHangingPortainerDoesNotHoldUpOrChangeDiscovery(t *testing.T) {
	o := composeCtr("c1", "app", "web", "1", map[string]string{
		LabelComposeConfigFiles: "/data/compose/12/docker-compose.yml", LabelComposeWorkingDir: "/data/compose/12"})
	fake := &docker.Fake{EngineInfo: protocol.DockerInfo{OS: "linux", Arch: "amd64"}}
	fake.Set([]docker.Container{o.Container}, map[string]docker.Image{"sha256:img": *o.Image})
	api := &hangingPortainer{stacks: []portainer.Stack{{ID: 12, Name: "app", Type: portainer.TypeCompose, EndpointID: 2}}}
	col := &Collector{Engine: fake, Portainer: api, PortainerTimeout: 20 * time.Millisecond, Now: time.Now,
		Options: Options{EnabledProviders: []string{"portainer", "compose", "docker"}}}

	good, err := col.Collect(context.Background())
	if err != nil || !good.Stacks[0].Deploy.Supported {
		t.Fatalf("good = %+v, %v", good.Stacks, err)
	}
	api.hang = true
	start := time.Now()
	bad, err := col.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("collecting took %s", d)
	}
	if !bad.Stacks[0].Deploy.Supported || bad.Stacks[0].Provider != protocol.ProviderPortainer {
		t.Fatalf("an outage changed the stack: %+v", bad.Stacks[0])
	}
	var status string
	for _, p := range bad.Providers {
		if p.Type == protocol.ProviderPortainer {
			status = p.Status
		}
	}
	if status != "error" {
		t.Fatalf("Portainer status = %q", status)
	}
}
