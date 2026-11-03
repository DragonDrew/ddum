package discovery

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/distribution/reference"

	"github.com/DragonDrew/ddum/internal/agent/docker"
	"github.com/DragonDrew/ddum/internal/agent/portainer"
	"github.com/DragonDrew/ddum/internal/protocol"
)

// OSFiles checks paths on the agent's own filesystem.
type OSFiles struct{}

// Resolve implements FileChecker. The path must exist and be readable.
func (OSFiles) Resolve(p string) (string, error) {
	resolved, err := filepath.EvalSymlinks(p)
	if err != nil {
		return "", err
	}
	f, err := os.Open(resolved)
	if err != nil {
		return "", err
	}
	f.Close()
	return filepath.ToSlash(resolved), nil
}

// Snapshot is one complete inventory collection.
type Snapshot struct {
	Docker    protocol.DockerInfo
	Providers []protocol.ProviderStatus
	Stacks    []protocol.Stack
	Hash      string
	At        time.Time
}

// Collector reads Docker and builds a Snapshot.
type Collector struct {
	Engine  docker.Engine
	Options Options
	Now     func() time.Time
	// Portainer, if set, is the Portainer API of the configured environment.
	Portainer PortainerLister
	// Rollback, if set, returns usable snapshot IDs per stack fingerprint,
	// newest first.
	Rollback func(ctx context.Context) map[string][]string
	// PortainerTimeout limits the Portainer calls of one collection (default 20 s).
	PortainerTimeout time.Duration

	mu    sync.Mutex
	files map[int]cachedFile
	// lastPortainer is the latest successful listing.
	lastPortainer map[string]PortainerStack
}

func (c *Collector) portainerTimeout() time.Duration {
	if c.PortainerTimeout > 0 {
		return c.PortainerTimeout
	}
	return 20 * time.Second
}

// Collect lists containers, inspects their images, and runs Discover. A
// Docker error is reported as provider status "error" with no stacks, so the
// bot still hears from the agent; it is also returned so the caller
// can log it.
func (c *Collector) Collect(ctx context.Context) (Snapshot, error) {
	snap := Snapshot{At: c.Now().UTC(), Stacks: []protocol.Stack{}}
	info, infoErr := c.Engine.Info(ctx)
	if infoErr == nil {
		info.ImageIDIsDigest, _ = c.Engine.ImageIDIsDigest(ctx)
	}
	snap.Docker = info

	opts := c.Options
	portainerStatus := protocol.ProviderStatus{Type: protocol.ProviderPortainer, Status: "ok"}
	if c.Portainer != nil {
		// Discovery runs before every poll, so a Portainer that hangs must not
		// make the host look offline.
		pctx, cancel := context.WithTimeout(ctx, c.portainerTimeout())
		m, perr := c.portainerStacks(pctx)
		cancel()
		if perr != nil {
			// Keep the stacks as they were: an outage says nothing about them,
			// and deploys through the API fail on their own.
			portainerStatus.Status, portainerStatus.Detail = "error", perr.Error()
			opts.Portainer = c.lastPortainer
		} else {
			c.lastPortainer = m
			opts.Portainer = m
			portainerStatus.Detail = strconv.Itoa(len(m)) + " stacks in the environment"
		}
	}
	observed, err := c.observe(ctx)
	if err == nil {
		err = infoErr
	}
	if err != nil {
		snap.Providers = c.statuses("error", err.Error())
	} else {
		snap.Providers = c.statuses("ok", "")
		for i := range snap.Providers {
			if snap.Providers[i].Type == protocol.ProviderPortainer {
				snap.Providers[i] = portainerStatus
			}
		}
		snap.Stacks = Discover(observed, opts)
		if c.Rollback != nil {
			avail := c.Rollback(ctx)
			for i := range snap.Stacks {
				if ids := avail[snap.Stacks[i].Fingerprint]; len(ids) > 0 {
					snap.Stacks[i].Rollback = protocol.RollbackInfo{Available: true, SnapshotID: ids[0], SnapshotIDs: ids}
				}
			}
		}
	}
	snap.Hash = protocol.InventoryHash(snap.Providers, snap.Stacks)
	return snap, err
}

