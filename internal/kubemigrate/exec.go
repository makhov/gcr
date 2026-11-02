package kubemigrate

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/httpstream"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
)

// InstallerLabel selects the pods of gcr's installer DaemonSet (gcr.yaml),
// one on each node, privileged and in the host's PID namespace.
const InstallerLabel = "app=gcr-installer"

// ExecRunner runs commands on a node in the pod of gcr's installer
// DaemonSet there (in namespace ns), through the API server's exec, in the
// host's namespaces, rather than in a Job: a node shutting down admits no
// new pod, but the kubelet stops the installer, node-critical, only after
// the pods it moves. env goes on the command's standard input, not in the
// exec request, which the API server may log.
func ExecRunner(cfg *rest.Config, cs kubernetes.Interface, ns string) Runner {
	return func(ctx context.Context, node, what string, env map[string]string, command ...string) (string, error) {
		pod, err := installerPod(ctx, cs, ns, node)
		if err != nil {
			return "", err
		}
		// The host's sh reads KEY=VALUE lines up to an empty one, then runs
		// the command.
		args := append([]string{"nsenter", "--target", "1", "--mount", "--uts", "--ipc", "--net", "--pid", "--",
			"sh", "-c", `while IFS= read -r kv && [ -n "$kv" ]; do export "$kv"; done; exec "$@"`, "sh"}, command...)
		keys := make([]string, 0, len(env))
		for k := range env {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var vars strings.Builder
		for _, k := range keys {
			fmt.Fprintf(&vars, "%s=%s\n", k, env[k])
		}
		vars.WriteString("\n")
		// Standard input stays open until the command has ended: closed
		// at once, it can end the exec before the command's output has
		// come back.
		stdin, w := io.Pipe()
		go func() { _, _ = io.WriteString(w, vars.String()) }()
		defer w.Close()
		req := cs.CoreV1().RESTClient().Post().Resource("pods").Namespace(ns).Name(pod).SubResource("exec").
			VersionedParams(&corev1.PodExecOptions{Container: "install", Command: args, Stdin: true, Stdout: true, Stderr: true},
				scheme.ParameterCodec)
		// As kubectl execs: over WebSocket, or SPDY where the API server
		// cannot.
		ws, err := remotecommand.NewWebSocketExecutor(cfg, "GET", req.URL().String())
		if err != nil {
			return "", err
		}
		spdy, err := remotecommand.NewSPDYExecutor(cfg, "POST", req.URL())
		if err != nil {
			return "", err
		}
		ex, err := remotecommand.NewFallbackExecutor(ws, spdy, func(err error) bool {
			return httpstream.IsUpgradeFailure(err) || httpstream.IsHTTPSProxyError(err)
		})
		if err != nil {
			return "", err
		}
		// Standard output and error are copied from two goroutines.
		var out lockedBuffer
		err = ex.StreamWithContext(ctx, remotecommand.StreamOptions{Stdin: stdin, Stdout: &out, Stderr: &out})
		if err != nil {
			return out.String(), fmt.Errorf("%s in %s on %s: %w", what, pod, node, err)
		}
		return out.String(), nil
	}
}

// lockedBuffer is a bytes.Buffer two goroutines may write.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// installerPod is the running pod of gcr's installer on node.
func installerPod(ctx context.Context, cs kubernetes.Interface, ns, node string) (string, error) {
	pods, err := cs.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: InstallerLabel, FieldSelector: "spec.nodeName=" + node})
	if err != nil {
		return "", err
	}
	for _, p := range pods.Items {
		if p.Status.Phase == corev1.PodRunning && p.DeletionTimestamp == nil {
			return p.Name, nil
		}
	}
	return "", fmt.Errorf("no pod of gcr's installer (%s) runs on %s", InstallerLabel, node)
}

// HasInstaller says whether gcr's installer runs on node, for ExecRunner.
func HasInstaller(ctx context.Context, cs kubernetes.Interface, ns, node string) bool {
	_, err := installerPod(ctx, cs, ns, node)
	return err == nil
}
