package scheduler

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/google/go-containerregistry/pkg/name"

	"github.com/DragonDrew/ddum/internal/bot/domain"
	"github.com/DragonDrew/ddum/internal/bot/store"
	"github.com/DragonDrew/ddum/internal/protocol"
)

// foundBefore records a successful check of a container, as an earlier cycle would.
func (e *env) foundBefore(containerName string) {
	e.t.Helper()
	c := e.container(containerName)
	if err := e.db.SetTracking(context.Background(), c.ID, domain.TrackingVersion, "", "", e.now); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) container(containerName string) store.Container {
	e.t.Helper()
	cs, err := e.db.Containers(context.Background(), store.ContainerFilter{})
	if err != nil {
		e.t.Fatal(err)
	}
	for _, c := range cs {
		if c.DisplayName == containerName {
			return c
		}
	}
	e.t.Fatalf("no container %s", containerName)
	return store.Container{}
}

// setHost reports the host's Docker details and the containers' platform.
func (e *env) setHost(info protocol.DockerInfo, platform string, images map[string]string) {
	e.t.Helper()
	ctx := context.Background()
	if _, err := e.db.RecordPoll(ctx, store.Poll{AgentID: agentID, HostName: "h", PollInterval: 30, Docker: info, At: e.now}); err != nil {
		e.t.Fatal(err)
	}
	st := protocol.Stack{Fingerprint: "compose:app", DisplayName: "app", Provider: "compose", Deploy: protocol.Support{Supported: true},
		Compose: &protocol.ComposeInfo{Project: "app", FilesAccessible: true}}
	for svc, image := range images {
		ref, digest, _ := strings.Cut(image, "@")
		repo := ref[:strings.LastIndex(ref, ":")]
		st.Containers = append(st.Containers, protocol.Container{IdentityKey: svc, ImageRef: e.host + "/" + ref,
			RepoDigests: []string{e.host + "/" + repo + "@" + digest}, State: "running", Platform: platform,
			TagChange: &protocol.TagChange{Supported: true, Source: "image"}})
	}
	if _, err := e.db.ApplyInventory(ctx, agentID, protocol.Inventory{InventoryHash: "y", Stacks: []protocol.Stack{st}}, e.now); err != nil {
		e.t.Fatal(err)
	}
}

// pushIndex writes a multi-platform image and returns its digest.
func (e *env) pushIndex(repoTag string, platforms ...v1.Platform) string {
	e.t.Helper()
	var idx v1.ImageIndex = empty.Index
	for _, p := range platforms {
		img, err := random.Image(64, 1)
		if err != nil {
			e.t.Fatal(err)
		}
		idx = mutate.AppendManifests(idx, mutate.IndexAddendum{Add: img, Descriptor: v1.Descriptor{Platform: &p}})
	}
	ref, _ := name.NewTag(e.host+"/"+repoTag, name.Insecure)
	if err := remote.WriteIndex(ref, idx); err != nil {
		e.t.Fatal(err)
	}
	d, _ := idx.Digest()
	return d.String()
}

var (
	amd64Only = v1.Platform{OS: "linux", Architecture: "amd64"}
	arm64V8   = v1.Platform{OS: "linux", Architecture: "arm64", Variant: "v8"}
)

// A local build on the containerd store is not a failing check.
func TestNeverFoundIsUntrackedNotFailing(t *testing.T) {
	e := newEnv(t)
	d := e.push("app:1.0.0")
	e.deploy(map[string]string{"web": "app:1.0.0@" + d, "mybuild": "mybuild:dev@" + d}, nil)
	r := e.run()
	if r.Errors != 0 {
		t.Fatalf("a local build counted as a failed check: %+v", r)
	}
	c := e.container("mybuild")
	if c.Tracking != domain.TrackingUntracked || c.UntrackedReason != domain.ReasonNotInRegistry || !c.CheckFailingSince.IsZero() {
		t.Fatalf("tracking=%s reason=%q failing=%v", c.Tracking, c.UntrackedReason, c.CheckFailingSince)
	}
	// If it turns up later, it is checked like any other image.
	e.push("mybuild:dev")
	e.run()
	if c := e.container("mybuild"); c.Tracking == domain.TrackingUntracked {
		t.Fatalf("still untracked: %q", c.UntrackedReason)
	}
}

// The repository does not hold what runs, so this may be a local build that
// happens to share a public repository's name.
func TestUpdateOfAnImageNotInTheRepositoryWaitsForAPerson(t *testing.T) {
	const localBuild = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	e := newEnv(t)
	e.push("app:1.0.0")
	e.push("app:1.1.0")
	e.deploy(map[string]string{"web": "app:1.0.0@" + localBuild}, nil)
	e.setHost(protocol.DockerInfo{Arch: "amd64", ImageIDIsDigest: true}, "linux/amd64", map[string]string{"web": "app:1.0.0@" + localBuild})
	e.run()
	u, ok := e.detected()["web"]
	if !ok || !u.UnverifiedOrigin || !u.AutoFailed {
		t.Fatalf("update = %+v", u)
	}

	// Not checked where a digest does show that an image came from a registry.
	e2 := newEnv(t)
	e2.push("app:1.0.0")
	e2.push("app:1.1.0")
	e2.deploy(map[string]string{"web": "app:1.0.0@" + localBuild}, nil)
	e2.run()
	if u := e2.detected()["web"]; u.UnverifiedOrigin {
		t.Fatalf("flagged on the classic store: %+v", u)
	}
}

