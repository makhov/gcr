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
