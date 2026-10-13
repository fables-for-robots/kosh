package render

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestPersistentVolumeClaimColumns(t *testing.T) {
	wantColumns(t, PersistentVolumeClaims, "NAMESPACE", "NAME", "STATUS", "VOLUME", "CAPACITY", "ACCESS MODES", "STORAGECLASS", "LABELS*", "VALID*", "AGE")
}

func pvcOf(phase corev1.PersistentVolumeClaimPhase) *corev1.PersistentVolumeClaim {
	class := "fast"
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "data", CreationTimestamp: ago(6 * time.Hour), Labels: map[string]string{"app": "db"}},
		Spec: corev1.PersistentVolumeClaimSpec{
			VolumeName:       "pv-1",
			StorageClassName: &class,
			Resources:        corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("5Gi")}},
		},
		Status: corev1.PersistentVolumeClaimStatus{
			Phase:       phase,
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce, corev1.ReadOnlyMany, corev1.ReadWriteOnce},
			Capacity:    corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("10Gi")},
		},
	}
}

func TestPersistentVolumeClaimRows(t *testing.T) {
	pending := pvcOf(corev1.ClaimPending)
	pending.Spec.VolumeName = ""
	pending.Status.AccessModes = nil
	pending.Status.Capacity = nil
	terminating := pvcOf(corev1.ClaimBound)
	now := ago(time.Minute)
	terminating.DeletionTimestamp = &now
	boundNoCap := pvcOf(corev1.ClaimBound)
	boundNoCap.Status.Capacity = nil
	beta := pvcOf(corev1.ClaimBound)
	beta.Annotations = map[string]string{corev1.BetaStorageClassAnnotation: "slow"}

	tests := []struct {
		name   string
		pvc    *corev1.PersistentVolumeClaim
		want   map[string]string
		health Health
	}{
		{"bound", pvcOf(corev1.ClaimBound), map[string]string{
			"NAMESPACE": "shop", "NAME": "data", "STATUS": "Bound", "VOLUME": "pv-1", "CAPACITY": "10Gi",
			"ACCESS MODES": "RWO,ROX", "STORAGECLASS": "fast", "LABELS": "app=db", "VALID": "", "AGE": "6h",
		}, OK},
		{"pending", pending, map[string]string{
			"STATUS": "Pending", "VOLUME": "", "CAPACITY": "", "ACCESS MODES": "", "VALID": "",
		}, Pending},
		{"lost", pvcOf(corev1.ClaimLost), map[string]string{"STATUS": "Lost", "VALID": "unexpected status Lost"}, Error},
		{"terminating", terminating, map[string]string{"STATUS": "Terminating", "VALID": "unexpected status Terminating"}, Error},
		{"bound without status capacity", boundNoCap, map[string]string{"CAPACITY": "0"}, OK},
		{"beta class annotation wins", beta, map[string]string{"STORAGECLASS": "slow"}, OK},
		{"metadata only", &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "bare"}}, map[string]string{
			"NAME": "bare", "STATUS": "", "VOLUME": "", "CAPACITY": "", "ACCESS MODES": "", "STORAGECLASS": "",
			"LABELS": MissingValue, "VALID": "unexpected status ", "AGE": UnknownValue,
		}, Error},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			row, cells := rowOf(t, PersistentVolumeClaims, tt.pvc)
			wantCells(t, cells, tt.want)
			if row.Health != tt.health {
				t.Errorf("health = %v, want %v", row.Health, tt.health)
			}
		})
	}
}

func TestPersistentVolumeClaimOrder(t *testing.T) {
	small := pvcOf(corev1.ClaimBound)
	small.Status.Capacity = corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("900Mi")}
	small.CreationTimestamp = ago(time.Minute)
	big := pvcOf(corev1.ClaimBound) // 10Gi, 6h
	rs, _ := rowOf(t, PersistentVolumeClaims, small)
	rb, _ := rowOf(t, PersistentVolumeClaims, big)
	for _, col := range []string{"CAPACITY", "AGE"} {
		if orderOf(t, PersistentVolumeClaims, rs, col) >= orderOf(t, PersistentVolumeClaims, rb, col) {
			t.Errorf("%s: 900Mi / 1m does not sort before 10Gi / 6h", col)
		}
	}
	if got := orderOf(t, PersistentVolumeClaims, rb, "CAPACITY"); got != 10*1024*1024*1024 {
		t.Errorf("CAPACITY order = %v, want the bytes", got)
	}
}
