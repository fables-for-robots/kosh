package kube

import (
	"context"
	"errors"
)

var errNotYet = errors.New("not implemented")

func (s *session) Delete(ctx context.Context, ref ObjectRef, opts DeleteOptions) error {
	if err := s.begin(ctx, actDelete, ref); err != nil {
		return err
	}
	return errNotYet
}

func (s *session) Kill(ctx context.Context, pod ObjectRef) error {
	if err := s.begin(ctx, actKill, pod); err != nil {
		return err
	}
	return errNotYet
}

func (s *session) Update(ctx context.Context, ref ObjectRef, yaml string) error {
	if err := s.begin(ctx, actUpdate, ref); err != nil {
		return err
	}
	return errNotYet
}

func (s *session) Scale(ctx context.Context, ref ObjectRef, replicas int32) error {
	if err := s.begin(ctx, actScale, ref); err != nil {
		return err
	}
	return errNotYet
}

func (s *session) Restart(ctx context.Context, ref ObjectRef) error {
	if err := s.begin(ctx, actRestart, ref); err != nil {
		return err
	}
	return errNotYet
}

func (s *session) SetImages(ctx context.Context, ref ObjectRef, images map[string]string) error {
	if err := s.begin(ctx, actSetImages, ref); err != nil {
		return err
	}
	return errNotYet
}

func (s *session) Rollback(ctx context.Context, replicaSet ObjectRef) error {
	if err := s.begin(ctx, actRollback, replicaSet); err != nil {
		return err
	}
	return errNotYet
}

func (s *session) Trigger(ctx context.Context, cronJob ObjectRef) (string, error) {
	if err := s.begin(ctx, actTrigger, cronJob); err != nil {
		return "", err
	}
	return "", errNotYet
}

func (s *session) SetSuspended(ctx context.Context, cronJob ObjectRef, suspended bool) error {
	action := actResume
	if suspended {
		action = actSuspend
	}
	if err := s.begin(ctx, action, cronJob); err != nil {
		return err
	}
	return errNotYet
}
