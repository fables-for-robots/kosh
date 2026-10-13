// Derived from k9s's internal/render/pvc.go, Copyright Authors of K9s, Apache-2.0 (see LICENSES/Apache-2.0.txt); changed for kosh.

package render

import (
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

type persistentVolumeClaim struct{}

func init() {
	Register(PersistentVolumeClaims, persistentVolumeClaim{}, func() runtime.Object { return &corev1.PersistentVolumeClaim{} })
}

func (persistentVolumeClaim) Columns() []Column {
	return []Column{
		{Name: "NAMESPACE"},
		{Name: "NAME"},
		{Name: "STATUS"},
		{Name: "VOLUME"},
		{Name: "CAPACITY", Numeric: true},
		{Name: "ACCESS MODES"},
		{Name: "STORAGECLASS"},
		{Name: "LABELS", Wide: true},
		{Name: "VALID", Wide: true},
		{Name: "AGE", Numeric: true},
	}
}

// Row follows k9s: a claim that is not bound to a volume has no capacity
// and no access modes to show.
func (persistentVolumeClaim) Row(obj runtime.Object, now time.Time) (Row, error) {
	pvc, ok := obj.(*corev1.PersistentVolumeClaim)
	if !ok {
		return Row{}, fmt.Errorf("persistentvolumeclaim renderer: got %T", obj)
	}
	phase := string(pvc.Status.Phase)
	if pvc.DeletionTimestamp != nil {
		phase = "Terminating"
	}

	var accessModes, capText string
	var capNum float64
	if pvc.Spec.VolumeName != "" {
		accessModes = coreAccessModes(pvc.Status.AccessModes)
		capText, capNum = coreStorage(pvc.Status.Capacity)
	}
	class, found := pvc.Annotations[corev1.BetaStorageClassAnnotation]
	if !found && pvc.Spec.StorageClassName != nil {
		class = *pvc.Spec.StorageClassName
	}

	b := NewRow(pvc)
	reason := ""
	switch phase {
	case "Bound", "Available":
	case "Pending":
		// The brief's one departure from k9s besides Lost: a claim
		// that waits for its volume is on its way, not invalid.
		b.Health(Pending)
	default:
		// As k9s: any other phase, an empty one and Terminating
		// included, is invalid. A claim held by the
		// pvc-protection finalizer is shown red on purpose.
		reason = "unexpected status " + phase
	}
	b.Text(pvc.Namespace).
		Text(pvc.Name).
		Text(phase).
		Text(pvc.Spec.VolumeName).
		Num(capText, capNum).
		Text(accessModes).
		Text(class).
		Map(pvc.Labels).
		Valid(reason).
		Age(pvc.CreationTimestamp.Time, now)
	return b.Row(), nil
}

// coreAccessModes is the access modes in k9s's short form ("RWO,ROX"),
// each once, in the order they come.
func coreAccessModes(modes []corev1.PersistentVolumeAccessMode) string {
	seen := map[corev1.PersistentVolumeAccessMode]bool{}
	var out []string
	for _, m := range modes {
		if seen[m] {
			continue
		}
		seen[m] = true
		switch m {
		case corev1.ReadWriteOnce:
			out = append(out, "RWO")
		case corev1.ReadOnlyMany:
			out = append(out, "ROX")
		case corev1.ReadWriteMany:
			out = append(out, "RWX")
		case corev1.ReadWriteOncePod:
			out = append(out, "RWOP")
		}
	}
	return strings.Join(out, ",")
}

// coreStorage is the storage quantity of the list as it is written
// ("10Gi") and its value in bytes. As k9s does, a list without one shows
// the zero quantity, "0".
func coreStorage(rl corev1.ResourceList) (string, float64) {
	q := rl[corev1.ResourceStorage]
	return q.String(), q.AsApproximateFloat64()
}
