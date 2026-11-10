// gcr-operator turns evictions into migrations: a pod that opts in with
// gcr.dev/migrate-on-evict=true moves to another node, its processes and
// memory carrying on there, when kubectl drain, the cluster autoscaler or
// a node upgrade evicts it (docs/design/gcr-operator.md).
//
// It serves a validating webhook on pods/eviction. An opted-in pod's
// eviction is answered 429, as a PodDisruptionBudget does, while the pod
// moves; evictions are retried, and once the pod has moved its source is
// gone. If the move fails, the next eviction goes through: a drain is
// never held by a pod that cannot move.
//
// With -registry, a StatefulSet's pod moves through a checkpoint there:
// it is stopped at a checkpoint and deleted, and a mutating webhook on pod
// creation steers the pod its StatefulSet creates again to restore from it.
// And pods on a node shutting down, which the node's gcr shutdown-guard
// checkpoints to the registry, are restored elsewhere: a StatefulSet's in
// the pod it creates again, a bare pod in a replacement.
//
// And it carries out gcr.dev's operations on pods: Snapshots, Migrations,
// TimePoints and Rewinds (api/v1alpha1, docs/operations.md). With
// -snapshot-registry, a Snapshot that names no image puts one there, as
// NAMESPACE/POD tagged with the Snapshot's name, and reports it in
// status.image.
package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"log"
	"math/big"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"

	"github.com/makhov/gcr/internal/kubemigrate"
)

const (
	// annotationOptIn on a pod (from its template) asks for a migration
	// instead of an eviction.
	annotationOptIn = "gcr.dev/migrate-on-evict"
	// staleAfter is when a pod's mark of a migration under way, left by a
	// migrator that went away, is taken for failed: longer than a
	// migration's waits (kubemigrate.Options.Timeout), together.
	staleAfter = 15 * time.Minute
)

func main() {
	listen := flag.String("listen", ":8443", "address the webhook listens on (TLS)")
	namespace := flag.String("namespace", envOr("POD_NAMESPACE", "gcr-system"), "gcr's namespace: its Service, and the migrations' Jobs")
	service := flag.String("service", "gcr-operator", "the webhook's Service, which its certificate names")
	webhook := flag.String("webhook", "gcr-operator", "the ValidatingWebhookConfiguration and the MutatingWebhookConfiguration to give the certificate's CA")
	image := flag.String("image", "", "the image migrations' Jobs run (default: gcr's installer's, or busybox without its DaemonSet)")
	gcrPath := flag.String("gcr", "/usr/local/bin/gcr", "gcr's path on the nodes")
	registry := flag.String("registry", "", "move StatefulSets' pods through checkpoints in this registry repository (host[:port]/path), which the nodes reach")
	plainHTTP := flag.Bool("plain-http", false, "talk to -registry without TLS")
	snapshotRegistry := flag.String("snapshot-registry", "", "keep Snapshots' images in this registry repository (host[:port]/path) when they name no image of their own")
	snapshotPlainHTTP := flag.Bool("snapshot-plain-http", false, "talk to -snapshot-registry without TLS")
	flag.Parse()

	cfg, err := rest.InClusterConfig()
	if err != nil {
		log.Fatal(err)
	}
	// client-go's default, 5 a second, queues the webhooks' calls behind
	// the moves' when a node's pods all move at once, past the webhooks'
	// timeout.
	cfg.QPS, cfg.Burst = 50, 100
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		log.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if *image == "" {
		if *image, err = kubemigrate.JobImage(ctx, cs, *namespace); err != nil {
			log.Fatalf("%v (or pass -image)", err)
		}
	}
	cert, ca, err := selfSigned(*service + "." + *namespace + ".svc")
	if err != nil {
		log.Fatal(err)
	}
	if err := giveCA(ctx, cs, *webhook, ca); err != nil {
		log.Fatalf("webhook %s: %v", *webhook, err)
	}

	op := &operator{cs: cs, opts: kubemigrate.Options{Namespace: *namespace, Image: *image, GCR: *gcrPath}, moving: map[string]bool{}, replaced: map[string]time.Time{}, restoring: map[types.UID]*kubemigrate.Restore{}, last: map[string]lastPod{},
		owned: map[types.UID]map[types.UID]lastPod{}, nodesDown: map[string]bool{}, operating: map[types.UID]bool{},
		self: metav1.ObjectMeta{Namespace: *namespace, Name: os.Getenv("POD_NAME")}, node: os.Getenv("NODE_NAME")}
	if *registry != "" {
		if owned, err := restoresReady(ctx, cs, *webhook); err != nil {
			log.Printf("StatefulSets' pods are evicted, not moved: %v", err)
		} else {
			op.registry = kubemigrate.RegistryOptions{Repository: *registry, PlainHTTP: *plainHTTP}
			op.ownedRestores = owned
			op.execRun = kubemigrate.ExecRunner(cfg, cs, *namespace)
			log.Printf("StatefulSets' pods move through %s, and pods on nodes shutting down are restored from it", *registry)
			if !owned {
				log.Printf("Deployments' and Jobs' pods on nodes shutting down start cold: admission policy %s is older than this operator; apply gcr.yaml (or the chart) again",
					kubemigrate.RuntimeClass)
			}
			go op.watchShutdowns(ctx)
		}
	}
	if *snapshotRegistry != "" {
		op.snapshotRegistry = kubemigrate.RegistryOptions{Repository: *snapshotRegistry, PlainHTTP: *snapshotPlainHTTP}
		log.Printf("Snapshots that name no image go to %s, as NAMESPACE/POD tagged with the Snapshot's name", *snapshotRegistry)
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		log.Fatal(err)
	}
	go op.watchOperations(ctx, dyn)
	go op.watchDeparted(ctx)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /evict", review(op.decide))
	mux.HandleFunc("POST /restore", review(op.steer))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	srv := &http.Server{Addr: *listen, Handler: mux, TLSConfig: &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS13},
		ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		// Draining its own node: the migrations it started finish first,
		// or the pods are left marked as moving, with nobody moving them.
		// It starts none meanwhile; the evictions it turns away are tried
		// again and reach the operator that replaces it.
		op.stop(shutdownWait)
		_ = srv.Shutdown(context.Background())
	}()
	log.Printf("serving evictions on %s; migrations' Jobs run %s", *listen, *image)
	if err := srv.ListenAndServeTLS("", ""); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

