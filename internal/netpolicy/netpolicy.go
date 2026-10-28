//go:build linux

// Package netpolicy limits which network objects processes in gcr
// containers may ask the kernel to create over netlink: link kinds
// (RTM_NEWLINK) and tc kinds (qdiscs, classes, filters), by allowlist.
// Root in a user namespace a container makes owns the network namespaces
// it makes, and the kernel code behind each kind is reachable from there;
// seccomp sees a socket's protocol, not the kind inside a message.
//
// The policy is a BPF LSM program on netlink_send (netlink.bpf.c): it
// runs in the kernel, with no round trip, and sees only tasks in cgroups
// the shim registered. It needs "bpf" in the kernel's LSM list
// (lsm=...,bpf on the kernel command line); without it, Available is
// false and containers run with seccomp alone.
//
// The program and its maps are pinned under /sys/fs/bpf/gcr, so they
// outlive the shim that loaded them and every shim shares them.
package netpolicy

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/containerd/errdefs"
	"golang.org/x/sys/unix"
)

//go:embed netlink.bpf.o
var object []byte

// Mode is a runtime handler's netlink policy setting.
type Mode string

const (
	// Auto applies the policy to every hardened container when the kernel
	// has the BPF LSM, and logs once when it does not.
	Auto Mode = "auto"
	// Off applies it to none.
	Off Mode = "off"
)

func (m Mode) Validate() error {
	switch m {
	case Auto, Off:
		return nil
	}
	return fmt.Errorf("NetlinkPolicy = %q, want auto or off: %w", m, errdefs.ErrInvalidArgument)
}

// PinDir is where the loaded policy lives; lockFile serializes loading
// it (bpffs holds only BPF objects).
const (
	PinDir   = "/sys/fs/bpf/gcr/netlink"
	lockFile = "/run/gcr/netlink.lock"
)

// LinkKinds are the link kinds containers may create: what container
// networking inside a container needs (veth pairs and bridges for nested
// runtimes, dummies and VLANs), and nothing with its own protocol
// implementation in the kernel (bonding, macvlan, vxlan, tunnels, ...).
var LinkKinds = []string{"veth", "dummy", "bridge", "vlan"}

// TCKinds are the qdisc, class and filter kinds containers may create.
var TCKinds = []string{
	"pfifo_fast", "pfifo", "bfifo", "fq_codel", "fq", "codel", "sfq", "tbf",
	"htb", "prio", "mq", "cake", "netem", "ingress", "clsact",
	"u32", "flower", "matchall", "basic", "fw",
}

// Available says whether the kernel has the BPF LSM enabled.
func Available() bool {
	b, err := os.ReadFile("/sys/kernel/security/lsm")
	if err != nil {
		return false
	}
	for _, l := range strings.Split(strings.TrimSpace(string(b)), ",") {
		if l == "bpf" {
			return true
		}
	}
	return false
}

// Policy is the loaded policy.
type Policy struct {
	cgroups *ebpf.Map
	stats   *ebpf.Map
}

type objects struct {
	Prog      *ebpf.Program `ebpf:"gcr_netlink_send"`
	Cgroups   *ebpf.Map     `ebpf:"cgroups"`
	LinkKinds *ebpf.Map     `ebpf:"link_kinds"`
	TCKinds   *ebpf.Map     `ebpf:"tc_kinds"`
	Stats     *ebpf.Map     `ebpf:"stats"`
}

// Load loads and attaches the policy, or opens it if a shim already did,
// and fills in the allowed kinds.
func Load() (*Policy, error) {
	if !Available() {
		return nil, errors.New("the BPF LSM is not enabled (add bpf to the kernel's lsm= list)")
	}
	if err := os.MkdirAll(PinDir, 0o700); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(lockFile), 0o700); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(lockFile, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return nil, err
	}

	spec, err := ebpf.LoadCollectionSpecFromReader(bytes.NewReader(object))
	if err != nil {
		return nil, fmt.Errorf("netlink policy object: %w", err)
	}
	for _, m := range spec.Maps {
		m.Pinning = ebpf.PinByName
	}
	var objs objects
	if err := spec.LoadAndAssign(&objs, &ebpf.CollectionOptions{Maps: ebpf.MapOptions{PinPath: PinDir}}); err != nil {
		return nil, fmt.Errorf("load netlink policy: %w", err)
	}
	defer objs.Prog.Close()
	defer objs.LinkKinds.Close()
	defer objs.TCKinds.Close()

	linkPin := filepath.Join(PinDir, "link")
	if _, err := os.Stat(linkPin); err != nil {
		l, err := link.AttachLSM(link.LSMOptions{Program: objs.Prog})
		if err != nil {
			return nil, fmt.Errorf("attach netlink policy: %w", err)
		}
		if err := l.Pin(linkPin); err != nil {
			l.Close()
			return nil, fmt.Errorf("pin netlink policy: %w", err)
		}
		l.Close() // the pin keeps it attached
	}
	for _, k := range LinkKinds {
		if err := objs.LinkKinds.Put(kindKey(k), uint8(1)); err != nil {
			return nil, err
		}
	}
	for _, k := range TCKinds {
		if err := objs.TCKinds.Put(kindKey(k), uint8(1)); err != nil {
			return nil, err
		}
	}
	return &Policy{cgroups: objs.Cgroups, stats: objs.Stats}, nil
}

func kindKey(name string) [16]byte {
	var k [16]byte
	copy(k[:], name)
	return k
}

// Add covers the processes in cgroup id (and its descendants).
func (p *Policy) Add(id uint64) error { return p.cgroups.Put(id, uint32(1)) }

// Remove uncovers cgroup id.
func (p *Policy) Remove(id uint64) error {
	if err := p.cgroups.Delete(id); err != nil && !errors.Is(err, ebpf.ErrKeyNotExist) {
		return err
	}
	return nil
}

// Stats returns how many messages the policy checked and refused.
func (p *Policy) Stats() (checked, refused uint64, err error) {
	if err := p.stats.Lookup(uint32(0), &checked); err != nil {
		return 0, 0, err
	}
	if err := p.stats.Lookup(uint32(1), &refused); err != nil {
		return 0, 0, err
	}
	return checked, refused, nil
}

func (p *Policy) Close() {
	p.cgroups.Close()
	p.stats.Close()
}

// CgroupID is the ID of the cgroup at path (cgroup v2: the directory's
// inode number), as bpf_get_current_cgroup_id reports it.
func CgroupID(path string) (uint64, error) {
	var st unix.Stat_t
	if err := unix.Stat(path, &st); err != nil {
		return 0, err
	}
	return st.Ino, nil
}
