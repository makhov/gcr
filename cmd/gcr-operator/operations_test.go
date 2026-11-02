package main

import (
	"context"
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	dynfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/makhov/gcr/api/v1alpha1"
)

func snapshotObject(t *testing.T, name, phase string) *unstructured.Unstructured {
	u, err := v1alpha1.ToUnstructured(&v1alpha1.Snapshot{
		TypeMeta:   metav1.TypeMeta{APIVersion: v1alpha1.Group + "/" + v1alpha1.Version, Kind: "Snapshot"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", UID: types.UID(name)},
		Spec:       v1alpha1.SnapshotSpec{Target: v1alpha1.Target{Pod: "p"}, Image: "registry.example/snap:v1"},
		Status:     v1alpha1.SnapshotStatus{Status: v1alpha1.Status{Phase: v1alpha1.Phase(phase)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestPickUp(t *testing.T) {
	for name, tc := range map[string]struct {
		phase   string
		pod     bool
		runErr  error
		ran     bool
		want    v1alpha1.Phase
		message string
	}{
		"new":               {"", true, nil, true, v1alpha1.Succeeded, ""},
		"fails":             {"", true, errors.New("no space"), true, v1alpha1.Failed, "no space"},
		"no pod":            {"", false, nil, false, v1alpha1.Failed, `pods "p" not found`},
		"left running":      {"Running", true, nil, false, v1alpha1.Failed, "gcr-operator restarted while it ran: it may be done, or half done; check, and create another to do it again"},
		"succeeded already": {"Succeeded", true, nil, false, v1alpha1.Succeeded, ""},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			u := snapshotObject(t, "s", tc.phase)
			dyn := dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
				map[schema.GroupVersionResource]string{v1alpha1.Snapshots: "SnapshotList"}, u)
			var objs []runtime.Object
			if tc.pod {
				objs = append(objs, pod(nil))
			}
			o := &operator{cs: fake.NewSimpleClientset(objs...), operating: map[types.UID]bool{}}
			ran := false
			op := operation{v1alpha1.Snapshots, func(_ context.Context, _ *unstructured.Unstructured, p *corev1.Pod) (map[string]any, error) {
				ran = true
				if p.Spec.NodeName != "n1" {
					t.Errorf("pod on %q", p.Spec.NodeName)
				}
				return map[string]any{"image": "registry.example/snap@sha256:1"}, tc.runErr
			}}
			o.pickUp(ctx, dyn, op, u)
			o.moves.Wait()
			got, err := dyn.Resource(v1alpha1.Snapshots).Namespace("default").Get(ctx, "s", metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			var s v1alpha1.Snapshot
			if err := v1alpha1.FromUnstructured(got, &s); err != nil {
				t.Fatal(err)
			}
			if ran != tc.ran || s.Status.Phase != tc.want || s.Status.Message != tc.message {
				t.Fatalf("ran %v, %s %q; want %v, %s %q", ran, s.Status.Phase, s.Status.Message, tc.ran, tc.want, tc.message)
			}
			if tc.want == v1alpha1.Succeeded && tc.ran && (s.Status.Image != "registry.example/snap@sha256:1" || s.Status.Node != "n1" ||
				s.Status.StartTime == nil || s.Status.CompletionTime == nil) {
				t.Errorf("status %+v", s.Status)
			}
			if len(o.operating) != 0 {
				t.Errorf("still operating: %v", o.operating)
			}
		})
	}
}

// One being carried out is left alone as its own events come in.
func TestPickUpOurs(t *testing.T) {
	ctx := context.Background()
	u := snapshotObject(t, "s", "")
	dyn := dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{v1alpha1.Snapshots: "SnapshotList"}, u)
	o := &operator{cs: fake.NewSimpleClientset(pod(nil)), operating: map[types.UID]bool{}}
	release := make(chan struct{})
	runs := 0
	op := operation{v1alpha1.Snapshots, func(context.Context, *unstructured.Unstructured, *corev1.Pod) (map[string]any, error) {
		runs++
		<-release
		return nil, nil
	}}
	o.pickUp(ctx, dyn, op, u)
	var running *unstructured.Unstructured
	if err := wait.PollUntilContextTimeout(ctx, 10*time.Millisecond, 5*time.Second, true, func(ctx context.Context) (bool, error) {
		var err error
		running, err = dyn.Resource(v1alpha1.Snapshots).Namespace("default").Get(ctx, "s", metav1.GetOptions{})
		phase, _, _ := unstructured.NestedString(running.Object, "status", "phase")
		return phase == string(v1alpha1.Running), err
	}); err != nil {
		t.Fatal(err)
	}
	o.pickUp(ctx, dyn, op, running) // the claim's event
	close(release)
	o.moves.Wait()
	got, _ := dyn.Resource(v1alpha1.Snapshots).Namespace("default").Get(ctx, "s", metav1.GetOptions{})
	if phase, _, _ := unstructured.NestedString(got.Object, "status", "phase"); phase != string(v1alpha1.Succeeded) || runs != 1 {
		t.Fatalf("%s after %d runs", phase, runs)
	}
}

// Stopping, it starts none: the next operator does.
func TestPickUpStopping(t *testing.T) {
	ctx := context.Background()
	u := snapshotObject(t, "s", "")
	dyn := dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{v1alpha1.Snapshots: "SnapshotList"}, u)
	o := &operator{cs: fake.NewSimpleClientset(pod(nil)), operating: map[types.UID]bool{}, stopping: true}
	o.pickUp(ctx, dyn, operation{v1alpha1.Snapshots, func(context.Context, *unstructured.Unstructured, *corev1.Pod) (map[string]any, error) {
		t.Fatal("ran")
		return nil, nil
	}}, u)
	got, _ := dyn.Resource(v1alpha1.Snapshots).Namespace("default").Get(ctx, "s", metav1.GetOptions{})
	if phase, _, _ := unstructured.NestedString(got.Object, "status", "phase"); phase != "" {
		t.Fatalf("phase %s", phase)
	}
}
