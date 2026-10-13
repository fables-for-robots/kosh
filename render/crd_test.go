package render

import (
	"testing"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func crdEstablished(status apiextensionsv1.ConditionStatus) []apiextensionsv1.CustomResourceDefinitionCondition {
	return []apiextensionsv1.CustomResourceDefinitionCondition{
		{Type: apiextensionsv1.NamesAccepted, Status: apiextensionsv1.ConditionTrue},
		{Type: apiextensionsv1.Established, Status: status},
	}
}

func crdSpec(versions ...apiextensionsv1.CustomResourceDefinitionVersion) apiextensionsv1.CustomResourceDefinitionSpec {
	return apiextensionsv1.CustomResourceDefinitionSpec{
		Group: "example.com",
		Names: apiextensionsv1.CustomResourceDefinitionNames{Plural: "widgets", Kind: "Widget", ShortNames: []string{"wd", "wg"}},
		Scope: apiextensionsv1.NamespaceScoped, Versions: versions,
	}
}

func TestCustomResourceDefinitionsColumns(t *testing.T) {
	wantColumns(t, CustomResourceDefinitions, "NAME", "GROUP", "KIND", "VERSIONS", "SCOPE", "ALIASES*", "LABELS*", "VALID*", "AGE")
}

func TestCustomResourceDefinitionsRows(t *testing.T) {
	deprecation := "use v2"
	tests := []struct {
		name   string
		obj    *apiextensionsv1.CustomResourceDefinition
		want   map[string]string
		health Health
	}{
		{
			name: "established, one version served and one not",
			obj: &apiextensionsv1.CustomResourceDefinition{
				ObjectMeta: metav1.ObjectMeta{Name: "widgets.example.com", CreationTimestamp: ago(9 * 24 * time.Hour), Labels: map[string]string{"a": "b"}},
				Spec: crdSpec(
					apiextensionsv1.CustomResourceDefinitionVersion{Name: "v1", Served: true, Storage: true},
					apiextensionsv1.CustomResourceDefinitionVersion{Name: "v1alpha1", Served: false},
					apiextensionsv1.CustomResourceDefinitionVersion{Name: "v2", Served: true},
				),
				Status: apiextensionsv1.CustomResourceDefinitionStatus{Conditions: crdEstablished(apiextensionsv1.ConditionTrue)},
			},
			want: map[string]string{
				"NAME": "widgets.example.com", "GROUP": "example.com", "KIND": "Widget", "VERSIONS": "v1,v2",
				"SCOPE": "Namespaced", "ALIASES": "wd,wg", "LABELS": "a=b", "VALID": "", "AGE": "9d",
			},
		},
		{
			name: "cluster scoped, without aliases",
			obj: &apiextensionsv1.CustomResourceDefinition{
				ObjectMeta: metav1.ObjectMeta{Name: "gadgets.example.com"},
				Spec: apiextensionsv1.CustomResourceDefinitionSpec{
					Group: "example.com", Scope: apiextensionsv1.ClusterScoped,
					Names:    apiextensionsv1.CustomResourceDefinitionNames{Plural: "gadgets", Kind: "Gadget"},
					Versions: []apiextensionsv1.CustomResourceDefinitionVersion{{Name: "v1", Served: true}},
				},
				Status: apiextensionsv1.CustomResourceDefinitionStatus{Conditions: crdEstablished(apiextensionsv1.ConditionTrue)},
			},
			want: map[string]string{"NAME": "gadgets.example.com", "SCOPE": "Cluster", "ALIASES": NAValue, "VALID": ""},
		},
		{
			name: "not established",
			obj: &apiextensionsv1.CustomResourceDefinition{
				ObjectMeta: metav1.ObjectMeta{Name: "widgets.example.com"},
				Spec:       crdSpec(apiextensionsv1.CustomResourceDefinitionVersion{Name: "v1", Served: true}),
				Status:     apiextensionsv1.CustomResourceDefinitionStatus{Conditions: crdEstablished(apiextensionsv1.ConditionFalse)},
			},
			want:   map[string]string{"VALID": "CRD widgets.example.com is not established"},
			health: Error,
		},
		{
			name: "without the condition",
			obj: &apiextensionsv1.CustomResourceDefinition{
				ObjectMeta: metav1.ObjectMeta{Name: "widgets.example.com"},
				Spec:       crdSpec(apiextensionsv1.CustomResourceDefinitionVersion{Name: "v1", Served: true}),
			},
			want:   map[string]string{"VALID": "CRD widgets.example.com is not established"},
			health: Error,
		},
		{
			name: "a deprecated version",
			obj: &apiextensionsv1.CustomResourceDefinition{
				ObjectMeta: metav1.ObjectMeta{Name: "widgets.example.com"},
				Spec: crdSpec(
					apiextensionsv1.CustomResourceDefinitionVersion{Name: "v1", Served: true},
					apiextensionsv1.CustomResourceDefinitionVersion{Name: "v1beta1", Served: true, Deprecated: true},
				),
				Status: apiextensionsv1.CustomResourceDefinitionStatus{Conditions: crdEstablished(apiextensionsv1.ConditionTrue)},
			},
			want:   map[string]string{"VERSIONS": "v1,v1beta1!", "VALID": "widgets.example.com[v1beta1] is deprecated"},
			health: Error,
		},
		{
			name: "a deprecated version with a warning",
			obj: &apiextensionsv1.CustomResourceDefinition{
				ObjectMeta: metav1.ObjectMeta{Name: "widgets.example.com"},
				Spec: crdSpec(
					apiextensionsv1.CustomResourceDefinitionVersion{Name: "v1", Served: true, Deprecated: true, DeprecationWarning: &deprecation},
				),
				Status: apiextensionsv1.CustomResourceDefinitionStatus{Conditions: crdEstablished(apiextensionsv1.ConditionTrue)},
			},
			want:   map[string]string{"VALID": "use v2"},
			health: Error,
		},
		{
			name: "no version served",
			obj: &apiextensionsv1.CustomResourceDefinition{
				ObjectMeta: metav1.ObjectMeta{Name: "widgets.example.com"},
				Spec:       crdSpec(apiextensionsv1.CustomResourceDefinitionVersion{Name: "v1", Served: false}),
				Status:     apiextensionsv1.CustomResourceDefinitionStatus{Conditions: crdEstablished(apiextensionsv1.ConditionTrue)},
			},
			want:   map[string]string{"VERSIONS": NAValue, "VALID": "CRD widgets.example.com is no longer served by the api server"},
			health: Error,
		},
		{
			name: "two faults are both said",
			obj: &apiextensionsv1.CustomResourceDefinition{
				ObjectMeta: metav1.ObjectMeta{Name: "widgets.example.com"},
				Spec:       crdSpec(apiextensionsv1.CustomResourceDefinitionVersion{Name: "v1", Served: false}),
			},
			want: map[string]string{
				"VALID": "CRD widgets.example.com is no longer served by the api server - CRD widgets.example.com is not established",
			},
			health: Error,
		},
		{
			name: "no versions",
			obj: &apiextensionsv1.CustomResourceDefinition{
				ObjectMeta: metav1.ObjectMeta{Name: "widgets.example.com"},
				Spec:       crdSpec(),
				Status:     apiextensionsv1.CustomResourceDefinitionStatus{Conditions: crdEstablished(apiextensionsv1.ConditionTrue)},
			},
			want:   map[string]string{"VERSIONS": NAValue, "VALID": "unable to assert CRD servers versions for widgets.example.com"},
			health: Error,
		},
		{
			name: "metadata only",
			obj:  &apiextensionsv1.CustomResourceDefinition{ObjectMeta: metav1.ObjectMeta{Name: "m.example.com"}},
			want: map[string]string{
				"NAME": "m.example.com", "GROUP": MissingValue, "KIND": MissingValue, "VERSIONS": NAValue, "SCOPE": MissingValue,
				"ALIASES": NAValue, "LABELS": MissingValue, "AGE": UnknownValue,
			},
			health: Error,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			row, cells := rowOf(t, CustomResourceDefinitions, tc.obj)
			wantCells(t, cells, tc.want)
			if row.Health != tc.health {
				t.Errorf("health = %v, want %v", row.Health, tc.health)
			}
			if row.Name != tc.obj.Name || cells["NAME"] != tc.obj.Name {
				t.Errorf("name = %q / %q, want %q", row.Name, cells["NAME"], tc.obj.Name)
			}
		})
	}
}

func TestCustomResourceDefinitionsOrder(t *testing.T) {
	young := &apiextensionsv1.CustomResourceDefinition{ObjectMeta: metav1.ObjectMeta{CreationTimestamp: ago(time.Minute)}}
	old := &apiextensionsv1.CustomResourceDefinition{ObjectMeta: metav1.ObjectMeta{CreationTimestamp: ago(time.Hour)}}
	a, _ := rowOf(t, CustomResourceDefinitions, young)
	b, _ := rowOf(t, CustomResourceDefinitions, old)
	if orderOf(t, CustomResourceDefinitions, a, "AGE") >= orderOf(t, CustomResourceDefinitions, b, "AGE") {
		t.Error("the younger should sort before the older")
	}
}
