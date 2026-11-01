package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/makhov/gcr/internal/kubemigrate"
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

// TestDecideHandover: a pod another operator was moving is taken over
// once that operator is gone, and left to it while it is not; a stopping
// operator starts nothing; its own eviction waits for the pods on its node
// that would move.
func TestDecideHandover(t *testing.T) {
	fresh := time.Now().UTC().Format(time.RFC3339)
	operatorPod := func(name string, phase corev1.PodPhase, deleting bool) *corev1.Pod {
		p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "gcr-system"}, Status: corev1.PodStatus{Phase: phase}}
		if deleting {
			p.DeletionTimestamp = &metav1.Time{Time: time.Now()}
		}
		return p
	}
	self := metav1.ObjectMeta{Namespace: "gcr-system", Name: "op-new"}
	req := &admissionv1.AdmissionRequest{Name: "p", Namespace: "default"}
	for name, tc := range map[string]struct {
		others  []*corev1.Pod
		started bool
	}{
		"its operator gone":        {nil, true},
		"its operator running":     {[]*corev1.Pod{operatorPod("op-old", corev1.PodRunning, false)}, false},
		"its operator terminating": {[]*corev1.Pod{operatorPod("op-old", corev1.PodRunning, true)}, false},
		"its operator finished":    {[]*corev1.Pod{operatorPod("op-old", corev1.PodFailed, false)}, true},
	} {
		t.Run(name, func(t *testing.T) {
			objs := []runtime.Object{pod(map[string]string{annotationOptIn: "true", annotationState: "migrating " + fresh + " by op-old"})}
			for _, p := range tc.others {
				objs = append(objs, p)
			}
			cs := fake.NewSimpleClientset(objs...)
			started := false
			o := &operator{cs: cs, moving: map[string]bool{}, self: self, start: func(*corev1.Pod) { started = true }}
			resp := o.decide(context.Background(), req)
			if started != tc.started || resp.Allowed {
				t.Fatalf("started %v, allowed %v; want started %v, refused", started, resp.Allowed, tc.started)
			}
			got, _ := cs.CoreV1().Pods("default").Get(context.Background(), "p", metav1.GetOptions{})
			if s := got.Annotations[annotationState]; tc.started && !strings.HasSuffix(s, " by op-new") {
				t.Errorf("taken over, but marked %q", s)
			}
		})
	}

	t.Run("stopping", func(t *testing.T) {
		started := false
		o := &operator{cs: fake.NewSimpleClientset(pod(map[string]string{annotationOptIn: "true"})), moving: map[string]bool{},
			self: self, stopping: true, start: func(*corev1.Pod) { started = true }}
		if resp := o.decide(context.Background(), req); resp.Allowed || started || resp.Result.Code != 429 {
			t.Fatalf("allowed %v, started %v: a stopping operator must turn it away for its successor", resp.Allowed, started)
		}
	})

	t.Run("its own node", func(t *testing.T) {
		mine := pod(map[string]string{annotationOptIn: "true"})
		cs := fake.NewSimpleClientset(mine)
		o := &operator{cs: cs, moving: map[string]bool{}, self: self, node: "n1"}
		own := &admissionv1.AdmissionRequest{Name: self.Name, Namespace: self.Namespace}
		if resp := o.decide(context.Background(), own); resp.Allowed {
			t.Fatal("evicted before the pod on its node moved")
		}
		mine.Annotations[annotationState] = "failed: x"
		if _, err := cs.CoreV1().Pods("default").Update(context.Background(), mine, metav1.UpdateOptions{}); err != nil {
			t.Fatal(err)
		}
		if resp := o.decide(context.Background(), own); !resp.Allowed {
			t.Fatalf("refused once the pod on its node could not move: %s", resp.Result.Message)
		}
	})
}