type operator struct {
	cs   kubernetes.Interface
	opts kubemigrate.Options
	// registry is where StatefulSets' pods move through; with none, they
	// are evicted.
	registry kubemigrate.RegistryOptions
	// snapshotRegistry is where Snapshots that name no image put theirs;
	// with none, such a Snapshot fails (api/v1alpha1).
	snapshotRegistry kubemigrate.RegistryOptions

	mu     sync.Mutex
	moving map[string]bool // namespace/name of pods being moved
	// replaced is when StatefulSets' pods moved (or were deleted trying),
	// by namespace/name: those pods' StatefulSets created them again.
	replaced map[string]time.Time
	// restoring is the pods stopped at their nodes' shutdowns being
	// restored, or tried, by UID: with a StatefulSet's pod's pending
	// restore. shutdownMu serializes starting them.
	restoring  map[types.UID]*kubemigrate.Restore
	shutdownMu sync.Mutex
	// last is the last the informer saw of StatefulSets' opted-in pods,
	// by namespace/name; owned, of ReplicaSets' and Jobs', by their
	// owner's UID and theirs.
	last  map[string]lastPod
	owned map[types.UID]map[types.UID]lastPod
	// ownedRestores: the admission policy lets ReplicaSets and Jobs
	// restore their pods. claimMu serializes claiming those restores.
	ownedRestores bool
	claimMu       sync.Mutex
	// execRun runs commands on a node through gcr's installer pod there,
	// for nodes shutting down (shutdownNode), which have been, by name, in
	// nodesDown.
	execRun   kubemigrate.Runner
	nodesDown map[string]bool
	stopping  bool // on its way out: it starts no migration
	// operating is the gcr.dev operations it carries out, by UID.
	operating map[types.UID]bool
	moves     sync.WaitGroup
	// start moves a pod in the background: move, but for tests; and
	// startRestore restores one: restore.
	start        func(*corev1.Pod)
	startRestore func(*corev1.Pod, *kubemigrate.Restore)
	// self is the operator's own pod, on node.
	self metav1.ObjectMeta
	node string
}

// shutdownWait bounds how long a stopping operator waits for its
// migrations (the Deployment's terminationGracePeriodSeconds is longer).
const shutdownWait = 100 * time.Second

// stop starts no more migrations, and waits up to wait for those running.
func (o *operator) stop(wait time.Duration) {
	o.mu.Lock()
	o.stopping = true
	n := len(o.moving)
	o.mu.Unlock()
	if n > 0 {
		log.Printf("stopping: waiting for %d migrations", n)
	}
	done := make(chan struct{})
	go func() { o.moves.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(wait):
		log.Printf("stopping with migrations still running")
	}
}

// review serves admission reviews, answered by decide.
func review(decide func(context.Context, *admissionv1.AdmissionRequest) *admissionv1.AdmissionResponse) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var review admissionv1.AdmissionReview
		if err := json.NewDecoder(r.Body).Decode(&review); err != nil || review.Request == nil {
			http.Error(w, "not an AdmissionReview", http.StatusBadRequest)
			return
		}
		req := review.Request
		resp := decide(r.Context(), req)
		resp.UID = req.UID
		review.Response = resp
		review.Request = nil
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(review)
	}
}

