//go:build linux

// Package userns runs containers in a user namespace by default: root in the
// container is an unprivileged range of host IDs, one range per pod.
package userns

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/containerd/errdefs"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"golang.org/x/sys/unix"

	"github.com/makhov/gcr/internal/ocispec"
)

const (
	// AnnotationHostUsers=true opts a container out (it runs as host root);
	// =false opts a Kubernetes pod in.
	AnnotationHostUsers = "gcr.dev/host-users"
	// AnnotationBase asks for a specific range, by its first host ID. A
	// migration target needs the source's range: CRIU restores processes
	// into the mapping they were dumped with.
	AnnotationBase = "gcr.dev/userns-base"

	// Size of each range; the container sees IDs 0..Size-1.
	Size = 65536
	// Ranges start at 2^30 and fill up to 2^31: 16384 pods per host.
	firstBase = 1 << 30
	slots     = 1 << 14
)

// stateDir holds the allocation files; a variable for tests.
var stateDir = "/run/gcr/userns"

// Range is a host ID range a container's IDs 0..Size-1 map to.
type Range struct {
	Base uint32
	// Key is what shares the range: the pod sandbox, or the container.
	Key string
}

func (r Range) mapping() []specs.LinuxIDMapping {
	return []specs.LinuxIDMapping{{ContainerID: 0, HostID: r.Base, Size: Size}}
}

// IDMapOption is the containerd mount option form, e.g. "0:1073741824:65536".
func (r Range) IDMapOption() string {
	return fmt.Sprintf("0:%d:%d", r.Base, Size)
}

// Mode is the runtime handler's user namespace policy.
type Mode string

const (
	// Auto: on by default; Kubernetes pods follow hostUsers unless they
	// opt in with gcr.dev/host-users=false; any container can opt out
	// with gcr.dev/host-users=true.
	Auto Mode = "auto"
	// Always: every container, whatever Kubernetes or annotations say. A
	// container that cannot run in one is refused, not run as host root.
	Always Mode = "always"
	// Never: gcr adds none (Kubernetes hostUsers: false still works).
	Never Mode = "never"
)

func (m Mode) Validate() error {
	switch m {
	case Auto, Always, Never:
		return nil
	}
	return fmt.Errorf("UserNamespaces = %q, want auto, always or never: %w", m, errdefs.ErrInvalidArgument)
}

// Decide reports whether gcr should add a user namespace to spec under
// mode, and why not if it should not. It fails when mode is Always and the
// container cannot have one.
func Decide(spec *specs.Spec, mode Mode) (bool, string, error) {
	if mode == Never {
		return false, "UserNamespaces = never", nil
	}
	if spec.Linux == nil {
		return false, "no linux config", nil
	}
	for _, ns := range spec.Linux.Namespaces {
		if ns.Type == specs.UserNamespace {
			// Kubernetes hostUsers: false, or a caller's own mapping.
			return false, "spec already has a user namespace", nil
		}
	}
	if why := blocker(spec); why != "" {
		if mode == Always {
			return false, why, fmt.Errorf("UserNamespaces = always, but the container cannot run in a user namespace: %s: %w", why, errdefs.ErrFailedPrecondition)
		}
		return false, why, nil
	}
	if mode == Always {
		return true, "", nil
	}
	if spec.Annotations[AnnotationHostUsers] == "true" {
		return false, AnnotationHostUsers + "=true", nil
	}
	// Kubernetes decides for pods: hostUsers defaults to true and kubelet
	// sends it like an explicit true, so gcr does not override it unless
	// configured to (Always). Pods get a user namespace from hostUsers:
	// false (containerd's own), or from gcr with gcr.dev/host-users=false.
	if ocispec.PodContainer(spec) && spec.Annotations[AnnotationHostUsers] != "false" {
		return false, "Kubernetes pod without hostUsers: false or " + AnnotationHostUsers + "=false", nil
	}
	return true, "", nil
}

