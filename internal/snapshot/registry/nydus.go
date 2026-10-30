//go:build linux

package registry

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/containerd/errdefs"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content"

	"github.com/makhov/gcr/internal/nydus"
)

// Formats a snapshot is pushed in. In the nydus format each part (the
// CRIU images, each writable layer) is a nydus image: a bootstrap, which
// identifies the part, and a blob of its file data, absent for a part
// without any. A node can mount a part with nydusd and read only what it
// touches.
const (
	FormatTar   = "tar"
	FormatNydus = "nydus"

	NydusBootstrapMediaType = "application/vnd.gcr.snapshot.nydus.bootstrap.v1"
	// NydusBlobMediaType is nydus's own, as in nydus container images.
	NydusBlobMediaType = "application/vnd.oci.image.layer.nydus.blob.v1"

	// On a bootstrap: which part it is, and the digest of its blob.
	annotationPart      = "dev.gcr.snapshot.part"
	annotationNydusBlob = "dev.gcr.snapshot.nydus.blob"
)

// part is one part of a snapshot in the registry: a tar (desc), or a
// nydus bootstrap (desc) and its blob. desc identifies the part.
type part struct {
	desc ocispec.Descriptor
	blob *ocispec.Descriptor
}

func (p part) descriptors() []ocispec.Descriptor {
	if p.blob == nil {
		return []ocispec.Descriptor{p.desc}
	}
	return []ocispec.Descriptor{p.desc, *p.blob}
}

// cached is the part as BlobsFile records it, for a snapshot's images or
// its writable layer.
func (p part) cached(images bool) blobs {
	d := p.desc
	if p.desc.MediaType != NydusBootstrapMediaType {
		if images {
			return blobs{Images: &d}
		}
		return blobs{Upper: &d}
	}
	if images {
		return blobs{NydusImages: p.descriptors()}
	}
	return blobs{NydusUpper: p.descriptors()}
}

