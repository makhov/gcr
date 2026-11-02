//go:build linux

package container

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/checkpoint-restore/go-criu/v8/crit"
	criucore "github.com/checkpoint-restore/go-criu/v8/crit/images/criu-core"
	"github.com/containerd/errdefs"
	"github.com/moby/sys/capability"
	"github.com/opencontainers/runc/libcontainer/configs"
	"golang.org/x/sys/unix"
)

// taskDead is CRIU's state of a task that has exited (TASK_DEAD).
const taskDead = 2

// checkCreds refuses CRIU images in dir whose threads hold more than the
// container may: CRIU restores each thread's IDs, capabilities and
// seccomp mode as the images say, whatever the container's spec, so
// images from elsewhere (an image made of a snapshot, a fork) could
// otherwise run with privileges no admission check saw. uid is the
// container's process's.
//
// It checks that no capability set holds one the container's bounding
// set lacks; that threads keep no_new_privs where the container sets
// it; and that, where the container runs as a user other than root and
// may not change it (no CAP_SETUID, or CAP_SETGID), no thread is root.
// Seccomp filters are not checked: the images do not say reliably which
// a thread is under (gcr's CRIU hands supervised filters over itself).
func checkCreds(dir string, cfg *configs.Config, uid uint32) error {
	allowed, err := capMask(cfg)
	if err != nil {
		return err
	}
	imgs, err := filepath.Glob(filepath.Join(dir, "core-*.img"))
	if err != nil {
		return err
	}
	for _, path := range imgs {
		in, err := os.OpenFile(path, os.O_RDONLY|unix.O_NOFOLLOW, 0)
		if err != nil {
			return err
		}
		img, err := crit.New(in, nil, "", false, false).Decode(&criucore.CoreEntry{})
		in.Close()
		if err != nil {
			return fmt.Errorf("%s: %w", filepath.Base(path), err)
		}
		for _, e := range img.Entries {
			core, ok := e.Message.(*criucore.CoreEntry)
			if !ok {
				return fmt.Errorf("%s: entry is %T", filepath.Base(path), e.Message)
			}
			if core.GetTc().GetTaskState() == taskDead {
				// A zombie: restored only to be reaped, it runs nothing,
				// and CRIU records neither its seccomp mode nor its flags.
				continue
			}
			if err := checkThread(core.GetThreadCore(), cfg, allowed, uid); err != nil {
				return fmt.Errorf("%s: %w: %w", filepath.Base(path), err, errdefs.ErrPermissionDenied)
			}
		}
	}
	return nil
}

func checkThread(t *criucore.ThreadCoreEntry, cfg *configs.Config, allowed []uint32, uid uint32) error {
	c := t.GetCreds()
	if c == nil {
		return fmt.Errorf("a thread without credentials")
	}
	for name, set := range map[string][]uint32{
		"effective": c.GetCapEff(), "permitted": c.GetCapPrm(), "inheritable": c.GetCapInh(),
		"bounding": c.GetCapBnd(), "ambient": c.GetCapAmb(),
	} {
		for i, w := range set {
			var ok uint32
			if i < len(allowed) {
				ok = allowed[i]
			}
			if extra := w &^ ok; extra != 0 {
				return fmt.Errorf("a thread's %s capabilities hold %s, which the container's bounding set does not",
					name, capNames(i, extra))
			}
		}
	}
	if cfg.NoNewPrivileges && c.GetNoNewPrivs() == 0 {
		return fmt.Errorf("a thread without no_new_privs, which the container sets")
	}
	if uid != 0 && !hasCap(allowed, capability.CAP_SETUID) {
		for _, id := range []uint32{c.GetUid(), c.GetEuid(), c.GetSuid(), c.GetFsuid()} {
			if id == 0 {
				return fmt.Errorf("a thread runs as root, and the container as uid %d, without CAP_SETUID", uid)
			}
		}
	}
	if uid != 0 && !hasCap(allowed, capability.CAP_SETGID) {
		for _, id := range []uint32{c.GetGid(), c.GetEgid(), c.GetSgid(), c.GetFsgid()} {
			if id == 0 {
				return fmt.Errorf("a thread runs in group root, and the container as uid %d, without CAP_SETGID", uid)
			}
		}
	}
	return nil
}

// capMask is the container's bounding set as CRIU's words: bit n of word
// n/32 for capability n.
func capMask(cfg *configs.Config) ([]uint32, error) {
	byName := map[string]capability.Cap{}
	for _, c := range capability.ListKnown() {
		byName["CAP_"+strings.ToUpper(c.String())] = c
	}
	mask := make([]uint32, 2)
	if cfg.Capabilities == nil {
		return mask, nil
	}
	for _, name := range cfg.Capabilities.Bounding {
		c, ok := byName[name]
		if !ok {
			continue // unknown to this kernel's list: no thread can hold it
		}
		for int(c)/32 >= len(mask) {
			mask = append(mask, 0)
		}
		mask[int(c)/32] |= 1 << (uint(c) % 32)
	}
	return mask, nil
}

func hasCap(mask []uint32, c capability.Cap) bool {
	return int(c)/32 < len(mask) && mask[int(c)/32]&(1<<(uint(c)%32)) != 0
}

func capNames(word int, bits uint32) string {
	var names []string
	for b := 0; b < 32; b++ {
		if bits&(1<<b) != 0 {
			names = append(names, "CAP_"+strings.ToUpper(capability.Cap(word*32+b).String()))
		}
	}
	return strings.Join(names, ", ")
}