// A StatefulSet's pod moves through the registry, if there is one; once
// its StatefulSet has created it again, restored, drain's retried
// evictions are told the pod they were for is gone.
func TestDecideStatefulSet(t *testing.T) {
	yes := true
	ready := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n2"}}
	draining := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n2"}, Spec: corev1.NodeSpec{Unschedulable: true}}
	for name, tc := range map[string]struct {
		registry  string
		restored  bool
		node      *corev1.Node
		moving    bool
		allowed   bool
		started   bool
		code      int32
		wantState string
	}{
		"no registry":               {"", false, ready, false, true, false, 0, "failed"},
		"a registry":                {"reg/x", false, ready, false, false, true, 429, "migrating"},
		"created again":             {"reg/x", true, ready, false, false, false, 404, ""},
		"created again, moving yet": {"reg/x", true, ready, true, false, false, 429, ""},
		"restored, its node drains": {"reg/x", true, draining, false, false, true, 429, "migrating"},
		// The move failed after stopping it: created again, cold.
		"created again, cold": {"reg/x", false, ready, false, false, false, 404, ""},
	} {
		t.Run(name, func(t *testing.T) {
			p := pod(map[string]string{annotationOptIn: "true"})
			p.OwnerReferences = []metav1.OwnerReference{{Kind: "StatefulSet", Name: "web", Controller: &yes}}
			p.CreationTimestamp = metav1.Now()
			if tc.restored {
				p.Annotations[kubemigrate.AnnotationRestoreToken] = "t0k"
				p.Spec.NodeName = "n2"
			}
			replaced := map[string]time.Time{}
			if name == "created again, cold" {
				p.Spec.NodeName = "n2"
				replaced["default/p"] = time.Now().Add(-time.Minute)
			}
			cs := fake.NewSimpleClientset(p, tc.node)
			started := false
			o := &operator{cs: cs, moving: map[string]bool{"default/p": tc.moving}, replaced: replaced,
				registry: kubemigrate.RegistryOptions{Repository: tc.registry}, start: func(*corev1.Pod) { started = true }}
			resp := o.decide(context.Background(), &admissionv1.AdmissionRequest{Name: "p", Namespace: "default"})
			if resp.Allowed != tc.allowed || started != tc.started {
				t.Fatalf("allowed %v, started %v; want %v, %v (%+v)", resp.Allowed, started, tc.allowed, tc.started, resp.Result)
			}
			if !resp.Allowed && resp.Result.Code != tc.code {
				t.Errorf("refused with %d, want %d", resp.Result.Code, tc.code)
			}
			got, _ := cs.CoreV1().Pods("default").Get(context.Background(), "p", metav1.GetOptions{})
			if tc.wantState != "" && !strings.HasPrefix(got.Annotations[annotationState], tc.wantState) {
				t.Errorf("state %q, want %s…", got.Annotations[annotationState], tc.wantState)
			}
		})
	}
}

// The pod a StatefulSet creates again is steered to its pending restore.
func TestSteer(t *testing.T) {
	yes := true
	ctx := context.Background()
	cs := fake.NewSimpleClientset()
	r := kubemigrate.Restore{Token: "t0k", Dir: "/var/lib/gcr/checkpoints/u1", Ref: "reg/x:u1", Since: time.Now()}
	if err := kubemigrate.RecordRestore(ctx, cs, "gcr-system", "default", "web-0", r); err != nil {
		t.Fatal(err)
	}
	review := func(name, owner string) *admissionv1.AdmissionRequest {
		p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Annotations: map[string]string{annotationOptIn: "true"},
			OwnerReferences: []metav1.OwnerReference{{Kind: owner, Name: "web", Controller: &yes}}}}
		raw, _ := json.Marshal(p)
		return &admissionv1.AdmissionRequest{Namespace: "default", Object: runtime.RawExtension{Raw: raw}}
	}
	o := &operator{cs: cs, opts: kubemigrate.Options{Namespace: "gcr-system"}, registry: kubemigrate.RegistryOptions{Repository: "reg/x"}}
	if resp := o.steer(ctx, review("web-0", "StatefulSet")); !resp.Allowed || resp.PatchType == nil || !strings.Contains(string(resp.Patch), "t0k") {
		t.Fatalf("not steered: %+v", resp)
	}
	for name, req := range map[string]*admissionv1.AdmissionRequest{
		"no restore":          review("web-1", "StatefulSet"),
		"not a StatefulSet's": review("web-0", "ReplicaSet"),
	} {
		if resp := o.steer(ctx, req); !resp.Allowed || resp.Patch != nil {
			t.Errorf("%s: %+v", name, resp)
		}
	}
	o.registry = kubemigrate.RegistryOptions{}
	if resp := o.steer(ctx, review("web-0", "StatefulSet")); resp.Patch != nil {
		t.Error("steered with no registry")
	}
}
