//go:build linux

package snapimage

import (
	"context"
	"fmt"
	"io"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/errdefs"
	"github.com/containerd/platforms"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// containerdBase is an image in containerd's content store.
type containerdBase struct {
	name     string
	store    content.Store
	manifest ocispec.Manifest
	config   []byte
}

// FromContainerd opens the image container id (in namespace ns) was
// created from, in the content store of the containerd at address. Close
// closes the connection.
func FromContainerd(ctx context.Context, address, ns, id string) (_ Base, closeFn func() error, retErr error) {
	client, err := containerd.New(address, containerd.WithDefaultNamespace(ns))
	if err != nil {
		return nil, nil, fmt.Errorf("containerd at %s: %w", address, err)
	}
	defer func() {
		if retErr != nil {
			client.Close()
		}
	}()
	c, err := client.LoadContainer(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	img, err := c.Image(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("container %s's image: %w", id, err)
	}
	store := client.ContentStore()
	m, err := images.Manifest(ctx, store, img.Target(), platforms.Default())
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", img.Name(), err)
	}
	config, err := content.ReadBlob(ctx, store, m.Config)
	if err != nil {
		return nil, nil, fmt.Errorf("%s's config: %w", img.Name(), err)
	}
	name := repository(img.Name()) + "@" + img.Target().Digest.String()
	return &containerdBase{name: name, store: store, manifest: m, config: config}, client.Close, nil
}

func (b *containerdBase) Name() string               { return b.name }
func (b *containerdBase) Manifest() ocispec.Manifest { return b.manifest }
func (b *containerdBase) Config() []byte             { return b.config }

func (b *containerdBase) Open(ctx context.Context, desc ocispec.Descriptor) (io.ReadCloser, error) {
	ra, err := b.store.ReaderAt(ctx, desc)
	if errdefs.IsNotFound(err) {
		// containerd keeps the compressed layers it unpacked, unless
		// told otherwise (discard_unpacked_layers).
		return nil, fmt.Errorf("not in containerd's content store (discard_unpacked_layers?): %w", err)
	}
	if err != nil {
		return nil, err
	}
	return struct {
		io.Reader
		io.Closer
	}{io.NewSectionReader(ra, 0, ra.Size()), ra}, nil
}
