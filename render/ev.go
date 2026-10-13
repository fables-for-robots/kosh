package render

import (
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// The columns of events are those of kubectl get events: k9s has no
// renderer of its own for them.
type event struct{}

func init() {
	Register(Events, event{}, func() runtime.Object { return &corev1.Event{} })
}

func (event) Columns() []Column {
	return []Column{
		{Name: "NAMESPACE"},
		{Name: "LAST SEEN", Numeric: true},
		{Name: "TYPE"},
		{Name: "REASON"},
		{Name: "OBJECT"},
		{Name: "MESSAGE"},
		{Name: "SUBOBJECT", Wide: true},
		{Name: "SOURCE", Wide: true},
		{Name: "FIRST SEEN", Wide: true, Numeric: true},
		{Name: "COUNT", Wide: true, Numeric: true},
		{Name: "NAME", Wide: true},
	}
}

func (event) Row(obj runtime.Object, now time.Time) (Row, error) {
	ev, ok := obj.(*corev1.Event)
	if !ok {
		return Row{}, fmt.Errorf("event renderer: got %T", obj)
	}
	b := NewRow(ev)
	if ev.Type != "Normal" {
		b.Health(Error)
	}
	b.Text(ev.Namespace).
		Age(eventLastSeen(ev), now).
		Text(ev.Type).
		Text(ev.Reason).
		Text(eventObject(ev.InvolvedObject)).
		Text(strings.Join(strings.Fields(ev.Message), " ")).
		Text(ev.InvolvedObject.FieldPath).
		Text(eventSource(ev)).
		Age(ev.FirstTimestamp.Time, now).
		Int(int64(eventCount(ev))).
		Text(ev.Name)
	return b.Row(), nil
}

// eventLastSeen is the lastTimestamp, or the eventTime, or the
// firstTimestamp, whichever is set first; the zero time when none is.
func eventLastSeen(ev *corev1.Event) time.Time {
	switch {
	case !ev.LastTimestamp.IsZero():
		return ev.LastTimestamp.Time
	case !ev.EventTime.IsZero():
		return ev.EventTime.Time
	}
	return ev.FirstTimestamp.Time
}

// eventObject is "kind/name" of the involved object with the kind in
// lower case; the name alone when it has no kind, MissingValue when it
// has neither.
func eventObject(ref corev1.ObjectReference) string {
	switch {
	case ref.Kind == "" && ref.Name == "":
		return MissingValue
	case ref.Kind == "":
		return ref.Name
	}
	return strings.ToLower(ref.Kind) + "/" + ref.Name
}

// eventSource is the source's component and host joined by ", ", the
// reporting controller when the source is empty.
func eventSource(ev *corev1.Event) string {
	var parts []string
	for _, s := range []string{ev.Source.Component, ev.Source.Host} {
		if s != "" {
			parts = append(parts, s)
		}
	}
	if len(parts) == 0 {
		return ev.ReportingController
	}
	return strings.Join(parts, ", ")
}

// eventCount is the count of the event's series when it has one.
func eventCount(ev *corev1.Event) int32 {
	if ev.Series != nil {
		return ev.Series.Count
	}
	return ev.Count
}
