package kube

import "context"

func (s *session) Cordon(ctx context.Context, node ObjectRef, on bool) error {
	action := actUncordon
	if on {
		action = actCordon
	}
	if err := s.begin(ctx, action, node); err != nil {
		return err
	}
	return errNotYet
}

func (s *session) Drain(ctx context.Context, node ObjectRef, opts DrainOptions) error {
	if err := s.begin(ctx, actDrain, node); err != nil {
		return err
	}
	return errNotYet
}
