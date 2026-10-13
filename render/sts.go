// Follows k9s's internal/render/sts.go (Apache-2.0).

package render

import (
	"fmt"
	"strconv"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

type statefulSet struct{}

func init() {
	Register(StatefulSets, statefulSet{}, func() runtime.Object { return &appsv1.StatefulSet{} })
}

var statefulSetColumns = []Column{
	{Name: "NAMESPACE"},
	{Name: "NAME"},
	{Name: "READY"},
	{Name: "SELECTOR", Wide: true},
	{Name: "SERVICE"},
	{Name: "CONTAINERS", Wide: true},
	{Name: "IMAGES", Wide: true},
	{Name: "LABELS", Wide: true},
	{Name: "VALID", Wide: true},
	{Name: "AGE", Numeric: true},
}

func (statefulSet) Columns() []Column {
	return append([]Column(nil), statefulSetColumns...)
}

func (statefulSet) Row(obj runtime.Object, now time.Time) (Row, error) {
	sts, ok := obj.(*appsv1.StatefulSet)
	if !ok || sts == nil {
		return Row{}, fmt.Errorf("statefulset renderer: got %T", obj)
	}
	var desired int32
	if sts.Spec.Replicas != nil {
		desired = *sts.Spec.Replicas
	}
	st := &sts.Status
	names, images := workloadContainers(&sts.Spec.Template.Spec, true)

	valid := ""
	switch {
	case st.Replicas != st.ReadyReplicas:
		valid = fmt.Sprintf("desired %d replicas got %d available", st.Replicas, st.ReadyReplicas)
	case desired != st.ReadyReplicas:
		valid = fmt.Sprintf("want %d replicas got %d available", desired, st.ReadyReplicas)
	}

	return NewRow(sts).
		Text(sts.Namespace).
		Text(sts.Name).
		Text(strconv.Itoa(int(st.ReadyReplicas))+"/"+strconv.Itoa(int(desired))).
		Text(workloadSelector(sts.Spec.Selector)).
		Text(workloadOr(sts.Spec.ServiceName, NAValue)).
		Text(Join(names)).
		Text(Join(images)).
		Map(sts.Labels).
		Valid(valid).
		Age(sts.CreationTimestamp.Time, now).
		Row(), nil
}
