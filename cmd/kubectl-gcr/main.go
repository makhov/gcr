// kubectl-gcr is gcr's kubectl plugin. Each command creates one of gcr's
// operations (gcr.dev/v1alpha1, docs/operations.md), which gcr-operator
// carries out, and waits for it:
//
//	kubectl gcr migrate [-n NAMESPACE] [-c CONTAINER] [--to NODE] [--post-copy | --through-registry] pod/NAME
//
// moves a running pod to another node: its processes carry on there,
// memory and all, after a pause of well under a second (a Migration).
//
//	kubectl gcr snapshot [-n NAMESPACE] [-c CONTAINER] [--registry REPO] [--push-secret SECRET] pod/NAME [IMAGE]
//
// pushes a snapshot of a running pod's container as an image: a pod
// that runs it carries on from where the container was (a Snapshot).
// Without an IMAGE, it goes to the registry gcr-operator keeps snapshots
// in; either way the image pushed is printed, by digest.
//
//	kubectl gcr point [-n NAMESPACE] [-c CONTAINER] pod/NAME [LABEL]
//	kubectl gcr rewind [-n NAMESPACE] [-c CONTAINER] pod/NAME POINT
//
// take a point on a container's timeline, and take it back to one (a
// TimePoint, a Rewind).
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/makhov/gcr/api/v1alpha1"
)

const usage = `usage: kubectl gcr migrate [-n NAMESPACE] [-c CONTAINER] [--to NODE] [--post-copy | --through-registry] pod/NAME
       kubectl gcr snapshot [-n NAMESPACE] [-c CONTAINER] [--registry REPO] [--push-secret SECRET] [--plain-http] pod/NAME [IMAGE]
       kubectl gcr point [-n NAMESPACE] [-c CONTAINER] pod/NAME [LABEL]
       kubectl gcr rewind [-n NAMESPACE] [-c CONTAINER] pod/NAME POINT`

