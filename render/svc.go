// Follows k9s's internal/render/svc.go (Apache-2.0).

package render

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

type service struct{}

func init() {
	Register(Services, service{}, func() runtime.Object { return &corev1.Service{} })
}

func (service) Columns() []Column {
	return []Column{
		{Name: "NAMESPACE"},
		{Name: "NAME"},
		{Name: "TYPE"},
		{Name: "CLUSTER-IP"},
		{Name: "EXTERNAL-IP"},
		{Name: "SELECTOR", Wide: true},
		{Name: "PORTS"},
		{Name: "LABELS", Wide: true},
		{Name: "VALID", Wide: true},
		{Name: "AGE", Numeric: true},
	}
}

func (service) Row(obj runtime.Object, now time.Time) (Row, error) {
	svc, ok := obj.(*corev1.Service)
	if !ok {
		return Row{}, fmt.Errorf("service renderer: got %T", obj)
	}
	b := NewRow(svc).
		Text(svc.Namespace).
		Text(svc.Name).
		Text(string(svc.Spec.Type)).
		Text(svcClusterIP(svc.Spec.ClusterIP)).
		Text(svcExternalIPs(svc)).
		Map(svc.Spec.Selector).
		Text(svcPorts(svc.Spec.Ports)).
		Map(svc.Labels).
		Valid("").
		Age(svc.CreationTimestamp.Time, now)
	return b.Row(), nil
}

// svcClusterIP is the cluster IP, empty for a headless service.
func svcClusterIP(ip string) string {
	if ip == "None" {
		return ""
	}
	return ip
}

// svcExternalIPs is the external addresses of the service, sorted, joined
// by commas. A load balancer that has none yet shows "<pending>".
func svcExternalIPs(svc *corev1.Service) string {
	var ips []string
	switch svc.Spec.Type {
	case corev1.ServiceTypeNodePort, corev1.ServiceTypeClusterIP:
		ips = append(ips, svc.Spec.ExternalIPs...)
	case corev1.ServiceTypeLoadBalancer:
		for _, in := range svc.Status.LoadBalancer.Ingress {
			if in.IP != "" {
				ips = append(ips, in.IP)
			} else if in.Hostname != "" {
				ips = append(ips, in.Hostname)
			}
		}
		ips = append(ips, svc.Spec.ExternalIPs...)
		if len(ips) == 0 {
			return "<pending>"
		}
	case corev1.ServiceTypeExternalName:
		ips = append(ips, svc.Spec.ExternalName)
	}
	// Deliberately every address on its own, sorted: the cell then does
	// not change when the API returns the ingress addresses in another
	// order. k9s sorts the ingress addresses, joined as one text in the
	// order of the API, together with the external IPs.
	sort.Strings(ips)
	return strings.Join(ips, ",")
}

// svcPorts is the ports as k9s writes them: "name:port►nodePort", with
// "╱PROTOCOL" when the protocol is not TCP.
func svcPorts(pp []corev1.ServicePort) string {
	ports := make([]string, len(pp))
	for i, p := range pp {
		if p.Name != "" {
			ports[i] = p.Name + ":"
		}
		ports[i] += strconv.Itoa(int(p.Port)) + "►" + strconv.Itoa(int(p.NodePort))
		if p.Protocol != corev1.ProtocolTCP && p.Protocol != "" {
			ports[i] += "╱" + string(p.Protocol)
		}
	}
	return strings.Join(ports, " ")
}
