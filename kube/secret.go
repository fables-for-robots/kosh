package kube

import (
	"context"
	"fmt"
	"sort"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SecretData reads the Secret with the typed client and returns its
// entries sorted by key, each value as its bytes are: the API has
// already decoded the base64. A ref that is not a Secret is an error,
// whatever the grant; a read-only grant gets ErrReadOnly for a Secret,
// before anything is read, and the refusal is logged.
func (s *session) SecretData(ctx context.Context, ref ObjectRef) ([]SecretEntry, error) {
	if err := s.c.checkRef(ref); err != nil {
		return nil, err
	}
	if !ref.Resource.Is("", "secrets") {
		return nil, fmt.Errorf("%s is not a Secret", ref)
	}
	if err := s.mayRead(ctx, readSecretData, ref); err != nil {
		return nil, err
	}
	secret, err := s.c.typed.CoreV1().Secrets(ref.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	entries := make([]SecretEntry, 0, len(secret.Data))
	for k, v := range secret.Data {
		entries = append(entries, SecretEntry{Key: k, Value: string(v)})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Key < entries[j].Key })
	return entries, nil
}
