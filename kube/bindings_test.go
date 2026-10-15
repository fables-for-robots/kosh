package kube

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stesting "k8s.io/client-go/testing"

	"github.com/fables-for-robots/kosh/keys"
)

// bindingsFixture is a Cluster on fakes, a Bindings of it whose roles the
// test sets, and its log.
type bindingsFixture struct {
	e   *env
	b   *Bindings
	log *auditLog

	mu    sync.Mutex
	roles []keys.Role
}

// newBindingsFixture makes a Bindings over the fakes, with the ClusterRoles
// and Roles of names ("view", "edit@team-a") existing, and roles as what
// the key list names.
func newBindingsFixture(t *testing.T, existing []keys.Role, roles ...keys.Role) *bindingsFixture {
	t.Helper()
	log := newAuditLog()
	f := &bindingsFixture{e: newEnv(t, envConfig{opts: Options{Log: log.logger()}}), log: log, roles: roles}
	f.b = NewBindings(f.e.c, f.currentRoles, log.logger())
	ctx := context.Background()
	for _, r := range existing {
		var err error
		if r.Kind == keys.KindClusterRole {
			_, err = f.e.typed.RbacV1().ClusterRoles().Create(ctx, &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: r.Name}}, metav1.CreateOptions{})
		} else {
			_, err = f.e.typed.RbacV1().Roles(r.Namespace).Create(ctx, &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Namespace: r.Namespace, Name: r.Name}}, metav1.CreateOptions{})
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	f.e.typed.ClearActions()
	return f
}

func (f *bindingsFixture) currentRoles() []keys.Role {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.roles)
}

func (f *bindingsFixture) setRoles(roles ...keys.Role) {
	f.mu.Lock()
	f.roles = roles
	f.mu.Unlock()
}

// writes returns the creates, updates and deletes of bindings sent so far,
// as "create clusterrolebindings kosh:clusterrole:view".
func (f *bindingsFixture) writes() []string {
	var out []string
	for _, a := range f.e.typed.Actions() {
		res := a.GetResource().Resource
		if res != "clusterrolebindings" && res != "rolebindings" {
			continue
		}
		var name string
		switch a := a.(type) {
		case k8stesting.CreateAction:
			name = a.GetObject().(metav1.Object).GetName()
		case k8stesting.UpdateAction:
			name = a.GetObject().(metav1.Object).GetName()
		case k8stesting.DeleteAction:
			name = a.GetName()
		default:
			continue
		}
		if ns := a.GetNamespace(); ns != "" {
			name = ns + "/" + name
		}
		out = append(out, a.GetVerb()+" "+res+" "+name)
	}
	return out
}

func (f *bindingsFixture) crb(t *testing.T, name string) (*rbacv1.ClusterRoleBinding, bool) {
	t.Helper()
	b, err := f.e.typed.RbacV1().ClusterRoleBindings().Get(context.Background(), name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, false
	}
	if err != nil {
		t.Fatal(err)
	}
	return b, true
}

func (f *bindingsFixture) rb(t *testing.T, namespace, name string) (*rbacv1.RoleBinding, bool) {
	t.Helper()
	b, err := f.e.typed.RbacV1().RoleBindings(namespace).Get(context.Background(), name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, false
	}
	if err != nil {
		t.Fatal(err)
	}
	return b, true
}

var (
	viewRole  = keys.Role{Kind: keys.KindClusterRole, Name: "view"}
	adminRole = keys.Role{Kind: keys.KindClusterRole, Name: "cluster-admin"}
	editTeamA = keys.Role{Kind: keys.KindRole, Namespace: "team-a", Name: "edit"}
	editTeamB = keys.Role{Kind: keys.KindRole, Namespace: "team-b", Name: "edit"}
)

// kosh's mark on a binding it made.
var koshLabels = map[string]string{"app.kubernetes.io/managed-by": "kosh"}

