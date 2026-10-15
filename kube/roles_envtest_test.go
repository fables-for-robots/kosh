package kube

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/fables-for-robots/kosh/keys"
)

// rolesEnv is an API server with RBAC on, the roles of the tests' grants
// bound to their groups, a Secret and a pod in default and in team-a,
// and a Cluster that runs as the user kosh, whose rights are the least
// of §7 of the roles design. Its log is kept.
type rolesEnv struct {
	admin *rest.Config
	c     *Cluster
	log   *auditLog
}

func newRolesEnv(t *testing.T) *rolesEnv {
	t.Helper()
	admin, kosh := startRBACAPIServer(t)
	typed := kubernetes.NewForConfigOrDie(admin)
	ctx := context.Background()
	for _, ns := range []string{"default", testTeamNamespace} {
		secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "db"}, Data: map[string][]byte{"password": []byte("s3cr3t")}}
		if _, err := typed.CoreV1().Secrets(ns).Create(ctx, secret, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
		p := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "web"},
			Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "web", Image: "nginx"}}},
		}
		if _, err := typed.CoreV1().Pods(ns).Create(ctx, p, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	log := newAuditLog()
	return &rolesEnv{admin: admin, c: newEnvCluster(t, kosh, Options{Log: log.logger()}), log: log}
}

func (r *rolesEnv) session(t *testing.T, grant keys.Grant) *session {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return r.c.Session(ctx, grant).(*session)
}

func (r *rolesEnv) secret(t *testing.T, s *session, namespace string) ObjectRef {
	t.Helper()
	return ObjectRef{Resource: readEnvResolve(t, s, "secrets"), Namespace: namespace, Name: "db"}
}

