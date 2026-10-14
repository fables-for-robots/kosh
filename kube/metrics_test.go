package kube

import (
	"context"
	"errors"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stesting "k8s.io/client-go/testing"

	"github.com/fables-for-robots/kosh/render"
)

// metricsResources is standardResources with the metrics API, as
// metrics-server serves it.
func metricsResources() []*metav1.APIResourceList {
	return append(standardResources(), &metav1.APIResourceList{GroupVersion: "metrics.k8s.io/v1beta1", APIResources: []metav1.APIResource{
		{Name: "pods", Kind: "PodMetrics", Namespaced: true, Verbs: metav1.Verbs{"get", "list"}},
		{Name: "nodes", Kind: "NodeMetrics", Verbs: metav1.Verbs{"get", "list"}},
	}})
}

// metricsAPI serves the lists of the metrics API in the dynamic fake,
// and counts them.
type metricsAPI struct {
	mu    sync.Mutex
	lists map[schema.GroupVersionResource]int
	items map[schema.GroupVersionResource][]unstructured.Unstructured
	fail  error
}

func newMetricsAPI() *metricsAPI {
	return &metricsAPI{
		lists: map[schema.GroupVersionResource]int{},
		items: map[schema.GroupVersionResource][]unstructured.Unstructured{},
	}
}

// install serves the lists in e's dynamic fake: for envConfig.before.
func (m *metricsAPI) install(e *env) {
	react := func(a k8stesting.Action) (bool, runtime.Object, error) {
		gvr := a.GetResource()
		kind := map[schema.GroupVersionResource]string{PodMetrics: "PodMetricsList", NodeMetrics: "NodeMetricsList"}[gvr]
		if kind == "" {
			return false, nil, nil
		}
		m.mu.Lock()
		defer m.mu.Unlock()
		m.lists[gvr]++
		if m.fail != nil {
			return true, nil, m.fail
		}
		list := &unstructured.UnstructuredList{Object: map[string]any{"kind": kind, "apiVersion": "metrics.k8s.io/v1beta1"}}
		for _, it := range m.items[gvr] {
			list.Items = append(list.Items, *it.DeepCopy())
		}
		return true, list, nil
	}
	e.dyn.PrependReactor("list", "pods", react)
	e.dyn.PrependReactor("list", "nodes", react)
}

// count returns how often gvr was listed.
func (m *metricsAPI) count(gvr schema.GroupVersionResource) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lists[gvr]
}

// setFail makes every list fail with err, or work again with nil.
func (m *metricsAPI) setFail(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.fail = err
}

// set replaces the items of gvr's lists.
func (m *metricsAPI) set(gvr schema.GroupVersionResource, items ...unstructured.Unstructured) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.items[gvr] = items
}

// podMetrics is a PodMetrics item; containers are name, cpu, memory.
func podMetrics(namespace, name string, containers ...[3]string) unstructured.Unstructured {
	cs := []any{}
	for _, c := range containers {
		cs = append(cs, map[string]any{"name": c[0], "usage": map[string]any{"cpu": c[1], "memory": c[2]}})
	}
	meta := map[string]any{}
	if namespace != "" {
		meta["namespace"] = namespace
	}
	if name != "" {
		meta["name"] = name
	}
	return unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "metrics.k8s.io/v1beta1", "kind": "PodMetrics",
		"metadata": meta, "timestamp": "2026-10-14T10:00:00Z", "window": "15s",
		"containers": cs,
	}}
}

// nodeMetrics is a NodeMetrics item.
func nodeMetrics(name, cpu, memory string) unstructured.Unstructured {
	return unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "metrics.k8s.io/v1beta1", "kind": "NodeMetrics",
		"metadata": map[string]any{"name": name}, "timestamp": "2026-10-14T10:00:00Z", "window": "15s",
		"usage": map[string]any{"cpu": cpu, "memory": memory},
	}}
}

// metricsEnv is a Cluster whose API serves api's metrics.
func metricsEnv(t *testing.T, api *metricsAPI, opts Options) *env {
	t.Helper()
	return newEnv(t, envConfig{opts: opts, resources: metricsResources(), before: api.install})
}