func TestMakeCreatesClusterRoleBinding(t *testing.T) {
	f := newBindingsFixture(t, []keys.Role{viewRole}, viewRole)
	if err := f.b.Make(context.Background()); err != nil {
		t.Fatal(err)
	}
	b, ok := f.crb(t, "kosh:clusterrole:view")
	if !ok {
		t.Fatal("no ClusterRoleBinding kosh:clusterrole:view")
	}
	if want := (rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: "view"}); b.RoleRef != want {
		t.Errorf("roleRef %+v, want %+v", b.RoleRef, want)
	}
	if want := []rbacv1.Subject{{APIGroup: "rbac.authorization.k8s.io", Kind: "Group", Name: "kosh:clusterrole:view"}}; !slices.Equal(b.Subjects, want) {
		t.Errorf("subjects %+v, want %+v", b.Subjects, want)
	}
	if b.Labels["app.kubernetes.io/managed-by"] != "kosh" {
		t.Errorf("labels %v", b.Labels)
	}
	if got := b.Annotations["kosh.fables-for-robots.dev/role"]; got != "ClusterRole/view" {
		t.Errorf("annotation %q", got)
	}
	if recs := f.log.with("a binding of the key list is made"); len(recs) != 1 {
		t.Errorf("records %+v, want the binding's", f.log.all())
	}
}

func TestMakeCreatesRoleBindingInNamespace(t *testing.T) {
	f := newBindingsFixture(t, []keys.Role{editTeamA}, editTeamA)
	if err := f.b.Make(context.Background()); err != nil {
		t.Fatal(err)
	}
	b, ok := f.rb(t, "team-a", "kosh:role:edit")
	if !ok {
		t.Fatal("no RoleBinding kosh:role:edit in team-a")
	}
	if want := (rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "Role", Name: "edit"}); b.RoleRef != want {
		t.Errorf("roleRef %+v, want %+v", b.RoleRef, want)
	}
	if want := []rbacv1.Subject{{APIGroup: "rbac.authorization.k8s.io", Kind: "Group", Name: "kosh:role:team-a:edit"}}; !slices.Equal(b.Subjects, want) {
		t.Errorf("subjects %+v, want %+v", b.Subjects, want)
	}
	if b.Labels["app.kubernetes.io/managed-by"] != "kosh" {
		t.Errorf("labels %v", b.Labels)
	}
	if got := b.Annotations["kosh.fables-for-robots.dev/role"]; got != "Role/team-a/edit" {
		t.Errorf("annotation %q", got)
	}
	if _, ok := f.crb(t, "kosh:role:edit"); ok {
		t.Error("a Role got a ClusterRoleBinding")
	}
}

func TestMakeKeepsACorrectBinding(t *testing.T) {
	f := newBindingsFixture(t, []keys.Role{viewRole, editTeamA}, viewRole, editTeamA)
	if err := f.b.Make(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.e.typed.ClearActions()
	if err := f.b.Make(context.Background()); err != nil {
		t.Fatal(err)
	}
	if w := f.writes(); len(w) != 0 {
		t.Errorf("writes %v to correct bindings", w)
	}
	if recs := f.log.with("a binding of the key list is made"); len(recs) != 2 {
		t.Errorf("records %+v, want the two first ones only", recs)
	}
}

func TestMakeReplacesAnotherRoleRef(t *testing.T) {
	f := newBindingsFixture(t, []keys.Role{viewRole}, viewRole)
	ctx := context.Background()
	wrong := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "kosh:clusterrole:view", Labels: koshLabels},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacGroup, Kind: "ClusterRole", Name: "cluster-admin"},
		Subjects:   []rbacv1.Subject{{APIGroup: rbacGroup, Kind: "Group", Name: "kosh:clusterrole:view"}},
	}
	if _, err := f.e.typed.RbacV1().ClusterRoleBindings().Create(ctx, wrong, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	f.e.typed.ClearActions()
	if err := f.b.Make(ctx); err != nil {
		t.Fatal(err)
	}
	want := []string{"delete clusterrolebindings kosh:clusterrole:view", "create clusterrolebindings kosh:clusterrole:view"}
	if w := f.writes(); !slices.Equal(w, want) {
		t.Errorf("writes %v, want %v", w, want)
	}
	if b, _ := f.crb(t, "kosh:clusterrole:view"); b == nil || b.RoleRef.Name != "view" {
		t.Errorf("binding %+v, want the roleRef view", b)
	}
	if recs := f.log.with("a binding of the key list is replaced: its roleRef was another"); len(recs) != 1 {
		t.Errorf("records %+v", f.log.all())
	}
}

