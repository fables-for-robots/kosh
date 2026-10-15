package kube

import (
	"context"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	k8stesting "k8s.io/client-go/testing"

	"github.com/fables-for-robots/kosh/render"
)

var podsResource = schema.GroupResource{Resource: "pods"}

// countLists counts the lists of a resource the fake dynamic client is
// asked for.
func countLists(e *env, resource string) *atomic.Int32 {
	var n atomic.Int32
	e.dyn.PrependReactor("list", resource, func(k8stesting.Action) (bool, k8sruntime.Object, error) {
		n.Add(1)
		return false, nil, nil
	})
	return &n
}

// acquire acquires the cache of gvr and releases it when the test ends.
func acquire(t *testing.T, e *env, gvr schema.GroupVersionResource) *kindCache {
	t.Helper()
	k, err := e.c.acquire(fullKey(gvr))
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func synced(k *kindCache) bool {
	s, _, _ := k.status()
	return s
}

func podNames(k *kindCache) map[string]bool {
	out := map[string]bool{}
	for _, o := range k.list() {
		p := o.(*corev1.Pod)
		out[p.Namespace+"/"+p.Name] = true
	}
	return out
}

func TestKindCacheHoldsTypedObjectsWithoutManagedFields(t *testing.T) {
	p := pod("default", "web", nil)
	p.ManagedFields = []metav1.ManagedFieldsEntry{{Manager: "kubectl", Operation: metav1.ManagedFieldsOperationApply}}
	e := newEnv(t, envConfig{}, p)
	k := acquire(t, e, render.Pods)
	eventually(t, "the first list", func() bool { return synced(k) })

	objs := k.list()
	if len(objs) != 1 {
		t.Fatalf("%d objects, want 1", len(objs))
	}
	got, ok := objs[0].(*corev1.Pod)
	if !ok {
		t.Fatalf("list gives %T, want *corev1.Pod", objs[0])
	}
	if got.ManagedFields != nil {
		t.Errorf("managedFields are kept: %v", got.ManagedFields)
	}
	o, ok := k.get("default", "web")
	if !ok {
		t.Fatal("get does not find default/web")
	}
	if _, ok := o.(*corev1.Pod); !ok {
		t.Errorf("get gives %T", o)
	}
	if _, ok := k.get("other", "web"); ok {
		t.Error("get finds other/web")
	}
}

func TestKindCacheDropsWhatDoesNotConvert(t *testing.T) {
	e := newEnv(t, envConfig{}, pod("default", "good", nil))
	bad := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "Pod",
		"metadata": map[string]any{"name": "bad", "namespace": "default"},
		"spec":     map[string]any{"containers": "not a list"},
	}}
	if err := e.dyn.Tracker().Add(bad); err != nil {
		t.Fatal(err)
	}
	k := acquire(t, e, render.Pods)
	eventually(t, "the first list", func() bool { return synced(k) })
	if got := podNames(k); len(got) != 1 || !got["default/good"] {
		t.Errorf("got %v, want default/good only", got)
	}
	if _, ok := k.get("default", "bad"); ok {
		t.Error("get finds the pod that does not convert")
	}
}