// blocker says why spec cannot run in a gcr user namespace, or "".
func blocker(spec *specs.Spec) string {
	if ocispec.Privileged(spec) {
		return "privileged container"
	}
	// CAP_NET_ADMIN means nothing in a pod's network namespace from inside a
	// user namespace (CRI creates it outside).
	if p := spec.Process; p != nil && p.Capabilities != nil && slices.Contains(p.Capabilities.Bounding, "CAP_NET_ADMIN") {
		for _, ns := range spec.Linux.Namespaces {
			if ns.Type == specs.NetworkNamespace && ns.Path != "" {
				return "needs CAP_NET_ADMIN in a pod network namespace"
			}
		}
	}
	// As in Kubernetes, no user namespace with the host's network, PID or
	// IPC namespace: OpenNetwork would otherwise change the host's sysctls.
	for _, t := range []specs.LinuxNamespaceType{specs.NetworkNamespace, specs.PIDNamespace, specs.IPCNamespace} {
		if hostNamespace(spec, t) {
			return "shares the host's " + string(t) + " namespace"
		}
	}
	return ""
}

// hostNamespace reports whether spec uses the host's namespace of type t:
// no entry for it, or a path to the same namespace as init's.
func hostNamespace(spec *specs.Spec, t specs.LinuxNamespaceType) bool {
	for _, ns := range spec.Linux.Namespaces {
		if ns.Type != t {
			continue
		}
		if ns.Path == "" {
			return false
		}
		var a, b unix.Stat_t
		if unix.Stat(ns.Path, &a) != nil || unix.Stat(fmt.Sprintf("/proc/1/ns/%s", nsProcName(t)), &b) != nil {
			return true // cannot tell: be safe
		}
		return a.Dev == b.Dev && a.Ino == b.Ino
	}
	return true
}

func nsProcName(t specs.LinuxNamespaceType) string {
	if t == specs.NetworkNamespace {
		return "net"
	}
	return string(t)
}

// Allocate returns the range for container id, shared by its pod. want, if
// not 0, is the exact base the range must have. Shims are separate
// processes, so allocation goes through files under a lock.
func Allocate(spec *specs.Spec, namespace, id string, want uint32) (Range, error) {
	key := namespace + "_" + id
	// A pod's containers share one range, so they can share volumes.
	if sb := ocispec.SandboxID(spec); sb != "" {
		key = namespace + "_pod_" + sb
	}
	var r Range
	err := locked(func() error {
		users := filepath.Join(stateDir, "keys", key)
		if b, err := os.ReadFile(filepath.Join(users, "base")); err == nil {
			base, err := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 32)
			if err != nil {
				return err
			}
			r = Range{Base: uint32(base), Key: key}
			if want != 0 && r.Base != want {
				return fmt.Errorf("pod already uses range %d, not %d: %w", r.Base, want, errdefs.ErrConflict)
			}
		} else {
			base, err := claim(key, want)
			if err != nil {
				return err
			}
			r = Range{Base: base, Key: key}
		}
		return os.WriteFile(filepath.Join(users, "user-"+id), nil, 0o600)
	})
	return r, err
}

// validKey refuses a key that is not one directory name under keys/.
// Keys name the files gcr writes and removes as root, and one may come
// from a snapshot's KeyFile.
func validKey(key string) error {
	ok := key != "" && len(key) <= 255 && key[0] != '.'
	for _, r := range key {
		ok = ok && (r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("._-:@+", r))
	}
	if !ok {
		return fmt.Errorf("user namespace range key %q: %w", key, errdefs.ErrInvalidArgument)
	}
	return nil
}

