package kubemigrate

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestMigratedContainer(t *testing.T) {
	always := corev1.ContainerRestartPolicyAlways
	yes := true
	// app and a log shipper beside it, each started (running gives app's
	// status as c's).
	two := func(mod func(*corev1.Pod)) *corev1.Pod {
		return running(func(p *corev1.Pod) {
			p.Spec.Containers = []corev1.Container{{Name: "app"}, {Name: "shipper"}}
			p.Status.ContainerStatuses = []corev1.ContainerStatus{
				{Name: "app", ContainerID: "containerd://app-id"}, {Name: "shipper", ContainerID: "containerd://shipper-id"}}
			if mod != nil {
				mod(p)
			}
		})
	}
	annotated := func(p *corev1.Pod) { p.Annotations = map[string]string{AnnotationContainer: "app"} }
	for name, tc := range map[string]struct {
		pod       *corev1.Pod
		container string // the Migration's
		id        string
		refuse    string
	}{
		"one container":              {running(nil), "", "abc", ""},
		"two, none named":            {two(nil), "", "", "2 containers: name the one"},
		"two, by annotation":         {two(annotated), "", "app-id", ""},
		"two, by the Migration":      {two(nil), "shipper", "shipper-id", ""},
		"the Migration over the pod": {two(annotated), "shipper", "shipper-id", ""},
		"no such container":          {two(nil), "db", "", "no container db"},
		"not started":                {two(func(p *corev1.Pod) { annotated(p); p.Status.ContainerStatuses = p.Status.ContainerStatuses[1:] }), "", "", "has not started"},
		"a sidecar, none named": {running(func(p *corev1.Pod) {
			p.Spec.InitContainers = []corev1.Container{{Name: "proxy", RestartPolicy: &always}}
		}), "", "", "2 containers"},
		"a sidecar, app named": {running(func(p *corev1.Pod) {
			p.Spec.InitContainers = []corev1.Container{{Name: "proxy", RestartPolicy: &always}}
			p.Annotations = map[string]string{AnnotationContainer: "c"}
		}), "", "abc", ""},
		"a sidecar can't be the one": {running(func(p *corev1.Pod) {
			p.Spec.InitContainers = []corev1.Container{{Name: "proxy", RestartPolicy: &always}}
		}), "proxy", "", "no container proxy"},
		"an init container is no second": {running(func(p *corev1.Pod) {
			p.Spec.InitContainers = []corev1.Container{{Name: "setup"}}
		}), "", "abc", ""},
		"a shared PID namespace": {two(func(p *corev1.Pod) { annotated(p); p.Spec.ShareProcessNamespace = &yes }), "", "", "shareProcessNamespace"},
	} {
		t.Run(name, func(t *testing.T) {
			id, err := Movable(tc.pod, tc.container)
			switch {
			case tc.refuse == "" && (err != nil || id != tc.id):
				t.Fatalf("%q, %v; want %q", id, err, tc.id)
			case tc.refuse != "" && (err == nil || !strings.Contains(err.Error(), tc.refuse)):
				t.Fatalf("%q, %v; want a refusal with %q", id, err, tc.refuse)
			}
		})
	}
}
