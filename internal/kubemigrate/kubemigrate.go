// Package kubemigrate moves a running pod to another node through
// Kubernetes' API: a target pod on the gcr-migrate runtime class waits for
// the migration, and a Job on the source node runs gcr migrate into it.
// It is what kubectl gcr migrate runs; the operator will reuse it.
package kubemigrate

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
)

const (
	// RuntimeClass is the handler targets run on: one whose PodAnnotations
	// let a pod ask its shim to listen for a migration. Only cluster
	// admins and the operator may use it (gcr.yaml's admission policy).
	RuntimeClass = "gcr-migrate"
	// containerdNamespace is where the CRI keeps pods' containers.
	containerdNamespace = "k8s.io"

	// gcr's annotations, named here rather than imported: this package
	// runs in kubectl's plugin, on any OS, and gcr's own packages need
	// Linux and cgo. kubemigrate_linux_test.go checks they agree.
	annotationMigrateIn    = "gcr.dev/migrate-in"
	annotationMigrateToken = "gcr.dev/migrate-token"
	annotationUsernsBase   = "gcr.dev/userns-base"
	annotationHostUsers    = "gcr.dev/host-users"
)

// info is what gcr info prints (control.Info).
type info struct {
	IDBase uint32 `json:"idBase"`
}

// Options say how to migrate.
type Options struct {
	// Node is where the pod goes; the scheduler chooses if empty, among
	// nodes other than the pod's.
	Node string
	// Namespace is where the Jobs and Secrets run: gcr's (gcr-system).
	Namespace string
	// Image runs the Jobs: gcr's installer image, which has nsenter.
	Image string
	// GCR is gcr's path on the nodes.
	GCR string
	// Timeout bounds each wait.
	Timeout time.Duration
	// Log, if set, is told each step.
	Log func(format string, args ...any)
}

// Result describes a finished migration.
type Result struct {
	Target   string
	Node     string
	Downtime string
	Output   string
}

