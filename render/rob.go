// Follows k9s's internal/render/rob.go (Apache-2.0).

package render

import (
	"fmt"
	"strings"
	"time"

	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

type roleBinding struct{}

func init() {
	Register(RoleBindings, roleBinding{}, func() runtime.Object { return &rbacv1.RoleBinding{} })
}

func (roleBinding) Columns() []Column {
	return []Column{
		{Name: "NAMESPACE"},
		{Name: "NAME"},
		{Name: "ROLE"},
		{Name: "KIND"},
		{Name: "SUBJECTS"},
		{Name: "LABELS", Wide: true},
		{Name: "VALID", Wide: true},
		{Name: "AGE", Numeric: true},
	}
}

func (roleBinding) Row(obj runtime.Object, now time.Time) (Row, error) {
	rb, ok := obj.(*rbacv1.RoleBinding)
	if !ok {
		return Row{}, fmt.Errorf("role binding renderer: got %T", obj)
	}
	kind, subjects := rbacSubjects(rb.Subjects)
	return NewRow(rb).
		Text(rb.Namespace).
		Text(rb.Name).
		Text(rb.RoleRef.Name).
		Text(kind).
		Text(subjects).
		Map(rb.Labels).
		Valid("").
		Age(rb.CreationTimestamp.Time, now).
		Row(), nil
}

// rbacSubjects returns the kind and the names of the subjects of a
// binding, for the cells of a role binding and of a cluster role binding.
// As k9s does, the kind is the one of the last subject. A binding
// without subjects has kind NAValue and subjects MissingValue.
func rbacSubjects(ss []rbacv1.Subject) (kind, subjects string) {
	if len(ss) == 0 {
		return NAValue, MissingValue
	}
	names := make([]string, 0, len(ss))
	for _, s := range ss {
		kind = rbacSubjectKind(s.Kind)
		names = append(names, s.Name)
	}
	return kind, strings.Join(names, ",")
}

// rbacSubjectKind shortens the kind of a subject as k9s does; another kind
// is shown in capitals.
func rbacSubjectKind(kind string) string {
	switch kind {
	case "":
		return ""
	case rbacv1.UserKind:
		return "User"
	case rbacv1.GroupKind:
		return "Group"
	case rbacv1.ServiceAccountKind:
		return "SvcAcct"
	}
	return strings.ToUpper(kind)
}
