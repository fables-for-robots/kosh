package render

import (
	"testing"
	"time"

	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestRoleBindingsColumns(t *testing.T) {
	wantColumns(t, RoleBindings, "NAMESPACE", "NAME", "ROLE", "KIND", "SUBJECTS", "LABELS*", "VALID*", "AGE")
}

func TestRoleBindingsRows(t *testing.T) {
	tests := []struct {
		name   string
		obj    *rbacv1.RoleBinding
		want   map[string]string
		health Health
	}{
		{
			name: "service accounts",
			obj: &rbacv1.RoleBinding{
				ObjectMeta: metav1.ObjectMeta{Namespace: "prod", Name: "bind", CreationTimestamp: ago(4 * time.Hour), Labels: map[string]string{"k": "v"}},
				RoleRef:    rbacv1.RoleRef{Kind: "Role", Name: "reader"},
				Subjects: []rbacv1.Subject{
					{Kind: rbacv1.ServiceAccountKind, Name: "a"},
					{Kind: rbacv1.ServiceAccountKind, Name: "b"},
				},
			},
			want: map[string]string{
				"NAMESPACE": "prod", "NAME": "bind", "ROLE": "reader", "KIND": "SvcAcct",
				"SUBJECTS": "a,b", "LABELS": "k=v", "VALID": "", "AGE": "4h",
			},
		},
		{
			name: "a user",
			obj: &rbacv1.RoleBinding{
				ObjectMeta: metav1.ObjectMeta{Name: "u"},
				Subjects:   []rbacv1.Subject{{Kind: rbacv1.UserKind, Name: "alice"}},
			},
			want: map[string]string{"KIND": "User", "SUBJECTS": "alice"},
		},
		{
			name: "a group",
			obj: &rbacv1.RoleBinding{
				ObjectMeta: metav1.ObjectMeta{Name: "g"},
				Subjects:   []rbacv1.Subject{{Kind: rbacv1.GroupKind, Name: "devs"}},
			},
			want: map[string]string{"KIND": "Group", "SUBJECTS": "devs"},
		},
		{
			name: "another kind of subject is in capitals",
			obj: &rbacv1.RoleBinding{
				ObjectMeta: metav1.ObjectMeta{Name: "x"},
				Subjects:   []rbacv1.Subject{{Kind: "Robot", Name: "r2"}},
			},
			want: map[string]string{"KIND": "ROBOT", "SUBJECTS": "r2"},
		},
		{
			name: "metadata only",
			obj:  &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "m"}},
			want: map[string]string{
				"NAME": "m", "ROLE": "", "KIND": NAValue, "SUBJECTS": MissingValue,
				"LABELS": MissingValue, "VALID": "", "AGE": UnknownValue,
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			row, cells := rowOf(t, RoleBindings, tc.obj)
			wantCells(t, cells, tc.want)
			if row.Health != tc.health {
				t.Errorf("health = %v, want %v", row.Health, tc.health)
			}
		})
	}
}

func TestRoleBindingsOrder(t *testing.T) {
	young := &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{CreationTimestamp: ago(time.Minute)}}
	old := &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{CreationTimestamp: ago(time.Hour)}}
	a, _ := rowOf(t, RoleBindings, young)
	b, _ := rowOf(t, RoleBindings, old)
	if orderOf(t, RoleBindings, a, "AGE") >= orderOf(t, RoleBindings, b, "AGE") {
		t.Error("the younger should sort before the older")
	}
}
