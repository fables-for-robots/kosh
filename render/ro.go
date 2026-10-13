// Derived from k9s's internal/render/ro.go, Copyright Authors of K9s, Apache-2.0 (see LICENSES/Apache-2.0.txt); changed for kosh.

package render

import (
	"fmt"
	"time"

	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

type role struct{}

func init() {
	Register(Roles, role{}, func() runtime.Object { return &rbacv1.Role{} })
}

func (role) Columns() []Column {
	return []Column{
		{Name: "NAMESPACE"},
		{Name: "NAME"},
		{Name: "LABELS", Wide: true},
		{Name: "VALID", Wide: true},
		{Name: "AGE", Numeric: true},
	}
}

func (role) Row(obj runtime.Object, now time.Time) (Row, error) {
	ro, ok := obj.(*rbacv1.Role)
	if !ok {
		return Row{}, fmt.Errorf("role renderer: got %T", obj)
	}
	return NewRow(ro).
		Text(ro.Namespace).
		Text(ro.Name).
		Map(ro.Labels).
		Valid("").
		Age(ro.CreationTimestamp.Time, now).
		Row(), nil
}
