package main

import (
	"context"
	"strings"
	"testing"
	"time"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func pod(annotations map[string]string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "default", Annotations: annotations},
		Spec:       corev1.PodSpec{NodeName: "n1", Containers: []corev1.Container{{Name: "c"}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{{Name: "c", ContainerID: "containerd://abc"}}},
	}
}

func TestDecide(t *testing.T) {
	stale := "migrating " + time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	fresh := "migrating " + time.Now().UTC().Format(time.RFC3339)
	for name, tc := range map[string]struct {
		annotations map[string]string
		allowed     bool
		started     bool
		state       string // the annotation after, "" for unchanged
	}{
		"not opted in":          {nil, true, false, ""},
		"opted in":              {map[string]string{annotationOptIn: "true"}, false, true, "migrating"},
		"moving":                {map[string]string{annotationOptIn: "true", annotationState: fresh}, false, false, ""},
		"left by another":       {map[string]string{annotationOptIn: "true", annotationState: stale}, true, false, "failed"},
		"failed: evicted":       {map[string]string{annotationOptIn: "true", annotationState: "failed: x"}, true, false, ""},
		"opted in, not movable": {map[string]string{annotationOptIn: "true", "unused": ""}, true, false, "failed"},
	} {
		t.Run(name, func(t *testing.T) {
			p := pod(tc.annotations)
			if name == "opted in, not movable" {
				p.Spec.Volumes = []corev1.Volume{{Name: "logs", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/var/log"}}}}
			}
			cs := fake.NewSimpleClientset(p)
			started := false
			o := &operator{cs: cs, moving: map[string]bool{}, start: func(*corev1.Pod) { started = true }}
			resp := o.decide(context.Background(), &admissionv1.AdmissionRequest{Name: "p", Namespace: "default"})
			if resp.Allowed != tc.allowed || started != tc.started {
				t.Fatalf("allowed %v, started %v; want %v, %v", resp.Allowed, started, tc.allowed, tc.started)
			}
			if !resp.Allowed && resp.Result.Code != 429 {
				t.Errorf("refused with %d, want 429 (drain retries it)", resp.Result.Code)
			}
			got, _ := cs.CoreV1().Pods("default").Get(context.Background(), "p", metav1.GetOptions{})
			if tc.state != "" && !strings.HasPrefix(got.Annotations[annotationState], tc.state) {
				t.Errorf("state %q, want %s…", got.Annotations[annotationState], tc.state)
			}
		})
	}
}

func TestDecideDryRun(t *testing.T) {
	dry := true
	o := &operator{cs: fake.NewSimpleClientset(pod(map[string]string{annotationOptIn: "true"})), moving: map[string]bool{},
		start: func(*corev1.Pod) { t.Fatal("a dry run started a migration") }}
	if resp := o.decide(context.Background(), &admissionv1.AdmissionRequest{Name: "p", Namespace: "default", DryRun: &dry}); !resp.Allowed {
		t.Fatal("dry run refused")
	}
}

// Draining the operator's node waits for the pods it is moving.
func TestDecideSelf(t *testing.T) {
	o := &operator{cs: fake.NewSimpleClientset(), moving: map[string]bool{}, self: metav1.ObjectMeta{Namespace: "gcr-system", Name: "op"}}
	req := &admissionv1.AdmissionRequest{Name: "op", Namespace: "gcr-system"}
	if !o.decide(context.Background(), req).Allowed {
		t.Fatal("refused while moving nothing")
	}
	o.moving["default/p"] = true
	if resp := o.decide(context.Background(), req); resp.Allowed || resp.Result.Code != 429 {
		t.Fatalf("allowed while moving a pod: %+v", resp)
	}
}
