// Derived from k9s's internal/render/cm.go, Copyright Authors of K9s, Apache-2.0 (see LICENSES/Apache-2.0.txt); changed for kosh.

package render

import (
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

type configMap struct{}

func init() {
	Register(ConfigMaps, configMap{}, func() runtime.Object { return &corev1.ConfigMap{} })
}

func (configMap) Columns() []Column {
	return []Column{
		{Name: "NAMESPACE"},
		{Name: "NAME"},
		{Name: "DATA", Numeric: true},
		{Name: "VALID", Wide: true},
		{Name: "AGE", Numeric: true},
	}
}

// Row counts both the text and the binary entries, as kubectl does; k9s
// counts the text ones only.
func (configMap) Row(obj runtime.Object, now time.Time) (Row, error) {
	cm, ok := obj.(*corev1.ConfigMap)
	if !ok {
		return Row{}, fmt.Errorf("configmap renderer: got %T", obj)
	}
	b := NewRow(cm).
		Text(cm.Namespace).
		Text(cm.Name).
		Int(int64(len(cm.Data)+len(cm.BinaryData))).
		Valid("").
		Age(cm.CreationTimestamp.Time, now)
	return b.Row(), nil
}
