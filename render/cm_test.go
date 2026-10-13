package render

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestConfigMapColumns(t *testing.T) {
	wantColumns(t, ConfigMaps, "NAMESPACE", "NAME", "DATA", "VALID*", "AGE")
}

func TestConfigMapRows(t *testing.T) {
	tests := []struct {
		name string
		cm   *corev1.ConfigMap
		want map[string]string
	}{
		{"with data", &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "cfg", CreationTimestamp: ago(5 * time.Hour)},
			Data:       map[string]string{"a": "1", "b": "2"},
			BinaryData: map[string][]byte{"c": {1}},
		}, map[string]string{"NAMESPACE": "shop", "NAME": "cfg", "DATA": "3", "VALID": "", "AGE": "5h"}},
		{"metadata only", &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "bare"}},
			map[string]string{"NAMESPACE": "", "NAME": "bare", "DATA": "0", "VALID": "", "AGE": UnknownValue}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			row, cells := rowOf(t, ConfigMaps, tt.cm)
			wantCells(t, cells, tt.want)
			if row.Health != OK {
				t.Errorf("health = %v", row.Health)
			}
		})
	}
}

func TestConfigMapOrder(t *testing.T) {
	few, _ := rowOf(t, ConfigMaps, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{CreationTimestamp: ago(time.Minute)}, Data: map[string]string{"a": ""}})
	many, _ := rowOf(t, ConfigMaps, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{CreationTimestamp: ago(time.Hour)}, Data: map[string]string{"a": "", "b": "", "c": "", "d": "", "e": "", "f": "", "g": "", "h": "", "i": "", "j": "", "k": ""}})
	for _, col := range []string{"DATA", "AGE"} {
		if orderOf(t, ConfigMaps, few, col) >= orderOf(t, ConfigMaps, many, col) {
			t.Errorf("%s does not sort 1 entry / 1m before 11 entries / 1h", col)
		}
	}
}