// Migrate moves pod name in namespace ns to another node. The source pod
// is deleted once its processes run in the target; if the migration
// fails, the target is deleted and the source carries on.
func Migrate(ctx context.Context, cs kubernetes.Interface, ns, name string, o Options) (res Result, retErr error) {
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
	src, err := cs.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return res, err
	}
	id, err := Movable(src)
	if err != nil {
		return res, fmt.Errorf("%s/%s: %w", ns, name, err)
	}
	if o.Node == src.Spec.NodeName {
		return res, fmt.Errorf("%s/%s is on %s already", ns, name, o.Node)
	}

	log("asking %s for %s's user namespace", src.Spec.NodeName, name)
	out, err := runJob(ctx, cs, o, src.Spec.NodeName, "info", nil, o.GCR, "-n", containerdNamespace, "info", id)
	if err != nil {
		return res, fmt.Errorf("gcr info on %s: %w\n%s", src.Spec.NodeName, err, out)
	}
	var inf info
	if err := json.Unmarshal([]byte(lastLine(out)), &inf); err != nil {
		return res, fmt.Errorf("gcr info on %s: %w: %q", src.Spec.NodeName, err, out)
	}

	token, err := randomToken()
	if err != nil {
		return res, err
	}
	n, err := rand.Int(rand.Reader, big.NewInt(1000))
	if err != nil {
		return res, err
	}
	addr := fmt.Sprintf("0.0.0.0:%d", 17000+n.Int64())
	dst := targetPod(src, o.Node, addr, token, inf)
	// A ReplicaSet's pod: the target stays out of its sight until it runs
	// the pod's processes, or the ReplicaSet would delete it as one too
	// many while it waits for them.
	var held map[string]string
	if ownedByReplicaSet(src) {
		rs, err := cs.AppsV1().ReplicaSets(ns).Get(ctx, src.OwnerReferences[0].Name, metav1.GetOptions{})
		if err != nil {
			return res, fmt.Errorf("the pod's ReplicaSet: %w", err)
		}
		held = holdBack(dst, rs.Spec.Selector)
	}
	dst, err = cs.CoreV1().Pods(ns).Create(ctx, dst, metav1.CreateOptions{})
	if err != nil {
		return res, fmt.Errorf("create the target: %w", err)
	}
	res.Target = dst.Name
	handedOver := false
	defer func() {
		if !handedOver {
			_ = cs.CoreV1().Pods(ns).Delete(context.WithoutCancel(ctx), dst.Name, metav1.DeleteOptions{GracePeriodSeconds: new(int64)})
		}
	}()
	log("target %s created; waiting for it to be scheduled", dst.Name)
	if err := wait.PollUntilContextTimeout(ctx, time.Second, o.Timeout, true, func(ctx context.Context) (bool, error) {
		p, err := cs.CoreV1().Pods(ns).Get(ctx, dst.Name, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		if p.Status.Phase == corev1.PodFailed {
			return false, fmt.Errorf("the target failed: %s", podFailure(p))
		}
		dst = p
		return p.Status.HostIP != "", nil
	}); err != nil {
		return res, fmt.Errorf("target %s: %w", dst.Name, err)
	}
	res.Node = dst.Spec.NodeName
	_, port, _ := net.SplitHostPort(addr)
	target := "http://" + net.JoinHostPort(dst.Status.HostIP, port)

	log("migrating %s from %s to %s (%s)", name, src.Spec.NodeName, res.Node, target)
	// The target's shim listens once the kubelet has created its
	// container; until then the migration is refused before anything
	// freezes, and is tried again.
	script := fmt.Sprintf(`for i in $(seq 60); do
  out=$(%s -n %s migrate -token "$TOKEN" %s %s 2>&1) && { echo "$out"; exit 0; }
  case "$out" in *"connection refused"*) sleep 1;; *) echo "$out"; exit 1;; esac
done
echo "$out"; exit 1`, o.GCR, containerdNamespace, id, target)
	out, err = runJob(ctx, cs, o, src.Spec.NodeName, "migrate", map[string]string{"TOKEN": token}, "sh", "-c", script)
	res.Output = out
	if err != nil {
		return res, fmt.Errorf("gcr migrate on %s: %w\n%s", src.Spec.NodeName, err, out)
	}
	for _, line := range strings.Split(out, "\n") {
		if d, ok := strings.CutPrefix(line, "downtime: "); ok {
			res.Downtime = d
		}
	}
	// The processes run in the target now; the source's are gone.
	handedOver = true
	if held != nil {
		// Before the source goes: the ReplicaSet then finds the target
		// its, and one pod too many, rather than one too few, and never
		// starts a cold one.
		log("handing %s over to %s %s", dst.Name, src.OwnerReferences[0].Kind, src.OwnerReferences[0].Name)
		if err := handOver(ctx, cs, src, dst.Name, held); err != nil {
			return res, fmt.Errorf("the pod runs in %s now, but handing it over to its ReplicaSet failed: %w", dst.Name, err)
		}
	}
	log("deleting %s", name)
	if err := cs.CoreV1().Pods(ns).Delete(ctx, name, metav1.DeleteOptions{GracePeriodSeconds: new(int64)}); err != nil && !apierrors.IsNotFound(err) {
		return res, fmt.Errorf("the pod runs in %s now, but deleting %s failed: %w", dst.Name, name, err)
	}
	if err := wait.PollUntilContextTimeout(ctx, time.Second, o.Timeout, true, func(ctx context.Context) (bool, error) {
		p, err := cs.CoreV1().Pods(ns).Get(ctx, dst.Name, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		return p.Status.Phase == corev1.PodRunning, nil
	}); err != nil {
		return res, fmt.Errorf("target %s after the migration: %w", dst.Name, err)
	}
	return res, nil
}

// Movable says whether the pod can be moved, and returns its container's
// containerd ID.
func Movable(p *corev1.Pod) (string, error) {
	switch {
	case p.Status.Phase != corev1.PodRunning:
		return "", fmt.Errorf("not running (%s)", p.Status.Phase)
	case len(p.OwnerReferences) > 1 || len(p.OwnerReferences) == 1 && !ownedByReplicaSet(p):
		// Its controller would replace it with a cold one once it is
		// deleted; ReplicaSets' pods are handed over (handOver).
		return "", fmt.Errorf("owned by %s %s: only pods without an owner, or a ReplicaSet's, can be moved yet", p.OwnerReferences[0].Kind, p.OwnerReferences[0].Name)
	case len(p.Spec.Containers) != 1 || len(p.Status.ContainerStatuses) != 1:
		return "", fmt.Errorf("%d containers: only pods with one can be moved yet", len(p.Spec.Containers))
	case p.Spec.HostUsers != nil && !*p.Spec.HostUsers:
		// The kubelet picks such a pod's ID range, and would give the
		// target another; CRIU restores processes into the one they had.
		return "", fmt.Errorf("hostUsers: false: Kubernetes picks the pod's user namespace range, and the target would get another one; "+
			"a pod in gcr's own user namespace (annotation %s: \"false\") can move", annotationHostUsers)
	}
	for _, c := range p.Spec.InitContainers {
		if c.RestartPolicy != nil && *c.RestartPolicy == corev1.ContainerRestartPolicyAlways {
			return "", fmt.Errorf("sidecar %s: only pods with one container can be moved yet", c.Name)
		}
	}
	for _, v := range p.Spec.Volumes {
		// A new node mounts these afresh with the same contents; others
		// hold files on this node that would not follow.
		if v.ConfigMap == nil && v.Secret == nil && v.Projected == nil && v.DownwardAPI == nil {
			return "", fmt.Errorf("volume %s: its files would stay on %s (only ConfigMap, Secret, projected and downward API volumes can move yet)", v.Name, p.Spec.NodeName)
		}
	}
	id, ok := strings.CutPrefix(p.Status.ContainerStatuses[0].ContainerID, "containerd://")
	if !ok {
		return "", fmt.Errorf("container %q is not containerd's", p.Status.ContainerStatuses[0].ContainerID)
	}
	return id, nil
}

func ownedByReplicaSet(p *corev1.Pod) bool {
	return len(p.OwnerReferences) == 1 && p.OwnerReferences[0].Kind == "ReplicaSet" &&
		p.OwnerReferences[0].Controller != nil && *p.OwnerReferences[0].Controller
}

// holdBack takes off dst the labels sel selects by, so that its
// ReplicaSet does not see it, and returns them.
func holdBack(dst *corev1.Pod, sel *metav1.LabelSelector) map[string]string {
	held := map[string]string{}
	labels := map[string]string{}
	for k, v := range dst.Labels {
		labels[k] = v
	}
	if sel != nil {
		for k := range sel.MatchLabels {
			if v, ok := labels[k]; ok {
				held[k] = v
				delete(labels, k)
			}
		}
		for _, e := range sel.MatchExpressions {
			if v, ok := labels[e.Key]; ok {
				held[e.Key] = v
				delete(labels, e.Key)
			}
		}
	}
	dst.Labels = labels
	return held
}

// handOver gives the target the labels held back and src's owner, which
// adopts it: the ReplicaSet then has one pod too many, and deletes the
// source, which no longer runs, first.
func handOver(ctx context.Context, cs kubernetes.Interface, src *corev1.Pod, target string, held map[string]string) error {
	patch := map[string]any{"metadata": map[string]any{
		"labels":          held,
		"ownerReferences": src.OwnerReferences,
	}}
	b, err := json.Marshal(patch)
	if err != nil {
		return err
	}
	_, err = cs.CoreV1().Pods(src.Namespace).Patch(ctx, target, types.StrategicMergePatchType, b, metav1.PatchOptions{})
	return err
}

// targetPod is the pod the migration goes into: the source's spec on the
// gcr-migrate runtime class, listening at addr for token, in the source's
// user namespace range, on node or off the source's.
func targetPod(src *corev1.Pod, node, addr, token string, inf info) *corev1.Pod {
	suffix := make([]byte, 3)
	_, _ = rand.Read(suffix)
	dst := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        fmt.Sprintf("%s-%x", src.Name, suffix),
			Namespace:   src.Namespace,
			Labels:      src.Labels,
			Annotations: map[string]string{},
		},
		Spec: *src.Spec.DeepCopy(),
	}
	for k, v := range src.Annotations {
		dst.Annotations[k] = v
	}
	dst.Annotations[annotationMigrateIn] = addr
	dst.Annotations[annotationMigrateToken] = token
	if inf.IDBase != 0 {
		dst.Annotations[annotationUsernsBase] = fmt.Sprint(inf.IDBase)
	} else {
		dst.Annotations[annotationHostUsers] = "true"
	}
	dst.Spec.RuntimeClassName = new(string)
	*dst.Spec.RuntimeClassName = RuntimeClass
	dst.Spec.NodeName = node
	if node == "" {
		notSource := corev1.NodeSelectorRequirement{Key: "kubernetes.io/hostname", Operator: corev1.NodeSelectorOpNotIn, Values: []string{src.Spec.NodeName}}
		if dst.Spec.Affinity == nil {
			dst.Spec.Affinity = &corev1.Affinity{}
		}
		if dst.Spec.Affinity.NodeAffinity == nil {
			dst.Spec.Affinity.NodeAffinity = &corev1.NodeAffinity{}
		}
		na := dst.Spec.Affinity.NodeAffinity
		if na.RequiredDuringSchedulingIgnoredDuringExecution == nil {
			na.RequiredDuringSchedulingIgnoredDuringExecution = &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{}}}
		}
		for i := range na.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms {
			t := &na.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms[i]
			t.MatchExpressions = append(t.MatchExpressions, notSource)
		}
	}
	return dst
}

