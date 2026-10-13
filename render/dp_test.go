package render

import (
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestDeploymentColumns(t *testing.T) {
	wantColumns(t, Deployments, "NAMESPACE", "NAME", "READY", "UP-TO-DATE", "AVAILABLE", "LABELS*", "VALID*", "AGE")
}

// deploymentTest is a deployment that wants want replicas, of which the
// status counts have, updated and available.
func deploymentTest(name string, want, have, updated, available int32, age time.Duration) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: name, Labels: map[string]string{"app": name}, CreationTimestamp: ago(age)},
		Spec:       appsv1.DeploymentSpec{Replicas: &want},
		Status:     appsv1.DeploymentStatus{Replicas: have, UpdatedReplicas: updated, AvailableReplicas: available},
	}
}

func TestDeploymentRows(t *testing.T) {
	tests := []struct {
		name   string
		dp     *appsv1.Deployment
		want   map[string]string
		health Health
	}{
		{
			name: "healthy, every cell",
			dp:   deploymentTest("web", 3, 3, 3, 3, 5*24*time.Hour),
			want: map[string]string{
				"NAMESPACE":  "shop",
				"NAME":       "web",
				"READY":      "3/3",
				"UP-TO-DATE": "3",
				"AVAILABLE":  "3",
				"LABELS":     "app=web",
				"VALID":      "",
				"AGE":        "5d",
			},
			health: OK,
		},
		{
			name:   "fewer available than it has",
			dp:     deploymentTest("short", 3, 3, 2, 1, time.Hour),
			want:   map[string]string{"READY": "1/3", "UP-TO-DATE": "2", "AVAILABLE": "1", "VALID": "desiring 3 replicas got 1 available"},
			health: Error,
		},
		{
			name:   "none available of those it wants",
			dp:     deploymentTest("down", 3, 3, 3, 0, time.Hour),
			want:   map[string]string{"READY": "0/3", "VALID": "desiring 3 replicas got 0 available"},
			health: Error,
		},
		{
			name:   "scaled to zero",
			dp:     deploymentTest("idle", 0, 0, 0, 0, time.Hour),
			want:   map[string]string{"READY": "0/0", "VALID": ""},
			health: Pending,
		},
		{
			name:   "scaling down to zero",
			dp:     deploymentTest("draining", 0, 2, 0, 1, time.Hour),
			want:   map[string]string{"READY": "1/0", "VALID": "desiring 2 replicas got 1 available"},
			health: Pending,
		},
		{
			name: "metadata only",
			dp:   &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "bare", CreationTimestamp: ago(10 * time.Minute)}},
			want: map[string]string{
				"NAMESPACE":  "shop",
				"NAME":       "bare",
				"READY":      "0/0",
				"UP-TO-DATE": "0",
				"AVAILABLE":  "0",
				"LABELS":     MissingValue,
				"VALID":      "",
				"AGE":        "10m",
			},
			health: Pending,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			row, cells := rowOf(t, Deployments, tt.dp)
			wantCells(t, cells, tt.want)
			if row.Health != tt.health {
				t.Errorf("health = %v, want %v", row.Health, tt.health)
			}
		})
	}
}

func TestDeploymentOrder(t *testing.T) {
	few, _ := rowOf(t, Deployments, deploymentTest("few", 9, 9, 2, 2, 3*time.Minute))
	many, _ := rowOf(t, Deployments, deploymentTest("many", 9, 9, 10, 10, 3*time.Hour))
	for _, col := range []string{"UP-TO-DATE", "AVAILABLE", "AGE"} {
		if a, b := orderOf(t, Deployments, few, col), orderOf(t, Deployments, many, col); a >= b {
			t.Errorf("%s: the smaller sorts at %v, the larger at %v", col, a, b)
		}
	}
}
