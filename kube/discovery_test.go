package kube

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/fables-for-robots/kosh/render"
)

func has(rr []Resource, group, plural string) (Resource, bool) {
	for _, r := range rr {
		if r.Is(group, plural) {
			return r, true
		}
	}
	return Resource{}, false
}

func TestDiscoverySkipsSubresourcesAndUnlistable(t *testing.T) {
	e := newEnv(t, envConfig{})
	rr := e.session(t).Resources()
	for _, r := range rr {
		if strings.Contains(r.GVR.Resource, "/") {
			t.Errorf("subresource %s is a resource", r.GVR.Resource)
		}
	}
	if _, ok := has(rr, "", "bindings"); ok {
		t.Error("bindings, which cannot be listed, is a resource")
	}
	if _, ok := has(rr, "", "pods"); !ok {
		t.Error("pods is not a resource")
	}
	if _, ok := has(rr, "apps", "deployments"); !ok {
		t.Error("deployments is not a resource")
	}
}

func TestDiscoveryMarksTypedKinds(t *testing.T) {
	hpaV2 := schema.GroupVersionResource{Group: "autoscaling", Version: "v2", Resource: "horizontalpodautoscalers"}
	e := newEnv(t, envConfig{
		resources: append(standardResources(),
			// batch serves jobs in two versions; v2 is the preferred one,
			// and the renderer's is v1.
			&metav1.APIResourceList{GroupVersion: "batch/v2", APIResources: []metav1.APIResource{
				{Name: "jobs", SingularName: "job", Namespaced: true, Kind: "Job", Verbs: allVerbs},
			}}),
		renderers: func(r *testRenderers) {
			// The API serves autoscaling/v1 only.
			r.extra[hpaV2] = &testRenderer{gvr: hpaV2}
		},
	})
	rr := e.session(t).Resources()

	pods, _ := has(rr, "", "pods")
	if !pods.Typed || pods.GVR != render.Pods {
		t.Errorf("pods: %+v, want typed with %v", pods, render.Pods)
	}
	if pods.Kind != "Pod" || pods.Singular != "pod" || !pods.Namespaced || len(pods.ShortNames) != 1 || pods.ShortNames[0] != "po" || !pods.Can("list") {
		t.Errorf("pods: %+v", pods)
	}
	jobs, _ := has(rr, "batch", "jobs")
	if !jobs.Typed || jobs.GVR != render.Jobs {
		t.Errorf("jobs: %+v, want typed with the renderer's %v", jobs, render.Jobs)
	}
	hpa, _ := has(rr, "autoscaling", "horizontalpodautoscalers")
	if hpa.Typed || hpa.GVR.Version != "v1" {
		t.Errorf("horizontalpodautoscalers: %+v, want not typed, in v1", hpa)
	}
	deploy, _ := has(rr, "apps", "deployments")
	if deploy.Typed {
		t.Errorf("deployments, which have no renderer here, are typed")
	}
}

func TestDiscoveryKeepsGoingWhenAGroupFails(t *testing.T) {
	e := newEnv(t, envConfig{failGroup: "apps/v1"})
	rr := e.session(t).Resources()
	if _, ok := has(rr, "apps", "deployments"); ok {
		t.Error("deployments are there although apps/v1 failed")
	}
	for _, want := range [][2]string{{"", "pods"}, {"batch", "jobs"}, {"example.com", "widgets"}} {
		if _, ok := has(rr, want[0], want[1]); !ok {
			t.Errorf("%s.%s is missing", want[1], want[0])
		}
	}
}

func TestDiscoverySortsByNameThenGroup(t *testing.T) {
	e := newEnv(t, envConfig{})
	rr := e.session(t).Resources()
	for i := 1; i < len(rr); i++ {
		a, b := rr[i-1].GVR, rr[i].GVR
		if a.Resource > b.Resource || a.Resource == b.Resource && a.Group > b.Group {
			t.Errorf("%v before %v", a, b)
		}
	}
	if _, ok := has(rr, "events.k8s.io", "events"); !ok {
		t.Error("events.k8s.io is missing")
	}
}

func TestResolveNames(t *testing.T) {
	e := newEnv(t, envConfig{})
	s := e.session(t)
	for _, tc := range []struct{ name, group, plural string }{
		{"PODS", "", "pods"},
		{"Pod", "", "pods"},
		{"pO", "", "pods"},
		{"DePlOy", "apps", "deployments"},
		{"replicaSet", "apps", "replicasets"},
		{"Deployments.Apps", "apps", "deployments"},
		{"WIDGETS.example.COM", "example.com", "widgets"},
		{"Wd", "example.com", "widgets"},
		{"widget", "example.com", "widgets"},
		{"EVENTS.events.k8s.io", "events.k8s.io", "events"},
	} {
		r, err := s.Resolve(context.Background(), tc.name)
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if !r.Is(tc.group, tc.plural) {
			t.Errorf("%s: %v, want %s.%s", tc.name, r.GVR, tc.plural, tc.group)
		}
	}
}

