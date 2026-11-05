package kubemigrate

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"syscall"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// The API server dropping the connection, or timing out, mid-wait: the
// wait goes on. An answer ends it.
func TestPollThroughTransient(t *testing.T) {
	refused := &url.Error{Op: "Get", URL: "https://10.96.0.1:443/apis/batch/v1/namespaces/gcr-system/jobs/gcr-migrate-1", Err: syscall.ECONNREFUSED}
	tries := 0
	err := poll(context.Background(), time.Millisecond, time.Second, func(context.Context) (bool, error) {
		tries++
		switch tries {
		case 1:
			return false, refused
		case 2:
			return false, apierrors.NewServerTimeout(schema.GroupResource{Resource: "jobs"}, "get", 1)
		}
		return true, nil
	})
	if err != nil || tries != 3 {
		t.Errorf("err %v after %d tries; want none, after 3", err, tries)
	}

	gone := apierrors.NewNotFound(schema.GroupResource{Resource: "jobs"}, "gcr-migrate-1")
	if err := poll(context.Background(), time.Millisecond, time.Second, func(context.Context) (bool, error) { return false, gone }); !errors.Is(err, gone) {
		t.Errorf("not found: %v", err)
	}

	err = poll(context.Background(), time.Millisecond, 20*time.Millisecond, func(context.Context) (bool, error) { return false, refused })
	if err == nil || !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("timed out on refusals: %v; want it to say the last", err)
	}
}

func TestAgain(t *testing.T) {
	tries := 0
	err := again(func() error {
		if tries++; tries < 3 {
			return apierrors.NewServiceUnavailable("overloaded")
		}
		return nil
	})
	if err != nil || tries != 3 {
		t.Errorf("err %v after %d tries", err, tries)
	}
	tries = 0
	conflict := apierrors.NewConflict(schema.GroupResource{Resource: "pods"}, "p", errors.New("changed"))
	if err := again(func() error { tries++; return conflict }); !errors.Is(err, conflict) || tries != 1 {
		t.Errorf("conflict: %v after %d tries", err, tries)
	}
}

func TestDeparted(t *testing.T) {
	refused := func(state corev1.ContainerState, last corev1.ContainerState) *corev1.Pod {
		return &corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{State: state, LastTerminationState: last}}}}
	}
	msg := "failed to create containerd task: failed to create shim task: this pod's container " + DepartedRefusal + ": failed precondition"
	startError := corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "StartError", Message: msg}}
	backOff := corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}}
	if !Departed(refused(backOff, startError)) || !Departed(refused(startError, corev1.ContainerState{})) {
		t.Error("a refused container's pod is not Departed")
	}
	crashed := corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "Error", ExitCode: 1}}
	if Departed(refused(backOff, crashed)) || Departed(&corev1.Pod{}) {
		t.Error("a crashing container's pod is Departed")
	}
}

func TestMigrating(t *testing.T) {
	marked := func(status corev1.ConditionStatus, ago time.Duration) *corev1.Pod {
		return &corev1.Pod{Status: corev1.PodStatus{Conditions: []corev1.PodCondition{{
			Type: corev1.DisruptionTarget, Status: status, Reason: "Migration",
			LastTransitionTime: metav1.NewTime(time.Now().Add(-ago)),
		}}}}
	}
	for _, c := range []struct {
		pod  *corev1.Pod
		want bool
	}{
		{marked(corev1.ConditionTrue, time.Minute), true},
		{marked(corev1.ConditionTrue, time.Hour), false},    // its migrator went away
		{marked(corev1.ConditionFalse, time.Minute), false}, // the migration failed
		{&corev1.Pod{}, false},
	} {
		if got := Migrating(c.pod, 10*time.Minute); got != c.want {
			t.Errorf("%+v: Migrating %v, want %v", c.pod.Status.Conditions, got, c.want)
		}
	}
}
