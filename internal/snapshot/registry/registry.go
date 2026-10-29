//go:build linux

// Package registry moves snapshots between hosts through OCI registries
// (any registry: one backed by S3, ECR, GHCR, Harbor, Zot...).
//
// A snapshot is one OCI artifact: a config, the CRIU images, and the
// writable layers a fork mounts, its own and those of the snapshots it
// was forked from, top first. Each is a zstd-compressed tar, written
// deterministically, so a layer two snapshots share is one blob in the
// registry. On the pulling host, the layers beneath go to a store, one
// directory per layer shared by every snapshot pulled that has it, and
// the snapshot to the directory asked for, with its LayersFile naming
// the store's copies. Both are registered (snapshot.Record) as if taken
// there, so they are counted and removed the same way.
package registry

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/containerd/errdefs"
	"github.com/klauspost/compress/zstd"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"golang.org/x/sys/unix"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/credentials"
	"oras.land/oras-go/v2/registry/remote/retry"

	"github.com/makhov/gcr/internal/imagetar"
	"github.com/makhov/gcr/internal/snapshot"
	"github.com/makhov/gcr/internal/userns"
)

const (
	ArtifactType    = "application/vnd.gcr.snapshot.v1"
	ConfigMediaType = "application/vnd.gcr.snapshot.config.v1+json"
	ImagesMediaType = "application/vnd.gcr.snapshot.images.v1.tar+zstd"
	UpperMediaType  = "application/vnd.gcr.snapshot.upper.v1.tar+zstd"

	// BlobsFile caches, in a snapshot, the descriptors of its blobs: a
	// snapshot does not change, so they are made once.
	BlobsFile = snapshot.BlobsFile
	// DefaultStore holds the layers beneath pulled snapshots.
	DefaultStore = "/var/lib/gcr/layers"
)

// Config is the artifact's config.
type Config struct {
	// Arch is the GOARCH the snapshot's processes run on.
	Arch string `json:"arch"`
	// UsernsBase and UsernsKey are the user namespace range the
	// snapshot's files and processes carry, if any: forks map it.
	UsernsBase uint32 `json:"usernsBase,omitempty"`
	UsernsKey  string `json:"usernsKey,omitempty"`
	// Layers are the digests of the layers beneath the snapshot's own,
	// top first: the manifest's upper layers after the first.
	Layers  []digest.Digest `json:"layers,omitempty"`
	Created time.Time       `json:"created"`
}

type blobs struct {
	Upper  *ocispec.Descriptor `json:"upper,omitempty"`
	Images *ocispec.Descriptor `json:"images,omitempty"`
}

// Repository opens the repository ref names, with the credentials in
// Docker's config (docker login). Registries on loopback are spoken to
// over plain HTTP, as Docker does; plainHTTP forces it for others.
func Repository(ref string, plainHTTP bool) (*remote.Repository, error) {
	repo, err := remote.NewRepository(ref)
	if err != nil {
		return nil, fmt.Errorf("%s: %v: %w", ref, err, errdefs.ErrInvalidArgument)
	}
	host := repo.Reference.Host()
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	repo.PlainHTTP = plainHTTP || host == "localhost" || net.ParseIP(host).IsLoopback()
	creds, err := credentials.NewStoreFromDocker(credentials.StoreOptions{})
	if err != nil {
		return nil, err
	}
	repo.Client = &auth.Client{
		Client:     retry.DefaultClient,
		Cache:      auth.NewCache(),
		Credential: credentials.Credential(creds),
	}
	return repo, nil
}