// claim gives key a range of its own: want, or with want 0 the first
// free one. Callers hold the lock.
func claim(key string, want uint32) (uint32, error) {
	if err := validKey(key); err != nil {
		return 0, err
	}
	pruneSnapshots()
	var got uint32
	for slot := 0; slot < slots; slot++ {
		base := uint32(firstBase + slot*Size)
		if want != 0 && base != want {
			continue
		}
		f, err := os.OpenFile(filepath.Join(stateDir, "bases", strconv.Itoa(int(base))), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return 0, err
		}
		_, _ = f.WriteString(key)
		f.Close()
		got = base
		break
	}
	if got == 0 && want != 0 {
		return 0, fmt.Errorf("user namespace range %d is in use or invalid: %w", want, errdefs.ErrConflict)
	}
	if got == 0 {
		return 0, fmt.Errorf("no free user namespace range: %w", errdefs.ErrResourceExhausted)
	}
	users := filepath.Join(stateDir, "keys", key)
	if err := os.MkdirAll(users, 0o700); err != nil {
		return 0, err
	}
	return got, os.WriteFile(filepath.Join(users, "base"), []byte(strconv.Itoa(int(got))), 0o600)
}

// Release drops container id's use of r; the last user frees it.
// ParseBase reads a range base from an annotation or a checkpoint file.
func ParseBase(s string) (uint32, error) {
	n, err := strconv.ParseUint(strings.TrimSpace(s), 10, 32)
	if err != nil || n < firstBase || n >= firstBase+slots*Size || (n-firstBase)%Size != 0 {
		return 0, fmt.Errorf("bad user namespace range base %q: %w", s, errdefs.ErrInvalidArgument)
	}
	return uint32(n), nil
}

// BaseFile is written into checkpoints of containers with a user namespace.
const BaseFile = "gcr-userns-base"

// KeyFile is where a snapshot records the allocator key of the range its
// container had, so a fork can join the same range: the snapshot's
// writable layer holds host IDs from it.
const KeyFile = "gcr-userns-key"

// Join adds id as a user of the range key holds, for a container that
// must map the same host IDs as the one a snapshot came from. The range
// lives until its last user is released. If key holds none (its users
// are gone, or the host rebooted), Join claims base for it again, unless
// something else has it since.
func Join(key, id string, base uint32) (Range, error) {
	return join(key, "user-"+id, base, "", false)
}

// HoldForSnapshot makes the snapshot in dir a user of r, so the range
// stays its container's while forks may map it. A snapshot that is
// deleted without ReleaseForSnapshot stops holding it when a range is
// next allocated.
func HoldForSnapshot(dir string, r Range) error {
	_, err := join(r.Key, snapshotUser(dir), r.Base, dir, false)
	return err
}

// ClaimForSnapshot makes the snapshot in dir a user of key's range. If
// key has none, it gets want, or with want 0 the first free range: a
// pulled snapshot keeps its range where it can, and is shifted into
// another (ShiftTree, ShiftImages) where it cannot.
func ClaimForSnapshot(dir, key string, want uint32) (Range, error) {
	return join(key, snapshotUser(dir), want, dir, want == 0)
}

// ReleaseForSnapshot drops the hold HoldForSnapshot took.
func ReleaseForSnapshot(dir string, r Range) error {
	return locked(func() error { return release(r, snapshotUser(dir)) })
}

func snapshotUser(dir string) string {
	sum := sha256.Sum256([]byte(dir))
	return "user-snapshot-" + hex.EncodeToString(sum[:8])
}

func join(key, user string, base uint32, holder string, anyFree bool) (Range, error) {
	if err := validKey(key); err != nil {
		return Range{}, err
	}
	var r Range
	err := locked(func() error {
		users := filepath.Join(stateDir, "keys", key)
		b, err := os.ReadFile(filepath.Join(users, "base"))
		switch {
		case err == nil:
			if base, err = ParseBase(string(b)); err != nil {
				return err
			}
		case errors.Is(err, os.ErrNotExist) && (base != 0 || anyFree):
			got, err := claim(key, base)
			if errdefs.IsConflict(err) {
				return fmt.Errorf("user namespace range %d of %q has been given to another container: %w", base, key, errdefs.ErrConflict)
			}
			if err != nil {
				return err
			}
			base = got
		default:
			return fmt.Errorf("user namespace range of %q: %w", key, err)
		}
		r = Range{Base: base, Key: key}
		return os.WriteFile(filepath.Join(users, user), []byte(holder), 0o600)
	})
	return r, err
}