func (o *operator) decide(ctx context.Context, req *admissionv1.AdmissionRequest) *admissionv1.AdmissionResponse {
	allow := &admissionv1.AdmissionResponse{Allowed: true}
	if req.DryRun != nil && *req.DryRun {
		return allow
	}
	if req.Namespace == o.self.Namespace && req.Name == o.self.Name {
		// Draining its own node evicts it with the pods it is moving:
		// not before it has moved them.
		o.mu.Lock()
		n := len(o.moving)
		o.mu.Unlock()
		if n > 0 {
			return tooMany(&corev1.Pod{ObjectMeta: o.self}, fmt.Sprintf("is moving %d pods to other nodes", n))
		}
		// The drain evicts them all at once: gone first, it would leave
		// their evictions to a replacement not up yet, and the webhook
		// lets an eviction through when it cannot be reached.
		if n := o.waitingOnNode(ctx); n > 0 {
			return tooMany(&corev1.Pod{ObjectMeta: o.self}, fmt.Sprintf("waits for %d pods on its node to move first", n))
		}
		return allow
	}
	pod, err := o.cs.CoreV1().Pods(req.Namespace).Get(ctx, req.Name, metav1.GetOptions{})
	if err != nil || pod.Annotations[annotationOptIn] != "true" || pod.DeletionTimestamp != nil {
		return allow
	}
	key := req.Namespace + "/" + req.Name
	state := pod.Annotations[kubemigrate.AnnotationState]
	switch {
	case strings.HasPrefix(state, "failed"):
		return allow // as without gcr: it restarts elsewhere
	case strings.HasPrefix(state, "migrating"):
		o.mu.Lock()
		ours := o.moving[key]
		o.mu.Unlock()
		since, by := parseMigrating(state)
		switch {
		case ours:
			return tooMany(pod, "is moving to another node")
		case time.Since(since) > staleAfter:
			o.mark(ctx, pod, "failed: the operator that was moving it went away")
			return allow
		case by == "" || by == o.self.Name || !o.gone(ctx, by):
			return tooMany(pod, "is moving to another node")
		}
		// Its operator is gone (evicted with the pods it was moving):
		// this one moves it.
		log.Printf("%s: operator %s, which was moving it, is gone: moving it", key, by)
	}
	o.mu.Lock()
	stopping, busy := o.stopping, o.moving[key]
	o.mu.Unlock()
	if !busy && o.recreated(ctx, pod) {
		// kubectl drain evicts a pod by name, and goes on trying: it is
		// the pod it was evicting, the one it waits to be gone, that is.
		return &admissionv1.AdmissionResponse{Allowed: false, Result: &metav1.Status{
			Code: http.StatusNotFound, Reason: metav1.StatusReasonNotFound,
			Message: fmt.Sprintf("pod %s/%s moved to %s already: the pod evicted is gone", pod.Namespace, pod.Name, pod.Spec.NodeName),
		}}
	}
	if busy {
		// A StatefulSet's pod being created again, by the same name.
		return tooMany(pod, "is moving to another node")
	}
	if stopping {
		return tooMany(pod, "waits for the operator, which is restarting")
	}
	if _, err := o.movable(pod); err != nil {
		o.mark(ctx, pod, "failed: "+err.Error())
		log.Printf("%s cannot move, evicting it: %v", key, err)
		return allow
	}
	o.mu.Lock()
	if o.moving[key] {
		o.mu.Unlock()
		return tooMany(pod, "is moving to another node")
	}
	o.moving[key] = true
	o.moves.Add(1)
	o.mu.Unlock()
	o.mark(ctx, pod, "migrating "+time.Now().UTC().Format(time.RFC3339)+" by "+o.self.Name)
	if o.start != nil {
		o.start(pod)
		o.moves.Done()
	} else {
		go o.move(pod)
	}
	return tooMany(pod, "is moving to another node instead (gcr)")
}

// move migrates pod, and marks it failed if it cannot be; on success its
// source is gone, and the evictions retried for it find nothing to evict.
func (o *operator) move(pod *corev1.Pod) {
	key := pod.Namespace + "/" + pod.Name
	defer func() {
		o.mu.Lock()
		delete(o.moving, key)
		o.mu.Unlock()
		o.moves.Done()
	}()
	ctx := context.Background()
	// Pre-copy: a node being drained is no node to depend on until the
	// memory has moved.
	res, err := o.migrate(ctx, pod, "", "", o.throughRegistry(pod), false)
	if err != nil {
		log.Printf("%s: migration failed, letting its eviction through: %v", key, err)
		o.mark(ctx, pod, "failed: "+firstLine(err.Error()))
		return
	}
	log.Printf("%s moved to %s as %s (downtime %s)", key, res.Node, res.Target, res.Downtime)
}

// migrate moves pod to node (or one the scheduler picks), node to node or
// through the registry.
func (o *operator) migrate(ctx context.Context, pod *corev1.Pod, node, container string, viaRegistry, postCopy bool) (kubemigrate.Result, error) {
	key := pod.Namespace + "/" + pod.Name
	opts := o.opts
	opts.Node = node
	opts.Container = container
	opts.PostCopy = postCopy
	opts.Log = func(format string, args ...any) { log.Printf(key+": "+format, args...) }
	if !viaRegistry {
		return kubemigrate.Migrate(ctx, o.cs, pod.Namespace, pod.Name, opts)
	}
	begin := time.Now()
	res, err := kubemigrate.MigrateViaRegistry(ctx, o.cs, pod.Namespace, pod.Name, opts, o.registry)
	if o.throughRegistry(pod) {
		o.mu.Lock()
		o.replaced[key] = begin
		o.mu.Unlock()
	}
	return res, err
}

