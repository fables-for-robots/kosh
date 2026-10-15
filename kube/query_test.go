package kube

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"k8s.io/client-go/rest"
)

// proxyNamespace is the namespace the review typed: joined into a path
// and cleaned, it would aim the list at a service through the API
// server's proxy.
const proxyNamespace = "../../../../api/v1/namespaces/prod/services/http:billing:80/proxy/admin"

// badNamespaces are strings that are no namespace's name.
var badNamespaces = []string{
	proxyNamespace,
	"a/b",
	"..",
	".",
	"A",
	strings.Repeat("a", 64),
	"a b",
	"%2e",
	"%2e%2e",
	"-a",
	"a-",
	"a.b",
}

// apiServer stands in for the API server: it answers every request with
// an empty table, and keeps the escaped path of each.
type apiServer struct {
	srv   *httptest.Server
	mu    sync.Mutex
	paths []string
}

func newAPIServer(t *testing.T) *apiServer {
	t.Helper()
	a := &apiServer{}
	a.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		a.paths = append(a.paths, r.URL.EscapedPath())
		a.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"kind":"Table","apiVersion":"meta.k8s.io/v1","columnDefinitions":[],"rows":[]}`)
	}))
	t.Cleanup(a.srv.Close)
	return a
}

// requests returns the paths of the requests so far.
func (a *apiServer) requests() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.paths...)
}

// tables returns the tableLister of a real client of a.
func (a *apiServer) tables(t *testing.T) *restTables {
	t.Helper()
	tables, err := newRESTTables(&rest.Config{Host: a.srv.URL, QPS: 1000, Burst: 1000})
	if err != nil {
		t.Fatal(err)
	}
	return tables
}

// firstRead waits until w has read its table once, and closes it.
func firstRead(t *testing.T, w Watch) {
	t.Helper()
	defer w.Close()
	eventually(t, "the first read", func() bool {
		tab := w.Table(time.Now())
		return !tab.Stale || tab.Err != nil
	})
}

func TestWatchRefusesWhatIsNoNamespace(t *testing.T) {
	e := newEnv(t, envConfig{})
	api := newAPIServer(t)
	e.setTables(api.tables(t))
	s := e.session(t)
	widgets := e.resource(t, "example.com", "widgets") // a table of the API server's
	pods := e.resource(t, "", "pods")                  // a typed kind
	e.typed.ClearActions()                             // of discovery
	e.dyn.ClearActions()

	for _, ns := range badNamespaces {
		for _, r := range []Resource{widgets, pods} {
			w, err := s.Watch(Query{Resource: r, Namespace: ns})
			if !errors.Is(err, ErrInvalidNamespace) {
				t.Errorf("%s in %q: got %v, want ErrInvalidNamespace", r.GVR.Resource, ns, err)
			}
			if w != nil {
				firstRead(t, w)
			}
		}
	}
	if got := api.requests(); len(got) != 0 {
		t.Fatalf("the API server was sent %q", got)
	}

	// A namespace for a resource that has none.
	nodes := e.resource(t, "", "nodes")
	if w, err := s.Watch(Query{Resource: nodes, Namespace: "default"}); !errors.Is(err, ErrInvalidNamespace) {
		t.Errorf("nodes in default: got %v, want ErrInvalidNamespace", err)
		if w != nil {
			w.Close()
		}
	}

	// A resource that discovery did not make.
	for _, change := range []func(*Resource){
		func(r *Resource) { r.GVR.Resource = "widgets/../../../api/v1/secrets" },
		func(r *Resource) { r.GVR.Resource = "." },
		func(r *Resource) { r.GVR.Group = "example.com/../.." },
		func(r *Resource) { r.GVR.Version = "v1/.." },
		func(r *Resource) { r.GVR.Resource = "gizmos" },
		func(r *Resource) { r.Namespaced = false },
	} {
		r := widgets
		change(&r)
		w, err := s.Watch(Query{Resource: r})
		if !errors.Is(err, ErrUnknownResource) {
			t.Errorf("%+v: got %v, want ErrUnknownResource", r.GVR, err)
		}
		if w != nil {
			firstRead(t, w)
		}
	}
	if got := api.requests(); len(got) != 0 {
		t.Fatalf("the API server was sent %q", got)
	}

	// An Of that is no object's reference.
	deployments := e.resource(t, "apps", "deployments")
	for _, of := range []ObjectRef{
		{Resource: deployments, Namespace: proxyNamespace, Name: "web"},
		{Resource: deployments, Namespace: "default", Name: "../web"},
	} {
		if w, err := s.Watch(Query{Resource: pods, Of: &of}); err == nil {
			w.Close()
			t.Errorf("pods of %s: no error", of)
		}
	}
	if a := append(e.typed.Actions(), e.dyn.Actions()...); len(a) != 0 {
		t.Errorf("the clients were sent %v", a)
	}

	// What is a namespace works.
	for _, ns := range []string{"default", ""} {
		w, err := s.Watch(Query{Resource: widgets, Namespace: ns})
		if err != nil {
			t.Fatalf("widgets in %q: %v", ns, err)
		}
		firstRead(t, w)
		w, err = s.Watch(Query{Resource: pods, Namespace: ns})
		if err != nil {
			t.Fatalf("pods in %q: %v", ns, err)
		}
		w.Close()
	}
	want := []string{"/apis/example.com/v1/namespaces/default/widgets", "/apis/example.com/v1/widgets"}
	if got := api.requests(); len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("requests %q, want %q", got, want)
	}
}

// Whatever reaches the function that builds a table's path, the path
// has the segments of a list, or no request is made.
func TestGenericListPath(t *testing.T) {
	e := newEnv(t, envConfig{})
	api := newAPIServer(t)
	tables := api.tables(t)
	widgets := e.resource(t, "example.com", "widgets")
	configmaps := e.resource(t, "", "configmaps")
	ctx := context.Background()

	for _, ns := range append([]string{"default", "kube-system", ""}, badNamespaces...) {
		for _, r := range []Resource{widgets, configmaps} {
			before := len(api.requests())
			_, err := tables.listTable(ctx, r, ns)
			got := api.requests()[before:]
			if len(got) == 0 {
				if err == nil {
					t.Errorf("%s in %q: no request and no error", r.GVR.Resource, ns)
				}
				continue
			}
			want := []string{"", "apis", "example.com", "v1"}
			if r.GVR.Group == "" {
				want = []string{"", "api", "v1"}
			}
			if ns != "" {
				want = append(want, "namespaces", ns)
			}
			want = append(want, r.GVR.Resource)
			if len(got) != 1 || !sameSegments(got[0], want) {
				t.Errorf("%s in %q: requests %q, want only the segments %q", r.GVR.Resource, ns, got, want)
			}
		}
	}
	// A resource whose strings are no names.
	for _, change := range []func(*Resource){
		func(r *Resource) { r.GVR.Resource = "widgets/../../../api/v1/secrets" },
		func(r *Resource) { r.GVR.Group = ".." },
		func(r *Resource) { r.GVR.Version = "v1/../.." },
	} {
		r := widgets
		change(&r)
		before := len(api.requests())
		if _, err := tables.listTable(ctx, r, ""); err == nil {
			t.Errorf("%+v: no error", r.GVR)
		}
		if got := api.requests()[before:]; len(got) != 0 {
			t.Errorf("%+v: requests %q", r.GVR, got)
		}
	}
}

// sameSegments reports whether the escaped path p is the segments want,
// each escaped as one segment.
func sameSegments(p string, want []string) bool {
	got := strings.Split(p, "/")
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		seg, err := url.PathUnescape(got[i])
		if err != nil || seg != want[i] {
			return false
		}
	}
	return true
}

// A reference whose namespace or name could not be an object's is
// refused, for a read and for an action, before anything is sent.
func TestCheckRefNamesAndNamespaces(t *testing.T) {
	f := actionEnv(t, fullGrant, nil, readConfigMap(), actionNode("n1"))
	configmaps := f.resource(t, "", "configmaps")
	nodes := f.resource(t, "", "nodes")
	ctx := context.Background()
	type tc struct {
		name string
		ref  ObjectRef
		ns   bool // the namespace is what is wrong: ErrInvalidNamespace
	}
	var cases []tc
	for _, ns := range badNamespaces {
		cases = append(cases, tc{"namespace " + ns, ObjectRef{Resource: configmaps, Namespace: ns, Name: "settings"}, true})
	}
	cases = append(cases,
		tc{"no namespace for a namespaced resource", ObjectRef{Resource: configmaps, Name: "settings"}, true},
		tc{"a namespace for a node", ObjectRef{Resource: nodes, Namespace: "default", Name: "n1"}, true},
	)
	for _, name := range []string{"", ".", "..", "a/b", "../settings", "a%2fb", "%2e%2e", strings.Repeat("a", 254)} {
		cases = append(cases,
			tc{"name " + name, ObjectRef{Resource: configmaps, Namespace: "default", Name: name}, false},
			tc{"node name " + name, ObjectRef{Resource: nodes, Name: name}, false})
	}
	for _, c := range cases {
		f.forget()
		_, yamlErr := f.s.YAML(ctx, c.ref, false)
		deleteErr := f.s.Delete(ctx, c.ref, DeleteOptions{})
		for what, err := range map[string]error{"YAML": yamlErr, "Delete": deleteErr} {
			switch {
			case err == nil:
				t.Errorf("%s, %s: no error", c.name, what)
			case c.ns && !errors.Is(err, ErrInvalidNamespace):
				t.Errorf("%s, %s: got %v, want ErrInvalidNamespace", c.name, what, err)
			}
		}
		if a := f.sent(); len(a) != 0 {
			t.Errorf("%s: the clients were sent %v", c.name, a)
		}
	}

	// What is an object's reference is read.
	if _, err := f.s.YAML(ctx, ObjectRef{Resource: configmaps, Namespace: "default", Name: "settings"}, false); err != nil {
		t.Errorf("YAML of default/settings: %v", err)
	}
	if _, err := f.s.YAML(ctx, ObjectRef{Resource: nodes, Name: "n1"}, false); err != nil {
		t.Errorf("YAML of node n1: %v", err)
	}
	if _, err := f.s.YAML(ctx, ObjectRef{Resource: configmaps, Namespace: "default", Name: strings.Repeat("a", 253)}, false); errors.Is(err, ErrInvalidNamespace) || err == nil {
		t.Errorf("YAML of a name of 253 characters: %v, want that it is not there", err)
	} else if !strings.Contains(err.Error(), "not found") {
		t.Errorf("YAML of a name of 253 characters: %v, want that it is not there", err)
	}
}
