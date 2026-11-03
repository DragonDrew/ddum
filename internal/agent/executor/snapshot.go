package executor

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"

	"github.com/DragonDrew/ddum/internal/agent/docker"
	"github.com/DragonDrew/ddum/internal/ids"
)

// snapshotMeta is snapshot.json.
type snapshotMeta struct {
	SnapshotID       string            `json:"snapshot_id"`
	OperationID      string            `json:"operation_id"`
	StackID          string            `json:"stack_id"`
	StackFingerprint string            `json:"stack_fingerprint"`
	Provider         string            `json:"provider"`
	PortainerStackID int               `json:"portainer_stack_id,omitempty"`
	Services         []snapshotService `json:"services"`
	CreatedAt        time.Time         `json:"created_at"`
}

// snapshotService is one service's previous state.
type snapshotService struct {
	IdentityKey string `json:"identity_key"`
	ContainerID string `json:"container_id"` // the bot's UUID
	ImageRef    string `json:"image_ref"`    // configured reference to restore
	NewTag      string `json:"new_tag"`      // the tag the deployment moved it to
	Digest      string `json:"digest"`
	ImageID     string `json:"image_id"`
	Pin         string `json:"pin"` // local tag that keeps the image
	DockerID    string `json:"docker_id"`
}

var unsafeTagChars = regexp.MustCompile(`[^A-Za-z0-9_.-]`)

// PinTag is the local tag that keeps a snapshot's previous image from being
// pruned: ddum-rollback/<snapshot short id>:<identity key>.
func PinTag(snapshotID, identityKey string) string {
	short := snapshotID
	if len(short) > 12 {
		short = short[len(short)-12:] // the random end of a UUIDv7
	}
	key := unsafeTagChars.ReplaceAllString(identityKey, "_")
	if len(key) > 120 {
		key = key[:120]
	}
	return "ddum-rollback/" + short + ":" + key
}

func (x *Executor) snapshotDir(id string) string {
	return filepath.Join(x.DataDir, "snapshots", id)
}

// snapshot pins each target's current image and records what a rollback needs
// before anything changes. It keeps no copies of the stack's
// files: those can hold secrets, and a rollback changes only the tags.
func (x *Executor) snapshot(ctx context.Context, opID, stackID string, sp stackPlan) (string, error) {
	id := x.NewID()
	dir := x.snapshotDir(id)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", err
	}
	meta := snapshotMeta{SnapshotID: id, OperationID: opID, StackID: stackID, StackFingerprint: sp.stack.Fingerprint,
		Provider: sp.stack.Provider, CreatedAt: x.Now().UTC()}
	for _, s := range sp.services {
		pin := PinTag(id, s.change.IdentityKey)
		if s.container.ImageID == "" {
			return "", errors.New("the running image of " + s.change.IdentityKey + " is unknown; cannot pin it")
		}
		source, err := x.pinSource(ctx, s)
		if err != nil {
			return "", err
		}
		if source == "" {
			// Nothing to pin, so this snapshot cannot be restored, and the bot is
			// told so. That must not stop the update itself.
			x.Log.Warn("the running image is gone and cannot be fetched again; this deploy cannot be rolled back", "service", s.change.IdentityKey)
			pin = ""
		} else if err := x.Engine.Tag(ctx, source, pin); err != nil {
			return "", err
		}
		meta.Services = append(meta.Services, snapshotService{IdentityKey: s.change.IdentityKey, ContainerID: s.change.ContainerID,
			ImageRef: s.container.ImageRef, NewTag: s.desiredTag, Digest: s.change.Current.Digest, ImageID: s.container.ImageID, Pin: pin,
			DockerID: s.container.DockerID})
	}
	if sp.portainer != nil {
		meta.PortainerStackID = sp.portainer.stack.ID
	}
	b, _ := json.MarshalIndent(meta, "", "  ")
	if err := WriteFileAtomic(filepath.Join(dir, "snapshot.json"), b, 0o640); err != nil {
		return "", err
	}
	x.prune(ctx, sp.stack.Fingerprint)
	return id, nil
}

