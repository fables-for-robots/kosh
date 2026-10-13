package render

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestServiceColumns(t *testing.T) {
	wantColumns(t, Services, "NAMESPACE", "NAME", "TYPE", "CLUSTER-IP", "EXTERNAL-IP", "SELECTOR*", "PORTS", "LABELS*", "VALID*", "AGE")
}

func TestServiceRows(t *testing.T) {
	meta := func(name string) metav1.ObjectMeta {
		return metav1.ObjectMeta{Namespace: "shop", Name: name, CreationTimestamp: ago(3 * time.Hour)}
	}
	tests := []struct {
		name   string
		svc    *corev1.Service
		want   map[string]string
		health Health
	}{
		{
			name: "cluster ip with two ports",
			svc: &corev1.Service{
				ObjectMeta: func() metav1.ObjectMeta {
					m := meta("web")
					m.Labels = map[string]string{"tier": "front", "app": "web"}
					return m
				}(),
				Spec: corev1.ServiceSpec{
					Type:      corev1.ServiceTypeClusterIP,
					ClusterIP: "10.0.0.5",
					Selector:  map[string]string{"app": "web"},
					Ports: []corev1.ServicePort{
						{Name: "http", Port: 80, Protocol: corev1.ProtocolTCP},
						{Port: 53, Protocol: corev1.ProtocolUDP},
					},
				},
			},
			want: map[string]string{
				"NAMESPACE": "shop", "NAME": "web", "TYPE": "ClusterIP", "CLUSTER-IP": "10.0.0.5",
				"EXTERNAL-IP": "", "SELECTOR": "app=web", "PORTS": "http:80►0 53►0╱UDP",
				"LABELS": "app=web,tier=front", "VALID": "", "AGE": "3h",
			},
		},
		{
			name: "load balancer with an address",
			svc: &corev1.Service{
				ObjectMeta: meta("lb"),
				Spec: corev1.ServiceSpec{
					Type: corev1.ServiceTypeLoadBalancer, ClusterIP: "10.0.0.6",
					Ports: []corev1.ServicePort{{Port: 443, NodePort: 30443, Protocol: corev1.ProtocolTCP}},
				},
				Status: corev1.ServiceStatus{LoadBalancer: corev1.LoadBalancerStatus{Ingress: []corev1.LoadBalancerIngress{
					{IP: "1.2.3.4"}, {Hostname: "lb.example.com"},
				}}},
			},
			want: map[string]string{"EXTERNAL-IP": "1.2.3.4,lb.example.com", "PORTS": "443►30443"},
		},
		{
			name: "load balancer without an address yet",
			svc: &corev1.Service{
				ObjectMeta: meta("lb2"),
				Spec:       corev1.ServiceSpec{Type: corev1.ServiceTypeLoadBalancer},
			},
			want: map[string]string{"EXTERNAL-IP": "<pending>"},
		},
		{
			name: "external name",
			svc: &corev1.Service{
				ObjectMeta: meta("ext"),
				Spec:       corev1.ServiceSpec{Type: corev1.ServiceTypeExternalName, ExternalName: "db.example.com"},
			},
			want: map[string]string{"EXTERNAL-IP": "db.example.com", "CLUSTER-IP": ""},
		},
		{
			name: "headless with external ips, sorted",
			svc: &corev1.Service{
				ObjectMeta: meta("hl"),
				Spec: corev1.ServiceSpec{
					Type: corev1.ServiceTypeClusterIP, ClusterIP: "None",
					ExternalIPs: []string{"9.9.9.9", "1.1.1.1"},
				},
			},
			want: map[string]string{"CLUSTER-IP": "", "EXTERNAL-IP": "1.1.1.1,9.9.9.9"},
		},
		{
			name: "metadata only",
			svc:  &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "bare"}},
			want: map[string]string{
				"NAMESPACE": "", "NAME": "bare", "TYPE": "", "CLUSTER-IP": "", "EXTERNAL-IP": "",
				"SELECTOR": MissingValue, "PORTS": "", "LABELS": MissingValue, "VALID": "", "AGE": UnknownValue,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			row, cells := rowOf(t, Services, tt.svc)
			wantCells(t, cells, tt.want)
			if row.Health != tt.health {
				t.Errorf("health = %v, want %v", row.Health, tt.health)
			}
		})
	}
}

func TestServiceDoesNotReorderSpec(t *testing.T) {
	svc := &corev1.Service{Spec: corev1.ServiceSpec{ExternalIPs: []string{"9.9.9.9", "1.1.1.1"}}}
	rowOf(t, Services, svc)
	if svc.Spec.ExternalIPs[0] != "9.9.9.9" {
		t.Errorf("the renderer sorted the object's ExternalIPs: %v", svc.Spec.ExternalIPs)
	}
}

func TestServiceOrder(t *testing.T) {
	young, _ := rowOf(t, Services, &corev1.Service{ObjectMeta: metav1.ObjectMeta{CreationTimestamp: ago(time.Hour)}})
	old, _ := rowOf(t, Services, &corev1.Service{ObjectMeta: metav1.ObjectMeta{CreationTimestamp: ago(48 * time.Hour)}})
	if orderOf(t, Services, young, "AGE") >= orderOf(t, Services, old, "AGE") {
		t.Error("a younger service does not sort before an older one")
	}
}