// commands: each one's operation's kind, and how many arguments it takes.
var commands = map[string]struct {
	kind     string
	min, max int
}{
	"migrate":  {"Migration", 1, 1},
	"snapshot": {"Snapshot", 1, 2},
	"point":    {"TimePoint", 1, 2},
	"rewind":   {"Rewind", 2, 2},
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	cmd := os.Args[1]
	c, ok := commands[cmd]
	if !ok {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	ns := fs.String("n", "", "the pod's namespace (default: the kubeconfig context's)")
	to := fs.String("to", "", "migrate: the node to move it to (default: one the scheduler picks, not its own)")
	viaRegistry := fs.Bool("through-registry", false, "migrate: move it through a checkpoint in gcr-operator's registry, rather than node to node: seconds of downtime, but nothing needed from the source once pushed (a StatefulSet's pod always moves this way)")
	postCopy := fs.Bool("post-copy", false, "migrate: run it on the target before its memory moves: the shortest pause, but it is lost if either node, or the connection, goes before all its memory arrives")
	container := fs.String("c", "", "migrate, snapshot, point, rewind: the container (default: the pod's only one; migrate: or the one its gcr.dev/migrate-container annotation names, the others starting anew)")
	registry := fs.String("registry", "", "snapshot: put the image in this registry repository (host[:port]/path) when no IMAGE is given (default: gcr-operator's -snapshot-registry)")
	pushSecret := fs.String("push-secret", "", "snapshot: a dockerconfigjson Secret in the pod's namespace with credentials for IMAGE's registry (default: the node's)")
	plainHTTP := fs.Bool("plain-http", false, "snapshot: talk to the registry without TLS")
	timeout := fs.Duration("timeout", 10*time.Minute, "how long to wait for it")
	kubeconfig := fs.String("kubeconfig", "", "kubeconfig file (default: $KUBECONFIG, ~/.kube/config)")
	_ = fs.Parse(os.Args[2:])
	if fs.NArg() < c.min || fs.NArg() > c.max {
		fmt.Fprintln(os.Stderr, usage)
		fs.PrintDefaults()
		os.Exit(2)
	}
	name, ok := strings.CutPrefix(fs.Arg(0), "pod/")
	if !ok {
		name = fs.Arg(0)
	}

	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	rules.ExplicitPath = *kubeconfig
	cfg := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{})
	if *ns == "" {
		*ns, _, _ = cfg.Namespace()
	}
	rest, err := cfg.ClientConfig()
	if err != nil {
		fail(err)
	}
	dyn, err := dynamic.NewForConfig(rest)
	if err != nil {
		fail(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	target := v1alpha1.Target{Pod: name, Container: *container}
	meta := metav1.ObjectMeta{GenerateName: name + "-", Namespace: *ns}
	var obj any
	switch cmd {
	case "migrate":
		var mode v1alpha1.MigrationMode
		if *postCopy {
			mode = v1alpha1.PostCopy
		}
		obj = &v1alpha1.Migration{ObjectMeta: meta, Spec: v1alpha1.MigrationSpec{Pod: name, Node: *to, ThroughRegistry: *viaRegistry, Mode: mode, Container: *container}}
	case "snapshot":
		obj = &v1alpha1.Snapshot{ObjectMeta: meta, Spec: v1alpha1.SnapshotSpec{Target: target, Image: fs.Arg(1), Registry: *registry, PushSecret: *pushSecret, PlainHTTP: *plainHTTP}}
	case "point":
		obj = &v1alpha1.TimePoint{ObjectMeta: meta, Spec: v1alpha1.TimePointSpec{Target: target, Label: fs.Arg(1)}}
	case "rewind":
		point, err := strconv.Atoi(strings.TrimPrefix(fs.Arg(1), "#"))
		if err != nil {
			fail(fmt.Errorf("bad point %q", fs.Arg(1)))
		}
		obj = &v1alpha1.Rewind{ObjectMeta: meta, Spec: v1alpha1.RewindSpec{Target: target, Point: point}}
	}
	u, err := v1alpha1.ToUnstructured(obj)
	if err != nil {
		fail(err)
	}
	u.SetAPIVersion(v1alpha1.Group + "/" + v1alpha1.Version)
	u.SetKind(c.kind)
	gvr := v1alpha1.Resource(c.kind)
	r := dyn.Resource(gvr).Namespace(*ns)
	if u, err = r.Create(ctx, u, metav1.CreateOptions{}); err != nil {
		if apierrors.IsNotFound(err) {
			err = fmt.Errorf("%w: gcr.dev's resources are not installed: install gcr-operator (docs/operator.md)", err)
		}
		fail(err)
	}
	ref := strings.ToLower(c.kind) + "/" + u.GetName()
	fmt.Fprintf(os.Stderr, "%s created; waiting for gcr-operator\n", ref)

	var status map[string]any
	if err := wait.PollUntilContextTimeout(ctx, 500*time.Millisecond, *timeout, true, func(ctx context.Context) (bool, error) {
		cur, err := r.Get(ctx, u.GetName(), metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		status, _, _ = unstructured.NestedMap(cur.Object, "status")
		phase, _ := status["phase"].(string)
		return v1alpha1.Phase(phase).Done(), nil
	}); err != nil {
		fail(fmt.Errorf("%s: %w (it goes on: kubectl get %s)", ref, err, ref))
	}
	if status["phase"] == string(v1alpha1.Failed) {
		fail(fmt.Errorf("%s failed: %v", ref, status["message"]))
	}
	switch cmd {
	case "migrate":
		fmt.Printf("pod/%s moved to %v as pod/%v (downtime %v", name, status["targetNode"], status["target"], status["downtime"])
		if m, ok := status["memoryMoved"]; ok {
			fmt.Printf(", all its memory there by %v", m)
		}
		fmt.Println(")")
	case "snapshot":
		fmt.Println(status["image"])
	case "point":
		fmt.Printf("#%v\n", status["point"])
	case "rewind":
		fmt.Printf("pod/%s rewound to #%s\n", name, strings.TrimPrefix(fs.Arg(1), "#"))
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "kubectl gcr:", err)
	os.Exit(1)
}
