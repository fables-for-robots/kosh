// Derived from k9s's internal/render/pv.go, Copyright Authors of K9s, Apache-2.0 (see LICENSES/Apache-2.0.txt); changed for kosh.

package render

import (
	"fmt"
	"path"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

type persistentVolume struct{}

func init() {
	Register(PersistentVolumes, persistentVolume{}, func() runtime.Object { return &corev1.PersistentVolume{} })
}

func (persistentVolume) Columns() []Column {
	return []Column{
		{Name: "NAME"},
		{Name: "CAPACITY", Numeric: true},
		{Name: "ACCESS MODES"},
		{Name: "RECLAIM POLICY"},
		{Name: "STATUS"},
		{Name: "CLAIM"},
		{Name: "STORAGECLASS"},
		{Name: "REASON"},
		{Name: "VOLUMEMODE", Wide: true},
		{Name: "LABELS", Wide: true},
		{Name: "VALID", Wide: true},
		{Name: "AGE", Numeric: true},
	}
}

func (persistentVolume) Row(obj runtime.Object, now time.Time) (Row, error) {
	pv, ok := obj.(*corev1.PersistentVolume)
	if !ok {
		return Row{}, fmt.Errorf("persistentvolume renderer: got %T", obj)
	}
	phase := pv.Status.Phase
	if pv.DeletionTimestamp != nil {
		phase = "Terminating"
	}
	claim := ""
	if pv.Spec.ClaimRef != nil {
		claim = path.Join(pv.Spec.ClaimRef.Namespace, pv.Spec.ClaimRef.Name)
	}
	class, found := pv.Annotations[corev1.BetaStorageClassAnnotation]
	if !found {
		class = pv.Spec.StorageClassName
	}
	volumeMode := MissingValue
	if pv.Spec.VolumeMode != nil {
		volumeMode = string(*pv.Spec.VolumeMode)
	}
	capText, capNum := coreStorage(pv.Spec.Capacity)

	b := NewRow(pv)
	reason := ""
	switch phase {
	case corev1.VolumePending, corev1.VolumeReleased:
		b.Health(Pending)
	case corev1.VolumeFailed:
		reason = "failed to delete or recycle"
	}
	b.Text(pv.Name).
		Num(capText, capNum).
		Text(coreAccessModes(pv.Spec.AccessModes)).
		Text(string(pv.Spec.PersistentVolumeReclaimPolicy)).
		Text(string(phase)).
		Text(claim).
		Text(class).
		Text(pv.Status.Reason).
		Text(volumeMode).
		Map(pv.Labels).
		Valid(reason).
		Age(pv.CreationTimestamp.Time, now)
	return b.Row(), nil
}
