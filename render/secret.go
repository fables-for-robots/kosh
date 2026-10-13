// Follows k9s's internal/render/secret.go (Apache-2.0).

package render

import (
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

type secret struct{}

func init() {
	Register(Secrets, secret{}, func() runtime.Object { return &corev1.Secret{} })
}

func (secret) Columns() []Column {
	return []Column{
		{Name: "NAMESPACE"},
		{Name: "NAME"},
		{Name: "TYPE"},
		{Name: "DATA", Numeric: true},
		{Name: "VALID", Wide: true},
		{Name: "AGE", Numeric: true},
	}
}

// Row shows the number of entries of the secret and nothing of their
// contents: no cell and no label holds a value of it.
func (secret) Row(obj runtime.Object, now time.Time) (Row, error) {
	sec, ok := obj.(*corev1.Secret)
	if !ok {
		return Row{}, fmt.Errorf("secret renderer: got %T", obj)
	}
	b := NewRow(sec).
		Text(sec.Namespace).
		Text(sec.Name).
		Text(string(sec.Type)).
		Int(int64(len(sec.Data))).
		Valid("").
		Age(sec.CreationTimestamp.Time, now)
	return b.Row(), nil
}
