package kubemigrate

import (
	"context"
	"fmt"
	"k8s.io/apimachinery/pkg/types"
	"reflect"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func running(mod func(*corev1.Pod)) *corev1.Pod {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "default", Labels: map[string]string{"app": "x"}},
		Spec:       corev1.PodSpec{NodeName: "n1", Containers: []corev1.Container{{Name: "c", Image: "busybox"}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{{Name: "c", ContainerID: "containerd://abc"}}},
	}
	if mod != nil {
		mod(p)
	}
	return p
}

func TestMovable(t *testing.T) {
	f, yes := false, true
	for name, tc := range map[string]struct {
		pod    *corev1.Pod
		refuse string
	}{
		"a bare pod":               {running(nil), ""},
		"owned, not as controller": {running(func(p *corev1.Pod) { p.OwnerReferences = []metav1.OwnerReference{{Kind: "ReplicaSet", Name: "rs"}} }), "owned by ReplicaSet"},
		"a ReplicaSet's": {running(func(p *corev1.Pod) {
			p.OwnerReferences = []metav1.OwnerReference{{Kind: "ReplicaSet", Name: "rs", Controller: &yes}}
		}), ""},
		"a Job's": {running(func(p *corev1.Pod) {
			p.OwnerReferences = []metav1.OwnerReference{{Kind: "Job", Name: "j", Controller: &yes}}
		}), ""},
		"a StatefulSet's": {running(func(p *corev1.Pod) {
			p.OwnerReferences = []metav1.OwnerReference{{Kind: "StatefulSet", Name: "s", Controller: &yes}}
		}), "owned by StatefulSet"},
		"two containers": {running(func(p *corev1.Pod) { p.Spec.Containers = append(p.Spec.Containers, corev1.Container{Name: "d"}) }), "2 containers"},
		"hostUsers":      {running(func(p *corev1.Pod) { p.Spec.HostUsers = &f }), "hostUsers: false"},
		"emptyDir": {running(func(p *corev1.Pod) {
			p.Spec.Volumes = []corev1.Volume{{Name: "scratch", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}}
		}), ""},
		"in memory": {running(func(p *corev1.Pod) {
			p.Spec.Volumes = []corev1.Volume{{Name: "shm", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{Medium: corev1.StorageMediumMemory}}}}
		}), ""},
		"hostPath": {running(func(p *corev1.Pod) {
			p.Spec.Volumes = []corev1.Volume{{Name: "logs", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/var/log"}}}}
		}), "volume logs"},
		"projected": {running(func(p *corev1.Pod) {
			p.Spec.Volumes = []corev1.Volume{{Name: "kube-api-access", VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{}}}}
		}), ""},
		"pending": {running(func(p *corev1.Pod) { p.Status.Phase = corev1.PodPending }), "not running"},
	} {
		id, err := Movable(tc.pod)
		switch {
		case tc.refuse == "" && (err != nil || id != "abc"):
			t.Errorf("%s: %q, %v; want abc", name, id, err)
		case tc.refuse != "" && (err == nil || !strings.Contains(err.Error(), tc.refuse)):
			t.Errorf("%s: %v; want a refusal with %q", name, err, tc.refuse)
		}
	}
}

func TestTargetPod(t *testing.T) {
	src := running(func(p *corev1.Pod) { p.Annotations = map[string]string{annotationHostUsers: "false", "keep": "me"} })
	dst := targetPod(src, "", "0.0.0.0:17001", "tok", info{IDBase: 65536})
	if *dst.Spec.RuntimeClassName != RuntimeClass || dst.Spec.NodeName != "" {
		t.Fatalf("runtime class %q, node %q", *dst.Spec.RuntimeClassName, dst.Spec.NodeName)
	}
	for k, want := range map[string]string{annotationMigrateIn: "0.0.0.0:17001", annotationMigrateToken: "tok",
		annotationUsernsBase: "65536", annotationHostUsers: "false", "keep": "me"} {
		if dst.Annotations[k] != want {
			t.Errorf("annotation %s = %q, want %q", k, dst.Annotations[k], want)
		}
	}
	terms := dst.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
	if len(terms) != 1 || terms[0].MatchExpressions[0].Values[0] != "n1" || terms[0].MatchExpressions[0].Operator != corev1.NodeSelectorOpNotIn {
		t.Errorf("affinity %+v: want off n1", terms)
	}
	if src.Annotations[annotationMigrateToken] != "" || src.Spec.RuntimeClassName != nil {
		t.Error("the source pod was changed")
	}
	if dst := targetPod(src, "n2", "a", "t", info{}); dst.Annotations[annotationHostUsers] != "true" || dst.Spec.NodeName != "n2" {
		t.Errorf("no user namespace: host-users %q, node %q", dst.Annotations[annotationHostUsers], dst.Spec.NodeName)
	}
}

