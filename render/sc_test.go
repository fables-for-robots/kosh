package render

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestStorageClassColumns(t *testing.T) {
	wantColumns(t, StorageClasses, "NAME", "PROVISIONER", "RECLAIMPOLICY", "VOLUMEBINDINGMODE", "ALLOWVOLUMEEXPANSION", "LABELS*", "VALID*", "AGE")
}

func TestStorageClassRows(t *testing.T) {
	policy := corev1.PersistentVolumeReclaimDelete
	mode := storagev1.VolumeBindingWaitForFirstConsumer
	yes, no := true, false
	tests := []struct {
		name string
		sc   *storagev1.StorageClass
		want map[string]string
	}{
		{"default class", &storagev1.StorageClass{
			ObjectMeta:           metav1.ObjectMeta{Name: "fast", CreationTimestamp: ago(72 * time.Hour), Labels: map[string]string{"k": "v"}, Annotations: map[string]string{"storageclass.kubernetes.io/is-default-class": "true"}},
			Provisioner:          "ebs.csi.aws.com",
			ReclaimPolicy:        &policy,
			VolumeBindingMode:    &mode,
			AllowVolumeExpansion: &yes,
		}, map[string]string{
			"NAME": "fast (default)", "PROVISIONER": "ebs.csi.aws.com", "RECLAIMPOLICY": "Delete",
			"VOLUMEBINDINGMODE": "WaitForFirstConsumer", "ALLOWVOLUMEEXPANSION": "true", "LABELS": "k=v", "VALID": "", "AGE": "3d",
		}},
		{"beta default annotation", &storagev1.StorageClass{
			ObjectMeta: metav1.ObjectMeta{Name: "old", Annotations: map[string]string{"storageclass.beta.kubernetes.io/is-default-class": "true"}},
		}, map[string]string{"NAME": "old (default)"}},
		{"default annotation false", &storagev1.StorageClass{
			ObjectMeta:           metav1.ObjectMeta{Name: "slow", Annotations: map[string]string{"storageclass.kubernetes.io/is-default-class": "false"}},
			AllowVolumeExpansion: &no,
		}, map[string]string{"NAME": "slow", "ALLOWVOLUMEEXPANSION": "false"}},
		{"metadata only", &storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: "bare"}}, map[string]string{
			"NAME": "bare", "PROVISIONER": "", "RECLAIMPOLICY": "", "VOLUMEBINDINGMODE": "", "ALLOWVOLUMEEXPANSION": "false",
			"LABELS": MissingValue, "VALID": "", "AGE": UnknownValue,
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			row, cells := rowOf(t, StorageClasses, tt.sc)
			wantCells(t, cells, tt.want)
			if row.Health != OK {
				t.Errorf("health = %v", row.Health)
			}
			if row.Name != tt.sc.Name {
				t.Errorf("row name = %q, want the object's %q", row.Name, tt.sc.Name)
			}
		})
	}
}

func TestStorageClassOrder(t *testing.T) {
	young, _ := rowOf(t, StorageClasses, &storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{CreationTimestamp: ago(time.Minute)}})
	old, _ := rowOf(t, StorageClasses, &storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{CreationTimestamp: ago(time.Hour)}})
	if orderOf(t, StorageClasses, young, "AGE") >= orderOf(t, StorageClasses, old, "AGE") {
		t.Error("a younger class does not sort before an older one")
	}
}