func Release(r Range, id string) error {
	return locked(func() error { return release(r, "user-"+id) })
}

func release(r Range, user string) error {
	if err := validKey(r.Key); err != nil {
		return err
	}
	users := filepath.Join(stateDir, "keys", r.Key)
	_ = os.Remove(filepath.Join(users, user))
	entries, err := os.ReadDir(users)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "user-") {
			return nil
		}
	}
	_ = os.Remove(filepath.Join(stateDir, "bases", strconv.Itoa(int(r.Base))))
	return os.RemoveAll(users)
}

// pullGrace is how long a snapshot's hold stays without the snapshot.
var pullGrace = 10 * time.Minute

// pruneSnapshots drops the holds of snapshots that are gone: their
// directory no longer records the range's key.
func pruneSnapshots() {
	keys, _ := os.ReadDir(filepath.Join(stateDir, "keys"))
	for _, k := range keys {
		users := filepath.Join(stateDir, "keys", k.Name())
		holds, _ := filepath.Glob(filepath.Join(users, "user-snapshot-*"))
		for _, h := range holds {
			// A pull claims the range before its snapshot is in place.
			if st, err := os.Stat(h); err != nil || time.Since(st.ModTime()) < pullGrace {
				continue
			}
			dir, err := os.ReadFile(h)
			if err != nil {
				continue
			}
			if key, err := os.ReadFile(filepath.Join(string(dir), KeyFile)); err == nil && strings.TrimSpace(string(key)) == k.Name() {
				continue
			} else if err != nil && !errors.Is(err, os.ErrNotExist) {
				continue // not knowably gone (a storage error)
			}
			b, err := os.ReadFile(filepath.Join(users, "base"))
			if err != nil {
				continue
			}
			base, err := ParseBase(string(b))
			if err != nil {
				continue
			}
			_ = release(Range{Base: base, Key: k.Name()}, filepath.Base(h))
		}
	}
}

