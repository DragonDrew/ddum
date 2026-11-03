package discovery

import (
	"context"
	"testing"
	"time"

	"github.com/DragonDrew/ddum/internal/agent/docker"
)

// On Docker 29.1's containerd store, a running image whose tag moved keeps
// existing with no digest record. It must stay tracked, by its ID.
func TestAnImageThatLostItsDigestRecordStaysTracked(t *testing.T) {
	const id = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	for _, c := range []struct {
		name      string
		digestIDs bool
		want      int
	}{{"containerd store", true, 1}, {"classic store", false, 0}} {
		t.Run(c.name, func(t *testing.T) {
			f := &docker.Fake{DigestIDs: c.digestIDs}
			f.Set([]docker.Container{{ID: "d1", Name: "web", ImageRef: "nginx:latest", ImageID: id, State: "running"}},
				map[string]docker.Image{id: {ID: id, OS: "linux", Arch: "arm64"}})
			col := &Collector{Engine: f, Now: time.Now, Options: Options{EnabledProviders: []string{"docker"}}}
			snap, err := col.Collect(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			got := snap.Stacks[0].Containers[0]
			if len(got.RepoDigests) != c.want {
				t.Fatalf("digests = %v", got.RepoDigests)
			}
			if c.want == 1 && got.RepoDigests[0] != "docker.io/library/nginx@"+id {
				t.Fatalf("digest = %s", got.RepoDigests[0])
			}
		})
	}
}

// Labels an image set on itself are not the owner's instructions.
func TestImageLabelsAreNotInstructions(t *testing.T) {
	image := map[string]string{"ddum.ignore": "true", "ddum.self": "bot", LabelComposeProject: "whoami",
		LabelComposeService: "whoami", "org.opencontainers.image.source": "https://example.org"}
	got := OwnLabels(map[string]string{"ddum.ignore": "true", "ddum.self": "bot", LabelComposeProject: "whoami",
		LabelComposeService: "whoami", "org.opencontainers.image.source": "https://example.org", "ddum.group": "mine"}, image)
	for _, k := range []string{"ddum.ignore", "ddum.self", LabelComposeProject, LabelComposeService} {
		if _, ok := got[k]; ok {
			t.Errorf("%s came from the image and should be gone", k)
		}
	}
	if got["ddum.group"] != "mine" || got["org.opencontainers.image.source"] == "" {
		t.Errorf("labels the image did not set, and other labels, must stay: %v", got)
	}
	// The owner overriding an image's label is the owner's choice.
	if got := OwnLabels(map[string]string{"ddum.ignore": "false"}, image); got["ddum.ignore"] != "false" {
		t.Errorf("an override was dropped: %v", got)
	}
}
