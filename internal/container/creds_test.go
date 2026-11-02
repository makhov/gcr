//go:build linux

package container

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/checkpoint-restore/go-criu/v8/crit"
	"github.com/checkpoint-restore/go-criu/v8/crit/images/creds"
	criucore "github.com/checkpoint-restore/go-criu/v8/crit/images/criu-core"
	"github.com/containerd/errdefs"
	"github.com/opencontainers/runc/libcontainer/configs"
	"google.golang.org/protobuf/proto"
)

// writeCore writes a core image of one task in state with creds c and
// seccomp mode.
func writeCore(t *testing.T, dir, name string, state uint32, c *creds.CredsEntry, mode criucore.SeccompMode) {
	t.Helper()
	core := &criucore.CoreEntry{
		Mtype:      criucore.CoreEntry_AARCH64.Enum(),
		Tc:         &criucore.TaskCoreEntry{TaskState: proto.Uint32(state), ExitCode: proto.Uint32(0), Personality: proto.Uint32(0), Flags: proto.Uint32(0), BlkSigset: proto.Uint64(0), Comm: proto.String("t")},
		ThreadCore: &criucore.ThreadCoreEntry{FutexRla: proto.Uint64(0), FutexRlaLen: proto.Uint32(0), Creds: c, SeccompMode: mode.Enum()},
	}
	f, err := os.Create(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	img := &crit.CriuImage{Magic: "CORE", Entries: []*crit.CriuEntry{{Message: core}}}
	if err := crit.New(nil, f, "", false, false).Encode(img); err != nil {
		t.Fatal(err)
	}
}

func credsOf(uid uint32, caps uint32, nnp uint32) *creds.CredsEntry {
	return &creds.CredsEntry{
		Uid: proto.Uint32(uid), Gid: proto.Uint32(uid), Euid: proto.Uint32(uid), Egid: proto.Uint32(uid),
		Suid: proto.Uint32(uid), Sgid: proto.Uint32(uid), Fsuid: proto.Uint32(uid), Fsgid: proto.Uint32(uid),
		CapEff: []uint32{caps, 0}, CapPrm: []uint32{caps, 0}, CapBnd: []uint32{caps, 0},
		Secbits: proto.Uint32(0), NoNewPrivs: proto.Uint32(nnp),
	}
}

func TestCheckCreds(t *testing.T) {
	const chown = 1 << 0 // CAP_CHOWN
	cfg := &configs.Config{
		Capabilities:    &configs.Capabilities{Bounding: []string{"CAP_CHOWN"}},
		NoNewPrivileges: true,
		Seccomp:         &configs.Seccomp{},
	}
	for _, tc := range []struct {
		name  string
		state uint32
		creds *creds.CredsEntry
		mode  criucore.SeccompMode
		uid   uint32
		want  string // in the error; "" for none
	}{
		{"within the spec", 1, credsOf(0, chown, 1), criucore.SeccompMode_filter, 0, ""},
		{"a capability more", 1, credsOf(0, chown|1<<21, 1), criucore.SeccompMode_filter, 0, "CAP_SYS_ADMIN"},
		{"no no_new_privs", 1, credsOf(0, chown, 0), criucore.SeccompMode_filter, 0, "no_new_privs"},
		{"root for a non-root container", 1, credsOf(0, 0, 1), criucore.SeccompMode_filter, 1000, "root"},
		{"a zombie, unchecked", taskDead, credsOf(0, 0, 0), criucore.SeccompMode_disabled, 1000, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeCore(t, dir, "core-1.img", tc.state, tc.creds, tc.mode)
			err := checkCreds(dir, cfg, tc.uid)
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("refused: %v", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Fatalf("got %v, want an error about %s", err, tc.want)
			case tc.want != "" && !errors.Is(err, errdefs.ErrPermissionDenied):
				t.Fatalf("%v is not permission denied", err)
			}
		})
	}
}
