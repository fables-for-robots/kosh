package render

import (
	"encoding/base64"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestSecretColumns(t *testing.T) {
	wantColumns(t, Secrets, "NAMESPACE", "NAME", "TYPE", "DATA", "VALID*", "AGE")
}

func TestSecretRows(t *testing.T) {
	tests := []struct {
		name string
		sec  *corev1.Secret
		want map[string]string
	}{
		{"with data", &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "creds", CreationTimestamp: ago(30 * time.Minute)},
			Type:       corev1.SecretTypeBasicAuth,
			Data:       map[string][]byte{"username": []byte("admin"), "password": []byte("hunter2")},
		}, map[string]string{"NAMESPACE": "shop", "NAME": "creds", "TYPE": "kubernetes.io/basic-auth", "DATA": "2", "VALID": "", "AGE": "30m"}},
		{"metadata only", &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "bare"}},
			map[string]string{"NAME": "bare", "TYPE": "", "DATA": "0", "VALID": "", "AGE": UnknownValue}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			row, cells := rowOf(t, Secrets, tt.sec)
			wantCells(t, cells, tt.want)
			if row.Health != OK {
				t.Errorf("health = %v", row.Health)
			}
		})
	}
}

// No cell of a secret's row, and no label of it, holds a value of the
// secret.
func TestSecretRowHoldsNoValue(t *testing.T) {
	const marker = "s3cr3t-m4rker"
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "creds", Labels: map[string]string{"app": "x"}},
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{"k": []byte(marker)},
		StringData: map[string]string{"j": marker},
	}
	row, _ := rowOf(t, Secrets, sec)
	forms := map[string]string{
		"raw":    marker,
		"base64": base64.StdEncoding.EncodeToString([]byte(marker)),
		"bytes":  fmt.Sprint([]byte(marker)),
	}
	for i, c := range row.Cells {
		for form, needle := range forms {
			if strings.Contains(c, needle) {
				t.Errorf("cell %d holds the value (%s): %q", i, form, c)
			}
		}
	}
	if !reflect.DeepEqual(row.Labels, sec.Labels) {
		t.Errorf("Labels = %v, want exactly the object's %v", row.Labels, sec.Labels)
	}
}

func TestSecretOrder(t *testing.T) {
	few, _ := rowOf(t, Secrets, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{CreationTimestamp: ago(time.Minute)}, Data: map[string][]byte{"a": nil}})
	many, _ := rowOf(t, Secrets, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{CreationTimestamp: ago(time.Hour)}, Data: map[string][]byte{"a": nil, "b": nil, "c": nil, "d": nil, "e": nil, "f": nil, "g": nil, "h": nil, "i": nil, "j": nil, "k": nil}})
	for _, col := range []string{"DATA", "AGE"} {
		if orderOf(t, Secrets, few, col) >= orderOf(t, Secrets, many, col) {
			t.Errorf("%s does not sort 1 entry / 1m before 11 entries / 1h", col)
		}
	}
}
