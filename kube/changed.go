package kube

import "sync"

// changes is the Changed of a watch whose signals come from more than one
// source: the cache of its kind, and the metrics poller that it holds for
// as long as the API serves the metrics. It has room for one signal, as
// every Changed: many are one. It is never closed.
type changes chan struct{}

func newChanges() changes { return make(changes, 1) }

// notify signals, without waiting.
func (ch changes) notify() {
	select {
	case ch <- struct{}{}:
	default:
	}
}

// forward notifies ch of every signal of from, from a goroutine of its
// own, until stop is called or done is closed. stop returns once the
// goroutine has ended, so that nothing is forwarded after it; it may be
// called more than once.
//
// The goroutine is the watch's, not the Cluster's: Close of the Cluster
// does not wait for it, and it ends with done, the Cluster's context,
// for a watch that outlives the Cluster.
func (ch changes) forward(from <-chan struct{}, done <-chan struct{}) (stop func()) {
	quit, ended := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(ended)
		for {
			select {
			case <-from:
				ch.notify()
			case <-quit:
				return
			case <-done:
				return
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() { close(quit) })
		<-ended
	}
}
