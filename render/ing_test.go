package render

import (
	"testing"
	"time"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestIngressColumns(t *testing.T) {
	wantColumns(t, Ingresses, "NAMESPACE", "NAME", "CLASS", "HOSTS", "ADDRESS", "PORTS", "AGE")
}

func TestIngressRows(t *testing.T) {
	class := "nginx"
	meta := metav1.ObjectMeta{Namespace: "shop", Name: "web", CreationTimestamp: ago(4 * 24 * time.Hour)}
	tests := []struct {
		name   string
		ing    *networkingv1.Ingress
		want   map[string]string
		health Health
	}{
		{"with tls", &networkingv1.Ingress{
			ObjectMeta: meta,
			Spec: networkingv1.IngressSpec{
				IngressClassName: &class,
				Rules:            []networkingv1.IngressRule{{Host: "a.example.com"}, {Host: ""}, {Host: "b.example.com"}},
				TLS:              []networkingv1.IngressTLS{{Hosts: []string{"a.example.com"}}},
			},
			Status: networkingv1.IngressStatus{LoadBalancer: networkingv1.IngressLoadBalancerStatus{Ingress: []networkingv1.IngressLoadBalancerIngress{
				{IP: "1.2.3.4"}, {Hostname: "lb.example.com"},
			}}},
		}, map[string]string{
			"NAMESPACE": "shop", "NAME": "web", "CLASS": "nginx", "HOSTS": "a.example.com,b.example.com",
			"ADDRESS": "1.2.3.4,lb.example.com", "PORTS": "80, 443", "AGE": "4d",
		}, OK},
		{"rules without a host", &networkingv1.Ingress{
			ObjectMeta: meta,
			Spec:       networkingv1.IngressSpec{Rules: []networkingv1.IngressRule{{}}},
		}, map[string]string{"CLASS": MissingValue, "HOSTS": "*", "ADDRESS": "", "PORTS": "80"}, OK},
		{"metadata only", &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Name: "bare"}},
			map[string]string{"NAME": "bare", "CLASS": MissingValue, "HOSTS": "*", "ADDRESS": "", "PORTS": "80", "AGE": UnknownValue}, OK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			row, cells := rowOf(t, Ingresses, tt.ing)
			wantCells(t, cells, tt.want)
			if row.Health != tt.health {
				t.Errorf("health = %v, want %v", row.Health, tt.health)
			}
		})
	}
}

func TestIngressOrder(t *testing.T) {
	young, _ := rowOf(t, Ingresses, &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{CreationTimestamp: ago(time.Minute)}})
	old, _ := rowOf(t, Ingresses, &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{CreationTimestamp: ago(time.Hour)}})
	if orderOf(t, Ingresses, young, "AGE") >= orderOf(t, Ingresses, old, "AGE") {
		t.Error("a younger ingress does not sort before an older one")
	}
}
