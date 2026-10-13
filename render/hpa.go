// k9s has no renderer for this kind: it shows the API server's table.
// The columns here are those of `kubectl get hpa`.

package render

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
	"k8s.io/apimachinery/pkg/runtime"
)

type horizontalPodAutoscaler struct{}

func init() {
	Register(HorizontalPodAutoscalers, horizontalPodAutoscaler{}, func() runtime.Object { return &autoscalingv2.HorizontalPodAutoscaler{} })
}

func (horizontalPodAutoscaler) Columns() []Column {
	return []Column{
		{Name: "NAMESPACE"},
		{Name: "NAME"},
		{Name: "REFERENCE"},
		{Name: "TARGETS"},
		{Name: "MINPODS", Right: true, Numeric: true},
		{Name: "MAXPODS", Right: true, Numeric: true},
		{Name: "REPLICAS", Right: true, Numeric: true},
		{Name: "AGE", Numeric: true},
	}
}

func (horizontalPodAutoscaler) Row(obj runtime.Object, now time.Time) (Row, error) {
	hpa, ok := obj.(*autoscalingv2.HorizontalPodAutoscaler)
	if !ok {
		return Row{}, fmt.Errorf("horizontal pod autoscaler renderer: got %T", obj)
	}
	b := NewRow(hpa).
		Text(hpa.Namespace).
		Text(hpa.Name).
		Text(hpaReference(hpa.Spec.ScaleTargetRef)).
		Text(hpaTargets(hpa.Spec.Metrics, hpa.Status.CurrentMetrics))
	if hpa.Spec.MinReplicas == nil {
		// The API server's default is one.
		b.Num("<unset>", 1)
	} else {
		b.Int(int64(*hpa.Spec.MinReplicas))
	}
	return b.
		Int(int64(hpa.Spec.MaxReplicas)).
		Int(int64(hpa.Status.CurrentReplicas)).
		Age(hpa.CreationTimestamp.Time, now).
		Row(), nil
}

// hpaReference returns "Kind/name" of the scale target, or MissingValue
// when it names nothing.
func hpaReference(ref autoscalingv2.CrossVersionObjectReference) string {
	if ref.Kind == "" && ref.Name == "" {
		return MissingValue
	}
	return ref.Kind + "/" + ref.Name
}

// hpaTargets returns one "name: current/target" for each metric of the
// spec, joined by ", ". The current value is read from the status's
// metric at the same position.
func hpaTargets(specs []autoscalingv2.MetricSpec, statuses []autoscalingv2.MetricStatus) string {
	if len(specs) == 0 {
		return MissingValue
	}
	parts := make([]string, 0, len(specs))
	for i, spec := range specs {
		var status *autoscalingv2.MetricStatus
		if i < len(statuses) {
			status = &statuses[i]
		}
		name, target, current := hpaMetric(spec, status)
		parts = append(parts, name+": "+hpaCurrent(target, current)+"/"+hpaTarget(target))
	}
	return strings.Join(parts, ", ")
}

// hpaMetric returns the name and the target of a metric of the spec, and
// the current value of the status's metric when that is of the same type.
func hpaMetric(spec autoscalingv2.MetricSpec, status *autoscalingv2.MetricStatus) (name string, target autoscalingv2.MetricTarget, current *autoscalingv2.MetricValueStatus) {
	switch spec.Type {
	case autoscalingv2.ResourceMetricSourceType:
		if spec.Resource != nil {
			name, target = string(spec.Resource.Name), spec.Resource.Target
		}
		if status != nil && status.Resource != nil {
			current = &status.Resource.Current
		}
	case autoscalingv2.ContainerResourceMetricSourceType:
		if spec.ContainerResource != nil {
			name, target = string(spec.ContainerResource.Name), spec.ContainerResource.Target
		}
		if status != nil && status.ContainerResource != nil {
			current = &status.ContainerResource.Current
		}
	case autoscalingv2.PodsMetricSourceType:
		if spec.Pods != nil {
			name, target = spec.Pods.Metric.Name, spec.Pods.Target
		}
		if status != nil && status.Pods != nil {
			current = &status.Pods.Current
		}
	case autoscalingv2.ObjectMetricSourceType:
		if spec.Object != nil {
			name, target = spec.Object.Metric.Name, spec.Object.Target
		}
		if status != nil && status.Object != nil {
			current = &status.Object.Current
		}
	case autoscalingv2.ExternalMetricSourceType:
		if spec.External != nil {
			name, target = spec.External.Metric.Name, spec.External.Target
		}
		if status != nil && status.External != nil {
			current = &status.External.Current
		}
	}
	if name == "" {
		name = Or(string(spec.Type))
	}
	return name, target, current
}

// hpaTarget returns the target of a metric: a percentage for a
// utilization, otherwise the quantity as written.
func hpaTarget(t autoscalingv2.MetricTarget) string {
	switch {
	case t.Type == autoscalingv2.UtilizationMetricType && t.AverageUtilization != nil:
		return strconv.FormatInt(int64(*t.AverageUtilization), 10) + "%"
	case t.Type == autoscalingv2.AverageValueMetricType && t.AverageValue != nil:
		return t.AverageValue.String()
	case t.Type == autoscalingv2.ValueMetricType && t.Value != nil:
		return t.Value.String()
	}
	return UnknownValue
}

// hpaCurrent returns the current value of a metric in the form its target
// is in, or UnknownValue when the status does not have it.
func hpaCurrent(t autoscalingv2.MetricTarget, c *autoscalingv2.MetricValueStatus) string {
	if c == nil {
		return UnknownValue
	}
	switch t.Type {
	case autoscalingv2.UtilizationMetricType:
		if c.AverageUtilization != nil {
			return strconv.FormatInt(int64(*c.AverageUtilization), 10) + "%"
		}
	case autoscalingv2.AverageValueMetricType:
		if c.AverageValue != nil {
			return c.AverageValue.String()
		}
	case autoscalingv2.ValueMetricType:
		if c.Value != nil {
			return c.Value.String()
		}
	}
	return UnknownValue
}
