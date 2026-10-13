package render

import (
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestCronJobColumns(t *testing.T) {
	wantColumns(t, CronJobs, "NAMESPACE", "NAME", "SCHEDULE", "SUSPEND", "ACTIVE", "LAST_SCHEDULE", "SELECTOR*", "CONTAINERS*", "IMAGES*", "LABELS*", "VALID*", "AGE")
}

// cronJobTest is a cron job with active jobs running, last scheduled
// ago (never when 0), created age ago.
func cronJobTest(name string, active int, last, age time.Duration) *batchv1.CronJob {
	cj := &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{Namespace: "batch", Name: name, Labels: map[string]string{"team": "data"}, CreationTimestamp: ago(age)},
		Spec: batchv1.CronJobSpec{
			Schedule: "*/5 * * * *",
			JobTemplate: batchv1.JobTemplateSpec{Spec: batchv1.JobSpec{
				Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"team": "data"}},
				Template: workloadTestTemplate(),
			}},
		},
	}
	for i := 0; i < active; i++ {
		cj.Status.Active = append(cj.Status.Active, corev1.ObjectReference{Kind: "Job", Name: name})
	}
	if last > 0 {
		at := ago(last)
		cj.Status.LastScheduleTime = &at
	}
	return cj
}

func TestCronJobRows(t *testing.T) {
	suspended := cronJobTest("paused", 0, 0, time.Hour)
	yes := true
	suspended.Spec.Suspend = &yes

	tests := []struct {
		name   string
		cj     *batchv1.CronJob
		want   map[string]string
		health Health
	}{
		{
			name: "every cell",
			cj:   cronJobTest("report", 2, 3*time.Minute, 5*24*time.Hour),
			want: map[string]string{
				"NAMESPACE":     "batch",
				"NAME":          "report",
				"SCHEDULE":      "*/5 * * * *",
				"SUSPEND":       "false",
				"ACTIVE":        "2",
				"LAST_SCHEDULE": "3m",
				"SELECTOR":      "team=data",
				"CONTAINERS":    "migrate,app,log",
				"IMAGES":        "migrate:2,nginx:1.27,busybox:1",
				"LABELS":        "team=data",
				"VALID":         "",
				"AGE":           "5d",
			},
			health: OK,
		},
		{
			name:   "suspended, never scheduled",
			cj:     suspended,
			want:   map[string]string{"SUSPEND": "true", "ACTIVE": "0", "LAST_SCHEDULE": MissingValue, "VALID": ""},
			health: OK,
		},
		{
			name: "metadata only",
			cj:   &batchv1.CronJob{ObjectMeta: metav1.ObjectMeta{Namespace: "batch", Name: "bare", CreationTimestamp: ago(10 * time.Minute)}},
			want: map[string]string{
				"NAMESPACE":     "batch",
				"NAME":          "bare",
				"SCHEDULE":      MissingValue,
				"SUSPEND":       "false",
				"ACTIVE":        "0",
				"LAST_SCHEDULE": MissingValue,
				"SELECTOR":      MissingValue,
				"CONTAINERS":    MissingValue,
				"IMAGES":        MissingValue,
				"LABELS":        MissingValue,
				"VALID":         "",
				"AGE":           "10m",
			},
			health: OK,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			row, cells := rowOf(t, CronJobs, tt.cj)
			wantCells(t, cells, tt.want)
			if row.Health != tt.health {
				t.Errorf("health = %v, want %v", row.Health, tt.health)
			}
		})
	}
}

func TestCronJobOrder(t *testing.T) {
	small, _ := rowOf(t, CronJobs, cronJobTest("small", 1, 3*time.Minute, 5*time.Minute))
	large, _ := rowOf(t, CronJobs, cronJobTest("large", 4, 3*time.Hour, 5*time.Hour))
	for _, col := range []string{"ACTIVE", "LAST_SCHEDULE", "AGE"} {
		if a, b := orderOf(t, CronJobs, small, col), orderOf(t, CronJobs, large, col); a >= b {
			t.Errorf("%s: the smaller sorts at %v, the larger at %v", col, a, b)
		}
	}
}
