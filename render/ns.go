// Derived from k9s's internal/render/ns.go, Copyright Authors of K9s, Apache-2.0 (see LICENSES/Apache-2.0.txt); changed for kosh.

package render

import (
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

type namespace struct{}

func init() {
	Register(Namespaces, namespace{}, func() runtime.Object { return &corev1.Namespace{} })
}

func (namespace) Columns() []Column {
	return []Column{
		{Name: "NAME"},
		{Name: "STATUS"},
		{Name: "LABELS", Wide: true},
		{Name: "VALID", Wide: true},
		{Name: "AGE", Numeric: true},
	}
}

func (namespace) Row(obj runtime.Object, now time.Time) (Row, error) {
	ns, ok := obj.(*corev1.Namespace)
	if !ok {
		return Row{}, fmt.Errorf("namespace renderer: got %T", obj)
	}
	phase := ns.Status.Phase
	b := NewRow(ns)
	reason := ""
	switch phase {
	case corev1.NamespaceActive:
	case corev1.NamespaceTerminating:
		b.Health(Pending)
	default:
		reason = "namespace not ready"
	}
	b.Text(ns.Name).
		Text(string(phase)).
		Map(ns.Labels).
		Valid(reason).
		Age(ns.CreationTimestamp.Time, now)
	return b.Row(), nil
}
