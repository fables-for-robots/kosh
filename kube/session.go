package kube

import (
	"context"

	"github.com/fables-for-robots/kosh/keys"
)

// session is the Session of one SSH session: the Cluster, the session's
// context, which ends every watch it opened, its grant, and what it acts
// as. Every request it makes is made with clients, its key's own; the
// tables of the typed kinds come from the caches of its role, which
// act as the role.
type session struct {
	c     *Cluster
	ctx   context.Context
	grant keys.Grant

	self       identity // the key's: "kosh:<key name>" and the role's group
	role       identity // the role's, whose caches and pollers the session shares
	clients    *clients // of self; nil when clientsErr says why
	clientsErr error

	can canCache // the answers of Can, the session's own
}

var _ Session = (*session)(nil)

// api returns the session's clients, or why there are none.
func (s *session) api() (*clients, error) {
	return s.clients, s.clientsErr
}

// cacheKey returns the key of the cache a session reads the objects of
// r from, for rows of namespace ("" for every namespace). The cache is
// its role's. A ClusterRole's caches are of every namespace, as the
// role may list them; a Role's are of namespace alone, since the API
// refuses a Role's list of every namespace. A Role-bound session that
// asks for another namespace gets that namespace's cache, which the API
// refuses too: its table shows the refusal.
func (s *session) cacheKey(r Resource, namespace string) cacheKey {
	k := cacheKey{id: s.role, gvr: r.GVR}
	if s.grant.Role.Kind == keys.KindRole && r.Namespaced {
		k.namespace = namespace
	}
	return k
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