// pushNydusPart pushes dir/name as a nydus image, unless the registry has
// it, and returns its descriptors (bootstrap first), made once per
// snapshot (BlobsFile).
func pushNydusPart(ctx context.Context, target oras.Target, dir, name string) ([]ocispec.Descriptor, error) {
	var cache blobs
	if b, err := os.ReadFile(filepath.Join(dir, BlobsFile)); err == nil {
		_ = json.Unmarshal(b, &cache)
	}
	slot := &cache.NydusUpper
	if name == "images" {
		slot = &cache.NydusImages
	}
	if *slot != nil {
		missing := false
		for _, d := range *slot {
			exists, err := target.Exists(ctx, d)
			if err != nil {
				return nil, err
			}
			missing = missing || !exists
		}
		if !missing {
			return *slot, nil
		}
	}
	work, err := os.MkdirTemp("", "gcr-nydus-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(work)
	img, err := nydus.Build(filepath.Join(dir, name), work)
	if err != nil {
		return nil, err
	}
	boot, err := fileDescriptor(img.Bootstrap, NydusBootstrapMediaType)
	if err != nil {
		return nil, err
	}
	boot.Annotations = map[string]string{annotationPart: name}
	descs := []ocispec.Descriptor{boot}
	files := []string{img.Bootstrap}
	if img.Blob != "" {
		blob, err := fileDescriptor(img.Blob, NydusBlobMediaType)
		if err != nil {
			return nil, err
		}
		// The bootstrap names its blob by its sha256, and nydusd fetches
		// it from the registry by that.
		if blob.Digest.Encoded() != filepath.Base(img.Blob) {
			return nil, fmt.Errorf("nydus-image named the blob of %s/%s %s, its digest is %s", dir, name, filepath.Base(img.Blob), blob.Digest)
		}
		blob.Annotations = map[string]string{"containerd.io/snapshot/nydus-blob": "true"}
		boot.Annotations[annotationNydusBlob] = blob.Digest.String()
		descs = []ocispec.Descriptor{boot, blob}
		files = append(files, img.Blob)
	}
	if *slot != nil && (*slot)[0].Digest != boot.Digest {
		return nil, fmt.Errorf("%s/%s changed since it was packed (%s, now %s)", dir, name, (*slot)[0].Digest, boot.Digest)
	}
	for i, d := range descs {
		if exists, err := target.Exists(ctx, d); err != nil {
			return nil, err
		} else if exists {
			continue
		}
		f, err := os.Open(files[i])
		if err != nil {
			return nil, err
		}
		err = target.Push(ctx, d, f)
		f.Close()
		if err != nil {
			return nil, err
		}
	}
	*slot = descs
	b, _ := json.Marshal(cache)
	_ = os.WriteFile(filepath.Join(dir, BlobsFile), b, 0o600)
	return descs, nil
}

func fileDescriptor(path, mediaType string) (ocispec.Descriptor, error) {
	f, err := os.Open(path)
	if err != nil {
		return ocispec.Descriptor{}, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return ocispec.Descriptor{}, err
	}
	return ocispec.Descriptor{MediaType: mediaType, Digest: digest.NewDigest(digest.SHA256, h), Size: n}, nil
}

// nydusParts reads the parts of a nydus snapshot's manifest: each a
// bootstrap, followed by its blob if it has one.
func nydusParts(layers []ocispec.Descriptor) ([]part, error) {
	var parts []part
	for i := 0; i < len(layers); i++ {
		d := layers[i]
		if d.MediaType != NydusBootstrapMediaType {
			return nil, fmt.Errorf("layer %d: %q, want a nydus bootstrap", i, d.MediaType)
		}
		p := part{desc: d}
		if want := d.Annotations[annotationNydusBlob]; want != "" {
			if i+1 >= len(layers) || layers[i+1].MediaType != NydusBlobMediaType || layers[i+1].Digest.String() != want {
				return nil, fmt.Errorf("layer %d: its blob %s does not follow it", i, want)
			}
			i++
			blob := layers[i]
			p.blob = &blob
		}
		parts = append(parts, p)
	}
	return parts, nil
}

// fetchNydus fetches a nydus part, mounts it with nydusd and copies it to
// dir with extract; sparse keeps files' holes (writable layers: CRIU's
// images are read with Go's tar, which takes no sparse entries).
func fetchNydus(ctx context.Context, source oras.ReadOnlyTarget, p part, dir string, sparse bool, extract func(context.Context, io.Reader, string) error) error {
	if err := nydus.Available(); err != nil {
		return fmt.Errorf("%w: %w", err, errdefs.ErrFailedPrecondition)
	}
	// Next to dir, on its filesystem: a blob is as large as the part.
	work, err := os.MkdirTemp(filepath.Dir(dir), ".nydus-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)
	bootstrap := filepath.Join(work, "bootstrap")
	if err := fetchFile(ctx, source, p.desc, bootstrap); err != nil {
		return fmt.Errorf("bootstrap: %w", err)
	}
	blobDir := filepath.Join(work, "blobs")
	if err := os.Mkdir(blobDir, 0o700); err != nil {
		return err
	}
	if p.blob != nil {
		if err := fetchFile(ctx, source, *p.blob, filepath.Join(blobDir, p.blob.Digest.Encoded())); err != nil {
			return fmt.Errorf("blob: %w", err)
		}
	}
	m, err := nydus.MountLocal(bootstrap, blobDir, filepath.Join(work, "mnt"), work)
	if err != nil {
		return err
	}
	defer m.Close()
	return copyOut(ctx, m.Dir, dir, sparse, extract)
}

// fetchFile fetches desc to path, failing if it does not match its digest.
func fetchFile(ctx context.Context, source oras.ReadOnlyTarget, desc ocispec.Descriptor, path string) error {
	rc, err := source.Fetch(ctx, desc)
	if err != nil {
		return err
	}
	defer rc.Close()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	vr := content.NewVerifyReader(rc, desc)
	if _, err := io.Copy(f, vr); err != nil {
		return err
	}
	if err := vr.Verify(); err != nil {
		return err
	}
	return f.Close()
}

// copyOut archives the tree at src with tar and hands the archive to
// extract, which writes it to dir: the same extraction as a tar part's.
// With sparse, holes are found by reading, as FUSE does not report them.
func copyOut(ctx context.Context, src, dir string, sparse bool, extract func(context.Context, io.Reader, string) error) error {
	args := []string{"--numeric-owner", "--xattrs", "--xattrs-include=*"}
	if sparse {
		args = append(args, "--sparse", "--hole-detection=raw")
	}
	cmd := exec.CommandContext(ctx, "tar", append(args, "-C", src, "-cf", "-", ".")...)
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	exErr := extract(ctx, out, dir)
	_, _ = io.Copy(io.Discard, out)
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("tar %s: %w: %s", src, err, stderr.String())
	}
	return exErr
}
