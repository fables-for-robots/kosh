package kube

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/duration"
	fakediscovery "k8s.io/client-go/discovery/fake"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	metadatafake "k8s.io/client-go/metadata/fake"

	"github.com/fables-for-robots/kosh/keys"
	"github.com/fables-for-robots/kosh/render"
)

// failRender is a label that makes the test renderers fail.
const failRender = "test.kosh/fail-render"

// testRenderer renders pods or jobs with the columns NAMESPACE, NAME and
// AGE, and remembers what it was given. An object with the label
// failRender is an error.
type testRenderer struct {
	gvr schema.GroupVersionResource

	mu   sync.Mutex
	seen []runtime.Object
}

func (r *testRenderer) Columns() []render.Column {
	return []render.Column{{Name: "NAMESPACE"}, {Name: "NAME"}, {Name: "AGE", Numeric: true}}
}

func (r *testRenderer) Row(obj runtime.Object, now time.Time) (render.Row, error) {
	r.mu.Lock()
	r.seen = append(r.seen, obj)
	r.mu.Unlock()
	switch r.gvr {
	case render.Pods:
		if _, ok := obj.(*corev1.Pod); !ok {
			return render.Row{}, fmt.Errorf("not a pod: %T", obj)
		}
	case render.Jobs:
		if _, ok := obj.(*batchv1.Job); !ok {
			return render.Row{}, fmt.Errorf("not a job: %T", obj)
		}
	}
	m, err := meta.Accessor(obj)
	if err != nil {
		return render.Row{}, err
	}
	if m.GetLabels()[failRender] != "" {
		return render.Row{}, fmt.Errorf("%s does not render", m.GetName())
	}
	age := now.Sub(m.GetCreationTimestamp().Time)
	return render.Row{
		Namespace: m.GetNamespace(),
		Name:      m.GetName(),
		Cells:     []string{m.GetNamespace(), m.GetName(), duration.HumanDuration(age)},
		Order:     []float64{0, 0, age.Seconds()},
		Labels:    m.GetLabels(),
	}, nil
}

func (r *testRenderer) given() []runtime.Object {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]runtime.Object(nil), r.seen...)
}

// testRenderers are the renderers of the tests: pods and jobs, and
// whatever a test adds before the Cluster is made.
type testRenderers struct {
	pods, jobs *testRenderer
	extra      map[schema.GroupVersionResource]render.Renderer
}

func newTestRenderers() *testRenderers {
	return &testRenderers{
		pods:  &testRenderer{gvr: render.Pods},
		jobs:  &testRenderer{gvr: render.Jobs},
		extra: map[schema.GroupVersionResource]render.Renderer{},
	}
}

func (t *testRenderers) For(gvr schema.GroupVersionResource) (render.Renderer, bool) {
	switch gvr {
	case render.Pods:
		return t.pods, true
	case render.Jobs:
		return t.jobs, true
	}
	r, ok := t.extra[gvr]
	return r, ok
}

// testZero is render.Zero for the kinds of the tests, whose renderers
// are not registered with render.
func testZero(gvr schema.GroupVersionResource) (runtime.Object, bool) {
	switch gvr {
	case render.Pods:
		return &corev1.Pod{}, true
	case render.Jobs:
		return &batchv1.Job{}, true
	}
	return nil, false
}

// testDiscovery is client-go's fake discovery, whose resources a test
// may change while the Cluster runs, which counts how often the groups
// were read, which fails to discover the group versions in fail, and
// which fails altogether while down.
type testDiscovery struct {
	*fakediscovery.FakeDiscovery

	mu     sync.Mutex
	groups int
	fail   map[string]bool
	down   bool
}

func (d *testDiscovery) ServerGroups() (*metav1.APIGroupList, error) {
	return d.ServerGroupsWithContext(context.Background())
}

func (d *testDiscovery) ServerGroupsWithContext(ctx context.Context) (*metav1.APIGroupList, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.groups++
	if d.down {
		return nil, errors.New("the API is down")
	}
	return d.FakeDiscovery.ServerGroupsWithContext(ctx)
}

func (d *testDiscovery) ServerResourcesForGroupVersion(gv string) (*metav1.APIResourceList, error) {
	return d.ServerResourcesForGroupVersionWithContext(context.Background(), gv)
}

func (d *testDiscovery) ServerResourcesForGroupVersionWithContext(ctx context.Context, gv string) (*metav1.APIResourceList, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.fail[gv] {
		return nil, fmt.Errorf("discovery of %s failed", gv)
	}
	return d.FakeDiscovery.ServerResourcesForGroupVersionWithContext(ctx, gv)
}

// setResources replaces what the API serves.
func (d *testDiscovery) setResources(rr []*metav1.APIResourceList) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.Resources = rr
}

// setDown makes every discovery fail, or work again.
func (d *testDiscovery) setDown(down bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.down = down
}

// addResources adds to what the API serves.
func (d *testDiscovery) addResources(rr ...*metav1.APIResourceList) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.Resources = append(d.Resources, rr...)
}

