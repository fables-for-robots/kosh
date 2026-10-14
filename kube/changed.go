package kube

import "sync"

// changes is the Changed of a watch whose signals come from more than one
// source: the cache of its kind, and the metrics poller that it holds for
// as long as the API serves the metrics. It has room for one signal, as
// every Changed: many are one. A watch of a table never closes it; the
// usage watch closes it at Close, once nothing can notify it any more.
type changes chan struct{}

func newChanges() changes { return make(changes, 1) }

// notify signals, without waiting.
func (ch changes) notify() {
	select {
	case ch <- struct{}{}:
	default:
	}
}

// forward notifies ch of every signal of from, from a goroutine that c
// runs, until stop is called or c is closed. stop returns once the
// goroutine has ended, so that nothing is forwarded after it; it may be
// called more than once. Once c is closed nothing is forwarded, and no
// goroutine is started.
func (ch changes) forward(c *Cluster, from <-chan struct{}) (stop func()) {
	return c.goUntil(func(quit <-chan struct{}) {
		for {
			select {
			case <-from:
				ch.notify()
			case <-quit:
				return
			case <-c.ctx.Done():
				return
			}
		}
	})
}

// goUntil runs f in a goroutine counted in c.wg, as every goroutine the
// Cluster runs is, so that Close waits for it; f returns when quit is
// closed or c's context is done. The returned function closes quit and
// waits for f to have returned; it may be called more than once. Once c
// is closed f is not run, and the function returns at once.
func (c *Cluster) goUntil(f func(quit <-chan struct{})) (stop func()) {
	quit, ended := make(chan struct{}), make(chan struct{})
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		close(ended)
	} else {
		c.wg.Add(1)
		c.mu.Unlock()
		go func() {
			defer c.wg.Done()
			defer close(ended)
			f(quit)
		}()
	}
	var once sync.Once
	return func() {
		once.Do(func() { close(quit) })
		<-ended
	}
}
