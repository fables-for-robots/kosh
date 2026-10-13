// Derived from k9s's internal/render/dp.go, Copyright Authors of K9s, Apache-2.0 (see LICENSES/Apache-2.0.txt); changed for kosh.

package render

import (
	"fmt"
	"strconv"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

type deployment struct{}

func init() {
	Register(Deployments, deployment{}, func() runtime.Object { return &appsv1.Deployment{} })
}

var deploymentColumns = []Column{
	{Name: "NAMESPACE"},
	{Name: "NAME"},
	{Name: "READY", Right: true},
	{Name: "UP-TO-DATE", Right: true, Numeric: true},
	{Name: "AVAILABLE", Right: true, Numeric: true},
	{Name: "LABELS", Wide: true},
	{Name: "VALID", Wide: true},
	{Name: "AGE", Numeric: true},
}

func (deployment) Columns() []Column {
	return append([]Column(nil), deploymentColumns...)
}

func (deployment) Row(obj runtime.Object, now time.Time) (Row, error) {
	dp, ok := obj.(*appsv1.Deployment)
	if !ok || dp == nil {
		return Row{}, fmt.Errorf("deployment renderer: got %T", obj)
	}
	var desired int32
	if dp.Spec.Replicas != nil {
		desired = *dp.Spec.Replicas
	}
	st := &dp.Status

	valid := ""
	if st.Replicas != st.AvailableReplicas {
		valid = fmt.Sprintf("desiring %d replicas got %d available", st.Replicas, st.AvailableReplicas)
	}

	b := NewRow(dp).
		Text(dp.Namespace).
		Text(dp.Name).
		Text(strconv.Itoa(int(st.AvailableReplicas))+"/"+strconv.Itoa(int(desired))).
		Int(int64(st.UpdatedReplicas)).
		Int(int64(st.AvailableReplicas)).
		Map(dp.Labels).
		Valid(valid).
		Age(dp.CreationTimestamp.Time, now)
	// As k9s colours it: a deployment that wants no replicas is dormant,
	// whatever its VALID cell says.
	if desired == 0 {
		b.Health(Pending)
	}
	return b.Row(), nil
}