// waitingOnNode counts the pods on the operator's own node that would
// move when evicted and have not yet: opted in, running, movable, and not
// failed to move.
func (o *operator) waitingOnNode(ctx context.Context) int {
	if o.node == "" {
		return 0
	}
	pods, err := o.cs.CoreV1().Pods("").List(ctx, metav1.ListOptions{FieldSelector: "spec.nodeName=" + o.node})
	if err != nil {
		return 0
	}
	n := 0
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.Annotations[annotationOptIn] != "true" || p.DeletionTimestamp != nil ||
			strings.HasPrefix(p.Annotations[kubemigrate.AnnotationState], "failed") {
			continue
		}
		if _, err := o.movable(p); err == nil {
			n++
		}
	}
	return n
}

// watchShutdowns restores the opted-in pods the kubelet stops for a
// graceful node shutdown: their shims checkpoint them as it stops them,
// and the nodes' gcr shutdown-guard pushes the checkpoints. An informer
// sees the kubelet's status on the pod as it is posted; and it keeps the
// last of each StatefulSet's pod, for the webhook to steer the pod its
// StatefulSet creates again even before then (lateRestore).
func (o *operator) watchShutdowns(ctx context.Context) {
	f := informers.NewSharedInformerFactory(o.cs, 0)
	nodes := f.Core().V1().Nodes().Informer()
	_, _ = nodes.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    o.nodeSeen,
		UpdateFunc: func(_, obj any) { o.nodeSeen(obj) },
	})
	pods := f.Core().V1().Pods().Informer()
	_, _ = pods.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj any) { o.seen(obj, false) },
		UpdateFunc: func(_, obj any) { o.seen(obj, false) },
		DeleteFunc: func(obj any) {
			if t, ok := obj.(cache.DeletedFinalStateUnknown); ok {
				obj = t.Obj
			}
			o.seen(obj, true)
		},
	})
	f.Start(ctx.Done())
}

// watchDeparted replaces the pods gcr refuses to start because their
// container migrated away (replaceDeparted).
func (o *operator) watchDeparted(ctx context.Context) {
	f := informers.NewSharedInformerFactory(o.cs, 0)
	pods := f.Core().V1().Pods().Informer()
	_, _ = pods.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj any) { o.replaceDeparted(ctx, obj) },
		UpdateFunc: func(_, obj any) { o.replaceDeparted(ctx, obj) },
	})
	f.Start(ctx.Done())
}

// replaceDeparted deletes a pod whose container migrated away and that
// its controller kept: its processes run in no pod, or one the
// controller does not know, as when its migration went wrong after they
// moved. gcr refuses to start the container in it again, so it would
// never run; its controller starts a new one, cold, in its place. A pod
// being migrated is left alone: it is departed for a moment before its
// migration deletes it.
func (o *operator) replaceDeparted(ctx context.Context, obj any) {
	p, ok := obj.(*corev1.Pod)
	if !ok || p.DeletionTimestamp != nil || !kubemigrate.Departed(p) || kubemigrate.Migrating(p, staleAfter) {
		return
	}
	owner := metav1.GetControllerOf(p)
	if owner == nil {
		return
	}
	key := p.Namespace + "/" + p.Name
	o.mu.Lock()
	moving := o.moving[key]
	o.mu.Unlock()
	if moving {
		return
	}
	log.Printf("%s: its container migrated away and does not start here again; deleting it for its %s %s to replace",
		key, owner.Kind, owner.Name)
	uid := p.UID
	err := o.cs.CoreV1().Pods(p.Namespace).Delete(ctx, p.Name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}})
	if err != nil && !apierrors.IsNotFound(err) && !apierrors.IsConflict(err) {
		log.Printf("%s: %v", key, err)
	}
}

// nodeShuttingDown is in the Ready condition's message of a node whose
// kubelet is stopping its pods for a graceful node shutdown, from its
// start.
const nodeShuttingDown = "node is shutting down"

// nodeSeen follows a node from the informer: one starting to shut down
// has its pods that would move on a drain migrate live (shutdownNode).
func (o *operator) nodeSeen(obj any) {
	n, ok := obj.(*corev1.Node)
	if !ok || o.execRun == nil {
		return
	}
	down := false
	for _, c := range n.Status.Conditions {
		down = down || c.Type == corev1.NodeReady && strings.Contains(c.Message, nodeShuttingDown)
	}
	o.mu.Lock()
	start := down && !o.nodesDown[n.Name] && !o.stopping
	if start || !down {
		o.nodesDown[n.Name] = down
	}
	o.mu.Unlock()
	if start {
		go o.shutdownNode(n.Name)
	}
}

