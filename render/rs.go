// Follows k9s's internal/render/rs.go (Apache-2.0).

package render

import (
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

type replicaSet struct{}

func init() {
	Register(ReplicaSets, replicaSet{}, func() runtime.Object { return &appsv1.ReplicaSet{} })
}

var replicaSetColumns = []Column{
	{Name: "NAMESPACE"},
	{Name: "NAME"},
	{Name: "DESIRED", Right: true, Numeric: true},
	{Name: "CURRENT", Right: true, Numeric: true},
	{Name: "READY", Right: true, Numeric: true},
	{Name: "CONTAINERS", Wide: true},
	{Name: "IMAGES", Wide: true},
	{Name: "SELECTOR", Wide: true},
	{Name: "VALID", Wide: true},
	{Name: "AGE", Numeric: true},
}

func (replicaSet) Columns() []Column {
	return append([]Column(nil), replicaSetColumns...)
}

func (replicaSet) Row(obj runtime.Object, now time.Time) (Row, error) {
	rs, ok := obj.(*appsv1.ReplicaSet)
	if !ok || rs == nil {
		return Row{}, fmt.Errorf("replicaset renderer: got %T", obj)
	}
	var desired int32
	if rs.Spec.Replicas != nil {
		desired = *rs.Spec.Replicas
	}
	st := &rs.Status
	names, images := workloadContainers(&rs.Spec.Template.Spec, false)

	valid := ""
	switch {
	case st.Replicas == st.ReadyReplicas:
	case st.Replicas == 0:
		valid = fmt.Sprintf("did not phase down correctly expecting 0 replicas but got %d", st.ReadyReplicas)
	default:
		valid = fmt.Sprintf("mismatch desired(%d) vs ready(%d)", st.Replicas, st.ReadyReplicas)
	}

	return NewRow(rs).
		Text(rs.Namespace).
		Text(rs.Name).
		Int(int64(desired)).
		Int(int64(st.Replicas)).
		Int(int64(st.ReadyReplicas)).
		Text(Join(names)).
		Text(Join(images)).
		Text(workloadSelector(rs.Spec.Selector)).
		Valid(valid).
		Age(rs.CreationTimestamp.Time, now).
		Row(), nil
}

// workloadContainers returns the names and the images of the containers
// of a pod template, its init containers first when withInit.
func workloadContainers(spec *corev1.PodSpec, withInit bool) (names, images []string) {
	add := func(cc []corev1.Container) {
		for i := range cc {
			names = append(names, cc[i].Name)
			images = append(images, cc[i].Image)
		}
	}
	if withInit {
		add(spec.InitContainers)
	}
	add(spec.Containers)
	return names, images
}

// workloadSelector is the SELECTOR cell: the selector as kubectl prints
// it, "app=web,tier in (a,b)", MissingValue for none, and "<error>" for
// one that is not well formed.
func workloadSelector(s *metav1.LabelSelector) string {
	return metav1.FormatLabelSelector(s)
}
