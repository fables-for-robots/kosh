package render

import (
	"fmt"
	"strings"
	"time"

	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// The columns of ingresses are those of kubectl get ingress: k9s has no
// renderer of its own for them.
type ingress struct{}

func init() {
	Register(Ingresses, ingress{}, func() runtime.Object { return &networkingv1.Ingress{} })
}

func (ingress) Columns() []Column {
	return []Column{
		{Name: "NAMESPACE"},
		{Name: "NAME"},
		{Name: "CLASS"},
		{Name: "HOSTS"},
		{Name: "ADDRESS"},
		{Name: "PORTS"},
		{Name: "AGE", Numeric: true},
	}
}

func (ingress) Row(obj runtime.Object, now time.Time) (Row, error) {
	ing, ok := obj.(*networkingv1.Ingress)
	if !ok {
		return Row{}, fmt.Errorf("ingress renderer: got %T", obj)
	}
	class := MissingValue
	if ing.Spec.IngressClassName != nil && *ing.Spec.IngressClassName != "" {
		class = *ing.Spec.IngressClassName
	}
	b := NewRow(ing).
		Text(ing.Namespace).
		Text(ing.Name).
		Text(class).
		Text(ingHosts(ing)).
		Text(ingAddress(ing)).
		Text(ingPorts(ing)).
		Age(ing.CreationTimestamp.Time, now)
	return b.Row(), nil
}

// ingHosts is the hosts of the rules joined by commas, "*" when no rule
// names one.
func ingHosts(ing *networkingv1.Ingress) string {
	var hosts []string
	for _, r := range ing.Spec.Rules {
		if r.Host != "" {
			hosts = append(hosts, r.Host)
		}
	}
	if len(hosts) == 0 {
		return "*"
	}
	return strings.Join(hosts, ",")
}

// ingAddress is the IPs and host names of the load balancer, joined by
// commas; empty when there are none.
func ingAddress(ing *networkingv1.Ingress) string {
	var addrs []string
	for _, in := range ing.Status.LoadBalancer.Ingress {
		if in.IP != "" {
			addrs = append(addrs, in.IP)
		} else if in.Hostname != "" {
			addrs = append(addrs, in.Hostname)
		}
	}
	return strings.Join(addrs, ",")
}

// ingPorts is "80", or "80, 443" when the ingress has a TLS section.
func ingPorts(ing *networkingv1.Ingress) string {
	if len(ing.Spec.TLS) > 0 {
		return "80, 443"
	}
	return "80"
}