func TestMakeSetsSubjects(t *testing.T) {
	f := newBindingsFixture(t, []keys.Role{editTeamA}, editTeamA)
	ctx := context.Background()
	// Someone added a subject, and the binding lost kosh's annotation: the
	// subjects are set, the mark is put back, and other labels stay.
	wrong := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "kosh:role:edit",
			Labels: map[string]string{"app.kubernetes.io/managed-by": "kosh", "team": "a"}},
		RoleRef: rbacv1.RoleRef{APIGroup: rbacGroup, Kind: "Role", Name: "edit"},
		Subjects: []rbacv1.Subject{
			{APIGroup: rbacGroup, Kind: "Group", Name: "kosh:role:team-a:edit"},
			{APIGroup: rbacGroup, Kind: "User", Name: "mallory"},
		},
	}
	if _, err := f.e.typed.RbacV1().RoleBindings("team-a").Create(ctx, wrong, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	f.e.typed.ClearActions()
	if err := f.b.Make(ctx); err != nil {
		t.Fatal(err)
	}
	if w := f.writes(); !slices.Equal(w, []string{"update rolebindings team-a/kosh:role:edit"}) {
		t.Errorf("writes %v, want one update", w)
	}
	b, _ := f.rb(t, "team-a", "kosh:role:edit")
	if want := []rbacv1.Subject{{APIGroup: rbacGroup, Kind: "Group", Name: "kosh:role:team-a:edit"}}; !slices.Equal(b.Subjects, want) {
		t.Errorf("subjects %+v, want %+v", b.Subjects, want)
	}
	if b.Labels["team"] != "a" || b.Labels["app.kubernetes.io/managed-by"] != "kosh" {
		t.Errorf("labels %v", b.Labels)
	}
	if got := b.Annotations["kosh.fables-for-robots.dev/role"]; got != "Role/team-a/edit" {
		t.Errorf("annotation %q", got)
	}
}

// TestMakeMarksABindingAtItsName: a binding at the name kosh gives a role
// that lacks kosh's label, as an operator may have made it by hand, is
// given it, so that Prune deletes it when no key needs it any more.
func TestMakeMarksABindingAtItsName(t *testing.T) {
	f := newBindingsFixture(t, []keys.Role{viewRole}, viewRole)
	ctx := context.Background()
	bare := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "kosh:clusterrole:view"},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacGroup, Kind: "ClusterRole", Name: "view"},
		Subjects:   []rbacv1.Subject{{APIGroup: rbacGroup, Kind: "Group", Name: "kosh:clusterrole:view"}},
	}
	if _, err := f.e.typed.RbacV1().ClusterRoleBindings().Create(ctx, bare, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := f.b.Make(ctx); err != nil {
		t.Fatal(err)
	}
	b, _ := f.crb(t, "kosh:clusterrole:view")
	if b.Labels["app.kubernetes.io/managed-by"] != "kosh" || b.Annotations["kosh.fables-for-robots.dev/role"] != "ClusterRole/view" {
		t.Errorf("labels %v, annotations %v", b.Labels, b.Annotations)
	}
}

