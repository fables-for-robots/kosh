package render

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestPersistentVolumeColumns(t *testing.T) {
	wantColumns(t, PersistentVolumes, "NAME", "CAPACITY", "ACCESS MODES", "RECLAIM POLICY", "STATUS", "CLAIM", "STORAGECLASS", "REASON", "VOLUMEMODE*", "LABELS*", "VALID*", "AGE")
}

func pvOf(phase corev1.PersistentVolumePhase) *corev1.PersistentVolume {
	mode := corev1.PersistentVolumeFilesystem
	return &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "pv-1", CreationTimestamp: ago(36 * time.Hour), Labels: map[string]string{"disk": "ssd"}},
		Spec: corev1.PersistentVolumeSpec{
			Capacity:                      corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("20Gi")},
			AccessModes:                   []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany, corev1.ReadWriteOncePod},
			PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimRetain,
			ClaimRef:                      &corev1.ObjectReference{Namespace: "shop", Name: "data"},
			StorageClassName:              "fast",
			VolumeMode:                    &mode,
		},
		Status: corev1.PersistentVolumeStatus{Phase: phase, Reason: "because"},
	}
}

func TestPersistentVolumeRows(t *testing.T) {
	unclaimed := pvOf(corev1.VolumeAvailable)
	unclaimed.Spec.ClaimRef = nil
	unclaimed.Spec.VolumeMode = nil
	terminating := pvOf(corev1.VolumeBound)
	del := ago(time.Minute)
	terminating.DeletionTimestamp = &del

	tests := []struct {
		name   string
		pv     *corev1.PersistentVolume
		want   map[string]string
		health Health
	}{
		{"bound", pvOf(corev1.VolumeBound), map[string]string{
			"NAME": "pv-1", "CAPACITY": "20Gi", "ACCESS MODES": "RWX,RWOP", "RECLAIM POLICY": "Retain", "STATUS": "Bound",
			"CLAIM": "shop/data", "STORAGECLASS": "fast", "REASON": "because", "VOLUMEMODE": "Filesystem",
			"LABELS": "disk=ssd", "VALID": "", "AGE": "36h",
		}, OK},
		{"available, no claim", unclaimed, map[string]string{"STATUS": "Available", "CLAIM": "", "VOLUMEMODE": MissingValue}, OK},
		{"pending", pvOf(corev1.VolumePending), map[string]string{"STATUS": "Pending", "VALID": ""}, Pending},
		{"released", pvOf(corev1.VolumeReleased), map[string]string{"STATUS": "Released", "VALID": ""}, Pending},
		{"failed", pvOf(corev1.VolumeFailed), map[string]string{"STATUS": "Failed", "VALID": "failed to delete or recycle"}, Error},
		{"terminating", terminating, map[string]string{"STATUS": "Terminating"}, OK},
		{"metadata only", &corev1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: "bare"}}, map[string]string{
			"NAME": "bare", "CAPACITY": "0", "ACCESS MODES": "", "RECLAIM POLICY": "", "STATUS": "", "CLAIM": "",
			"STORAGECLASS": "", "REASON": "", "VOLUMEMODE": MissingValue, "LABELS": MissingValue, "VALID": "", "AGE": UnknownValue,
		}, OK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			row, cells := rowOf(t, PersistentVolumes, tt.pv)
			wantCells(t, cells, tt.want)
			if row.Health != tt.health {
				t.Errorf("health = %v, want %v", row.Health, tt.health)
			}
		})
	}
}

func TestPersistentVolumeOrder(t *testing.T) {
	small := pvOf(corev1.VolumeBound)
	small.Spec.Capacity = corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("500Mi")}
	small.CreationTimestamp = ago(time.Minute)
	big := pvOf(corev1.VolumeBound)
	rs, _ := rowOf(t, PersistentVolumes, small)
	rb, _ := rowOf(t, PersistentVolumes, big)
	for _, col := range []string{"CAPACITY", "AGE"} {
		if orderOf(t, PersistentVolumes, rs, col) >= orderOf(t, PersistentVolumes, rb, col) {
			t.Errorf("%s: 500Mi / 1m does not sort before 20Gi / 36h", col)
		}
	}
}
