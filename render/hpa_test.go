package render

import (
	"testing"
	"time"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func hpaInt32(n int32) *int32 { return &n }

func hpaQty(s string) *resource.Quantity {
	q := resource.MustParse(s)
	return &q
}

func TestHorizontalPodAutoscalersColumns(t *testing.T) {
	wantColumns(t, HorizontalPodAutoscalers, "NAMESPACE", "NAME", "REFERENCE", "TARGETS", "MINPODS", "MAXPODS", "REPLICAS", "AGE")
}

func TestHorizontalPodAutoscalersRows(t *testing.T) {
	tests := []struct {
		name   string
		obj    *autoscalingv2.HorizontalPodAutoscaler
		want   map[string]string
		health Health
	}{
		{
			name: "three kinds of metric",
			obj: &autoscalingv2.HorizontalPodAutoscaler{
				ObjectMeta: metav1.ObjectMeta{Namespace: "prod", Name: "web", CreationTimestamp: ago(26 * time.Hour)},
				Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
					ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{Kind: "Deployment", Name: "web"},
					MinReplicas:    hpaInt32(2),
					MaxReplicas:    10,
					Metrics: []autoscalingv2.MetricSpec{
						{Type: autoscalingv2.ResourceMetricSourceType, Resource: &autoscalingv2.ResourceMetricSource{
							Name:   corev1.ResourceCPU,
							Target: autoscalingv2.MetricTarget{Type: autoscalingv2.UtilizationMetricType, AverageUtilization: hpaInt32(80)},
						}},
						{Type: autoscalingv2.ResourceMetricSourceType, Resource: &autoscalingv2.ResourceMetricSource{
							Name:   corev1.ResourceMemory,
							Target: autoscalingv2.MetricTarget{Type: autoscalingv2.AverageValueMetricType, AverageValue: hpaQty("200Mi")},
						}},
						{Type: autoscalingv2.PodsMetricSourceType, Pods: &autoscalingv2.PodsMetricSource{
							Metric: autoscalingv2.MetricIdentifier{Name: "http_requests"},
							Target: autoscalingv2.MetricTarget{Type: autoscalingv2.AverageValueMetricType, AverageValue: hpaQty("100")},
						}},
					},
				},
				Status: autoscalingv2.HorizontalPodAutoscalerStatus{
					CurrentReplicas: 3,
					CurrentMetrics: []autoscalingv2.MetricStatus{
						{Type: autoscalingv2.ResourceMetricSourceType, Resource: &autoscalingv2.ResourceMetricStatus{
							Name: corev1.ResourceCPU, Current: autoscalingv2.MetricValueStatus{AverageUtilization: hpaInt32(23)},
						}},
						{Type: autoscalingv2.ResourceMetricSourceType, Resource: &autoscalingv2.ResourceMetricStatus{
							Name: corev1.ResourceMemory, Current: autoscalingv2.MetricValueStatus{AverageValue: hpaQty("120Mi")},
						}},
						{Type: autoscalingv2.PodsMetricSourceType, Pods: &autoscalingv2.PodsMetricStatus{
							Metric:  autoscalingv2.MetricIdentifier{Name: "http_requests"},
							Current: autoscalingv2.MetricValueStatus{AverageValue: hpaQty("12")},
						}},
					},
				},
			},
			want: map[string]string{
				"NAMESPACE": "prod", "NAME": "web", "REFERENCE": "Deployment/web",
				"TARGETS": "cpu: 23%/80%, memory: 120Mi/200Mi, http_requests: 12/100",
				"MINPODS": "2", "MAXPODS": "10", "REPLICAS": "3", "AGE": "26h",
			},
		},
		{
			name: "the status has no metrics yet",
			obj: &autoscalingv2.HorizontalPodAutoscaler{
				ObjectMeta: metav1.ObjectMeta{Name: "h"},
				Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
					MaxReplicas: 4,
					Metrics: []autoscalingv2.MetricSpec{
						{Type: autoscalingv2.ResourceMetricSourceType, Resource: &autoscalingv2.ResourceMetricSource{
							Name:   corev1.ResourceCPU,
							Target: autoscalingv2.MetricTarget{Type: autoscalingv2.UtilizationMetricType, AverageUtilization: hpaInt32(50)},
						}},
						{Type: autoscalingv2.ExternalMetricSourceType, External: &autoscalingv2.ExternalMetricSource{
							Metric: autoscalingv2.MetricIdentifier{Name: "queue_depth"},
							Target: autoscalingv2.MetricTarget{Type: autoscalingv2.ValueMetricType, Value: hpaQty("30")},
						}},
					},
				},
			},
			want: map[string]string{
				"TARGETS": "cpu: <unknown>/50%, queue_depth: <unknown>/30",
				"MINPODS": "<unset>", "MAXPODS": "4", "REPLICAS": "0",
			},
		},
		{
			name: "an object metric of a value",
			obj: &autoscalingv2.HorizontalPodAutoscaler{
				Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
					Metrics: []autoscalingv2.MetricSpec{
						{Type: autoscalingv2.ObjectMetricSourceType, Object: &autoscalingv2.ObjectMetricSource{
							Metric: autoscalingv2.MetricIdentifier{Name: "rps"},
							Target: autoscalingv2.MetricTarget{Type: autoscalingv2.ValueMetricType, Value: hpaQty("1k")},
						}},
					},
				},
				Status: autoscalingv2.HorizontalPodAutoscalerStatus{
					CurrentMetrics: []autoscalingv2.MetricStatus{
						{Type: autoscalingv2.ObjectMetricSourceType, Object: &autoscalingv2.ObjectMetricStatus{
							Current: autoscalingv2.MetricValueStatus{Value: hpaQty("450")},
						}},
					},
				},
			},
			want: map[string]string{"TARGETS": "rps: 450/1k"},
		},
		{
			name: "a spec that names no metrics",
			obj: &autoscalingv2.HorizontalPodAutoscaler{
				ObjectMeta: metav1.ObjectMeta{Name: "h"},
				Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
					ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{Kind: "StatefulSet", Name: "db"},
				},
			},
			want: map[string]string{"REFERENCE": "StatefulSet/db", "TARGETS": MissingValue},
		},
		{
			name: "a container resource metric",
			obj: &autoscalingv2.HorizontalPodAutoscaler{
				Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
					Metrics: []autoscalingv2.MetricSpec{
						{Type: autoscalingv2.ContainerResourceMetricSourceType, ContainerResource: &autoscalingv2.ContainerResourceMetricSource{
							Name: corev1.ResourceCPU, Container: "app",
							Target: autoscalingv2.MetricTarget{Type: autoscalingv2.UtilizationMetricType, AverageUtilization: hpaInt32(60)},
						}},
					},
				},
				Status: autoscalingv2.HorizontalPodAutoscalerStatus{
					CurrentMetrics: []autoscalingv2.MetricStatus{
						{Type: autoscalingv2.ContainerResourceMetricSourceType, ContainerResource: &autoscalingv2.ContainerResourceMetricStatus{
							Name: corev1.ResourceCPU, Container: "app", Current: autoscalingv2.MetricValueStatus{AverageUtilization: hpaInt32(41)},
						}},
					},
				},
			},
			want: map[string]string{"TARGETS": "cpu: 41%/60%"},
		},
		{
			name: "the status metric at the position is of another type",
			obj: &autoscalingv2.HorizontalPodAutoscaler{
				Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
					Metrics: []autoscalingv2.MetricSpec{
						{Type: autoscalingv2.ResourceMetricSourceType, Resource: &autoscalingv2.ResourceMetricSource{
							Name:   corev1.ResourceCPU,
							Target: autoscalingv2.MetricTarget{Type: autoscalingv2.UtilizationMetricType, AverageUtilization: hpaInt32(80)},
						}},
					},
				},
				Status: autoscalingv2.HorizontalPodAutoscalerStatus{
					CurrentMetrics: []autoscalingv2.MetricStatus{
						{Type: autoscalingv2.PodsMetricSourceType, Pods: &autoscalingv2.PodsMetricStatus{
							Metric:  autoscalingv2.MetricIdentifier{Name: "x"},
							Current: autoscalingv2.MetricValueStatus{AverageValue: hpaQty("5")},
						}},
					},
				},
			},
			want: map[string]string{"TARGETS": "cpu: <unknown>/80%"},
		},
		{
			name: "a reference with a kind only",
			obj: &autoscalingv2.HorizontalPodAutoscaler{Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
				ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{Kind: "Deployment"}}},
			want: map[string]string{"REFERENCE": "Deployment/" + MissingValue},
		},
		{
			name: "a reference with a name only",
			obj: &autoscalingv2.HorizontalPodAutoscaler{Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
				ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{Name: "web"}}},
			want: map[string]string{"REFERENCE": MissingValue + "/web"},
		},
		{
			name: "metadata only",
			obj:  &autoscalingv2.HorizontalPodAutoscaler{ObjectMeta: metav1.ObjectMeta{Name: "h"}},
			want: map[string]string{
				"NAME": "h", "REFERENCE": MissingValue, "TARGETS": MissingValue,
				"MINPODS": "<unset>", "MAXPODS": "0", "REPLICAS": "0", "AGE": UnknownValue,
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			row, cells := rowOf(t, HorizontalPodAutoscalers, tc.obj)
			wantCells(t, cells, tc.want)
			if row.Health != tc.health {
				t.Errorf("health = %v, want %v", row.Health, tc.health)
			}
		})
	}
}

