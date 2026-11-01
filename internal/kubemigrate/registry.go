package kubemigrate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	annotationForkFrom = "gcr.dev/fork-from"
	annotationForkWait = "gcr.dev/fork-wait"
	// checkpointDir is where checkpoints are written and pulled on the
	// nodes: setup.CheckpointDir, which gcr-migrate may restore from.
	checkpointDir = "/var/lib/gcr/checkpoints"
	// forkWait is how long the replacement's shim waits for the
	// checkpoint to be pulled to its node (container.MaxForkWait at most).
	forkWait = "100s"
)

// RegistryOptions say where checkpoints go.
type RegistryOptions struct {
	// Repository is a registry repository (host[:port]/path) checkpoints
	// are pushed to, tagged by the pod's UID, which the nodes can reach.
	Repository string
	// PlainHTTP talks to it without TLS.
	PlainHTTP bool
}

// MigrateViaRegistry moves pod name to another node through a checkpoint
// in a registry, rather than from node to node: the source is stopped at
// a checkpoint, which is pushed, and a replacement pod restores from it
// once it is pulled to the replacement's node. The downtime is seconds,
// not the fraction of one Migrate takes, but nothing on the source is
// needed once the checkpoint is pushed: what a node going away (spot,
// shutdown), or a pod that can't have a twin running (a StatefulSet's),
// needs.
//
// Bare pods only, for now. If the checkpoint fails, the source runs on
// and the replacement is deleted. Once the source has stopped there is no
// going back to it: a later failure leaves the replacement waiting for a
// checkpoint, and says so.
func MigrateViaRegistry(ctx context.Context, cs kubernetes.Interface, ns, name string, o Options, r RegistryOptions) (res Result, retErr error) {
	if o.Timeout == 0 {
		o.Timeout = 5 * time.Minute
	}
	if o.GCR == "" {
		o.GCR = "/usr/local/bin/gcr"
	}
	log := o.Log
	if log == nil {
		log = func(string, ...any) {}
	}
	if r.Repository == "" || strings.Contains(path.Base(r.Repository), ":") || strings.Contains(r.Repository, "@") {
		return res, fmt.Errorf("registry %q: want a repository, host[:port]/path, with no tag", r.Repository)
	}
	src, err := cs.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return res, err
	}
	id, err := Movable(src)
	if err != nil {
		return res, fmt.Errorf("%s/%s: %w", ns, name, err)
	}
	if len(src.OwnerReferences) > 0 {
		return res, fmt.Errorf("%s/%s is owned by %s %s: through a registry, only bare pods can be moved yet", ns, name,
			src.OwnerReferences[0].Kind, src.OwnerReferences[0].Name)
	}
	for _, v := range src.Spec.Volumes {
		// A checkpoint holds the writable layer; Migrate carries emptyDirs.
		if v.EmptyDir != nil {
			return res, fmt.Errorf("%s/%s: volume %s: through a registry, emptyDir volumes don't move yet (without -registry they do)", ns, name, v.Name)
		}
	}
	if o.Node == src.Spec.NodeName {
		return res, fmt.Errorf("%s/%s is on %s already", ns, name, o.Node)
	}

	log("asking %s for %s's user namespace", src.Spec.NodeName, name)
	out, err := runJob(ctx, cs, o, src.Spec.NodeName, "info", nil, o.GCR, "-n", containerdNamespace, "info", id)
	if err != nil {
		return res, fmt.Errorf("gcr info on %s: %w\n%s", src.Spec.NodeName, err, out)
	}
	var inf info
	if err := json.Unmarshal([]byte(lastLine(out)), &inf); err != nil {
		return res, fmt.Errorf("gcr info on %s: %w: %q", src.Spec.NodeName, err, out)
	}

	// The replacement restores from a checkpoint not there yet: its node's
	// shim waits for it, once the pod is placed and the checkpoint pulled.
	dir := path.Join(checkpointDir, string(src.UID))
	ref := r.Repository + ":" + string(src.UID)
	dst := targetPod(src, o.Node, "", "", inf)
	delete(dst.Annotations, annotationMigrateIn)
	delete(dst.Annotations, annotationMigrateToken)
	dst.Annotations[annotationForkFrom] = dir
	dst.Annotations[annotationForkWait] = forkWait
	if dst, err = cs.CoreV1().Pods(ns).Create(ctx, dst, metav1.CreateOptions{}); err != nil {
		return res, fmt.Errorf("create the replacement: %w", err)
	}
	res.Target = dst.Name
	stopped, done := false, false
	defer func() {
		if !stopped {
			_ = cs.CoreV1().Pods(ns).Delete(context.WithoutCancel(ctx), dst.Name, metav1.DeleteOptions{GracePeriodSeconds: new(int64)})
		} else if !done {
			retErr = fmt.Errorf("%w (%s stopped at its checkpoint %s; %s waits for it)", retErr, name, ref, dst.Name)
		}
	}()
	log("replacement %s created; waiting for it to be scheduled", dst.Name)
	if dst, err = waitScheduled(ctx, cs, ns, dst.Name, o.Timeout); err != nil {
		return res, err
	}
	res.Node = dst.Spec.NodeName

	plain := ""
	if r.PlainHTTP {
		plain = " -plain-http"
	}
	// "stopped" marks the point of no return: the snapshot is written and
	// the source killed. Before it, a failure leaves the source running.
	script := fmt.Sprintf(`set -e
%[1]s -n %[2]s snapshot -stop %[3]s %[4]s
echo stopped
%[1]s snapshot-push%[5]s %[4]s %[6]s
%[1]s snapshot-rm %[4]s`, o.GCR, containerdNamespace, id, dir, plain, ref)
	log("checkpointing %s on %s to %s", name, src.Spec.NodeName, ref)
	begin := time.Now()
	out, err = runJob(ctx, cs, o, src.Spec.NodeName, "checkpoint", nil, "sh", "-c", script)
	stopped = strings.Contains(out, "stopped")
	switch {
	case errors.Is(err, errNoOutput):
		// It succeeded (set -e): the source stopped and was pushed.
		stopped = true
		log("%v", err)
	case err != nil:
		return res, fmt.Errorf("checkpoint on %s: %w\n%s", src.Spec.NodeName, err, out)
	}
	log("restoring it on %s", res.Node)
	// It says so when done: runJob takes an empty log for one not read yet.
	pull := fmt.Sprintf("%s snapshot-pull%s %s %s && echo pulled", o.GCR, plain, ref, dir)
	if out, err = runJob(ctx, cs, o, res.Node, "pull", nil, "sh", "-c", pull); errors.Is(err, errNoOutput) {
		log("%v", err) // it succeeded
	} else if err != nil {
		return res, fmt.Errorf("pull on %s: %w\n%s", res.Node, err, out)
	}
	if err := waitReady(ctx, cs, ns, dst.Name, o.Timeout); err != nil {
		return res, fmt.Errorf("%s restored from %s, but it is not Ready: %w", dst.Name, ref, err)
	}
	res.Downtime = time.Since(begin).Round(time.Millisecond).String() + " (from the stop to Ready)"
	done = true
	log("deleting %s", name)
	if err := cs.CoreV1().Pods(ns).Delete(ctx, name, metav1.DeleteOptions{GracePeriodSeconds: new(int64)}); err != nil && !apierrors.IsNotFound(err) {
		return res, fmt.Errorf("%s runs in %s now, but deleting %s failed: %w", name, dst.Name, name, err)
	}
	return res, nil
}
