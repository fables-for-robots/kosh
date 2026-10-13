package render

import (
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestStatefulSetColumns(t *testing.T) {
	wantColumns(t, StatefulSets, "NAMESPACE", "NAME", "READY", "SELECTOR*", "SERVICE", "CONTAINERS*", "IMAGES*", "LABELS*", "VALID*", "AGE")
}

// statefulSetTest is a stateful set that wants want replicas, of which
// its status counts have and ready.
func statefulSetTest(name string, want, have, ready int32, age time.Duration) *appsv1.StatefulSet {
	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: name, Labels: map[string]string{"app": "db"}, CreationTimestamp: ago(age)},
		Spec: appsv1.StatefulSetSpec{
			Replicas:    &want,
			Selector:    &metav1.LabelSelector{MatchLabels: map[string]string{"app": "db"}},
			ServiceName: "db-headless",
			Template:    workloadTestTemplate(),
		},
		Status: appsv1.StatefulSetStatus{Replicas: have, ReadyReplicas: ready},
	}
}

func TestStatefulSetRows(t *testing.T) {
	tests := []struct {
		name   string
		sts    *appsv1.StatefulSet
		want   map[string]string
		health Health
	}{
		{
			name: "healthy, every cell",
			sts:  statefulSetTest("db", 3, 3, 3, 5*24*time.Hour),
			want: map[string]string{
				"NAMESPACE":  "shop",
				"NAME":       "db",
				"READY":      "3/3",
				"SELECTOR":   "app=db",
				"SERVICE":    "db-headless",
				"CONTAINERS": "migrate,app,log",
				"IMAGES":     "migrate:2,nginx:1.27,busybox:1",
				"LABELS":     "app=db",
				"VALID":      "",
				"AGE":        "5d",
			},
			health: OK,
		},
		{
			name:   "fewer ready than it has",
			sts:    statefulSetTest("db", 3, 3, 2, time.Hour),
			want:   map[string]string{"READY": "2/3", "VALID": "desired 3 replicas got 2 available"},
			health: Error,
		},
		{
			name:   "fewer than it wants",
			sts:    statefulSetTest("db", 3, 2, 2, time.Hour),
			want:   map[string]string{"READY": "2/3", "VALID": "want 3 replicas got 2 available"},
			health: Error,
		},
		{
			name: "metadata only",
			sts:  &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "bare", CreationTimestamp: ago(10 * time.Minute)}},
			want: map[string]string{
				"NAMESPACE":  "shop",
				"NAME":       "bare",
				"READY":      "0/0",
				"SELECTOR":   MissingValue,
				"SERVICE":    NAValue,
				"CONTAINERS": MissingValue,
				"IMAGES":     MissingValue,
				"LABELS":     MissingValue,
				"VALID":      "",
				"AGE":        "10m",
			},
			health: OK,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			row, cells := rowOf(t, StatefulSets, tt.sts)
			wantCells(t, cells, tt.want)
			if row.Health != tt.health {
				t.Errorf("health = %v, want %v", row.Health, tt.health)
			}
		})
	}
}

func TestStatefulSetOrder(t *testing.T) {
	young, _ := rowOf(t, StatefulSets, statefulSetTest("young", 1, 1, 1, 3*time.Minute))
	old, _ := rowOf(t, StatefulSets, statefulSetTest("old", 1, 1, 1, 3*time.Hour))
	if a, b := orderOf(t, StatefulSets, young, "AGE"), orderOf(t, StatefulSets, old, "AGE"); a >= b {
		t.Errorf("AGE: 3m sorts at %v, 3h at %v", a, b)
	}
}