func TestHorizontalPodAutoscalersOrder(t *testing.T) {
	small := &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{CreationTimestamp: ago(time.Minute)},
		Spec:       autoscalingv2.HorizontalPodAutoscalerSpec{MinReplicas: hpaInt32(2), MaxReplicas: 5},
		Status:     autoscalingv2.HorizontalPodAutoscalerStatus{CurrentReplicas: 2},
	}
	big := &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{CreationTimestamp: ago(time.Hour)},
		Spec:       autoscalingv2.HorizontalPodAutoscalerSpec{MinReplicas: hpaInt32(10), MaxReplicas: 50},
		Status:     autoscalingv2.HorizontalPodAutoscalerStatus{CurrentReplicas: 20},
	}
	a, _ := rowOf(t, HorizontalPodAutoscalers, small)
	b, _ := rowOf(t, HorizontalPodAutoscalers, big)
	for _, col := range []string{"MINPODS", "MAXPODS", "REPLICAS", "AGE"} {
		if orderOf(t, HorizontalPodAutoscalers, a, col) >= orderOf(t, HorizontalPodAutoscalers, b, col) {
			t.Errorf("%s: the smaller should sort before the larger", col)
		}
	}
}

func TestHorizontalPodAutoscalersUnsetMinPodsSortsAsOne(t *testing.T) {
	unset, _ := rowOf(t, HorizontalPodAutoscalers, &autoscalingv2.HorizontalPodAutoscaler{})
	zero, _ := rowOf(t, HorizontalPodAutoscalers, &autoscalingv2.HorizontalPodAutoscaler{Spec: autoscalingv2.HorizontalPodAutoscalerSpec{MinReplicas: hpaInt32(0)}})
	one, _ := rowOf(t, HorizontalPodAutoscalers, &autoscalingv2.HorizontalPodAutoscaler{Spec: autoscalingv2.HorizontalPodAutoscalerSpec{MinReplicas: hpaInt32(1)}})
	two, _ := rowOf(t, HorizontalPodAutoscalers, &autoscalingv2.HorizontalPodAutoscaler{Spec: autoscalingv2.HorizontalPodAutoscalerSpec{MinReplicas: hpaInt32(2)}})
	u := orderOf(t, HorizontalPodAutoscalers, unset, "MINPODS")
	if u != orderOf(t, HorizontalPodAutoscalers, one, "MINPODS") {
		t.Errorf("unset sorts as %v, want 1", u)
	}
	if !(orderOf(t, HorizontalPodAutoscalers, zero, "MINPODS") < u && u < orderOf(t, HorizontalPodAutoscalers, two, "MINPODS")) {
		t.Error("unset should sort between 0 and 2")
	}
}