// discoveries returns how often the groups were read: each discovery
// reads them a fixed number of times.
func (d *testDiscovery) discoveries() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.groups
}

var allVerbs = metav1.Verbs{"create", "delete", "get", "list", "patch", "update", "watch"}

// standardResources is what the fake API serves unless a test says
// otherwise.
func standardResources() []*metav1.APIResourceList {
	return []*metav1.APIResourceList{
		{GroupVersion: "v1", APIResources: []metav1.APIResource{
			{Name: "pods", SingularName: "pod", Namespaced: true, Kind: "Pod", ShortNames: []string{"po"}, Verbs: allVerbs},
			{Name: "pods/log", SingularName: "", Namespaced: true, Kind: "Pod", Verbs: metav1.Verbs{"get"}},
			{Name: "services", SingularName: "service", Namespaced: true, Kind: "Service", ShortNames: []string{"svc"}, Verbs: allVerbs},
			{Name: "nodes", SingularName: "node", Kind: "Node", ShortNames: []string{"no"}, Verbs: allVerbs},
			{Name: "namespaces", SingularName: "namespace", Kind: "Namespace", ShortNames: []string{"ns"}, Verbs: allVerbs},
			{Name: "configmaps", SingularName: "configmap", Namespaced: true, Kind: "ConfigMap", ShortNames: []string{"cm"}, Verbs: allVerbs},
			{Name: "secrets", SingularName: "secret", Namespaced: true, Kind: "Secret", Verbs: allVerbs},
			{Name: "events", SingularName: "event", Namespaced: true, Kind: "Event", ShortNames: []string{"ev"}, Verbs: allVerbs},
			{Name: "bindings", SingularName: "binding", Namespaced: true, Kind: "Binding", Verbs: metav1.Verbs{"create"}},
		}},
		{GroupVersion: "apps/v1", APIResources: []metav1.APIResource{
			{Name: "deployments", SingularName: "deployment", Namespaced: true, Kind: "Deployment", ShortNames: []string{"deploy"}, Verbs: allVerbs},
			{Name: "deployments/scale", Namespaced: true, Kind: "Scale", Verbs: metav1.Verbs{"get", "patch", "update"}},
			{Name: "replicasets", SingularName: "replicaset", Namespaced: true, Kind: "ReplicaSet", ShortNames: []string{"rs"}, Verbs: allVerbs},
			{Name: "statefulsets", SingularName: "statefulset", Namespaced: true, Kind: "StatefulSet", ShortNames: []string{"sts"}, Verbs: allVerbs},
			{Name: "daemonsets", SingularName: "daemonset", Namespaced: true, Kind: "DaemonSet", ShortNames: []string{"ds"}, Verbs: allVerbs},
		}},
		{GroupVersion: "batch/v1", APIResources: []metav1.APIResource{
			{Name: "jobs", SingularName: "job", Namespaced: true, Kind: "Job", Verbs: allVerbs},
			{Name: "cronjobs", SingularName: "cronjob", Namespaced: true, Kind: "CronJob", ShortNames: []string{"cj"}, Verbs: allVerbs},
		}},
		{GroupVersion: "events.k8s.io/v1", APIResources: []metav1.APIResource{
			{Name: "events", SingularName: "event", Namespaced: true, Kind: "Event", ShortNames: []string{"ev"}, Verbs: allVerbs},
		}},
		{GroupVersion: "autoscaling/v1", APIResources: []metav1.APIResource{
			{Name: "horizontalpodautoscalers", SingularName: "horizontalpodautoscaler", Namespaced: true, Kind: "HorizontalPodAutoscaler", ShortNames: []string{"hpa"}, Verbs: allVerbs},
		}},
		{GroupVersion: "rbac.authorization.k8s.io/v1", APIResources: []metav1.APIResource{
			{Name: "roles", SingularName: "role", Namespaced: true, Kind: "Role", Verbs: allVerbs},
			{Name: "clusterroles", SingularName: "clusterrole", Kind: "ClusterRole", Verbs: allVerbs},
		}},
		{GroupVersion: "example.com/v1", APIResources: []metav1.APIResource{
			{Name: "widgets", SingularName: "widget", Namespaced: true, Kind: "Widget", ShortNames: []string{"wd"}, Verbs: allVerbs},
		}},
	}
}

// testScheme knows the kinds the fake dynamic client holds.
func testScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{corev1.AddToScheme, appsv1.AddToScheme, batchv1.AddToScheme, rbacv1.AddToScheme} {
		if err := add(s); err != nil {
			panic(err)
		}
	}
	return s
}

// env is a Cluster on client-go's fakes, and the fakes.
type env struct {
	c         *Cluster
	dyn       *dynamicfake.FakeDynamicClient
	typed     *fake.Clientset
	disc      *testDiscovery
	renderers *testRenderers
}