func (c *Collector) observe(ctx context.Context) ([]Observed, error) {
	list, err := c.Engine.Containers(ctx)
	if err != nil {
		return nil, err
	}
	images := map[string]*docker.Image{}
	out := make([]Observed, 0, len(list))
	for _, ctr := range list {
		img, seen := images[ctr.ImageID]
		if !seen {
			i, err := c.Engine.Image(ctx, ctr.ImageID)
			switch {
			case err == nil:
				img = &i
			case errors.Is(err, docker.ErrNotFound):
				img = nil
			default:
				return nil, err
			}
			images[ctr.ImageID] = img
		}
		switch {
		case img == nil:
			img = c.goneImage(ctx, ctr)
		case len(img.RepoDigests) == 0:
			ctr.Labels = OwnLabels(ctr.Labels, img.Labels)
			if known := c.goneImage(ctx, ctr); known != nil {
				withDigest := *img
				withDigest.RepoDigests = known.RepoDigests
				img = &withDigest
			}
		default:
			ctr.Labels = OwnLabels(ctr.Labels, img.Labels)
		}
		out = append(out, Observed{Container: ctr, Image: img})
	}
	return out, nil
}

// goneImage describes the image of a container whose image Docker no longer
// has, or has kept without its digest record. The containerd image store drops
// an image when its last tag moves, even while a container runs it, which a plain
// `docker pull` on the host can cause; Docker 29.1 keeps the image but forgets
// its digest. Either way the image ID is the registry digest, so the digest is
// still known and the container stays tracked: it does run an older image than
// its tag names, and a build that was never pushed is recognised by its checks
// . Other stores keep such an image with no way to tell its digest.
func (c *Collector) goneImage(ctx context.Context, ctr docker.Container) *docker.Image {
	ok, err := c.Engine.ImageIDIsDigest(ctx)
	if err != nil || !ok || !strings.HasPrefix(ctr.ImageID, "sha256:") {
		return nil
	}
	named, err := reference.ParseNormalizedNamed(ctr.ImageRef)
	if err != nil {
		return nil
	}
	return &docker.Image{ID: ctr.ImageID, RepoDigests: []string{named.Name() + "@" + ctr.ImageID}}
}

// statuses reports one entry per enabled provider.
func (c *Collector) statuses(status, detail string) []protocol.ProviderStatus {
	var out []protocol.ProviderStatus
	for _, p := range c.Options.EnabledProviders {
		s := protocol.ProviderStatus{Type: p, Status: status, Detail: detail}
		out = append(out, s)
	}
	return out
}

// ReadFile implements FileChecker. Compose and env files are small; anything
// over 1 MiB is refused.
func (OSFiles) ReadFile(p string) ([]byte, error) {
	resolved, err := filepath.EvalSymlinks(p)
	if err != nil {
		return nil, err
	}
	st, err := os.Stat(resolved)
	if err != nil {
		return nil, err
	}
	if st.Size() > 1<<20 {
		return nil, fmt.Errorf("%s is larger than 1 MiB", p)
	}
	return os.ReadFile(resolved)
}

// PortainerLister is the part of the Portainer API discovery uses.
type PortainerLister interface {
	Stacks(ctx context.Context) ([]portainer.Stack, error)
	StackFile(ctx context.Context, id int) (string, error)
}

type cachedFile struct {
	content string
	at      time.Time
}

// portainerFileTTL bounds how stale a cached stack file may be.
const portainerFileTTL = time.Minute

// portainerStacks lists the environment's stacks by lower-case name (Compose
// project names are lower case), with each file-based stack's file.
func (c *Collector) portainerStacks(ctx context.Context) (map[string]PortainerStack, error) {
	stacks, err := c.Portainer.Stacks(ctx)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	if c.files == nil {
		c.files = map[int]cachedFile{}
	}
	c.mu.Unlock()
	out := map[string]PortainerStack{}
	for _, s := range stacks {
		ps := PortainerStack{ID: s.ID, EndpointID: s.EndpointID, Git: s.GitBased()}
		for _, e := range s.Env {
			ps.Env = append(ps.Env, e.Name+"="+e.Value)
		}
		if !ps.Git {
			c.mu.Lock()
			f, ok := c.files[s.ID]
			c.mu.Unlock()
			if !ok || c.Now().Sub(f.at) > portainerFileTTL {
				content, err := c.Portainer.StackFile(ctx, s.ID)
				if err != nil {
					return nil, err
				}
				f = cachedFile{content: content, at: c.Now()}
				c.mu.Lock()
				c.files[s.ID] = f
				c.mu.Unlock()
			}
			ps.Content = f.content
		}
		out[strings.ToLower(s.Name)] = ps
	}
	return out, nil
}

// ForgetPortainerFile drops a cached stack file after the agent changed it.
func (c *Collector) ForgetPortainerFile(id int) {
	c.mu.Lock()
	delete(c.files, id)
	c.mu.Unlock()
}
