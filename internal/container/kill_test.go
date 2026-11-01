//go:build linux

package container

import (
	"errors"
	"fmt"
	"io/fs"
	"testing"

	"github.com/opencontainers/runc/libcontainer"
	"golang.org/x/sys/unix"
)

// cgroupGone: killing a container whose cgroup has gone, or is going, has
// nothing left to do; libcontainer's errors wrap the cause.
func TestCgroupGone(t *testing.T) {
	path := func(errno error) error {
		return &fs.PathError{Op: "write", Path: "/sys/fs/cgroup/x/cgroup.kill", Err: errno}
	}
	for name, tc := range map[string]struct {
		err  error
		gone bool
	}{
		"no cgroup":     {libcontainer.ErrCgroupNotExist, true},
		"removed":       {fmt.Errorf("unable to kill all processes: %w", path(unix.ENOENT)), true},
		"being removed": {fmt.Errorf("unable to kill all processes: %w", fmt.Errorf("failed to write %q: %w", "1", path(unix.ENODEV))), true},
		"refused":       {fmt.Errorf("unable to kill all processes: %w", path(unix.EPERM)), false},
		"other":         {errors.New("boom"), false},
	} {
		if got := cgroupGone(tc.err); got != tc.gone {
			t.Errorf("%s: %v, want %v", name, got, tc.gone)
		}
	}
}
