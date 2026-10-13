package kube

import (
	"context"
	"errors"
)

func (s *session) Describe(ctx context.Context, ref ObjectRef) (string, error) {
	return "", errors.New("not implemented")
}

func (s *session) Owner(ctx context.Context, ref ObjectRef) (ObjectRef, error) {
	return ObjectRef{}, errors.New("not implemented")
}

func (s *session) Images(ctx context.Context, ref ObjectRef) ([]ContainerImage, error) {
	return nil, errors.New("not implemented")
}

func (s *session) Replicas(ctx context.Context, ref ObjectRef) (int32, error) {
	return 0, errors.New("not implemented")
}