func TestResolvePrefersTypedThenCore(t *testing.T) {
	gizmos := schema.GroupVersionResource{Group: "zz.example.com", Version: "v1", Resource: "gizmos"}
	e := newEnv(t, envConfig{
		resources: append(standardResources(),
			// gizmos in two groups, neither the core one; the later one is typed.
			&metav1.APIResourceList{GroupVersion: "aa.example.com/v1", APIResources: []metav1.APIResource{
				{Name: "gizmos", SingularName: "gizmo", Namespaced: true, Kind: "Gizmo", Verbs: allVerbs},
				{Name: "gadgets", SingularName: "gadget", Namespaced: true, Kind: "Gadget", Verbs: allVerbs},
			}},
			&metav1.APIResourceList{GroupVersion: "zz.example.com/v1", APIResources: []metav1.APIResource{
				{Name: "gizmos", SingularName: "gizmo", Namespaced: true, Kind: "Gizmo", Verbs: allVerbs},
			}},
			&metav1.APIResourceList{GroupVersion: "bb.example.com/v1", APIResources: []metav1.APIResource{
				{Name: "gadgets", SingularName: "gadget", Namespaced: true, Kind: "Gadget", Verbs: allVerbs},
			}},
		),
		renderers: func(r *testRenderers) { r.extra[gizmos] = &testRenderer{gvr: gizmos} },
	})
	s := e.session(t)
	for _, tc := range []struct{ name, group, plural string }{
		{"events", "", "events"},
		{"ev", "", "events"},
		{"gizmos", "zz.example.com", "gizmos"},
		{"gadget", "aa.example.com", "gadgets"},
	} {
		r, err := s.Resolve(context.Background(), tc.name)
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if !r.Is(tc.group, tc.plural) {
			t.Errorf("%s: %v, want %s.%s", tc.name, r.GVR, tc.plural, tc.group)
		}
	}
}

func TestResolveRediscoversOnceOnAMiss(t *testing.T) {
	e := newEnv(t, envConfig{})
	s := e.session(t)
	// How often one discovery reads the groups.
	n := e.disc.discoveries()
	e.c.refresh(context.Background())
	perDiscovery := e.disc.discoveries() - n

	e.disc.addResources(&metav1.APIResourceList{GroupVersion: "late.example.com/v1", APIResources: []metav1.APIResource{
		{Name: "latecomers", SingularName: "latecomer", Namespaced: true, Kind: "Latecomer", Verbs: allVerbs},
	}})

	before := e.disc.discoveries()
	r, err := s.Resolve(context.Background(), "latecomer")
	if err != nil {
		t.Fatalf("a resource added after start: %v", err)
	}
	if !r.Is("late.example.com", "latecomers") {
		t.Fatalf("got %v", r.GVR)
	}
	if _, ok := has(s.Resources(), "late.example.com", "latecomers"); !ok {
		t.Error("the new resource is not among the resources")
	}
	once := e.disc.discoveries()
	if once-before != perDiscovery {
		t.Fatalf("a miss read the groups %d times, want %d: one discovery", once-before, perDiscovery)
	}

	for _, name := range []string{"nothing", "nothing-either"} {
		if _, err := s.Resolve(context.Background(), name); !errors.Is(err, ErrUnknownResource) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if got := e.disc.discoveries(); got != once {
		t.Errorf("misses within 5 seconds of a discovery discovered again: %d reads of the groups, want %d", got, once)
	}
}

func TestResolveUnknown(t *testing.T) {
	e := newEnv(t, envConfig{})
	_, err := e.session(t).Resolve(context.Background(), "flubbers")
	if !errors.Is(err, ErrUnknownResource) {
		t.Fatalf("got %v, want ErrUnknownResource", err)
	}
	if !strings.Contains(err.Error(), "flubbers") {
		t.Errorf("%q does not name the resource", err)
	}
}

func TestDiscoveryRunsAgain(t *testing.T) {
	e := newEnv(t, envConfig{opts: Options{DiscoveryEvery: 10 * time.Millisecond}})
	e.disc.addResources(&metav1.APIResourceList{GroupVersion: "late.example.com/v1", APIResources: []metav1.APIResource{
		{Name: "latecomers", SingularName: "latecomer", Namespaced: true, Kind: "Latecomer", Verbs: allVerbs},
	}})
	eventually(t, "the background discovery finds latecomers", func() bool {
		_, ok := has(e.c.resourceList(), "late.example.com", "latecomers")
		return ok
	})
	// A discovery that fails keeps what was known.
	e.disc.setDown(true)
	n := e.disc.discoveries()
	eventually(t, "a failed discovery", func() bool { return e.disc.discoveries() > n+2 })
	if _, ok := has(e.c.resourceList(), "", "pods"); !ok {
		t.Error("a failed discovery dropped what was known")
	}
}
