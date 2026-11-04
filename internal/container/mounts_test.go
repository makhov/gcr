//go:build linux

package container

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/checkpoint-restore/go-criu/v8/crit"
	"github.com/checkpoint-restore/go-criu/v8/crit/images/mnt"
	"github.com/containerd/errdefs"
	devices "github.com/opencontainers/cgroups/devices/config"
	"github.com/opencontainers/runc/libcontainer/configs"
	"github.com/opencontainers/runc/libcontainer/utils"
	"golang.org/x/sys/unix"
	"google.golang.org/protobuf/proto"
)

// writeMounts writes a mountpoints image with a mount at each of
// external (bind mounts from the spec), plus / and a masked path.
func writeMounts(t *testing.T, dir string, external ...string) {
	t.Helper()
	entry := func(id uint32, mp, key string) *crit.CriuEntry {
		m := &mnt.MntEntry{Fstype: proto.Uint32(0), MntId: proto.Uint32(id), RootDev: proto.Uint32(0),
			ParentMntId: proto.Uint32(1), Flags: proto.Uint32(0), Root: proto.String("/"), Mountpoint: proto.String(mp),
			Source: proto.String("none"), Options: proto.String("")}
		if key != "" {
			m.ExtKey = proto.String(key)
		}
		return &crit.CriuEntry{Message: m}
	}
	img := &crit.CriuImage{Magic: "MNTS", Entries: []*crit.CriuEntry{entry(1, "/", ""), entry(2, "/proc/kcore", "/dev/null")}}
	for i, e := range external {
		img.Entries = append(img.Entries, entry(uint32(10+i), e, e))
	}
	f, err := os.Create(filepath.Join(dir, "mountpoints-13.img"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := crit.New(nil, f, "", false, false).Encode(img); err != nil {
		t.Fatal(err)
	}
}

func TestCheckMounts(t *testing.T) {
	bind := func(dst string) *configs.Mount {
		return &configs.Mount{Destination: dst, Device: "bind", Flags: unix.MS_BIND}
	}
	cfg := func(dsts ...string) *configs.Config {
		c := &configs.Config{Devices: []*devices.Device{{Path: "/dev/null"}}}
		c.Mounts = append(c.Mounts, &configs.Mount{Destination: "/proc", Device: "proc"})
		for _, d := range dsts {
			c.Mounts = append(c.Mounts, bind(d))
		}
		return c
	}
	for _, tc := range []struct {
		name string
		had  []string
		has  *configs.Config
		want string
	}{
		{"the same", []string{"/etc/hosts", "/data"}, cfg("/etc/hosts", "/data"), ""},
		{"a device bound", []string{"/etc/hosts", "/dev/null"}, cfg("/etc/hosts"), ""},
		{"one gone", []string{"/etc/hosts", "/data"}, cfg("/etc/hosts"), "had /data mounted"},
		{"one more", []string{"/etc/hosts"}, cfg("/etc/hosts", "/data"), "mounts /data"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeMounts(t, dir, tc.had...)
			err := checkImageMounts(dir, tc.has)
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("refused: %v", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Fatalf("got %v, want an error about %s", err, tc.want)
			case tc.want != "" && !errors.Is(err, errdefs.ErrFailedPrecondition):
				t.Fatalf("%v is not a failed precondition", err)
			}
		})
	}
}

// A mount lands where the image's own symlinks point: /lib -> usr/lib in
// a Debian image, so libcontainer mounts, and CRIU records,
// /usr/lib/firmware/... for a CDI mount the spec writes as /lib/firmware/...
// Comparing the spec's spelling against the image found every such mount
// missing and the same mount extra, which refused every fork of a GPU
// container that took its devices through CDI.
func TestCheckMountsThroughRootfsSymlinks(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "usr/lib/firmware/nvidia"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("usr/lib", filepath.Join(root, "lib")); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	// What the dump recorded: the resolved names.
	writeMounts(t, dir, "/usr/lib/firmware/nvidia/gsp_ga10x.bin", "/etc/hosts")
	cfg := &configs.Config{
		Rootfs:  root,
		Devices: []*devices.Device{{Path: "/dev/nvidia0"}},
		Mounts: []*configs.Mount{
			{Destination: "/proc", Device: "proc"},
			// What the spec asks for: through the symlink.
			{Destination: "/lib/firmware/nvidia/gsp_ga10x.bin", Device: "bind", Flags: unix.MS_BIND},
			{Destination: "/etc/hosts", Device: "bind", Flags: unix.MS_BIND},
		},
	}
	if err := checkImageMounts(dir, cfg); err != nil {
		t.Fatalf("refused a fork whose mounts differ only by a symlink in the image: %v", err)
	}
}

func checkImageMounts(dir string, cfg *configs.Config) error {
	had, err := imageBinds(dir)
	if err != nil {
		return err
	}
	return checkMounts(had, nil, cfg)
}

// A pod's containers are created by the shim the sandbox started, in the
// sandbox's bundle: each one's hooks have to be told its own.
func TestSetBundle(t *testing.T) {
	cfg := &configs.Config{Labels: []string{"io.kubernetes.cri.container-name=model", "bundle=/run/containerd/task/sandbox"}}
	setBundle(cfg, "/run/containerd/task/model")
	bundle, annotations := utils.Annotations(cfg.Labels)
	if bundle != "/run/containerd/task/model" || annotations["io.kubernetes.cri.container-name"] != "model" {
		t.Fatalf("bundle %q, annotations %v", bundle, annotations)
	}
}
