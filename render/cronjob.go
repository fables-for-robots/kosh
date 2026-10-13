// Derived from k9s's internal/render/cronjob.go, Copyright Authors of K9s, Apache-2.0 (see LICENSES/Apache-2.0.txt); changed for kosh.

package render

import (
	"fmt"
	"strconv"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

type cronJob struct{}

func init() {
	Register(CronJobs, cronJob{}, func() runtime.Object { return &batchv1.CronJob{} })
}

var cronJobColumns = []Column{
	{Name: "NAMESPACE"},
	{Name: "NAME"},
	{Name: "SCHEDULE"},
	{Name: "SUSPEND"},
	{Name: "ACTIVE", Numeric: true},
	{Name: "LAST_SCHEDULE", Numeric: true},
	{Name: "SELECTOR", Wide: true},
	{Name: "CONTAINERS", Wide: true},
	{Name: "IMAGES", Wide: true},
	{Name: "LABELS", Wide: true},
	{Name: "VALID", Wide: true},
	{Name: "AGE", Numeric: true},
}

func (cronJob) Columns() []Column {
	return append([]Column(nil), cronJobColumns...)
}

func (cronJob) Row(obj runtime.Object, now time.Time) (Row, error) {
	cj, ok := obj.(*batchv1.CronJob)
	if !ok || cj == nil {
		return Row{}, fmt.Errorf("cronjob renderer: got %T", obj)
	}
	tmpl := &cj.Spec.JobTemplate.Spec
	names, images := workloadContainers(&tmpl.Template.Spec, true)

	b := NewRow(cj).
		Text(cj.Namespace).
		Text(cj.Name).
		Text(Or(cj.Spec.Schedule)).
		Text(strconv.FormatBool(cj.Spec.Suspend != nil && *cj.Spec.Suspend)).
		Int(int64(len(cj.Status.Active)))
	if cj.Status.LastScheduleTime == nil {
		b.Num(MissingValue, 0)
	} else {
		b.Age(cj.Status.LastScheduleTime.Time, now)
	}
	// k9s finds nothing wrong with a cron job: its VALID is empty.
	return b.Text(workloadSelector(tmpl.Selector)).
		Text(Join(names)).
		Text(Join(images)).
		Map(cj.Labels).
		Valid("").
		Age(cj.CreationTimestamp.Time, now).
		Row(), nil
}
