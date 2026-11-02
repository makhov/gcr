package kubemigrate

import (
	"context"
	"errors"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// SnapshotImage snapshots a container of pod ns/name on its node, and
// pushes it to ref as an image that runs as any other, restoring the
// container's processes (gcr snapshot-image). The pod keeps running.
// container names the container, which may be "" in a pod with one. It
// returns the image by digest, or ref if the Job's log was lost.
func SnapshotImage(ctx context.Context, cs kubernetes.Interface, ns, name, container, ref string, plainHTTP bool, o Options) (string, error) {
	if o.GCR == "" {
		o.GCR = "/usr/local/bin/gcr"
	}
	log := o.Log
	if log == nil {
		log = func(string, ...any) {}
	}
	p, err := cs.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	if p.Status.Phase != corev1.PodRunning || p.Spec.NodeName == "" {
		return "", fmt.Errorf("pod %s is %s, not running", name, p.Status.Phase)
	}
	if container == "" {
		if len(p.Spec.Containers) != 1 {
			return "", fmt.Errorf("pod %s has %d containers: say which", name, len(p.Spec.Containers))
		}
		container = p.Spec.Containers[0].Name
	}
	var id string
	for _, s := range p.Status.ContainerStatuses {
		if s.Name != container {
			continue
		}
		if s.State.Running == nil {
			return "", fmt.Errorf("container %s is not running", container)
		}
		var ok bool
		if id, ok = strings.CutPrefix(s.ContainerID, "containerd://"); !ok {
			return "", fmt.Errorf("container %q is not containerd's", s.ContainerID)
		}
	}
	if id == "" {
		return "", fmt.Errorf("pod %s has no container %s", name, container)
	}
	args := []string{o.GCR, "-n", containerdNamespace, "snapshot-image"}
	if plainHTTP {
		args = append(args, "-plain-http")
	}
	args = append(args, id, ref)
	log("snapshotting %s/%s on %s, into %s", name, container, p.Spec.NodeName, ref)
	out, err := o.run(ctx, cs, p.Spec.NodeName, "snapshot", nil, args...)
	switch {
	case errors.Is(err, errNoOutput):
		log("%v", err) // it succeeded
		return ref, nil
	case err != nil:
		return "", fmt.Errorf("snapshot on %s: %w\n%s", p.Spec.NodeName, err, out)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	return lines[len(lines)-1], nil
}
