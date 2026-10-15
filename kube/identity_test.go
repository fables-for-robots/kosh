package kube

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"

	"github.com/fables-for-robots/kosh/keys"
	"github.com/fables-for-robots/kosh/render"
)

func TestIdentities(t *testing.T) {
	for _, tc := range []struct {
		name string
		got  identity
		want identity
	}{
		{"a key of view", identityOf(viewGrant, keys.DefaultInstance), identity{user: "kosh:alice/view", group: "kosh:kosh:clusterrole:view"}},
		{"a key of a Role", identityOf(teamGrant, keys.DefaultInstance), identity{user: "kosh:carol/edit@team-a", group: "kosh:kosh:role:team-a:edit"}},
		{"the ClusterRole view", roleIdentity(viewGrant.Role, keys.DefaultInstance), identity{user: "kosh:kosh:clusterrole:view", group: "kosh:kosh:clusterrole:view"}},
		{"the Role edit of team-a", roleIdentity(teamGrant.Role, keys.DefaultInstance), identity{user: "kosh:kosh:role:team-a:edit", group: "kosh:kosh:role:team-a:edit"}},
		// A ClusterRole whose name holds a colon is not the Role of the
		// namespace before it.
		{"the ClusterRole team-a:edit", roleIdentity(keys.Role{Kind: keys.KindClusterRole, Name: "team-a:edit"}, keys.DefaultInstance),
			identity{user: "kosh:kosh:clusterrole:team-a:edit", group: "kosh:kosh:clusterrole:team-a:edit"}},
	} {
		if tc.got != tc.want {
			t.Errorf("%s: %+v, want %+v", tc.name, tc.got, tc.want)
		}
	}
}

func TestSessionClientsImpersonateTheKey(t *testing.T) {
	e := newEnv(t, envConfig{}, readConfigMap())
	s := e.sessionOf(t, viewGrant)
	imp, ok := e.madeWith("kosh:alice/view")
	if !ok {
		t.Fatal("no clients were made for the key")
	}
	want := rest.ImpersonationConfig{UserName: "kosh:alice/view", Groups: []string{"kosh:kosh:clusterrole:view"}}
	if !reflect.DeepEqual(imp, want) {
		t.Errorf("the key's clients impersonate %+v, want %+v", imp, want)
	}
	ref := ObjectRef{Resource: e.resource(t, "", "configmaps"), Namespace: "default", Name: "settings"}
	if _, err := s.YAML(context.Background(), ref, false); err != nil {
		t.Fatal(err)
	}
	got := e.sent()
	if len(got) != 1 || got[0] != (sentRequest{user: "kosh:alice/view", verb: "get", gvr: ref.Resource.GVR, namespace: "default"}) {
		t.Errorf("sent %+v, want one get as the key", got)
	}
	// A second session of the key has the same clients.
	if again := e.sessionOf(t, viewGrant); again.clients != s.clients {
		t.Error("a second session of the key made other clients")
	}
}

