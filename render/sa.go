// Derived from k9s's internal/render/sa.go, Copyright Authors of K9s, Apache-2.0 (see LICENSES/Apache-2.0.txt); changed for kosh.

package render

import (
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

type serviceAccount struct{}

func init() {
	Register(ServiceAccounts, serviceAccount{}, func() runtime.Object { return &corev1.ServiceAccount{} })
}

func (serviceAccount) Columns() []Column {
	return []Column{
		{Name: "NAMESPACE"},
		{Name: "NAME"},
		{Name: "SECRET", Numeric: true},
		{Name: "LABELS", Wide: true},
		{Name: "VALID", Wide: true},
		{Name: "AGE", Numeric: true},
	}
}

func (serviceAccount) Row(obj runtime.Object, now time.Time) (Row, error) {
	sa, ok := obj.(*corev1.ServiceAccount)
	if !ok {
		return Row{}, fmt.Errorf("service account renderer: got %T", obj)
	}
	return NewRow(sa).
		Text(sa.Namespace).
		Text(sa.Name).
		Int(int64(len(sa.Secrets))).
		Map(sa.Labels).
		Valid("").
		Age(sa.CreationTimestamp.Time, now).
		Row(), nil
}