// acquired acquires the poller of gvr and fails the test on an error.
func acquired(t *testing.T, c *Cluster, gvr schema.GroupVersionResource) *metricsPoller {
	t.Helper()
	p, err := c.acquireMetrics(gvr)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// stopsListing fails the test when gvr is listed again after a while
// for settling.
func stopsListing(t *testing.T, api *metricsAPI, gvr schema.GroupVersionResource) {
	t.Helper()
	time.Sleep(200 * time.Millisecond)
	n := api.count(gvr)
	time.Sleep(200 * time.Millisecond)
	if got := api.count(gvr); got != n {
		t.Errorf("%s listed %d times, then %d: still polling", gvr.Resource, n, got)
	}
}

func (c *Cluster) pollerOf(gvr schema.GroupVersionResource) *metricsPoller {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.pollers[gvr]
}

func TestMetricsServedFollowsDiscovery(t *testing.T) {
	e := newEnv(t, envConfig{})
	if e.c.metricsServed(PodMetrics) || e.c.metricsServed(NodeMetrics) {
		t.Error("served without the group")
	}
	e.disc.setResources(metricsResources())
	e.c.refresh(context.Background())
	if !e.c.metricsServed(PodMetrics) || !e.c.metricsServed(NodeMetrics) {
		t.Error("not served with the group")
	}
	e.disc.setResources(standardResources())
	e.c.refresh(context.Background())
	if e.c.metricsServed(PodMetrics) || e.c.metricsServed(NodeMetrics) {
		t.Error("served after the group went away")
	}
}

func TestPollerListsAtOnceAndEveryInterval(t *testing.T) {
	t.Run("at once", func(t *testing.T) {
		api := newMetricsAPI()
		e := metricsEnv(t, api, Options{MetricsPoll: time.Hour})
		p := acquired(t, e.c, PodMetrics)
		defer e.c.releaseMetrics(p)
		eventually(t, "the first list", func() bool { return api.count(PodMetrics) == 1 })
	})

	api := newMetricsAPI()
	api.set(PodMetrics,
		podMetrics("default", "a", [3]string{"c1", "12m", "34Mi"}),
		podMetrics("kube-system", "b", [3]string{"c1", "1", "1Gi"}, [3]string{"c2", "250m", "0"}))
	e := metricsEnv(t, api, Options{MetricsPoll: 50 * time.Millisecond})
	p := acquired(t, e.c, PodMetrics)
	eventually(t, "three lists", func() bool { return api.count(PodMetrics) >= 3 })

	r, err := p.current()
	if err != nil {
		t.Fatal(err)
	}
	want := reading{
		"default/a":     {"c1": {CPU: 12, Mem: 34 << 20}},
		"kube-system/b": {"c1": {CPU: 1000, Mem: 1 << 30}, "c2": {CPU: 250, Mem: 0}},
	}
	if !reflect.DeepEqual(r, want) {
		t.Errorf("reading %v, want %v", r, want)
	}
	if api.count(NodeMetrics) != 0 {
		t.Error("the nodes were listed for the pods")
	}

	e.c.releaseMetrics(p)
	if e.c.pollerOf(PodMetrics) != nil {
		t.Error("the poller is kept without a user")
	}
	stopsListing(t, api, PodMetrics)
}

func TestPollerIsShared(t *testing.T) {
	api := newMetricsAPI()
	e := metricsEnv(t, api, Options{MetricsPoll: 50 * time.Millisecond})
	start := time.Now()
	p1 := acquired(t, e.c, PodMetrics)
	p2 := acquired(t, e.c, PodMetrics)
	if p1 != p2 {
		t.Fatal("two pollers of the same kind")
	}
	time.Sleep(500 * time.Millisecond)
	// One goroutine lists at once and then at most once a tick.
	n := api.count(PodMetrics)
	if most := 1 + int(time.Since(start)/(50*time.Millisecond)); n > most {
		t.Errorf("%d lists, one poller makes at most %d", n, most)
	}

	e.c.releaseMetrics(p1)
	n = api.count(PodMetrics)
	eventually(t, "polling with one user left", func() bool { return api.count(PodMetrics) >= n+2 })

	e.c.releaseMetrics(p2)
	stopsListing(t, api, PodMetrics)

	p3 := acquired(t, e.c, PodMetrics)
	defer e.c.releaseMetrics(p3)
	if p3 == p1 {
		t.Error("the stopped poller was given again")
	}
}

func TestParseReadingSkipsBadItems(t *testing.T) {
	list := &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
		podMetrics("default", "", [3]string{"c1", "1m", "1Mi"}),
		podMetrics("default", "bad", [3]string{"c1", "abc", "1Mi"}),
		podMetrics("default", "good", [3]string{"c1", "12m", "34Mi"}),
	}}
	var logged []string
	r := parseReading(PodMetrics, list, func(msg string, args ...any) { logged = append(logged, msg) })
	want := reading{"default/good": {"c1": {CPU: 12, Mem: 34 << 20}}}
	if !reflect.DeepEqual(r, want) {
		t.Errorf("reading %v, want %v", r, want)
	}
	if len(logged) != 1 || !strings.Contains(logged[0], "does not parse") {
		t.Errorf("logged %q, want one line that the items do not parse", logged)
	}

	// A poller logs it once, however many lists bring it.
	log := newAuditLog()
	api := newMetricsAPI()
	api.set(PodMetrics, list.Items...)
	e := metricsEnv(t, api, Options{MetricsPoll: 20 * time.Millisecond, Log: log.logger()})
	p := acquired(t, e.c, PodMetrics)
	defer e.c.releaseMetrics(p)
	eventually(t, "three lists", func() bool { return api.count(PodMetrics) >= 3 })
	if recs := log.with("a metrics item does not parse and is left out"); len(recs) != 1 {
		t.Errorf("records %+v, want one", recs)
	}
}

