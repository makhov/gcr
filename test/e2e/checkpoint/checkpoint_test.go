//go:build e2e && linux

// Package checkpoint checks checkpoint and restore through containerd: a
// counter carries on from where it was checkpointed instead of starting
// over, in the same user namespace.
package checkpoint

import (
	"strings"
	"testing"
	"time"

	"github.com/makhov/gcr/test/e2e/framework"
)

func TestMain(m *testing.M) { framework.Main(m) }

func TestCheckpointRestore(t *testing.T) {
	ns := framework.NewNamespace(t, "gcr-e2e")
	c := ns.Run(framework.UniqueID("ckpt"), framework.Spec{
		Args: []string{"sh", "-c", "i=0; while true; do echo $i > /tmp/count; i=$((i+1)); sleep 0.2; done"},
	})
	time.Sleep(4 * time.Second)
	before := c.ReadInt("/tmp/count")
	mapBefore, _ := c.Exec("cat", "/proc/self/uid_map")

	c.CheckpointRestore()
	time.Sleep(2 * time.Second)
	after := c.ReadInt("/tmp/count")
	t.Logf("counter %d before checkpoint, %d 2s after restore", before, after)
	// A fresh start would be at ~10 now; a restore continues past before.
	if after <= before {
		t.Fatalf("counter restarted (%d -> %d)", before, after)
	}

	mapAfter, _ := c.Exec("cat", "/proc/self/uid_map")
	if strings.Fields(mapBefore) == nil || strings.Join(strings.Fields(mapBefore), " ") != strings.Join(strings.Fields(mapAfter), " ") {
		t.Fatalf("user namespace mapping changed: %q -> %q", mapBefore, mapAfter)
	}
	if owner, _ := c.Exec("stat", "-c", "%u", "/bin/sh"); strings.TrimSpace(owner) != "0" {
		t.Fatalf("image files owned by %q inside after restore", owner)
	}
}