// With a configuration, the clients are client-go's, and every request
// of a session carries the key's impersonation, and every request of its
// role's cache the role's. Each of the session's clients is asked once:
// the dynamic (YAML), the typed (SecretData, the pod of Logs), the
// metadata (Owner), the log opener (Logs) and the tables (a kind that is
// not typed), so that a call site that slid back to the ServiceAccount's
// clients would send a request without the key's name.
func TestARequestCarriesTheImpersonation(t *testing.T) {
	type seen struct{ path, as, user string }
	var (
		mu       sync.Mutex
		requests []seen
		groups   = map[string][]string{}
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user := r.Header.Get("Impersonate-User")
		accept := r.Header.Get("Accept")
		as := ""
		switch {
		case strings.Contains(accept, "as=Table"):
			as = "Table"
		case strings.Contains(accept, "as=PartialObjectMetadata"):
			as = "PartialObjectMetadata"
		}
		mu.Lock()
		requests = append(requests, seen{r.URL.Path, as, user})
		groups[user] = r.Header.Values("Impersonate-Group")
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Query().Get("watch") == "true":
			<-r.Context().Done()
		case as == "Table":
			fmt.Fprint(w, `{"kind":"Table","apiVersion":"meta.k8s.io/v1","metadata":{},"columnDefinitions":[],"rows":[]}`)
		case as == "PartialObjectMetadata":
			fmt.Fprint(w, `{"kind":"PartialObjectMetadata","apiVersion":"meta.k8s.io/v1","metadata":{"namespace":"default","name":"settings"}}`)
		case r.URL.Path == "/api/v1/pods":
			fmt.Fprint(w, `{"kind":"PodList","apiVersion":"v1","metadata":{"resourceVersion":"1"},"items":[]}`)
		case r.URL.Path == "/api/v1/namespaces/default/pods/web":
			fmt.Fprint(w, `{"kind":"Pod","apiVersion":"v1","metadata":{"namespace":"default","name":"web"},"spec":{"containers":[{"name":"web","image":"nginx"}]}}`)
		case r.URL.Path == "/api/v1/namespaces/default/pods/web/log":
			w.Header().Set("Content-Type", "text/plain")
			fmt.Fprint(w, "2026-10-15T10:00:00Z hello\n")
		case r.URL.Path == "/api/v1/namespaces/default/secrets/db":
			fmt.Fprint(w, `{"kind":"Secret","apiVersion":"v1","metadata":{"namespace":"default","name":"db"},"data":{"password":"czNjcjN0"}}`)
		default:
			fmt.Fprint(w, `{"kind":"ConfigMap","apiVersion":"v1","metadata":{"namespace":"default","name":"settings"}}`)
		}
	}))
	t.Cleanup(srv.Close)
	cfg := &rest.Config{Host: srv.URL}
	e := newEnv(t, envConfig{})
	c := newCluster(cfg, e.typed, e.dyn, nil, e.c.resourceList(), Options{Log: newAuditLog().logger()})
	t.Cleanup(c.Close)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	s := c.Session(ctx, viewGrant).(*session)
	bg := context.Background()

	configMaps := e.resource(t, "", "configmaps")
	settings := ObjectRef{Resource: configMaps, Namespace: "default", Name: "settings"}
	if _, err := s.YAML(bg, settings, false); err != nil {
		t.Errorf("YAML: %v", err)
	}
	if _, err := s.Owner(bg, settings); !errors.Is(err, ErrNoOwner) {
		t.Errorf("Owner: %v, want ErrNoOwner", err)
	}
	db := ObjectRef{Resource: e.resource(t, "", "secrets"), Namespace: "default", Name: "db"}
	if got, err := s.SecretData(bg, db); err != nil || len(got) != 1 || got[0].Value != "s3cr3t" {
		t.Errorf("SecretData: %v, %v", got, err)
	}
	pods := e.resource(t, "", "pods")
	ls, err := s.Logs(bg, ObjectRef{Resource: pods, Namespace: "default", Name: "web"}, LogOptions{})
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	if lines := readFlatten(readCollect(t, ls)); len(lines) != 1 || lines[0].Text != "hello" {
		t.Errorf("Logs: %+v", lines)
	}
	table, err := s.Watch(Query{Resource: configMaps})
	if err != nil {
		t.Fatal(err)
	}
	defer table.Close()
	w, err := s.Watch(Query{Resource: pods})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	eventually(t, "the pods' list and the table of configmaps", func() bool {
		tab, cms := w.Table(time.Now()), table.Table(time.Now())
		return tab.Err == nil && !tab.Stale && cms.Err == nil && !cms.Stale
	})

	mu.Lock()
	defer mu.Unlock()
	const key = "kosh:alice/view"
	want := []seen{
		{"/api/v1/namespaces/default/configmaps/settings", "", key},                      // YAML: dynamic
		{"/api/v1/namespaces/default/configmaps/settings", "PartialObjectMetadata", key}, // Owner: metadata
		{"/api/v1/namespaces/default/secrets/db", "", key},                               // SecretData: typed
		{"/api/v1/namespaces/default/pods/web", "", key},                                 // Logs: typed
		{"/api/v1/namespaces/default/pods/web/log", "", key},                             // Logs: the opener
		{"/api/v1/configmaps", "Table", key},                                             // the tables
		{"/api/v1/pods", "", "kosh:kosh:clusterrole:view"},                               // the role's cache
	}
	for _, r := range want {
		if !slices.Contains(requests, r) {
			t.Errorf("no request %+v among %+v", r, requests)
		}
	}
	for _, r := range requests {
		if r.user == "" {
			t.Errorf("%s was asked without impersonation", r.path)
		}
	}
	for user, g := range groups {
		if !reflect.DeepEqual(g, []string{"kosh:kosh:clusterrole:view"}) {
			t.Errorf("%s impersonates the groups %v", user, g)
		}
	}
}