// TestPruneDeletesOnlyLabelledUnneeded is Review Focus 4: Prune deletes
// the bindings that carry kosh's label and that no role needs, and no
// other, whatever its name.
func TestPruneDeletesOnlyLabelledUnneeded(t *testing.T) {
	f := newBindingsFixture(t, nil, viewRole, editTeamA)
	ctx := context.Background()
	rbac := f.e.typed.RbacV1()
	crb := func(name, role string, labels map[string]string) {
		b := &rbacv1.ClusterRoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels},
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacGroup, Kind: "ClusterRole", Name: role},
		}
		if _, err := rbac.ClusterRoleBindings().Create(ctx, b, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	rb := func(ns, name string, labels map[string]string) {
		b := &rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Labels: labels},
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacGroup, Kind: "Role", Name: "edit"},
		}
		if _, err := rbac.RoleBindings(ns).Create(ctx, b, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	crb("kosh:clusterrole:view", "view", koshLabels)                                          // needed
	crb("kosh:clusterrole:x", "x", nil)                                                       // not kosh's: stays
	crb("kosh:clusterrole:cluster-admin", "cluster-admin", koshLabels)                        // unneeded: goes
	crb("someone-elses", "edit", map[string]string{"app": "other"})                           // not kosh's: stays
	rb("team-a", "kosh:role:edit", koshLabels)                                                // needed
	rb("team-a", "kosh:role:admin", koshLabels)                                               // unneeded: goes
	rb("team-b", "kosh:role:edit", koshLabels)                                                // unneeded: goes, though team-a's of that name is needed
	rb("team-b", "kosh:role:view", nil)                                                       // not kosh's: stays
	rb("team-c", "kosh:role:edit", map[string]string{"app.kubernetes.io/managed-by": "helm"}) // not kosh's: stays
	f.e.typed.ClearActions()

	if err := f.b.Prune(ctx); err != nil {
		t.Fatal(err)
	}
	got := f.writes()
	slices.Sort(got)
	want := []string{
		"delete clusterrolebindings kosh:clusterrole:cluster-admin",
		"delete rolebindings team-a/kosh:role:admin",
		"delete rolebindings team-b/kosh:role:edit",
	}
	if !slices.Equal(got, want) {
		t.Errorf("writes %v\nwant %v", got, want)
	}
	for _, name := range []string{"kosh:clusterrole:view", "kosh:clusterrole:x", "someone-elses"} {
		if _, ok := f.crb(t, name); !ok {
			t.Errorf("the ClusterRoleBinding %s is gone", name)
		}
	}
	for _, nn := range [][2]string{{"team-a", "kosh:role:edit"}, {"team-b", "kosh:role:view"}, {"team-c", "kosh:role:edit"}} {
		if _, ok := f.rb(t, nn[0], nn[1]); !ok {
			t.Errorf("the RoleBinding %s/%s is gone", nn[0], nn[1])
		}
	}
	if recs := f.log.with("a binding no key needs is deleted"); len(recs) != 3 {
		t.Errorf("records %+v, want the three deletions", f.log.all())
	}
}

func TestMissingRoleIsLoggedAndBound(t *testing.T) {
	f := newBindingsFixture(t, []keys.Role{viewRole}, viewRole, adminRole, editTeamB)
	if err := f.b.Make(context.Background()); err != nil {
		t.Fatal(err)
	}
	recs := f.log.with("the role of a key does not exist")
	var missing []string
	for _, r := range recs {
		if r.Level != slog.LevelWarn {
			t.Errorf("level %v", r.Level)
		}
		role, _ := r.attr("role")
		missing = append(missing, role)
	}
	if want := []string{"ClusterRole/cluster-admin", "Role/team-b/edit"}; !slices.Equal(missing, want) {
		t.Errorf("missing %v, want %v", missing, want)
	}
	// Bound all the same: the role works the moment it is made.
	if _, ok := f.crb(t, "kosh:clusterrole:cluster-admin"); !ok {
		t.Error("the missing ClusterRole is not bound")
	}
	if _, ok := f.rb(t, "team-b", "kosh:role:edit"); !ok {
		t.Error("the missing Role is not bound")
	}
	// At every reading.
	if err := f.b.Make(context.Background()); err != nil {
		t.Fatal(err)
	}
	if recs := f.log.with("the role of a key does not exist"); len(recs) != 4 {
		t.Errorf("%d records after a second Make, want 4", len(recs))
	}
}

// TestRoleThatCannotBeRead: a ServiceAccount with the least rights of §7
// may bind a role and not read it, so whether it exists is not known; that
// is no failure. Any other error reading it is one, and the binding is
// made all the same.
func TestRoleThatCannotBeRead(t *testing.T) {
	for _, tc := range []struct {
		name    string
		err     error
		failure bool
	}{
		{"forbidden", apierrors.NewForbidden(schema.GroupResource{Group: rbacGroup, Resource: "clusterroles"}, "view", errors.New("no")), false},
		{"the API fails", apierrors.NewInternalError(errors.New("etcd is down")), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newBindingsFixture(t, nil, viewRole)
			f.e.typed.PrependReactor("get", "clusterroles", func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, tc.err
			})
			err := f.b.Make(context.Background())
			if (err != nil) != tc.failure {
				t.Errorf("Make: %v, want a failure: %v", err, tc.failure)
			}
			if _, ok := f.crb(t, "kosh:clusterrole:view"); !ok {
				t.Error("not bound")
			}
			if recs := f.log.with("the role of a key does not exist"); len(recs) != 0 {
				t.Errorf("records %+v", recs)
			}
		})
	}
}

