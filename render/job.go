// Derived from k9s's internal/render/job.go, Copyright Authors of K9s, Apache-2.0 (see LICENSES/Apache-2.0.txt); changed for kosh.

package render

import (
	"fmt"
	"strconv"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/duration"
)

type job struct{}

func init() {
	Register(Jobs, job{}, func() runtime.Object { return &batchv1.Job{} })
}

var jobColumns = []Column{
	{Name: "NAMESPACE"},
	{Name: "NAME"},
	{Name: "COMPLETIONS"},
	{Name: "DURATION", Numeric: true},
	{Name: "SELECTOR", Wide: true},
	{Name: "CONTAINERS", Wide: true},
	{Name: "IMAGES", Wide: true},
	{Name: "VALID", Wide: true},
	{Name: "AGE", Numeric: true},
}

func (job) Columns() []Column {
	return append([]Column(nil), jobColumns...)
}

func (job) Row(obj runtime.Object, now time.Time) (Row, error) {
	j, ok := obj.(*batchv1.Job)
	if !ok || j == nil {
		return Row{}, fmt.Errorf("job renderer: got %T", obj)
	}
	st := &j.Status
	names, images := workloadContainers(&j.Spec.Template.Spec, true)

	// A job is done when it says so, and not when its count of pods
	// that succeeded reaches a number: a work queue has pods running
	// after the first succeeded.
	done := st.CompletionTime != nil || jobComplete(st)

	valid := ""
	if !done && st.Failed > 0 {
		valid = fmt.Sprintf("%d pods failed", st.Failed)
	}

	b := NewRow(j).
		Text(j.Namespace).
		Text(j.Name).
		Text(jobCompletions(&j.Spec, st))
	if st.StartTime == nil || st.CompletionTime == nil {
		b.Num(MissingValue, 0)
	} else {
		d := st.CompletionTime.Sub(st.StartTime.Time)
		b.Num(duration.HumanDuration(d), d.Seconds())
	}
	b.Text(workloadSelector(j.Spec.Selector)).
		Text(Join(jobFirst(names))).
		Text(Join(jobFirst(images))).
		Valid(valid).
		Age(j.CreationTimestamp.Time, now)
	if done {
		b.Health(Completed)
	}
	return b.Row(), nil
}

// jobComplete says whether the job's Complete condition is true.
func jobComplete(st *batchv1.JobStatus) bool {
	for _, c := range st.Conditions {
		if c.Type == batchv1.JobComplete && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// jobCompletions is the COMPLETIONS cell: the pods that succeeded of
// those the job wants, "2/3"; "0/1 of 3" for a job that names no
// completions and runs pods in parallel.
func jobCompletions(spec *batchv1.JobSpec, st *batchv1.JobStatus) string {
	succeeded := strconv.Itoa(int(st.Succeeded))
	if spec.Completions != nil {
		return succeeded + "/" + strconv.Itoa(int(*spec.Completions))
	}
	if spec.Parallelism != nil && *spec.Parallelism > 1 {
		return succeeded + "/1 of " + strconv.Itoa(int(*spec.Parallelism))
	}
	return succeeded + "/1"
}

// jobShown is how many containers and images a job's row names.
const jobShown = 2

// jobFirst returns the first jobShown of ss, and then how many more
// there are: "a,b,(+3)...".
func jobFirst(ss []string) []string {
	if len(ss) <= jobShown {
		return ss
	}
	return append(ss[:jobShown:jobShown], "(+"+strconv.Itoa(len(ss)-jobShown)+")...")
}
