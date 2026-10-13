package kube

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	k8stesting "k8s.io/client-go/testing"
)

var actionsType = reflect.TypeOf((*Actions)(nil)).Elem()

func TestEveryMethodOfActionsTakesAContextAndReturnsAnError(t *testing.T) {
	ctxType := reflect.TypeOf((*context.Context)(nil)).Elem()
	errType := reflect.TypeOf((*error)(nil)).Elem()
	if actionsType.NumMethod() == 0 {
		t.Fatal("Actions has no methods")
	}
	for i := 0; i < actionsType.NumMethod(); i++ {
		m := actionsType.Method(i)
		if m.Type.NumIn() == 0 || m.Type.In(0) != ctxType {
			t.Errorf("%s: the first parameter is not a context.Context", m.Name)
		}
		if n := m.Type.NumOut(); n == 0 || m.Type.Out(n-1) != errType {
			t.Errorf("%s: the last result is not an error", m.Name)
		}
	}
}

func TestReadOnlyGrantIsRefusedEveryAction(t *testing.T) {
	f := actionEnv(t, readOnlyGrant, nil, pod("default", "web", nil))
	v := reflect.ValueOf(f.s)
	for i := 0; i < actionsType.NumMethod(); i++ {
		m := actionsType.Method(i)
		args := make([]reflect.Value, m.Type.NumIn())
		args[0] = reflect.ValueOf(context.Background())
		for j := 1; j < len(args); j++ {
			args[j] = reflect.Zero(m.Type.In(j))
		}
		f.log.reset()
		out := v.MethodByName(m.Name).Call(args)
		err, _ := out[len(out)-1].Interface().(error)
		if !errors.Is(err, ErrReadOnly) {
			t.Errorf("%s: %v, want ErrReadOnly", m.Name, err)
		}
		if recs, all := f.log.with("refused"), f.log.all(); len(recs) != 1 || len(all) != 1 || recs[0].Level != slog.LevelWarn {
			t.Errorf("%s: records %+v, want one refusal", m.Name, all)
		}
	}
	if a := f.typed.Actions(); len(a) != 0 {
		t.Errorf("the clientset was sent %v", a)
	}
	if a := f.dyn.Actions(); len(a) != 0 {
		t.Errorf("the dynamic client was sent %v", a)
	}
}

func TestRefusalIsLogged(t *testing.T) {
	f := actionEnv(t, readOnlyGrant, nil)
	ref := f.ref(t, "apps", "deployments", "default", "web")
	if err := f.s.Scale(context.Background(), ref, 3); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("Scale: %v", err)
	}
	recs := f.log.all()
	if len(recs) != 1 {
		t.Fatalf("records %+v, want one", recs)
	}
	r := recs[0]
	if r.Level != slog.LevelWarn || r.Message != "refused" {
		t.Errorf("level %v, message %q", r.Level, r.Message)
	}
	want := map[string]string{
		"key":         readOnlyGrant.Name,
		"fingerprint": readOnlyGrant.Fingerprint,
		"action":      "scale",
		"object":      "deployments/default/web",
	}
	for k, v := range want {
		if got, ok := r.attr(k); !ok || got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
}

// endedSession returns a session of f's Cluster with a full grant, and
// the function that ends it, as sshd does when the key is removed.
func endedSession(t *testing.T, f *actionFixture) (*session, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return f.c.Session(ctx, fullGrant).(*session), cancel
}

