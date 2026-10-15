package kube

import (
	"context"
	"errors"
	"log/slog"
	"reflect"
	"sync"
	"testing"
	"time"

	authorizationv1 "k8s.io/api/authorization/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	k8stesting "k8s.io/client-go/testing"

	"github.com/fables-for-robots/kosh/keys"
)

// review is one SelfSubjectAccessReview the API was asked: the user
// whose clients it came through, and what it asked.
type review struct {
	user  string
	attrs authorizationv1.ResourceAttributes
}

// reviewer answers the SelfSubjectAccessReviews of every identity's
// typed client: each identity is given a fake of its own, whose reactor
// records the review with the identity's user and answers it from
// allowed, or with err when it is set.
type reviewer struct {
	mu      sync.Mutex
	allowed map[authorizationv1.ResourceAttributes]bool
	// byUser are the answers to one identity alone, by its user.
	byUser map[string]map[authorizationv1.ResourceAttributes]bool
	err    error
	asked  []review
}

// reviewEnv returns an env whose identities' typed clients are the
// reviewer's.
func reviewEnv(t *testing.T, opts Options) (*env, *reviewer) {
	t.Helper()
	e := newEnv(t, envConfig{opts: opts})
	r := &reviewer{allowed: map[authorizationv1.ResourceAttributes]bool{}}
	e.c.newClients = func(cfg *rest.Config) (*clients, error) {
		cl, err := e.newClients(cfg)
		if err != nil {
			return nil, err
		}
		user := cfg.Impersonate.UserName
		typed := fake.NewSimpleClientset()
		typed.PrependReactor("create", "selfsubjectaccessreviews", func(a k8stesting.Action) (bool, runtime.Object, error) {
			ssar := a.(k8stesting.CreateAction).GetObject().(*authorizationv1.SelfSubjectAccessReview)
			attrs := *ssar.Spec.ResourceAttributes
			r.mu.Lock()
			defer r.mu.Unlock()
			r.asked = append(r.asked, review{user: user, attrs: attrs})
			if r.err != nil {
				return true, nil, r.err
			}
			out := ssar.DeepCopy()
			out.Status.Allowed = r.allowed[attrs] || r.byUser[user][attrs]
			return true, out, nil
		})
		own := *cl
		own.typed = typed
		return &own, nil
	}
	return e, r
}

func (r *reviewer) allow(attrs authorizationv1.ResourceAttributes) {
	r.mu.Lock()
	r.allowed[attrs] = true
	r.mu.Unlock()
}

// allowFor answers yes to attrs asked by the identity of user alone.
func (r *reviewer) allowFor(user string, attrs authorizationv1.ResourceAttributes) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.byUser == nil {
		r.byUser = map[string]map[authorizationv1.ResourceAttributes]bool{}
	}
	if r.byUser[user] == nil {
		r.byUser[user] = map[authorizationv1.ResourceAttributes]bool{}
	}
	r.byUser[user][attrs] = true
}

func (r *reviewer) setErr(err error) {
	r.mu.Lock()
	r.err = err
	r.mu.Unlock()
}

func (r *reviewer) reviews() []review {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]review(nil), r.asked...)
}

