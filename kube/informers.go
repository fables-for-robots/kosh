package kube

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/cache"
)

// errClosed is what acquire returns once the Cluster is closed.
var errClosed = errors.New("the connection to the cluster is closed")

// cacheKey names one cache: the identity it lists and watches as (a
// role's), its kind, and its namespace, "" for every namespace.
type cacheKey struct {
	id        identity
	gvr       schema.GroupVersionResource
	namespace string
}

// kindCache is the informer of one kind for one role: shared by the
// sessions of the role, of every namespace for a ClusterRole and of one
// for a Role, over the dynamic client of the role's identity, whose
// objects are converted into their typed structs before they are
// stored.
type kindCache struct {
	c        *Cluster
	key      cacheKey
	zero     func() runtime.Object
	typ      reflect.Type // of what zero returns
	informer cache.SharedIndexInformer
	cancel   context.CancelFunc

	// Guarded by c.mu.
	users int
	idle  *time.Timer
	gen   int // counts the times the cache became idle

	mu            sync.Mutex
	subs          map[chan struct{}]struct{}
	listErr       error
	stale         bool
	convertLogged bool
}

// acquire returns the cache of key, of a typed kind, started when it
// was not running, and counts one more user of it; release counts one
// less.
func (c *Cluster) acquire(key cacheKey) (*kindCache, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, errClosed
	}
	k := c.caches[key]
	if k == nil {
		var err error
		if k, err = c.startCache(key); err != nil {
			return nil, err
		}
		c.caches[key] = k
	}
	k.users++
	k.stopTimer()
	return k, nil
}

// release counts one user of k less. A cache without users for
// opts.IdleStop is stopped and dropped.
func (c *Cluster) release(k *kindCache) {
	c.mu.Lock()
	defer c.mu.Unlock()
	k.users--
	if k.users > 0 || c.closed || c.caches[k.key] != k {
		return
	}
	k.gen++
	gen := k.gen
	// The timer's function is counted in c.wg from now: Close waits for
	// it, or stopTimer takes it back when it never runs.
	c.wg.Add(1)
	k.idle = time.AfterFunc(c.opts.IdleStop, func() { c.stopIdle(k, gen) })
}

// stopTimer stops k's idle timer, and when its function will not run,
// takes back what release counted for it. c.mu is held.
func (k *kindCache) stopTimer() {
	if k.idle == nil {
		return
	}
	if k.idle.Stop() {
		k.c.wg.Done()
	}
	k.idle = nil
}

// stopIdle stops k when it has had no user since it became idle the
// gen-th time. It is the function of an idle timer.
func (c *Cluster) stopIdle(k *kindCache, gen int) {
	defer c.wg.Done()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || k.users > 0 || k.gen != gen || c.caches[k.key] != k {
		return
	}
	delete(c.caches, k.key)
	k.idle = nil
	k.cancel()
}

// stopCaches stops every cache and every metrics poller, for Close.
func (c *Cluster) stopCaches() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	for key, k := range c.caches {
		k.stopTimer()
		k.cancel()
		delete(c.caches, key)
	}
	c.stopPollers()
}

// listWatch is a cache.ListWatch that always lists, then watches: the
// reflector would otherwise stream the first list as a watch, and the
// error of a list that fails is what a table shows (§8.5).
type listWatch struct {
	*cache.ListWatch
}

func (listWatch) IsWatchListSemanticsUnSupported() bool { return true }

// startCache makes the cache of key and starts its informer, which
// lists and watches as key's identity, in key's namespace when it has
// one. c.mu is held.
func (c *Cluster) startCache(key cacheKey) (*kindCache, error) {
	gvr := key.gvr
	if _, ok := c.zeroOf(gvr); !ok {
		return nil, fmt.Errorf("kosh has no type for %s", gvr)
	}
	api, err := c.clientsFor(key.id)
	if err != nil {
		return nil, err
	}
	var objects dynamic.ResourceInterface = api.dyn.Resource(gvr)
	if key.namespace != "" {
		objects = api.dyn.Resource(gvr).Namespace(key.namespace)
	}
	ctx, cancel := context.WithCancel(c.ctx)
	k := &kindCache{
		c:      c,
		key:    key,
		cancel: cancel,
		subs:   map[chan struct{}]struct{}{},
	}
	k.zero = func() runtime.Object {
		o, _ := c.zeroOf(gvr)
		return o
	}
	k.typ = reflect.TypeOf(k.zero())

	lw := listWatch{&cache.ListWatch{
		ListWithContextFunc: func(ctx context.Context, opts metav1.ListOptions) (runtime.Object, error) {
			list, err := objects.List(ctx, opts)
			if ctx.Err() == nil {
				k.listed(err)
			}
			if err != nil {
				return nil, err
			}
			return list, nil
		},
		WatchFuncWithContext: func(ctx context.Context, opts metav1.ListOptions) (watch.Interface, error) {
			w, err := objects.Watch(ctx, opts)
			if err != nil {
				if ctx.Err() == nil {
					k.setStale(true)
				}
				return nil, err
			}
			k.setStale(false)
			return c.probe(w, func() { k.setStale(true) }), nil
		},
	}}
	k.informer = cache.NewSharedIndexInformerWithOptions(lw, &unstructured.Unstructured{},
		cache.SharedIndexInformerOptions{ObjectDescription: gvr.String()})
	if err := k.informer.SetTransform(k.transform); err != nil {
		cancel()
		return nil, err
	}
	signal := func(any) { k.signal() }
	if _, err := k.informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    signal,
		UpdateFunc: func(_, obj any) { k.signal() },
		DeleteFunc: signal,
	}); err != nil {
		cancel()
		return nil, err
	}

	c.wg.Add(2)
	go func() {
		defer c.wg.Done()
		k.informer.RunWithContext(ctx)
	}()
	go func() {
		// The first list in is a change of status, even when it brought
		// no object.
		defer c.wg.Done()
		select {
		case <-k.informer.HasSyncedChecker().Done():
			k.signal()
		case <-ctx.Done():
		}
	}()
	return k, nil
}

