package render

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestServiceAccountsColumns(t *testing.T) {
	wantColumns(t, ServiceAccounts, "NAMESPACE", "NAME", "SECRET", "LABELS*", "VALID*", "AGE")
}

func TestServiceAccountsRows(t *testing.T) {
	tests := []struct {
		name   string
		obj    *corev1.ServiceAccount
		want   map[string]string
		health Health
	}{
		{
			name: "complete",
			obj: &corev1.ServiceAccount{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "prod", Name: "builder", CreationTimestamp: ago(3 * 24 * time.Hour),
					Labels: map[string]string{"team": "ci", "app": "x"},
				},
				Secrets: []corev1.ObjectReference{{Name: "a"}, {Name: "b"}},
			},
			want: map[string]string{
				"NAMESPACE": "prod", "NAME": "builder", "SECRET": "2",
				"LABELS": "app=x,team=ci", "VALID": "", "AGE": "3d",
			},
		},
		{
			name: "metadata only",
			obj:  &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "default"}},
			want: map[string]string{
				"NAMESPACE": "", "NAME": "default", "SECRET": "0",
				"LABELS": MissingValue, "VALID": "", "AGE": UnknownValue,
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			row, cells := rowOf(t, ServiceAccounts, tc.obj)
			wantCells(t, cells, tc.want)
			if row.Health != tc.health {
				t.Errorf("health = %v, want %v", row.Health, tc.health)
			}
		})
	}
}

func TestServiceAccountsOrder(t *testing.T) {
	few := &corev1.ServiceAccount{Secrets: []corev1.ObjectReference{{Name: "a"}}, ObjectMeta: metav1.ObjectMeta{CreationTimestamp: ago(time.Hour)}}
	many := &corev1.ServiceAccount{Secrets: make([]corev1.ObjectReference, 10), ObjectMeta: metav1.ObjectMeta{CreationTimestamp: ago(48 * time.Hour)}}
	a, _ := rowOf(t, ServiceAccounts, few)
	b, _ := rowOf(t, ServiceAccounts, many)
	if orderOf(t, ServiceAccounts, a, "SECRET") >= orderOf(t, ServiceAccounts, b, "SECRET") {
		t.Error("1 secret should sort before 10")
	}
	if orderOf(t, ServiceAccounts, a, "AGE") >= orderOf(t, ServiceAccounts, b, "AGE") {
		t.Error("the younger should sort before the older")
	}
}