// Push uploads the snapshot in dir to target and tags it. It returns the
// manifest's descriptor.
func Push(ctx context.Context, dir string, target oras.Target, tag string) (ocispec.Descriptor, error) {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return ocispec.Descriptor{}, err
	}
	if _, err := os.Stat(filepath.Join(dir, "images")); err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("%s is not a snapshot: %w", dir, err)
	}
	chain, err := readLayers(dir)
	if err != nil {
		return ocispec.Descriptor{}, err
	}
	cfg := Config{Arch: runtime.GOARCH, Created: time.Now().UTC()}
	if b, err := os.ReadFile(filepath.Join(dir, userns.BaseFile)); err == nil {
		if cfg.UsernsBase, err = userns.ParseBase(string(b)); err != nil {
			return ocispec.Descriptor{}, err
		}
		key, err := os.ReadFile(filepath.Join(dir, userns.KeyFile))
		if err != nil {
			return ocispec.Descriptor{}, err
		}
		cfg.UsernsKey = strings.TrimSpace(string(key))
	}

	images, err := pushPart(ctx, target, dir, "images", ImagesMediaType)
	if err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("images: %w", err)
	}
	layers := []ocispec.Descriptor{images}
	ancestors, err := owners(chain)
	if err != nil {
		return ocispec.Descriptor{}, err
	}
	for _, d := range append([]string{dir}, ancestors...) {
		upper, err := pushPart(ctx, target, d, snapshot.Upper, UpperMediaType)
		if err != nil {
			return ocispec.Descriptor{}, fmt.Errorf("layer %s: %w", d, err)
		}
		layers = append(layers, upper)
		if d != dir {
			cfg.Layers = append(cfg.Layers, upper.Digest)
		}
	}

	b, err := json.Marshal(cfg)
	if err != nil {
		return ocispec.Descriptor{}, err
	}
	config := content.NewDescriptorFromBytes(ConfigMediaType, b)
	if err := pushBytes(ctx, target, config, b); err != nil {
		return ocispec.Descriptor{}, err
	}
	manifest, err := oras.PackManifest(ctx, target, oras.PackManifestVersion1_1, ArtifactType, oras.PackManifestOptions{
		ConfigDescriptor:    &config,
		Layers:              layers,
		ManifestAnnotations: map[string]string{ocispec.AnnotationCreated: cfg.Created.Format(time.RFC3339)},
	})
	if err != nil {
		return ocispec.Descriptor{}, err
	}
	if err := target.Tag(ctx, manifest, tag); err != nil {
		return ocispec.Descriptor{}, err
	}
	return manifest, nil
}

// pushPart pushes dir/part as a blob, unless the registry has it, and
// returns its descriptor, made once per snapshot (BlobsFile).
func pushPart(ctx context.Context, target oras.Target, dir, part, mediaType string) (ocispec.Descriptor, error) {
	var cached blobs
	if b, err := os.ReadFile(filepath.Join(dir, BlobsFile)); err == nil {
		_ = json.Unmarshal(b, &cached)
	}
	slot := &cached.Upper
	if part == "images" {
		slot = &cached.Images
	}
	if *slot == nil {
		f, desc, err := pack(filepath.Join(dir, part), mediaType)
		if err != nil {
			return ocispec.Descriptor{}, err
		}
		defer os.Remove(f.Name())
		defer f.Close()
		if exists, err := target.Exists(ctx, desc); err != nil {
			return ocispec.Descriptor{}, err
		} else if !exists {
			if _, err := f.Seek(0, io.SeekStart); err != nil {
				return ocispec.Descriptor{}, err
			}
			if err := target.Push(ctx, desc, f); err != nil {
				return ocispec.Descriptor{}, err
			}
		}
		*slot = &desc
		b, _ := json.Marshal(cached)
		_ = os.WriteFile(filepath.Join(dir, BlobsFile), b, 0o600)
		return desc, nil
	}
	desc := **slot
	if exists, err := target.Exists(ctx, desc); err != nil || exists {
		return desc, err
	}
	// Known here, not there: pack it again (the same bytes).
	f, again, err := pack(filepath.Join(dir, part), mediaType)
	if err != nil {
		return ocispec.Descriptor{}, err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if again.Digest != desc.Digest {
		return ocispec.Descriptor{}, fmt.Errorf("%s/%s changed since it was packed (%s, now %s)", dir, part, desc.Digest, again.Digest)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return ocispec.Descriptor{}, err
	}
	return desc, target.Push(ctx, desc, f)
}

func pushBytes(ctx context.Context, target oras.Target, desc ocispec.Descriptor, b []byte) error {
	if exists, err := target.Exists(ctx, desc); err != nil || exists {
		return err
	}
	return target.Push(ctx, desc, strings.NewReader(string(b)))
}

// pack writes dir as a zstd-compressed tar to a temporary file. The tar
// is deterministic (sorted, no access or change times), so the same
// directory packs to the same blob; it keeps owners as numbers, xattrs
// (overlay's opaque markers among them) and device nodes (its whiteouts).
func pack(dir, mediaType string) (*os.File, ocispec.Descriptor, error) {
	f, err := os.CreateTemp("", "gcr-blob-")
	if err != nil {
		return nil, ocispec.Descriptor{}, err
	}
	fail := func(err error) (*os.File, ocispec.Descriptor, error) {
		f.Close()
		os.Remove(f.Name())
		return nil, ocispec.Descriptor{}, err
	}
	h := sha256.New()
	counted := &counter{w: io.MultiWriter(f, h)}
	// A fixed concurrency: the blob must not depend on the host's CPUs.
	zw, err := zstd.NewWriter(counted, zstd.WithEncoderConcurrency(4))
	if err != nil {
		return fail(err)
	}
	cmd := exec.Command("tar", "--sort=name", "--format=posix",
		"--pax-option=exthdr.name=%d/PaxHeaders/%f,delete=atime,delete=ctime",
		"--numeric-owner", "--xattrs", "--xattrs-include=*", "-C", dir, "-cf", "-", ".")
	cmd.Stdout = zw
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fail(fmt.Errorf("tar %s: %w: %s", dir, err, stderr.String()))
	}
	if err := zw.Close(); err != nil {
		return fail(err)
	}
	return f, ocispec.Descriptor{
		MediaType: mediaType,
		Digest:    digest.NewDigest(digest.SHA256, h),
		Size:      counted.n,
	}, nil
}

