package kube

import (
	"context"
	"errors"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/fables-for-robots/kosh/keys"
)

// usageWatch is the cluster's use of its nodes, for the header. While
// the API serves the metrics of nodes it holds their poller and the
// nodes' cache, which has what each node can give; while it does not it
// holds neither, so that a session on a cluster without metrics-server
// keeps no informer of nodes for a header that shows nothing. The hold
// follows discovery at open, on every Current and every
// opts.MetricsPoll; the usage is computed on each Current from what the
// two hold then.
//
// Changed receives at open, after each list of the poller, when the hold
// is taken or given back, and once when the nodes' cache first syncs,
// but not when a node changes: the header's arrow compares one reading
// with the one before, and is to show the change from one poll to the
// next, not from a node's status update between two polls. A node's
// allocatable rarely changes, and the next poll's Current reads it from
// the cache. Close closes Changed.
//
// The poller and the cache are the session's role's, shared with every
// session of the role. A Role cannot grant nodes, which are of no
// namespace, so the watch of a Role-bound session never takes the hold
// and is never Served: its header has no lines.
type usageWatch struct {
	c           *Cluster
	role        identity // whose poller and cache the hold is
	clusterRole bool     // the role is a ClusterRole, which may grant nodes
	changed     chan struct{}
	stopTick    func()

	closeOnce sync.Once

	// sigMu guards the sends on changed and its close, and nothing else,
	// so that a goroutine that signals never waits for the hold, which
	// may be waiting for that goroutine to end.
	sigMu     sync.Mutex
	sigClosed bool

	mu          sync.Mutex
	closed      bool
	stopAfter   func() bool // ends the Close that comes with the session's end
	poller      *metricsPoller
	cache       *kindCache // held with poller
	stopHold    func()     // ends the forwarding of the poller's signals and the wait for the cache's sync
	logged      bool       // the watch's failure to take the hold is in the log
	nodesLogged bool       // the nodes' list's failure is in the log
}

func (s *session) ClusterUsage() UsageWatch {
	w := &usageWatch{c: s.c, role: s.role, clusterRole: s.grant.Role.Kind != keys.KindRole, changed: make(chan struct{}, 1)}
	// The poller and the cache may be another session's, synced and with
	// a reading, and signal nothing until they change: the consumer is
	// not to wait for that to see what Current says. The signal is sent
	// before anything can close the watch.
	w.notify()
	w.stopTick = w.tick()
	// The polling starts at open: the hold is taken now when the API
	// serves the metrics of nodes.
	w.follow()

	// Last, as everything Close stops is in place: a session that has
	// ended already closes the watch at once.
	w.mu.Lock()
	w.stopAfter = context.AfterFunc(s.ctx, w.Close)
	w.mu.Unlock()
	return w
}

// notify signals Changed, without waiting, unless the watch is closed.
func (w *usageWatch) notify() {
	w.sigMu.Lock()
	defer w.sigMu.Unlock()
	if w.sigClosed {
		return
	}
	select {
	case w.changed <- struct{}{}:
	default:
	}
}

// follow takes the hold, the nodes' poller and the nodes' cache, when
// the API serves the metrics of nodes and the cluster has nodes kosh can
// read, and gives it back when not; it returns what is held, nil while
// nothing is. It logs once for the watch when the nodes cannot be
// listed, which leaves Current without a percentage.
func (w *usageWatch) follow() (*metricsPoller, *kindCache) {
	nodes, ok := w.c.resourceFor("", "Node")
	served := w.clusterRole && ok && nodes.Typed && w.c.metricsServed(NodeMetrics)
	w.mu.Lock()
	defer w.mu.Unlock()
	switch {
	case w.closed:
		return nil, nil
	case served && w.poller == nil:
		w.take(nodes)
	case !served && w.poller != nil:
		w.drop()
	}
	if w.cache != nil && !w.nodesLogged {
		if synced, _, err := w.cache.status(); err != nil && !synced {
			w.nodesLogged = true
			w.c.log.Warn("the nodes cannot be listed: the cluster's CPU and memory are n/a", "err", err)
		}
	}
	return w.poller, w.cache
}

