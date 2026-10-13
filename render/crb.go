// Derived from k9s's internal/render/crb.go, Copyright Authors of K9s, Apache-2.0 (see LICENSES/Apache-2.0.txt); changed for kosh.

package render

import (
	"fmt"
	"time"

	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

type clusterRoleBinding struct{}

func init() {
	Register(ClusterRoleBindings, clusterRoleBinding{}, func() runtime.Object { return &rbacv1.ClusterRoleBinding{} })
}

func (clusterRoleBinding) Columns() []Column {
	return []Column{
		{Name: "NAME"},
		{Name: "CLUSTERROLE"},
		{Name: "SUBJECT-KIND"},
		{Name: "SUBJECTS"},
		{Name: "LABELS", Wide: true},
		{Name: "AGE", Numeric: true},
	}
}

func (clusterRoleBinding) Row(obj runtime.Object, now time.Time) (Row, error) {
	crb, ok := obj.(*rbacv1.ClusterRoleBinding)
	if !ok {
		return Row{}, fmt.Errorf("cluster role binding renderer: got %T", obj)
	}
	kind, subjects := rbacSubjects(crb.Subjects)
	return NewRow(crb).
		Text(crb.Name).
		Text(Or(crb.RoleRef.Name)).
		Text(kind).
		Text(subjects).
		Map(crb.Labels).
		Age(crb.CreationTimestamp.Time, now).
		Row(), nil
}
