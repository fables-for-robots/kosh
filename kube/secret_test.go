package kube

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stesting "k8s.io/client-go/testing"
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

	// A ConfigMap is no Secret: an error, and nothing is read.
	cm := ObjectRef{Resource: e.resource(t, "", "configmaps"), Namespace: "default", Name: "settings"}
	if _, err := e.session(t).SecretData(ctx, cm); err == nil || !strings.Contains(err.Error(), "is not a Secret") {
		t.Errorf("a ConfigMap: got %v, want that it is not a Secret", err)
	}
	if aa := append(readActionsOn(e, "secrets"), readActionsOn(e, "configmaps")...); len(aa) != 0 {
		t.Errorf("something was read: %v", aa)
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

// kosh refuses a view key nothing: its SecretData goes to the API, and
// what it gets is the API's answer, a refusal or the data.
func TestNoReadOnlyRefusal(t *testing.T) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "token"},
		Data:       map[string][]byte{"token": []byte("s3cr3t")},
	}
	refused := true
	e := newEnv(t, envConfig{before: func(e *env) {
		e.typed.PrependReactor("get", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
			if !refused {
				return false, nil, nil
			}
			return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "secrets"}, "token", errors.New("view may not get secrets"))
		})
	}}, secret)
	ref := ObjectRef{Resource: e.resource(t, "", "secrets"), Namespace: "default", Name: "token"}
	view := e.sessionOf(t, viewGrant)
	e.typed.ClearActions()
	if _, err := view.SecretData(context.Background(), ref); !apierrors.IsForbidden(err) {
		t.Errorf("got %v, want the API's refusal", err)
	}
	if aa := readActionsOn(e, "secrets"); len(aa) != 1 || aa[0].GetVerb() != "get" {
		t.Errorf("the API was asked %v, want one get", aa)
	}
	refused = false
	got, err := view.SecretData(context.Background(), ref)
	if err != nil || len(got) != 1 || got[0].Value != "s3cr3t" {
		t.Errorf("allowed by the API: %v, %v", got, err)
	}
}