func TestCanAsksAsTheKey(t *testing.T) {
	e, r := reviewEnv(t, Options{})
	s := e.sessionOf(t, viewGrant)
	pods := e.resource(t, "", "pods")
	deployments := e.resource(t, "apps", "deployments")
	r.allow(authorizationv1.ResourceAttributes{Verb: "list", Resource: "pods", Namespace: "default"})
	r.allow(authorizationv1.ResourceAttributes{Verb: "get", Group: "apps", Resource: "deployments", Subresource: "scale", Namespace: "default"})

	ctx := context.Background()
	if !s.Can(ctx, "list", pods, "", "default") {
		t.Error("list pods in default: not allowed")
	}
	if s.Can(ctx, "delete", pods, "", "default") {
		t.Error("delete pods in default: allowed")
	}
	if !s.Can(ctx, "get", deployments, "scale", "default") {
		t.Error("get deployments/scale in default: not allowed")
	}
	want := []authorizationv1.ResourceAttributes{
		{Verb: "list", Resource: "pods", Namespace: "default"},
		{Verb: "delete", Resource: "pods", Namespace: "default"},
		{Verb: "get", Group: "apps", Resource: "deployments", Subresource: "scale", Namespace: "default"},
	}
	got := r.reviews()
	if len(got) != len(want) {
		t.Fatalf("reviews %+v, want %+v", got, want)
	}
	for i, rv := range got {
		if rv.attrs != want[i] {
			t.Errorf("review %d asked %+v, want %+v", i, rv.attrs, want[i])
		}
		if rv.user != "kosh:alice/view" {
			t.Errorf("review %d came through the clients of %q, not the key's", i, rv.user)
		}
	}
	imp, ok := e.madeWith("kosh:alice/view")
	if wantImp := (rest.ImpersonationConfig{UserName: "kosh:alice/view", Groups: []string{"kosh:kosh:clusterrole:view"}}); !ok || !reflect.DeepEqual(imp, wantImp) {
		t.Errorf("the key's clients impersonate %+v, want %+v", imp, wantImp)
	}
}

func TestCanAsksOnceWithinCanFor(t *testing.T) {
	e, r := reviewEnv(t, Options{})
	s := e.sessionOf(t, viewGrant)
	now := time.Now()
	s.can.now = func() time.Time { return now }
	pods := e.resource(t, "", "pods")
	deployments := e.resource(t, "apps", "deployments")
	r.allow(authorizationv1.ResourceAttributes{Verb: "delete", Resource: "pods", Namespace: "default"})

	ctx := context.Background()
	for range 3 {
		if !s.Can(ctx, "delete", pods, "", "default") {
			t.Fatal("delete pods in default: not allowed")
		}
	}
	if n := len(r.reviews()); n != 1 {
		t.Fatalf("%d reviews for one question asked three times, want 1", n)
	}

	// Each part of the question is part of the key: another verb, group,
	// resource, subresource or namespace is asked of the API.
	others := []struct {
		verb string
		res  Resource
		sub  string
		ns   string
	}{
		{"get", pods, "", "default"},
		{"delete", deployments, "", "default"},
		{"delete", e.resource(t, "", "services"), "", "default"},
		{"delete", pods, "eviction", "default"},
		{"delete", pods, "", "kube-system"},
		{"delete", pods, "", ""},
	}
	for _, o := range others {
		before := len(r.reviews())
		s.Can(ctx, o.verb, o.res, o.sub, o.ns)
		s.Can(ctx, o.verb, o.res, o.sub, o.ns)
		if n := len(r.reviews()) - before; n != 1 {
			t.Errorf("%s %s/%s in %q: %d reviews, want 1", o.verb, o.res.GVR.Resource, o.sub, o.ns, n)
		}
	}
	// The group of a resource is part of it: deployments of apps asked
	// above is not deployments of another group.
	other := deployments
	other.GVR.Group = "example.com"
	before := len(r.reviews())
	s.Can(ctx, "delete", other, "", "default")
	if n := len(r.reviews()) - before; n != 1 {
		t.Errorf("deployments of another group: %d reviews, want 1", n)
	}

	// The answers are the session's: another session of the key asks.
	before = len(r.reviews())
	if !e.sessionOf(t, viewGrant).Can(ctx, "delete", pods, "", "default") {
		t.Error("another session: delete pods in default: not allowed")
	}
	if n := len(r.reviews()) - before; n != 1 {
		t.Errorf("another session of the key: %d reviews, want 1", n)
	}

	// Within canFor the answer is kept; after it, the API is asked again.
	before = len(r.reviews())
	now = now.Add(canFor - time.Second)
	s.Can(ctx, "delete", pods, "", "default")
	if n := len(r.reviews()) - before; n != 0 {
		t.Errorf("within canFor: %d reviews, want 0", n)
	}
	now = now.Add(2 * time.Second)
	s.Can(ctx, "delete", pods, "", "default")
	if n := len(r.reviews()) - before; n != 1 {
		t.Errorf("after canFor: %d reviews, want 1", n)
	}
}

