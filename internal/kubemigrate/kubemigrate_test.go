package kubemigrate

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
		}), "owned by Job"},
		"two containers": {running(func(p *corev1.Pod) { p.Spec.Containers = append(p.Spec.Containers, corev1.Container{Name: "d"}) }), "2 containers"},
		"hostUsers":      {running(func(p *corev1.Pod) { p.Spec.HostUsers = &f }), "hostUsers: false"},
		"emptyDir": {running(func(p *corev1.Pod) {
			p.Spec.Volumes = []corev1.Volume{{Name: "scratch", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}}
		}), "volume scratch"},
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
