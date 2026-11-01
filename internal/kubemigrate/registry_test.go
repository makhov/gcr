package kubemigrate

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	jsonpatch "gopkg.in/evanphx/json-patch.v4"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
)

func TestMovableThroughRegistry(t *testing.T) {
	yes := true
	sts := func(p *corev1.Pod) {
		p.OwnerReferences = []metav1.OwnerReference{{Kind: "StatefulSet", Name: "web", Controller: &yes}}
	}
	claim := corev1.Volume{Name: "data", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "data-p"}}}
	for name, tc := range map[string]struct {
		pod    *corev1.Pod
		refuse string
	}{
		"a bare pod":       {running(nil), ""},
		"a StatefulSet's":  {running(sts), ""},
		"with a claim":     {running(func(p *corev1.Pod) { sts(p); p.Spec.Volumes = []corev1.Volume{claim} }), ""},
		"bare, with claim": {running(func(p *corev1.Pod) { p.Spec.Volumes = []corev1.Volume{claim} }), "volume data"},
		"a ReplicaSet's": {running(func(p *corev1.Pod) {
			p.OwnerReferences = []metav1.OwnerReference{{Kind: "ReplicaSet", Name: "rs", Controller: &yes}}
		}), "owned by ReplicaSet"},
		"an emptyDir": {running(func(p *corev1.Pod) {
			sts(p)
			p.Spec.Volumes = []corev1.Volume{{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}}
		}), "emptyDir"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := MovableThroughRegistry(tc.pod)
			switch {
			case tc.refuse == "" && err != nil:
				t.Fatalf("refused: %v", err)
			case tc.refuse != "" && (err == nil || !strings.Contains(err.Error(), tc.refuse)):
				t.Fatalf("got %v, want a refusal with %q", err, tc.refuse)
			}
		})
	}
}

func TestClaimsFollow(t *testing.T) {
	pv := func(name string, mod func(*corev1.PersistentVolume)) *corev1.PersistentVolume {
		v := &corev1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: name}}
		mod(v)
		return v
	}
	pvc := func(name, volume string) *corev1.PersistentVolumeClaim {
		return &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"}, Spec: corev1.PersistentVolumeClaimSpec{VolumeName: volume}}
	}
	cs := fake.NewSimpleClientset(
		pv("nfs", func(v *corev1.PersistentVolume) { v.Spec.NFS = &corev1.NFSVolumeSource{Server: "s", Path: "/"} }),
		pv("local", func(v *corev1.PersistentVolume) { v.Spec.Local = &corev1.LocalVolumeSource{Path: "/mnt"} }),
		pv("pinned", func(v *corev1.PersistentVolume) {
			v.Spec.CSI = &corev1.CSIPersistentVolumeSource{Driver: "rancher.io/local-path"}
			v.Spec.NodeAffinity = &corev1.VolumeNodeAffinity{Required: &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{
				MatchExpressions: []corev1.NodeSelectorRequirement{{Key: "kubernetes.io/hostname", Operator: corev1.NodeSelectorOpIn, Values: []string{"n1"}}}}}}}
		}),
		pvc("nfs", "nfs"), pvc("local", "local"), pvc("pinned", "pinned"), pvc("pending", ""),
	)
	for claim, refuse := range map[string]string{"nfs": "", "local": "local volume", "pinned": "some nodes only", "pending": "not bound", "missing": "not found"} {
		t.Run(claim, func(t *testing.T) {
			p := running(func(p *corev1.Pod) {
				p.Spec.Volumes = []corev1.Volume{{Name: "data", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: claim}}}}
			})
			err := claimsFollow(context.Background(), cs, p)
			switch {
			case refuse == "" && err != nil:
				t.Fatalf("refused: %v", err)
			case refuse != "" && (err == nil || !strings.Contains(err.Error(), refuse)):
				t.Fatalf("got %v, want a refusal with %q", err, refuse)
			}
		})
	}
}

func TestRestores(t *testing.T) {
	ctx := context.Background()
	cs := fake.NewSimpleClientset()
	r := Restore{Token: "t0k", Dir: "/var/lib/gcr/checkpoints/u1", Ref: "reg/x:u1", UID: "u1", Node: "n1", IDBase: 65536, Since: time.Now().UTC()}
	if got, err := PendingRestore(ctx, cs, "gcr-system", "default", "web-0"); err != nil || got != nil {
		t.Fatalf("with no ConfigMap: %v, %v", got, err)
	}
	// Recorded with no ConfigMap yet, it is created; then patched.
	if err := RecordRestore(ctx, cs, "gcr-system", "default", "web-0", r); err != nil {
		t.Fatal(err)
	}
	if err := RecordRestore(ctx, cs, "gcr-system", "other", "web-0", r); err != nil {
		t.Fatal(err)
	}
	got, err := PendingRestore(ctx, cs, "gcr-system", "default", "web-0")
	if err != nil || got == nil || *got != r {
		t.Fatalf("got %+v, %v; want %+v", got, err, r)
	}
	cm, _ := cs.CoreV1().ConfigMaps("gcr-system").Get(ctx, RestoresConfigMap, metav1.GetOptions{})
	// What the gcr-migrate admission policy matches.
	if v, want := cm.Data["default.web-0"], `{"token":"t0k","dir":"/var/lib/gcr/checkpoints/u1",`; !strings.HasPrefix(v, want) {
		t.Errorf("recorded %s, want it to start %s", v, want)
	}
	if err := ForgetRestore(ctx, cs, "gcr-system", "default", "web-0"); err != nil {
		t.Fatal(err)
	}
	if got, _ := PendingRestore(ctx, cs, "gcr-system", "default", "web-0"); got != nil {
		t.Errorf("forgotten, but pending: %+v", got)
	}
	if got, _ := PendingRestore(ctx, cs, "gcr-system", "other", "web-0"); got == nil {
		t.Error("forgetting one forgot another")
	}
	r.Since = time.Now().Add(-RestoreTTL - time.Minute)
	if err := RecordRestore(ctx, cs, "gcr-system", "default", "web-0", r); err != nil {
		t.Fatal(err)
	}
	if got, _ := PendingRestore(ctx, cs, "gcr-system", "default", "web-0"); got != nil {
		t.Errorf("a stale restore is pending: %+v", got)
	}
}

