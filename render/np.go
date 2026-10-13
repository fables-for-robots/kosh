// Follows k9s's internal/render/np.go (Apache-2.0).

package render

import (
	"fmt"
	"strings"
	"time"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

type networkPolicy struct{}

func init() {
	Register(NetworkPolicies, networkPolicy{}, func() runtime.Object { return &networkingv1.NetworkPolicy{} })
}

func (networkPolicy) Columns() []Column {
	return []Column{
		{Name: "NAMESPACE"},
		{Name: "NAME"},
		{Name: "POD-SELECTOR"},
		{Name: "ING-SELECTOR", Wide: true},
		{Name: "ING-PORTS", Wide: true},
		{Name: "ING-BLOCK", Wide: true},
		{Name: "EGR-SELECTOR", Wide: true},
		{Name: "EGR-PORTS", Wide: true},
		{Name: "EGR-BLOCK", Wide: true},
		{Name: "LABELS", Wide: true},
		{Name: "VALID", Wide: true},
		{Name: "AGE", Numeric: true},
	}
}

func (networkPolicy) Row(obj runtime.Object, now time.Time) (Row, error) {
	np, ok := obj.(*networkingv1.NetworkPolicy)
	if !ok {
		return Row{}, fmt.Errorf("network policy renderer: got %T", obj)
	}

	var ingPorts, ingSels, ingBlocks []string
	for _, rule := range np.Spec.Ingress {
		sels, blocks := npPeers(rule.From)
		ingPorts = npAppend(ingPorts, npPorts(rule.Ports))
		ingSels = npAppend(ingSels, sels)
		ingBlocks = npAppend(ingBlocks, blocks)
	}
	var egrPorts, egrSels, egrBlocks []string
	for _, rule := range np.Spec.Egress {
		sels, blocks := npPeers(rule.To)
		egrPorts = npAppend(egrPorts, npPorts(rule.Ports))
		egrSels = npAppend(egrSels, sels)
		egrBlocks = npAppend(egrBlocks, blocks)
	}

	return NewRow(np).
		Text(np.Namespace).
		Text(np.Name).
		Text(Or(npSelector(np.Spec.PodSelector))).
		Text(Join(ingSels)).
		Text(Join(ingPorts)).
		Text(Join(ingBlocks)).
		Text(Join(egrSels)).
		Text(Join(egrPorts)).
		Text(Join(egrBlocks)).
		Map(np.Labels).
		Valid("").
		Age(np.CreationTimestamp.Time, now).
		Row(), nil
}

// npAppend appends s to list unless it is empty.
func npAppend(list []string, s string) []string {
	if s == "" {
		return list
	}
	return append(list, s)
}

// npSelector returns the labels and the expressions of a selector as
// "a=1,b=2::key-Opvalues", or "" for a selector that holds neither.
func npSelector(s metav1.LabelSelector) string {
	var out string
	if len(s.MatchLabels) > 0 {
		out = MapText(s.MatchLabels)
	}
	if len(s.MatchExpressions) > 0 {
		out += "::" + npExpressions(s.MatchExpressions)
	}
	return out
}

func npExpressions(ee []metav1.LabelSelectorRequirement) string {
	ss := make([]string, len(ee))
	for i, e := range ee {
		ss[i] = fmt.Sprintf("%s-%s%s", e.Key, e.Operator, strings.Join(e.Values, ","))
	}
	return strings.Join(ss, ",")
}

// npPorts returns the ports of a rule as "TCP:80,UDP:53". What a port
// lacks is NAValue.
func npPorts(pp []networkingv1.NetworkPolicyPort) string {
	ports := make([]string, 0, len(pp))
	for _, p := range pp {
		proto, port := NAValue, NAValue
		if p.Protocol != nil {
			proto = string(*p.Protocol)
		}
		if p.Port != nil {
			port = p.Port.String()
		}
		ports = append(ports, proto+":"+port)
	}
	return strings.Join(ports, ",")
}

// npPeers returns the peers of a rule: those that select pods or
// namespaces, and those that are an IP block.
func npPeers(pp []networkingv1.NetworkPolicyPeer) (selectors, blocks string) {
	var sels, ips []string
	for _, p := range pp {
		sels = npAppend(sels, npPeer(p))
		if p.IPBlock != nil {
			ips = npAppend(ips, npBlock(p.IPBlock))
		}
	}
	return strings.Join(sels, ","), strings.Join(ips, ",")
}

// npBlock returns the CIDR of a block, and the first two of its
// exceptions.
func npBlock(b *networkingv1.IPBlock) string {
	switch {
	case len(b.Except) == 0:
		return b.CIDR
	case len(b.Except) > 2:
		return b.CIDR + "[" + strings.Join(b.Except[:2], ",") + "...]"
	}
	return b.CIDR + "[" + strings.Join(b.Except, ",") + "]"
}

// npPeer returns the selectors of a peer: "po:" for its pods, "ns:" for
// its namespaces, and "--" before an expression.
func npPeer(p networkingv1.NetworkPolicyPeer) string {
	var s string
	if p.PodSelector != nil {
		if len(p.PodSelector.MatchLabels) > 0 {
			s += "po:" + MapText(p.PodSelector.MatchLabels)
		}
		if len(p.PodSelector.MatchExpressions) > 0 {
			s += "--" + npExpressions(p.PodSelector.MatchExpressions)
		}
	}
	if p.NamespaceSelector != nil {
		if len(p.NamespaceSelector.MatchLabels) > 0 {
			s += "ns:" + MapText(p.NamespaceSelector.MatchLabels)
		}
		if len(p.NamespaceSelector.MatchExpressions) > 0 {
			s += "--" + npExpressions(p.NamespaceSelector.MatchExpressions)
		}
	}
	return s
}
