package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"regexp"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/retry"

	"github.com/makhov/gcr/api/v1alpha1"
	"github.com/makhov/gcr/internal/kubemigrate"
)

// An operation is one of gcr.dev's resources (api/v1alpha1): run carries
// out the one in u, on pod, and returns what its status reports besides
// the phase.
type operation struct {
	gvr schema.GroupVersionResource
	run func(ctx context.Context, u *unstructured.Unstructured, pod *corev1.Pod) (map[string]any, error)
}

func (o *operator) operations() []operation {
	return []operation{
		{v1alpha1.Snapshots, o.snapshot},
		{v1alpha1.Migrations, o.migration},
		{v1alpha1.TimePoints, o.timePoint},
		{v1alpha1.Rewinds, o.rewind},
	}
}

// watchOperations carries out gcr.dev's resources as they are created,
// each once: it marks one Running, with an update that fails if another
// operator got there first, carries it out, and marks it Succeeded or
// Failed. One left Running by an operator that went away is Failed: it
// may have been done, or half done, and is not done again.
func (o *operator) watchOperations(ctx context.Context, dyn dynamic.Interface) {
	f := dynamicinformer.NewDynamicSharedInformerFactory(dyn, 0)
	for _, op := range o.operations() {
		if _, err := dyn.Resource(op.gvr).List(ctx, metav1.ListOptions{Limit: 1}); err != nil {
			log.Printf("%s: %v: not carrying them out (apply gcr-operator.yaml, or the chart, again)", op.gvr.GroupResource(), err)
			continue
		}
		handle := func(obj any) {
			if u, ok := obj.(*unstructured.Unstructured); ok {
				o.pickUp(ctx, dyn, op, u)
			}
		}
		_, _ = f.ForResource(op.gvr).Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
			AddFunc:    handle,
			UpdateFunc: func(_, obj any) { handle(obj) },
		})
	}
	f.Start(ctx.Done())
	log.Printf("carrying out gcr.dev's operations")
}

func (o *operator) pickUp(ctx context.Context, dyn dynamic.Interface, op operation, u *unstructured.Unstructured) {
	phase, _, _ := unstructured.NestedString(u.Object, "status", "phase")
	if v1alpha1.Phase(phase).Done() {
		return
	}
	o.mu.Lock()
	ours := o.operating[u.GetUID()]
	stopping := o.stopping
	o.mu.Unlock()
	switch {
	case ours, stopping && phase == "":
		return // done when it is back
	case phase == string(v1alpha1.Running):
		o.finish(ctx, dyn, op, u, nil, errors.New("gcr-operator restarted while it ran: it may be done, or half done; check, and create another to do it again"))
		return
	}
	// Ours from before the claim: the claim's own event finds it so.
	o.mu.Lock()
	o.operating[u.GetUID()] = true
	o.mu.Unlock()
	started := false
	defer func() {
		if !started {
			o.mu.Lock()
			delete(o.operating, u.GetUID())
			o.mu.Unlock()
		}
	}()
	pod, err := o.cs.CoreV1().Pods(u.GetNamespace()).Get(ctx, podOf(u), metav1.GetOptions{})
	if err != nil {
		o.finish(ctx, dyn, op, u, nil, err)
		return
	}
	now := metav1.Now()
	claimed := u.DeepCopy()
	_ = unstructured.SetNestedMap(claimed.Object, map[string]any{
		"phase": string(v1alpha1.Running), "node": pod.Spec.NodeName, "startTime": now.UTC().Format(time.RFC3339),
	}, "status")
	// The informer's copy's resourceVersion: an update after another
	// operator's, or an earlier event's, conflicts.
	claimed, err = claim(ctx, dyn.Resource(op.gvr).Namespace(u.GetNamespace()), op.gvr.GroupResource(), claimed)
	if err != nil {
		if !apierrors.IsConflict(err) && !apierrors.IsNotFound(err) {
			log.Printf("%s %s/%s: %v", op.gvr.Resource, u.GetNamespace(), u.GetName(), err)
		}
		return
	}
	o.mu.Lock()
	if o.stopping {
		o.mu.Unlock()
		o.finish(ctx, dyn, op, claimed, nil, errors.New("gcr-operator stopped as it started: create another"))
		return
	}
	o.moves.Add(1)
	o.mu.Unlock()
	started = true
	go func() {
		defer func() {
			o.mu.Lock()
			delete(o.operating, u.GetUID())
			o.mu.Unlock()
			o.moves.Done()
		}()
		log.Printf("%s %s/%s: on pod %s, on %s", op.gvr.Resource, u.GetNamespace(), u.GetName(), pod.Name, pod.Spec.NodeName)
		status, err := op.run(context.WithoutCancel(ctx), claimed, pod)
		o.finish(context.WithoutCancel(ctx), dyn, op, claimed, status, err)
	}()
}