func TestParseReadingOfNodes(t *testing.T) {
	list := &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
		nodeMetrics("n1", "1500m", "2Gi"),
		nodeMetrics("n2", "", "2Gi"),
	}}
	var logged int
	r := parseReading(NodeMetrics, list, func(string, ...any) { logged++ })
	want := reading{"n1": {"": {CPU: 1500, Mem: 2 << 30}}}
	if !reflect.DeepEqual(r, want) {
		t.Errorf("reading %v, want %v", r, want)
	}
	if logged != 1 {
		t.Errorf("logged %d times, want once for the node without a CPU", logged)
	}
	if r := parseReading(NodeMetrics, &unstructured.UnstructuredList{}, nil); r == nil || len(r) != 0 {
		t.Errorf("an empty list reads %#v, want an empty reading", r)
	}
}

func TestPollerRecordsAFailedList(t *testing.T) {
	log := newAuditLog()
	api := newMetricsAPI()
	api.set(PodMetrics, podMetrics("default", "a", [3]string{"c1", "12m", "34Mi"}))
	e := metricsEnv(t, api, Options{MetricsPoll: 20 * time.Millisecond, Log: log.logger()})
	p := acquired(t, e.c, PodMetrics)
	defer e.c.releaseMetrics(p)
	eventually(t, "a reading", func() bool {
		r, _ := p.current()
		return r != nil
	})
	good, _ := p.current()

	api.setFail(apierrors.NewServiceUnavailable("metrics-server is away"))
	n := api.count(PodMetrics)
	eventually(t, "three failed lists", func() bool { return api.count(PodMetrics) >= n+4 })
	r, err := p.current()
	if !apierrors.IsServiceUnavailable(err) {
		t.Errorf("err %v, want the list's", err)
	}
	if !reflect.DeepEqual(r, good) {
		t.Errorf("reading %v after a failed list, want the last good one %v", r, good)
	}
	recs := log.with("metrics could not be read")
	if len(recs) != 1 {
		t.Fatalf("records %+v, want one", recs)
	}
	if recs[0].Level != slog.LevelWarn || recs[0].Attrs["kind"].String() != "pods.metrics.k8s.io" || recs[0].Attrs["err"].String() == "" {
		t.Errorf("record %+v", recs[0])
	}

	api.setFail(nil)
	eventually(t, "a list that works", func() bool {
		_, err := p.current()
		return err == nil
	})
	eventually(t, "the line that it works again", func() bool { return len(log.with("metrics can be read again")) == 1 })
	if rec := log.with("metrics can be read again")[0]; rec.Level != slog.LevelInfo || rec.Attrs["kind"].String() != "pods.metrics.k8s.io" {
		t.Errorf("record %+v", rec)
	}

	// Failing again is logged again.
	api.setFail(errors.New("broken"))
	eventually(t, "the second warning", func() bool { return len(log.with("metrics could not be read")) == 2 })
}

func TestPollerSignals(t *testing.T) {
	api := newMetricsAPI()
	e := metricsEnv(t, api, Options{MetricsPoll: 50 * time.Millisecond})
	p := acquired(t, e.c, PodMetrics)
	defer e.c.releaseMetrics(p)
	ch, stop := p.subscribe()
	receive := func(what string) {
		t.Helper()
		select {
		case <-ch:
		case <-time.After(5 * time.Second):
			t.Fatalf("no signal within 5s %s", what)
		}
	}
	for range 3 {
		receive("after a list")
	}

	api.setFail(errors.New("broken"))
	n := api.count(PodMetrics)
	eventually(t, "a failed list", func() bool { return api.count(PodMetrics) > n+1 })
	drain(ch)
	receive("after a failed list")

	stop()
	drain(ch)
	time.Sleep(200 * time.Millisecond)
	select {
	case <-ch:
		t.Error("a signal after the subscription ended")
	default:
	}
}

func TestCloseStopsPollers(t *testing.T) {
	api := newMetricsAPI()
	e := metricsEnv(t, api, Options{MetricsPoll: 20 * time.Millisecond})
	pods := acquired(t, e.c, PodMetrics)
	nodes := acquired(t, e.c, NodeMetrics)
	eventually(t, "both listed", func() bool { return api.count(PodMetrics) > 0 && api.count(NodeMetrics) > 0 })

	e.c.Close()
	// Close waited for the pollers' goroutines: nothing lists after it.
	np, nn := api.count(PodMetrics), api.count(NodeMetrics)
	time.Sleep(200 * time.Millisecond)
	if api.count(PodMetrics) != np || api.count(NodeMetrics) != nn {
		t.Error("listed after Close")
	}
	if _, err := e.c.acquireMetrics(PodMetrics); !errors.Is(err, errClosed) {
		t.Errorf("acquire after Close: %v, want errClosed", err)
	}
	// A watch that closes after the Cluster releases what it holds.
	e.c.releaseMetrics(pods)
	e.c.releaseMetrics(nodes)
}

func TestAcquireMetricsOfAnotherKind(t *testing.T) {
	e := newEnv(t, envConfig{})
	if _, err := e.c.acquireMetrics(render.Pods); err == nil {
		t.Error("a poller of pods, not of their metrics")
	}
}