func TestSteer(t *testing.T) {
	gcr := "gcr"
	p := running(func(p *corev1.Pod) {
		p.Name = "web-0"
		p.Annotations = map[string]string{"gcr.dev/migrate-on-evict": "true", annotationMigrateIn: "0.0.0.0:1"}
		p.Spec.RuntimeClassName = &gcr
	})
	for name, tc := range map[string]struct {
		idBase uint32
		want   map[string]string
	}{
		"gcr's user namespace": {65536, map[string]string{annotationUsernsBase: "65536"}},
		"the host's":           {0, map[string]string{annotationHostUsers: "true"}},
	} {
		t.Run(name, func(t *testing.T) {
			patch, err := Steer(p, Restore{Token: "t0k", Dir: "/var/lib/gcr/checkpoints/u1", IDBase: tc.idBase})
			if err != nil {
				t.Fatal(err)
			}
			jp, err := jsonpatch.DecodePatch(patch)
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(p)
			if raw, err = jp.Apply(raw); err != nil {
				t.Fatal(err)
			}
			var got corev1.Pod
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatal(err)
			}
			if got.Spec.RuntimeClassName == nil || *got.Spec.RuntimeClassName != RuntimeClass {
				t.Errorf("runtime class %v, want %s", got.Spec.RuntimeClassName, RuntimeClass)
			}
			want := map[string]string{"gcr.dev/migrate-on-evict": "true", annotationForkFrom: "/var/lib/gcr/checkpoints/u1",
				annotationForkWait: forkWait, AnnotationRestoreToken: "t0k"}
			for k, v := range tc.want {
				want[k] = v
			}
			if len(got.Annotations) != len(want) {
				t.Errorf("annotations %v, want %v", got.Annotations, want)
			}
			for k, v := range want {
				if got.Annotations[k] != v {
					t.Errorf("%s: %q, want %q", k, got.Annotations[k], v)
				}
			}
		})
	}
}

func TestStoppedByShutdown(t *testing.T) {
	const msg = "Pod was terminated in response to imminent node shutdown."
	for name, tc := range map[string]struct {
		status corev1.PodStatus
		want   bool
	}{
		"running": {corev1.PodStatus{Phase: corev1.PodRunning}, false},
		"stopped by the kubelet, shutting down": {corev1.PodStatus{Conditions: []corev1.PodCondition{{Type: corev1.DisruptionTarget,
			Status: corev1.ConditionTrue, Reason: corev1.PodReasonTerminationByKubelet, Message: msg}}}, true},
		"failed, shutting down": {corev1.PodStatus{Phase: corev1.PodFailed, Reason: "Terminated", Message: msg}, true},
		// Node-pressure eviction: no checkpoint was taken.
		"stopped by the kubelet, evicted": {corev1.PodStatus{Phase: corev1.PodFailed, Reason: "Evicted", Conditions: []corev1.PodCondition{{
			Type: corev1.DisruptionTarget, Status: corev1.ConditionTrue, Reason: corev1.PodReasonTerminationByKubelet, Message: "The node was low on resource: memory."}}}, false},
	} {
		if got := StoppedByShutdown(&corev1.Pod{Status: tc.status}); got != tc.want {
			t.Errorf("%s: %v, want %v", name, got, tc.want)
		}
	}
}