// claimBackoff is how long claim tries for, over the API server's
// trouble.
var claimBackoff = wait.Backoff{Duration: 500 * time.Millisecond, Factor: 1.5, Jitter: 0.1, Steps: 10, Cap: 10 * time.Second}

// claim marks an operation Running, as claimed has it. An answer lost to
// the API server's trouble leaves unknown whether the mark landed: claim
// reads the operation back, and goes ahead if it did, or marks it again
// if nobody did. Otherwise, it would be left Running, and taken for one
// an operator gone away left half done (pickUp).
func claim(ctx context.Context, r dynamic.ResourceInterface, gr schema.GroupResource, claimed *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	start, _, _ := unstructured.NestedString(claimed.Object, "status", "startTime")
	var got *unstructured.Unstructured
	err := retry.OnError(claimBackoff, kubemigrate.Transient, func() error {
		var err error
		if got, err = r.UpdateStatus(ctx, claimed, metav1.UpdateOptions{}); err == nil || !kubemigrate.Transient(err) {
			return err
		}
		cur, gerr := r.Get(ctx, claimed.GetName(), metav1.GetOptions{})
		if gerr != nil || cur.GetUID() != claimed.GetUID() {
			return err
		}
		phase, _, _ := unstructured.NestedString(cur.Object, "status", "phase")
		at, _, _ := unstructured.NestedString(cur.Object, "status", "startTime")
		switch {
		case phase == string(v1alpha1.Running) && at == start:
			got = cur
			return nil
		case phase != "":
			return apierrors.NewConflict(gr, claimed.GetName(), errors.New("another operator claimed it"))
		}
		claimed.SetResourceVersion(cur.GetResourceVersion())
		return err
	})
	return got, err
}

// finish marks u Succeeded with status, or Failed with err.
func (o *operator) finish(ctx context.Context, dyn dynamic.Interface, op operation, u *unstructured.Unstructured, status map[string]any, err error) {
	if status == nil {
		status = map[string]any{}
	}
	status["phase"] = string(v1alpha1.Succeeded)
	status["message"] = nil
	if err != nil {
		status["phase"] = string(v1alpha1.Failed)
		status["message"] = err.Error()
		log.Printf("%s %s/%s failed: %v", op.gvr.Resource, u.GetNamespace(), u.GetName(), err)
	} else {
		log.Printf("%s %s/%s succeeded", op.gvr.Resource, u.GetNamespace(), u.GetName())
	}
	status["completionTime"] = metav1.Now().UTC().Format(time.RFC3339)
	r := dyn.Resource(op.gvr).Namespace(u.GetNamespace())
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cur, err := r.Get(ctx, u.GetName(), metav1.GetOptions{})
		if err != nil {
			return err
		}
		if cur.GetUID() != u.GetUID() {
			return apierrors.NewNotFound(op.gvr.GroupResource(), u.GetName())
		}
		st, _, _ := unstructured.NestedMap(cur.Object, "status")
		if st == nil {
			st = map[string]any{}
		}
		for k, v := range status {
			if v == nil {
				delete(st, k)
			} else {
				st[k] = v
			}
		}
		cur.Object["status"] = st
		_, err = r.UpdateStatus(ctx, cur, metav1.UpdateOptions{})
		return err
	}); err != nil && !apierrors.IsNotFound(err) {
		log.Printf("%s %s/%s: status: %v", op.gvr.Resource, u.GetNamespace(), u.GetName(), err)
	}
}

func podOf(u *unstructured.Unstructured) string {
	pod, _, _ := unstructured.NestedString(u.Object, "spec", "pod")
	return pod
}

func (o *operator) logFor(u *unstructured.Unstructured) kubemigrate.Options {
	opts := o.opts
	prefix := fmt.Sprintf("%s %s/%s: ", u.GetKind(), u.GetNamespace(), u.GetName())
	opts.Log = func(format string, args ...any) { log.Printf(prefix+format, args...) }
	return opts
}

// imageTag is what a Snapshot's name has to look like to be the tag of
// the image it pushes: the registries' own rule.
var imageTag = regexp.MustCompile(`^[a-zA-Z0-9_][a-zA-Z0-9._-]{0,127}$`)

