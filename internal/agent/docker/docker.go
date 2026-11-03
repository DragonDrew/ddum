// Package docker is the agent's thin interface over the Docker Engine API
// . Discovery depends only on the Engine interface so tests can
// use the Fake.
package docker

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/client"

	"github.com/DragonDrew/ddum/internal/protocol"
)

// Container is what discovery needs to know about one container.
type Container struct {
	ID           string
	Name         string // without the leading slash
	Labels       map[string]string
	ImageRef     string // Config.Image: the reference as configured
	ImageID      string
	State        string // created, running, paused, restarting, removing, exited, dead
	Health       string // none, starting, healthy, unhealthy
	RestartCount int
	CreatedAt    time.Time
	// Mounts and Env are what the container was created with. They stay on the
	// agent: Env can hold secrets, and only the deploy preflight reads it.
	Mounts []Mount
	Env    []string
}

// Mount is one of a container's mounts.
type Mount struct {
	Type        string // bind, volume, tmpfs, ...
	Source      string
	Destination string
}

// Image is what discovery needs to know about one local image.
type Image struct {
	ID          string
	RepoDigests []string
	OS          string
	Arch        string
	Variant     string
	Labels      map[string]string // the image's own labels
}

// Engine is the subset of the Engine API the agent uses.
type Engine interface {
	Info(ctx context.Context) (protocol.DockerInfo, error)
	Containers(ctx context.Context) ([]Container, error)
	// Image returns ErrNotFound if the image no longer exists locally. id may
	// also be a reference such as nginx:1.27.
	Image(ctx context.Context, id string) (Image, error)
	// Tag adds the tag target to the local image source.
	Tag(ctx context.Context, source, target string) error
	// Untag removes a tag (the image goes only if nothing else uses it).
	Untag(ctx context.Context, ref string) error
	// ImageIDIsDigest reports whether image IDs are registry digests, as they
	// are in the containerd image store (Docker's default on new installs since
	// 29). Otherwise an ID is the digest of the image's config.
	ImageIDIsDigest(ctx context.Context) (bool, error)
}

// ErrNotFound is returned for objects that do not exist.
var ErrNotFound = errors.New("not found")

// Client implements Engine with the moby client.
type Client struct {
	c *client.Client
}

// New connects to the Engine API at host (DOCKER_HOST syntax). API version
// negotiation is on by default, so older daemons work.
func New(host string) (*Client, error) {
	c, err := client.New(client.WithHost(host), client.WithUserAgent("ddum-agent"))
	if err != nil {
		return nil, fmt.Errorf("docker client: %w", err)
	}
	return &Client{c: c}, nil
}

// Close releases the client's connections.
func (d *Client) Close() error { return d.c.Close() }

// Info returns the engine version and platform.
func (d *Client) Info(ctx context.Context) (protocol.DockerInfo, error) {
	v, err := d.c.ServerVersion(ctx, client.ServerVersionOptions{})
	if err != nil {
		return protocol.DockerInfo{}, err
	}
	return protocol.DockerInfo{EngineVersion: v.Version, APIVersion: v.APIVersion, OS: v.Os, Arch: v.Arch}, nil
}

// Containers lists every container, running or not, and inspects each one.
// A container removed between list and inspect is skipped.
func (d *Client) Containers(ctx context.Context) ([]Container, error) {
	list, err := d.c.ContainerList(ctx, client.ContainerListOptions{All: true})
	if err != nil {
		return nil, err
	}
	out := make([]Container, 0, len(list.Items))
	for _, s := range list.Items {
		res, err := d.c.ContainerInspect(ctx, s.ID, client.ContainerInspectOptions{})
		if err != nil {
			if isNotFound(err) {
				continue
			}
			return nil, fmt.Errorf("inspect %s: %w", s.ID, err)
		}
		in := res.Container
		c := Container{
			ID:           in.ID,
			Name:         strings.TrimPrefix(in.Name, "/"),
			ImageID:      in.Image,
			RestartCount: in.RestartCount,
			State:        string(s.State),
			Health:       "none",
		}
		if in.Config != nil {
			c.Labels = in.Config.Labels
			c.ImageRef = in.Config.Image
			c.Env = in.Config.Env
		}
		for _, m := range in.Mounts {
			c.Mounts = append(c.Mounts, Mount{Type: string(m.Type), Source: m.Source, Destination: m.Destination})
		}
		if in.State != nil {
			if in.State.Status != "" {
				c.State = string(in.State.Status)
			}
			if in.State.Health != nil && in.State.Health.Status != "" {
				c.Health = string(in.State.Health.Status)
			}
		}
		if t, err := time.Parse(time.RFC3339Nano, in.Created); err == nil {
			c.CreatedAt = t.UTC()
		}
		out = append(out, c)
	}
	return out, nil
}

// Image inspects a local image by ID.
func (d *Client) Image(ctx context.Context, id string) (Image, error) {
	res, err := d.c.ImageInspect(ctx, id)
	if err != nil {
		if isNotFound(err) {
			return Image{}, ErrNotFound
		}
		return Image{}, err
	}
	img := Image{
		ID:          res.ID,
		RepoDigests: res.RepoDigests,
		OS:          res.Os,
		Arch:        res.Architecture,
		Variant:     res.Variant,
	}
	if res.Config != nil {
		img.Labels = res.Config.Labels
	}
	return img, nil
}

// isNotFound matches the Engine API's not-found errors. The client reports a
// daemon 404 as an error that matches errdefs.ErrNotFound through errors.Is,
// without the NotFound() marker method its own client-side errors have;
// errdefs.IsNotFound accepts both.
func isNotFound(err error) bool {
	return cerrdefs.IsNotFound(err)
}

// ImageIDIsDigest implements Engine.
func (d *Client) ImageIDIsDigest(ctx context.Context) (bool, error) {
	res, err := d.c.Info(ctx, client.InfoOptions{})
	if err != nil {
		return false, err
	}
	for _, kv := range res.Info.DriverStatus {
		if kv[0] == "driver-type" && strings.HasPrefix(kv[1], "io.containerd.snapshotter") {
			return true, nil
		}
	}
	return false, nil
}

// Tag implements Engine.
func (d *Client) Tag(ctx context.Context, source, target string) error {
	_, err := d.c.ImageTag(ctx, client.ImageTagOptions{Source: source, Target: target})
	return err
}

// Untag implements Engine. A missing tag is not an error.
func (d *Client) Untag(ctx context.Context, ref string) error {
	_, err := d.c.ImageRemove(ctx, ref, client.ImageRemoveOptions{})
	if err != nil && isNotFound(err) {
		return nil
	}
	return err
}
