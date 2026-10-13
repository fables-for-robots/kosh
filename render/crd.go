// Derived from k9s's internal/render/crd.go, Copyright Authors of K9s, Apache-2.0 (see LICENSES/Apache-2.0.txt); changed for kosh.
// Unlike k9s's, NAME is the definition's name, and a definition that is
// not Established is invalid.

package render

import (
	"fmt"
	"strings"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

type customResourceDefinition struct{}

func init() {
	Register(CustomResourceDefinitions, customResourceDefinition{}, func() runtime.Object { return &apiextensionsv1.CustomResourceDefinition{} })
}

func (customResourceDefinition) Columns() []Column {
	return []Column{
		{Name: "NAME"},
		{Name: "GROUP"},
		{Name: "KIND"},
		{Name: "VERSIONS"},
		{Name: "SCOPE"},
		{Name: "ALIASES", Wide: true},
		{Name: "LABELS", Wide: true},
		{Name: "VALID", Wide: true},
		{Name: "AGE", Numeric: true},
	}
}

func (customResourceDefinition) Row(obj runtime.Object, now time.Time) (Row, error) {
	crd, ok := obj.(*apiextensionsv1.CustomResourceDefinition)
	if !ok {
		return Row{}, fmt.Errorf("custom resource definition renderer: got %T", obj)
	}
	var versions []string
	for _, v := range crd.Spec.Versions {
		if !v.Served {
			continue
		}
		name := v.Name
		if v.Deprecated {
			name += "!"
		}
		versions = append(versions, name)
	}
	return NewRow(crd).
		Text(crd.Name).
		Text(Or(crd.Spec.Group)).
		Text(Or(crd.Spec.Names.Kind)).
		Text(crdNA(versions)).
		Text(Or(string(crd.Spec.Scope))).
		Text(crdNA(crd.Spec.Names.ShortNames)).
		Map(crd.Labels).
		Valid(crdDiagnose(crd)).
		Age(crd.CreationTimestamp.Time, now).
		Row(), nil
}

// crdNA returns the values joined by commas, or NAValue when there are
// none.
func crdNA(values []string) string {
	if len(values) == 0 {
		return NAValue
	}
	return strings.Join(values, ",")
}

// crdDiagnose says what is wrong with a definition, in k9s's words for
// its versions, and says so when its condition Established is not true.
// The reasons are joined by " - ".
func crdDiagnose(crd *apiextensionsv1.CustomResourceDefinition) string {
	n := crd.Name
	var reasons []string
	if len(crd.Spec.Versions) == 0 {
		reasons = append(reasons, "unable to assert CRD servers versions for "+n)
	}
	served := len(crd.Spec.Versions) == 0 // said already
	for _, v := range crd.Spec.Versions {
		if v.Served {
			served = true
		}
		if v.Deprecated {
			if v.DeprecationWarning != nil {
				reasons = append(reasons, *v.DeprecationWarning)
			} else {
				reasons = append(reasons, fmt.Sprintf("%s[%s] is deprecated", n, v.Name))
			}
		}
	}
	if !served {
		reasons = append(reasons, fmt.Sprintf("CRD %s is no longer served by the api server", n))
	}
	if !crdEstablishedCondition(crd) {
		reasons = append(reasons, fmt.Sprintf("CRD %s is not established", n))
	}
	return strings.Join(reasons, " - ")
}

// crdEstablishedCondition reports whether the condition Established of
// the definition is true.
func crdEstablishedCondition(crd *apiextensionsv1.CustomResourceDefinition) bool {
	for _, c := range crd.Status.Conditions {
		if c.Type == apiextensionsv1.Established {
			return c.Status == apiextensionsv1.ConditionTrue
		}
	}
	return false
}
