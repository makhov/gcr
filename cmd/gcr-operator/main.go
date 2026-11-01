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
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/makhov/gcr/internal/kubemigrate"
)

const (
	// annotationOptIn on a pod (from its template) asks for a migration
	// instead of an eviction.
	annotationOptIn = "gcr.dev/migrate-on-evict"
	// annotationState is the operator's own, on the pod being moved:
	// "migrating <since>" or "failed: why".
	annotationState = "gcr.dev/migration"
	// staleAfter is when a "migrating" mark left by an operator that went
	// away counts as failed, and the eviction is let through.
	staleAfter = 15 * time.Minute
)

func main() {
	listen := flag.String("listen", ":8443", "address the webhook listens on (TLS)")
	namespace := flag.String("namespace", envOr("POD_NAMESPACE", "gcr-system"), "gcr's namespace: its Service, and the migrations' Jobs")
	service := flag.String("service", "gcr-operator", "the webhook's Service, which its certificate names")
	webhook := flag.String("webhook", "gcr-operator", "the ValidatingWebhookConfiguration to give the certificate's CA")
	image := flag.String("image", "", "the image migrations' Jobs run (default: gcr's installer's, or busybox without its DaemonSet)")
	gcrPath := flag.String("gcr", "/usr/local/bin/gcr", "gcr's path on the nodes")
	flag.Parse()

	cfg, err := rest.InClusterConfig()
	if err != nil {
		log.Fatal(err)
	}
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

	op := &operator{cs: cs, opts: kubemigrate.Options{Namespace: *namespace, Image: *image, GCR: *gcrPath}, moving: map[string]bool{},
		self: metav1.ObjectMeta{Namespace: *namespace, Name: os.Getenv("POD_NAME")}, node: os.Getenv("NODE_NAME")}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /evict", op.evict)
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

	mu       sync.Mutex
	moving   map[string]bool // namespace/name of pods being moved
	stopping bool            // on its way out: it starts no migration
	moves    sync.WaitGroup
	// start moves a pod in the background: move, but for tests.
	start func(*corev1.Pod)
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

// evict answers an eviction's admission review.
func (o *operator) evict(w http.ResponseWriter, r *http.Request) {
	var review admissionv1.AdmissionReview
	if err := json.NewDecoder(r.Body).Decode(&review); err != nil || review.Request == nil {
		http.Error(w, "not an AdmissionReview", http.StatusBadRequest)
		return
	}
	req := review.Request
	resp := o.decide(r.Context(), req)
	resp.UID = req.UID
	review.Response = resp
	review.Request = nil
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(review)
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
	state := pod.Annotations[annotationState]
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
	stopping := o.stopping
	o.mu.Unlock()
	if stopping {
		return tooMany(pod, "waits for the operator, which is restarting")
	}
	if _, err := kubemigrate.Movable(pod); err != nil {
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
	opts := o.opts
	opts.Log = func(format string, args ...any) { log.Printf(key+": "+format, args...) }
	res, err := kubemigrate.Migrate(ctx, o.cs, pod.Namespace, pod.Name, opts)
	if err != nil {
		log.Printf("%s: migration failed, letting its eviction through: %v", key, err)
		o.mark(ctx, pod, "failed: "+firstLine(err.Error()))
		return
	}
	log.Printf("%s moved to %s as %s (downtime %s)", key, res.Node, res.Target, res.Downtime)
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
			strings.HasPrefix(p.Annotations[annotationState], "failed") {
			continue
		}
		if _, err := kubemigrate.Movable(p); err == nil {
			n++
		}
	}
	return n
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
	patch, _ := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": map[string]string{annotationState: state}}})
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

// giveCA sets every webhook of the configuration to trust ca.
func giveCA(ctx context.Context, cs kubernetes.Interface, name string, ca []byte) error {
	cfg, err := cs.AdmissionregistrationV1().ValidatingWebhookConfigurations().Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	for i := range cfg.Webhooks {
		cfg.Webhooks[i].ClientConfig.CABundle = ca
	}
	_, err = cs.AdmissionregistrationV1().ValidatingWebhookConfigurations().Update(ctx, cfg, metav1.UpdateOptions{})
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
