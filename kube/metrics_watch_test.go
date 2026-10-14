package kube

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/fables-for-robots/kosh/render"
)

// meteredEnv is a Cluster with render's renderers, whose API serves what
// resources says (nil: standardResources, without the metrics API) and
// api's metrics.
func meteredEnv(t *testing.T, api *metricsAPI, resources []*metav1.APIResourceList, opts Options, objs ...runtime.Object) *env {
	t.Helper()
	return newEnv(t, envConfig{opts: opts, resources: resources, real: true, before: api.install}, objs...)
}

// meteredPod is a pod with the containers named, each of which requests
// 100m and 64Mi.
func meteredPod(namespace, name string, containers ...string) *corev1.Pod {
	p := pod(namespace, name, nil)
	for _, c := range containers {
		p.Spec.Containers = append(p.Spec.Containers, corev1.Container{Name: c, Image: c + ":1", Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("64Mi")},
		}})
	}
	return p
}

// allocNode is a node that can give cpu and memory.
func allocNode(name, cpu, memory string) *corev1.Node {
	return &corev1.Node{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Node"},
		ObjectMeta: metav1.ObjectMeta{Name: name, CreationTimestamp: metav1.Now()},
		Status: corev1.NodeStatus{Allocatable: corev1.ResourceList{
			corev1.ResourceCPU: resource.MustParse(cpu), corev1.ResourceMemory: resource.MustParse(memory),
		}},
	}
}

// podsOnly is standardResources with the metrics API of pods, not of
// nodes.
func podsOnly() []*metav1.APIResourceList {
	return append(standardResources(), &metav1.APIResourceList{GroupVersion: "metrics.k8s.io/v1beta1", APIResources: []metav1.APIResource{
		{Name: "pods", Kind: "PodMetrics", Namespaced: true, Verbs: metav1.Verbs{"get", "list"}},
	}})
}

// column returns the index of the column named name, -1 when the table
// has none.
func column(tab Table, name string) int {
	for i, c := range tab.Columns {
		if c.Name == name {
			return i
		}
	}
	return -1
}

// cellOf returns the cell of the column named name in the row of key,
// "namespace/name" ("/name" for a node), and "" when there is none.
func cellOf(tab Table, key, name string) string {
	i := column(tab, name)
	if i < 0 {
		return ""
	}
	for _, r := range tab.Rows {
		if r.Namespace+"/"+r.Name == key && i < len(r.Cells) {
			return r.Cells[i]
		}
	}
	return ""
}

// cellWhen waits until the cell of key in the column name is want, and
// returns the table.
func cellWhen(t *testing.T, w Watch, key, name, want string) Table {
	t.Helper()
	var tab Table
	eventually(t, key+" "+name+" "+want, func() bool {
		tab = w.Table(time.Now())
		return cellOf(tab, key, name) == want
	})
	return tab
}

// whole fails the test when a row has not one cell and one number for
// every column.
func whole(t *testing.T, tab Table) {
	t.Helper()
	for _, r := range tab.Rows {
		if len(r.Cells) != len(tab.Columns) || len(r.Order) != len(tab.Columns) {
			t.Errorf("row %s/%s: %d cells, %d numbers for %d columns", r.Namespace, r.Name, len(r.Cells), len(r.Order), len(tab.Columns))
		}
	}
}

// pollerCount returns how many pollers the Cluster runs.
func (c *Cluster) pollerCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.pollers)
}

func TestPodsTableHasMetricColumnsOnlyWhenServed(t *testing.T) {
	pods := []runtime.Object{meteredPod("default", "a", "c1"), meteredPod("default", "b", "c1")}
	reading := podMetrics("default", "a", [3]string{"c1", "12m", "34Mi"})
	all, _ := render.For(render.Pods)

	t.Run("without the API", func(t *testing.T) {
		api := newMetricsAPI()
		api.set(PodMetrics, reading)
		e := meteredEnv(t, api, nil, Options{MetricsPoll: 20 * time.Millisecond}, pods...)
		w := watchOf(t, e.session(t), Query{Resource: e.resource(t, "", "pods")})
		tab := rowsWhen(t, w, "default/a", "default/b")
		if column(tab, "CPU") >= 0 || column(tab, "%MEM/L") >= 0 {
			t.Errorf("columns %v: metric columns without the API", tab.Columns)
		}
		if want, _ := render.StripMetrics(all.Columns(), nil); !reflect.DeepEqual(tab.Columns, want) {
			t.Errorf("columns %v, want the renderer's without the metric ones", tab.Columns)
		}
		whole(t, tab)
		if n := e.c.pollerCount(); n != 0 || api.count(PodMetrics) != 0 {
			t.Errorf("%d pollers, %d lists without the API", n, api.count(PodMetrics))
		}
	})

	api := newMetricsAPI()
	api.set(PodMetrics, reading)
	e := meteredEnv(t, api, metricsResources(), Options{MetricsPoll: 20 * time.Millisecond}, pods...)
	w := watchOf(t, e.session(t), Query{Resource: e.resource(t, "", "pods")})
	tab := cellWhen(t, w, "default/a", "CPU", "12")
	if !reflect.DeepEqual(tab.Columns, all.Columns()) {
		t.Errorf("columns %v, want the renderer's", tab.Columns)
	}
	for name, at := range map[string]int{"CPU": 6, "%CPU/R": 8, "%CPU/L": 9, "MEM": 10, "%MEM/R": 12, "%MEM/L": 13, "IP": 14} {
		if got := column(tab, name); got != at {
			t.Errorf("%s at %d, want %d", name, got, at)
		}
	}
	whole(t, tab)
	if got := cellOf(tab, "default/a", "%CPU/R"); got != "12" {
		t.Errorf("default/a %%CPU/R %q, want 12", got)
	}
	if got := cellOf(tab, "default/a", "MEM"); got != "34" {
		t.Errorf("default/a MEM %q, want 34", got)
	}
	for _, name := range []string{"CPU", "%CPU/R", "%CPU/L", "MEM", "%MEM/R", "%MEM/L"} {
		if got := cellOf(tab, "default/b", name); got != render.NAValue {
			t.Errorf("default/b, which the reading does not cover: %s %q, want n/a", name, got)
		}
	}
}

