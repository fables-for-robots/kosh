package kube

import (
	"context"
	"errors"
)

func (s *session) Delete(ctx context.Context, ref ObjectRef, opts DeleteOptions) error {
	return errors.New("not implemented")
}

func (s *session) Kill(ctx context.Context, pod ObjectRef) error {
	return errors.New("not implemented")
}

func (s *session) Update(ctx context.Context, ref ObjectRef, yaml string) error {
	return errors.New("not implemented")
}

func (s *session) Scale(ctx context.Context, ref ObjectRef, replicas int32) error {
	return errors.New("not implemented")
}

func (s *session) Restart(ctx context.Context, ref ObjectRef) error {
	return errors.New("not implemented")
}

func (s *session) SetImages(ctx context.Context, ref ObjectRef, images map[string]string) error {
	return errors.New("not implemented")
}

func (s *session) Rollback(ctx context.Context, replicaSet ObjectRef) error {
	return errors.New("not implemented")
}

func (s *session) Trigger(ctx context.Context, cronJob ObjectRef) (string, error) {
	return "", errors.New("not implemented")
}

func (s *session) SetSuspended(ctx context.Context, cronJob ObjectRef, suspended bool) error {
	return errors.New("not implemented")
}

func (s *session) Cordon(ctx context.Context, node ObjectRef, on bool) error {
	return errors.New("not implemented")
}

func (s *session) Drain(ctx context.Context, node ObjectRef, opts DrainOptions) error {
	return errors.New("not implemented")
}
