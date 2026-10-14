package kube

import (
	"context"
	"errors"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
)

// usageWatch is the cluster's use of its nodes, for the header: the
// nodes' cache, which has what each node can give, and the poller of the
// nodes' metrics, held while the API serves them. It is computed on each
// Current from what the two hold now.
type usageWatch struct {
	c *Cluster
	// cache is nil when the cluster has no nodes kosh can read: Current
	// is then never ok, and nothing runs.
	cache   *kindCache
	metrics *metricsHold
	changed changes
	stop    func() // ends the forwarding of the cache's signals and the tick

	closeOnce sync.Once

	mu        sync.Mutex
	stopAfter func() bool // ends the Close that comes with the session's end
}

func (s *session) ClusterUsage() UsageWatch {
	c := s.c
	w := &usageWatch{c: c, changed: newChanges()}
	nodes, ok := c.resourceFor("", "Node")
	if !ok || !nodes.Typed {
		return w
	}
	k, err := c.acquire(nodes.GVR)
	if err != nil {
		if !errors.Is(err, errClosed) {
			c.log.Warn("the nodes cannot be read for the cluster's usage", "err", err)
		}
		return w
	}
	w.cache = k
	w.metrics = &metricsHold{c: c, gvr: NodeMetrics, changed: w.changed}
	sub, unsubscribe := k.subscribe()
	stopForward := w.changed.forward(sub, c.ctx.Done())
	stopTick := w.tick()
	w.stop = func() {
		stopTick()
		stopForward()
		unsubscribe()
	}
	// So that the first Current says whether the API is served.
	w.metrics.follow()

	w.mu.Lock()
	w.stopAfter = context.AfterFunc(s.ctx, w.Close)
	w.mu.Unlock()
	return w
}

// tick signals Changed every opts.MetricsPoll at which discovery says
// otherwise than the hold of the poller does, from a goroutine of its
// own, until the returned function is called or the Cluster is closed:
// without the poller nothing else signals when metrics-server is
// installed. The Current that follows takes the poller, or gives it
// back. The function returns once the goroutine has ended.
func (w *usageWatch) tick() (stop func()) {
	quit, ended := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(ended)
		t := time.NewTicker(w.c.opts.MetricsPoll)
		defer t.Stop()
		for {
			select {
			case <-quit:
				return
			case <-w.c.ctx.Done():
				return
			case <-t.C:
				if w.c.metricsServed(NodeMetrics) != w.metrics.held() {
					w.changed.notify()
				}
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() { close(quit) })
		<-ended
	}
}

// Current takes or gives back the poller as discovery says now, as a
// table's watch does on every Table, and computes the usage from the
// last reading and the nodes of the cache.
func (w *usageWatch) Current() (ClusterUsage, bool) {
	if w.cache == nil {
		return ClusterUsage{}, false
	}
	p := w.metrics.follow()
	if p == nil {
		return ClusterUsage{}, false
	}
	u := ClusterUsage{Served: true}
	r := readingOf(p)
	if r == nil {
		return u, false
	}
	var cpu, mem, allocCPU, allocMem int64
	for _, o := range w.cache.list() {
		n, ok := o.(*corev1.Node)
		if !ok {
			continue
		}
		allocCPU += n.Status.Allocatable.Cpu().MilliValue()
		allocMem += n.Status.Allocatable.Memory().Value()
		if use, ok := r[n.Name][""]; ok {
			cpu += use.CPU
			mem += use.Mem
		}
	}
	// No node that says what it can give, or none in the cache yet: no
	// percentage.
	if allocCPU == 0 || allocMem == 0 {
		return u, false
	}
	u.CPU = int(100 * cpu / allocCPU)
	u.Mem = int(100 * mem / allocMem)
	return u, true
}

func (w *usageWatch) Changed() <-chan struct{} { return w.changed }

func (w *usageWatch) Close() {
	w.closeOnce.Do(func() {
		if w.cache == nil {
			return
		}
		w.mu.Lock()
		stop := w.stopAfter
		w.mu.Unlock()
		stop()
		w.metrics.close()
		w.stop()
		w.c.release(w.cache)
	})
}