func TestContainersTableIsMetered(t *testing.T) {
	api := newMetricsAPI()
	api.set(PodMetrics, podMetrics("default", "a", [3]string{"c1", "12m", "34Mi"}))
	e := meteredEnv(t, api, metricsResources(), Options{MetricsPoll: 20 * time.Millisecond}, meteredPod("default", "a", "c1", "c2"))
	of := ObjectRef{Resource: e.resource(t, "", "pods"), Namespace: "default", Name: "a"}
	w := watchOf(t, e.session(t), Query{Resource: Containers, Of: &of})
	tab := cellWhen(t, w, "default/c1", "CPU", "12")
	if !reflect.DeepEqual(tab.Columns, render.ContainerColumns()) {
		t.Errorf("columns %v, want ContainerColumns", tab.Columns)
	}
	whole(t, tab)
	if got := cellOf(tab, "default/c2", "CPU"); got != render.NAValue {
		t.Errorf("c2, which the reading does not cover: CPU %q, want n/a", got)
	}

	// Without the API, the columns of stage 1.
	e.disc.setResources(standardResources())
	e.c.refresh(context.Background())
	tab = w.Table(time.Now())
	if want, _ := render.StripMetrics(render.ContainerColumns(), nil); !reflect.DeepEqual(tab.Columns, want) {
		t.Errorf("columns %v without the API, want ContainerColumns without the metric ones", tab.Columns)
	}
	whole(t, tab)
}

func TestNodesTableIsMetered(t *testing.T) {
	api := newMetricsAPI()
	api.set(NodeMetrics, nodeMetrics("n1", "1500m", "3Gi"))
	e := meteredEnv(t, api, metricsResources(), Options{MetricsPoll: 20 * time.Millisecond},
		allocNode("n1", "4", "8Gi"), allocNode("n2", "2", "4Gi"))
	w := watchOf(t, e.session(t), Query{Resource: e.resource(t, "", "nodes")})
	tab := cellWhen(t, w, "/n1", "CPU", "1500")
	whole(t, tab)
	for key, want := range map[string]map[string]string{
		"/n1": {"CPU/A": "4000", "%CPU": "37", "MEM": "3072", "MEM/A": "8192", "%MEM": "37"},
		"/n2": {"CPU": render.NAValue, "CPU/A": "2000", "%CPU": render.NAValue, "MEM": render.NAValue, "MEM/A": "4096", "%MEM": render.NAValue},
	} {
		for name, cell := range want {
			if got := cellOf(tab, key, name); got != cell {
				t.Errorf("%s %s %q, want %q", key, name, got, cell)
			}
		}
	}
}

func TestWatchPicksUpTheAPILater(t *testing.T) {
	api := newMetricsAPI()
	api.set(PodMetrics, podMetrics("default", "a", [3]string{"c1", "12m", "34Mi"}))
	e := meteredEnv(t, api, nil, Options{MetricsPoll: 20 * time.Millisecond}, meteredPod("default", "a", "c1"))
	w := watchOf(t, e.session(t), Query{Resource: e.resource(t, "", "pods")})
	tab := rowsWhen(t, w, "default/a")
	if column(tab, "CPU") >= 0 || e.c.pollerCount() != 0 {
		t.Fatalf("columns %v, %d pollers without the API", tab.Columns, e.c.pollerCount())
	}

	e.disc.setResources(metricsResources())
	e.c.refresh(context.Background())
	tab = w.Table(time.Now())
	if column(tab, "CPU") < 0 {
		t.Errorf("columns %v: no CPU once the API is served", tab.Columns)
	}
	if e.c.pollerCount() != 1 || e.c.pollerOf(PodMetrics) == nil {
		t.Errorf("%d pollers, want the pods' one", e.c.pollerCount())
	}
	cellWhen(t, w, "default/a", "CPU", "12")
}

