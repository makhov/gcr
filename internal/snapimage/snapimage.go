//go:build linux

// Package snapimage turns a snapshot into a container image that runs as
// any other: a pod names it in `image:`, the kubelet pulls it, and gcr
// restores the processes it carries instead of starting its entrypoint.
//
// The image is the container's own image, with two layers on top:
//   - the container's writable layer at the snapshot, as an OCI layer
//     (overlay's whiteouts and opaque directories in OCI's form), so the
//     image's filesystem is the container's as it was;
//   - its state: the snapshot's CRIU images, in /.gcr-snapshot, which
//     marks the image as one to restore (Find).
//
// Its config is the container's image's, labelled LabelSnapshot.
package snapimage

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/containerd/containerd/v2/core/mount"
	"github.com/klauspost/compress/gzip"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"golang.org/x/sys/unix"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content"

	"github.com/makhov/gcr/internal/userns"

	"github.com/makhov/gcr/internal/container"
	"github.com/makhov/gcr/internal/snapshot/registry"
)

const (
	// Dir holds the state in the image's root: the snapshot's CRIU
	// images (Dir/images) and InfoFile.
	Dir = ".gcr-snapshot"
	// InfoFile describes the snapshot (Info).
	InfoFile = "snapshot.json"
	// LabelSnapshot, in the image's config, says it is a snapshot.
	LabelSnapshot = "dev.gcr.snapshot"
	// AnnotationImageName is the image a CRI container was created from,
	// as its pod names it.
	AnnotationImageName = "io.kubernetes.cri.image-name"
)

// Info is InfoFile.
type Info struct {
	// Arch is the GOARCH the snapshot's processes run on.
	Arch    string    `json:"arch"`
	Created time.Time `json:"created"`
	// Base is the image the container ran, by digest.
	Base string `json:"base,omitempty"`
}

// Base is the image a snapshot's container ran: its layers go into the
// snapshot's image as they are.
type Base interface {
	// Name is its reference, by digest if known.
	Name() string
	Manifest() ocispec.Manifest
	Config() []byte
	// Open reads one of its blobs.
	Open(ctx context.Context, desc ocispec.Descriptor) (io.ReadCloser, error)
}

// Push pushes the snapshot in dir, of a container that ran base, to
// target as an image, and tags it. It returns the manifest's descriptor.
func Push(ctx context.Context, dir string, base Base, target oras.Target, tag string) (ocispec.Descriptor, error) {
	var img ocispec.Image
	if err := json.Unmarshal(base.Config(), &img); err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("%s's config: %w", base.Name(), err)
	}
	if img.Architecture != "" && img.Architecture != runtime.GOARCH {
		return ocispec.Descriptor{}, fmt.Errorf("%s is %s, this node %s", base.Name(), img.Architecture, runtime.GOARCH)
	}
	m := base.Manifest()
	if len(m.Layers) != len(img.RootFS.DiffIDs) {
		return ocispec.Descriptor{}, fmt.Errorf("%s: %d layers, %d diff IDs", base.Name(), len(m.Layers), len(img.RootFS.DiffIDs))
	}
	layers := make([]ocispec.Descriptor, 0, len(m.Layers)+2)
	for _, l := range m.Layers {
		l.MediaType = ociMediaType(l.MediaType)
		if err := pushFrom(ctx, target, l, func() (io.ReadCloser, error) { return base.Open(ctx, l) }); err != nil {
			return ocispec.Descriptor{}, fmt.Errorf("%s's layer %s: %w", base.Name(), l.Digest, err)
		}
		layers = append(layers, l)
	}

	// The writable layers, bottom first: those of the snapshots it was
	// forked from, then its own.
	uppers, err := container.ForkLayers(dir)
	if err != nil {
		return ocispec.Descriptor{}, err
	}
	slices.Reverse(uppers)
	now := time.Now().UTC()
	add := func(what string, write func(*tar.Writer) error) error {
		desc, diffID, err := pushLayer(ctx, target, write)
		if err != nil {
			return fmt.Errorf("%s: %w", what, err)
		}
		layers = append(layers, desc)
		img.RootFS.DiffIDs = append(img.RootFS.DiffIDs, diffID)
		img.History = append(img.History, ocispec.History{Created: &now, CreatedBy: "gcr snapshot: " + what})
		return nil
	}
	for _, u := range uppers {
		if err := add("writable layer", func(tw *tar.Writer) error { return writeUpper(tw, u) }); err != nil {
			return ocispec.Descriptor{}, err
		}
	}
	info := Info{Arch: runtime.GOARCH, Created: now, Base: base.Name()}
	if err := add("state", func(tw *tar.Writer) error { return writeState(tw, filepath.Join(dir, "images"), info) }); err != nil {
		return ocispec.Descriptor{}, err
	}

	if img.Config.Labels == nil {
		img.Config.Labels = map[string]string{}
	}
	img.Config.Labels[LabelSnapshot] = "v1"
	img.Created = &now
	b, err := json.Marshal(img)
	if err != nil {
		return ocispec.Descriptor{}, err
	}
	config := content.NewDescriptorFromBytes(ocispec.MediaTypeImageConfig, b)
	if err := pushBytes(ctx, target, config, b); err != nil {
		return ocispec.Descriptor{}, err
	}
	manifest := ocispec.Manifest{
		Versioned:   m.Versioned,
		MediaType:   ocispec.MediaTypeImageManifest,
		Config:      config,
		Layers:      layers,
		Annotations: map[string]string{ocispec.AnnotationCreated: now.Format(time.RFC3339), ocispec.AnnotationBaseImageName: base.Name()},
	}
	manifest.SchemaVersion = 2
	b, err = json.Marshal(manifest)
	if err != nil {
		return ocispec.Descriptor{}, err
	}
	desc := content.NewDescriptorFromBytes(ocispec.MediaTypeImageManifest, b)
	if err := pushBytes(ctx, target, desc, b); err != nil {
		return ocispec.Descriptor{}, err
	}
	if err := target.Tag(ctx, desc, tag); err != nil {
		return ocispec.Descriptor{}, err
	}
	return desc, nil
}

