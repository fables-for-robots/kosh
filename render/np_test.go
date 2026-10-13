package render

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

func npPort(proto corev1.Protocol, port int) networkingv1.NetworkPolicyPort {
	p := intstr.FromInt32(int32(port))
	return networkingv1.NetworkPolicyPort{Protocol: &proto, Port: &p}
}

func TestNetworkPoliciesColumns(t *testing.T) {
	wantColumns(t, NetworkPolicies, "NAMESPACE", "NAME", "POD-SELECTOR", "ING-SELECTOR*", "ING-PORTS*", "ING-BLOCK*", "EGR-SELECTOR*", "EGR-PORTS*", "EGR-BLOCK*", "LABELS*", "VALID*", "AGE")
}

func TestNetworkPoliciesRows(t *testing.T) {
	tests := []struct {
		name   string
		obj    *networkingv1.NetworkPolicy
		want   map[string]string
		health Health
	}{
		{
			name: "ingress and egress",
			obj: &networkingv1.NetworkPolicy{
				ObjectMeta: metav1.ObjectMeta{Namespace: "prod", Name: "db", CreationTimestamp: ago(10 * 24 * time.Hour), Labels: map[string]string{"a": "b"}},
				Spec: networkingv1.NetworkPolicySpec{
					PodSelector: metav1.LabelSelector{
						MatchLabels:      map[string]string{"app": "web"},
						MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "tier", Operator: metav1.LabelSelectorOpIn, Values: []string{"a", "b"}}},
					},
					Ingress: []networkingv1.NetworkPolicyIngressRule{
						{
							From: []networkingv1.NetworkPolicyPeer{
								{PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "api"}}},
								{IPBlock: &networkingv1.IPBlock{CIDR: "10.0.0.0/8", Except: []string{"10.1.0.0/16", "10.2.0.0/16", "10.3.0.0/16"}}},
							},
							Ports: []networkingv1.NetworkPolicyPort{npPort(corev1.ProtocolTCP, 5432)},
						},
					},
					Egress: []networkingv1.NetworkPolicyEgressRule{
						{
							To:    []networkingv1.NetworkPolicyPeer{{NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"name": "kube-system"}}}},
							Ports: []networkingv1.NetworkPolicyPort{npPort(corev1.ProtocolUDP, 53)},
						},
					},
				},
			},
			want: map[string]string{
				"NAMESPACE": "prod", "NAME": "db", "POD-SELECTOR": "app=web::tier-Ina,b",
				"ING-SELECTOR": "po:app=api", "ING-PORTS": "TCP:5432", "ING-BLOCK": "10.0.0.0/8[10.1.0.0/16,10.2.0.0/16...]",
				"EGR-SELECTOR": "ns:name=kube-system", "EGR-PORTS": "UDP:53", "EGR-BLOCK": MissingValue,
				"LABELS": "a=b", "VALID": "", "AGE": "10d",
			},
		},
		{
			name: "a block with one exception, and a port without a protocol",
			obj: &networkingv1.NetworkPolicy{
				ObjectMeta: metav1.ObjectMeta{Name: "x"},
				Spec: networkingv1.NetworkPolicySpec{
					Egress: []networkingv1.NetworkPolicyEgressRule{
						{
							To:    []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{CIDR: "0.0.0.0/0", Except: []string{"169.254.0.0/16"}}}},
							Ports: []networkingv1.NetworkPolicyPort{{Port: func() *intstr.IntOrString { p := intstr.FromString("metrics"); return &p }()}},
						},
					},
				},
			},
			want: map[string]string{"EGR-BLOCK": "0.0.0.0/0[169.254.0.0/16]", "EGR-PORTS": "n/a:metrics", "EGR-SELECTOR": MissingValue},
		},
		{
			name: "metadata only",
			obj:  &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: "m"}},
			want: map[string]string{
				"NAME": "m", "POD-SELECTOR": MissingValue,
				"ING-SELECTOR": MissingValue, "ING-PORTS": MissingValue, "ING-BLOCK": MissingValue,
				"EGR-SELECTOR": MissingValue, "EGR-PORTS": MissingValue, "EGR-BLOCK": MissingValue,
				"LABELS": MissingValue, "VALID": "", "AGE": UnknownValue,
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			row, cells := rowOf(t, NetworkPolicies, tc.obj)
			wantCells(t, cells, tc.want)
			if row.Health != tc.health {
				t.Errorf("health = %v, want %v", row.Health, tc.health)
			}
		})
	}
}

func TestNetworkPoliciesOrder(t *testing.T) {
	young := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{CreationTimestamp: ago(time.Minute)}}
	old := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{CreationTimestamp: ago(time.Hour)}}
	a, _ := rowOf(t, NetworkPolicies, young)
	b, _ := rowOf(t, NetworkPolicies, old)
	if orderOf(t, NetworkPolicies, a, "AGE") >= orderOf(t, NetworkPolicies, b, "AGE") {
		t.Error("the younger should sort before the older")
	}
}
