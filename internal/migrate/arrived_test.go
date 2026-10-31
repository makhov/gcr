package migrate

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestArrived(t *testing.T) {
	ArrivedDir = filepath.Join(t.TempDir(), "migrated")
	pod := map[string]string{"io.kubernetes.cri.sandbox-uid": "d79ebd15", "io.kubernetes.cri.container-name": "c"}
	key := ArrivedKey(pod)
	if key == "" || Arrived(key) {
		t.Fatalf("key %q: arrived before any migration", key)
	}
	if err := MarkArrived(key); err != nil || !Arrived(key) {
		t.Fatalf("after MarkArrived: %v, arrived %v", err, Arrived(key))
	}
	// Not a pod's container (ctr), or names that could escape the directory.
	for _, a := range []map[string]string{
		{},
		{"io.kubernetes.cri.sandbox-uid": "../x", "io.kubernetes.cri.container-name": "c"},
		{"io.kubernetes.cri.sandbox-uid": ".", "io.kubernetes.cri.container-name": "c"},
	} {
		if k := ArrivedKey(a); k != "" {
			t.Errorf("%v: key %q", a, k)
		}
	}
	// Old records go.
	old := filepath.Join(ArrivedDir, "old_c")
	if err := os.WriteFile(old, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-keepArrived - time.Hour)
	if err := os.Chtimes(old, past, past); err != nil {
		t.Fatal(err)
	}
	if err := MarkArrived(key); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Errorf("an old record is kept: %v", err)
	}
}
