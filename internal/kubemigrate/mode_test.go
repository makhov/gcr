package kubemigrate

import (
	"context"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// A post-copy migration runs gcr migrate -post-copy, and reports when the
// memory arrived as well as the downtime.
func TestMigratePostCopy(t *testing.T) {
	for _, postCopy := range []bool{false, true} {
		cs := fake.NewClientset(running(nil))
		// The target is scheduled and runs as it is created.
		var waits string
		cs.PrependReactor("create", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
			p := a.(k8stesting.CreateAction).GetObject().(*corev1.Pod)
			waits = p.Annotations[AnnotationContainer]
			p.Spec.NodeName, p.Status.HostIP, p.Status.Phase = "n2", "10.0.0.2", corev1.PodRunning
			return false, nil, nil
		})
		var script string
		res, err := Migrate(context.Background(), cs, "default", "p", Options{
			Timeout:  2 * time.Second,
			PostCopy: postCopy,
			Run: func(_ context.Context, _, what string, _ map[string]string, command ...string) (string, error) {
				if what != "migrate" {
					return "{}", nil
				}
				script = command[len(command)-1]
				if postCopy {
					return "post-copy: downtime 210ms, memory fetched by 903ms\n", nil
				}
				return "pre-copy rounds: 2 (pages [65832 51])\ndowntime: 490ms\n", nil
			},
		})
		if err != nil {
			t.Fatalf("post-copy %v: %v", postCopy, err)
		}
		if got := strings.Contains(script, " migrate -token \"$TOKEN\" -post-copy "); got != postCopy {
			t.Errorf("post-copy %v: script %q", postCopy, script)
		}
		want := Result{Downtime: "490ms"}
		if postCopy {
			want = Result{Downtime: "210ms", MemoryMoved: "903ms"}
		}
		if waits != "c" {
			t.Errorf("the target's container waiting for the migration: %q, want c", waits)
		}
		if res.Downtime != want.Downtime || res.MemoryMoved != want.MemoryMoved || res.Node != "n2" {
			t.Errorf("post-copy %v: %+v", postCopy, res)
		}
	}
}

// A ReplicaSet's pod whose target never gets Ready (a readiness probe on a
// backend that is down) is handed over all the same, once the source is
// not Ready either: otherwise the target runs on without an owner while
// the ReplicaSet starts a cold pod.
func TestMigrateHandsOverNotReady(t *testing.T) {
	yes := true
	rs := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: "rs", Namespace: "default", UID: "rs-uid"},
		Spec: appsv1.ReplicaSetSpec{Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "x"}}}}
	src := running(func(p *corev1.Pod) {
		p.OwnerReferences = []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: "rs", UID: "rs-uid", Controller: &yes}}
	})
	cs := fake.NewClientset(src, rs)
	cs.PrependReactor("create", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		p := a.(k8stesting.CreateAction).GetObject().(*corev1.Pod)
		p.Spec.NodeName, p.Status.HostIP, p.Status.Phase = "n2", "10.0.0.2", corev1.PodRunning
		p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}}
		return false, nil, nil
	})
	res, err := Migrate(context.Background(), cs, "default", "p", Options{
		Timeout: time.Second,
		Run: func(_ context.Context, _, what string, _ map[string]string, _ ...string) (string, error) {
			if what == "info" {
				return "{}", nil
			}
			return "downtime: 100ms\n", nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	dst, err := cs.CoreV1().Pods("default").Get(context.Background(), res.Target, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(dst.OwnerReferences) != 1 || dst.OwnerReferences[0].Name != "rs" || dst.Labels["app"] != "x" {
		t.Errorf("the target wasn't handed over: owners %v, labels %v", dst.OwnerReferences, dst.Labels)
	}
	if _, err := cs.CoreV1().Pods("default").Get(context.Background(), "p", metav1.GetOptions{}); err == nil {
		t.Error("the source is still there")
	}
}