type counter struct {
	w io.Writer
	n int64
}

func (c *counter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// Pulled says how a snapshot was pulled.
type Pulled struct {
	// Range is the user namespace range the snapshot maps here (zero
	// without one), and From the one it was taken in, if that differs:
	// it was moved, being taken.
	Range userns.Range
	From  uint32
}

// Pull writes the snapshot ref names in source to dir, which must not
// exist or be empty, and the layers beneath it to store. A fork of dir
// can then be started on this host.
//
// The snapshot keeps its user namespace range if that is free here, and
// is moved into a free one otherwise (userns.ShiftTree, ShiftImages).
// Snapshots taken from the same container, in the same range, share one
// range here too, as forks of one snapshot do, and so their layers.
func Pull(ctx context.Context, source oras.ReadOnlyTarget, ref, dir, store string) (_ Pulled, retErr error) {
	var pulled Pulled
	dir, err := filepath.Abs(dir)
	if err != nil {
		return pulled, err
	}
	if entries, err := os.ReadDir(dir); err == nil && len(entries) > 0 {
		return pulled, fmt.Errorf("%s is not empty: %w", dir, errdefs.ErrAlreadyExists)
	}
	manifestDesc, err := source.Resolve(ctx, ref)
	if err != nil {
		return pulled, err
	}
	var manifest ocispec.Manifest
	if err := fetchJSON(ctx, source, manifestDesc, &manifest); err != nil {
		return pulled, fmt.Errorf("manifest: %w", err)
	}
	if manifest.ArtifactType != ArtifactType || manifest.Config.MediaType != ConfigMediaType {
		return pulled, fmt.Errorf("%s is not a gcr snapshot (artifact type %q): %w", ref, manifest.ArtifactType, errdefs.ErrInvalidArgument)
	}
	// Digests become paths in the store: refuse any that is not one
	// (go-digest does not check on decoding, and "sha256:../.." is not).
	for _, d := range append([]ocispec.Descriptor{manifest.Config}, manifest.Layers...) {
		if err := d.Digest.Validate(); err != nil {
			return pulled, fmt.Errorf("%s: %q: %v: %w", ref, d.Digest, err, errdefs.ErrInvalidArgument)
		}
	}
	var cfg Config
	if err := fetchJSON(ctx, source, manifest.Config, &cfg); err != nil {
		return pulled, fmt.Errorf("config: %w", err)
	}
	for _, d := range cfg.Layers {
		if err := d.Validate(); err != nil {
			return pulled, fmt.Errorf("%s: config: %q: %v: %w", ref, d, err, errdefs.ErrInvalidArgument)
		}
	}
	if cfg.Arch != runtime.GOARCH {
		return pulled, fmt.Errorf("snapshot is for %s, this host is %s: %w", cfg.Arch, runtime.GOARCH, errdefs.ErrFailedPrecondition)
	}
	if len(manifest.Layers) != len(cfg.Layers)+2 || manifest.Layers[0].MediaType != ImagesMediaType {
		return pulled, fmt.Errorf("%s: malformed snapshot manifest: %w", ref, errdefs.ErrInvalidArgument)
	}
	uppers := manifest.Layers[1:]
	for i, d := range uppers {
		if d.MediaType != UpperMediaType || (i > 0 && d.Digest != cfg.Layers[i-1]) {
			return pulled, fmt.Errorf("%s: malformed snapshot manifest: %w", ref, errdefs.ErrInvalidArgument)
		}
	}

	// The range first: the layers are written in it.
	if cfg.UsernsBase != 0 {
		// Keyed by the range the snapshot was taken in, on the host it
		// was taken on, not that host's key, which may name something
		// else here.
		// Also by the lineage's first layer: the publisher chose the
		// rest, and could copy them to share another lineage's range.
		sum := sha256.Sum256([]byte(fmt.Sprintf("%s@%d@%s", cfg.UsernsKey, cfg.UsernsBase, uppers[len(uppers)-1].Digest)))
		key := "pulled_" + hex.EncodeToString(sum[:8])
		r, err := userns.ClaimForSnapshot(dir, key, cfg.UsernsBase)
		if errdefs.IsConflict(err) {
			r, err = userns.ClaimForSnapshot(dir, key, 0)
		}
		if err != nil {
			return pulled, fmt.Errorf("user namespace range: %w", err)
		}
		defer func() {
			if retErr != nil {
				_ = userns.ReleaseForSnapshot(dir, r)
			}
		}()
		pulled.Range = r
		if r.Base != cfg.UsernsBase {
			pulled.From = cfg.UsernsBase
		}
	}

	// The layers beneath, bottom first: each one's own LayersFile names
	// the ones below it.
	store, err = filepath.Abs(store)
	if err != nil {
		return pulled, err
	}
	var below []string // top first
	for i := len(uppers) - 1; i >= 1; i-- {
		layerDir, err := pullLayer(ctx, source, uppers[i], below, store, pulled)
		if err != nil {
			return pulled, fmt.Errorf("layer %s: %w", uppers[i].Digest, err)
		}
		below = append([]string{filepath.Join(layerDir, snapshot.Upper)}, below...)
	}

	tmp, err := os.MkdirTemp(filepath.Dir(dir), "."+filepath.Base(dir)+".gcr-pull-")
	if err != nil {
		return pulled, err
	}
	defer os.RemoveAll(tmp)
	if err := unpackImages(ctx, source, manifest.Layers[0], filepath.Join(tmp, "images")); err != nil {
		return pulled, fmt.Errorf("images: %w", err)
	}
	if err := unpackLayer(ctx, source, uppers[0], filepath.Join(tmp, snapshot.Upper)); err != nil {
		return pulled, fmt.Errorf("writable layer: %w", err)
	}
	b := blobs{Upper: &uppers[0], Images: &manifest.Layers[0]}
	if pulled.From != 0 {
		if err := userns.ShiftTree(filepath.Join(tmp, snapshot.Upper), pulled.From, pulled.Range.Base); err != nil {
			return pulled, fmt.Errorf("move writable layer to range %d: %w", pulled.Range.Base, err)
		}
		if err := userns.ShiftImages(filepath.Join(tmp, "images"), pulled.From, pulled.Range.Base); err != nil {
			return pulled, fmt.Errorf("move images to range %d: %w", pulled.Range.Base, err)
		}
		b = blobs{} // they are not the registry's blobs any more
	}
	if err := writeSnapshotFiles(tmp, below, b); err != nil {
		return pulled, err
	}
	if pulled.Range.Base != 0 {
		if err := os.WriteFile(filepath.Join(tmp, userns.BaseFile), []byte(strconv.Itoa(int(pulled.Range.Base))), 0o600); err != nil {
			return pulled, err
		}
		if err := os.WriteFile(filepath.Join(tmp, userns.KeyFile), []byte(pulled.Range.Key), 0o600); err != nil {
			return pulled, err
		}
	}
	_ = os.Remove(dir) // empty, if there
	if err := os.Rename(tmp, dir); err != nil {
		return pulled, err
	}
	// Counted like a snapshot taken here: it holds the layers beneath,
	// and its user namespace range, which no other container may have.
	if err := snapshot.Record(dir, below); err != nil {
		_ = os.RemoveAll(dir)
		return pulled, err
	}
	return pulled, nil
}

// pullLayer makes sure store has the layer desc (with below beneath it),
// in the snapshot's range here, and returns its directory there.
func pullLayer(ctx context.Context, source oras.ReadOnlyTarget, desc ocispec.Descriptor, below []string, store string, pulled Pulled) (string, error) {
	name := desc.Digest.Algorithm().String() + "-" + desc.Digest.Encoded()
	if pulled.Range.Base != 0 {
		name += "-" + strconv.Itoa(int(pulled.Range.Base))
	}
	layerDir := filepath.Join(store, name)
	if _, err := os.Stat(filepath.Join(layerDir, snapshot.Upper)); err == nil {
		return layerDir, nil // pulled before, with another snapshot
	}
	if err := os.MkdirAll(store, 0o711); err != nil {
		return "", err
	}
	tmp, err := os.MkdirTemp(store, ".pull-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	if err := unpackLayer(ctx, source, desc, filepath.Join(tmp, snapshot.Upper)); err != nil {
		return "", err
	}
	b := blobs{Upper: &desc}
	if pulled.From != 0 {
		if err := userns.ShiftTree(filepath.Join(tmp, snapshot.Upper), pulled.From, pulled.Range.Base); err != nil {
			return "", fmt.Errorf("move to range %d: %w", pulled.Range.Base, err)
		}
		b = blobs{}
	}
	if err := writeSnapshotFiles(tmp, below, b); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, layerDir); err != nil {
		if errors.Is(err, os.ErrExist) || errors.Is(err, unix.ENOTEMPTY) {
			return layerDir, nil // another pull got there first
		}
		return "", err
	}
	return layerDir, snapshot.Record(layerDir, below)
}

