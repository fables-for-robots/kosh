package kube

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/fables-for-robots/kosh/render"
)

// Watch opens the table of q: of Containers and Rules from watchPseudo,
// of a typed resource from its kind's cache, and of any other resource
// from the API server's table. Every watch ends when the session's
// context is done. q is checked first, as checkQuery says: nothing is
// opened or sent for a query that fails.
func (s *session) Watch(q Query) (Watch, error) {
	if err := s.c.checkQuery(q); err != nil {
		return nil, err
	}
	switch {
	case isPseudo(q.Resource):
		return s.watchPseudo(q)
	case q.Resource.Typed:
		return s.watchTyped(q)
	case q.Of != nil:
		return nil, ErrNoRelation
	default:
		return s.watchTable(q)
	}
}

// checkQuery refuses a query before anything is opened or sent: a
// resource that checkResource refuses, Containers and Rules apart; a
// namespace that checkNamespace refuses (wrapping ErrInvalidNamespace),
// a namespace for a resource that is not namespaced among them; and an
// Of that checkRef refuses.
func (c *Cluster) checkQuery(q Query) error {
	if !isPseudo(q.Resource) {
		if err := c.checkResource(q.Resource); err != nil {
			return err
		}
	}
	if err := checkNamespace(q.Resource, q.Namespace); err != nil {
		return err
	}
	if q.Of != nil {
		return c.checkRef(*q.Of)
	}
	return nil
}

// isPseudo reports whether r is Containers or Rules, which are no
// resource of the API.
func isPseudo(r Resource) bool {
	return r.Is(Containers.GVR.Group, Containers.GVR.Resource) || r.Is(Rules.GVR.Group, Rules.GVR.Resource)
}

// typedWatch is the table of a typed resource: the rows of the objects
// of its kind's cache that pass its filter. The table of pods or of
// nodes is metered while the API serves their metrics.
type typedWatch struct {
	c        *Cluster
	res      Resource
	renderer render.Renderer
	cache    *kindCache
	filter   filter
	changed  <-chan struct{}

	// metered and metrics are nil for a kind without metric columns.
	metered render.Metered
	metrics *metricsHold

	unsubscribe func()
	stopCache   func() // ends the forwarding of the cache's signals; nil without metrics
	closeOnce   sync.Once

	mu           sync.Mutex
	stopAfter    func() bool // ends the Close that comes with the session's end
	renderLogged bool
}

func (s *session) watchTyped(q Query) (Watch, error) {
	renderer, ok := s.c.opts.Renderers(q.Resource.GVR)
	if !ok {
		return nil, fmt.Errorf("kosh has no renderer for %s", q.Resource.GVR)
	}
	api, err := s.api()
	if err != nil {
		return nil, err
	}
	f, err := api.filterOf(s.ctx, q)
	if err != nil {
		return nil, err
	}
	namespace := q.Namespace
	if q.Of != nil {
		namespace = q.Of.Namespace
	}
	k, err := s.c.acquire(s.cacheKey(q.Resource, namespace))
	if err != nil {
		return nil, err
	}
	changed, unsubscribe := k.subscribe()
	w := &typedWatch{
		c:           s.c,
		res:         q.Resource,
		renderer:    renderer,
		cache:       k,
		filter:      f,
		changed:     changed,
		unsubscribe: unsubscribe,
	}
	if m, ok := renderer.(render.Metered); ok {
		if gvr, ok := metricsOf(q.Resource); ok {
			// The cache's signals and the poller's are one Changed.
			ch := newChanges()
			w.metered = m
			w.metrics = &metricsHold{c: s.c, key: metricsKeyOf(k, gvr), changed: ch}
			w.changed = ch
			w.stopCache = ch.forward(s.c, changed)
		}
	}
	w.mu.Lock()
	w.stopAfter = context.AfterFunc(s.ctx, w.Close)
	w.mu.Unlock()
	return w, nil
}

// metricsKeyOf returns the key of the metrics poller of kind gvr that
// meters the rows of k: of k's role, and of its namespace.
func metricsKeyOf(k *kindCache, gvr schema.GroupVersionResource) cacheKey {
	return cacheKey{id: k.key.id, gvr: gvr, namespace: k.key.namespace}
}

// metricsOf returns the kind of the metrics API that meters r: PodMetrics
// for pods, NodeMetrics for nodes; ok is false for any other resource.
func metricsOf(r Resource) (gvr schema.GroupVersionResource, ok bool) {
	switch {
	case r.Is("", "pods"):
		return PodMetrics, true
	case r.Is("", "nodes"):
		return NodeMetrics, true
	}
	return schema.GroupVersionResource{}, false
}