func TestActionsEndWithTheSession(t *testing.T) {
	t.Run("an ended session is refused every action", func(t *testing.T) {
		f := actionEnv(t, fullGrant, nil, pod("default", "web", nil))
		s, end := endedSession(t, f)
		end()
		v := reflect.ValueOf(s)
		for i := 0; i < actionsType.NumMethod(); i++ {
			m := actionsType.Method(i)
			args := make([]reflect.Value, m.Type.NumIn())
			args[0] = reflect.ValueOf(context.Background())
			for j := 1; j < len(args); j++ {
				args[j] = reflect.Zero(m.Type.In(j))
			}
			f.log.reset()
			out := v.MethodByName(m.Name).Call(args)
			err, _ := out[len(out)-1].Interface().(error)
			if err == nil || !strings.Contains(err.Error(), "the session has ended") {
				t.Errorf("%s: %v, want that the session has ended", m.Name, err)
			}
			recs, all := f.log.with("refused"), f.log.all()
			if len(recs) != 1 || len(all) != 1 || recs[0].Level != slog.LevelWarn {
				t.Errorf("%s: records %+v, want one refusal", m.Name, all)
				continue
			}
			if r, _ := recs[0].attr("reason"); r != "the session has ended" {
				t.Errorf("%s: reason %q", m.Name, r)
			}
			if k, _ := recs[0].attr("fingerprint"); k != fullGrant.Fingerprint {
				t.Errorf("%s: fingerprint %q", m.Name, k)
			}
		}
		if a := f.sent(); len(a) != 0 {
			t.Errorf("the clients were sent %v", a)
		}
	})

	t.Run("a drain in flight", func(t *testing.T) {
		started, release := make(chan struct{}, 1), make(chan struct{})
		t.Cleanup(func() { close(release) })
		evictions := func(e *env) {
			rr := standardResources()
			rr[0].APIResources = append(rr[0].APIResources, metav1.APIResource{Name: "pods/eviction", Namespaced: true, Group: "policy", Version: "v1", Kind: "Eviction", Verbs: metav1.Verbs{"create"}})
			e.disc.setResources(rr)
			e.typed.PrependReactor("create", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
				if a.GetSubresource() != "eviction" {
					return false, nil, nil
				}
				select {
				case started <- struct{}{}:
				default:
				}
				<-release // the fake takes no context: it holds the eviction
				return true, nil, apierrors.NewTooManyRequests("held", 0)
			})
		}
		f := actionEnv(t, fullGrant, evictions, actionDrainObjects()[:3]...)
		s, end := endedSession(t, f)
		opts := DefaultDrainOptions()
		opts.IgnoreDaemonSets = true
		opts.Timeout = 0 // as long as it takes
		done := make(chan error, 1)
		go func() { done <- s.Drain(context.Background(), f.ref(t, "", "nodes", "", "n1"), opts) }()
		select {
		case <-started:
		case err := <-done:
			t.Fatalf("the drain returned %v before it evicted", err)
		case <-time.After(5 * time.Second):
			t.Fatal("the drain has not evicted")
		}
		end()
		select {
		case err := <-done:
			if err == nil {
				t.Error("the drain returned no error")
			}
		case <-time.After(time.Second):
			t.Fatal("the drain has not returned a second after the session ended")
		}
		recs := f.log.with("action")
		if len(recs) != 1 {
			t.Fatalf("records %+v, want one action", f.log.all())
		}
		if e, ok := recs[0].attr("error"); !ok || !strings.Contains(e, "the session has ended") {
			t.Errorf("the action's error is %q", e)
		}
	})

	t.Run("a delete waiting on the API", func(t *testing.T) {
		started, stop := make(chan struct{}, 1), make(chan struct{})
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			select {
			case started <- struct{}{}:
			default:
			}
			// Holds the request until the client gives up.
			select {
			case <-r.Context().Done():
			case <-stop:
			}
		}))
		t.Cleanup(srv.Close)
		t.Cleanup(func() { close(stop) })
		dyn, err := dynamic.NewForConfig(&rest.Config{Host: srv.URL})
		if err != nil {
			t.Fatal(err)
		}
		e := newEnv(t, envConfig{})
		log := newAuditLog()
		c := newCluster(nil, e.typed, dyn, nil, e.c.resourceList(), Options{Log: log.logger()})
		t.Cleanup(c.Close)
		ctx, end := context.WithCancel(context.Background())
		t.Cleanup(end)
		s := c.Session(ctx, fullGrant).(*session)
		ref := ObjectRef{Resource: e.resource(t, "", "configmaps"), Namespace: "default", Name: "settings"}
		done := make(chan error, 1)
		go func() { done <- s.Delete(context.Background(), ref, DeleteOptions{}) }()
		select {
		case <-started:
		case err := <-done:
			t.Fatalf("the delete returned %v before it was sent", err)
		case <-time.After(5 * time.Second):
			t.Fatal("the delete was not sent")
		}
		end()
		select {
		case err := <-done:
			if err == nil {
				t.Error("the delete returned no error")
			}
		case <-time.After(time.Second):
			t.Fatal("the delete has not returned a second after the session ended")
		}
		recs := log.with("action")
		if len(recs) != 1 {
			t.Fatalf("records %+v, want one action", log.all())
		}
		if _, ok := recs[0].attr("error"); !ok {
			t.Errorf("the action's line has no error: %+v", recs[0])
		}
	})
}

// heldSession is a session of a Cluster whose clients reach an API
// server that holds every request until the client gives up, and the
// function that ends the session.
func heldSession(t *testing.T) (s *session, end context.CancelFunc, started <-chan struct{}, e *env) {
	t.Helper()
	sent, stop := make(chan struct{}, 1), make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case sent <- struct{}{}:
		default:
		}
		select {
		case <-r.Context().Done():
		case <-stop:
		}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(stop) })
	cfg := &rest.Config{Host: srv.URL}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	e = newEnv(t, envConfig{})
	c := newCluster(cfg, e.typed, dyn, nil, e.c.resourceList(), Options{Log: newAuditLog().logger()})
	t.Cleanup(c.Close)
	ctx, end := context.WithCancel(context.Background())
	t.Cleanup(end)
	return c.Session(ctx, fullGrant).(*session), end, sent, e
}

// What a session reads stops when the session ends, even when the caller
// gave it no deadline.
func TestReadsEndWithTheSession(t *testing.T) {
	for name, read := range map[string]func(s *session, ref ObjectRef) error{
		"YAML":     func(s *session, ref ObjectRef) error { _, err := s.YAML(context.Background(), ref, false); return err },
		"Describe": func(s *session, ref ObjectRef) error { _, err := s.Describe(context.Background(), ref); return err },
		"Owner":    func(s *session, ref ObjectRef) error { _, err := s.Owner(context.Background(), ref); return err },
	} {
		t.Run(name, func(t *testing.T) {
			s, end, started, e := heldSession(t)
			ref := ObjectRef{Resource: e.resource(t, "", "configmaps"), Namespace: "default", Name: "settings"}
			done := make(chan error, 1)
			go func() { done <- read(s, ref) }()
			select {
			case <-started:
			case err := <-done:
				t.Fatalf("returned %v before it was sent", err)
			case <-time.After(5 * time.Second):
				t.Fatal("nothing was sent")
			}
			end()
			select {
			case err := <-done:
				if err == nil {
					t.Error("no error")
				}
			case <-time.After(time.Second):
				t.Fatal("it has not returned a second after the session ended")
			}
		})
	}
}
