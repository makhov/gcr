//go:build linux

package netpolicy

import (
	"bytes"
	"os"
	"slices"
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