// Owned restores: a ReplicaSet's or a Job's new pods each claim one of
// their owner's, once; an Indexed Job's, one of the same index.
func TestClaimRestore(t *testing.T) {
	ctx := context.Background()
	cs := fake.NewSimpleClientset()
	yes := true
	stopped := func(uid, owner, index string) *corev1.Pod {
		p := running(func(p *corev1.Pod) {
			p.UID = types.UID(uid)
			p.OwnerReferences = []metav1.OwnerReference{{Kind: "Job", Name: "j", UID: types.UID(owner), Controller: &yes}}
			if index != "" {
				p.Annotations = map[string]string{annotationJobIndex: index}
			}
		})
		return p
	}
	reg := RegistryOptions{Repository: "reg/x"}
	var recorded []*Restore
	for _, p := range []*corev1.Pod{stopped("a", "job1", "0"), stopped("b", "job1", "1"), stopped("c", "job2", "")} {
		r, err := ShutdownRestore(p, reg)
		if err != nil {
			t.Fatal(err)
		}
		if r.Owner == "" || r.ID == "" {
			t.Fatalf("not an owned restore: %+v", r)
		}
		if err := RecordOwnedRestore(ctx, cs, "gcr-system", "default", *r); err != nil {
			t.Fatal(err)
		}
		recorded = append(recorded, r)
	}
	newPod := func(owner, index string) *corev1.Pod {
		p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{GenerateName: "j-",
			OwnerReferences: []metav1.OwnerReference{{Kind: "Job", Name: "j", UID: types.UID(owner), Controller: &yes}}}}
		if index != "" {
			p.Annotations = map[string]string{annotationJobIndex: index}
		}
		return p
	}
	claim := func(p *corev1.Pod) *Restore {
		r, err := ClaimRestore(ctx, cs, "gcr-system", "default", p)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	if r := claim(newPod("job1", "1")); r == nil || r.UID != "b" {
		t.Fatalf("index 1 claimed %+v, want b's", r)
	}
	if r := claim(newPod("job1", "1")); r != nil {
		t.Fatalf("b's claimed twice: %+v", r)
	}
	if r := claim(newPod("job1", "0")); r == nil || r.UID != "a" {
		t.Fatalf("index 0 claimed %+v, want a's", r)
	}
	if r := claim(newPod("job2", "")); r == nil || r.UID != "c" {
		t.Fatalf("job2 claimed %+v, want c's", r)
	}
	if r := claim(newPod("job3", "")); r != nil {
		t.Fatalf("another owner's pod claimed %+v", r)
	}
	if r, _ := ClaimRestore(ctx, cs, "gcr-system", "elsewhere", newPod("job2", "")); r != nil {
		t.Fatalf("a pod in another namespace claimed %+v", r)
	}
	// What the admission policy matches: NAMESPACE.TOKEN, starting with
	// the token and the directory.
	cm, _ := cs.CoreV1().ConfigMaps("gcr-system").Get(ctx, RestoresConfigMap, metav1.GetOptions{})
	want := `{"token":"` + recorded[0].Token + `","dir":"` + recorded[0].Dir + `",`
	if v := cm.Data["default."+recorded[0].Token]; !strings.HasPrefix(v, want) {
		t.Errorf("recorded %s, want it to start %s", v, want)
	}
	// A stale one is not claimed.
	old := stopped("d", "job4", "")
	r, _ := ShutdownRestore(old, reg)
	r.Since = time.Now().Add(-RestoreTTL - time.Minute)
	_ = RecordOwnedRestore(ctx, cs, "gcr-system", "default", *r)
	if r := claim(newPod("job4", "")); r != nil {
		t.Fatalf("a stale restore claimed: %+v", r)
	}
	if err := ForgetOwnedRestore(ctx, cs, "gcr-system", "default", *recorded[0]); err != nil {
		t.Fatal(err)
	}
	cm, _ = cs.CoreV1().ConfigMaps("gcr-system").Get(ctx, RestoresConfigMap, metav1.GetOptions{})
	if _, ok := cm.Data["default."+recorded[0].Token]; ok {
		t.Error("forgotten, but recorded")
	}
}

// A pod that claimed an owned restore is labelled with its ID, to be found
// once it has a name.
func TestSteerOwned(t *testing.T) {
	p := running(func(p *corev1.Pod) { p.Labels = map[string]string{"app": "x"} })
	patch, err := Steer(p, Restore{Token: "t0k", Dir: "/d", ID: "abc123"})
	if err != nil {
		t.Fatal(err)
	}
	jp, _ := jsonpatch.DecodePatch(patch)
	raw, _ := json.Marshal(p)
	if raw, err = jp.Apply(raw); err != nil {
		t.Fatal(err)
	}
	var got corev1.Pod
	_ = json.Unmarshal(raw, &got)
	if got.Labels[LabelRestore] != "abc123" || got.Labels["app"] != "x" {
		t.Errorf("labels %v", got.Labels)
	}
}

// Evictions move ReplicaSets' and Jobs' pods node to node, not through a
// registry; their node's shutdown, through one.
func TestOwnedAtShutdown(t *testing.T) {
	yes := true
	for _, kind := range []string{"ReplicaSet", "Job"} {
		p := running(func(p *corev1.Pod) {
			p.OwnerReferences = []metav1.OwnerReference{{Kind: kind, Name: "o", Controller: &yes}}
		})
		if _, err := MovableThroughRegistry(p); err == nil {
			t.Errorf("%s's pod: movable through a registry when evicted", kind)
		}
		if _, err := MovableAtShutdown(p); err != nil || !OwnedRestore(p) {
			t.Errorf("%s's pod at shutdown: %v, owned %v", kind, err, OwnedRestore(p))
		}
	}
}
