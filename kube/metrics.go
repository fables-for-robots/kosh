package kube

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/fables-for-robots/kosh/render"
)

// The resources of the metrics API that metrics-server serves.
var (
	PodMetrics  = schema.GroupVersionResource{Group: "metrics.k8s.io", Version: "v1beta1", Resource: "pods"}
	NodeMetrics = schema.GroupVersionResource{Group: "metrics.k8s.io", Version: "v1beta1", Resource: "nodes"}
)

// reading is what one list of a metrics kind said: for pods, the usage
// of each container of each pod, keyed "namespace/name" then the
// container's name; for nodes, the usage of each node, keyed by name
// under the container key "". A nil reading is no reading. A reading is
// not changed once it is made.
type reading map[string]map[string]render.Usage

// metricsServed reports whether the API serves gvr, as discovery last
// found, without a request.
func (c *Cluster) metricsServed(gvr schema.GroupVersionResource) bool {
	c.resMu.RLock()
	defer c.resMu.RUnlock()
	for _, r := range c.resources {
		if r.GVR == gvr {
			return true
		}
	}
	return false
}

// metricsPoller reads the metrics of one kind every opts.MetricsPoll
// while it has a user, and keeps the last reading. The metrics API can
// be listed and not watched, so this is what an informer is to a kind
// that can.
type metricsPoller struct {
	c      *Cluster
	gvr    schema.GroupVersionResource
	cancel context.CancelFunc

	users int // guarded by c.mu, as kindCache.users is

	mu          sync.Mutex
	subs        map[chan struct{}]struct{}
	reading     reading // nil until a list worked
	err         error   // of the last list, nil when it worked
	logged      bool    // err is in the log
	parseLogged bool    // an item that does not parse is in the log
}

// acquireMetrics returns the poller of gvr, PodMetrics or NodeMetrics,
// started when it was not running, and counts one more user of it;
// releaseMetrics counts one less.
func (c *Cluster) acquireMetrics(gvr schema.GroupVersionResource) (*metricsPoller, error) {
	if gvr != PodMetrics && gvr != NodeMetrics {
		return nil, fmt.Errorf("kosh reads no metrics of %s", gvr)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, errClosed
	}
	p := c.pollers[gvr]
	if p == nil {
		ctx, cancel := context.WithCancel(c.ctx)
		p = &metricsPoller{c: c, gvr: gvr, cancel: cancel, subs: map[chan struct{}]struct{}{}}
		c.pollers[gvr] = p
		c.wg.Add(1)
		go p.run(ctx)
	}
	p.users++
	return p, nil
}

// releaseMetrics counts one user of p less. A poller without users is
// stopped at once: there is no cache to keep warm, and the next user
// gets a reading within one request.
func (c *Cluster) releaseMetrics(p *metricsPoller) {
	c.mu.Lock()
	defer c.mu.Unlock()
	p.users--
	if p.users > 0 || c.pollers[p.gvr] != p {
		return
	}
	delete(c.pollers, p.gvr)
	p.cancel()
}

// stopPollers stops every poller, for Close. c.mu is held.
func (c *Cluster) stopPollers() {
	for gvr, p := range c.pollers {
		p.cancel()
		delete(c.pollers, gvr)
	}
}

// run lists at once, then every opts.MetricsPoll, until ctx is done.
func (p *metricsPoller) run(ctx context.Context) {
	defer p.c.wg.Done()
	t := time.NewTicker(p.c.opts.MetricsPoll)
	defer t.Stop()
	p.list(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.list(ctx)
		}
	}
}

// list reads the metrics of the whole cluster in one request, keeps the
// reading or the error, logs a change between the two, and tells the
// subscribers.
func (p *metricsPoller) list(ctx context.Context) {
	l, err := p.c.dyn.Resource(p.gvr).List(ctx, metav1.ListOptions{})
	if ctx.Err() != nil {
		// Released or closed while it listed: nobody wants the answer.
		return
	}
	var r reading
	if err == nil {
		r = parseReading(p.gvr, l, p.logParse)
	}
	kind := p.gvr.GroupResource().String()

	p.mu.Lock()
	warn, again := false, false
	if err != nil {
		warn = !p.logged
		p.err, p.logged = err, true
	} else {
		again = p.logged
		p.reading, p.err, p.logged = r, nil, false
	}
	p.mu.Unlock()

	switch {
	case warn:
		p.c.log.Warn("metrics could not be read", "kind", kind, "err", err)
	case again:
		p.c.log.Info("metrics can be read again", "kind", kind)
	}
	p.signal()
}