func TestPublishedImageIsNotFlagged(t *testing.T) {
	e := newEnv(t)
	d := e.push("app:1.0.0")
	e.push("app:1.1.0")
	e.deploy(map[string]string{"web": "app:1.0.0@" + d}, nil)
	e.setHost(protocol.DockerInfo{Arch: "amd64", ImageIDIsDigest: true}, "linux/amd64", map[string]string{"web": "app:1.0.0@" + d})
	e.run()
	if u, ok := e.detected()["web"]; !ok || u.UnverifiedOrigin {
		t.Fatalf("update = %+v", u)
	}
}

// An arm64 host is offered the newest version it can run.
func TestArmHostSkipsVersionsWithoutABuildForIt(t *testing.T) {
	e := newEnv(t)
	d := e.pushIndex("app:1.0.0", amd64Only, arm64V8)
	e.pushIndex("app:1.1.0", amd64Only, arm64V8)
	e.pushIndex("app:1.2.0", amd64Only) // no arm64 build
	e.deploy(map[string]string{"web": "app:1.0.0@" + d}, nil)
	e.setHost(protocol.DockerInfo{Arch: "arm64"}, "linux/arm64", map[string]string{"web": "app:1.0.0@" + d})
	e.run()
	if u := e.detected()["web"]; u.TargetVersion != "1.1.0" {
		t.Fatalf("offered %q, want 1.1.0", u.TargetVersion)
	}
	// An amd64 host of the same image gets the newest, without the extra requests.
	e2 := newEnv(t)
	d2 := e2.pushIndex("app:1.0.0", amd64Only, arm64V8)
	e2.pushIndex("app:1.2.0", amd64Only)
	e2.deploy(map[string]string{"web": "app:1.0.0@" + d2}, nil)
	e2.setHost(protocol.DockerInfo{Arch: "amd64"}, "linux/amd64", map[string]string{"web": "app:1.0.0@" + d2})
	e2.run()
	if u := e2.detected()["web"]; u.TargetVersion != "1.2.0" {
		t.Fatalf("offered %q, want 1.2.0", u.TargetVersion)
	}
}

func TestArmHostIgnoresATagThatMovedToABuildItCannotRun(t *testing.T) {
	e := newEnv(t)
	d := e.pushIndex("app:latest", amd64Only, arm64V8)
	e.deploy(map[string]string{"web": "app:latest@" + d}, nil)
	e.setHost(protocol.DockerInfo{Arch: "arm64"}, "linux/arm64", map[string]string{"web": "app:latest@" + d})
	e.pushIndex("app:latest", amd64Only)
	e.run()
	if u, ok := e.detected()["web"]; ok {
		t.Fatalf("offered %+v", u)
	}
	e.pushIndex("app:latest", amd64Only, arm64V8)
	e.run()
	if _, ok := e.detected()["web"]; !ok {
		t.Fatal("a build it can run was not offered")
	}
}

// finishOperation records an operation that ended now on the host's agent.
func (e *env) finishOperation(hostID string) {
	e.t.Helper()
	at := store.FormatTime(e.now.Add(time.Second))
	err := e.db.WriteTx(context.Background(), func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO operations (id, kind, agent_id, host_id, scope_type, scope_id, payload_json, state,
			expires_at, initiator_kind, created_at, updated_at, finished_at)
			VALUES ('0195f2c4-0000-7000-8000-0000000000aa', 'rollback', ?, ?, 'stack', 's', '{}', 'succeeded', ?, 'user', ?, ?, ?)`,
			agentID, hostID, at, at, at, at)
		return err
	})
	if err != nil {
		e.t.Fatal(err)
	}
}

// A container just rolled back still looks like the new version until its host
// reports, and a check must not conclude from that that it needs no update.
func TestACheckWaitsForAHostThatJustFinishedAnOperation(t *testing.T) {
	e := newEnv(t)
	d := e.push("app:1.0.0")
	e.push("app:1.1.0")
	e.deploy(map[string]string{"web": "app:1.1.0@" + d}, nil)
	h, err := e.db.HostByAgent(context.Background(), agentID)
	if err != nil {
		t.Fatal(err)
	}
	e.finishOperation(h.ID)

	// Never reports: the host's containers are left alone.
	e.check.InventoryWait = 2 * time.Second
	var slept int
	e.check.Sleep = func(context.Context, time.Duration) error { slept++; return nil }
	if r := e.run(); r.Checked != 0 || slept != 2 {
		t.Fatalf("result = %+v after %d waits", r, slept)
	}

	// Reports during the wait: the check goes ahead with what it now runs.
	e.check.InventoryWait = 0
	slept = 0
	e.check.Sleep = func(context.Context, time.Duration) error {
		slept++
		e.now = e.now.Add(2 * time.Second)
		e.deploy(map[string]string{"web": "app:1.1.0@" + d}, nil)
		return nil
	}
	if r := e.run(); r.Checked != 1 || slept != 1 {
		t.Fatalf("result = %+v after %d waits", r, slept)
	}
}

// A version outside tag_track is mentioned, and never offered.
func TestNewerVersionOutsideTrackIsRecorded(t *testing.T) {
	e := newEnv(t)
	d := e.push("app:2026.12.1")
	e.push("app:2027.1.0")
	e.deploy(map[string]string{"web": "app:2026.12.1@" + d}, nil)
	e.run()
	if u, ok := e.detected()["web"]; ok {
		t.Fatalf("offered %+v under same-major", u)
	}
	if c := e.container("web"); c.NewerOutsideTrack != "2027.1.0" {
		t.Fatalf("newer = %q", c.NewerOutsideTrack)
	}
	// Within the track there is nothing else to mention.
	e2 := newEnv(t)
	d2 := e2.push("app:1.0.0")
	e2.push("app:1.2.0")
	e2.deploy(map[string]string{"web": "app:1.0.0@" + d2}, nil)
	e2.run()
	if c := e2.container("web"); c.NewerOutsideTrack != "" {
		t.Fatalf("newer = %q", c.NewerOutsideTrack)
	}
}
