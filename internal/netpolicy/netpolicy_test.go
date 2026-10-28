//go:build linux

package netpolicy

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/cilium/ebpf"
)

func TestMode(t *testing.T) {
	for _, m := range []Mode{Auto, Off} {
		if err := m.Validate(); err != nil {
			t.Error(err)
		}
	}
	if Mode("sometimes").Validate() == nil {
		t.Error("accepted an unknown mode")
	}
}

// TestObject checks the embedded program is what the loader expects:
// an LSM program on netlink_send and the maps it fills.
func TestObject(t *testing.T) {
	spec, err := ebpf.LoadCollectionSpecFromReader(bytes.NewReader(object))
	if err != nil {
		t.Fatal(err)
	}
	prog := spec.Programs["gcr_netlink_send"]
	if prog == nil || prog.Type != ebpf.LSM || prog.AttachTo != "netlink_send" {
		t.Fatalf("program %+v", prog)
	}
	for _, name := range []string{"cgroups", "link_kinds", "tc_kinds", "stats"} {
		if spec.Maps[name] == nil {
			t.Errorf("no map %s", name)
		}
	}
	if k := spec.Maps["link_kinds"]; k.KeySize != 16 {
		t.Errorf("kind key size %d, want 16", k.KeySize)
	}
	for _, k := range slices.Concat(LinkKinds, TCKinds) {
		if len(k) >= 16 {
			t.Errorf("kind %q does not fit the key", k)
		}
	}
}

func TestCgroupID(t *testing.T) {
	if _, err := os.Stat("/sys/fs/cgroup/cgroup.controllers"); err != nil {
		t.Skip("no cgroup v2")
	}
	id, err := CgroupID("/sys/fs/cgroup")
	if err != nil || id == 0 {
		t.Fatalf("CgroupID = %d, %v", id, err)
	}
}

// TestObjectIsCurrent checks the embedded object was built from the
// source in the tree: `make bpf` records the source's hash beside it,
// since the object differs between clang versions and comparing it would
// say nothing.
func TestObjectIsCurrent(t *testing.T) {
	recorded, err := os.ReadFile("netlink.bpf.sha256")
	if err != nil {
		t.Fatal(err)
	}
	want, _, ok := strings.Cut(strings.TrimSpace(string(recorded)), " ")
	if !ok {
		t.Fatalf("netlink.bpf.sha256: %q", recorded)
	}
	src, err := os.ReadFile("bpf/netlink.bpf.c")
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprintf("%x", sha256.Sum256(src)); got != want {
		t.Fatalf("bpf/netlink.bpf.c changed since the embedded object was built: run make bpf and commit\n got %s\nwant %s", got, want)
	}
}