// logParse logs an item that does not parse, once for the poller: the
// same item comes back on every list.
func (p *metricsPoller) logParse(msg string, args ...any) {
	p.mu.Lock()
	logged := p.parseLogged
	p.parseLogged = true
	p.mu.Unlock()
	if !logged {
		p.c.log.Warn(msg, append([]any{"kind", p.gvr.GroupResource().String()}, args...)...)
	}
}

// current returns the last reading, nil until a list worked, and the
// error of the last list, nil when it worked. A failed list keeps the
// reading before it, which the caller shows as unknown while err is
// set.
func (p *metricsPoller) current() (r reading, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.reading, p.err
}

// subscribe returns a channel with room for one signal that receives
// after every list, worked or not, and a function that ends that.
func (p *metricsPoller) subscribe() (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	p.mu.Lock()
	p.subs[ch] = struct{}{}
	p.mu.Unlock()
	return ch, func() {
		p.mu.Lock()
		delete(p.subs, ch)
		p.mu.Unlock()
	}
}

// signal tells every subscriber, without waiting for any.
func (p *metricsPoller) signal() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for ch := range p.subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// parseReading reads a list of PodMetrics or NodeMetrics, which are
// parsed from unstructured rather than through k8s.io/metrics: two
// quantities per item do not warrant a module. An item that lacks a
// name, or whose quantity does not parse, is left out, and the first
// such item of the list is given to log.
func parseReading(gvr schema.GroupVersionResource, list *unstructured.UnstructuredList, log func(msg string, args ...any)) reading {
	r := reading{}
	logged := false
	for _, it := range list.Items {
		var (
			key   string
			usage map[string]render.Usage
			err   error
		)
		switch {
		case it.GetName() == "":
			err = errors.New("it has no name")
		case gvr == NodeMetrics:
			key = it.GetName()
			var u render.Usage
			if u, err = parseUsage(it.Object, "usage"); err == nil {
				usage = map[string]render.Usage{"": u}
			}
		case it.GetNamespace() == "":
			err = errors.New("it has no namespace")
		default:
			key = it.GetNamespace() + "/" + it.GetName()
			usage, err = parseContainers(it.Object)
		}
		if err != nil {
			if !logged && log != nil {
				log("a metrics item does not parse and is left out", "namespace", it.GetNamespace(), "name", it.GetName(), "err", err)
				logged = true
			}
			continue
		}
		r[key] = usage
	}
	return r
}

// parseContainers reads the usage of each container of a PodMetrics.
func parseContainers(obj map[string]any) (map[string]render.Usage, error) {
	cs, _, err := unstructured.NestedSlice(obj, "containers")
	if err != nil {
		return nil, err
	}
	out := make(map[string]render.Usage, len(cs))
	for _, c := range cs {
		m, ok := c.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("a container is a %T", c)
		}
		name, _, _ := unstructured.NestedString(m, "name")
		if name == "" {
			return nil, errors.New("a container has no name")
		}
		u, err := parseUsage(m, "usage")
		if err != nil {
			return nil, fmt.Errorf("container %s: %w", name, err)
		}
		out[name] = u
	}
	return out, nil
}

// parseUsage reads the quantities cpu and memory under field of obj:
// CPU in millicores, memory in bytes.
func parseUsage(obj map[string]any, field string) (render.Usage, error) {
	cpu, err := quantity(obj, field, "cpu")
	if err != nil {
		return render.Usage{}, err
	}
	mem, err := quantity(obj, field, "memory")
	if err != nil {
		return render.Usage{}, err
	}
	return render.Usage{CPU: cpu.MilliValue(), Mem: mem.Value()}, nil
}

func quantity(obj map[string]any, fields ...string) (resource.Quantity, error) {
	s, _, err := unstructured.NestedString(obj, fields...)
	if err != nil {
		return resource.Quantity{}, err
	}
	q, err := resource.ParseQuantity(s)
	if err != nil {
		return resource.Quantity{}, fmt.Errorf("%s %q: %w", fields[len(fields)-1], s, err)
	}
	return q, nil
}