func writeSnapshotFiles(dir string, below []string, b blobs) error {
	if len(below) > 0 {
		if err := os.WriteFile(filepath.Join(dir, snapshot.LayersFile), []byte(strings.Join(below, "\n")+"\n"), 0o600); err != nil {
			return err
		}
	}
	j, err := json.Marshal(b)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, BlobsFile), j, 0o600)
}

// unpack fetches desc and extracts it into dir, failing if its content
// does not match its digest.
// untar extracts a writable layer, into a directory of its own (a
// temporary one, empty): keeping owners, xattrs and overlay's whiteouts,
// which only GNU tar does, and safely only once, into an empty directory.
func untar(ctx context.Context, r io.Reader, dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "tar", "--numeric-owner", "--xattrs", "--xattrs-include=*", "-C", dir, "-xpf", "-")
	cmd.Stdin = r
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("untar: %w: %s", err, out)
	}
	return nil
}

// unpackImages extracts CRIU images: regular files only (imagetar).
func unpackImages(ctx context.Context, source oras.ReadOnlyTarget, desc ocispec.Descriptor, dir string) error {
	return unpack(ctx, source, desc, dir, func(_ context.Context, r io.Reader, dir string) error {
		return imagetar.Extract(r, dir)
	})
}

func unpackLayer(ctx context.Context, source oras.ReadOnlyTarget, desc ocispec.Descriptor, dir string) error {
	return unpack(ctx, source, desc, dir, untar)
}

