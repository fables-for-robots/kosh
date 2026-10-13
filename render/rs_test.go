package render

import (
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestReplicaSetColumns(t *testing.T) {
	wantColumns(t, ReplicaSets, "NAMESPACE", "NAME", "DESIRED", "CURRENT", "READY", "CONTAINERS*", "IMAGES*", "SELECTOR*", "VALID*", "AGE")
}

// workloadTestTemplate is a pod template with an init container and two
// containers.
func workloadTestTemplate() corev1.PodTemplateSpec {
	return corev1.PodTemplateSpec{Spec: corev1.PodSpec{
		InitContainers: []corev1.Container{{Name: "migrate", Image: "migrate:2"}},
		Containers: []corev1.Container{
			{Name: "app", Image: "nginx:1.27"},
			{Name: "log", Image: "busybox:1"},
		},
	}}
}

// replicaSetTest is a replica set that wants want replicas, of which its
// status counts have and ready.
func replicaSetTest(name string, want, have, ready int32, age time.Duration) *appsv1.ReplicaSet {
	return &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: name, Labels: map[string]string{"app": "web"}, CreationTimestamp: ago(age)},
		Spec: appsv1.ReplicaSetSpec{
			Replicas: &want,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "web", "pod-template-hash": "abc"}},
			Template: workloadTestTemplate(),
		},
		Status: appsv1.ReplicaSetStatus{Replicas: have, ReadyReplicas: ready},
	}
}

func TestReplicaSetRows(t *testing.T) {
	byExpression := replicaSetTest("web-def", 3, 3, 1, time.Hour)
	byExpression.Spec.Selector = &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
		{Key: "tier", Operator: metav1.LabelSelectorOpIn, Values: []string{"a", "b"}},
	}}

	tests := []struct {
		name   string
		rs     *appsv1.ReplicaSet
		want   map[string]string
		health Health
	}{
		{
			name: "healthy, every cell",
			rs:   replicaSetTest("web-abc", 3, 3, 3, 5*24*time.Hour),
			want: map[string]string{
				"NAMESPACE":  "shop",
				"NAME":       "web-abc",
				"DESIRED":    "3",
				"CURRENT":    "3",
				"READY":      "3",
				"CONTAINERS": "app,log",
				"IMAGES":     "nginx:1.27,busybox:1",
				"SELECTOR":   "app=web,pod-template-hash=abc",
				"VALID":      "",
				"AGE":        "5d",
			},
			health: OK,
		},
		{
			name:   "fewer ready than it has",
			rs:     byExpression,
			want:   map[string]string{"CURRENT": "3", "READY": "1", "SELECTOR": "tier in (a,b)", "VALID": "mismatch desired(3) vs ready(1)"},
			health: Error,
		},
		{
			name:   "ready replicas left after scaling to zero",
			rs:     replicaSetTest("web-old", 0, 0, 2, time.Hour),
			want:   map[string]string{"DESIRED": "0", "READY": "2", "VALID": "did not phase down correctly expecting 0 replicas but got 2"},
			health: Error,
		},
		{
			name: "metadata only",
			rs:   &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "bare", CreationTimestamp: ago(10 * time.Minute)}},
			want: map[string]string{
				"NAMESPACE":  "shop",
				"NAME":       "bare",
				"DESIRED":    "0",
				"CURRENT":    "0",
				"READY":      "0",
				"CONTAINERS": MissingValue,
				"IMAGES":     MissingValue,
				"SELECTOR":   MissingValue,
				"VALID":      "",
				"AGE":        "10m",
			},
			health: OK,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			row, cells := rowOf(t, ReplicaSets, tt.rs)
			wantCells(t, cells, tt.want)
			if row.Health != tt.health {
				t.Errorf("health = %v, want %v", row.Health, tt.health)
			}
		})
	}
}

func TestReplicaSetOrder(t *testing.T) {
	few, _ := rowOf(t, ReplicaSets, replicaSetTest("few", 2, 2, 2, 3*time.Minute))
	many, _ := rowOf(t, ReplicaSets, replicaSetTest("many", 10, 10, 10, 3*time.Hour))
	for _, col := range []string{"DESIRED", "CURRENT", "READY", "AGE"} {
		if a, b := orderOf(t, ReplicaSets, few, col), orderOf(t, ReplicaSets, many, col); a >= b {
			t.Errorf("%s: the smaller sorts at %v, the larger at %v", col, a, b)
		}
	}
}
