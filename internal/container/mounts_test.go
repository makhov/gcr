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
			err := checkMounts(dir, tc.has)
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
