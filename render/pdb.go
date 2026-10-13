// Follows k9s's internal/render/pdb.go (Apache-2.0).

package render

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
)

type podDisruptionBudget struct{}

func init() {
	Register(PodDisruptionBudgets, podDisruptionBudget{}, func() runtime.Object { return &policyv1.PodDisruptionBudget{} })
}

func (podDisruptionBudget) Columns() []Column {
	return []Column{
		{Name: "NAMESPACE"},
		{Name: "NAME"},
		{Name: "MIN-AVAILABLE", Right: true, Numeric: true},
		{Name: "MAX-UNAVAILABLE", Right: true, Numeric: true},
		{Name: "ALLOWED-DISRUPTIONS", Right: true, Numeric: true},
		{Name: "CURRENT", Right: true, Numeric: true},
		{Name: "DESIRED", Right: true, Numeric: true},
		{Name: "EXPECTED", Right: true, Numeric: true},
		{Name: "LABELS", Wide: true},
		{Name: "VALID", Wide: true},
		{Name: "AGE", Numeric: true},
	}
}

func (podDisruptionBudget) Row(obj runtime.Object, now time.Time) (Row, error) {
	pdb, ok := obj.(*policyv1.PodDisruptionBudget)
	if !ok {
		return Row{}, fmt.Errorf("pod disruption budget renderer: got %T", obj)
	}
	minText, minOrder := pdbBound(pdb.Spec.MinAvailable)
	maxText, maxOrder := pdbBound(pdb.Spec.MaxUnavailable)
	return NewRow(pdb).
		Text(pdb.Namespace).
		Text(pdb.Name).
		Num(minText, minOrder).
		Num(maxText, maxOrder).
		Int(int64(pdb.Status.DisruptionsAllowed)).
		Int(int64(pdb.Status.CurrentHealthy)).
		Int(int64(pdb.Status.DesiredHealthy)).
		Int(int64(pdb.Status.ExpectedPods)).
		Map(pdb.Labels).
		Valid(pdbDiagnose(pdb.Spec.MinAvailable, pdb.Status.CurrentHealthy)).
		Age(pdb.CreationTimestamp.Time, now).
		Row(), nil
}

// pdbBound returns the text of minAvailable or maxUnavailable, and the
// number it is sorted by: the count, or the number of a percentage. Unset
// is NAValue and sorts as zero.
func pdbBound(v *intstr.IntOrString) (string, float64) {
	if v == nil {
		return NAValue, 0
	}
	if v.Type == intstr.Int {
		return strconv.Itoa(int(v.IntVal)), float64(v.IntVal)
	}
	n, _ := strconv.ParseFloat(strings.TrimSuffix(v.StrVal, "%"), 64)
	return v.StrVal, n
}

// pdbDiagnose says why a budget is not as it should be: it demands more
// available pods than there are healthy ones. A percentage is not
// checked.
func pdbDiagnose(minAvailable *intstr.IntOrString, healthy int32) string {
	if minAvailable == nil || minAvailable.Type != intstr.Int {
		return ""
	}
	if minAvailable.IntVal > healthy {
		return fmt.Sprintf("expected %d but got %d", minAvailable.IntVal, healthy)
	}
	return ""
}
