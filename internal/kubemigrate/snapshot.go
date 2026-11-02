package kubemigrate

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// SnapshotRequest is what SnapshotImage pushes, and how.
type SnapshotRequest struct {
	// Container names the container, which may be "" in a pod with one.
	Container string
	// Ref is the image's reference.
	Ref       string
	PlainHTTP bool
	// Auth is a Docker config (config.json, or a dockerconfigjson
	// Secret's) with credentials for Ref's registry. Without, the node's
	// own are used.
	Auth []byte
}

// SnapshotImage snapshots a container of pod ns/name on its node, and
// pushes it as an image that runs as any other, restoring the
// container's processes (gcr snapshot-image). The pod keeps running. It
// returns the image by digest, or the reference if the Job's log was
// lost.
func SnapshotImage(ctx context.Context, cs kubernetes.Interface, ns, name string, r SnapshotRequest, o Options) (string, error) {
	o, log := o.defaults()
	p, id, container, err := RunningContainer(ctx, cs, ns, name, r.Container)
	if err != nil {
		return "", err
	}
	args := []string{o.GCR, "-n", containerdNamespace, "snapshot-image"}
	if r.PlainHTTP {
		args = append(args, "-plain-http")
	}
	args = append(args, id, r.Ref)
	var env map[string]string
	if len(r.Auth) > 0 {
		env = map[string]string{registryAuthEnv: string(r.Auth)}
	}
	log("snapshotting %s/%s on %s, into %s", name, container, p.Spec.NodeName, r.Ref)
	out, err := o.run(ctx, cs, p.Spec.NodeName, "snapshot", env, args...)
	switch {
	case errors.Is(err, errNoOutput):
		log("%v", err) // it succeeded
		return r.Ref, nil
	case err != nil:
		return "", fmt.Errorf("snapshot on %s: %w\n%s", p.Spec.NodeName, err, out)
	}
	return lastLine(out), nil
}

// registryAuthEnv hands gcr on the node a Docker config with credentials
// (registry.AuthEnv).
const registryAuthEnv = "GCR_REGISTRY_AUTH"

// TakePoint takes a point on the timeline of a container of pod ns/name
// (gcr point), which has time travel on, and returns its number.
func TakePoint(ctx context.Context, cs kubernetes.Interface, ns, name, container, label string, o Options) (int, error) {
	o, log := o.defaults()
	p, id, container, err := RunningContainer(ctx, cs, ns, name, container)
	if err != nil {
		return 0, err
	}
	args := []string{o.GCR, "-n", containerdNamespace, "point", id}
	if label != "" {
		args = append(args, label)
	}
	log("taking a point on %s/%s's timeline, on %s", name, container, p.Spec.NodeName)
	out, err := o.run(ctx, cs, p.Spec.NodeName, "point", nil, args...)
	if err != nil {
		return 0, fmt.Errorf("point on %s: %w\n%s", p.Spec.NodeName, err, out)
	}
	n, err := strconv.Atoi(strings.TrimPrefix(lastLine(out), "#"))
	if err != nil {
		return 0, fmt.Errorf("point on %s: %q is no point", p.Spec.NodeName, lastLine(out))
	}
	return n, nil
}

// Rewind takes a container of pod ns/name back to point on its timeline
// (gcr rewind).
func Rewind(ctx context.Context, cs kubernetes.Interface, ns, name, container string, point int, o Options) error {
	o, log := o.defaults()
	p, id, container, err := RunningContainer(ctx, cs, ns, name, container)
	if err != nil {
		return err
	}
	log("rewinding %s/%s to #%d, on %s", name, container, point, p.Spec.NodeName)
	out, err := o.run(ctx, cs, p.Spec.NodeName, "rewind", nil, o.GCR, "-n", containerdNamespace, "rewind", id, strconv.Itoa(point))
	if err != nil && !errors.Is(err, errNoOutput) {
		return fmt.Errorf("rewind on %s: %w\n%s", p.Spec.NodeName, err, out)
	}
	return nil
}

// RunningContainer finds pod ns/name, running, and its container's
// containerd ID. container may be "" in a pod with one; it returns the
// container's name.
func RunningContainer(ctx context.Context, cs kubernetes.Interface, ns, name, container string) (*corev1.Pod, string, string, error) {
	p, err := cs.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, "", "", err
	}
	if p.Status.Phase != corev1.PodRunning || p.Spec.NodeName == "" {
		return nil, "", "", fmt.Errorf("pod %s is %s, not running", name, p.Status.Phase)
	}
	if container == "" {
		if len(p.Spec.Containers) != 1 {
			return nil, "", "", fmt.Errorf("pod %s has %d containers: say which", name, len(p.Spec.Containers))
		}
		container = p.Spec.Containers[0].Name
	}
	for _, s := range p.Status.ContainerStatuses {
		if s.Name != container {
			continue
		}
		if s.State.Running == nil {
			return nil, "", "", fmt.Errorf("container %s is not running", container)
		}
		id, ok := strings.CutPrefix(s.ContainerID, "containerd://")
		if !ok {
			return nil, "", "", fmt.Errorf("container %q is not containerd's", s.ContainerID)
		}
		return p, id, container, nil
	}
	return nil, "", "", fmt.Errorf("pod %s has no container %s", name, container)
}

func (o Options) defaults() (Options, func(string, ...any)) {
	if o.Timeout == 0 {
		o.Timeout = 5 * time.Minute
	}
	if o.GCR == "" {
		o.GCR = "/usr/local/bin/gcr"
	}
	log := o.Log
	if log == nil {
		log = func(string, ...any) {}
	}
	return o, log
}