// take acquires the nodes' cache and their poller, and signals. w.mu is
// held.
func (w *usageWatch) take(nodes Resource) {
	c := w.c
	k, err := c.acquire(cacheKey{id: w.role, gvr: nodes.GVR})
	if err != nil {
		w.logTake(err)
		return
	}
	p, err := c.acquireMetrics(cacheKey{id: w.role, gvr: NodeMetrics})
	if err != nil {
		c.release(k)
		w.logTake(err)
		return
	}
	sub, unsubscribe := p.subscribe()
	stopForward := c.forward(sub, w.notify)
	stopSynced := w.synced(k)
	w.poller, w.cache = p, k
	w.stopHold = func() {
		stopSynced()
		stopForward()
		unsubscribe()
	}
	// The poller may have a reading already, or have listed before the
	// subscription.
	w.notify()
}

// logTake logs, once for the watch, why the hold could not be taken;
// after Close of the Cluster there is nothing to say. w.mu is held.
func (w *usageWatch) logTake(err error) {
	if errors.Is(err, errClosed) || w.logged {
		return
	}
	w.logged = true
	w.c.log.Warn("the cluster's usage cannot be read", "err", err)
}

// drop gives the hold back. w.mu is held. The goroutines it stops only
// signal, which takes sigMu, not w.mu.
func (w *usageWatch) drop() {
	w.stopHold()
	w.c.releaseMetrics(w.poller)
	w.c.release(w.cache)
	w.poller, w.cache, w.stopHold = nil, nil, nil
}

// held reports whether the watch has the hold.
func (w *usageWatch) held() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.poller != nil
}

// synced signals Changed once k has its first list, from a goroutine the
// Cluster runs, until the returned function is called or the Cluster is
// closed: a reading that came before the nodes has no percentage, and
// the next list may be a whole MetricsPoll away. Before that Current was
// never ok, so the signal moves no arrow. The function returns once the
// goroutine has ended.
func (w *usageWatch) synced(k *kindCache) (stop func()) {
	return w.c.goUntil(func(quit <-chan struct{}) {
		select {
		case <-k.informer.HasSyncedChecker().Done():
			w.notify()
		case <-quit:
		case <-w.c.ctx.Done():
		}
	})
}

// tick takes the hold or gives it back as discovery says, every
// opts.MetricsPoll, from a goroutine the Cluster runs, until the
// returned function is called or the Cluster is closed: without the
// poller nothing else signals when metrics-server is installed, and the
// header's reading is not to wait for its consumer to call Current. It
// signals Changed when the hold changed. The function returns once the
// goroutine has ended.
func (w *usageWatch) tick() (stop func()) {
	return w.c.goUntil(func(quit <-chan struct{}) {
		t := time.NewTicker(w.c.opts.MetricsPoll)
		defer t.Stop()
		for {
			select {
			case <-quit:
				return
			case <-w.c.ctx.Done():
				return
			case <-t.C:
				held := w.held()
				if p, _ := w.follow(); (p != nil) != held {
					w.notify()
				}
			}
		}
	})
}

// Current takes or gives back the hold as discovery says now, as a
// table's watch does on every Table, and computes the usage from the
// last reading and the nodes of the cache.
func (w *usageWatch) Current() (ClusterUsage, bool) {
	p, k := w.follow()
	if p == nil {
		return ClusterUsage{}, false
	}
	u := ClusterUsage{Served: true}
	r := readingOf(p)
	if r == nil {
		return u, false
	}
	var cpu, mem, allocCPU, allocMem int64
	for _, o := range k.list() {
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
	// When either sum of allocatable is 0 (no node in the cache yet, the
	// nodes cannot be listed, or none says what it can give) there is no
	// percentage of it, and none of the other either: a ClusterUsage has
	// one ok for both lines, so both say n/a rather than one say a number
	// nobody can check.
	if allocCPU == 0 || allocMem == 0 {
		return u, false
	}
	u.CPU = int(100 * cpu / allocCPU)
	u.Mem = int(100 * mem / allocMem)
	return u, true
}

func (w *usageWatch) Changed() <-chan struct{} { return w.changed }

// Close gives the hold back, stops the tick, and closes Changed, under
// sigMu, so that no signal can come after: a consumer waiting on Changed
// ends with the watch, and does not outlive the session that opened it.
func (w *usageWatch) Close() {
	w.closeOnce.Do(func() {
		w.mu.Lock()
		w.closed = true
		stop := w.stopAfter
		if w.poller != nil {
			w.drop()
		}
		w.mu.Unlock()
		if stop != nil {
			stop()
		}
		w.stopTick()

		w.sigMu.Lock()
		w.sigClosed = true
		close(w.changed)
		w.sigMu.Unlock()
	})
}
