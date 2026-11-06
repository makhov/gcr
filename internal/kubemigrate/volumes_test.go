package kubemigrate

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestSharedClaims(t *testing.T) {
	claimed := func(claim string) *corev1.Pod {
		return running(func(p *corev1.Pod) {
			p.Spec.Volumes = []corev1.Volume{{Name: "data", VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: claim}}}}
		})
	}
	pvc := func(name, volume string) *corev1.PersistentVolumeClaim {
		return &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec: corev1.PersistentVolumeClaimSpec{VolumeName: volume}}
	}
	pv := func(name string, mode corev1.PersistentVolumeAccessMode, src corev1.PersistentVolumeSource) *corev1.PersistentVolume {
		return &corev1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: corev1.PersistentVolumeSpec{AccessModes: []corev1.PersistentVolumeAccessMode{mode}, PersistentVolumeSource: src}}
	}
	nfs := corev1.PersistentVolumeSource{NFS: &corev1.NFSVolumeSource{Server: "nfs", Path: "/srv"}}
	local := corev1.PersistentVolumeSource{Local: &corev1.LocalVolumeSource{Path: "/data"}}
	cs := fake.NewClientset(
		pvc("shared", "nfs-rwx"), pv("nfs-rwx", corev1.ReadWriteMany, nfs),
		pvc("disk", "nfs-rwo"), pv("nfs-rwo", corev1.ReadWriteOnce, nfs),
		pvc("local", "local-rwx"), pv("local-rwx", corev1.ReadWriteMany, local),
		pvc("pending", ""),
	)
	for claim, refuse := range map[string]string{
		"shared":  "",
		"disk":    "not ReadWriteMany",
		"local":   "is a local volume",
		"pending": "not bound",
		"missing": "not found",
	} {
		err := SharedClaims(context.Background(), cs, claimed(claim))
		if refuse == "" && err != nil || refuse != "" && (err == nil || !strings.Contains(err.Error(), refuse)) {
			t.Errorf("claim %s: %v; want %q", claim, err, refuse)
		}
	}
	// Movable itself leaves claims to SharedClaims.
	if _, err := Movable(claimed("disk"), ""); err != nil {
		t.Errorf("Movable: %v", err)
	}
}