// ociMediaType is Docker's layer media types' OCI name: the same bytes.
func ociMediaType(t string) string {
	switch t {
	case "application/vnd.docker.image.rootfs.diff.tar.gzip":
		return ocispec.MediaTypeImageLayerGzip
	case "application/vnd.docker.image.rootfs.foreign.diff.tar.gzip":
		return ocispec.MediaTypeImageLayerNonDistributableGzip //nolint:staticcheck // what Docker's foreign layers are
	}
	return t
}

func pushFrom(ctx context.Context, target oras.Target, desc ocispec.Descriptor, open func() (io.ReadCloser, error)) error {
	if ok, err := target.Exists(ctx, desc); err == nil && ok {
		return nil
	}
	r, err := open()
	if err != nil {
		return err
	}
	defer r.Close()
	return target.Push(ctx, desc, r)
}

func pushBytes(ctx context.Context, target oras.Target, desc ocispec.Descriptor, b []byte) error {
	return pushFrom(ctx, target, desc, func() (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader(string(b))), nil
	})
}

// pushLayer writes a gzipped tar layer with write, into a temporary file,
// and pushes it. It returns the layer's descriptor and diff ID.
func pushLayer(ctx context.Context, target oras.Target, write func(*tar.Writer) error) (ocispec.Descriptor, digest.Digest, error) {
	if err := os.MkdirAll(registry.WorkDir, 0o700); err != nil {
		return ocispec.Descriptor{}, "", err
	}
	f, err := os.CreateTemp(registry.WorkDir, "gcr-layer-")
	if err != nil {
		return ocispec.Descriptor{}, "", err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	compressed, uncompressed := sha256.New(), sha256.New()
	counted := &counter{w: io.MultiWriter(f, compressed)}
	zw := gzip.NewWriter(counted)
	tw := tar.NewWriter(io.MultiWriter(zw, uncompressed))
	if err := write(tw); err != nil {
		return ocispec.Descriptor{}, "", err
	}
	if err := tw.Close(); err != nil {
		return ocispec.Descriptor{}, "", err
	}
	if err := zw.Close(); err != nil {
		return ocispec.Descriptor{}, "", err
	}
	desc := ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageLayerGzip,
		Digest:    digest.NewDigest(digest.SHA256, compressed),
		Size:      counted.n,
	}
	err = pushFrom(ctx, target, desc, func() (io.ReadCloser, error) {
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return nil, err
		}
		return io.NopCloser(f), nil
	})
	return desc, digest.NewDigest(digest.SHA256, uncompressed), err
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

