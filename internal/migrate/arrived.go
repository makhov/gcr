package migrate

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ArrivedDir records the pods' containers a migration came into on this
// node. Such a pod keeps gcr.dev/migrate-in, its sandbox's annotations
// and so every container's in it after the first: a container that
// restarts in it, or in a sandbox the kubelet makes anew, must start as a
// new one, not wait for a migration that will not come.
var ArrivedDir = "/var/lib/gcr/migrated"

// keepArrived is how long a record is kept: longer than a pod lives on a
// node, near enough.
const keepArrived = 90 * 24 * time.Hour

// ArrivedKey names a pod's container for Arrived: the pod's UID and the
// container's name, "" for a container that is not a pod's.
func ArrivedKey(annotations map[string]string) string {
	uid, name := annotations["io.kubernetes.cri.sandbox-uid"], annotations["io.kubernetes.cri.container-name"]
	if uid == "" || name == "" || strings.ContainsAny(uid+name, "/\x00") || strings.HasPrefix(uid+name, ".") {
		return ""
	}
	return uid + "_" + name
}

// DepartedDir records the pods' containers that migrated away from this
// node. Their processes run elsewhere now, while the pod here lingers
// until it is deleted: a container the kubelet restarts in it would run
// the workload a second time, cold, so it is refused.
var DepartedDir = "/var/lib/gcr/departed"

// Arrived says whether a migration came into key's container before.
func Arrived(key string) bool { return marked(ArrivedDir, key) }

// MarkArrived records that a migration came into key's container, and
// forgets records older than keepArrived.
func MarkArrived(key string) error { return mark(ArrivedDir, key) }

// Departed says whether key's container migrated away from this node.
func Departed(key string) bool { return marked(DepartedDir, key) }

// MarkDeparted records that key's container migrated away.
func MarkDeparted(key string) error { return mark(DepartedDir, key) }

func marked(dir, key string) bool {
	if key == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(dir, key))
	return err == nil
}

func mark(dir, key string) error {
	if key == "" {
		return nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if entries, err := os.ReadDir(dir); err == nil {
		for _, e := range entries {
			if info, err := e.Info(); err == nil && time.Since(info.ModTime()) > keepArrived {
				_ = os.Remove(filepath.Join(dir, e.Name()))
			}
		}
	}
	err := os.WriteFile(filepath.Join(dir, key), nil, 0o600)
	if errors.Is(err, os.ErrExist) {
		return nil
	}
	return err
}