func TestKindCacheFollowsChangesAndSignals(t *testing.T) {
	e := newEnv(t, envConfig{}, pod("default", "a", nil))
	k := acquire(t, e, render.Pods)
	ch, stop := k.subscribe()
	defer stop()
	eventually(t, "the first list", func() bool { return synced(k) })
	drain(ch)

	ctx := context.Background()
	if _, err := e.dyn.Resource(render.Pods).Namespace("default").Create(ctx, toUnstructured(t, pod("default", "b", nil)), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	waitSignal(t, ch)
	eventually(t, "b is there", func() bool { return podNames(k)["default/b"] })

	drain(ch)
	if err := e.dyn.Resource(render.Pods).Namespace("default").Delete(ctx, "a", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	waitSignal(t, ch)
	eventually(t, "a is gone", func() bool { return !podNames(k)["default/a"] })
}

func TestKindCacheSignalsSyncOfAnEmptyList(t *testing.T) {
	e := newEnv(t, envConfig{})
	k := acquire(t, e, render.Pods)
	ch, stop := k.subscribe()
	defer stop()
	eventually(t, "a signal after which the cache is synced", func() bool {
		select {
		case <-ch:
		case <-time.After(5 * time.Second):
			t.Fatal("no signal")
		}
		return synced(k)
	})
}

func TestKindCacheSignalsDoNotBlock(t *testing.T) {
	e := newEnv(t, envConfig{})
	k := acquire(t, e, render.Pods)
	ch, stop := k.subscribe()
	defer stop()
	eventually(t, "the first list", func() bool { return synced(k) })
	ctx := context.Background()
	for _, name := range []string{"a", "b", "c", "d"} {
		if _, err := e.dyn.Resource(render.Pods).Namespace("default").Create(ctx, toUnstructured(t, pod("default", name, nil)), metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	// Nobody reads ch: the informer goes on all the same.
	eventually(t, "all four are there", func() bool { return len(podNames(k)) == 4 })
	if len(ch) != 1 {
		t.Errorf("%d signals waiting, want 1", len(ch))
	}
}

func TestKindCacheStatus(t *testing.T) {
	t.Run("a list that fails", func(t *testing.T) {
		forbidden := apierrors.NewForbidden(podsResource, "", nil)
		e := newEnv(t, envConfig{before: func(e *env) {
			e.dyn.PrependReactor("list", "pods", func(k8stesting.Action) (bool, k8sruntime.Object, error) {
				return true, nil, forbidden
			})
		}})
		k := acquire(t, e, render.Pods)
		eventually(t, "the list's error", func() bool {
			_, _, err := k.status()
			return apierrors.IsForbidden(err)
		})
	})
	t.Run("a watch that cannot be opened", func(t *testing.T) {
		e := newEnv(t, envConfig{before: func(e *env) {
			e.dyn.PrependWatchReactor("pods", func(k8stesting.Action) (bool, watch.Interface, error) {
				return true, nil, apierrors.NewTooManyRequests("the API is busy", 1)
			})
		}}, pod("default", "a", nil))
		k := acquire(t, e, render.Pods)
		eventually(t, "stale after the first list", func() bool {
			s, stale, err := k.status()
			return s && stale && err == nil
		})
		if !podNames(k)["default/a"] {
			t.Error("the listed pod is not kept")
		}
	})
	t.Run("a list that works after one that failed", func(t *testing.T) {
		var lists atomic.Int32
		e := newEnv(t, envConfig{before: func(e *env) {
			e.dyn.PrependReactor("list", "pods", func(k8stesting.Action) (bool, k8sruntime.Object, error) {
				if lists.Add(1) == 1 {
					return true, nil, apierrors.NewForbidden(podsResource, "", nil)
				}
				return false, nil, nil
			})
		}}, pod("default", "a", nil))
		k := acquire(t, e, render.Pods)
		eventually(t, "the first list's error", func() bool {
			_, _, err := k.status()
			return err != nil
		})
		eventually(t, "the error cleared by the next list", func() bool {
			s, _, err := k.status()
			return s && err == nil && lists.Load() >= 2
		})
	})
	t.Run("a watch that opens after one that broke", func(t *testing.T) {
		var watches atomic.Int32
		e := newEnv(t, envConfig{before: func(e *env) {
			e.dyn.PrependWatchReactor("pods", func(k8stesting.Action) (bool, watch.Interface, error) {
				if watches.Add(1) == 1 {
					return true, nil, apierrors.NewTooManyRequests("the API is busy", 1)
				}
				return false, nil, nil
			})
		}}, pod("default", "a", nil))
		k := acquire(t, e, render.Pods)
		eventually(t, "stale after the first watch", func() bool {
			_, stale, _ := k.status()
			return stale
		})
		eventually(t, "stale cleared by the next watch", func() bool {
			_, stale, _ := k.status()
			return !stale && watches.Load() >= 2
		})
	})
	t.Run("a watch that ends with an error", func(t *testing.T) {
		var fw atomic.Pointer[watch.FakeWatcher]
		var watches atomic.Int32
		e := newEnv(t, envConfig{before: func(e *env) {
			e.dyn.PrependWatchReactor("pods", func(k8stesting.Action) (bool, watch.Interface, error) {
				if watches.Add(1) > 1 {
					// The watches after the broken one cannot be opened.
					return true, nil, apierrors.NewTooManyRequests("the API is busy", 1)
				}
				w := watch.NewFakeWithChanSize(1, false)
				fw.Store(w)
				return true, w, nil
			})
			e.dyn.PrependReactor("list", "pods", func(k8stesting.Action) (bool, k8sruntime.Object, error) {
				if watches.Load() > 0 {
					return true, nil, apierrors.NewTooManyRequests("the API is busy", 1)
				}
				return false, nil, nil
			})
		}})
		k := acquire(t, e, render.Pods)
		eventually(t, "the watch", func() bool { return fw.Load() != nil && synced(k) })
		if _, stale, _ := k.status(); stale {
			t.Fatal("stale with a working watch")
		}
		fw.Load().Error(&metav1.Status{Status: metav1.StatusFailure, Code: 500, Reason: metav1.StatusReasonInternalError, Message: "broken"})
		eventually(t, "stale", func() bool {
			_, stale, _ := k.status()
			return stale
		})
	})
}

func TestKindCacheIsShared(t *testing.T) {
	e := newEnv(t, envConfig{})
	lists := countLists(e, "pods")
	a := acquire(t, e, render.Pods)
	b := acquire(t, e, render.Pods)
	if a != b {
		t.Fatal("two caches of one kind")
	}
	eventually(t, "the first list", func() bool { return synced(a) })
	if n := lists.Load(); n != 1 {
		t.Errorf("%d lists, want 1", n)
	}
}

func TestKindCacheStopsWhenIdle(t *testing.T) {
	e := newEnv(t, envConfig{opts: Options{IdleStop: 20 * time.Millisecond}})
	lists := countLists(e, "pods")

	a := acquire(t, e, render.Pods)
	eventually(t, "the first list", func() bool { return synced(a) })
	e.c.release(a)
	b := acquire(t, e, render.Pods)
	if a != b || lists.Load() != 1 {
		t.Fatalf("a cache acquired again before IdleStop was not kept: %d lists", lists.Load())
	}
	e.c.release(b)

	eventually(t, "the idle cache is dropped", func() bool {
		e.c.mu.Lock()
		defer e.c.mu.Unlock()
		return e.c.caches[fullKey(render.Pods)] == nil
	})
	c := acquire(t, e, render.Pods)
	defer e.c.release(c)
	if c == a {
		t.Fatal("the stopped cache is used again")
	}
	eventually(t, "a new list", func() bool { return lists.Load() == 2 && synced(c) })
}

func TestAcquireAfterClose(t *testing.T) {
	e := newEnv(t, envConfig{})
	e.c.Close()
	if _, err := e.c.acquire(fullKey(render.Pods)); err == nil {
		t.Error("acquire after Close starts an informer")
	}
}

func TestCloseStopsEverything(t *testing.T) {
	before := runtime.NumGoroutine()
	e := newEnv(t, envConfig{opts: Options{DiscoveryEvery: time.Millisecond}}, pod("default", "a", nil))
	k := acquire(t, e, render.Pods)
	eventually(t, "the first list", func() bool { return synced(k) })
	j := acquire(t, e, render.Jobs)
	e.c.release(j) // idle, with its timer running
	e.setTables(&fakeTables{table: &metav1.Table{}})
	s := e.session(t)
	if _, err := s.Watch(Query{Resource: e.resource(t, "example.com", "widgets")}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Watch(Query{Resource: e.resource(t, "", "pods")}); err != nil {
		t.Fatal(err)
	}
	e.c.Close()
	e.c.Close()
	eventually(t, "the goroutines of the Cluster are gone", func() bool {
		return runtime.NumGoroutine() <= before
	})
}

func TestCloseWaitsForIdleTimers(t *testing.T) {
	e := newEnv(t, envConfig{opts: Options{IdleStop: time.Millisecond}})
	k := acquire(t, e, render.Pods)
	j := acquire(t, e, render.Jobs)
	eventually(t, "the first lists", func() bool { return synced(k) && synced(j) })

	// The timers fire while Close waits for c.mu, so that their functions
	// run after Close has seen them; Close returns only after they ended.
	e.c.release(k)
	e.c.release(j)
	e.c.mu.Lock()
	closed := make(chan struct{})
	go func() {
		e.c.Close()
		close(closed)
	}()
	time.Sleep(20 * time.Millisecond) // the timers fire, and wait for c.mu
	e.c.mu.Unlock()
	<-closed
	// wg.Done is stopIdle's last deferred call, so the goroutine that woke
	// Close may still be on its way out of it for a moment.
	buf := make([]byte, 1<<20)
	eventually(t, "no idle timer's function runs after Close", func() bool {
		return !strings.Contains(string(buf[:runtime.Stack(buf, true)]), "stopIdle")
	})
}

func drain(ch <-chan struct{}) {
	select {
	case <-ch:
	default:
	}
}

func waitSignal(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("no signal within 5s")
	}
}