// A Cluster without a configuration, and without a factory of a test,
// gives no identity clients: a session of it is not the ServiceAccount
// in disguise, and is refused what it asks.
func TestNoConfigNoClients(t *testing.T) {
	e := newEnv(t, envConfig{}, readConfigMap())
	log := newAuditLog()
	c := newCluster(nil, e.typed, e.dyn, nil, e.c.resourceList(), Options{Log: log.logger()})
	t.Cleanup(c.Close)
	e.typed.ClearActions() // the discovery of newEnv's own Cluster
	e.dyn.ClearActions()
	if _, err := c.clientsFor(identityOf(fullGrant, keys.DefaultInstance)); !errors.Is(err, errNoIdentityConfig) {
		t.Errorf("clientsFor: %v, want errNoIdentityConfig", err)
	}
	s := c.Session(context.Background(), fullGrant)
	if recs := log.with("the clients of a session cannot be made"); len(recs) != 1 {
		t.Errorf("records %+v, want the session's error", log.all())
	}
	ref := ObjectRef{Resource: e.resource(t, "", "configmaps"), Namespace: "default", Name: "settings"}
	if _, err := s.YAML(context.Background(), ref, false); !errors.Is(err, errNoIdentityConfig) {
		t.Errorf("YAML: %v, want errNoIdentityConfig", err)
	}
	if err := s.Delete(context.Background(), ref, DeleteOptions{}); !errors.Is(err, errNoIdentityConfig) {
		t.Errorf("Delete: %v, want errNoIdentityConfig", err)
	}
	if _, err := s.Watch(Query{Resource: e.resource(t, "", "pods")}); !errors.Is(err, errNoIdentityConfig) {
		t.Errorf("Watch: %v, want errNoIdentityConfig", err)
	}
	if a := append(e.typed.Actions(), e.dyn.Actions()...); len(a) != 0 {
		t.Errorf("the ServiceAccount's clients were sent %v", a)
	}
}

// watchCache returns the cache a typed watch reads.
func watchCache(t *testing.T, w Watch) *kindCache {
	t.Helper()
	tw, ok := w.(*typedWatch)
	if !ok {
		t.Fatalf("%T is not the watch of a typed kind", w)
	}
	return tw.cache
}

// openWatch opens the watch of q in s and closes it when the test ends.
func openWatch(t *testing.T, s *session, q Query) Watch {
	t.Helper()
	w, err := s.Watch(q)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(w.Close)
	return w
}

// listsOf returns the lists of gvr among what was sent.
func listsOf(e *env, gvr schema.GroupVersionResource) []sentRequest {
	var out []sentRequest
	for _, r := range e.sent() {
		if r.verb == "list" && r.gvr == gvr {
			out = append(out, r)
		}
	}
	return out
}

func TestCachesArePerRole(t *testing.T) {
	e := newEnv(t, envConfig{}, pod("default", "web", nil))
	bob := keys.Grant{Name: "bob/view", Fingerprint: "SHA256:bob", Role: viewGrant.Role}
	q := Query{Resource: e.resource(t, "", "pods")}

	alice := openWatch(t, e.sessionOf(t, viewGrant), q)
	again := openWatch(t, e.sessionOf(t, bob), q)
	admin := openWatch(t, e.sessionOf(t, fullGrant), q)
	if watchCache(t, alice) != watchCache(t, again) {
		t.Error("two keys of view hold two caches")
	}
	if watchCache(t, alice) == watchCache(t, admin) {
		t.Error("a key of view and one of cluster-admin share a cache")
	}
	if got, want := watchCache(t, alice).key, (cacheKey{id: roleIdentity(viewGrant.Role, keys.DefaultInstance), gvr: render.Pods}); got != want {
		t.Errorf("the cache of view is %+v, want %+v", got, want)
	}

	eventually(t, "the two caches' lists", func() bool { return len(listsOf(e, render.Pods)) >= 2 })
	users := map[string]bool{}
	for _, r := range listsOf(e, render.Pods) {
		users[r.user] = true
		if r.namespace != "" {
			t.Errorf("a ClusterRole's cache listed %q, not every namespace", r.namespace)
		}
	}
	if !reflect.DeepEqual(users, map[string]bool{"kosh:kosh:clusterrole:view": true, "kosh:kosh:clusterrole:cluster-admin": true}) {
		t.Errorf("the pods were listed as %v, want as each role", users)
	}
	imp, ok := e.madeWith("kosh:kosh:clusterrole:view")
	if want := (rest.ImpersonationConfig{UserName: "kosh:kosh:clusterrole:view", Groups: []string{"kosh:kosh:clusterrole:view"}}); !ok || !reflect.DeepEqual(imp, want) {
		t.Errorf("the cache's clients impersonate %+v, want %+v", imp, want)
	}
}

