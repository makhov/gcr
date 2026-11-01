package kubemigrate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"path"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
)

const (
	annotationForkFrom = "gcr.dev/fork-from"
	annotationForkWait = "gcr.dev/fork-wait"
	// AnnotationRestoreToken is on a StatefulSet's pod steered to restore
	// from a checkpoint: its pending restore's token, which lets the
	// StatefulSet controller create it on gcr-migrate.
	AnnotationRestoreToken = "gcr.dev/restore-token"
	// RestoresConfigMap, in gcr's namespace, holds the pending restores:
	// "NAMESPACE.NAME" (namespaces have no dots) to a Restore, as JSON.
	// The gcr-migrate admission policy reads it as its parameter.
	RestoresConfigMap = "gcr-restores"
	// RestoreTTL is how long a pending restore is good for: one older was
	// left by a migration that went away, and its pod starts cold.
	RestoreTTL = 15 * time.Minute
	// checkpointDir is where checkpoints are written and pulled on the
	// nodes: setup.CheckpointDir, which gcr-migrate may restore from.
	checkpointDir = "/var/lib/gcr/checkpoints"
	// forkWait is how long the replacement's shim waits for the
	// checkpoint to be pulled to its node (container.MaxForkWait at most).
	forkWait = "100s"
	// pushWait is how long a checkpoint taken as its node shut down may
	// take to reach the registry: the node's gcr shutdown-guard holds the
	// shutdown for 80 s by default.
	pushWait = 3 * time.Minute
)

// RegistryOptions say where checkpoints go.
type RegistryOptions struct {
	// Repository is a registry repository (host[:port]/path) checkpoints
	// are pushed to, tagged by the pod's UID, which the nodes can reach.
	Repository string
	// PlainHTTP talks to it without TLS.
	PlainHTTP bool
}

// Restore is a pending restore: a StatefulSet's pod, checkpointed and
// deleted, which its StatefulSet creates again to restore from Ref.
//
// Token and Dir come first, in this order: the gcr-migrate admission
// policy matches the start of the JSON, {"token":"T","dir":"D", against
// the new pod's annotations, CEL having no JSON parser.
type Restore struct {
	Token string `json:"token"`
	Dir   string `json:"dir"`
	Ref   string `json:"ref"`
	// UID is the checkpointed pod's, and Node where it ran.
	UID    string    `json:"uid"`
	Node   string    `json:"node"`
	IDBase uint32    `json:"idBase,omitempty"`
	Since  time.Time `json:"since"`
}

// MovableThroughRegistry says whether MigrateViaRegistry can move the pod,
// and returns its container's containerd ID: one without an owner, or a
// StatefulSet's, whose PersistentVolumeClaims follow it (unless they are
// node-local, which MigrateViaRegistry checks; a bare pod has none).
func MovableThroughRegistry(p *corev1.Pod) (string, error) {
	if p.Status.Phase != corev1.PodRunning {
		return "", fmt.Errorf("not running (%s)", p.Status.Phase)
	}
	return MovableAtShutdown(p)
}