func TestRunRetriesAFailure(t *testing.T) {
	f := newBindingsFixture(t, []keys.Role{viewRole}, viewRole)
	f.b.retry = 5 * time.Millisecond
	var creates atomic.Int32
	f.e.typed.PrependReactor("create", "clusterrolebindings", func(k8stesting.Action) (bool, runtime.Object, error) {
		if creates.Add(1) <= 2 {
			// A webhook's text, which the log must not hold.
			return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: rbacGroup, Resource: "clusterrolebindings"}, "kosh:clusterrole:view", errors.New("the webhook says s3cr3t"))
		}
		return false, nil, nil
	})
	// The first reconcile, as kosh serve makes it at start.
	f.b.MakeNow()
	f.b.PruneNow()
	if _, ok := f.crb(t, "kosh:clusterrole:view"); ok {
		t.Fatal("made despite the failure")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.b.Run(ctx)
	}()
	eventually(t, "the binding, made on a retry", func() bool {
		_, ok := f.crb(t, "kosh:clusterrole:view")
		return ok
	})
	// Once it worked, nothing is retried.
	time.Sleep(50 * time.Millisecond)
	if n := creates.Load(); n != 3 {
		t.Errorf("%d creates, want 3", n)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return when its context ended")
	}
	recs := f.log.with("the bindings of the key list could not be made")
	if len(recs) != 2 {
		t.Fatalf("records %+v, want the two failures", f.log.all())
	}
	for _, r := range recs {
		if r.Level != slog.LevelError {
			t.Errorf("level %v", r.Level)
		}
		e, _ := r.attr("err")
		if strings.Contains(e, "s3cr3t") || !strings.Contains(e, "the API answered Forbidden (403)") || !strings.Contains(e, "kosh:clusterrole:view") {
			t.Errorf("err %q: the binding and the API's code, and nothing of its text", e)
		}
	}
}

// TestOrderOnChange is Review Focus 1: registered on the key list as kosh
// serve registers them, the bindings a narrowed key's new role needs are
// made before its sessions end, and the binding of its old role is deleted
// only after: no session acts without its binding, and none keeps one it
// should have lost once its sessions have ended.
func TestOrderOnChange(t *testing.T) {
	f := newBindingsFixture(t, []keys.Role{viewRole, adminRole})
	pub := newSSHKey(t)
	list := keys.NewList(keySet(t, pub, "alice/cluster-admin"))
	f.b.roles = list.Roles
	ctx := context.Background()
	if err := f.b.Make(ctx); err != nil {
		t.Fatal(err)
	}
	if err := f.b.Prune(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.crb(t, "kosh:clusterrole:cluster-admin"); !ok {
		t.Fatal("the first Make made no binding")
	}

	var ended []string // the bindings there when the sessions end
	list.OnChange(f.b.MakeNow)
	list.OnChange(func() {
		// What sshd's keysChanged does: the grant of alice's sessions is
		// no longer the list's, and they end.
		g, _ := list.Lookup(pub)
		if g.Role != viewRole {
			t.Errorf("the sessions end with alice's role %v", g.Role)
		}
		bs, err := f.e.typed.RbacV1().ClusterRoleBindings().List(ctx, metav1.ListOptions{})
		if err != nil {
			t.Fatal(err)
		}
		for _, b := range bs.Items {
			ended = append(ended, b.Name)
		}
	})
	list.OnChange(f.b.PruneNow)

	list.Replace(keySet(t, pub, "alice/view"))
	slices.Sort(ended)
	if want := []string{"kosh:clusterrole:cluster-admin", "kosh:clusterrole:view"}; !slices.Equal(ended, want) {
		t.Errorf("when the sessions end: %v, want %v", ended, want)
	}
	if _, ok := f.crb(t, "kosh:clusterrole:cluster-admin"); ok {
		t.Error("the binding alice lost is there after the change")
	}
	if _, ok := f.crb(t, "kosh:clusterrole:view"); !ok {
		t.Error("the binding alice needs is not there after the change")
	}
}

func newSSHKey(t *testing.T) ssh.PublicKey {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// keySet returns the key list of key named name.
func keySet(t *testing.T, key ssh.PublicKey, name string) *keys.Set {
	t.Helper()
	line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key))) + " " + name + "\n"
	set, rejected := keys.Parse(strings.NewReader(line))
	if len(rejected) != 0 {
		t.Fatalf("rejected %+v", rejected)
	}
	return set
}
