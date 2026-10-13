package kube

import (
	"context"
	"fmt"
	"sort"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SecretData reads the Secret with the typed client and returns its
// entries sorted by key, each value as its bytes are: the API has
// already decoded the base64. A read-only grant gets ErrReadOnly,
// whatever ref is, before anything is read; a ref that is not a Secret
// is an error.
func (s *session) SecretData(ctx context.Context, ref ObjectRef) ([]SecretEntry, error) {
	if s.grant.ReadOnly {
		return nil, ErrReadOnly
	}
	if err := checkRef(ref); err != nil {
		return nil, err
	}
	if !ref.Resource.Is("", "secrets") {
		return nil, fmt.Errorf("%s is not a Secret", ref)
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
