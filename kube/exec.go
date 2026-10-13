package kube

import (
	"context"
	"errors"

	// The exec stream of the API, and the errors of its upgrade. Named
	// here so that go.mod holds what Exec is built on.
	_ "k8s.io/client-go/tools/remotecommand"
	_ "k8s.io/streaming/pkg/httpstream"
)

// Exec is not built yet: it begins as every action does, and then says
// that it is not.
func (s *session) Exec(ctx context.Context, pod ObjectRef, opts ExecOptions) (ExecResult, error) {
	_, release, err := s.begin(ctx, actExec, pod)
	if err != nil {
		return ExecResult{}, err
	}
	defer release()
	return ExecResult{}, errors.New("not implemented")
}
