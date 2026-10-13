// Derived from k9s's internal/render/ds.go, Copyright Authors of K9s, Apache-2.0 (see LICENSES/Apache-2.0.txt); changed for kosh.

package render

import (
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

type daemonSet struct{}

func init() {
	Register(DaemonSets, daemonSet{}, func() runtime.Object { return &appsv1.DaemonSet{} })
}

var daemonSetColumns = []Column{
	{Name: "NAMESPACE"},
	{Name: "NAME"},
	{Name: "DESIRED", Right: true, Numeric: true},
	{Name: "CURRENT", Right: true, Numeric: true},
	{Name: "READY", Right: true, Numeric: true},
	{Name: "UP-TO-DATE", Right: true, Numeric: true},
	{Name: "AVAILABLE", Right: true, Numeric: true},
	{Name: "LABELS", Wide: true},
	{Name: "VALID", Wide: true},
	{Name: "AGE", Numeric: true},
}

func (daemonSet) Columns() []Column {
	return append([]Column(nil), daemonSetColumns...)
}

func (daemonSet) Row(obj runtime.Object, now time.Time) (Row, error) {
	ds, ok := obj.(*appsv1.DaemonSet)
	if !ok || ds == nil {
		return Row{}, fmt.Errorf("daemonset renderer: got %T", obj)
	}
	st := &ds.Status

	valid := ""
	if st.DesiredNumberScheduled != st.NumberReady {
		valid = fmt.Sprintf("desiring %d replicas but %d ready", st.DesiredNumberScheduled, st.NumberReady)
	}

	return NewRow(ds).
		Text(ds.Namespace).
		Text(ds.Name).
		Int(int64(st.DesiredNumberScheduled)).
		Int(int64(st.CurrentNumberScheduled)).
		Int(int64(st.NumberReady)).
		Int(int64(st.UpdatedNumberScheduled)).
		Int(int64(st.NumberAvailable)).
		Map(ds.Labels).
		Valid(valid).
		Age(ds.CreationTimestamp.Time, now).
		Row(), nil
}
