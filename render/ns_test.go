package render

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestNamespaceColumns(t *testing.T) {
	wantColumns(t, Namespaces, "NAME", "STATUS", "LABELS*", "VALID*", "AGE")
}

func TestNamespaceRows(t *testing.T) {
	ns := func(phase corev1.NamespacePhase) *corev1.Namespace {
		return &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: "shop", CreationTimestamp: ago(2 * 24 * time.Hour), Labels: map[string]string{"team": "a"}},
			Status:     corev1.NamespaceStatus{Phase: phase},
		}
	}
	tests := []struct {
		name   string
		ns     *corev1.Namespace
		want   map[string]string
		health Health
	}{
		{"active", ns(corev1.NamespaceActive),
			map[string]string{"NAME": "shop", "STATUS": "Active", "LABELS": "team=a", "VALID": "", "AGE": "2d"}, OK},
		{"terminating", ns(corev1.NamespaceTerminating),
			map[string]string{"STATUS": "Terminating", "VALID": ""}, Pending},
		{"unknown phase", ns("Weird"),
			map[string]string{"STATUS": "Weird", "VALID": "namespace not ready"}, Error},
		{"metadata only", &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "bare"}},
			map[string]string{"NAME": "bare", "STATUS": "", "LABELS": MissingValue, "VALID": "namespace not ready", "AGE": UnknownValue}, Error},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			row, cells := rowOf(t, Namespaces, tt.ns)
			wantCells(t, cells, tt.want)
			if row.Health != tt.health {
				t.Errorf("health = %v, want %v", row.Health, tt.health)
			}
		})
	}
}

func TestNamespaceOrder(t *testing.T) {
	young, _ := rowOf(t, Namespaces, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{CreationTimestamp: ago(time.Minute)}})
	old, _ := rowOf(t, Namespaces, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{CreationTimestamp: ago(time.Hour)}})
	if orderOf(t, Namespaces, young, "AGE") >= orderOf(t, Namespaces, old, "AGE") {
		t.Error("a younger namespace does not sort before an older one")
	}
}