func unpack(ctx context.Context, source oras.ReadOnlyTarget, desc ocispec.Descriptor, dir string, extract func(context.Context, io.Reader, string) error) error {
	rc, err := source.Fetch(ctx, desc)
	if err != nil {
		return err
	}
	defer rc.Close()
	vr := content.NewVerifyReader(rc, desc)
	zr, err := zstd.NewReader(vr)
	if err != nil {
		return err
	}
	defer zr.Close()
	if err := extract(ctx, zr, dir); err != nil {
		return err
	}
	// tar stops at the archive's end; the verifier needs all of it.
	if _, err := io.Copy(io.Discard, vr); err != nil {
		return err
	}
	return vr.Verify()
}

func fetchJSON(ctx context.Context, source oras.ReadOnlyTarget, desc ocispec.Descriptor, v any) error {
	b, err := content.FetchAll(ctx, source, desc)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// readLayers reads dir's LayersFile without checking the layers exist.
func readLayers(dir string) ([]string, error) {
	b, err := os.ReadFile(filepath.Join(dir, snapshot.LayersFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var layers []string
	for _, l := range strings.Split(string(b), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			layers = append(layers, l)
		}
	}
	return layers, nil
}

// owners maps layers to the snapshot directories they are the uppers of.
func owners(layers []string) ([]string, error) {
	dirs := make([]string, len(layers))
	for i, l := range layers {
		d, err := snapshot.Owner(l)
		if err != nil {
			return nil, err
		}
		dirs[i] = d
	}
	return dirs, nil
}
