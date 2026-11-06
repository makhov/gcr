package kubemigrate

import (
	"context"
	"fmt"
	"slices"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// SharedClaims says whether p's claims can move live: each must be bound
// to a volume that is ReadWriteMany (NFS, CephFS, a cloud's file share),
// which the target mounts while the source still has it, and not tied to
// a node. A ReadWriteOnce volume is attached to one node at a time, so
// it would have to be detached and attached again while the pod is
// frozen.
func SharedClaims(ctx context.Context, cs kubernetes.Interface, p *corev1.Pod) error {
	for _, v := range p.Spec.Volumes {
		if v.PersistentVolumeClaim == nil {
			continue
		}
		pvc, err := cs.CoreV1().PersistentVolumeClaims(p.Namespace).Get(ctx, v.PersistentVolumeClaim.ClaimName, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("volume %s: %w", v.Name, err)
		}
		if pvc.Spec.VolumeName == "" {
			return fmt.Errorf("volume %s: claim %s is not bound", v.Name, pvc.Name)
		}
		pv, err := cs.CoreV1().PersistentVolumes().Get(ctx, pvc.Spec.VolumeName, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("volume %s: %w", v.Name, err)
		}
		if why := nodeLocal(pv); why != "" {
			return fmt.Errorf("volume %s: %s %s, so its files would stay on %s", v.Name, pv.Name, why, p.Spec.NodeName)
		}
		if !slices.Contains(pv.Spec.AccessModes, corev1.ReadWriteMany) {
			return fmt.Errorf("volume %s: %s is not ReadWriteMany: only one node at a time can have it (%v)", v.Name, pv.Name, pv.Spec.AccessModes)
		}
	}
	return nil
}