// pinSource names the image to pin for a service: the running image or, if
// Docker no longer has it, that image pulled again by digest. Docker's
// containerd image store drops an image once its last tag moves, even while a
// container runs it, so a `docker pull` on the host can cause this. It returns
// "" when the image cannot be had.
func (x *Executor) pinSource(ctx context.Context, s servicePlan) (string, error) {
	_, err := x.Engine.Image(ctx, s.container.ImageID)
	if err == nil {
		return s.container.ImageID, nil
	}
	if !errors.Is(err, docker.ErrNotFound) {
		return "", err
	}
	if s.change.Current.Digest == "" {
		return "", nil
	}
	ref := s.repo + "@" + s.change.Current.Digest
	if out, err := x.pull(ctx, ref); err != nil {
		x.Log.Warn("could not fetch the running image again", "image", ref, "err", err, "output", tail(string(out), 300))
		return "", nil
	}
	return ref, nil
}

// loadSnapshot reads snapshot.json.
// Snapshot IDs come from the bot in rollback commands; only a UUID may name a
// directory, so a crafted ID cannot reach outside DATA_DIR/snapshots.
func (x *Executor) loadSnapshot(id string) (*snapshotMeta, error) {
	if _, err := ids.Parse(id); err != nil {
		return nil, err
	}
	b, err := os.ReadFile(filepath.Join(x.snapshotDir(id), "snapshot.json"))
	if err != nil {
		return nil, err
	}
	var m snapshotMeta
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// snapshots lists every snapshot, newest first.
func (x *Executor) snapshots() []*snapshotMeta {
	dirs, err := os.ReadDir(filepath.Join(x.DataDir, "snapshots"))
	if err != nil {
		return nil
	}
	var out []*snapshotMeta
	for _, d := range dirs {
		if m, err := x.loadSnapshot(d.Name()); err == nil {
			out = append(out, m)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out
}

// dropFileCopies removes the copies of stack files that snapshots made before
// 0.10. Nothing reads them any more, and they can hold secrets.
func (x *Executor) dropFileCopies() {
	dirs, err := os.ReadDir(filepath.Join(x.DataDir, "snapshots"))
	if err != nil {
		return
	}
	for _, d := range dirs {
		if err := os.RemoveAll(filepath.Join(x.snapshotDir(d.Name()), "files")); err != nil {
			x.Log.Warn("remove old snapshot file copies", "snapshot_id", d.Name(), "err", err)
		}
	}
}

// usable reports whether a snapshot can still be restored: every pinned image
// exists. If one was deleted outside ddum the snapshot is unavailable; the
// agent never re-pulls an old tag.
func (x *Executor) usable(ctx context.Context, m *snapshotMeta) bool {
	for _, s := range m.Services {
		if s.Pin == "" {
			return false
		}
		if _, err := x.Engine.Image(ctx, s.Pin); err != nil {
			return false
		}
	}
	return true
}

// AvailableSnapshots returns the usable snapshot IDs of each stack
// fingerprint, newest first, for the inventory.
func (x *Executor) AvailableSnapshots(ctx context.Context) map[string][]string {
	out := map[string][]string{}
	for _, m := range x.snapshots() {
		if x.usable(ctx, m) {
			out[m.StackFingerprint] = append(out[m.StackFingerprint], m.SnapshotID)
		}
	}
	return out
}

// prune keeps the newest RetentionCount snapshots of a stack and removes
// older ones with their pin tags.
func (x *Executor) prune(ctx context.Context, fingerprint string) {
	keep := x.RetentionCount
	if keep < 1 {
		keep = 3
	}
	n := 0
	for _, m := range x.snapshots() {
		if m.StackFingerprint != fingerprint {
			continue
		}
		n++
		if n <= keep {
			continue
		}
		for _, s := range m.Services {
			if s.Pin == "" {
				continue
			}
			if err := x.Engine.Untag(ctx, s.Pin); err != nil {
				x.Log.Warn("remove pin tag", "tag", s.Pin, "err", err)
			}
		}
		if err := os.RemoveAll(x.snapshotDir(m.SnapshotID)); err != nil {
			x.Log.Warn("remove snapshot", "snapshot_id", m.SnapshotID, "err", err)
		} else {
			x.Log.Info("snapshot pruned", "snapshot_id", m.SnapshotID, "stack", fingerprint)
		}
	}
}
