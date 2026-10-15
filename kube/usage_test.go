package kube

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
	k := c.findCache(gvr)
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
	if e.c.pollerCount() != 0 || e.c.running(render.Nodes) {
		t.Errorf("%d pollers, the nodes' informer running %v without the API", e.c.pollerCount(), e.c.running(render.Nodes))
	}
}

// The usage watch holds the nodes' cache and their poller while the API
// serves the metrics of nodes, and neither while it does not.
func TestUsageWatchHoldsTheNodesOnlyWhileServed(t *testing.T) {
	api := newMetricsAPI()
	api.set(NodeMetrics, nodeMetrics("n1", "1", "2Gi"))
	e := meteredEnv(t, api, nil, Options{MetricsPoll: time.Hour}, allocNode("n1", "4", "8Gi"))
	u := e.session(t).ClusterUsage()
	defer u.Close()
	if _, ok := u.Current(); ok || e.c.running(render.Nodes) || e.c.pollerCount() != 0 {
		t.Fatalf("without the API: ok %v, nodes' informer %v, %d pollers", ok, e.c.running(render.Nodes), e.c.pollerCount())
	}

	e.disc.setResources(metricsResources())
	e.c.refresh(context.Background())
	eventually(t, "the cluster's usage once the API is served", func() bool {
		got, ok := u.Current()
		return ok && got.Served
	})
	if !e.c.held(render.Nodes) || e.c.pollerOf(NodeMetrics) == nil {
		t.Error("the nodes' cache and poller are not held while the API is served")
	}

	e.disc.setResources(standardResources())
	e.c.refresh(context.Background())
	if got, ok := u.Current(); ok || got.Served {
		t.Errorf("%+v, %v after the API went away", got, ok)
	}
	if e.c.pollerCount() != 0 || !e.c.idle(render.Nodes) {
		t.Error("the nodes' poller or cache is held after the API went away")
	}
}

// Nodes that kosh may not list leave the header n/a, and the log says
// why, once for the watch however often it is read.
func TestUsageWatchLogsNodesItMayNotList(t *testing.T) {
	log := newAuditLog()
	api := newMetricsAPI()
	api.set(NodeMetrics, nodeMetrics("n1", "1", "2Gi"))
	e := newEnv(t, envConfig{
		opts: Options{MetricsPoll: 10 * time.Millisecond, Log: log.logger()}, resources: metricsResources(), real: true,
		before: func(e *env) {
			api.install(e)
			e.dyn.PrependReactor("list", "nodes", func(a k8stesting.Action) (bool, runtime.Object, error) {
				if a.GetResource().Group != "" {
					return false, nil, nil
				}
				return true, nil, apierrors.NewForbidden(a.GetResource().GroupResource(), "", errors.New("RBAC"))
			})
		},
	}, allocNode("n1", "4", "8Gi"))
	u := e.session(t).ClusterUsage()
	defer u.Close()
	const msg = "the nodes cannot be listed: the cluster's CPU and memory are n/a"
	eventually(t, "the line that the nodes cannot be listed", func() bool {
		u.Current()
		return len(log.with(msg)) > 0
	})
	n := api.count(NodeMetrics)
	eventually(t, "more polls", func() bool {
		u.Current()
		return api.count(NodeMetrics) > n+5
	})
	if got, ok := u.Current(); ok || !got.Served {
		t.Errorf("%+v, %v; want served and n/a", got, ok)
	}
	if recs := log.with(msg); len(recs) != 1 || recs[0].Level != slog.LevelWarn || !strings.Contains(recs[0].Attrs["err"].String(), "forbidden") {
		t.Errorf("records %+v, want one WARN with the API's refusal", recs)
	}
}

// A watch opened where there are no nodes kosh can read closes Changed
// when the session ends, as every usage watch does.
func TestUsageWatchWithoutNodesClosesAtTheSessionsEnd(t *testing.T) {
	// The tests' renderers have none for nodes: nodes are not typed.
	e := newEnv(t, envConfig{resources: metricsResources()})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	u := e.c.Session(ctx, fullGrant).ClusterUsage()
	if got, ok := u.Current(); ok || got.Served {
		t.Errorf("%+v, %v without nodes kosh can read", got, ok)
	}
	cancel()
	closedWithin(t, u.Changed(), time.Second, "the session ended")
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

// held reports whether the cache of gvr runs and has a user.
func (c *Cluster) held(gvr schema.GroupVersionResource) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	k := c.findCache(gvr)
	return k != nil && k.users > 0
}

// A second session's watch opens onto the nodes' cache and the poller of
// the first, synced and with a reading, which signal nothing until the
// next poll: it signals at once all the same.
func TestSecondUsageWatchSeesTheReadingAtOnce(t *testing.T) {
	api := newMetricsAPI()
	api.set(NodeMetrics, nodeMetrics("n1", "1", "2Gi"))
	e := meteredEnv(t, api, metricsResources(), Options{MetricsPoll: 5 * time.Second}, allocNode("n1", "4", "8Gi"))
	first := e.session(t).ClusterUsage()
	defer first.Close()
	eventually(t, "the first watch's reading", func() bool {
		_, ok := first.Current()
		return ok
	})
	n := api.count(NodeMetrics)

	second := e.session(t).ClusterUsage()
	defer second.Close()
	select {
	case <-second.Changed():
	case <-time.After(time.Second):
		t.Fatal("the second watch did not signal within a second of its opening")
	}
	if got, ok := second.Current(); !ok || got != (ClusterUsage{CPU: 25, Mem: 25, Served: true}) {
		t.Errorf("the second watch: %+v, %v; want 25%%, 25%%, ok", got, ok)
	}
	if api.count(NodeMetrics) != n {
		t.Error("the second watch's signal waited for a poll")
	}
}

