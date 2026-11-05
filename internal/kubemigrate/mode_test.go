package kubemigrate

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
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
		cs.PrependReactor("create", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
			p := a.(k8stesting.CreateAction).GetObject().(*corev1.Pod)
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
		if res.Downtime != want.Downtime || res.MemoryMoved != want.MemoryMoved || res.Node != "n2" {
			t.Errorf("post-copy %v: %+v", postCopy, res)
		}
	}
}
