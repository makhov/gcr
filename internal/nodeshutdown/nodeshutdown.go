// Package nodeshutdown checkpoints pods' containers when their node shuts
// down, so that they carry on elsewhere (docs/design/gcr-operator.md,
// Spot and node shutdown).
//
// On a graceful node shutdown the kubelet stops every pod itself, not
// through evictions, and admits no new pod meanwhile: nothing from the
// cluster can reach the node to checkpoint them. So it happens on the
// node. gcr shutdown-guard holds a systemd-logind delay lock, and when
// the shutdown starts marks it here (Begin). The shim, asked by the
// kubelet to stop a container that opted in (gcr.dev/migrate-on-evict),
// first checkpoints and stops it into CheckpointDir/POD-UID, the
// directory gcr-operator restores the pod's replacement from; the guard
// pushes each checkpoint to the registry, tagged with the pod's UID, and
// lets the shutdown go on once they are pushed.
//
// The state is files in Dir: Dir itself while the node shuts down, and
// UID.pending, UID.ready or UID.failed for each pod's checkpoint, then
// UID.pushed once the guard has pushed it.
package nodeshutdown

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// Dir is there while the node shuts down, and holds the checkpoints'
// states. It is under /run, so a boot starts without it.
var Dir = "/run/gcr/shutdown"

// CheckpointDir is where the checkpoints are written: setup.CheckpointDir,
// which gcr-migrate restores from.
var CheckpointDir = "/var/lib/gcr/checkpoints"

// AnnotationOptIn is the pod's opt-in to moving rather than stopping:
// gcr-operator's, for evictions too.
const AnnotationOptIn = "gcr.dev/migrate-on-evict"

// States of a pod's checkpoint, the suffixes of its file in Dir.
const (
	Pending = ".pending" // being written
	Ready   = ".ready"   // written, to be pushed
	Failed  = ".failed"  // not written: the file says why
	Pushed  = ".pushed"  // in the registry
)

// PodUID returns the UID of the pod a container belongs to if its
// checkpoint is wanted when the node shuts down: the pod opted in and the
// container is its workload's. "" otherwise.
func PodUID(annotations map[string]string) string {
	if annotations[AnnotationOptIn] != "true" || annotations["io.kubernetes.cri.container-type"] != "container" {
		return ""
	}
	uid := annotations["io.kubernetes.cri.sandbox-uid"]
	if uid == "" || strings.ContainsAny(uid, "/\x00") || strings.HasPrefix(uid, ".") {
		return ""
	}
	return uid
}

// ShuttingDown says whether the node is shutting down.
func ShuttingDown() bool {
	_, err := os.Stat(Dir)
	return err == nil
}

// Begin marks the node shutting down.
func Begin() error { return os.MkdirAll(Dir, 0o700) }

// Cancel unmarks it: a shutdown called off, or a guard starting.
func Cancel() error { return os.RemoveAll(Dir) }

// CheckpointPath is where pod uid's checkpoint goes.
func CheckpointPath(uid string) string { return filepath.Join(CheckpointDir, uid) }

// Set records the state of pod uid's checkpoint, with why for Failed.
// The others states' files go.
func Set(uid, state, why string) error {
	if err := os.WriteFile(filepath.Join(Dir, uid+state), []byte(why), 0o600); err != nil {
		return err
	}
	for _, s := range []string{Pending, Ready, Failed, Pushed} {
		if s != state {
			_ = os.Remove(filepath.Join(Dir, uid+s))
		}
	}
	return nil
}

// States returns the pods' checkpoints' states, by pod UID.
func States() (map[string]string, error) {
	entries, err := os.ReadDir(Dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	states := map[string]string{}
	for _, e := range entries {
		ext := filepath.Ext(e.Name())
		switch ext {
		case Pending, Ready, Failed, Pushed:
			states[strings.TrimSuffix(e.Name(), ext)] = ext
		}
	}
	return states, nil
}
