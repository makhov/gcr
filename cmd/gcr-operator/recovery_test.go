package main

import (
	"context"
	"net/url"
	"syscall"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	dynfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/makhov/gcr/api/v1alpha1"
	"github.com/makhov/gcr/internal/kubemigrate"
)

// The claim's answer lost to the API server's trouble, whether the claim
// landed or not: the operation still runs, once, rather than being taken
// for one left Running by an operator gone away.
func TestPickUpClaimAnswerLost(t *testing.T) {
	defer func(b wait.Backoff) { claimBackoff = b }(claimBackoff)
	claimBackoff = wait.Backoff{Duration: time.Millisecond, Steps: 5}
	refused := &url.Error{Op: "Put", URL: "https://10.96.0.1:443/apis/gcr.dev/v1alpha1/namespaces/default/snapshots/s/status", Err: syscall.ECONNREFUSED}
	for name, landed := range map[string]bool{"landed": true, "did not land": false} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			u := snapshotObject(t, "s", "")
			dyn := dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
				map[schema.GroupVersionResource]string{v1alpha1.Snapshots: "SnapshotList"}, u)
			first := true
			dyn.PrependReactor("update", "snapshots", func(a k8stesting.Action) (bool, runtime.Object, error) {
				if !first {
					return false, nil, nil
				}
				first = false
				if landed {
					obj := a.(k8stesting.UpdateAction).GetObject().(*unstructured.Unstructured).DeepCopy()
					if err := dyn.Tracker().Update(v1alpha1.Snapshots, obj, "default"); err != nil {
						t.Fatal(err)
					}
				}
				return true, nil, refused
			})
			o := &operator{cs: fake.NewSimpleClientset(pod(nil)), operating: map[types.UID]bool{}}
			runs := 0
			o.pickUp(ctx, dyn, operation{v1alpha1.Snapshots, func(context.Context, *unstructured.Unstructured, *corev1.Pod) (map[string]any, error) {
				runs++
				return nil, nil
			}}, u)
			o.moves.Wait()
			got, _ := dyn.Resource(v1alpha1.Snapshots).Namespace("default").Get(ctx, "s", metav1.GetOptions{})
			if phase, _, _ := unstructured.NestedString(got.Object, "status", "phase"); phase != string(v1alpha1.Succeeded) || runs != 1 {
				t.Fatalf("%s after %d runs", phase, runs)
			}
		})
	}
}

func TestReplaceDeparted(t *testing.T) {
	refusal := corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "StartError",
		Message: "failed to create shim task: this pod's container " + kubemigrate.DepartedRefusal + ": failed precondition"}}
	yes := true
	byReplicaSet := []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: "rs", UID: "rs", Controller: &yes}}
	departed := func(name string, owners []metav1.OwnerReference, conds ...corev1.PodCondition) *corev1.Pod {
		p := pod(nil)
		p.Name, p.UID, p.OwnerReferences = name, types.UID(name), owners
		p.Status.ContainerStatuses[0].LastTerminationState = refusal
		p.Status.Conditions = conds
		return p
	}
	migrating := func(ago time.Duration) corev1.PodCondition {
		return corev1.PodCondition{Type: corev1.DisruptionTarget, Status: corev1.ConditionTrue, Reason: "Migration",
			LastTransitionTime: metav1.NewTime(time.Now().Add(-ago))}
	}
	crashing := pod(nil)
	crashing.Name, crashing.OwnerReferences = "crashing", byReplicaSet
	for _, tc := range []struct {
		pod    *corev1.Pod
		moving bool
		gone   bool
	}{
		{departed("lost", byReplicaSet), false, true},
		{departed("bare", nil), false, false},                                    // nothing would replace it
		{departed("moving", byReplicaSet, migrating(time.Minute)), false, false}, // being handed over
		{departed("left", byReplicaSet, migrating(time.Hour)), false, true},      // its migrator went away
		{departed("ours", byReplicaSet), true, false},                            // this operator moves it
		{crashing, false, false},
	} {
		cs := fake.NewSimpleClientset(tc.pod)
		o := &operator{cs: cs, moving: map[string]bool{"default/" + tc.pod.Name: tc.moving}}
		o.replaceDeparted(context.Background(), tc.pod)
		_, err := cs.CoreV1().Pods("default").Get(context.Background(), tc.pod.Name, metav1.GetOptions{})
		if gone := apierrors.IsNotFound(err); gone != tc.gone {
			t.Errorf("%s: deleted %v, want %v", tc.pod.Name, gone, tc.gone)
		}
	}
}
