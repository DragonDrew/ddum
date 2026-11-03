package docker

import (
	"context"
	"sync"

	"github.com/DragonDrew/ddum/internal/protocol"
)

// Fake is an in-memory Engine for tests.
type Fake struct {
	mu         sync.Mutex
	EngineInfo protocol.DockerInfo
	List       []Container
	Images     map[string]Image
	Err        error // returned by every call when set
	Tags       []string
	// DigestIDs makes image IDs registry digests, like the containerd store.
	DigestIDs bool
}

var _ Engine = (*Fake)(nil)

// Info implements Engine.
func (f *Fake) Info(context.Context) (protocol.DockerInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.EngineInfo, f.Err
}

// Containers implements Engine.
func (f *Fake) Containers(context.Context) ([]Container, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return nil, f.Err
	}
	return append([]Container(nil), f.List...), nil
}

// Image implements Engine.
func (f *Fake) Image(_ context.Context, id string) (Image, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return Image{}, f.Err
	}
	img, ok := f.Images[id]
	if !ok {
		return Image{}, ErrNotFound
	}
	return img, nil
}

// ImageIDIsDigest implements Engine.
func (f *Fake) ImageIDIsDigest(context.Context) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.DigestIDs, f.Err
}

// Set replaces the container list and images under the lock.
func (f *Fake) Set(list []Container, images map[string]Image) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.List = list
	f.Images = images
}

// Tag implements Engine: the target name resolves to the source image.
func (f *Fake) Tag(_ context.Context, source, target string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return f.Err
	}
	img, ok := f.Images[source]
	if !ok {
		return ErrNotFound
	}
	if f.Images == nil {
		f.Images = map[string]Image{}
	}
	f.Images[target] = img
	f.Tags = append(f.Tags, source+" -> "+target)
	return nil
}

// Update applies fn to the container list under the lock.
func (f *Fake) Update(fn func(list []Container, images map[string]Image) ([]Container, map[string]Image)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.List, f.Images = fn(f.List, f.Images)
}

// Untag implements Engine.
func (f *Fake) Untag(_ context.Context, ref string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.Images, ref)
	f.Tags = append(f.Tags, "untag "+ref)
	return nil
}
