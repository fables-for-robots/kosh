// Derived from k9s's internal/render/sc.go, Copyright Authors of K9s, Apache-2.0 (see LICENSES/Apache-2.0.txt); changed for kosh.

package render

import (
	"fmt"
	"strconv"
	"time"

	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

type storageClass struct{}

func init() {
	Register(StorageClasses, storageClass{}, func() runtime.Object { return &storagev1.StorageClass{} })
}

func (storageClass) Columns() []Column {
	return []Column{
		{Name: "NAME"},
		{Name: "PROVISIONER"},
		{Name: "RECLAIMPOLICY"},
		{Name: "VOLUMEBINDINGMODE"},
		{Name: "ALLOWVOLUMEEXPANSION"},
		{Name: "LABELS", Wide: true},
		{Name: "VALID", Wide: true},
		{Name: "AGE", Numeric: true},
	}
}

func (storageClass) Row(obj runtime.Object, now time.Time) (Row, error) {
	sc, ok := obj.(*storagev1.StorageClass)
	if !ok {
		return Row{}, fmt.Errorf("storageclass renderer: got %T", obj)
	}
	name := sc.Name
	if scIsDefault(sc) {
		name += " (default)"
	}
	reclaim, binding := "", ""
	if sc.ReclaimPolicy != nil {
		reclaim = string(*sc.ReclaimPolicy)
	}
	if sc.VolumeBindingMode != nil {
		binding = string(*sc.VolumeBindingMode)
	}
	expand := sc.AllowVolumeExpansion != nil && *sc.AllowVolumeExpansion

	b := NewRow(sc).
		Text(name).
		Text(sc.Provisioner).
		Text(reclaim).
		Text(binding).
		Text(strconv.FormatBool(expand)).
		Map(sc.Labels).
		Valid("").
		Age(sc.CreationTimestamp.Time, now)
	return b.Row(), nil
}

// scIsDefault: the class carries the annotation that marks it the
// default, in its current or its beta spelling.
func scIsDefault(sc *storagev1.StorageClass) bool {
	return sc.Annotations["storageclass.kubernetes.io/is-default-class"] == "true" ||
		sc.Annotations["storageclass.beta.kubernetes.io/is-default-class"] == "true"
}
