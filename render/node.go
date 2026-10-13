// Derived from k9s's internal/render/node.go, Copyright Authors of K9s, Apache-2.0 (see LICENSES/Apache-2.0.txt); changed for kosh.

package render

import (
	"fmt"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

const (
	nodeRolePrefix = "node-role.kubernetes.io/"
	nodeRoleSuffix = "kubernetes.io/role"
)

type node struct{}

func init() {
	Register(Nodes, node{}, func() runtime.Object { return &corev1.Node{} })
}

func (node) Columns() []Column {
	return []Column{
		{Name: "NAME"},
		{Name: "STATUS"},
		{Name: "ROLE"},
		{Name: "ARCH", Wide: true},
		{Name: "TAINTS", Numeric: true},
		{Name: "VERSION"},
		{Name: "OS-IMAGE", Wide: true},
		{Name: "KERNEL", Wide: true},
		{Name: "INTERNAL-IP", Wide: true},
		{Name: "EXTERNAL-IP", Wide: true},
		{Name: "PODS", Right: true, Numeric: true},
		{Name: "LABELS", Wide: true},
		{Name: "VALID", Wide: true},
		{Name: "AGE", Numeric: true},
	}
}

func (node) Row(obj runtime.Object, now time.Time) (Row, error) {
	n, ok := obj.(*corev1.Node)
	if !ok {
		return Row{}, fmt.Errorf("node renderer: got %T", obj)
	}
	statuses, ready := nodeStatuses(n)
	internal, external := nodeAddresses(n.Status.Addresses)

	b := NewRow(n).
		Text(n.Name).
		Text(strings.Join(statuses, ",")).
		Text(nodeRoles(n)).
		Text(n.Status.NodeInfo.Architecture).
		Int(int64(len(n.Spec.Taints))).
		Text(n.Status.NodeInfo.KubeletVersion).
		Text(n.Status.NodeInfo.OSImage).
		Text(n.Status.NodeInfo.KernelVersion).
		Text(Or(internal)).
		Text(Or(external))

	// k9s shows the number of pods that run on the node. The renderer
	// cannot know that from the node alone, so the cell holds the number
	// of pods the node can take, status.capacity.pods.
	if q, ok := n.Status.Capacity[corev1.ResourcePods]; ok {
		b.Int(q.Value())
	} else {
		b.Num(NAValue, 0)
	}

	// As k9s: VALID says why the node is not as it should be. A node
	// that is ready and cordoned is only on its way (Pending), so
	// Health is set after Valid, which would make the row an Error.
	reason := ""
	switch {
	case !ready:
		reason = "node is not ready"
	case n.Spec.Unschedulable:
		reason = "node is cordoned"
	}
	b.Map(n.Labels).
		Valid(reason).
		Age(n.CreationTimestamp.Time, now)
	if ready && n.Spec.Unschedulable {
		b.Health(Pending)
	}
	return b.Row(), nil
}

// nodeStatuses returns the words of the STATUS cell, sorted, and whether
// the node is ready. A node that does not report Ready is "Unknown".
func nodeStatuses(n *corev1.Node) ([]string, bool) {
	var statuses []string
	ready := false
	found := false
	for _, c := range n.Status.Conditions {
		if c.Type != corev1.NodeReady {
			continue
		}
		found = true
		if c.Status == corev1.ConditionTrue {
			ready = true
			statuses = append(statuses, "Ready")
		} else {
			statuses = append(statuses, "NotReady")
		}
	}
	if !found {
		statuses = append(statuses, "Unknown")
	}
	if n.Spec.Unschedulable {
		statuses = append(statuses, "SchedulingDisabled")
	}
	sort.Strings(statuses)
	return statuses, ready
}

// nodeRoles returns the roles of the node, sorted and joined by commas:
// the node-role.kubernetes.io/ labels, and the value of a
// kubernetes.io/role label. MissingValue when there are none.
func nodeRoles(n *corev1.Node) string {
	var roles []string
	for k, v := range n.Labels {
		switch {
		case strings.HasPrefix(k, nodeRolePrefix):
			if role := strings.TrimPrefix(k, nodeRolePrefix); role != "" {
				roles = append(roles, role)
			}
		case strings.HasSuffix(k, nodeRoleSuffix) && v != "":
			roles = append(roles, v)
		}
	}
	sort.Strings(roles)
	return Join(roles)
}

func nodeAddresses(addrs []corev1.NodeAddress) (internal, external string) {
	for _, a := range addrs {
		switch a.Type {
		case corev1.NodeInternalIP:
			internal = a.Address
		case corev1.NodeExternalIP:
			external = a.Address
		}
	}
	return internal, external
}