// shutdownNode moves the opted-in pods of node, which is shutting down,
// that a drain would migrate live, as a drain does: their containers'
// shims hold the kubelet's stop meanwhile (nodeshutdown.Await). The rest
// are checkpointed there as the kubelet stops them, and restored from the
// registry (shutdownStopped); so are those whose migration fails. Its
// plan for each reaches the node through gcr's installer pod there, as do
// the migrations: the node admits no Job.
func (o *operator) shutdownNode(node string) {
	ctx := context.Background()
	if !kubemigrate.HasInstaller(ctx, o.cs, o.opts.Namespace, node) {
		log.Printf("node %s shuts down, and gcr's installer does not run there: its pods are checkpointed, not migrated live", node)
		return
	}
	pods, err := o.cs.CoreV1().Pods("").List(ctx, metav1.ListOptions{FieldSelector: "spec.nodeName=" + node})
	if err != nil {
		log.Printf("node %s shuts down: %v", node, err)
		return
	}
	var live []*corev1.Pod
	var liveUIDs, checkpointUIDs []string
	// A pod a drain or a Migration moves already is left to that move.
	// The others are restored from a checkpoint only if their
	// migrations fail.
	o.mu.Lock()
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.Annotations[annotationOptIn] != "true" || p.DeletionTimestamp != nil {
			continue
		}
		if _, err := kubemigrate.Movable(p, ""); err == nil {
			if key := p.Namespace + "/" + p.Name; !o.moving[key] {
				o.moving[key] = true
				o.restoring[p.UID] = nil
				live = append(live, p)
				liveUIDs = append(liveUIDs, string(p.UID))
			}
		} else {
			checkpointUIDs = append(checkpointUIDs, string(p.UID))
		}
	}
	o.mu.Unlock()
	if len(live) == 0 && len(checkpointUIDs) == 0 {
		return
	}
	notMoving := func(pods []*corev1.Pod) {
		o.mu.Lock()
		for _, p := range pods {
			delete(o.restoring, p.UID)
			delete(o.moving, p.Namespace+"/"+p.Name)
		}
		o.mu.Unlock()
	}
	plan := func(how string, uids []string) error {
		if len(uids) == 0 {
			return nil
		}
		_, err := o.execRun(ctx, node, "plan", nil, append([]string{o.opts.GCR, "shutdown-plan", how}, uids...)...)
		return err
	}
	if err := plan("-checkpoint", checkpointUIDs); err != nil {
		log.Printf("node %s shuts down: %v", node, err) // they are checkpointed, a little later
	}
	if err := plan("-live", liveUIDs); err != nil {
		log.Printf("node %s shuts down, and its pods are checkpointed, not migrated live: %v", node, err)
		notMoving(live)
		return
	}
	log.Printf("node %s shuts down: migrating %d pods live, %d through checkpoints", node, len(live), len(checkpointUIDs))
	for i, p := range live {
		o.mu.Lock()
		if o.stopping {
			o.mu.Unlock()
			notMoving(live[i:])
			return
		}
		o.moves.Add(1)
		o.mu.Unlock()
		go o.moveLive(p)
	}
}

// moveLive migrates pod off its node, shutting down, as a drain does; if
// it cannot, the pod is checkpointed there and restored from the registry.
func (o *operator) moveLive(pod *corev1.Pod) {
	key := pod.Namespace + "/" + pod.Name
	defer func() {
		o.mu.Lock()
		delete(o.moving, key)
		o.mu.Unlock()
		o.moves.Done()
	}()
	opts := o.opts
	opts.Run = o.execRun
	opts.Log = func(format string, args ...any) { log.Printf(key+": "+format, args...) }
	res, err := kubemigrate.Migrate(context.Background(), o.cs, pod.Namespace, pod.Name, opts)
	if err != nil {
		log.Printf("%s: its node %s shuts down, and it could not migrate live, so it is checkpointed: %v", key, pod.Spec.NodeName, firstLine(err.Error()))
		o.mu.Lock()
		delete(o.restoring, pod.UID)
		o.mu.Unlock()
		if _, err := o.execRun(context.Background(), pod.Spec.NodeName, "plan", nil, o.opts.GCR, "shutdown-plan", "-checkpoint", string(pod.UID)); err != nil {
			log.Printf("%s: %v (it is checkpointed, a little later)", key, err)
		}
		return
	}
	// Handed over: the kubelet may let the source go, which would have
	// had its controller start another, cold, before.
	if _, err := o.execRun(context.Background(), pod.Spec.NodeName, "plan", nil, o.opts.GCR, "shutdown-plan", "-released", string(pod.UID)); err != nil {
		log.Printf("%s: %v (the kubelet stops it a little later)", key, err)
	}
	if res.Downtime == "" {
		res.Downtime = "unknown: " + strings.ReplaceAll(strings.TrimSpace(res.Output), "\n", " | ")
	}
	log.Printf("%s moved off %s, shutting down, to %s as %s (downtime %s)", key, pod.Spec.NodeName, res.Node, res.Target, res.Downtime)
}

