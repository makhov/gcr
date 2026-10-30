//go:build linux

// Package nydus builds directories into nydus images (RAFS v6, EROFS
// compatible) and mounts them with nydusd. It runs nydus's own tools,
// nydus-image and nydusd (github.com/dragonflyoss/nydus).
package nydus

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// Available says whether nydus's tools are installed.
func Available() error {
	for _, tool := range []string{"nydus-image", "nydusd"} {
		if _, err := exec.LookPath(tool); err != nil {
			return fmt.Errorf("the nydus format needs %s (github.com/dragonflyoss/nydus) in PATH", tool)
		}
	}
	return nil
}

// Image is a built nydus image: its bootstrap (the filesystem's metadata)
// and its blob (the files' data, chunked and compressed), named by its
// sha256, which the bootstrap refers to it by. A directory without file
// data has no blob.
type Image struct {
	Bootstrap string
	Blob      string // "" without one
}

// Build makes a nydus image of src under dir, which it creates. Files
// keep their owners, modes, xattrs and device numbers as they are:
// overlay's whiteouts and opaque markers are not interpreted.
func Build(src, dir string) (Image, error) {
	blobs := filepath.Join(dir, "blobs")
	if err := os.MkdirAll(blobs, 0o700); err != nil {
		return Image{}, err
	}
	img := Image{Bootstrap: filepath.Join(dir, "bootstrap")}
	cmd := exec.Command("nydus-image", "create", "--fs-version", "6", "--compressor", "zstd",
		"--whiteout-spec", "none", "--blob-dir", blobs, "--bootstrap", img.Bootstrap,
		"--log-level", "error", src)
	if out, err := cmd.CombinedOutput(); err != nil {
		return Image{}, fmt.Errorf("nydus-image create %s: %w: %s", src, err, lastLines(string(out), 5))
	}
	entries, err := os.ReadDir(blobs)
	if err != nil {
		return Image{}, err
	}
	switch len(entries) {
	case 0:
	case 1:
		img.Blob = filepath.Join(blobs, entries[0].Name())
	default:
		return Image{}, fmt.Errorf("nydus-image create %s: %d blobs, want one", src, len(entries))
	}
	return img, nil
}

// Mount is nydusd serving an image at a mountpoint.
type Mount struct {
	Dir  string
	cmd  *exec.Cmd
	done chan error
}

// MountLocal mounts the image bootstrap describes at dir, with nydusd over
// FUSE, reading its blob from blobDir (a file named by its sha256) and
// keeping its cache in work.
func MountLocal(bootstrap, blobDir, dir, work string) (*Mount, error) {
	backend := map[string]any{"type": "localfs", "config": map[string]any{"dir": blobDir}}
	return mount(bootstrap, backend, dir, work)
}

func mount(bootstrap string, backend map[string]any, dir, work string) (*Mount, error) {
	cache := filepath.Join(work, "cache")
	if err := os.MkdirAll(cache, 0o700); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	cfg, err := json.Marshal(map[string]any{
		"device": map[string]any{
			"backend": backend,
			// RAFS v6 needs a local blob cache.
			"cache": map[string]any{"type": "blobcache", "config": map[string]any{"work_dir": cache}},
		},
		"mode": "direct",
		// nydusd cannot validate RAFS v6 chunks; blobs are checked
		// against their digests when they are fetched whole.
		"digest_validate": false,
		"enable_xattr":    true,
		"iostats_files":   false,
	})
	if err != nil {
		return nil, err
	}
	cfgPath := filepath.Join(work, "nydusd.json")
	if err := os.WriteFile(cfgPath, cfg, 0o600); err != nil {
		return nil, err
	}
	log := filepath.Join(work, "nydusd.log")
	cmd := exec.Command("nydusd", "--config", cfgPath, "--bootstrap", bootstrap,
		"--mountpoint", dir, "--log-level", "warn", "--log-file", log)
	// A session of its own: a daemon left running is not in the caller's
	// process group, nor killed with its terminal.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("nydusd: %w", err)
	}
	m := &Mount{Dir: dir, cmd: cmd, done: make(chan error, 1)}
	go func() { m.done <- cmd.Wait() }()
	for deadline := time.Now().Add(10 * time.Second); ; {
		if Mounted(dir) {
			return m, nil
		}
		select {
		case err := <-m.done:
			b, _ := os.ReadFile(log)
			return nil, fmt.Errorf("nydusd exited (%v): %s", err, lastLines(string(b), 5))
		case <-time.After(20 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			<-m.done
			return nil, fmt.Errorf("nydusd did not mount %s", dir)
		}
	}
}

// MountRegistry mounts the image bootstrap describes at dir, with a nydusd
// that reads its blob from a registry, a chunk at a time as files are read,
// and keeps what it read in work. nydusd runs on its own, and exits when
// dir is unmounted (Unmount).
func MountRegistry(bootstrap string, reg Registry, dir, work string) error {
	cfg := map[string]any{
		"scheme": reg.Scheme, "host": reg.Host, "repo": reg.Repo,
		"timeout": 30, "connect_timeout": 10, "retry_limit": 3,
	}
	if reg.Username != "" {
		cfg["auth"] = base64.StdEncoding.EncodeToString([]byte(reg.Username + ":" + reg.Password))
	}
	m, err := mount(bootstrap, map[string]any{"type": "registry", "config": cfg}, dir, work)
	if err != nil {
		return err
	}
	// Left running: it serves the mount after this process exits.
	return m.cmd.Process.Release()
}

// Registry is where MountRegistry's nydusd reads blobs from.
type Registry struct {
	Scheme, Host, Repo string
	Username, Password string
}

// Unmount unmounts dir if something is mounted there; a nydusd serving it
// exits.
func Unmount(dir string) error {
	if !Mounted(dir) {
		return nil
	}
	if err := unix.Unmount(dir, 0); err != nil {
		return unix.Unmount(dir, unix.MNT_DETACH)
	}
	return nil
}

// Close unmounts the image; nydusd exits with its mount.
func (m *Mount) Close() error {
	if err := unix.Unmount(m.Dir, 0); err != nil && !errors.Is(err, unix.EINVAL) {
		_ = unix.Unmount(m.Dir, unix.MNT_DETACH)
	}
	select {
	case <-m.done:
	case <-time.After(5 * time.Second):
		_ = m.cmd.Process.Kill()
		<-m.done
	}
	return nil
}

// Mounted says whether dir is a FUSE mountpoint.
func Mounted(dir string) bool {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return false
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		// id parent major:minor root mountpoint opts ... - fstype ...
		fields := strings.Fields(s.Text())
		if len(fields) < 5 || fields[4] != dir {
			continue
		}
		for i, f := range fields {
			if f == "-" && i+1 < len(fields) && strings.HasPrefix(fields[i+1], "fuse") {
				return true
			}
		}
	}
	return false
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.Join(lines[max(0, len(lines)-n):], "\n")
}