// overlayXattr is the prefix of overlay's own xattrs, trusted.* as root
// mounts it, user.* with userxattr.
func overlayXattr(name string) bool {
	return strings.HasPrefix(name, "trusted.overlay.") || strings.HasPrefix(name, "user.overlay.")
}

// writeUpper writes overlay's writable layer dir as an OCI layer: a
// whiteout (a 0/0 character device, or a file overlay marks as one) as a
// .wh. file, an opaque directory with a .wh..wh..opq in it, and overlay's
// xattrs left out. It is deterministic: sorted, with no access or change
// times, and owners as numbers.
func writeUpper(tw *tar.Writer, dir string) error {
	links := map[[2]uint64]string{}
	return filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil || rel == "." {
			return err
		}
		var st unix.Stat_t
		if err := unix.Lstat(p, &st); err != nil {
			return err
		}
		xattrs, err := container.Xattrs(p)
		if err != nil {
			return err
		}
		whiteout := st.Mode&unix.S_IFMT == unix.S_IFCHR && st.Rdev == 0 ||
			st.Mode&unix.S_IFMT == unix.S_IFREG && st.Size == 0 &&
				(xattrs["trusted.overlay.whiteout"] != "" || xattrs["user.overlay.whiteout"] != "")
		if whiteout {
			return tw.WriteHeader(&tar.Header{
				Typeflag: tar.TypeReg, Name: filepath.Join(filepath.Dir(rel), ".wh."+filepath.Base(rel)),
				Mode: 0o600, ModTime: modTime(st), Format: tar.FormatPAX,
			})
		}
		h, err := header(p, rel, st)
		if err != nil {
			return err
		}
		for k, v := range xattrs {
			if !overlayXattr(k) {
				if h.PAXRecords == nil {
					h.PAXRecords = map[string]string{}
				}
				h.PAXRecords["SCHILY.xattr."+k] = v
			}
		}
		if h.Typeflag == tar.TypeReg && st.Nlink > 1 {
			key := [2]uint64{uint64(st.Dev), st.Ino}
			if first, ok := links[key]; ok {
				h.Typeflag, h.Linkname, h.Size = tar.TypeLink, first, 0
			} else {
				links[key] = h.Name
			}
		}
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
		if h.Typeflag == tar.TypeReg && h.Size > 0 {
			if err := copyFile(tw, p); err != nil {
				return err
			}
		}
		if d.IsDir() && (xattrs["trusted.overlay.opaque"] == "y" || xattrs["user.overlay.opaque"] == "y") {
			return tw.WriteHeader(&tar.Header{
				Typeflag: tar.TypeReg, Name: filepath.Join(rel, ".wh..wh..opq"),
				Mode: 0o600, ModTime: modTime(st), Format: tar.FormatPAX,
			})
		}
		return nil
	})
}

// writeState writes the state layer: images under Dir/images, and Info,
// in a Dir that hides any a lower layer has (an image of a container
// restored from one).
func writeState(tw *tar.Writer, images string, info Info) error {
	b, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return err
	}
	t := info.Created
	for _, h := range []*tar.Header{
		{Typeflag: tar.TypeDir, Name: Dir + "/", Mode: 0o700},
		{Typeflag: tar.TypeReg, Name: Dir + "/.wh..wh..opq", Mode: 0o600},
		{Typeflag: tar.TypeReg, Name: Dir + "/" + InfoFile, Mode: 0o600, Size: int64(len(b))},
	} {
		h.ModTime, h.Format = t, tar.FormatPAX
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
	}
	if _, err := tw.Write(b); err != nil {
		return err
	}
	return filepath.WalkDir(images, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(images, p)
		if err != nil {
			return err
		}
		var st unix.Stat_t
		if err := unix.Lstat(p, &st); err != nil {
			return err
		}
		h, err := header(p, filepath.Join(Dir, "images", rel), st)
		if err != nil {
			return err
		}
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
		if h.Typeflag == tar.TypeReg && h.Size > 0 {
			return copyFile(tw, p)
		}
		return nil
	})
}

func modTime(st unix.Stat_t) time.Time {
	return time.Unix(st.Mtim.Unix()).UTC()
}