// seen follows a pod from the informer.
func (o *operator) seen(obj any, deleted bool) {
	p, ok := obj.(*corev1.Pod)
	if !ok || p.Annotations[annotationOptIn] != "true" {
		return
	}
	if o.throughRegistry(p) {
		key := p.Namespace + "/" + p.Name
		o.mu.Lock()
		o.last[key] = lastPod{p, deleted, time.Now()}
		for k, l := range o.last {
			if l.gone && time.Since(l.at) > kubemigrate.RestoreTTL {
				delete(o.last, k)
			}
		}
		o.mu.Unlock()
	}
	if kubemigrate.OwnedRestore(p) {
		owner := p.OwnerReferences[0].UID
		o.mu.Lock()
		if o.owned[owner] == nil {
			o.owned[owner] = map[types.UID]lastPod{}
		}
		o.owned[owner][p.UID] = lastPod{p, deleted, time.Now()}
		for owner, pods := range o.owned {
			for uid, l := range pods {
				if l.gone && time.Since(l.at) > kubemigrate.RestoreTTL {
					delete(pods, uid)
				}
			}
			if len(pods) == 0 {
				delete(o.owned, owner)
			}
		}
		o.mu.Unlock()
	}
	if !deleted && kubemigrate.StoppedByShutdown(p) {
		o.shutdownStopped(p)
	}
}

// lastPod is the last the informer saw of a StatefulSet's pod.
type lastPod struct {
	pod  *corev1.Pod
	gone bool
	at   time.Time
}

// shutdownStopped starts restoring p, stopped as its node shut down, once,
// and returns a StatefulSet's pod's pending restore.
func (o *operator) shutdownStopped(p *corev1.Pod) *kubemigrate.Restore {
	o.shutdownMu.Lock()
	defer o.shutdownMu.Unlock()
	key := p.Namespace + "/" + p.Name
	o.mu.Lock()
	rs, started := o.restoring[p.UID]
	stopping := o.stopping
	o.mu.Unlock()
	if started || stopping {
		return rs
	}
	owned := kubemigrate.OwnedRestore(p)
	refuse := func(why string) *kubemigrate.Restore {
		log.Printf("%s: its node %s shut down, and %s", key, p.Spec.NodeName, why)
		o.mu.Lock()
		o.restoring[p.UID] = nil // said once
		o.mu.Unlock()
		return nil
	}
	if _, err := kubemigrate.MovableAtShutdown(p); err != nil {
		return refuse("it cannot be restored elsewhere: " + err.Error())
	}
	if owned && !o.ownedRestores {
		return refuse("its replacement starts cold: the admission policy is older than this operator")
	}
	if o.throughRegistry(p) || owned {
		var err error
		if rs, err = kubemigrate.ShutdownRestore(p, o.registry); err == nil {
			if owned {
				err = kubemigrate.RecordOwnedRestore(context.Background(), o.cs, o.opts.Namespace, p.Namespace, *rs)
			} else {
				err = kubemigrate.RecordRestore(context.Background(), o.cs, o.opts.Namespace, p.Namespace, p.Name, *rs)
			}
		}
		if err != nil {
			return refuse("its restore could not be recorded: " + err.Error())
		}
	}
	o.mu.Lock()
	o.restoring[p.UID] = rs
	o.mu.Unlock()
	o.moves.Add(1)
	if o.startRestore != nil {
		o.startRestore(p, rs)
		o.moves.Done()
	} else {
		go o.restore(p, rs)
	}
	return rs
}

// lateRestore is the pending restore for a StatefulSet's pod ns/name being
// created again, when there is none yet: its StatefulSet may replace the
// pod the kubelet stopped at its node's shutdown before the informer has
// told the operator. It waits for the informer a little, if the pod it
// replaces was on a node that is not Ready.
func (o *operator) lateRestore(ctx context.Context, ns, name string) *kubemigrate.Restore {
	key := ns + "/" + name
	o.mu.Lock()
	l, ok := o.last[key]
	o.mu.Unlock()
	if !ok {
		return nil
	}
	if !kubemigrate.StoppedByShutdown(l.pod) {
		if nodeReady(ctx, o.cs, l.pod.Spec.NodeName) {
			return nil // replaced on a working node: a rollout, say
		}
		deadline := time.Now().Add(2 * time.Second)
		for !kubemigrate.StoppedByShutdown(l.pod) {
			if time.Now().After(deadline) {
				return nil
			}
			time.Sleep(100 * time.Millisecond)
			o.mu.Lock()
			l = o.last[key]
			o.mu.Unlock()
		}
	}
	return o.shutdownStopped(l.pod)
}

func nodeReady(ctx context.Context, cs kubernetes.Interface, name string) bool {
	n, err := cs.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return false
	}
	for _, c := range n.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// claim claims an owned restore for pod, which its ReplicaSet or Job is