func TestWatchLetsTheAPIGo(t *testing.T) {
	api := newMetricsAPI()
	api.set(PodMetrics, podMetrics("default", "a", [3]string{"c1", "12m", "34Mi"}))
	e := meteredEnv(t, api, metricsResources(), Options{MetricsPoll: 20 * time.Millisecond}, meteredPod("default", "a", "c1"))
	w := watchOf(t, e.session(t), Query{Resource: e.resource(t, "", "pods")})
	cellWhen(t, w, "default/a", "CPU", "12")

	e.disc.setResources(standardResources())
	e.c.refresh(context.Background())
	tab := w.Table(time.Now())
	if column(tab, "CPU") >= 0 {
		t.Errorf("columns %v after the API went away", tab.Columns)
	}
	whole(t, tab)
	if n := e.c.pollerCount(); n != 0 {
		t.Errorf("%d pollers after the only watch let the API go", n)
	}
	stopsListing(t, api, PodMetrics)
}

func TestPodsServedButNotNodes(t *testing.T) {
	api := newMetricsAPI()
	api.set(PodMetrics, podMetrics("default", "a", [3]string{"c1", "12m", "34Mi"}))
	api.set(NodeMetrics, nodeMetrics("n1", "1500m", "3Gi"))
	e := meteredEnv(t, api, podsOnly(), Options{MetricsPoll: 20 * time.Millisecond},
		meteredPod("default", "a", "c1"), allocNode("n1", "4", "8Gi"))
	s := e.session(t)
	pods := watchOf(t, s, Query{Resource: e.resource(t, "", "pods")})
	cellWhen(t, pods, "default/a", "CPU", "12")

	nodes := watchOf(t, s, Query{Resource: e.resource(t, "", "nodes")})
	tab := rowsWhen(t, nodes, "/n1")
	if column(tab, "CPU") >= 0 || column(tab, "CPU/A") >= 0 {
		t.Errorf("nodes' columns %v without their metrics API", tab.Columns)
	}
	whole(t, tab)

	u := s.ClusterUsage()
	defer u.Close()
	if got, ok := u.Current(); got.Served || ok {
		t.Errorf("ClusterUsage %+v, %v without the nodes' metrics API", got, ok)
	}
	if e.c.pollerOf(NodeMetrics) != nil || api.count(NodeMetrics) != 0 {
		t.Error("the nodes' metrics are read without their API")
	}
}

func TestChangedFiresOnAPoll(t *testing.T) {
	api := newMetricsAPI()
	api.set(PodMetrics, podMetrics("default", "a", [3]string{"c1", "12m", "34Mi"}))
	const poll = 50 * time.Millisecond
	e := meteredEnv(t, api, metricsResources(), Options{MetricsPoll: poll}, meteredPod("default", "a", "c1"))
	w := watchOf(t, e.session(t), Query{Resource: e.resource(t, "", "pods")})
	cellWhen(t, w, "default/a", "CPU", "12")

	// No pod changes from here: what Changed receives is the polls'.
	drain(w.Changed())
	n := api.count(PodMetrics)
	start := time.Now()
	for range 3 {
		waitChanged(t, w)
	}
	if took := time.Since(start); took > 40*poll {
		t.Errorf("three signals took %v with a poll every %v", took, poll)
	}
	if api.count(PodMetrics) <= n {
		t.Error("signals without a poll")
	}
}

func TestFailedListShowsNA(t *testing.T) {
	api := newMetricsAPI()
	api.set(PodMetrics, podMetrics("default", "a", [3]string{"c1", "12m", "34Mi"}))
	e := meteredEnv(t, api, metricsResources(), Options{MetricsPoll: 20 * time.Millisecond}, meteredPod("default", "a", "c1"))
	w := watchOf(t, e.session(t), Query{Resource: e.resource(t, "", "pods")})
	cellWhen(t, w, "default/a", "CPU", "12")

	api.setFail(errors.New("metrics-server is away"))
	tab := cellWhen(t, w, "default/a", "CPU", render.NAValue)
	if column(tab, "CPU") < 0 || column(tab, "%MEM/L") < 0 {
		t.Errorf("columns %v after a failed list, want the metric ones", tab.Columns)
	}
	for _, name := range []string{"%CPU/R", "MEM", "%MEM/R"} {
		if got := cellOf(tab, "default/a", name); got != render.NAValue {
			t.Errorf("%s %q after a failed list, want n/a", name, got)
		}
	}
	whole(t, tab)
}