func locked(fn func() error) error {
	if err := os.MkdirAll(filepath.Join(stateDir, "bases"), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(stateDir, "lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		return err
	}
	defer unix.Flock(int(f.Fd()), unix.LOCK_UN)
	return fn()
}

// Apply puts spec's process in a new user namespace mapped to r, and
// idmaps the bind mounts whose filesystems support it, so volumes keep
// their on-disk owners (host root stays container root).
func Apply(spec *specs.Spec, r Range) {
	// A pod's containers share one user namespace, the sandbox's, so they
	// can share its PID and IPC namespaces; the sandbox creates it.
	userNS := specs.LinuxNamespace{Type: specs.UserNamespace}
	if pid := podProcess(spec, r); pid != 0 {
		userNS.Path = fmt.Sprintf("/proc/%d/ns/user", pid)
	}
	spec.Linux.Namespaces = append(spec.Linux.Namespaces, userNS)
	spec.Linux.UIDMappings = r.mapping()
	spec.Linux.GIDMappings = r.mapping()
	joinsNetwork := false
	for _, ns := range spec.Linux.Namespaces {
		if ns.Type == specs.NetworkNamespace && ns.Path != "" {
			joinsNetwork = true
		}
	}
	for i, m := range spec.Mounts {
		// sysfs can only be mounted from a network namespace owned by the
		// container's user namespace; a pod's is not. Bind it read-only
		// instead, as runc does for rootless containers.
		if m.Type == "sysfs" && joinsNetwork {
			spec.Mounts[i] = specs.Mount{
				Destination: m.Destination, Type: "bind", Source: "/sys",
				Options: []string{"rbind", "nosuid", "noexec", "nodev", "ro"},
			}
			continue
		}
		if !isBind(m) || !idmappable(m.Source) {
			continue
		}
		spec.Mounts[i].Options = append(slices.Clone(m.Options), "idmap")
	}
}

// DelegateCgroup makes the container's cgroup mount writable, for a
// container in a user namespace that manages nested containers
// (CAP_SYS_ADMIN there), and returns the host UID of its root, which the
// runtime gives the container's cgroup to; its cgroup namespace keeps it
// inside that subtree. 0: no delegation.
func DelegateCgroup(spec *specs.Spec) int {
	if spec.Linux == nil || !ocispec.HasCapability(spec, "CAP_SYS_ADMIN") {
		return 0
	}
	root := -1
	for _, m := range spec.Linux.UIDMappings {
		if m.ContainerID == 0 && m.Size > 0 {
			root = int(m.HostID)
		}
	}
	if root <= 0 {
		return 0
	}
	for i, m := range spec.Mounts {
		if m.Destination == "/sys/fs/cgroup" {
			spec.Mounts[i].Options = slices.DeleteFunc(slices.Clone(m.Options), func(o string) bool { return o == "ro" })
		}
	}
	return root
}

// podProcess finds the pod sandbox's process from the /proc/<pid>/ns paths
// CRI gives a pod's containers, if it is in a user namespace mapped to r.
func podProcess(spec *specs.Spec, r Range) int {
	for _, ns := range spec.Linux.Namespaces {
		rest, ok := strings.CutPrefix(ns.Path, "/proc/")
		if !ok {
			continue
		}
		pidStr, _, ok := strings.Cut(rest, "/")
		if !ok {
			continue
		}
		pid, err := strconv.Atoi(pidStr)
		if err != nil {
			continue
		}
		b, err := os.ReadFile(fmt.Sprintf("/proc/%d/uid_map", pid))
		if err != nil {
			continue
		}
		if f := strings.Fields(string(b)); len(f) == 3 && f[0] == "0" && f[1] == strconv.Itoa(int(r.Base)) {
			return pid
		}
	}
	return 0
}

func isBind(m specs.Mount) bool {
	if m.Type == "bind" {
		return true
	}
	return slices.Contains(m.Options, "bind") || slices.Contains(m.Options, "rbind")
}

// idmappable reports whether path's filesystem supports idmapped mounts.
func idmappable(path string) bool {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return false
	}
	switch st.Type {
	case unix.EXT4_SUPER_MAGIC, unix.XFS_SUPER_MAGIC, unix.BTRFS_SUPER_MAGIC,
		unix.TMPFS_MAGIC, unix.F2FS_SUPER_MAGIC, unix.MSDOS_SUPER_MAGIC:
		return true
	}
	return false
}

// OpenNetwork makes a network namespace created outside the container's user
// namespace (a pod's) usable by its root: ports below 1024 and ping. The
// container cannot write that namespace's sysctls, so the spec's net.*
// sysctls move here too. Containers with their own network namespace own it
// and need nothing.
func OpenNetwork(spec *specs.Spec) error {
	path := ""
	for _, ns := range spec.Linux.Namespaces {
		if ns.Type == specs.NetworkNamespace {
			path = ns.Path
		}
	}
	if path == "" {
		return nil
	}
	sysctls := map[string]string{
		"net.ipv4.ip_unprivileged_port_start": "0",
		"net.ipv4.ping_group_range":           "0 2147483647",
	}
	for k, v := range spec.Linux.Sysctl {
		if strings.HasPrefix(k, "net.") {
			sysctls[k] = v
			delete(spec.Linux.Sysctl, k)
		}
	}
	errc := make(chan error, 1)
	go func() {
		// This thread switches network namespace and is never reused.
		runtime.LockOSThread()
		errc <- func() error {
			fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC, 0)
			if err != nil {
				return err
			}
			defer unix.Close(fd)
			if err := unix.Unshare(unix.CLONE_FS); err != nil {
				return err
			}
			if err := unix.Setns(fd, unix.CLONE_NEWNET); err != nil {
				return fmt.Errorf("enter %s: %w", path, err)
			}
			for k, v := range sysctls {
				file := "/proc/sys/" + strings.ReplaceAll(k, ".", "/")
				if err := os.WriteFile(file, []byte(v), 0o644); err != nil {
					return fmt.Errorf("sysctl %s: %w", k, err)
				}
			}
			return nil
		}()
	}()
	return <-errc
}
