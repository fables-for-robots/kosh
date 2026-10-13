package render

import (
	"testing"
	"time"

	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestRolesColumns(t *testing.T) {
	wantColumns(t, Roles, "NAMESPACE", "NAME", "LABELS*", "VALID*", "AGE")
}

func TestRolesRows(t *testing.T) {
	tests := []struct {
		name   string
		obj    *rbacv1.Role
		want   map[string]string
		health Health
	}{
		{
			name: "complete",
			obj: &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{
				Namespace: "prod", Name: "reader", CreationTimestamp: ago(2 * time.Hour),
				Labels: map[string]string{"a": "b"},
			}},
			want: map[string]string{"NAMESPACE": "prod", "NAME": "reader", "LABELS": "a=b", "VALID": "", "AGE": "2h"},
		},
		{
			name: "metadata only",
			obj:  &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: "r"}},
			want: map[string]string{"NAME": "r", "LABELS": MissingValue, "VALID": "", "AGE": UnknownValue},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			row, cells := rowOf(t, Roles, tc.obj)
			wantCells(t, cells, tc.want)
			if row.Health != tc.health {
				t.Errorf("health = %v, want %v", row.Health, tc.health)
			}
		})
	}
}

func TestRolesOrder(t *testing.T) {
	young := &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{CreationTimestamp: ago(time.Minute)}}
	old := &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{CreationTimestamp: ago(time.Hour)}}
	a, _ := rowOf(t, Roles, young)
	b, _ := rowOf(t, Roles, old)
	if orderOf(t, Roles, a, "AGE") >= orderOf(t, Roles, b, "AGE") {
		t.Error("the younger should sort before the older")
	}
}
