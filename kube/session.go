package kube

import (
	"context"

	"github.com/fables-for-robots/kosh/keys"
)

// session is the Session of one SSH session: the Cluster, the session's
// context, which ends every watch it opened, and its grant.
type session struct {
	c     *Cluster
	ctx   context.Context
	grant keys.Grant
}

var _ Session = (*session)(nil)

// mayChange returns ErrReadOnly for a read-only grant.
func (s *session) mayChange() error {
	if s.grant.ReadOnly {
		return ErrReadOnly
	}
	return nil
}

// maySee returns ErrReadOnly for a read-only grant and a Secret: a
// reference that is one by its group and resource, or by its group and
// kind. A read asks mayRead, which logs the refusal, not maySee.
func (s *session) maySee(ref ObjectRef) error {
	secret := ref.Resource.Is("", "secrets") || ref.Resource.GVR.Group == "" && ref.Resource.Kind == "Secret"
	if s.grant.ReadOnly && secret {
		return ErrReadOnly
	}
	return nil
}

// bound returns a context that ends when ctx ends or when the session
// does, with errSessionEnded as its cause then, and the function that
// releases it, which the caller defers.
func (s *session) bound(ctx context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancelCause(ctx)
	stop := context.AfterFunc(s.ctx, func() { cancel(errSessionEnded) })
	return ctx, func() {
		stop()
		cancel(nil)
	}
}

func (s *session) Grant() keys.Grant { return s.grant }

func (s *session) ClusterVersion() string { return s.c.version }

func (s *session) Resources() []Resource { return s.c.resourceList() }

func (s *session) Resolve(ctx context.Context, name string) (Resource, error) {
	return s.c.resolve(ctx, name)
}