// MovableAtShutdown says whether RestoreAfterShutdown can restore the pod,
// checkpointed as its node shut down: as MovableThroughRegistry, whether
// or not it is still running.
func MovableAtShutdown(p *corev1.Pod) (string, error) {
	if len(p.OwnerReferences) > 0 && !ownedBy(p, "StatefulSet") {
		return "", fmt.Errorf("owned by %s %s: through a registry, only pods without an owner, or a StatefulSet's, can be moved yet",
			p.OwnerReferences[0].Kind, p.OwnerReferences[0].Name)
	}
	for _, v := range p.Spec.Volumes {
		// A checkpoint holds the writable layer; Migrate carries emptyDirs.
		if v.EmptyDir != nil {
			return "", fmt.Errorf("volume %s: through a registry, emptyDir volumes don't move yet", v.Name)
		}
	}
	// A bare pod's replacement is made before the source goes, and
	// couldn't attach a ReadWriteOnce volume the source holds.
	return movable(p, len(p.OwnerReferences) > 0)
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
// A bare pod's replacement is created here. A StatefulSet's pod is
// deleted, and the StatefulSet creates it again by the same name: a
// pending restore (RecordRestore) has gcr-operator's webhook steer it to
// restore from the checkpoint (Steer).
//
// If the checkpoint fails, the source runs on. Once the source has
// stopped there is no going back to it: a bare pod's replacement is left
// waiting for the checkpoint, and a StatefulSet's pod is deleted, to be
// started again cold.
func MigrateViaRegistry(ctx context.Context, cs kubernetes.Interface, ns, name string, o Options, r RegistryOptions) (res Result, retErr error) {
	if o.Timeout == 0 {
		o.Timeout = 5 * time.Minute
	}
	if o.GCR == "" {
		o.GCR = "/usr/local/bin/gcr"
	}
	if o.Log == nil {
		o.Log = func(string, ...any) {}
	}
	log := o.Log
	if r.Repository == "" || strings.Contains(path.Base(r.Repository), ":") || strings.Contains(r.Repository, "@") {
		return res, fmt.Errorf("registry %q: want a repository, host[:port]/path, with no tag", r.Repository)
	}
	src, err := cs.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return res, err
	}
	id, err := MovableThroughRegistry(src)
	if err != nil {
		return res, fmt.Errorf("%s/%s: %w", ns, name, err)
	}
	if o.Node == src.Spec.NodeName {
		return res, fmt.Errorf("%s/%s is on %s already", ns, name, o.Node)
	}
	sts := ownedBy(src, "StatefulSet")
	if sts && o.Node != "" {
		return res, fmt.Errorf("%s/%s is a StatefulSet's: the scheduler places the pod it creates again, and a node can't be chosen", ns, name)
	}
	if err := claimsFollow(ctx, cs, src); err != nil {
		return res, fmt.Errorf("%s/%s: %w", ns, name, err)
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

	dir := path.Join(checkpointDir, string(src.UID))
	ref := r.Repository + ":" + string(src.UID)
	if sts {
		return viaStatefulSet(ctx, cs, o, r, src, id, inf, dir, ref)
	}

	// The replacement restores from a checkpoint not there yet: its node's
	// shim waits for it, once the pod is placed and the checkpoint pulled.
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

	log("checkpointing %s on %s to %s", name, src.Spec.NodeName, ref)
	begin := time.Now()
	if stopped, err = stopAndPush(ctx, cs, o, r, src, id, dir, ref); err != nil {
		return res, err
	}
	log("restoring it on %s", res.Node)
	if err := pull(ctx, cs, o, r, res.Node, ref, dir); err != nil {
		return res, err
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

// viaStatefulSet moves a StatefulSet's pod: checkpointed and deleted, it
// is created again by its StatefulSet, steered to restore by its pending
// restore. Unlike a bare pod's replacement, the new pod can't be made
// first: it has the same name.
func viaStatefulSet(ctx context.Context, cs kubernetes.Interface, o Options, r RegistryOptions, src *corev1.Pod, id string, inf info, dir, ref string) (res Result, retErr error) {
	ns, name, log := src.Namespace, src.Name, o.Log
	token, err := randomToken()
	if err != nil {
		return res, err
	}
	rs := Restore{Token: token, Dir: dir, Ref: ref, UID: string(src.UID), Node: src.Spec.NodeName, IDBase: inf.IDBase, Since: time.Now().UTC()}
	// Once it is stopped, it can only start again: cold, if it can't be
	// restored.
	deleteSource := func() error {
		uid := src.UID
		err := cs.CoreV1().Pods(ns).Delete(context.WithoutCancel(ctx), name, metav1.DeleteOptions{
			GracePeriodSeconds: new(int64), Preconditions: &metav1.Preconditions{UID: &uid}})
		if apierrors.IsNotFound(err) || apierrors.IsConflict(err) {
			return nil // gone already
		}
		return err
	}
	log("checkpointing %s on %s to %s", name, src.Spec.NodeName, ref)
	begin := time.Now()
	if stopped, err := stopAndPush(ctx, cs, o, r, src, id, dir, ref); err != nil {
		if stopped {
			_ = deleteSource()
			err = fmt.Errorf("%w (%s was stopped: deleted it, for its StatefulSet to start it again, cold)", err, name)
		}
		return res, err
	}
	if err := RecordRestore(ctx, cs, o.Namespace, ns, name, rs); err != nil {
		_ = deleteSource()
		return res, fmt.Errorf("record the restore: %w (deleted %s, for its StatefulSet to start it again, cold)", err, name)
	}
	defer func() {
		if err := ForgetRestore(context.WithoutCancel(ctx), cs, o.Namespace, ns, name); err != nil {
			log("forgetting the restore: %v", err)
		}
	}()
	log("deleting %s, for its StatefulSet to create it again", name)
	if err := deleteSource(); err != nil {
		return res, fmt.Errorf("delete %s: %w", name, err)
	}
	dst, err := waitRecreated(ctx, cs, ns, name, src.UID, o.Timeout)
	if err != nil {
		return res, err
	}
	res.Target, res.Node = dst.Name, dst.Spec.NodeName
	if dst.Annotations[AnnotationRestoreToken] != token {
		return res, fmt.Errorf("%s was created again without restoring, so it started cold: gcr-operator's webhook did not steer it", name)
	}
	// Not restored: deleted again, it is created again cold.
	cold := func(err error) error {
		uid := dst.UID
		_ = cs.CoreV1().Pods(ns).Delete(context.WithoutCancel(ctx), name, metav1.DeleteOptions{
			GracePeriodSeconds: new(int64), Preconditions: &metav1.Preconditions{UID: &uid}})
		return fmt.Errorf("%w (deleted %s again, for its StatefulSet to start it cold)", err, name)
	}
	log("restoring it on %s", res.Node)
	if err := pull(ctx, cs, o, r, res.Node, ref, dir); err != nil {
		return res, cold(err)
	}
	if err := waitReady(ctx, cs, ns, name, o.Timeout); err != nil {
		return res, cold(fmt.Errorf("%s restored from %s, but it is not Ready: %w", name, ref, err))
	}
	res.Downtime = time.Since(begin).Round(time.Millisecond).String() + " (from the stop to Ready)"
	return res, nil
}

// stopAndPush stops the source at a checkpoint in dir on its node, and
// pushes it to ref. Stopped says whether it got as far as stopping it,
// the point of no return, even when it fails after.
func stopAndPush(ctx context.Context, cs kubernetes.Interface, o Options, r RegistryOptions, src *corev1.Pod, id, dir, ref string) (stopped bool, err error) {
	script := fmt.Sprintf(`set -e
%[1]s -n %[2]s snapshot -stop %[3]s %[4]s
echo stopped
%[1]s snapshot-push%[5]s %[4]s %[6]s
%[1]s snapshot-rm %[4]s`, o.GCR, containerdNamespace, id, dir, plainHTTP(r), ref)
	out, err := runJob(ctx, cs, o, src.Spec.NodeName, "checkpoint", nil, "sh", "-c", script)
	switch {
	case errors.Is(err, errNoOutput):
		// It succeeded (set -e): the source stopped and was pushed.
		o.Log("%v", err)
		return true, nil
	case err != nil:
		return strings.Contains(out, "stopped"), fmt.Errorf("checkpoint on %s: %w\n%s", src.Spec.NodeName, err, out)
	}
	return true, nil
}

// pull pulls the checkpoint at ref into dir on node.
func pull(ctx context.Context, cs kubernetes.Interface, o Options, r RegistryOptions, node, ref, dir string) error {
	// It says so when done: runJob takes an empty log for one not read yet.
	script := fmt.Sprintf("%s snapshot-pull%s %s %s && echo pulled", o.GCR, plainHTTP(r), ref, dir)
	if out, err := runJob(ctx, cs, o, node, "pull", nil, "sh", "-c", script); errors.Is(err, errNoOutput) {
		o.Log("%v", err) // it succeeded
	} else if err != nil {
		return fmt.Errorf("pull on %s: %w\n%s", node, err, out)
	}
	return nil
}

func plainHTTP(r RegistryOptions) string {
	if r.PlainHTTP {
		return " -plain-http"
	}
	return ""
}

// waitRecreated waits for pod name to be created again, other than the
// pod uid: until it is scheduled, or, not steered to restore, created.
func waitRecreated(ctx context.Context, cs kubernetes.Interface, ns, name string, uid types.UID, timeout time.Duration) (*corev1.Pod, error) {
	var dst *corev1.Pod
	err := wait.PollUntilContextTimeout(ctx, 250*time.Millisecond, timeout, true, func(ctx context.Context) (bool, error) {
		p, err := cs.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
		switch {
		case apierrors.IsNotFound(err):
			return false, nil
		case err != nil:
			return false, err
		case p.UID == uid:
			return false, nil
		}
		dst = p
		return p.Status.HostIP != "" || p.Annotations[AnnotationRestoreToken] == "", nil
	})
	if err != nil {
		if dst == nil {
			return nil, fmt.Errorf("%s was not created again: %w", name, err)
		}
		return nil, fmt.Errorf("%s was not scheduled: %w", name, err)
	}
	return dst, nil
}

// claimsFollow checks that the pod's PersistentVolumeClaims can follow it
// to another node: bound, and to volumes not tied to one node.
func claimsFollow(ctx context.Context, cs kubernetes.Interface, p *corev1.Pod) error {
	for _, v := range p.Spec.Volumes {
		if v.PersistentVolumeClaim == nil {
			continue
		}
		pvc, err := cs.CoreV1().PersistentVolumeClaims(p.Namespace).Get(ctx, v.PersistentVolumeClaim.ClaimName, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("volume %s: %w", v.Name, err)
		}
		if pvc.Spec.VolumeName == "" {
			return fmt.Errorf("volume %s: claim %s is not bound", v.Name, pvc.Name)
		}
		pv, err := cs.CoreV1().PersistentVolumes().Get(ctx, pvc.Spec.VolumeName, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("volume %s: %w", v.Name, err)
		}
		if why := nodeLocal(pv); why != "" {
			return fmt.Errorf("volume %s: %s %s, so its files would stay on %s", v.Name, pv.Name, why, p.Spec.NodeName)
		}
	}
	return nil
}

// nodeLocal says why a volume can't be used from another node, or "".
func nodeLocal(pv *corev1.PersistentVolume) string {
	switch {
	case pv.Spec.Local != nil:
		return "is a local volume"
	case pv.Spec.HostPath != nil:
		return "is a hostPath volume"
	case pv.Spec.NodeAffinity != nil && pv.Spec.NodeAffinity.Required != nil:
		for _, t := range pv.Spec.NodeAffinity.Required.NodeSelectorTerms {
			for _, e := range t.MatchExpressions {
				if e.Key == "kubernetes.io/hostname" {
					return "can be used on some nodes only"
				}
			}
		}
	}
	return ""
}

func restoreKey(ns, name string) string { return ns + "." + name }

// RecordRestore records the pending restore of pod ns/name, in gcrNS.
func RecordRestore(ctx context.Context, cs kubernetes.Interface, gcrNS, ns, name string, r Restore) error {
	v, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return patchRestores(ctx, cs, gcrNS, map[string]any{restoreKey(ns, name): string(v)})
}

// ForgetRestore removes pod ns/name's pending restore, if any.
func ForgetRestore(ctx context.Context, cs kubernetes.Interface, gcrNS, ns, name string) error {
	err := patchRestores(ctx, cs, gcrNS, map[string]any{restoreKey(ns, name): nil})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

// patchRestores sets (or, with nil, deletes) keys of the pending
// restores: a merge patch, so that moves running at once don't conflict.
func patchRestores(ctx context.Context, cs kubernetes.Interface, gcrNS string, data map[string]any) error {
	patch, _ := json.Marshal(map[string]any{"data": data})
	_, err := cs.CoreV1().ConfigMaps(gcrNS).Patch(ctx, RestoresConfigMap, types.MergePatchType, patch, metav1.PatchOptions{})
	if !apierrors.IsNotFound(err) {
		return err
	}
	// gcr.yaml makes it; created here, it is just as good.
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: RestoresConfigMap, Namespace: gcrNS}, Data: map[string]string{}}
	for k, v := range data {
		if s, ok := v.(string); ok {
			cm.Data[k] = s
		}
	}
	if len(cm.Data) == 0 {
		return nil
	}
	_, err = cs.CoreV1().ConfigMaps(gcrNS).Create(ctx, cm, metav1.CreateOptions{})
	return err
}

// PendingRestore returns pod ns/name's pending restore, or nil: none, or
// one older than RestoreTTL.
func PendingRestore(ctx context.Context, cs kubernetes.Interface, gcrNS, ns, name string) (*Restore, error) {
	cm, err := cs.CoreV1().ConfigMaps(gcrNS).Get(ctx, RestoresConfigMap, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	v, ok := cm.Data[restoreKey(ns, name)]
	if !ok {
		return nil, nil
	}
	var r Restore
	if err := json.Unmarshal([]byte(v), &r); err != nil {
		return nil, fmt.Errorf("restore of %s/%s: %w", ns, name, err)
	}
	if time.Since(r.Since) > RestoreTTL {
		return nil, nil
	}
	return &r, nil
}

// Steer returns the JSON patch that makes pod, created again by its
// StatefulSet, restore as r says: on gcr-migrate, from r's checkpoint
// once it is pulled to the pod's node, in the user namespace the pod ran
// in, and with r's token, which lets the StatefulSet controller create it.
func Steer(pod *corev1.Pod, r Restore) ([]byte, error) {
	ann := maps.Clone(pod.Annotations)
	if ann == nil {
		ann = map[string]string{}
	}
	ann[annotationForkFrom] = r.Dir
	ann[annotationForkWait] = forkWait
	ann[AnnotationRestoreToken] = r.Token
	delete(ann, annotationMigrateIn)
	delete(ann, annotationMigrateToken)
	if r.IDBase != 0 {
		ann[annotationUsernsBase] = fmt.Sprint(r.IDBase)
	} else {
		ann[annotationHostUsers] = "true"
	}
	return json.Marshal([]map[string]any{
		{"op": "add", "path": "/metadata/annotations", "value": ann},
		{"op": "add", "path": "/spec/runtimeClassName", "value": RuntimeClass},
	})
}

// RestoreAfterShutdown restores pod src, which its node's shutdown stopped
// at a checkpoint (internal/nodeshutdown): the node's shim checkpoints it
// as the kubelet stops it, and gcr shutdown-guard pushes the checkpoint to
// r, tagged with the pod's UID. A StatefulSet's pod is restored in the pod
// its StatefulSet creates again, steered by a pending restore recorded
// here (which must be before the StatefulSet sees src fail); a bare pod
// in a replacement made here. If the checkpoint never arrives, the
// StatefulSet's pod starts cold, and the bare pod is left failed.
func RestoreAfterShutdown(ctx context.Context, cs kubernetes.Interface, src *corev1.Pod, rs *Restore, o Options, r RegistryOptions) (res Result, retErr error) {
	if o.Timeout == 0 {
		o.Timeout = 5 * time.Minute
	}
	if o.GCR == "" {
		o.GCR = "/usr/local/bin/gcr"
	}
	if o.Log == nil {
		o.Log = func(string, ...any) {}
	}
	ns, name, log := src.Namespace, src.Name, o.Log
	if _, err := MovableAtShutdown(src); err != nil {
		return res, fmt.Errorf("%s/%s: %w", ns, name, err)
	}
	dir := path.Join(checkpointDir, string(src.UID))
	ref := r.Repository + ":" + string(src.UID)
	begin := time.Now()
	var err error

	if !ownedBy(src, "StatefulSet") {
		dst := targetPod(src, "", "", "", info{})
		delete(dst.Annotations, annotationMigrateIn)
		delete(dst.Annotations, annotationMigrateToken)
		dst.Annotations[annotationForkFrom] = dir
		dst.Annotations[annotationForkWait] = forkWait
		if dst, err = cs.CoreV1().Pods(ns).Create(ctx, dst, metav1.CreateOptions{}); err != nil {
			return res, fmt.Errorf("create the replacement: %w", err)
		}
		res.Target = dst.Name
		done := false
		defer func() {
			if !done {
				_ = cs.CoreV1().Pods(ns).Delete(context.WithoutCancel(ctx), dst.Name, metav1.DeleteOptions{GracePeriodSeconds: new(int64)})
			}
		}()
		log("replacement %s created, to restore from %s", dst.Name, ref)
		if dst, err = waitScheduled(ctx, cs, ns, dst.Name, o.Timeout); err != nil {
			return res, err
		}
		res.Node = dst.Spec.NodeName
		if err := pullWhenPushed(ctx, cs, o, r, res.Node, ref, dir); err != nil {
			return res, err
		}
		if err := waitReady(ctx, cs, ns, dst.Name, o.Timeout); err != nil {
			return res, fmt.Errorf("%s restored from %s, but it is not Ready: %w", dst.Name, ref, err)
		}
		done = true
		res.Downtime = time.Since(begin).Round(time.Millisecond).String() + " (from the shutdown seen to Ready)"
		if err := cs.CoreV1().Pods(ns).Delete(ctx, name, metav1.DeleteOptions{GracePeriodSeconds: new(int64)}); err != nil && !apierrors.IsNotFound(err) {
			log("deleting %s: %v", name, err)
		}
		return res, nil
	}

	if rs == nil {
		if rs, err = ShutdownRestore(src, r); err != nil {
			return res, err
		}
		if err := RecordRestore(ctx, cs, o.Namespace, ns, name, *rs); err != nil {
			return res, fmt.Errorf("record the restore: %w", err)
		}
	}
	token := rs.Token
	defer func() {
		if err := ForgetRestore(context.WithoutCancel(ctx), cs, o.Namespace, ns, name); err != nil {
			log("forgetting the restore: %v", err)
		}
	}()
	log("waiting for its StatefulSet to create %s again, to restore from %s", name, ref)
	dst, err := waitRecreated(ctx, cs, ns, name, src.UID, pushWait+o.Timeout)
	if err != nil {
		return res, err
	}
	res.Target, res.Node = dst.Name, dst.Spec.NodeName
	if dst.Annotations[AnnotationRestoreToken] != token {
		return res, fmt.Errorf("%s was created again without restoring, so it started cold: gcr-operator's webhook did not steer it", name)
	}
	cold := func(err error) error {
		uid := dst.UID
		_ = cs.CoreV1().Pods(ns).Delete(context.WithoutCancel(ctx), name, metav1.DeleteOptions{
			GracePeriodSeconds: new(int64), Preconditions: &metav1.Preconditions{UID: &uid}})
		return fmt.Errorf("%w (deleted %s again, for its StatefulSet to start it cold)", err, name)
	}
	if err := pullWhenPushed(ctx, cs, o, r, res.Node, ref, dir); err != nil {
		return res, cold(err)
	}
	if err := waitReady(ctx, cs, ns, name, o.Timeout); err != nil {
		return res, cold(fmt.Errorf("%s restored from %s, but it is not Ready: %w", name, ref, err))
	}
	res.Downtime = time.Since(begin).Round(time.Millisecond).String() + " (from the shutdown seen to Ready)"
	return res, nil
}

// ShutdownRestore is the pending restore of a StatefulSet's pod src, which
// its node's shutdown checkpointed, to record before its StatefulSet
// creates it again.
func ShutdownRestore(src *corev1.Pod, r RegistryOptions) (*Restore, error) {
	token, err := randomToken()
	if err != nil {
		return nil, err
	}
	return &Restore{Token: token, Dir: path.Join(checkpointDir, string(src.UID)), Ref: r.Repository + ":" + string(src.UID),
		UID: string(src.UID), Node: src.Spec.NodeName, Since: time.Now().UTC()}, nil
}

// StoppedByShutdown says whether the kubelet stopped pod p for a graceful
// node shutdown, which the node's shim checkpointed it in, if it opted in.
func StoppedByShutdown(p *corev1.Pod) bool {
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.DisruptionTarget && c.Status == corev1.ConditionTrue &&
			c.Reason == corev1.PodReasonTerminationByKubelet && strings.Contains(c.Message, "node shutdown") {
			return true
		}
	}
	return p.Status.Reason == "Terminated" && strings.Contains(p.Status.Message, "node shutdown")
}

// pullWhenPushed pulls the checkpoint at ref into dir on node, once it is
// there: the shut-down node's guard pushes it as soon as it can.
func pullWhenPushed(ctx context.Context, cs kubernetes.Interface, o Options, r RegistryOptions, node, ref, dir string) error {
	o.Log("pulling %s on %s once it is pushed", ref, node)
	script := fmt.Sprintf(`i=0
until %[1]s snapshot-pull%[2]s %[3]s %[4]s; do
  i=$((i+1)); [ $i -lt %[5]d ] || exit 1
  rm -rf %[4]s; sleep 2
done
echo pulled`, o.GCR, plainHTTP(r), ref, dir, int(pushWait/(2*time.Second)))
	if out, err := runJob(ctx, cs, o, node, "pull", nil, "sh", "-c", script); errors.Is(err, errNoOutput) {
		o.Log("%v", err) // it succeeded
	} else if err != nil {
		return fmt.Errorf("pull on %s: %w\n%s", node, err, lastLines(out, 5))
	}
	return nil
}

// lastLines returns s's last n lines.
func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