// creating in namespace ns: one of its owner's pods stopped at its node's
// shutdown. The controller may replace a stopped pod before the informer
// has said it stopped: the owner's pods on nodes that are not Ready get
// 2 s to show up as stopped.
func (o *operator) claim(ctx context.Context, ns string, pod *corev1.Pod) (*kubemigrate.Restore, error) {
	o.claimMu.Lock()
	defer o.claimMu.Unlock()
	owner := pod.OwnerReferences[0].UID
	deadline := time.Now().Add(2 * time.Second)
	ready := map[string]bool{} // nodes found Ready: nothing to wait for there
	for {
		// Each time round: the informer may have recorded one meanwhile.
		if r, err := kubemigrate.ClaimRestore(ctx, o.cs, o.opts.Namespace, ns, pod); r != nil || err != nil {
			return r, err
		}
		var stopped []*corev1.Pod
		var nodes []string
		o.mu.Lock()
		for uid, l := range o.owned[owner] {
			if _, started := o.restoring[uid]; started || l.gone {
				continue
			}
			if kubemigrate.StoppedByShutdown(l.pod) {
				stopped = append(stopped, l.pod)
			} else if n := l.pod.Spec.NodeName; n != "" && !ready[n] {
				nodes = append(nodes, n)
			}
		}
		o.mu.Unlock()
		for _, p := range stopped {
			o.shutdownStopped(p)
		}
		if len(stopped) > 0 {
			continue
		}
		down := false
		for _, n := range nodes {
			if nodeReady(ctx, o.cs, n) {
				ready[n] = true
			} else {
				down = true
			}
		}
		if !down || time.Now().After(deadline) {
			return nil, nil
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// restore restores pod, which its node's shutdown checkpointed.
func (o *operator) restore(pod *corev1.Pod, rs *kubemigrate.Restore) {
	defer o.moves.Done()
	key := pod.Namespace + "/" + pod.Name
	opts := o.opts
	opts.Log = func(format string, args ...any) { log.Printf(key+": "+format, args...) }
	log.Printf("%s: stopped as its node %s shuts down: restoring it elsewhere from its checkpoint", key, pod.Spec.NodeName)
	res, err := kubemigrate.RestoreAfterShutdown(context.Background(), o.cs, pod, rs, opts, o.registry)
	if err != nil {
		log.Printf("%s: not restored: %v", key, err)
		return
	}
	log.Printf("%s restored on %s as %s (downtime %s)", key, res.Node, res.Target, res.Downtime)
}

// throughRegistry says whether pod moves through a checkpoint in the
// registry, rather than node to node: a StatefulSet's pod can't have a
// twin running.
func (o *operator) throughRegistry(pod *corev1.Pod) bool {
	return len(pod.OwnerReferences) == 1 && pod.OwnerReferences[0].Kind == "StatefulSet"
}

// movable says whether the pod can be moved, the way move would.
func (o *operator) movable(pod *corev1.Pod) (string, error) {
	if !o.throughRegistry(pod) {
		id, err := kubemigrate.Movable(pod, "")
		if err == nil {
			err = kubemigrate.SharedClaims(context.Background(), o.cs, pod)
		}
		return id, err
	}
	if o.registry.Repository == "" {
		return "", errors.New("a StatefulSet's pod moves through a registry, and gcr-operator has none (-registry)")
	}
	return kubemigrate.MovableThroughRegistry(pod)
}

// recreated says whether pod is one its StatefulSet created again in
// place of the one evicted, restored from its checkpoint or, if the move
// failed, cold: kubectl drain evicts by name, and its retries reach the
// new pod. One evicted in its own right is on a node being drained.
func (o *operator) recreated(ctx context.Context, pod *corev1.Pod) bool {
	o.mu.Lock()
	since, moved := o.replaced[pod.Namespace+"/"+pod.Name]
	o.mu.Unlock()
	moved = moved && pod.CreationTimestamp.After(since)
	if !moved && pod.Annotations[kubemigrate.AnnotationRestoreToken] == "" || time.Since(pod.CreationTimestamp.Time) > staleAfter {
		return false
	}
	if pod.Spec.NodeName == "" {
		return true // not scheduled yet
	}
	node, err := o.cs.CoreV1().Nodes().Get(ctx, pod.Spec.NodeName, metav1.GetOptions{})
	if err != nil || node.Spec.Unschedulable {
		return false
	}
	for _, t := range node.Spec.Taints {
		if t.Key == "ToBeDeletedByClusterAutoscaler" {
			return false
		}
	}
	return true
}

// steer answers the admission review of a pod's creation: one its
// StatefulSet creates again, with a pending restore, restores from it.
func (o *operator) steer(ctx context.Context, req *admissionv1.AdmissionRequest) *admissionv1.AdmissionResponse {
	allow := &admissionv1.AdmissionResponse{Allowed: true}
	var pod corev1.Pod
	if o.registry.Repository == "" || json.Unmarshal(req.Object.Raw, &pod) != nil {
		return allow
	}
	// A StatefulSet's pod has its name already; a ReplicaSet's or a Job's
	// is named after this, from its generateName.
	name := pod.Name
	if name == "" {
		name = pod.GenerateName + "…"
	}
	var r *kubemigrate.Restore
	var err error
	switch {
	case o.throughRegistry(&pod) && pod.Name != "":
		if r, err = kubemigrate.PendingRestore(ctx, o.cs, o.opts.Namespace, req.Namespace, pod.Name); err == nil && r == nil {
			r = o.lateRestore(ctx, req.Namespace, pod.Name)
		}
	case kubemigrate.OwnedRestore(&pod) && o.ownedRestores:
		r, err = o.claim(ctx, req.Namespace, &pod)
	}
	if err != nil {
		log.Printf("%s/%s: %v: it starts cold", req.Namespace, name, err)
		return allow
	}
	if r == nil {
		return allow
	}
	patch, err := kubemigrate.Steer(&pod, *r)
	if err != nil {
		log.Printf("%s/%s: %v: it starts cold", req.Namespace, name, err)
		return allow
	}
	log.Printf("%s/%s: created, to restore from %s", req.Namespace, name, r.Ref)
	pt := admissionv1.PatchTypeJSONPatch
	return &admissionv1.AdmissionResponse{Allowed: true, Patch: patch, PatchType: &pt}
}

// restoresReady checks that the cluster lets StatefulSets' pods restore:
// the gcr-migrate admission policy reads the pending restores, and the
// webhook that steers pods is there. owned says whether the policy lets
// ReplicaSets' and Jobs' pods restore too.
func restoresReady(ctx context.Context, cs kubernetes.Interface, webhook string) (owned bool, err error) {
	p, err := cs.AdmissionregistrationV1().ValidatingAdmissionPolicies().Get(ctx, kubemigrate.RuntimeClass, metav1.GetOptions{})
	if err != nil {
		return false, fmt.Errorf("admission policy %s: %w", kubemigrate.RuntimeClass, err)
	}
	if p.Spec.ParamKind == nil || p.Spec.ParamKind.Kind != "ConfigMap" {
		return false, fmt.Errorf("admission policy %s reads no pending restores (%s): it is older than this operator; apply gcr.yaml again",
			kubemigrate.RuntimeClass, kubemigrate.RestoresConfigMap)
	}
	if _, err := cs.AdmissionregistrationV1().MutatingWebhookConfigurations().Get(ctx, webhook, metav1.GetOptions{}); err != nil {
		return false, fmt.Errorf("mutating webhook %s: %w", webhook, err)
	}
	// A policy that lets ReplicaSets and Jobs restore their pods names
	// their controllers.
	for _, v := range p.Spec.Validations {
		owned = owned || strings.Contains(v.Expression, "replicaset-controller")
	}
	return owned, nil
}

// parseMigrating reads a "migrating TIME by OPERATOR-POD" state; the
// operator is "" in states older operators wrote.
func parseMigrating(state string) (since time.Time, by string) {
	rest := strings.TrimPrefix(state, "migrating ")
	ts, by, _ := strings.Cut(rest, " by ")
	since, _ = time.Parse(time.RFC3339, ts)
	return since, by
}

// gone says whether operator pod name is no more: deleted, or finished. One
// on its way out still finishes what it moves.
func (o *operator) gone(ctx context.Context, name string) bool {
	p, err := o.cs.CoreV1().Pods(o.self.Namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return true
	}
	return err == nil && (p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed)
}

func (o *operator) mark(ctx context.Context, pod *corev1.Pod, state string) {
	patch, _ := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": map[string]string{kubemigrate.AnnotationState: state}}})
	if _, err := o.cs.CoreV1().Pods(pod.Namespace).Patch(ctx, pod.Name, types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
		log.Printf("%s/%s: marking it %q: %v", pod.Namespace, pod.Name, state, err)
	}
}

// tooMany refuses the eviction for now, as a PodDisruptionBudget does:
// kubectl drain and the autoscaler try again.
func tooMany(pod *corev1.Pod, why string) *admissionv1.AdmissionResponse {
	return &admissionv1.AdmissionResponse{Allowed: false, Result: &metav1.Status{
		Code: http.StatusTooManyRequests, Reason: metav1.StatusReasonTooManyRequests,
		Message: fmt.Sprintf("pod %s/%s %s", pod.Namespace, pod.Name, why),
	}}
}

// selfSigned makes a CA and a certificate for host signed by it.
func selfSigned(host string) (tls.Certificate, []byte, error) {
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	now := time.Now()
	caTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "gcr-operator CA"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.AddDate(10, 0, 0),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: host}, DNSNames: []string{host},
		NotBefore: now.Add(-time.Hour), NotAfter: now.AddDate(10, 0, 0),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	caCert, _ := x509.ParseCertificate(caDER)
	der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &key.PublicKey, caKey)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key},
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), nil
}

// giveCA sets every webhook of the configurations to trust ca: the
// validating one, and the mutating one if there is one (older manifests
// have none).
func giveCA(ctx context.Context, cs kubernetes.Interface, name string, ca []byte) error {
	cfg, err := cs.AdmissionregistrationV1().ValidatingWebhookConfigurations().Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	for i := range cfg.Webhooks {
		cfg.Webhooks[i].ClientConfig.CABundle = ca
	}
	if _, err = cs.AdmissionregistrationV1().ValidatingWebhookConfigurations().Update(ctx, cfg, metav1.UpdateOptions{}); err != nil {
		return err
	}
	m, err := cs.AdmissionregistrationV1().MutatingWebhookConfigurations().Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) || apierrors.IsForbidden(err) {
		return nil
	} else if err != nil {
		return err
	}
	for i := range m.Webhooks {
		m.Webhooks[i].ClientConfig.CABundle = ca
	}
	_, err = cs.AdmissionregistrationV1().MutatingWebhookConfigurations().Update(ctx, m, metav1.UpdateOptions{})
	return err
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}
