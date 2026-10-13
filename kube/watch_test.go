package kube

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	k8stesting "k8s.io/client-go/testing"

	"github.com/fables-for-robots/kosh/render"
)

// watchOf opens a watch and closes it when the test ends.
func watchOf(t *testing.T, s *session, q Query) Watch {
	t.Helper()
	w, err := s.Watch(q)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	t.Cleanup(w.Close)
	return w
}

// rowsWhen waits until the table has rows for exactly want.
func rowsWhen(t *testing.T, w Watch, want ...string) Table {
	t.Helper()
	var tab Table
	eventually(t, "rows "+join(want), func() bool {
		tab = w.Table(time.Now())
		got := names(tab.Rows)
		if len(got) != len(want) {
			return false
		}
		for _, n := range want {
			if !got[n] {
				return false
			}
		}
		return true
	})
	return tab
}

func join(ss []string) string {
	out := ""
	for i, s := range ss {
		if i > 0 {
			out += ","
		}
		out += s
	}
	return "[" + out + "]"
}

func TestWatchShowsObjectsAndFollowsChanges(t *testing.T) {
	e := newEnv(t, envConfig{}, pod("default", "a", nil), pod("default", "b", nil))
	s := e.session(t)
	pods := e.resource(t, "", "pods")
	w := watchOf(t, s, Query{Resource: pods})

	tab := rowsWhen(t, w, "default/a", "default/b")
	if tab.Stale || tab.Err != nil || !tab.Namespaced {
		t.Errorf("table: stale %v, err %v, namespaced %v", tab.Stale, tab.Err, tab.Namespaced)
	}
	if len(tab.Columns) != 3 || tab.Columns[0].Name != "NAMESPACE" || tab.Columns[2].Name != "AGE" {
		t.Errorf("columns %+v, want the renderer's", tab.Columns)
	}

	drain(w.Changed())
	ctx := context.Background()
	if _, err := e.dyn.Resource(render.Pods).Namespace("default").Create(ctx, toUnstructured(t, pod("default", "c", nil)), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	waitChanged(t, w)
	rowsWhen(t, w, "default/a", "default/b", "default/c")

	drain(w.Changed())
	if err := e.dyn.Resource(render.Pods).Namespace("default").Delete(ctx, "a", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	waitChanged(t, w)
	rowsWhen(t, w, "default/b", "default/c")
}

func TestWatchLeavesOutWhatDoesNotRender(t *testing.T) {
	bad := pod("default", "bad", map[string]string{failRender: "yes"})
	e := newEnv(t, envConfig{}, pod("default", "good", nil), bad)
	w := watchOf(t, e.session(t), Query{Resource: e.resource(t, "", "pods")})
	rowsWhen(t, w, "default/good")
}

func TestWatchBeforeTheFirstListIsStale(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	open := func() { once.Do(func() { close(release) }) }
	e := newEnv(t, envConfig{before: func(e *env) {
		e.dyn.PrependReactor("list", "pods", func(k8stesting.Action) (bool, k8sruntime.Object, error) {
			<-release
			return false, nil, nil
		})
	}}, pod("default", "a", nil))
	t.Cleanup(open)

	w := watchOf(t, e.session(t), Query{Resource: e.resource(t, "", "pods")})
	tab := w.Table(time.Now())
	if len(tab.Rows) != 0 || !tab.Stale || tab.Err != nil {
		t.Fatalf("before the first list: %d rows, stale %v, err %v", len(tab.Rows), tab.Stale, tab.Err)
	}
	if len(tab.Columns) != 3 {
		t.Errorf("columns %+v", tab.Columns)
	}
	drain(w.Changed())
	open()
	waitChanged(t, w)
	tab = rowsWhen(t, w, "default/a")
	if tab.Stale {
		t.Error("stale after the first list")
	}
}

func TestWatchFiltersByNamespace(t *testing.T) {
	e := newEnv(t, envConfig{}, pod("one", "a", nil), pod("one", "b", nil), pod("two", "c", nil))
	s := e.session(t)
	pods := e.resource(t, "", "pods")
	rowsWhen(t, watchOf(t, s, Query{Resource: pods, Namespace: "one"}), "one/a", "one/b")
	rowsWhen(t, watchOf(t, s, Query{Resource: pods, Namespace: "two"}), "two/c")
	rowsWhen(t, watchOf(t, s, Query{Resource: pods}), "one/a", "one/b", "two/c")
}

// A namespace for a resource that has none is refused; a resource that
// says it has none when discovery found one is no resource of the
// cluster.
func TestWatchRefusesTheNamespaceOfAClusterResource(t *testing.T) {
	e := newEnv(t, envConfig{}, pod("one", "a", nil))
	s := e.session(t)
	if _, err := s.Watch(Query{Resource: e.resource(t, "", "nodes"), Namespace: "two"}); !errors.Is(err, ErrInvalidNamespace) {
		t.Errorf("nodes in two: got %v, want ErrInvalidNamespace", err)
	}
	pods := e.resource(t, "", "pods")
	pods.Namespaced = false
	if _, err := s.Watch(Query{Resource: pods, Namespace: "two"}); !errors.Is(err, ErrUnknownResource) {
		t.Errorf("pods made cluster-wide: got %v, want ErrUnknownResource", err)
	}
}

func TestWatchListErrorIsInTheTable(t *testing.T) {
	forbidden := apierrors.NewForbidden(podsResource, "", errors.New("no"))
	e := newEnv(t, envConfig{before: func(e *env) {
		e.dyn.PrependReactor("list", "pods", func(k8stesting.Action) (bool, k8sruntime.Object, error) {
			return true, nil, forbidden
		})
	}}, pod("default", "a", nil))
	w := watchOf(t, e.session(t), Query{Resource: e.resource(t, "", "pods")})
	var tab Table
	eventually(t, "the list's error", func() bool {
		tab = w.Table(time.Now())
		return tab.Err != nil
	})
	if !apierrors.IsForbidden(tab.Err) {
		t.Errorf("Err %v, want forbidden", tab.Err)
	}
	if len(tab.Rows) != 0 || len(tab.Columns) != 0 {
		t.Errorf("%d rows and %d columns with Err", len(tab.Rows), len(tab.Columns))
	}
}

func TestWatchStaleWhenTheWatchBreaks(t *testing.T) {
	e := newEnv(t, envConfig{before: func(e *env) {
		e.dyn.PrependWatchReactor("pods", func(k8stesting.Action) (bool, watch.Interface, error) {
			return true, nil, apierrors.NewTooManyRequests("the API is busy", 1)
		})
	}}, pod("default", "a", nil))
	w := watchOf(t, e.session(t), Query{Resource: e.resource(t, "", "pods")})
	eventually(t, "stale with the last rows", func() bool {
		tab := w.Table(time.Now())
		return tab.Stale && len(tab.Rows) == 1 && tab.Err == nil
	})
}

func TestWatchKeepsItsRowsWhenARelistFails(t *testing.T) {
	var fw atomic.Pointer[watch.FakeWatcher]
	var lists atomic.Int32
	forbidden := apierrors.NewForbidden(podsResource, "", errors.New("no"))
	e := newEnv(t, envConfig{before: func(e *env) {
		e.dyn.PrependWatchReactor("pods", func(k8stesting.Action) (bool, watch.Interface, error) {
			if fw.Load() != nil {
				return true, nil, apierrors.NewTooManyRequests("the API is busy", 1)
			}
			w := watch.NewFakeWithChanSize(1, false)
			fw.Store(w)
			return true, w, nil
		})
		e.dyn.PrependReactor("list", "pods", func(k8stesting.Action) (bool, k8sruntime.Object, error) {
			if lists.Add(1) > 1 {
				return true, nil, forbidden
			}
			return false, nil, nil
		})
	}}, pod("default", "a", nil))
	w := watchOf(t, e.session(t), Query{Resource: e.resource(t, "", "pods")})
	rowsWhen(t, w, "default/a")
	eventually(t, "the watch", func() bool { return fw.Load() != nil })

	// The watch ends with an error, the relist fails.
	fw.Load().Error(&metav1.Status{Status: metav1.StatusFailure, Code: 500, Reason: metav1.StatusReasonInternalError, Message: "broken"})
	eventually(t, "the relist", func() bool { return lists.Load() > 1 })
	var tab Table
	eventually(t, "the failed relist in the table", func() bool {
		tab = w.Table(time.Now())
		return tab.Stale
	})
	if tab.Err != nil || len(tab.Rows) != 1 || len(tab.Columns) != 3 {
		t.Errorf("after a failed relist: err %v, %d rows, %d columns; want the last rows, stale", tab.Err, len(tab.Rows), len(tab.Columns))
	}
}

func TestObjectsAreTypedAndWithoutManagedFields(t *testing.T) {
	p := pod("default", "web", nil)
	p.ManagedFields = []metav1.ManagedFieldsEntry{{Manager: "kubectl", Operation: metav1.ManagedFieldsOperationApply}}
	e := newEnv(t, envConfig{}, p)
	rowsWhen(t, watchOf(t, e.session(t), Query{Resource: e.resource(t, "", "pods")}), "default/web")
	given := e.renderers.pods.given()
	if len(given) == 0 {
		t.Fatal("the renderer was given nothing")
	}
	for _, o := range given {
		p, ok := o.(*corev1.Pod)
		if !ok {
			t.Fatalf("the renderer was given %T", o)
		}
		if p.ManagedFields != nil {
			t.Errorf("managedFields are there: %v", p.ManagedFields)
		}
	}
}

func TestInformerIsShared(t *testing.T) {
	e := newEnv(t, envConfig{}, pod("default", "a", nil))
	lists := countLists(e, "pods")
	pods := e.resource(t, "", "pods")
	rowsWhen(t, watchOf(t, e.session(t), Query{Resource: pods}), "default/a")
	rowsWhen(t, watchOf(t, e.session(t), Query{Resource: pods, Namespace: "default"}), "default/a")
	if n := lists.Load(); n != 1 {
		t.Errorf("%d lists for two watches, want 1", n)
	}
}

func TestInformerStopsWhenIdle(t *testing.T) {
	e := newEnv(t, envConfig{opts: Options{IdleStop: 20 * time.Millisecond}}, pod("default", "a", nil))
	lists := countLists(e, "pods")
	s := e.session(t)
	pods := e.resource(t, "", "pods")

	a, b := watchOf(t, s, Query{Resource: pods}), watchOf(t, s, Query{Resource: pods})
	rowsWhen(t, a, "default/a")
	a.Close()
	b.Close()
	b.Close()
	// Reopened at once: the informer is still there.
	c := watchOf(t, s, Query{Resource: pods})
	rowsWhen(t, c, "default/a")
	if n := lists.Load(); n != 1 {
		t.Fatalf("%d lists for a watch reopened before IdleStop, want 1", n)
	}
	c.Close()

	eventually(t, "the informer stops", func() bool { return !e.c.running(render.Pods) })
	rowsWhen(t, watchOf(t, s, Query{Resource: pods}), "default/a")
	if n := lists.Load(); n != 2 {
		t.Errorf("%d lists, want 2: one more after the informer stopped", n)
	}
}

func TestWatchClosesWithTheSession(t *testing.T) {
	e := newEnv(t, envConfig{opts: Options{IdleStop: 20 * time.Millisecond}}, pod("default", "a", nil))
	ctx, cancel := context.WithCancel(context.Background())
	s := e.c.Session(ctx, fullGrant).(*session)
	w, err := s.Watch(Query{Resource: e.resource(t, "", "pods")})
	if err != nil {
		t.Fatal(err)
	}
	rowsWhen(t, w, "default/a")
	cancel()
	eventually(t, "the informer stops", func() bool { return !e.c.running(render.Pods) })
	w.Close() // after the session's end, too
}

func TestWatchOfAClosedCluster(t *testing.T) {
	e := newEnv(t, envConfig{})
	e.c.Close()
	if _, err := e.session(t).Watch(Query{Resource: e.resource(t, "", "pods")}); err == nil {
		t.Error("a watch of a closed Cluster")
	}
}

func TestWatchChangedDoesNotBlock(t *testing.T) {
	e := newEnv(t, envConfig{})
	w := watchOf(t, e.session(t), Query{Resource: e.resource(t, "", "pods")})
	ctx := context.Background()
	for _, n := range []string{"a", "b", "c"} {
		if _, err := e.dyn.Resource(render.Pods).Namespace("default").Create(ctx, toUnstructured(t, pod("default", n, nil)), metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	rowsWhen(t, w, "default/a", "default/b", "default/c")
	if len(w.Changed()) > 1 {
		t.Errorf("%d signals waiting", len(w.Changed()))
	}
}

func TestGrantChecks(t *testing.T) {
	e := newEnv(t, envConfig{})
	secret := ObjectRef{Resource: e.resource(t, "", "secrets"), Namespace: "default", Name: "s"}
	configMap := ObjectRef{Resource: e.resource(t, "", "configmaps"), Namespace: "default", Name: "c"}

	full := e.c.Session(context.Background(), fullGrant).(*session)
	ro := e.c.Session(context.Background(), readOnlyGrant).(*session)
	if full.Grant() != fullGrant || ro.Grant() != readOnlyGrant {
		t.Error("Grant is not the session's")
	}
	if err := full.mayChange(); err != nil {
		t.Errorf("full mayChange: %v", err)
	}
	if err := ro.mayChange(); !errors.Is(err, ErrReadOnly) {
		t.Errorf("read-only mayChange: %v", err)
	}
	for _, ref := range []ObjectRef{secret, configMap} {
		if err := full.maySee(ref); err != nil {
			t.Errorf("full maySee %s: %v", ref, err)
		}
	}
	if err := ro.maySee(secret); !errors.Is(err, ErrReadOnly) {
		t.Errorf("read-only maySee of a Secret: %v", err)
	}
	if err := ro.maySee(configMap); err != nil {
		t.Errorf("read-only maySee of a ConfigMap: %v", err)
	}
}
