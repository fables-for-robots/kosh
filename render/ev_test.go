package render

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestEventColumns(t *testing.T) {
	wantColumns(t, Events, "NAMESPACE", "LAST SEEN", "TYPE", "REASON", "OBJECT", "MESSAGE", "SUBOBJECT*", "SOURCE*", "FIRST SEEN*", "COUNT*", "NAME*")
}

func TestEventRows(t *testing.T) {
	full := &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Namespace: "shop", Name: "web.abc"},
		InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: "web-7d4b9", FieldPath: "spec.containers{app}"},
		Type:           "Normal", Reason: "Pulled",
		Message:        "  Successfully pulled\n image   \"nginx\"\r\n ",
		Source:         corev1.EventSource{Component: "kubelet", Host: "n1"},
		FirstTimestamp: ago(10 * time.Minute),
		LastTimestamp:  ago(2 * time.Minute),
		Count:          3,
	}
	warning := &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Name: "w"},
		InvolvedObject: corev1.ObjectReference{Kind: "Node", Name: "n1"},
		Type:           "Warning", Reason: "NodeNotReady",
		FirstTimestamp: ago(time.Hour),
	}
	series := &corev1.Event{
		ObjectMeta:          metav1.ObjectMeta{Name: "s"},
		Type:                "Normal",
		Count:               2,
		Series:              &corev1.EventSeries{Count: 17},
		EventTime:           metav1.NewMicroTime(testNow.Add(-5 * time.Second)),
		ReportingController: "my-controller",
		FirstTimestamp:      ago(time.Hour),
	}
	tests := []struct {
		name   string
		ev     *corev1.Event
		want   map[string]string
		health Health
	}{
		{"normal", full, map[string]string{
			"NAMESPACE": "shop", "LAST SEEN": "2m", "TYPE": "Normal", "REASON": "Pulled", "OBJECT": "pod/web-7d4b9",
			"MESSAGE": `Successfully pulled image "nginx"`, "SUBOBJECT": "spec.containers{app}", "SOURCE": "kubelet, n1",
			"FIRST SEEN": "10m", "COUNT": "3", "NAME": "web.abc",
		}, OK},
		{"warning, only first timestamp", warning, map[string]string{
			"LAST SEEN": "60m", "TYPE": "Warning", "OBJECT": "node/n1", "SUBOBJECT": "", "SOURCE": "", "FIRST SEEN": "60m", "COUNT": "0",
		}, Error},
		{"series, event time, reporting controller", series, map[string]string{
			"LAST SEEN": "5s", "COUNT": "17", "SOURCE": "my-controller",
		}, OK},
		{"metadata only", &corev1.Event{ObjectMeta: metav1.ObjectMeta{Name: "bare"}}, map[string]string{
			"LAST SEEN": UnknownValue, "OBJECT": MissingValue, "MESSAGE": "", "FIRST SEEN": UnknownValue, "COUNT": "0", "NAME": "bare",
		}, Error},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			row, cells := rowOf(t, Events, tt.ev)
			wantCells(t, cells, tt.want)
			if row.Health != tt.health {
				t.Errorf("health = %v, want %v", row.Health, tt.health)
			}
		})
	}
}

func TestEventLastSeenPrefersLastThenEventTimeThenFirst(t *testing.T) {
	ev := &corev1.Event{
		LastTimestamp:  ago(time.Minute),
		EventTime:      metav1.NewMicroTime(testNow.Add(-2 * time.Minute)),
		FirstTimestamp: ago(3 * time.Minute),
	}
	if _, c := rowOf(t, Events, ev); c["LAST SEEN"] != "60s" {
		t.Errorf("last timestamp not used: %q", c["LAST SEEN"])
	}
	ev.LastTimestamp = metav1.Time{}
	if _, c := rowOf(t, Events, ev); c["LAST SEEN"] != "2m" {
		t.Errorf("event time not used: %q", c["LAST SEEN"])
	}
}

func TestEventOrder(t *testing.T) {
	recent := &corev1.Event{LastTimestamp: ago(time.Minute), FirstTimestamp: ago(time.Hour), Count: 2}
	older := &corev1.Event{LastTimestamp: ago(time.Hour), FirstTimestamp: ago(48 * time.Hour), Count: 10}
	rr, _ := rowOf(t, Events, recent)
	ro, _ := rowOf(t, Events, older)
	for _, col := range []string{"LAST SEEN", "FIRST SEEN", "COUNT"} {
		lo, hi := orderOf(t, Events, rr, col), orderOf(t, Events, ro, col)
		if lo >= hi {
			t.Errorf("%s: %v does not sort before %v", col, lo, hi)
		}
	}
}