func TestViewCannotReadASecret(t *testing.T) {
	r := newRolesEnv(t)
	s := r.session(t, viewGrant)
	ref := r.secret(t, s, "default")
	_, err := s.SecretData(context.Background(), ref)
	if !apierrors.IsForbidden(err) {
		t.Fatalf("SecretData: %v, want the API's refusal", err)
	}
	if !strings.Contains(err.Error(), `"kosh:alice/view"`) {
		t.Errorf("the refusal %q does not name the key", err)
	}
	recs := r.log.with("refused")
	if len(recs) != 1 {
		t.Fatalf("records %+v, want one refusal", r.log.all())
	}
	for k, v := range map[string]string{"key": viewGrant.Name, "role": "ClusterRole/view", "action": readSecretData, "reason": "the API answered Forbidden (403)"} {
		if got, _ := recs[0].attr(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	// Nor describe it: the describers ask as the key too, and the
	// refusal is logged as the read's.
	_, err = s.Describe(context.Background(), ref)
	if !apierrors.IsForbidden(err) || !strings.Contains(err.Error(), `"kosh:alice/view"`) {
		t.Errorf("Describe: %v, want the API's refusal of the key", err)
	}
	if recs := r.log.with("refused"); len(recs) != 2 {
		t.Errorf("records %+v, want the describe's refusal", r.log.all())
	} else if a, _ := recs[1].attr("action"); a != readDescribe {
		t.Errorf("the second refusal is of %q, want %q", a, readDescribe)
	}
	// view may read what is not a Secret.
	if _, err := s.YAML(context.Background(), ObjectRef{Resource: readEnvResolve(t, s, "pods"), Namespace: "default", Name: "web"}, false); err != nil {
		t.Errorf("YAML of a pod: %v", err)
	}
	// And may change nothing: the API refuses, and kosh logs the action.
	pod := ObjectRef{Resource: readEnvResolve(t, s, "pods"), Namespace: "default", Name: "web"}
	if err := s.Kill(context.Background(), pod); !apierrors.IsForbidden(err) {
		t.Errorf("Kill: %v, want the API's refusal", err)
	}
	if recs := r.log.with("action"); len(recs) != 1 {
		t.Errorf("records %+v, want the kill's line", r.log.all())
	} else if e, _ := recs[0].attr("error"); e != "the API answered Forbidden (403)" {
		t.Errorf("the kill's error is %q", e)
	}
}

func TestClusterAdminCan(t *testing.T) {
	r := newRolesEnv(t)
	s := r.session(t, fullGrant)
	got, err := s.SecretData(context.Background(), r.secret(t, s, "default"))
	if err != nil || len(got) != 1 || got[0].Value != "s3cr3t" {
		t.Fatalf("SecretData: %v, %v", got, err)
	}
	pods := readEnvResolve(t, s, "pods")
	w, err := s.Watch(Query{Resource: pods})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	eventually(t, "the pods of every namespace", func() bool { return len(w.Table(time.Now()).Rows) == 2 })
	if err := s.Kill(context.Background(), ObjectRef{Resource: pods, Namespace: "default", Name: "web"}); err != nil {
		t.Errorf("Kill: %v", err)
	}
	if recs := r.log.with("refused"); len(recs) != 0 {
		t.Errorf("refusals %+v", recs)
	}
}

func TestRoleSeesItsNamespaceOnly(t *testing.T) {
	r := newRolesEnv(t)
	s := r.session(t, teamGrant)
	pods := readEnvResolve(t, s, "pods")
	open := func(q Query) Watch {
		w, err := s.Watch(q)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(w.Close)
		return w
	}
	own := open(Query{Resource: pods, Namespace: testTeamNamespace})
	eventually(t, "the pod of team-a", func() bool {
		tab := own.Table(time.Now())
		return tab.Err == nil && len(tab.Rows) == 1 && tab.Rows[0].Namespace == testTeamNamespace
	})
	// The tables of pods are read from the Role's caches, as the Role's
	// user; nodes, which the tests' renderers do not type, from the API's
	// tables, as the key.
	for _, tc := range []struct {
		name, user string
		q          Query
	}{
		{"default", "kosh:role:team-a:edit", Query{Resource: pods, Namespace: "default"}},
		{"every namespace", "kosh:role:team-a:edit", Query{Resource: pods}},
		{"the nodes", "kosh:carol/edit@team-a", Query{Resource: readEnvResolve(t, s, "nodes")}},
	} {
		w := open(tc.q)
		var tab Table
		eventually(t, "the refusal of "+tc.name, func() bool {
			tab = w.Table(time.Now())
			return tab.Err != nil
		})
		if !apierrors.IsForbidden(tab.Err) || !strings.Contains(tab.Err.Error(), `"`+tc.user+`"`) {
			t.Errorf("%s: %v, want the API's refusal of %s", tc.name, tab.Err, tc.user)
		}
		if len(tab.Rows) != 0 {
			t.Errorf("%s: rows %v", tc.name, tab.Rows)
		}
	}
	// The Role's own namespace's Secret it may read.
	if _, err := s.SecretData(context.Background(), r.secret(t, s, testTeamNamespace)); err != nil {
		t.Errorf("the Secret of team-a: %v", err)
	}
	if _, err := s.SecretData(context.Background(), r.secret(t, s, "default")); !apierrors.IsForbidden(err) {
		t.Errorf("the Secret of default: %v, want the API's refusal", err)
	}
	// A Role cannot grant nodes: no usage, and nothing asked for it.
	u := s.ClusterUsage()
	defer u.Close()
	if got, ok := u.Current(); ok || got.Served {
		t.Errorf("usage %+v, %v", got, ok)
	}
}

func TestImpersonatedUserInTheAPIsAnswer(t *testing.T) {
	r := newRolesEnv(t)
	s := r.session(t, viewGrant)
	review, err := s.clients.typed.AuthenticationV1().SelfSubjectReviews().Create(context.Background(), &authenticationv1.SelfSubjectReview{}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	info := review.Status.UserInfo
	if info.Username != "kosh:alice/view" || !slices.Contains(info.Groups, "kosh:clusterrole:view") || !slices.Contains(info.Groups, "system:authenticated") {
		t.Errorf("the API sees %+v, want the key with its role's group", info)
	}
	// The role's caches are made as the role.
	api, err := r.c.clientsFor(roleIdentity(viewGrant.Role))
	if err != nil {
		t.Fatal(err)
	}
	review, err = api.typed.AuthenticationV1().SelfSubjectReviews().Create(context.Background(), &authenticationv1.SelfSubjectReview{}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := review.Status.UserInfo.Username; got != "kosh:clusterrole:view" {
		t.Errorf("the role's cache is %q to the API", got)
	}
}

// TestBindingsWorkForReal: the bindings kosh makes, as the user of §7's
// least rights, are the ones a session's rights come from. The fixture's
// binding of view is deleted first, for Bindings to make it; those of
// cluster-admin and of edit in team-a, which have no label, stay for the
// other tests and show here that a binding without kosh's label is not
// pruned, and that one at kosh's name is marked when a role needs it.
func TestBindingsWorkForReal(t *testing.T) {
	r := newRolesEnv(t)
	ctx := context.Background()
	rbac := kubernetes.NewForConfigOrDie(r.admin).RbacV1()
	if err := rbac.ClusterRoleBindings().Delete(ctx, viewGrant.Role.Group(), metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	s := r.session(t, viewGrant)
	listPods := func() error {
		_, err := s.clients.typed.CoreV1().Pods("default").List(ctx, metav1.ListOptions{})
		return err
	}
	if err := listPods(); !apierrors.IsForbidden(err) {
		t.Fatalf("list pods without a binding: %v, want the API's refusal", err)
	}

	var mu sync.Mutex
	roles := []keys.Role{viewGrant.Role, teamGrant.Role}
	b := NewBindings(r.c, func() []keys.Role {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(roles)
	}, r.c.log)
	if err := b.Make(ctx); err != nil {
		t.Fatalf("Make: %v", err)
	}
	eventually(t, "view may list pods", func() bool { return listPods() == nil })
	crb, err := rbac.ClusterRoleBindings().Get(ctx, "kosh:clusterrole:view", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if crb.Labels["app.kubernetes.io/managed-by"] != "kosh" || crb.Annotations["kosh.fables-for-robots.dev/role"] != "ClusterRole/view" ||
		crb.RoleRef.Name != "view" || len(crb.Subjects) != 1 || crb.Subjects[0].Name != "kosh:clusterrole:view" {
		t.Errorf("the binding of view: %+v", crb)
	}
	rb, err := rbac.RoleBindings(testTeamNamespace).Get(ctx, "kosh:role:edit", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if rb.Labels["app.kubernetes.io/managed-by"] != "kosh" {
		t.Errorf("the fixture's binding of edit in team-a is not marked: %v", rb.Labels)
	}
	// kosh may bind the roles and not read them: no role is said to be
	// missing, and nothing failed.
	if recs := r.log.with("the role of a key does not exist"); len(recs) != 0 {
		t.Errorf("records %+v", recs)
	}

	mu.Lock()
	roles = []keys.Role{teamGrant.Role}
	mu.Unlock()
	if err := b.Prune(ctx); err != nil {
		t.Fatalf("Prune: %v", err)
	}
	eventually(t, "view is refused again", func() bool { return apierrors.IsForbidden(listPods()) })
	if _, err := rbac.ClusterRoleBindings().Get(ctx, "kosh:clusterrole:view", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("the binding of view after Prune: %v", err)
	}
	if _, err := rbac.RoleBindings(testTeamNamespace).Get(ctx, "kosh:role:edit", metav1.GetOptions{}); err != nil {
		t.Errorf("the binding of edit in team-a, which a role needs: %v", err)
	}
	if _, err := rbac.ClusterRoleBindings().Get(ctx, fullGrant.Role.Group(), metav1.GetOptions{}); err != nil {
		t.Errorf("the fixture's binding of cluster-admin, without kosh's label: %v", err)
	}
}

func TestCanAgainstRBAC(t *testing.T) {
	r := newRolesEnv(t)
	ctx := context.Background()
	view := r.session(t, viewGrant)
	pods := readEnvResolve(t, view, "pods")
	secrets := readEnvResolve(t, view, "secrets")
	team := r.session(t, teamGrant)
	for _, tc := range []struct {
		name      string
		s         *session
		verb      string
		res       Resource
		namespace string
		want      bool
	}{
		{"view: list pods", view, "list", pods, "default", true},
		{"view: list pods everywhere", view, "list", pods, "", true},
		{"view: delete pods", view, "delete", pods, "default", false},
		{"view: get secrets", view, "get", secrets, "default", false},
		{"edit@team-a: delete pods in team-a", team, "delete", pods, testTeamNamespace, true},
		{"edit@team-a: delete pods in default", team, "delete", pods, "default", false},
		{"edit@team-a: delete pods everywhere", team, "delete", pods, "", false},
	} {
		if got := tc.s.Can(ctx, tc.verb, tc.res, "", tc.namespace); got != tc.want {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
	// A subresource is asked as one: view may read a pod's log and not
	// open a shell in it.
	if !view.Can(ctx, "get", pods, "log", "default") {
		t.Error("view: get pods/log: not allowed")
	}
	if view.Can(ctx, "create", pods, "exec", "default") {
		t.Error("view: create pods/exec: allowed")
	}
	if recs := r.log.with("the key's rights could not be checked"); len(recs) != 0 {
		t.Errorf("errors %+v", recs)
	}
}