// transform turns what the API sent into the kind's typed struct,
// without managedFields. An object that does not convert is logged, once
// for the kind, and kept only as its name, which list and get leave out.
func (k *kindCache) transform(obj any) (any, error) {
	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return obj, nil
	}
	typed := k.zero()
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, typed); err != nil {
		k.mu.Lock()
		logged := k.convertLogged
		k.convertLogged = true
		k.mu.Unlock()
		if !logged {
			k.c.log.Warn("an object does not convert to its type and is left out", "resource", k.key.gvr.String(),
				"namespace", u.GetNamespace(), "name", u.GetName(), "err", err)
		}
		return &metav1.PartialObjectMetadata{ObjectMeta: metav1.ObjectMeta{Namespace: u.GetNamespace(), Name: u.GetName()}}, nil
	}
	if m, err := meta.Accessor(typed); err == nil {
		m.SetManagedFields(nil)
	}
	return typed, nil
}

// list returns the objects the cache holds, typed (*corev1.Pod …), in no
// order.
func (k *kindCache) list() []runtime.Object {
	items := k.informer.GetStore().List()
	out := make([]runtime.Object, 0, len(items))
	for _, it := range items {
		if o, ok := it.(runtime.Object); ok && reflect.TypeOf(o) == k.typ {
			out = append(out, o)
		}
	}
	return out
}

// get returns one object by namespace and name.
func (k *kindCache) get(namespace, name string) (runtime.Object, bool) {
	key := name
	if namespace != "" {
		key = namespace + "/" + name
	}
	it, ok, err := k.informer.GetStore().GetByKey(key)
	if err != nil || !ok {
		return nil, false
	}
	o, ok := it.(runtime.Object)
	if !ok || reflect.TypeOf(o) != k.typ {
		return nil, false
	}
	return o, true
}

// subscribe returns a channel with room for one signal that receives
// after anything of the kind changed, and a function that ends that.
func (k *kindCache) subscribe() (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	k.mu.Lock()
	k.subs[ch] = struct{}{}
	k.mu.Unlock()
	return ch, func() {
		k.mu.Lock()
		delete(k.subs, ch)
		k.mu.Unlock()
	}
}

// status reports whether the first list is in (synced), whether the
// watch is broken at the moment (stale), and the error of a list that
// failed.
func (k *kindCache) status() (synced, stale bool, err error) {
	synced = k.informer.HasSynced()
	k.mu.Lock()
	defer k.mu.Unlock()
	return synced, k.stale, k.listErr
}

// signal tells every subscriber, without waiting for any.
func (k *kindCache) signal() {
	k.mu.Lock()
	defer k.mu.Unlock()
	for ch := range k.subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// listed records what became of a list: its error, or that the cache is
// fresh again.
func (k *kindCache) listed(err error) {
	k.mu.Lock()
	changed := (err == nil) != (k.listErr == nil) || err != nil && err.Error() != k.listErr.Error() || err == nil && k.stale
	k.listErr = err
	if err == nil {
		k.stale = false
	}
	k.mu.Unlock()
	if changed {
		k.signal()
	}
}

// setStale records whether the watch is broken.
func (k *kindCache) setStale(stale bool) {
	k.mu.Lock()
	changed := k.stale != stale
	k.stale = stale
	k.mu.Unlock()
	if changed {
		k.signal()
	}
}

// watchProbe passes the events of a watch on, and calls broken when one
// of them is an error.
type watchProbe struct {
	inner watch.Interface
	out   chan watch.Event
	stop  chan struct{}
	once  sync.Once
}

// probe returns w, watched for an error. It is called from an informer's
// goroutine, which stops what it returns before it ends.
func (c *Cluster) probe(w watch.Interface, broken func()) watch.Interface {
	p := &watchProbe{inner: w, out: make(chan watch.Event), stop: make(chan struct{})}
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		defer close(p.out)
		for {
			select {
			case <-p.stop:
				return
			case ev, ok := <-w.ResultChan():
				if !ok {
					return
				}
				if ev.Type == watch.Error {
					broken()
				}
				select {
				case p.out <- ev:
				case <-p.stop:
					return
				}
			}
		}
	}()
	return p
}

func (p *watchProbe) ResultChan() <-chan watch.Event { return p.out }

func (p *watchProbe) Stop() {
	p.once.Do(func() {
		close(p.stop)
		p.inner.Stop()
	})
}
