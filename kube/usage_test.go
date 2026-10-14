package kube

import (
	"errors"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stesting "k8s.io/client-go/testing"

	"github.com/fables-for-robots/kosh/render"
)

// idle reports whether the cache of gvr is kept without a user, its idle
// timer set.
func (c *Cluster) idle(gvr schema.GroupVersionResource) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	k := c.caches[gvr]
	return k != nil && k.users == 0 && k.idle != nil
}

func TestClusterUsage(t *testing.T) {
	api := newMetricsAPI()
	api.set(NodeMetrics, nodeMetrics("n1", "1500m", "3Gi"), nodeMetrics("n2", "1500m", "3Gi"))
	// The first list of the nodes' metrics waits for the test, which
	// looks at the watch before it.
	entered, gate := make(chan struct{}), make(chan struct{})
	e := newEnv(t, envConfig{
		opts: Options{MetricsPoll: 20 * time.Millisecond}, resources: metricsResources(), real: true,
		before: func(e *env) {
			api.install(e)
			first := true
			e.dyn.PrependReactor("list", "nodes", func(a k8stesting.Action) (bool, runtime.Object, error) {
				if a.GetResource() == NodeMetrics && first {
					first = false
					close(entered)
					<-gate
				}
				return false, nil, nil
			})
		},
	}, allocNode("n1", "4", "8Gi"), allocNode("n2", "2", "4Gi"))
	// Registered after newEnv's, so run before its Close, which waits for
	// the poller that the gate holds.
	opened := false
	t.Cleanup(func() {
		if !opened {
			close(gate)
		}
	})

	u := e.session(t).ClusterUsage()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the nodes' metrics were not listed within 5s")
	}
	if got, ok := u.Current(); ok || !got.Served {
		t.Errorf("before the first list: %+v, %v; want served and not ok", got, ok)
	}
	opened = true
	close(gate)

	eventually(t, "the cluster's usage", func() bool {
		got, ok := u.Current()
		return ok && got == ClusterUsage{CPU: 50, Mem: 50, Served: true}
	})

	drain(u.Changed())
	api.setFail(errors.New("metrics-server is away"))
	select {
	case <-u.Changed():
	case <-time.After(5 * time.Second):
		t.Fatal("no change within 5s of a failed list")
	}
	eventually(t, "not ok after a failed list", func() bool {
		got, ok := u.Current()
		return !ok && got.Served
	})

	u.Close()
	u.Close()
	if e.c.pollerOf(NodeMetrics) != nil {
		t.Error("the nodes' poller is kept after Close")
	}
	if !e.c.idle(render.Nodes) {
		t.Error("the nodes' cache is not idle after Close")
	}
}

func TestClusterUsageWithoutTheAPI(t *testing.T) {
	api := newMetricsAPI()
	e := meteredEnv(t, api, nil, Options{MetricsPoll: 20 * time.Millisecond}, allocNode("n1", "4", "8Gi"))
	u := e.session(t).ClusterUsage()
	defer u.Close()
	if got, ok := u.Current(); ok || got.Served {
		t.Errorf("%+v, %v without the API", got, ok)
	}
	if e.c.pollerCount() != 0 {
		t.Error("a poller without the API")
	}
}

func TestCloseWithWatchesOpen(t *testing.T) {
	api := newMetricsAPI()
	api.set(PodMetrics, podMetrics("default", "a", [3]string{"c1", "12m", "34Mi"}))
	api.set(NodeMetrics, nodeMetrics("n1", "1500m", "3Gi"))
	e := meteredEnv(t, api, metricsResources(), Options{MetricsPoll: 10 * time.Millisecond},
		meteredPod("default", "a", "c1"), allocNode("n1", "4", "8Gi"))
	s := e.session(t)
	pods := watchOf(t, s, Query{Resource: e.resource(t, "", "pods")})
	nodes := watchOf(t, s, Query{Resource: e.resource(t, "", "nodes")})
	u := s.ClusterUsage()
	cellWhen(t, pods, "default/a", "CPU", "12")
	cellWhen(t, nodes, "/n1", "CPU", "1500")
	eventually(t, "the cluster's usage", func() bool {
		_, ok := u.Current()
		return ok
	})

	closed := make(chan struct{})
	go func() {
		e.c.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("Close did not return within a second with watches open")
	}

	// The watches close after the Cluster, as a session that ends late
	// closes them.
	done := make(chan struct{})
	go func() {
		pods.Close()
		nodes.Close()
		u.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("the watches did not close within a second after the Cluster")
	}
}
