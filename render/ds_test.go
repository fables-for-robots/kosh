package render

import (
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestDaemonSetColumns(t *testing.T) {
	wantColumns(t, DaemonSets, "NAMESPACE", "NAME", "DESIRED", "CURRENT", "READY", "UP-TO-DATE", "AVAILABLE", "LABELS*", "VALID*", "AGE")
}

// daemonSetTest is a daemon set whose status has the counts given.
func daemonSetTest(name string, desired, current, ready, updated, available int32, age time.Duration) *appsv1.DaemonSet {
	return &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: "kube-system", Name: name, Labels: map[string]string{"k8s-app": name}, CreationTimestamp: ago(age)},
		Status: appsv1.DaemonSetStatus{
			DesiredNumberScheduled: desired,
			CurrentNumberScheduled: current,
			NumberReady:            ready,
			UpdatedNumberScheduled: updated,
			NumberAvailable:        available,
		},
	}
}

func TestDaemonSetRows(t *testing.T) {
	tests := []struct {
		name   string
		ds     *appsv1.DaemonSet
		want   map[string]string
		health Health
	}{
		{
			name: "healthy, every cell",
			ds:   daemonSetTest("kube-proxy", 5, 4, 5, 3, 2, 5*24*time.Hour),
			want: map[string]string{
				"NAMESPACE":  "kube-system",
				"NAME":       "kube-proxy",
				"DESIRED":    "5",
				"CURRENT":    "4",
				"READY":      "5",
				"UP-TO-DATE": "3",
				"AVAILABLE":  "2",
				"LABELS":     "k8s-app=kube-proxy",
				"VALID":      "",
				"AGE":        "5d",
			},
			health: OK,
		},
		{
			name:   "fewer ready than desired",
			ds:     daemonSetTest("cni", 5, 5, 3, 5, 3, time.Hour),
			want:   map[string]string{"DESIRED": "5", "READY": "3", "VALID": "desiring 5 replicas but 3 ready"},
			health: Error,
		},
		{
			name: "metadata only",
			ds:   &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Namespace: "kube-system", Name: "bare", CreationTimestamp: ago(10 * time.Minute)}},
			want: map[string]string{
				"NAMESPACE":  "kube-system",
				"NAME":       "bare",
				"DESIRED":    "0",
				"CURRENT":    "0",
				"READY":      "0",
				"UP-TO-DATE": "0",
				"AVAILABLE":  "0",
				"LABELS":     MissingValue,
				"VALID":      "",
				"AGE":        "10m",
			},
			health: OK,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			row, cells := rowOf(t, DaemonSets, tt.ds)
			wantCells(t, cells, tt.want)
			if row.Health != tt.health {
				t.Errorf("health = %v, want %v", row.Health, tt.health)
			}
		})
	}
}

func TestDaemonSetOrder(t *testing.T) {
	few, _ := rowOf(t, DaemonSets, daemonSetTest("few", 2, 2, 2, 2, 2, 3*time.Minute))
	many, _ := rowOf(t, DaemonSets, daemonSetTest("many", 10, 10, 10, 10, 10, 3*time.Hour))
	for _, col := range []string{"DESIRED", "CURRENT", "READY", "UP-TO-DATE", "AVAILABLE", "AGE"} {
		if a, b := orderOf(t, DaemonSets, few, col), orderOf(t, DaemonSets, many, col); a >= b {
			t.Errorf("%s: the smaller sorts at %v, the larger at %v", col, a, b)
		}
	}
}