// envConfig says how newEnv makes the Cluster.
type envConfig struct {
	opts      Options
	resources []*metav1.APIResourceList // nil: standardResources
	failGroup string                    // a group version whose discovery fails
	renderers func(*testRenderers)      // adds to the renderers
	before    func(*env)                // runs before the Cluster is made: reactors
}

// newEnv makes a Cluster on fakes that hold objs, with the test
// renderers, and closes it when the test ends.
func newEnv(t *testing.T, cfg envConfig, objs ...runtime.Object) *env {
	t.Helper()
	e := &env{
		dyn:       dynamicfake.NewSimpleDynamicClient(testScheme(), objs...),
		typed:     fake.NewSimpleClientset(objs...),
		renderers: newTestRenderers(),
	}
	e.disc = &testDiscovery{FakeDiscovery: e.typed.Discovery().(*fakediscovery.FakeDiscovery), fail: map[string]bool{}}
	rr := cfg.resources
	if rr == nil {
		rr = standardResources()
	}
	e.disc.setResources(rr)
	if cfg.failGroup != "" {
		e.disc.fail[cfg.failGroup] = true
	}
	if cfg.renderers != nil {
		cfg.renderers(e.renderers)
	}
	if cfg.before != nil {
		cfg.before(e)
	}
	opts := cfg.opts
	if opts.Log == nil {
		opts.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	opts.Renderers = e.renderers.For
	e.c = newCluster(nil, e.typed, e.dyn, e.disc, nil, opts)
	e.c.meta = metadataFake(objs)
	e.c.zeroOf = testZero
	t.Cleanup(e.c.Close)
	return e
}

var (
	fullGrant     = keys.Grant{Name: "full", Fingerprint: "SHA256:full"}
	readOnlyGrant = keys.Grant{Name: "ro", Fingerprint: "SHA256:ro", ReadOnly: true}
)

// session returns a session of the Cluster with a full grant, which ends
// with the test.
func (e *env) session(t *testing.T) *session {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return e.c.Session(ctx, fullGrant).(*session)
}

// resource returns the resource of the Cluster with the plural name and
// group.
func (e *env) resource(t *testing.T, group, plural string) Resource {
	t.Helper()
	for _, r := range e.c.resourceList() {
		if r.Is(group, plural) {
			return r
		}
	}
	t.Fatalf("no resource %s.%s", plural, group)
	return Resource{}
}

// eventually fails the test when cond is not true within 5 seconds.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("not within 5s: %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// waitChanged waits for w's signal, at most 5 seconds.
func waitChanged(t *testing.T, w Watch) {
	t.Helper()
	select {
	case <-w.Changed():
	case <-time.After(5 * time.Second):
		t.Fatal("no change within 5s")
	}
}

// names returns the "namespace/name" of the rows.
func names(rows []render.Row) map[string]bool {
	out := map[string]bool{}
	for _, r := range rows {
		out[r.Namespace+"/"+r.Name] = true
	}
	return out
}

func pod(namespace, name string, labels map[string]string) *corev1.Pod {
	return &corev1.Pod{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"},
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name, Labels: labels, CreationTimestamp: metav1.Now()},
	}
}

// toUnstructured converts a typed object for the dynamic client.
func toUnstructured(t *testing.T, obj runtime.Object) *unstructured.Unstructured {
	t.Helper()
	m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
	if err != nil {
		t.Fatal(err)
	}
	return &unstructured.Unstructured{Object: m}
}

// running reports whether the informer of gvr runs.
func (c *Cluster) running(gvr schema.GroupVersionResource) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.caches[gvr] != nil
}

// metadataFake returns a fake metadata client that holds the metadata
// of objs.
func metadataFake(objs []runtime.Object) *metadatafake.FakeMetadataClient {
	scheme := testScheme()
	metaScheme := metadatafake.NewTestScheme()
	var metas []runtime.Object
	for _, o := range objs {
		gvk := o.GetObjectKind().GroupVersionKind()
		if gvk.Empty() {
			gvks, _, err := scheme.ObjectKinds(o)
			if err != nil {
				panic(err)
			}
			gvk = gvks[0]
		}
		if !metaScheme.Recognizes(gvk) {
			metaScheme.AddKnownTypeWithName(gvk, &metav1.PartialObjectMetadata{})
			metaScheme.AddKnownTypeWithName(gvk.GroupVersion().WithKind(gvk.Kind+"List"), &metav1.PartialObjectMetadataList{})
		}
		m, err := meta.Accessor(o)
		if err != nil {
			panic(err)
		}
		metas = append(metas, &metav1.PartialObjectMetadata{
			TypeMeta: metav1.TypeMeta{APIVersion: gvk.GroupVersion().String(), Kind: gvk.Kind},
			ObjectMeta: metav1.ObjectMeta{
				Namespace: m.GetNamespace(), Name: m.GetName(), UID: m.GetUID(),
				ResourceVersion: m.GetResourceVersion(), Labels: m.GetLabels(),
				OwnerReferences: m.GetOwnerReferences(),
			},
		})
	}
	return metadatafake.NewSimpleMetadataClient(metaScheme, metas...)
}
