package kube

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	authorizationv1 "k8s.io/api/authorization/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// canFor is how long Can keeps an answer. A reading of the key list that
// changes a key's role ends its sessions; a change of the role's own
// rules is seen within canFor.
const canFor = 2 * time.Minute

// canQuestion is what Can asks the API: verb on group/resource, or on
// its subresource, in namespace ("" for the cluster scope or every
// namespace).
type canQuestion struct {
	verb, group, resource, subresource, namespace string
}

// canAnswer is the API's answer to a question, and until when it is
// kept.
type canAnswer struct {
	allowed bool
	until   time.Time
}

// canCache is a session's answers of Can. Its zero value is ready; it
// ends with its session, which holds it.
type canCache struct {
	// now is the clock of the answers' lifetimes: time.Now when nil.
	// Tests set it.
	now func() time.Time

	mu      sync.Mutex
	answers map[canQuestion]canAnswer
	// warned says that an error has been logged: one line a session.
	warned bool
}

func (c *canCache) clock() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

// get returns the answer to q while it is kept.
func (c *canCache) get(q canQuestion) (allowed, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	a, ok := c.answers[q]
	if !ok || !c.clock().Before(a.until) {
		return false, false
	}
	return a.allowed, true
}

// put keeps the answer to q for canFor.
func (c *canCache) put(q canQuestion, allowed bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.answers == nil {
		c.answers = map[canQuestion]canAnswer{}
	}
	c.answers[q] = canAnswer{allowed: allowed, until: c.clock().Add(canFor)}
}

// warnOnce reports whether no error has been logged yet, and from then
// on that one has.
func (c *canCache) warnOnce() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	first := !c.warned
	c.warned = true
	return first
}

// Can asks the API server, as the key, whether the key's role allows
// verb on res, or on its subresource, in namespace: a
// SelfSubjectAccessReview made with the session's own clients, so that
// the answer is the role's. An empty namespace asks for the cluster
// scope, which for a namespaced resource is the right in every
// namespace. The answer is kept for canFor. An error is answered as
// false and not kept, and the first of a session is logged.
func (s *session) Can(ctx context.Context, verb string, res Resource, subresource, namespace string) bool {
	q := canQuestion{verb: verb, group: res.GVR.Group, resource: res.GVR.Resource, subresource: subresource, namespace: namespace}
	if allowed, ok := s.can.get(q); ok {
		return allowed
	}
	allowed, err := s.ask(ctx, q)
	if err != nil {
		// A question whose asker cancelled it, or whose session has
		// ended, says nothing of the key's rights. One whose asker's
		// deadline passed is an API that did not answer in time, which
		// the operator is to see.
		if !errors.Is(ctx.Err(), context.Canceled) && s.ctx.Err() == nil && s.can.warnOnce() {
			s.c.log.LogAttrs(ctx, slog.LevelWarn, "the key's rights could not be checked",
				slog.String("key", s.grant.Name),
				slog.String("fingerprint", s.grant.Fingerprint),
				slog.String("role", s.grant.Role.String()),
				slog.String("err", canError(err).Error()))
		}
		return false
	}
	s.can.put(q, allowed)
	return allowed
}

// ask sends q to the API as a SelfSubjectAccessReview of the session's
// key.
func (s *session) ask(ctx context.Context, q canQuestion) (bool, error) {
	api, err := s.api()
	if err != nil {
		return false, err
	}
	ctx, release := s.bound(ctx)
	defer release()
	review := &authorizationv1.SelfSubjectAccessReview{Spec: authorizationv1.SelfSubjectAccessReviewSpec{
		ResourceAttributes: &authorizationv1.ResourceAttributes{
			Verb:        q.verb,
			Group:       q.group,
			Resource:    q.resource,
			Subresource: q.subresource,
			Namespace:   q.namespace,
		},
	}}
	got, err := api.typed.AuthorizationV1().SelfSubjectAccessReviews().Create(ctx, review, metav1.CreateOptions{})
	if err != nil {
		return false, err
	}
	return got.Status.Allowed, nil
}

// canError is what the log says of an error of Can. An error of the API
// is reduced to its reason and code, as an action's is: its text is the
// API's, or an admission webhook's, which kosh does not log. Any other
// error keeps its text: it is kosh's or client-go's own (the API could
// not be reached, the session has no clients), holds nothing of an
// object, and is what the operator needs to see why.
func canError(err error) error {
	var api apierrors.APIStatus
	if errors.As(err, &api) {
		return loggableError("can", err)
	}
	return err
}
