package render

import (
	"testing"
	"time"

	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

func pdbInt(n int) *intstr.IntOrString {
	v := intstr.FromInt32(int32(n))
	return &v
}

func pdbPercent(s string) *intstr.IntOrString {
	v := intstr.FromString(s)
	return &v
}

func TestPodDisruptionBudgetsColumns(t *testing.T) {
	wantColumns(t, PodDisruptionBudgets, "NAMESPACE", "NAME", "MIN-AVAILABLE", "MAX-UNAVAILABLE", "ALLOWED-DISRUPTIONS", "CURRENT", "DESIRED", "EXPECTED", "LABELS*", "VALID*", "AGE")
}

func TestPodDisruptionBudgetsRows(t *testing.T) {
	tests := []struct {
		name   string
		obj    *policyv1.PodDisruptionBudget
		want   map[string]string
		health Health
	}{
		{
			name: "as it should be",
			obj: &policyv1.PodDisruptionBudget{
				ObjectMeta: metav1.ObjectMeta{Namespace: "prod", Name: "web", CreationTimestamp: ago(48 * time.Hour), Labels: map[string]string{"a": "b"}},
				Spec:       policyv1.PodDisruptionBudgetSpec{MinAvailable: pdbInt(3)},
				Status:     policyv1.PodDisruptionBudgetStatus{DisruptionsAllowed: 1, CurrentHealthy: 4, DesiredHealthy: 3, ExpectedPods: 5},
			},
			want: map[string]string{
				"NAMESPACE": "prod", "NAME": "web", "MIN-AVAILABLE": "3", "MAX-UNAVAILABLE": NAValue,
				"ALLOWED-DISRUPTIONS": "1", "CURRENT": "4", "DESIRED": "3", "EXPECTED": "5",
				"LABELS": "a=b", "VALID": "", "AGE": "2d",
			},
		},
		{
			name: "a percentage of unavailable pods",
			obj: &policyv1.PodDisruptionBudget{
				ObjectMeta: metav1.ObjectMeta{Name: "p"},
				Spec:       policyv1.PodDisruptionBudgetSpec{MaxUnavailable: pdbPercent("25%")},
			},
			want: map[string]string{"MIN-AVAILABLE": NAValue, "MAX-UNAVAILABLE": "25%", "VALID": ""},
		},
		{
			name: "a percentage of available pods is not checked against the count",
			obj: &policyv1.PodDisruptionBudget{
				ObjectMeta: metav1.ObjectMeta{Name: "p"},
				Spec:       policyv1.PodDisruptionBudgetSpec{MinAvailable: pdbPercent("50%")},
				Status:     policyv1.PodDisruptionBudgetStatus{CurrentHealthy: 0},
			},
			want: map[string]string{"MIN-AVAILABLE": "50%", "VALID": ""},
		},
		{
			name: "fewer healthy pods than it demands",
			obj: &policyv1.PodDisruptionBudget{
				ObjectMeta: metav1.ObjectMeta{Name: "p"},
				Spec:       policyv1.PodDisruptionBudgetSpec{MinAvailable: pdbInt(3)},
				Status:     policyv1.PodDisruptionBudgetStatus{CurrentHealthy: 2, DesiredHealthy: 3, ExpectedPods: 3},
			},
			want:   map[string]string{"VALID": "expected 3 but got 2"},
			health: Error,
		},
		{
			name: "metadata only",
			obj:  &policyv1.PodDisruptionBudget{ObjectMeta: metav1.ObjectMeta{Name: "m"}},
			want: map[string]string{
				"NAME": "m", "MIN-AVAILABLE": NAValue, "MAX-UNAVAILABLE": NAValue, "ALLOWED-DISRUPTIONS": "0",
				"CURRENT": "0", "DESIRED": "0", "EXPECTED": "0", "LABELS": MissingValue, "VALID": "", "AGE": UnknownValue,
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			row, cells := rowOf(t, PodDisruptionBudgets, tc.obj)
			wantCells(t, cells, tc.want)
			if row.Health != tc.health {
				t.Errorf("health = %v, want %v", row.Health, tc.health)
			}
		})
	}
}

func TestPodDisruptionBudgetsOrder(t *testing.T) {
	small := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{CreationTimestamp: ago(time.Minute)},
		Spec:       policyv1.PodDisruptionBudgetSpec{MinAvailable: pdbInt(2), MaxUnavailable: pdbPercent("5%")},
		Status:     policyv1.PodDisruptionBudgetStatus{DisruptionsAllowed: 2, CurrentHealthy: 3, DesiredHealthy: 2, ExpectedPods: 4},
	}
	big := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{CreationTimestamp: ago(time.Hour)},
		Spec:       policyv1.PodDisruptionBudgetSpec{MinAvailable: pdbInt(10), MaxUnavailable: pdbPercent("50%")},
		Status:     policyv1.PodDisruptionBudgetStatus{DisruptionsAllowed: 12, CurrentHealthy: 30, DesiredHealthy: 10, ExpectedPods: 40},
	}
	a, _ := rowOf(t, PodDisruptionBudgets, small)
	b, _ := rowOf(t, PodDisruptionBudgets, big)
	for _, col := range []string{"MIN-AVAILABLE", "MAX-UNAVAILABLE", "ALLOWED-DISRUPTIONS", "CURRENT", "DESIRED", "EXPECTED", "AGE"} {
		if orderOf(t, PodDisruptionBudgets, a, col) >= orderOf(t, PodDisruptionBudgets, b, col) {
			t.Errorf("%s: the smaller should sort before the larger", col)
		}
	}
}
