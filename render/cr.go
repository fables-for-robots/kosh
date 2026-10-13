// Follows k9s's internal/render/cr.go (Apache-2.0).

package render

import (
	"fmt"
	"time"

	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

type clusterRole struct{}

func init() {
	Register(ClusterRoles, clusterRole{}, func() runtime.Object { return &rbacv1.ClusterRole{} })
}

func (clusterRole) Columns() []Column {
	return []Column{
		{Name: "NAME"},
		{Name: "LABELS", Wide: true},
		{Name: "AGE", Numeric: true},
	}
}

func (clusterRole) Row(obj runtime.Object, now time.Time) (Row, error) {
	cr, ok := obj.(*rbacv1.ClusterRole)
	if !ok {
		return Row{}, fmt.Errorf("cluster role renderer: got %T", obj)
	}
	return NewRow(cr).
		Text(cr.Name).
		Map(cr.Labels).
		Age(cr.CreationTimestamp.Time, now).
		Row(), nil
}