func TestRoleCacheIsNamespaced(t *testing.T) {
	e := newEnv(t, envConfig{}, pod(testTeamNamespace, "api", nil), pod("default", "web", nil))
	s := e.sessionOf(t, teamGrant)
	w := openWatch(t, s, Query{Resource: e.resource(t, "", "pods"), Namespace: testTeamNamespace})
	if got, want := watchCache(t, w).key, (cacheKey{id: roleIdentity(teamGrant.Role, keys.DefaultInstance), gvr: render.Pods, namespace: testTeamNamespace}); got != want {
		t.Errorf("the cache is %+v, want %+v", got, want)
	}
	var tab Table
	eventually(t, "the pod of team-a", func() bool {
		tab = w.Table(time.Now())
		return len(tab.Rows) == 1
	})
	if got := names(tab.Rows); !got["team-a/api"] {
		t.Errorf("rows %v", got)
	}
	if got := watchCache(t, w).list(); len(got) != 1 {
		t.Errorf("the cache holds %d pods, want team-a's one", len(got))
	}
	lists := listsOf(e, render.Pods)
	if len(lists) == 0 {
		t.Fatal("nothing was listed")
	}
	for _, r := range lists {
		if r.namespace != testTeamNamespace || r.user != "kosh:kosh:role:team-a:edit" {
			t.Errorf("listed %+v, want team-a as the Role's user", r)
		}
	}
}

func TestAnotherNamespaceIsAnotherCache(t *testing.T) {
	e := newEnv(t, envConfig{}, pod(testTeamNamespace, "api", nil), pod("default", "web", nil))
	pods := e.resource(t, "", "pods")
	team := e.sessionOf(t, teamGrant)
	own := openWatch(t, team, Query{Resource: pods, Namespace: testTeamNamespace})
	other := openWatch(t, team, Query{Resource: pods, Namespace: "default"})
	all := openWatch(t, team, Query{Resource: pods})
	id := roleIdentity(teamGrant.Role, keys.DefaultInstance)
	for _, tc := range []struct {
		w    Watch
		want cacheKey
	}{
		{own, cacheKey{id: id, gvr: render.Pods, namespace: testTeamNamespace}},
		// The API refuses these to the Role, and their tables show it:
		// envtest's TestRoleSeesItsNamespaceOnly.
		{other, cacheKey{id: id, gvr: render.Pods, namespace: "default"}},
		{all, cacheKey{id: id, gvr: render.Pods}},
	} {
		if got := watchCache(t, tc.w).key; got != tc.want {
			t.Errorf("cache %+v, want %+v", got, tc.want)
		}
	}
	// A ClusterRole's queries of two namespaces share its one cache.
	admin := e.sessionOf(t, fullGrant)
	a := openWatch(t, admin, Query{Resource: pods, Namespace: testTeamNamespace})
	b := openWatch(t, admin, Query{Resource: pods, Namespace: "default"})
	if watchCache(t, a) != watchCache(t, b) {
		t.Error("a ClusterRole's queries of two namespaces hold two caches")
	}
	// The pods of a deployment of team-a are read from the cache of
	// team-a.
	dep := actionDeployment(testTeamNamespace, "api", "api")
	if _, err := e.typed.AppsV1().Deployments(testTeamNamespace).Create(context.Background(), dep, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	of := ObjectRef{Resource: e.resource(t, "apps", "deployments"), Namespace: testTeamNamespace, Name: "api"}
	if w := openWatch(t, team, Query{Resource: pods, Of: &of}); watchCache(t, w) != watchCache(t, own) {
		t.Error("the pods of a deployment of team-a are not read from team-a's cache")
	}
}

// pollerKeys returns the keys of the running metrics pollers.
func (c *Cluster) pollerKeys() []cacheKey {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]cacheKey, 0, len(c.pollers))
	for k := range c.pollers {
		out = append(out, k)
	}
	return out
}

