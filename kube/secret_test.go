package kube

import (
	"context"
	"errors"
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestSecretData(t *testing.T) {
	binary := string([]byte{0x00, 0xff, 0x1b, '[', '2', 'J', 0x80})
	full := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "full"},
		Data: map[string][]byte{
			"username": []byte("admin"),
			"cert.bin": []byte(binary),
			"api-key":  []byte("k3y"),
		},
	}
	empty := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "empty"}}
	e := newEnv(t, envConfig{}, full, empty, readConfigMap())
	secrets := e.resource(t, "", "secrets")
	ctx := context.Background()

	ro := readSessionOf(t, e, readOnlyGrant)
	for _, ref := range []ObjectRef{
		{Resource: secrets, Namespace: "default", Name: "full"},
		{Resource: e.resource(t, "", "configmaps"), Namespace: "default", Name: "settings"},
	} {
		if _, err := ro.SecretData(ctx, ref); !errors.Is(err, ErrReadOnly) {
			t.Errorf("read-only, %s: got %v, want ErrReadOnly", ref, err)
		}
	}
	if aa := append(readActionsOn(e, "secrets"), readActionsOn(e, "configmaps")...); len(aa) != 0 {
		t.Errorf("read-only: something was read: %v", aa)
	}

	s := e.session(t)
	got, err := s.SecretData(ctx, ObjectRef{Resource: secrets, Namespace: "default", Name: "full"})
	if err != nil {
		t.Fatalf("SecretData: %v", err)
	}
	want := []SecretEntry{
		{Key: "api-key", Value: "k3y"},
		{Key: "cert.bin", Value: binary},
		{Key: "username", Value: "admin"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("entries %q, want %q", got, want)
	}

	got, err = s.SecretData(ctx, ObjectRef{Resource: secrets, Namespace: "default", Name: "empty"})
	if err != nil || len(got) != 0 {
		t.Errorf("a Secret without data: %q, %v; want no entries", got, err)
	}

	if _, err := s.SecretData(ctx, ObjectRef{Resource: e.resource(t, "", "configmaps"), Namespace: "default", Name: "settings"}); err == nil {
		t.Error("a configmap's reference: no error")
	}
	if _, err := s.SecretData(ctx, ObjectRef{Resource: secrets, Namespace: "default", Name: "missing"}); err == nil {
		t.Error("a Secret that is not there: no error")
	}
}