// header is the tar header of p, named name.
func header(p, name string, st unix.Stat_t) (*tar.Header, error) {
	h := &tar.Header{
		Name: name, Mode: int64(st.Mode & 0o7777), Uid: int(st.Uid), Gid: int(st.Gid),
		ModTime: modTime(st), Format: tar.FormatPAX,
	}
	switch st.Mode & unix.S_IFMT {
	case unix.S_IFREG:
		h.Typeflag, h.Size = tar.TypeReg, st.Size
	case unix.S_IFDIR:
		h.Typeflag, h.Name = tar.TypeDir, name+"/"
	case unix.S_IFLNK:
		h.Typeflag = tar.TypeSymlink
		target, err := os.Readlink(p)
		if err != nil {
			return nil, err
		}
		h.Linkname = target
	case unix.S_IFCHR, unix.S_IFBLK:
		h.Typeflag = tar.TypeChar
		if st.Mode&unix.S_IFMT == unix.S_IFBLK {
			h.Typeflag = tar.TypeBlock
		}
		h.Devmajor, h.Devminor = int64(unix.Major(st.Rdev)), int64(unix.Minor(st.Rdev))
	case unix.S_IFIFO:
		h.Typeflag = tar.TypeFifo
	default:
		return nil, fmt.Errorf("%s: a socket or an unknown type, which an image cannot hold", p)
	}
	return h, nil
}

func copyFile(w io.Writer, p string) error {
	f, err := os.Open(p)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(w, f)
	return err
}

// Find returns the state directory (Dir) of the snapshot the rootfs
// mounts carry, in the topmost layer that has one: "" for an image that
// is not a snapshot.
func Find(rootfs []mount.Mount) string {
	var layers []string
	for _, m := range rootfs {
		switch m.Type {
		case "overlay":
			for _, o := range m.Options {
				if v, ok := strings.CutPrefix(o, "lowerdir="); ok {
					layers = append(layers, strings.Split(v, ":")...)
				}
			}
		case "bind", "rbind":
			layers = append(layers, m.Source)
		}
	}
	for _, l := range layers {
		d := filepath.Join(l, Dir)
		if fi, err := os.Lstat(d); err == nil {
			if fi.IsDir() && contained(d) {
				return d
			}
			// Hidden by a layer above, not one of gcr's, or linking out.
			return ""
		}
	}
	return ""
}

// contained says whether the snapshot in d keeps to d: its info and key
// regular files, its images a directory without links out of it. CRIU
// reads them as root on the
// node: through a link, an image could restore another pod's checkpoint,
// or take its user namespace. Its writable layer's links are the
// container's own, followed in it.
func contained(d string) bool {
	for _, f := range []string{InfoFile, userns.KeyFile} {
		fi, err := os.Lstat(filepath.Join(d, f))
		if f == userns.KeyFile && errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil || !fi.Mode().IsRegular() {
			return false
		}
	}
	images := filepath.Join(d, "images")
	if fi, err := os.Lstat(images); err != nil || !fi.IsDir() {
		return false
	}
	return filepath.WalkDir(images, func(p string, e fs.DirEntry, err error) error {
		if err != nil || e.Type()&fs.ModeSymlink == 0 {
			return err
		}
		to, err := os.Readlink(p)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(images, filepath.Join(filepath.Dir(p), to))
		if filepath.IsAbs(to) || err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
			return fs.ErrPermission
		}
		return nil
	}) == nil
}

// ReadInfo reads the InfoFile in a state directory.
func ReadInfo(dir string) (Info, error) {
	var info Info
	b, err := os.ReadFile(filepath.Join(dir, InfoFile))
	if err != nil {
		return info, err
	}
	return info, json.Unmarshal(b, &info)
}

// Allowed says whether image (a reference, as a pod names it) matches one
// of patterns: "*" matches any, a pattern ending in "*" any reference it
// is a prefix of, and another the reference, or its repository (without
// repository is ref without its digest or tag.
func repository(ref string) string {
	if i := strings.Index(ref, "@"); i >= 0 {
		ref = ref[:i]
	}
	if i := strings.LastIndex(ref, ":"); i > strings.LastIndex(ref, "/") {
		ref = ref[:i]
	}
	return ref
}

// tag or digest).
func Allowed(image string, patterns []string) bool {
	repo := repository(image)
	for _, p := range patterns {
		switch {
		case p == "*":
			return true
		case strings.HasSuffix(p, "*"):
			if image != "" && strings.HasPrefix(image, strings.TrimSuffix(p, "*")) {
				return true
			}
		case p == image || p == repo:
			if image != "" {
				return true
			}
		}
	}
	return false
}
