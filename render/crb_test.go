package render

import (
	"testing"
	"time"

	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestClusterRoleBindingsColumns(t *testing.T) {
	wantColumns(t, ClusterRoleBindings, "NAME", "CLUSTERROLE", "SUBJECT-KIND", "SUBJECTS", "LABELS*", "AGE")
}

func TestClusterRoleBindingsRows(t *testing.T) {
	tests := []struct {
		name   string
		obj    *rbacv1.ClusterRoleBinding
		want   map[string]string
		health Health
	}{
		{
			name: "complete",
			obj: &rbacv1.ClusterRoleBinding{
				ObjectMeta: metav1.ObjectMeta{Name: "admins", CreationTimestamp: ago(30 * time.Hour), Labels: map[string]string{"k": "v"}},
				RoleRef:    rbacv1.RoleRef{Kind: "ClusterRole", Name: "cluster-admin"},
				Subjects: []rbacv1.Subject{
					{Kind: rbacv1.GroupKind, Name: "system:masters"},
					{Kind: rbacv1.GroupKind, Name: "ops"},
				},
			},
			want: map[string]string{
				"NAME": "admins", "CLUSTERROLE": "cluster-admin", "SUBJECT-KIND": "Group",
				"SUBJECTS": "system:masters,ops", "LABELS": "k=v", "AGE": "30h",
			},
		},
		{
			name: "metadata only",
			obj:  &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "m"}},
			want: map[string]string{
				"NAME": "m", "CLUSTERROLE": "", "SUBJECT-KIND": NAValue, "SUBJECTS": MissingValue,
				"LABELS": MissingValue, "AGE": UnknownValue,
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			row, cells := rowOf(t, ClusterRoleBindings, tc.obj)
			wantCells(t, cells, tc.want)
			if row.Health != tc.health {
				t.Errorf("health = %v, want %v", row.Health, tc.health)
			}
		})
	}
}

func TestClusterRoleBindingsOrder(t *testing.T) {
	young := &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{CreationTimestamp: ago(time.Minute)}}
	old := &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{CreationTimestamp: ago(time.Hour)}}
	a, _ := rowOf(t, ClusterRoleBindings, young)
	b, _ := rowOf(t, ClusterRoleBindings, old)
	if orderOf(t, ClusterRoleBindings, a, "AGE") >= orderOf(t, ClusterRoleBindings, b, "AGE") {
		t.Error("the younger should sort before the older")
	}
}