// runJob runs command on node in the host's namespaces (a privileged Job of
// gcr's installer image, which has nsenter), with env from a Secret, and
// returns its output.
func runJob(ctx context.Context, cs kubernetes.Interface, o Options, node, what string, env map[string]string, command ...string) (string, error) {
	suffix := make([]byte, 3)
	_, _ = rand.Read(suffix)
	name := fmt.Sprintf("gcr-%s-%x", what, suffix)
	var envVars []corev1.EnvVar
	if len(env) > 0 {
		sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: o.Namespace}, StringData: env}
		if _, err := cs.CoreV1().Secrets(o.Namespace).Create(ctx, sec, metav1.CreateOptions{}); err != nil {
			return "", err
		}
		defer func() {
			_ = cs.CoreV1().Secrets(o.Namespace).Delete(context.WithoutCancel(ctx), name, metav1.DeleteOptions{})
		}()
		for k := range env {
			envVars = append(envVars, corev1.EnvVar{Name: k, ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: name}, Key: k}}})
		}
	}
	privileged, never, zero := true, false, int32(0)
	ttl := int32(600)
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: o.Namespace, Labels: map[string]string{"app.kubernetes.io/name": "gcr-migrate"}},
		Spec: batchv1.JobSpec{
			BackoffLimit:            &zero,
			TTLSecondsAfterFinished: &ttl,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					NodeName:                     node,
					HostPID:                      true,
					RestartPolicy:                corev1.RestartPolicyNever,
					AutomountServiceAccountToken: &never,
					Tolerations:                  []corev1.Toleration{{Operator: corev1.TolerationOpExists}},
					Containers: []corev1.Container{{
						Name:            what,
						Image:           o.Image,
						Command:         append([]string{"nsenter", "--target", "1", "--mount", "--uts", "--ipc", "--net", "--pid", "--"}, command...),
						Env:             envVars,
						SecurityContext: &corev1.SecurityContext{Privileged: &privileged},
					}},
				},
			},
		},
	}
	if _, err := cs.BatchV1().Jobs(o.Namespace).Create(ctx, job, metav1.CreateOptions{}); err != nil {
		return "", err
	}
	background := metav1.DeletePropagationBackground
	defer func() {
		_ = cs.BatchV1().Jobs(o.Namespace).Delete(context.WithoutCancel(ctx), name, metav1.DeleteOptions{PropagationPolicy: &background})
	}()
	var failed bool
	if err := wait.PollUntilContextTimeout(ctx, 500*time.Millisecond, o.Timeout, true, func(ctx context.Context) (bool, error) {
		j, err := cs.BatchV1().Jobs(o.Namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		failed = j.Status.Failed > 0
		return j.Status.Succeeded > 0 || failed, nil
	}); err != nil {
		return "", fmt.Errorf("job %s: %w", name, err)
	}
	out := jobLogs(ctx, cs, o.Namespace, name)
	if failed {
		return out, fmt.Errorf("job %s failed", name)
	}
	return out, nil
}

func jobLogs(ctx context.Context, cs kubernetes.Interface, ns, job string) string {
	pods, err := cs.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: "job-name=" + job})
	if err != nil || len(pods.Items) == 0 {
		return ""
	}
	r, err := cs.CoreV1().Pods(ns).GetLogs(pods.Items[0].Name, &corev1.PodLogOptions{}).Stream(ctx)
	if err != nil {
		return ""
	}
	defer r.Close()
	b, _ := io.ReadAll(r)
	return strings.TrimSpace(string(b))
}

func podFailure(p *corev1.Pod) string {
	for _, s := range p.Status.ContainerStatuses {
		if t := s.State.Terminated; t != nil {
			return t.Reason + ": " + t.Message
		}
	}
	return p.Status.Reason + ": " + p.Status.Message
}

func lastLine(s string) string {
	var last string
	sc := bufio.NewScanner(strings.NewReader(s))
	for sc.Scan() {
		if l := strings.TrimSpace(sc.Text()); l != "" {
			last = l
		}
	}
	return last
}

func randomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