// The same of a table: a watch that takes a poller that already has a
// reading, onto a cache that has synced, signals at once.
func TestSecondTableWatchSeesTheReadingAtOnce(t *testing.T) {
	api := newMetricsAPI()
	api.set(PodMetrics, podMetrics("default", "a", [3]string{"c1", "12m", "34Mi"}))
	e := meteredEnv(t, api, metricsResources(), Options{MetricsPoll: 5 * time.Second}, meteredPod("default", "a", "c1"))
	s := e.session(t)
	q := Query{Resource: e.resource(t, "", "pods")}
	cellWhen(t, watchOf(t, s, q), "default/a", "CPU", "12")
	n := api.count(PodMetrics)

	w := watchOf(t, s, q)
	if got := cellOf(w.Table(time.Now()), "default/a", "CPU"); got != "12" {
		t.Errorf("CPU %q in the second watch's first table, want the shared reading's 12", got)
	}
	select {
	case <-w.Changed():
	case <-time.After(time.Second):
		t.Fatal("the second watch did not signal within a second of taking the poller")
	}
	if api.count(PodMetrics) != n {
		t.Error("the second watch's signal waited for a poll")
	}
}

// What goUntil runs is counted in c.wg: Close waits for it to end, and
// after Close nothing is run.
func TestGoUntilIsWaitedForByClose(t *testing.T) {
	e := newEnv(t, envConfig{})
	var ended atomic.Bool
	e.c.goUntil(func(quit <-chan struct{}) {
		<-e.c.ctx.Done()
		time.Sleep(50 * time.Millisecond)
		ended.Store(true)
	})
	e.c.Close()
	if !ended.Load() {
		t.Error("Close returned before the goroutine ended")
	}

	ran := false
	stop := e.c.goUntil(func(<-chan struct{}) { ran = true })
	stop()
	stop()
	if ran {
		t.Error("run after Close")
	}
}

// The header's arrow compares one reading with the one before, so the
// usage watch signals on a poll and not on a node's change between two
// polls.
func TestUsageWatchSignalsOnPollsNotOnNodes(t *testing.T) {
	api := newMetricsAPI()
	api.set(NodeMetrics, nodeMetrics("n1", "1", "2Gi"))
	e := meteredEnv(t, api, metricsResources(), Options{MetricsPoll: 300 * time.Millisecond}, allocNode("n1", "4", "8Gi"))
	u := e.session(t).ClusterUsage()
	defer u.Close()
	eventually(t, "a reading", func() bool {
		_, ok := u.Current()
		return ok
	})
	// The watch signals once when the nodes' cache first syncs, from a
	// goroutine that may run a moment after Current is first ok: two
	// polls' worth of time lets it land, so that the loop below meets
	// no signal that is neither a poll's nor the node's change.
	time.Sleep(2 * e.c.opts.MetricsPoll)
	nodes := e.dyn.Resource(render.Nodes)
	for attempt := 0; ; attempt++ {
		if attempt == 5 {
			t.Fatal("a poll came between the node's change and the look at Changed five times")
		}
		// Right after a poll, the next one is a whole interval away.
		drain(u.Changed())
		waitSignal(t, u.Changed())
		n := api.count(NodeMetrics)
		label := fmt.Sprintf("v%d", attempt)
		obj, err := nodes.Get(context.Background(), "n1", metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		obj.SetLabels(map[string]string{"test": label})
		if _, err := nodes.Update(context.Background(), obj, metav1.UpdateOptions{}); err != nil {
			t.Fatal(err)
		}
		eventually(t, "the node's change in the cache", func() bool {
			e.c.mu.Lock()
			k := e.c.findCache(render.Nodes)
			e.c.mu.Unlock()
			o, ok := k.get("", "n1")
			return ok && o.(*corev1.Node).Labels["test"] == label
		})
		// A poll's signal comes after its list is counted: a signal seen
		// before a count that has not moved is not a poll's.
		signalled := false
		select {
		case <-u.Changed():
			signalled = true
		default:
		}
		if api.count(NodeMetrics) != n {
			continue
		}
		if signalled {
			t.Fatal("Changed after a node's change between two polls")
		}
		break
	}
	waitSignal(t, u.Changed())
}

// closedWithin waits for ch to be closed, receiving any signal before
// that, and fails the test after d.
func closedWithin(t *testing.T, ch <-chan struct{}, d time.Duration, what string) {
	t.Helper()
	deadline := time.After(d)
	for {
		select {
		case _, open := <-ch:
			if !open {
				return
			}
		case <-deadline:
			t.Fatalf("Changed not closed within %v: %s", d, what)
		}
	}
}

// A session that ended before ClusterUsage was called closes the watch
// at once, from AfterFunc's goroutine, while ClusterUsage is still
// opening it: nothing may signal the closed Changed. Another session
// keeps the nodes' cache and the poller warm, so that the opening is
// quick and the race is close.
func TestClusterUsageOnAnEndedSession(t *testing.T) {
	api := newMetricsAPI()
	api.set(NodeMetrics, nodeMetrics("n1", "1", "2Gi"))
	e := meteredEnv(t, api, metricsResources(), Options{MetricsPoll: time.Hour}, allocNode("n1", "4", "8Gi"))
	warm := e.session(t).ClusterUsage()
	defer warm.Close()
	eventually(t, "the warm watch's reading", func() bool {
		_, ok := warm.Current()
		return ok
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ended := e.c.Session(ctx, fullGrant)
	for range 2000 {
		u := ended.ClusterUsage()
		closedWithin(t, u.Changed(), 5*time.Second, "a watch of a session that had ended")
	}
}
