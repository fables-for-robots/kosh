package kube

import (
	"context"
	"fmt"
	"sort"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SecretData reads the Secret with the session's typed client and
// returns its entries sorted by key, each value as its bytes are: the
// API has already decoded the base64. A ref that is not a Secret is an
// error, before anything is read. The API refuses a role that may not
// get the Secret, and the refusal is logged.
func (s *session) SecretData(ctx context.Context, ref ObjectRef) ([]SecretEntry, error) {
	ctx, release := s.bound(ctx)
	defer release()
	if err := s.c.checkRef(ref); err != nil {
		return nil, err
	}
	if !ref.Resource.Is("", "secrets") {
		return nil, fmt.Errorf("%s is not a Secret", ref)
	}
	api, err := s.api()
	if err != nil {
		return nil, err
	}
	secret, err := api.typed.CoreV1().Secrets(ref.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
	if err != nil {
		s.logReadRefusal(ctx, readSecretData, ref, err)
		return nil, err
	}
	entries := make([]SecretEntry, 0, len(secret.Data))
	for k, v := range secret.Data {
		entries = append(entries, SecretEntry{Key: k, Value: string(v)})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Key < entries[j].Key })
	return entries, nil
}
