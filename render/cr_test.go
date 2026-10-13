package render

import (
	"testing"
	"time"

	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestClusterRolesColumns(t *testing.T) {
	wantColumns(t, ClusterRoles, "NAME", "LABELS*", "AGE")
}

func TestClusterRolesRows(t *testing.T) {
	tests := []struct {
		name   string
		obj    *rbacv1.ClusterRole
		want   map[string]string
		health Health
	}{
		{
			name: "complete",
			obj: &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{
				Name: "admin", CreationTimestamp: ago(5 * 24 * time.Hour),
				Labels: map[string]string{"rbac": "yes"},
			}},
			want: map[string]string{"NAME": "admin", "LABELS": "rbac=yes", "AGE": "5d"},
		},
		{
			name: "metadata only",
			obj:  &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: "c"}},
			want: map[string]string{"NAME": "c", "LABELS": MissingValue, "AGE": UnknownValue},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			row, cells := rowOf(t, ClusterRoles, tc.obj)
			wantCells(t, cells, tc.want)
			if row.Namespace != "" {
				t.Errorf("namespace = %q", row.Namespace)
			}
			if row.Health != tc.health {
				t.Errorf("health = %v, want %v", row.Health, tc.health)
			}
		})
	}
}

func TestClusterRolesOrder(t *testing.T) {
	young := &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{CreationTimestamp: ago(time.Minute)}}
	old := &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{CreationTimestamp: ago(time.Hour)}}
	a, _ := rowOf(t, ClusterRoles, young)
	b, _ := rowOf(t, ClusterRoles, old)
	if orderOf(t, ClusterRoles, a, "AGE") >= orderOf(t, ClusterRoles, b, "AGE") {
		t.Error("the younger should sort before the older")
	}
}
