package kube

import (
	"context"
	"errors"
)

func (s *session) Describe(ctx context.Context, ref ObjectRef) (string, error) {
	return "", errors.New("not implemented")
}