// snapshotRef is the image s pushes to, and whether that is the registry
// gcr-operator keeps snapshots in (its -snapshot-registry, usual) rather
// than one s names itself. A Snapshot that gives no image goes to the
// repository it names (spec.registry) or else to that one, as
// NAMESPACE/POD tagged with the Snapshot's own name.
func snapshotRef(s *v1alpha1.Snapshot, usual string) (ref string, ours bool, err error) {
	if s.Spec.Image != "" {
		return s.Spec.Image, false, nil
	}
	repo, ours := s.Spec.Registry, false
	if repo == "" {
		repo, ours = usual, true
	}
	if repo == "" {
		return "", false, errors.New("no image, and gcr-operator keeps snapshots in no registry: give spec.image or spec.registry, or run the operator with -snapshot-registry")
	}
	if !imageTag.MatchString(s.Name) {
		return "", false, fmt.Errorf("the name %q cannot be an image tag: give spec.image", s.Name)
	}
	return fmt.Sprintf("%s/%s/%s:%s", strings.TrimSuffix(repo, "/"), s.Namespace, s.Spec.Pod, s.Name), ours, nil
}

func (o *operator) snapshot(ctx context.Context, u *unstructured.Unstructured, pod *corev1.Pod) (map[string]any, error) {
	var s v1alpha1.Snapshot
	if err := v1alpha1.FromUnstructured(u, &s); err != nil {
		return nil, err
	}
	ref, ours, err := snapshotRef(&s, o.snapshotRegistry.Repository)
	if err != nil {
		return nil, err
	}
	r := kubemigrate.SnapshotRequest{Container: s.Spec.Container, Ref: ref,
		PlainHTTP: s.Spec.PlainHTTP || (ours && o.snapshotRegistry.PlainHTTP)}
	if s.Spec.PushSecret != "" {
		sec, err := o.cs.CoreV1().Secrets(s.Namespace).Get(ctx, s.Spec.PushSecret, metav1.GetOptions{})
		if err != nil {
			return nil, fmt.Errorf("pushSecret: %w", err)
		}
		if sec.Type != corev1.SecretTypeDockerConfigJson {
			return nil, fmt.Errorf("pushSecret %s is %s, not %s", sec.Name, sec.Type, corev1.SecretTypeDockerConfigJson)
		}
		r.Auth = sec.Data[corev1.DockerConfigJsonKey]
	}
	img, err := kubemigrate.SnapshotImage(ctx, o.cs, s.Namespace, pod.Name, r, o.logFor(u))
	if err != nil {
		return nil, err
	}
	return map[string]any{"image": img}, nil
}

func (o *operator) migration(ctx context.Context, u *unstructured.Unstructured, pod *corev1.Pod) (map[string]any, error) {
	var m v1alpha1.Migration
	if err := v1alpha1.FromUnstructured(u, &m); err != nil {
		return nil, err
	}
	viaRegistry := m.Spec.ThroughRegistry || o.throughRegistry(pod)
	if viaRegistry && o.registry.Repository == "" {
		return nil, errors.New("through a registry, and gcr-operator has none (-registry)")
	}
	postCopy := m.Spec.Mode == v1alpha1.PostCopy
	if postCopy && viaRegistry {
		return nil, errors.New("post-copy, and its pod moves through a registry (a StatefulSet's always does)")
	}
	key := pod.Namespace + "/" + pod.Name
	// A drain's eviction of it meanwhile waits, as for the moves it starts.
	o.mu.Lock()
	if o.moving[key] {
		o.mu.Unlock()
		return nil, errors.New("it is moving already")
	}
	o.moving[key] = true
	o.mu.Unlock()
	defer func() {
		o.mu.Lock()
		delete(o.moving, key)
		o.mu.Unlock()
	}()
	res, err := o.migrate(ctx, pod, m.Spec.Node, viaRegistry, postCopy)
	if err != nil {
		return nil, err
	}
	status := map[string]any{"targetNode": res.Node, "target": res.Target, "downtime": res.Downtime}
	if res.MemoryMoved != "" {
		status["memoryMoved"] = res.MemoryMoved
	}
	return status, nil
}

func (o *operator) timePoint(ctx context.Context, u *unstructured.Unstructured, pod *corev1.Pod) (map[string]any, error) {
	var p v1alpha1.TimePoint
	if err := v1alpha1.FromUnstructured(u, &p); err != nil {
		return nil, err
	}
	n, err := kubemigrate.TakePoint(ctx, o.cs, p.Namespace, pod.Name, p.Spec.Container, p.Spec.Label, o.logFor(u))
	if err != nil {
		return nil, err
	}
	return map[string]any{"point": int64(n)}, nil
}

func (o *operator) rewind(ctx context.Context, u *unstructured.Unstructured, pod *corev1.Pod) (map[string]any, error) {
	var r v1alpha1.Rewind
	if err := v1alpha1.FromUnstructured(u, &r); err != nil {
		return nil, err
	}
	return nil, kubemigrate.Rewind(ctx, o.cs, r.Namespace, pod.Name, r.Spec.Container, r.Spec.Point, o.logFor(u))
}