func (w *typedWatch) Table(now time.Time) Table {
	var p *metricsPoller
	if w.metrics != nil {
		p = w.metrics.follow()
	}
	synced, stale, err := w.cache.status()
	if err != nil && !synced {
		// The first list failed: there is nothing to show but why.
		return Table{Namespaced: w.res.Namespaced, Err: err}
	}
	// A later list that failed leaves the last rows, which may be out of
	// date.
	t := Table{Columns: w.renderer.Columns(), Namespaced: w.res.Namespaced, Stale: stale || err != nil}
	if !synced {
		t.Stale = true
		return w.strip(t, p)
	}
	usage := readingOf(p)
	for _, o := range w.cache.list() {
		if !w.filter(o) {
			continue
		}
		row, err := w.renderer.Row(o, now)
		if err != nil {
			w.logRenderError(err)
			continue
		}
		if usage != nil {
			// Metered whole, before any column is stripped: Meter finds
			// the cells at the renderer's indexes.
			w.metered.Meter(&row, o, usage[metricsKey(o)])
		}
		t.Rows = append(t.Rows, row)
	}
	return w.strip(t, p)
}

// strip leaves the metric columns out of t when the watch is of a
// metered kind and holds no poller: the API does not serve the metrics.
func (w *typedWatch) strip(t Table, p *metricsPoller) Table {
	if w.metrics != nil && p == nil {
		t.Columns, t.Rows = render.StripMetrics(t.Columns, t.Rows)
	}
	return t
}

// logRenderError logs the first object of the watch that does not
// render.
func (w *typedWatch) logRenderError(err error) {
	w.mu.Lock()
	logged := w.renderLogged
	w.renderLogged = true
	w.mu.Unlock()
	if !logged {
		w.c.log.Warn("an object does not render and is left out", "resource", w.res.GVR.String(), "err", err)
	}
}

func (w *typedWatch) Changed() <-chan struct{} { return w.changed }

func (w *typedWatch) Close() {
	w.closeOnce.Do(func() {
		w.mu.Lock()
		stop := w.stopAfter
		w.mu.Unlock()
		stop()
		if w.metrics != nil {
			w.metrics.close()
			w.stopCache()
		}
		w.unsubscribe()
		w.c.release(w.cache)
	})
}

// metricsHold is a watch's hold on the metrics poller of its kind: taken
// while discovery says the API serves the metrics, given back when it
// stops saying so or the watch closes. While it is held, the poller's
// signals are forwarded into the watch's Changed.
type metricsHold struct {
	c       *Cluster
	key     cacheKey // of the poller
	changed changes

	mu     sync.Mutex
	poller *metricsPoller // nil while not held
	stop   func()         // ends the forwarding of the poller's signals
	closed bool
	logged bool // an error of acquireMetrics is in the log
}

// follow takes the poller when the API serves the metrics and the hold
// has none, gives it back when the API does not, and returns it: nil
// while it is not held. It is called on every Table, so that a watch
// opened before metrics-server was installed picks it up, and one opened
// before it was removed lets it go.
func (h *metricsHold) follow() *metricsPoller {
	served := h.c.metricsServed(h.key.gvr)
	h.mu.Lock()
	defer h.mu.Unlock()
	switch {
	case h.closed:
		return nil
	case served && h.poller == nil:
		p, err := h.c.acquireMetrics(h.key)
		if err != nil {
			// After Close of the Cluster there is nothing to say.
			if !errors.Is(err, errClosed) && !h.logged {
				h.logged = true
				h.c.log.Warn("metrics cannot be read for a table", "kind", h.key.gvr.GroupResource().String(), "err", err)
			}
			return nil
		}
		sub, unsubscribe := p.subscribe()
		stopForward := h.changed.forward(h.c, sub)
		h.poller = p
		h.stop = func() {
			stopForward()
			unsubscribe()
		}
		// The poller may have a reading already, shared with another
		// watch, or have listed before the subscription: the watch is not
		// to wait for the next poll to show it.
		h.changed.notify()
	case !served && h.poller != nil:
		h.drop()
	}
	return h.poller
}

// drop gives the poller back. h.mu is held.
func (h *metricsHold) drop() {
	h.stop()
	h.c.releaseMetrics(h.poller)
	h.poller, h.stop = nil, nil
}

// close gives the poller back, if it is held, and takes none after.
func (h *metricsHold) close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closed = true
	if h.poller != nil {
		h.drop()
	}
}

// readingOf returns p's last reading, nil without p, before a list
// worked, and while the last list failed: a reading older than the last
// poll is not shown as current, and its cells say n/a.
func readingOf(p *metricsPoller) reading {
	if p == nil {
		return nil
	}
	r, err := p.current()
	if err != nil {
		return nil
	}
	return r
}

// metricsKey is what a reading keys obj's usage by: "namespace/name" for
// a pod, the name for a node.
func metricsKey(obj runtime.Object) string {
	m, ok := obj.(metav1.Object)
	if !ok {
		return ""
	}
	if m.GetNamespace() == "" {
		return m.GetName()
	}
	return m.GetNamespace() + "/" + m.GetName()
}
