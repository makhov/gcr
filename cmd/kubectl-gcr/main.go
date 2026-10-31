// kubectl-gcr is gcr's kubectl plugin:
//
//	kubectl gcr migrate [-n NAMESPACE] [--to NODE] pod/NAME
//
// moves a running pod to another node: its processes carry on there,
// memory and all, after a pause of well under a second (docs/design/gcr-operator.md).
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/makhov/gcr/internal/kubemigrate"
)

func main() {
	if len(os.Args) < 2 || os.Args[1] != "migrate" {
		fmt.Fprintln(os.Stderr, "usage: kubectl gcr migrate [-n NAMESPACE] [--to NODE] pod/NAME")
		os.Exit(2)
	}
	fs := flag.NewFlagSet("migrate", flag.ExitOnError)
	ns := fs.String("n", "", "the pod's namespace (default: the kubeconfig context's)")
	to := fs.String("to", "", "the node to move it to (default: one the scheduler picks, not its own)")
	system := fs.String("gcr-namespace", "gcr-system", "gcr's namespace, where the migration's Jobs run")
	image := fs.String("image", "", "the image the Jobs run (default: gcr's installer's, from its DaemonSet)")
	gcrPath := fs.String("gcr", "/usr/local/bin/gcr", "gcr's path on the nodes")
	timeout := fs.Duration("timeout", 5*time.Minute, "how long each step may take")
	kubeconfig := fs.String("kubeconfig", "", "kubeconfig file (default: $KUBECONFIG, ~/.kube/config)")
	_ = fs.Parse(os.Args[2:])
	if fs.NArg() != 1 {
		fs.Usage()
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
	cs, err := kubernetes.NewForConfig(rest)
	if err != nil {
		fail(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if *image == "" {
		ds, err := cs.AppsV1().DaemonSets(*system).Get(ctx, "gcr-installer", metav1.GetOptions{})
		if err != nil {
			fail(fmt.Errorf("gcr's installer DaemonSet (for its image; or pass -image): %w", err))
		}
		*image = ds.Spec.Template.Spec.Containers[0].Image
	}
	res, err := kubemigrate.Migrate(ctx, cs, *ns, name, kubemigrate.Options{
		Node: *to, Namespace: *system, Image: *image, GCR: *gcrPath, Timeout: *timeout,
		Log: func(format string, args ...any) { fmt.Fprintf(os.Stderr, format+"\n", args...) },
	})
	if err != nil {
		fail(err)
	}
	fmt.Printf("pod/%s moved to %s as pod/%s (downtime %s)\n", name, res.Node, res.Target, res.Downtime)
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "kubectl gcr:", err)
	os.Exit(1)
}
