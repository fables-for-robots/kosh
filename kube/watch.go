package kube

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/fables-for-robots/kosh/render"
)

// Watch opens the table of q: of Containers and Rules from watchPseudo,
// of a typed resource from its kind's cache, and of any other resource
// from the API server's table. Every watch ends when the session's
// context is done.
func (s *session) Watch(q Query) (Watch, error) {
	switch {
	case q.Resource.Is(Containers.GVR.Group, Containers.GVR.Resource),
		q.Resource.Is(Rules.GVR.Group, Rules.GVR.Resource):
		return s.watchPseudo(q)
	case q.Resource.Typed:
		return s.watchTyped(q)
	case q.Of != nil:
		return nil, ErrNoRelation
	default:
		return s.watchTable(q)
	}
}

// typedWatch is the table of a typed resource: the rows of the objects
// of its kind's cache that pass its filter.
type typedWatch struct {
	c        *Cluster
	res      Resource
	renderer render.Renderer
	cache    *kindCache
	filter   filter
	changed  <-chan struct{}

	unsubscribe func()
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
	f, err := s.c.filterOf(s.ctx, q)
	if err != nil {
		return nil, err
	}
	k, err := s.c.acquire(q.Resource.GVR)
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
	w.mu.Lock()
	w.stopAfter = context.AfterFunc(s.ctx, w.Close)
	w.mu.Unlock()
	return w, nil
}

func (w *typedWatch) Table(now time.Time) Table {
	synced, stale, err := w.cache.status()
	if err != nil {
		return Table{Namespaced: w.res.Namespaced, Err: err}
	}
	t := Table{Columns: w.renderer.Columns(), Namespaced: w.res.Namespaced, Stale: stale}
	if !synced {
		t.Stale = true
		return t
	}
	for _, o := range w.cache.list() {
		if !w.filter(o) {
			continue
		}
		row, err := w.renderer.Row(o, now)
		if err != nil {
			w.logRenderError(err)
			continue
		}
		t.Rows = append(t.Rows, row)
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
		w.unsubscribe()
		w.c.release(w.cache)
	})
}