func TestMetricsPollersPerRole(t *testing.T) {
	api := newMetricsAPI()
	api.set(PodMetrics, podMetrics("default", "web", [3]string{"web", "10m", "20Mi"}), podMetrics(testTeamNamespace, "api", [3]string{"api", "30m", "40Mi"}))
	e := meteredEnv(t, api, metricsResources(), Options{MetricsPoll: time.Hour},
		meteredPod("default", "web", "web"), meteredPod(testTeamNamespace, "api", "api"))
	pods := e.resource(t, "", "pods")
	bob := keys.Grant{Name: "bob/view", Fingerprint: "SHA256:bob", Role: viewGrant.Role}
	for _, open := range []struct {
		grant keys.Grant
		q     Query
	}{
		{viewGrant, Query{Resource: pods}},
		{bob, Query{Resource: pods}},
		{fullGrant, Query{Resource: pods}},
		{teamGrant, Query{Resource: pods, Namespace: testTeamNamespace}},
	} {
		w := openWatch(t, e.sessionOf(t, open.grant), open.q)
		w.Table(time.Now()) // takes the poller
	}
	got := e.c.pollerKeys()
	want := []cacheKey{
		{id: roleIdentity(viewGrant.Role, keys.DefaultInstance), gvr: PodMetrics},
		{id: roleIdentity(fullGrant.Role, keys.DefaultInstance), gvr: PodMetrics},
		{id: roleIdentity(teamGrant.Role, keys.DefaultInstance), gvr: PodMetrics, namespace: testTeamNamespace},
	}
	if len(got) != len(want) {
		t.Fatalf("pollers %+v, want %+v", got, want)
	}
	for _, k := range want {
		if !slices.Contains(got, k) {
			t.Errorf("no poller %+v among %+v", k, got)
		}
	}
	eventually(t, "each poller's list", func() bool { return len(listsOf(e, PodMetrics)) >= 3 })
	for _, r := range listsOf(e, PodMetrics) {
		wantNS := ""
		if r.user == "kosh:kosh:role:team-a:edit" {
			wantNS = testTeamNamespace
		}
		if r.namespace != wantNS {
			t.Errorf("%s listed the metrics of %q, want %q", r.user, r.namespace, wantNS)
		}
	}
}

func TestClusterUsagePerRole(t *testing.T) {
	api := newMetricsAPI()
	api.set(NodeMetrics, nodeMetrics("n1", "1", "2Gi"))
	e := meteredEnv(t, api, metricsResources(), Options{MetricsPoll: time.Hour}, allocNode("n1", "4", "8Gi"))
	bob := keys.Grant{Name: "bob/view", Fingerprint: "SHA256:bob", Role: viewGrant.Role}
	var usages []UsageWatch
	for _, g := range []keys.Grant{viewGrant, bob, fullGrant} {
		u := e.sessionOf(t, g).ClusterUsage()
		t.Cleanup(u.Close)
		usages = append(usages, u)
	}
	for i, u := range usages {
		eventually(t, fmt.Sprintf("usage %d", i), func() bool {
			got, ok := u.Current()
			return ok && got == ClusterUsage{CPU: 25, Mem: 25, Served: true}
		})
	}
	wantPollers := []cacheKey{{id: roleIdentity(viewGrant.Role, keys.DefaultInstance), gvr: NodeMetrics}, {id: roleIdentity(fullGrant.Role, keys.DefaultInstance), gvr: NodeMetrics}}
	wantCaches := []cacheKey{{id: roleIdentity(viewGrant.Role, keys.DefaultInstance), gvr: render.Nodes}, {id: roleIdentity(fullGrant.Role, keys.DefaultInstance), gvr: render.Nodes}}
	if got := e.c.pollerKeys(); len(got) != 2 || !slices.Contains(got, wantPollers[0]) || !slices.Contains(got, wantPollers[1]) {
		t.Errorf("pollers %+v, want %+v", got, wantPollers)
	}
	if got := e.c.cacheKeys(); len(got) != 2 || !slices.Contains(got, wantCaches[0]) || !slices.Contains(got, wantCaches[1]) {
		t.Errorf("caches %+v, want %+v", got, wantCaches)
	}

	// A Role cannot grant nodes: its session's usage is not served, and
	// holds nothing.
	team := e.sessionOf(t, teamGrant).ClusterUsage()
	t.Cleanup(team.Close)
	if got, ok := team.Current(); ok || got.Served {
		t.Errorf("a Role's usage %+v, %v; want not served", got, ok)
	}
	for _, k := range append(e.c.pollerKeys(), e.c.cacheKeys()...) {
		if k.id == roleIdentity(teamGrant.Role, keys.DefaultInstance) {
			t.Errorf("a Role's usage holds %+v", k)
		}
	}
}
