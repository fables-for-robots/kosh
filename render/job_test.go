package render

import (
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestJobColumns(t *testing.T) {
	wantColumns(t, Jobs, "NAMESPACE", "NAME", "COMPLETIONS", "DURATION", "SELECTOR*", "CONTAINERS*", "IMAGES*", "VALID*", "AGE")
}

// jobTest is a job that wants completions (none when nil), with
// succeeded and failed pods, that started ago and ran for took (still
// running when took is 0).
func jobTest(name string, completions *int32, succeeded, failed int32, started, took time.Duration) *batchv1.Job {
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Namespace: "batch", Name: name, CreationTimestamp: ago(started + time.Minute)},
		Spec: batchv1.JobSpec{
			Completions: completions,
			Selector:    &metav1.LabelSelector{MatchLabels: map[string]string{"batch.kubernetes.io/controller-uid": "u1"}},
			Template:    workloadTestTemplate(),
		},
		Status: batchv1.JobStatus{Succeeded: succeeded, Failed: failed},
	}
	start := ago(started)
	job.Status.StartTime = &start
	if took > 0 {
		done := metav1.NewTime(start.Add(took))
		job.Status.CompletionTime = &done
	}
	return job
}

func TestJobRows(t *testing.T) {
	three, one := int32(3), int32(1)
	parallel := int32(3)
	queue := func(name string, succeeded, failed, active int32, took time.Duration) *batchv1.Job {
		j := jobTest(name, nil, succeeded, failed, time.Hour, took)
		j.Spec.Parallelism = &parallel
		j.Status.Active = active
		return j
	}
	workQueue := queue("queue", 0, 0, 3, 0)
	completeByCondition := jobTest("by-condition", &one, 1, 0, time.Hour, 0)
	completeByCondition.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: "True"}}

	tests := []struct {
		name   string
		job    *batchv1.Job
		want   map[string]string
		health Health
	}{
		{
			name: "completed, every cell",
			job:  jobTest("backup", &three, 3, 0, 5*24*time.Hour, 90*time.Second),
			want: map[string]string{
				"NAMESPACE":   "batch",
				"NAME":        "backup",
				"COMPLETIONS": "3/3",
				"DURATION":    "90s",
				"SELECTOR":    "batch.kubernetes.io/controller-uid=u1",
				"CONTAINERS":  "migrate,app,(+1)...",
				"IMAGES":      "migrate:2,nginx:1.27,(+1)...",
				"VALID":       "",
				"AGE":         "5d",
			},
			health: Completed,
		},
		{
			name:   "running",
			job:    jobTest("running", &three, 1, 0, time.Hour, 0),
			want:   map[string]string{"COMPLETIONS": "1/3", "DURATION": MissingValue, "VALID": ""},
			health: OK,
		},
		{
			name:   "pods failed",
			job:    jobTest("failing", &one, 0, 2, time.Hour, 0),
			want:   map[string]string{"COMPLETIONS": "0/1", "VALID": "2 pods failed"},
			health: Error,
		},
		{
			name:   "a pod failed and it completed after all",
			job:    jobTest("retried", &one, 1, 1, time.Hour, time.Minute),
			want:   map[string]string{"COMPLETIONS": "1/1", "DURATION": "60s", "VALID": ""},
			health: Completed,
		},
		{
			name:   "a work queue",
			job:    workQueue,
			want:   map[string]string{"COMPLETIONS": "0/1 of 3", "VALID": ""},
			health: OK,
		},
		{
			name:   "a work queue, one pod succeeded and others still running",
			job:    queue("queue-busy", 1, 0, 2, 0),
			want:   map[string]string{"COMPLETIONS": "1/1 of 3", "DURATION": MissingValue, "VALID": ""},
			health: OK,
		},
		{
			name:   "a work queue, completed",
			job:    queue("queue-done", 3, 0, 0, 20*time.Minute),
			want:   map[string]string{"COMPLETIONS": "3/1 of 3", "DURATION": "20m", "VALID": ""},
			health: Completed,
		},
		{
			name:   "a work queue, pods failed while one succeeded",
			job:    queue("queue-failing", 1, 2, 2, 0),
			want:   map[string]string{"COMPLETIONS": "1/1 of 3", "VALID": "2 pods failed"},
			health: Error,
		},
		{
			name:   "complete by its condition, no completion time",
			job:    completeByCondition,
			want:   map[string]string{"COMPLETIONS": "1/1", "DURATION": MissingValue, "VALID": ""},
			health: Completed,
		},
		{
			name:   "as many succeeded as wanted, not yet complete",
			job:    jobTest("finishing", &three, 3, 0, time.Hour, 0),
			want:   map[string]string{"COMPLETIONS": "3/3", "VALID": ""},
			health: OK,
		},
		{
			name:   "no completions, done",
			job:    jobTest("once", nil, 1, 0, 4*time.Hour, 3*time.Hour),
			want:   map[string]string{"COMPLETIONS": "1/1", "DURATION": "3h"},
			health: Completed,
		},
		{
			name: "metadata only",
			job:  &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Namespace: "batch", Name: "bare", CreationTimestamp: ago(10 * time.Minute)}},
			want: map[string]string{
				"NAMESPACE":   "batch",
				"NAME":        "bare",
				"COMPLETIONS": "0/1",
				"DURATION":    MissingValue,
				"SELECTOR":    MissingValue,
				"CONTAINERS":  MissingValue,
				"IMAGES":      MissingValue,
				"VALID":       "",
				"AGE":         "10m",
			},
			health: OK,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			row, cells := rowOf(t, Jobs, tt.job)
			wantCells(t, cells, tt.want)
			if row.Health != tt.health {
				t.Errorf("health = %v, want %v", row.Health, tt.health)
			}
		})
	}
}

func TestJobOrder(t *testing.T) {
	one := int32(1)
	quick, _ := rowOf(t, Jobs, jobTest("quick", &one, 1, 0, 3*time.Minute, 90*time.Second))
	slow, _ := rowOf(t, Jobs, jobTest("slow", &one, 1, 0, 3*time.Hour, 2*time.Hour))
	for _, col := range []string{"DURATION", "AGE"} {
		if a, b := orderOf(t, Jobs, quick, col), orderOf(t, Jobs, slow, col); a >= b {
			t.Errorf("%s: the shorter sorts at %v, the longer at %v", col, a, b)
		}
	}
}
