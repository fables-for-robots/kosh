package render

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestNodeColumns(t *testing.T) {
	wantColumns(t, Nodes, "NAME", "STATUS", "ROLE", "ARCH*", "TAINTS", "VERSION", "OS-IMAGE*", "KERNEL*", "INTERNAL-IP*", "EXTERNAL-IP*", "PODS", "LABELS*", "VALID*", "AGE")
	r, _ := For(Nodes)
	for _, c := range r.Columns() {
		if c.Name == "PODS" && !c.Right {
			t.Error("PODS is not right aligned")
		}
	}
}

func nodeWithReady(status corev1.ConditionStatus) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "n1",
			CreationTimestamp: ago(50 * 24 * time.Hour),
			Labels: map[string]string{
				"node-role.kubernetes.io/worker":        "",
				"node-role.kubernetes.io/control-plane": "",
				"kubernetes.io/hostname":                "n1",
			},
		},
		Spec: corev1.NodeSpec{Taints: []corev1.Taint{{Key: "a"}, {Key: "b"}}},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{{Type: corev1.NodeMemoryPressure, Status: corev1.ConditionFalse}, {Type: corev1.NodeReady, Status: status}},
			Capacity:   corev1.ResourceList{corev1.ResourcePods: resource.MustParse("110")},
			Addresses: []corev1.NodeAddress{
				{Type: corev1.NodeInternalIP, Address: "10.1.1.1"},
				{Type: corev1.NodeExternalIP, Address: "34.1.1.1"},
				{Type: corev1.NodeHostName, Address: "n1"},
			},
			NodeInfo: corev1.NodeSystemInfo{Architecture: "arm64", KubeletVersion: "v1.30.1", OSImage: "Ubuntu 24.04", KernelVersion: "6.8.0"},
		},
	}
}

func TestNodeRows(t *testing.T) {
	cordoned := nodeWithReady(corev1.ConditionTrue)
	cordoned.Spec.Unschedulable = true
	cordonedDown := nodeWithReady(corev1.ConditionFalse)
	cordonedDown.Spec.Unschedulable = true
	unknown := nodeWithReady(corev1.ConditionUnknown)

	tests := []struct {
		name   string
		node   *corev1.Node
		want   map[string]string
		health Health
	}{
		{
			name: "ready",
			node: nodeWithReady(corev1.ConditionTrue),
			want: map[string]string{
				"NAME": "n1", "STATUS": "Ready", "ROLE": "control-plane,worker", "ARCH": "arm64", "TAINTS": "2",
				"VERSION": "v1.30.1", "OS-IMAGE": "Ubuntu 24.04", "KERNEL": "6.8.0", "INTERNAL-IP": "10.1.1.1",
				"EXTERNAL-IP": "34.1.1.1", "PODS": "110",
				"LABELS": "kubernetes.io/hostname=n1,node-role.kubernetes.io/control-plane=,node-role.kubernetes.io/worker=",
				"VALID":  "", "AGE": "50d",
			},
			health: OK,
		},
		{
			name:   "not ready",
			node:   nodeWithReady(corev1.ConditionFalse),
			want:   map[string]string{"STATUS": "NotReady", "VALID": "node is not ready"},
			health: Error,
		},
		{
			name:   "ready condition unknown",
			node:   unknown,
			want:   map[string]string{"STATUS": "NotReady", "VALID": "node is not ready"},
			health: Error,
		},
		{
			name:   "ready and cordoned",
			node:   cordoned,
			want:   map[string]string{"STATUS": "Ready,SchedulingDisabled", "VALID": "node is cordoned"},
			health: Pending,
		},
		{
			name:   "not ready and cordoned",
			node:   cordonedDown,
			want:   map[string]string{"STATUS": "NotReady,SchedulingDisabled", "VALID": "node is not ready"},
			health: Error,
		},
		{
			name: "role from the kubernetes.io/role label",
			node: &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "r", Labels: map[string]string{"kubernetes.io/role": "master"}}},
			want: map[string]string{"ROLE": "master"},
			// no conditions: not ready
			health: Error,
		},
		{
			name: "metadata only",
			node: &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "bare"}},
			want: map[string]string{
				"NAME": "bare", "STATUS": "Unknown", "ROLE": MissingValue, "TAINTS": "0", "INTERNAL-IP": MissingValue,
				"EXTERNAL-IP": MissingValue, "PODS": NAValue, "LABELS": MissingValue, "VALID": "node is not ready", "AGE": UnknownValue,
			},
			health: Error,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			row, cells := rowOf(t, Nodes, tt.node)
			wantCells(t, cells, tt.want)
			if row.Health != tt.health {
				t.Errorf("health = %v, want %v", row.Health, tt.health)
			}
		})
	}
}

func TestNodeOrder(t *testing.T) {
	small := nodeWithReady(corev1.ConditionTrue)
	small.Status.Capacity[corev1.ResourcePods] = resource.MustParse("9")
	small.Spec.Taints = nil
	small.CreationTimestamp = ago(time.Hour)
	big := nodeWithReady(corev1.ConditionTrue)
	rs, _ := rowOf(t, Nodes, small)
	rb, _ := rowOf(t, Nodes, big)
	for _, col := range []string{"PODS", "TAINTS", "AGE"} {
		if orderOf(t, Nodes, rs, col) >= orderOf(t, Nodes, rb, col) {
			t.Errorf("%s: 9 pods, 0 taints, 1h does not sort before 110, 2, 50d", col)
		}
	}
}