func TestCanErrorIsFalseAndLoggedOnce(t *testing.T) {
	log := newAuditLog()
	e, r := reviewEnv(t, Options{Log: log.logger()})
	s := e.sessionOf(t, teamGrant)
	pods := e.resource(t, "", "pods")
	attrs := authorizationv1.ResourceAttributes{Verb: "delete", Resource: "pods", Namespace: testTeamNamespace}
	r.allow(attrs)
	r.setErr(errors.New("the API is down"))

	// A question its asker cancelled says nothing of the key's rights:
	// it is false and not logged, and does not use up the session's line.
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if s.Can(cancelled, "delete", pods, "", testTeamNamespace) {
		t.Error("delete with a cancelled context and the API down: allowed")
	}
	if recs := log.with("the key's rights could not be checked"); len(recs) != 0 {
		t.Fatalf("a cancelled question was logged: %+v", recs)
	}

	ctx := context.Background()
	for _, verb := range []string{"delete", "delete", "get"} {
		if s.Can(ctx, verb, pods, "", testTeamNamespace) {
			t.Errorf("%s with the API down: allowed", verb)
		}
	}
	recs := log.with("the key's rights could not be checked")
	if len(recs) != 1 {
		t.Fatalf("records %+v, want one line", log.all())
	}
	if recs[0].Level != slog.LevelWarn {
		t.Errorf("logged at %v, want warn", recs[0].Level)
	}
	for k, v := range map[string]string{"key": teamGrant.Name, "fingerprint": teamGrant.Fingerprint, "role": "Role/team-a/edit"} {
		if got, _ := recs[0].attr(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	if got, _ := recs[0].attr("err"); got == "" {
		t.Error("the line says no error")
	}
	// An error is not kept: once the API answers, so does Can.
	r.setErr(nil)
	if !s.Can(ctx, "delete", pods, "", testTeamNamespace) {
		t.Error("delete pods in team-a after the API is back: not allowed")
	}
	if n := len(r.reviews()); n != 5 {
		t.Errorf("%d reviews, want 5: an error is asked again", n)
	}
	// Another session logs its own line. Its asker's deadline passed: an
	// API that does not answer in time is logged.
	r.setErr(context.DeadlineExceeded)
	other := keys.Grant{Name: "dave/edit@team-a", Fingerprint: "SHA256:dave", Role: teamGrant.Role}
	expired, cancelExpired := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancelExpired()
	if e.sessionOf(t, other).Can(expired, "delete", pods, "", testTeamNamespace) {
		t.Error("delete past the deadline: allowed")
	}
	if n := len(log.with("the key's rights could not be checked")); n != 2 {
		t.Errorf("%d lines after a second session's error past its deadline, want 2", n)
	}
}

func TestCanClusterWideWhenNamespaceEmpty(t *testing.T) {
	e, r := reviewEnv(t, Options{})
	s := e.sessionOf(t, teamGrant)
	pods := e.resource(t, "", "pods")
	r.allow(authorizationv1.ResourceAttributes{Verb: "list", Resource: "pods", Namespace: testTeamNamespace})

	ctx := context.Background()
	if !s.Can(ctx, "list", pods, "", testTeamNamespace) {
		t.Error("list pods in team-a: not allowed")
	}
	if s.Can(ctx, "list", pods, "", "") {
		t.Error("list pods in every namespace: allowed by the right of one")
	}
	got := r.reviews()
	if len(got) != 2 {
		t.Fatalf("reviews %+v, want 2", got)
	}
	if want := (authorizationv1.ResourceAttributes{Verb: "list", Resource: "pods"}); got[1].attrs != want {
		t.Errorf("every namespace asked %+v, want %+v: the cluster-wide right", got[1].attrs, want)
	}
}