func TestHoldBack(t *testing.T) {
	dst := running(func(p *corev1.Pod) {
		p.Labels = map[string]string{"app": "x", "pod-template-hash": "abc", "tier": "web"}
	})
	sel := &metav1.LabelSelector{
		MatchLabels:      map[string]string{"app": "x", "pod-template-hash": "abc"},
		MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "tier", Operator: metav1.LabelSelectorOpExists}},
	}
	held := holdBack(dst, sel)
	if len(dst.Labels) != 0 {
		t.Errorf("target keeps %v: its ReplicaSet would see it", dst.Labels)
	}
	if len(held) != 3 || held["pod-template-hash"] != "abc" || held["tier"] != "web" {
		t.Errorf("held back %v", held)
	}
}

func TestJobImage(t *testing.T) {
	ctx := context.Background()
	if img, err := JobImage(ctx, fake.NewClientset(), "gcr-system"); err != nil || img != BareImage {
		t.Errorf("without the DaemonSet: %q, %v; want %q", img, err, BareImage)
	}
	ds := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: "gcr-installer", Namespace: "gcr-system"},
		Spec: appsv1.DaemonSetSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "install", Image: "ghcr.io/makhov/gcr:v0.1.2"}}}}}}
	if img, err := JobImage(ctx, fake.NewClientset(ds), "gcr-system"); err != nil || img != "ghcr.io/makhov/gcr:v0.1.2" {
		t.Errorf("with the DaemonSet: %q, %v", img, err)
	}
}

// TestTargetPodChain: each migration keeps the pod off the node it left,
// and only that one; a user's own node affinity stays, and the name keeps
// one suffix of ours.
func TestTargetPodChain(t *testing.T) {
	own := corev1.NodeSelectorRequirement{Key: "kubernetes.io/hostname", Operator: corev1.NodeSelectorOpNotIn, Values: []string{"bad"}}
	src := running(func(p *corev1.Pod) {
		p.Name = "web-6d4cf56db6-x7k2p"
		p.Spec.Affinity = &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
			NodeSelectorTerms: []corev1.NodeSelectorTerm{{MatchExpressions: []corev1.NodeSelectorRequirement{own}}}}}}
	})
	p := src
	for i, node := range []string{"n1", "n2", "n3"} {
		p.Spec.NodeName = node
		p.UID = types.UID(fmt.Sprint("uid-", i))
		dst := targetPod(p, "", "a", "t", info{})
		exprs := dst.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms[0].MatchExpressions
		want := []corev1.NodeSelectorRequirement{own, {Key: "kubernetes.io/hostname", Operator: corev1.NodeSelectorOpNotIn, Values: []string{node}}}
		if !reflect.DeepEqual(exprs, want) {
			t.Errorf("move %d, from %s: affinity %+v, want %+v", i+1, node, exprs, want)
		}
		if dst.Labels[labelTargetOf] != string(p.UID) || dst.Labels["app"] != "x" {
			t.Errorf("move %d: labels %v, want %s=%s and the source's", i+1, dst.Labels, labelTargetOf, p.UID)
		}
		if dst.Annotations[annotationMigratedFrom] != node {
			t.Errorf("move %d: %s = %q, want %s", i+1, annotationMigratedFrom, dst.Annotations[annotationMigratedFrom], node)
		}
		if !strings.HasPrefix(dst.Name, "web-6d4cf56db6-x7k2p-") || len(dst.Name) != len("web-6d4cf56db6-x7k2p-")+6 {
			t.Errorf("move %d: name %s", i+1, dst.Name)
		}
		p = dst
	}
	// Pinned to a node, the last move adds no term of its own, and takes
	// out the one before it, leaving the user's.
	p.Spec.NodeName = "n4"
	dst := targetPod(p, "n5", "a", "t", info{})
	if exprs := dst.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms[0].MatchExpressions; !reflect.DeepEqual(exprs, []corev1.NodeSelectorRequirement{own}) {
		t.Errorf("pinned: affinity %+v, want only the user's", exprs)
	}
	if _, ok := dst.Annotations[annotationMigratedFrom]; ok {
		t.Error("pinned: still marked as placed by the scheduler")
	}
	// Without a user's term, nothing is left behind: an empty term would
	// match no node.
	bare := running(func(p *corev1.Pod) { p.Spec.NodeName = "n1" })
	moved := targetPod(bare, "", "a", "t", info{})
	moved.Spec.NodeName = "n2"
	if again := targetPod(moved, "n3", "a", "t", info{}); again.Spec.Affinity != nil {
		t.Errorf("pinned move of a pod with no affinity of its own: %+v", again.Spec.Affinity)
	}
}
