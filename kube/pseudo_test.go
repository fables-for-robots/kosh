package kube

import (
	"context"
	"errors"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	k8stesting "k8s.io/client-go/testing"

	"github.com/fables-for-robots/kosh/render"
)

func TestContainersOfAPod(t *testing.T) {
	p := readPodWith("default", "web", nil, "app", "sidecar")
	p.Spec.InitContainers = []corev1.Container{{Name: "init", Image: "init:1"}}
	// What the API sends has whole seconds.
	p.CreationTimestamp = metav1.NewTime(p.CreationTimestamp.Truncate(time.Second))
	e := newEnv(t, envConfig{opts: Options{IdleStop: 10 * time.Millisecond}}, p, readDeployment("default", "web", "web"))
	s := e.session(t)
	of := ObjectRef{Resource: e.resource(t, "", "pods"), Namespace: "default", Name: "web"}
	w := watchOf(t, s, Query{Resource: Containers, Of: &of})

	tab := rowsWhen(t, w, "default/init", "default/app", "default/sidecar")
	now := time.Now()
	tab = w.Table(now)
	if want := render.ContainerRows(p, now); !reflect.DeepEqual(tab.Rows, want) {
		t.Errorf("rows\n%+v\nwant render.ContainerRows\n%+v", tab.Rows, want)
	}
	if !reflect.DeepEqual(tab.Columns, render.ContainerColumns()) || tab.Namespaced || tab.Stale || tab.Err != nil {
		t.Errorf("table: columns %v, namespaced %v, stale %v, err %v", tab.Columns, tab.Namespaced, tab.Stale, tab.Err)
	}

	drain(w.Changed())
	changed := p.DeepCopy()
	changed.Spec.Containers = append(changed.Spec.Containers, corev1.Container{Name: "debug", Image: "busybox"})
	pods := e.dyn.Resource(of.Resource.GVR).Namespace("default")
	if _, err := pods.Update(context.Background(), toUnstructured(t, changed), metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	waitChanged(t, w)
	rowsWhen(t, w, "default/init", "default/app", "default/sidecar", "default/debug")

	drain(w.Changed())
	if err := pods.Delete(context.Background(), "web", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	waitChanged(t, w)
	eventually(t, "no rows for a pod that is gone", func() bool {
		tab := w.Table(time.Now())
		return len(tab.Rows) == 0 && tab.Err == nil
	})

	for _, bad := range []*ObjectRef{
		nil,
		{Resource: e.resource(t, "apps", "deployments"), Namespace: "default", Name: "web"},
	} {
		if _, err := s.Watch(Query{Resource: Containers, Of: bad}); !errors.Is(err, ErrNoRelation) {
			t.Errorf("Of %v: got %v, want ErrNoRelation", bad, err)
		}
	}

	w.Close()
	w.Close()
	eventually(t, "the pods' cache is idle and stopped", func() bool { return !e.c.running(render.Pods) })
}

func TestContainersFollowTheCacheStatus(t *testing.T) {
	e := newEnv(t, envConfig{before: func(e *env) {
		e.dyn.PrependReactor("list", "pods", func(k8stesting.Action) (bool, k8sruntime.Object, error) {
			return true, nil, errors.New("pods are forbidden")
		})
	}})
	of := ObjectRef{Resource: e.resource(t, "", "pods"), Namespace: "default", Name: "web"}
	w := watchOf(t, e.session(t), Query{Resource: Containers, Of: &of})
	eventually(t, "the list's error", func() bool {
		tab := w.Table(time.Now())
		return tab.Err != nil && len(tab.Rows) == 0
	})
}

func TestContainersKeepTheirRowsWhenARelistFails(t *testing.T) {
	var fw atomic.Pointer[watch.FakeWatcher]
	var lists atomic.Int32
	e := newEnv(t, envConfig{before: func(e *env) {
		e.dyn.PrependWatchReactor("pods", func(k8stesting.Action) (bool, watch.Interface, error) {
			if fw.Load() != nil {
				return true, nil, apierrors.NewTooManyRequests("the API is busy", 1)
			}
			w := watch.NewFakeWithChanSize(1, false)
			fw.Store(w)
			return true, w, nil
		})
		e.dyn.PrependReactor("list", "pods", func(k8stesting.Action) (bool, k8sruntime.Object, error) {
			if lists.Add(1) > 1 {
				return true, nil, errors.New("pods are forbidden")
			}
			return false, nil, nil
		})
	}}, readPodWith("default", "web", nil, "app"))
	of := ObjectRef{Resource: e.resource(t, "", "pods"), Namespace: "default", Name: "web"}
	w := watchOf(t, e.session(t), Query{Resource: Containers, Of: &of})
	rowsWhen(t, w, "default/app")
	eventually(t, "the watch", func() bool { return fw.Load() != nil })

	// The watch ends with an error, the relist fails.
	fw.Load().Error(&metav1.Status{Status: metav1.StatusFailure, Code: 500, Reason: metav1.StatusReasonInternalError, Message: "broken"})
	cache := w.(*containersWatch).cache
	eventually(t, "the failed relist in the cache", func() bool {
		_, _, err := cache.status()
		return err != nil
	})
	tab := w.Table(time.Now())
	if tab.Err != nil || !tab.Stale || len(tab.Rows) != 1 || len(tab.Columns) == 0 {
		t.Errorf("after a failed relist: err %v, stale %v, %d rows, %d columns; want the last rows, stale",
			tab.Err, tab.Stale, len(tab.Rows), len(tab.Columns))
	}
}

// readResources are standardResources and the bindings of roles.
func readResources() []*metav1.APIResourceList {
	rr := standardResources()
	for _, l := range rr {
		if l.GroupVersion == "rbac.authorization.k8s.io/v1" {
			l.APIResources = append(l.APIResources,
				metav1.APIResource{Name: "rolebindings", SingularName: "rolebinding", Namespaced: true, Kind: "RoleBinding", Verbs: allVerbs},
				metav1.APIResource{Name: "clusterrolebindings", SingularName: "clusterrolebinding", Kind: "ClusterRoleBinding", Verbs: allVerbs})
		}
	}
	return rr
}

func readRole(namespace, name string, rules ...rbacv1.PolicyRule) *rbacv1.Role {
	return &rbacv1.Role{ObjectMeta: objMeta(namespace, name), Rules: rules}
}

func readRule(group, resource string, verbs ...string) rbacv1.PolicyRule {
	return rbacv1.PolicyRule{APIGroups: []string{group}, Resources: []string{resource}, Verbs: verbs}
}

// readRulesWhen waits until the table's rows are those of rules.
func readRulesWhen(t *testing.T, w Watch, rules ...rbacv1.PolicyRule) Table {
	t.Helper()
	want := render.RuleRows(rules)
	var tab Table
	eventually(t, "the rows of the rules", func() bool {
		tab = w.Table(time.Now())
		return reflect.DeepEqual(tab.Rows, want)
	})
	return tab
}

func TestRulesOfARole(t *testing.T) {
	podsRule := readRule("", "pods", "get", "list")
	nsPodsRule := readRule("", "pods", "get", "list", "watch", "delete")
	allRule := readRule("*", "*", "*")
	e := newEnv(t, envConfig{resources: readResources()},
		readRole("default", "reader", podsRule),
		readRole("other", "reader", nsPodsRule),
		&rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: "reader"}, Rules: []rbacv1.PolicyRule{allRule}},
		&rbacv1.RoleBinding{ObjectMeta: objMeta("other", "to-role"),
			RoleRef: rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "Role", Name: "reader"}},
		&rbacv1.RoleBinding{ObjectMeta: objMeta("default", "to-clusterrole"),
			RoleRef: rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: "reader"}},
		&rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "crb"},
			RoleRef: rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: "reader"}},
	)
	s := e.session(t)
	roleBindings := e.resource(t, "rbac.authorization.k8s.io", "rolebindings")
	clusterRoleBindings := e.resource(t, "rbac.authorization.k8s.io", "clusterrolebindings")
	for _, tc := range []struct {
		name string
		of   ObjectRef
		want rbacv1.PolicyRule
	}{
		{"a role", ObjectRef{Resource: e.resource(t, "rbac.authorization.k8s.io", "roles"), Namespace: "default", Name: "reader"}, podsRule},
		{"a clusterrole", ObjectRef{Resource: e.resource(t, "rbac.authorization.k8s.io", "clusterroles"), Name: "reader"}, allRule},
		{"a rolebinding to a role, of its namespace", ObjectRef{Resource: roleBindings, Namespace: "other", Name: "to-role"}, nsPodsRule},
		{"a rolebinding to a clusterrole", ObjectRef{Resource: roleBindings, Namespace: "default", Name: "to-clusterrole"}, allRule},
		{"a clusterrolebinding", ObjectRef{Resource: clusterRoleBindings, Name: "crb"}, allRule},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := watchOf(t, s, Query{Resource: Rules, Of: &tc.of})
			tab := readRulesWhen(t, w, tc.want)
			if len(tab.Rows) == 0 || !reflect.DeepEqual(tab.Columns, render.RuleColumns()) || tab.Namespaced || tab.Stale || tab.Err != nil {
				t.Errorf("table: %d rows, columns %v, namespaced %v, stale %v, err %v", len(tab.Rows), tab.Columns, tab.Namespaced, tab.Stale, tab.Err)
			}
		})
	}
}

func TestRulesFollowAChange(t *testing.T) {
	first := readRule("", "pods", "get")
	e := newEnv(t, envConfig{opts: Options{TablePoll: 10 * time.Millisecond}}, readRole("default", "reader", first))
	of := ObjectRef{Resource: e.resource(t, "rbac.authorization.k8s.io", "roles"), Namespace: "default", Name: "reader"}
	w := watchOf(t, e.session(t), Query{Resource: Rules, Of: &of})
	readRulesWhen(t, w, first)

	drain(w.Changed())
	added := readRule("apps", "deployments", "get", "list")
	if _, err := e.typed.RbacV1().Roles("default").Update(context.Background(), readRole("default", "reader", first, added), metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	waitChanged(t, w)
	if got, want := w.Table(time.Now()).Rows, render.RuleRows([]rbacv1.PolicyRule{first, added}); !reflect.DeepEqual(got, want) {
		t.Errorf("after Changed: %+v, want %+v", got, want)
	}
}

func TestRulesErrors(t *testing.T) {
	var fail atomic.Bool
	var reads atomic.Int32
	e := newEnv(t, envConfig{opts: Options{TablePoll: 10 * time.Millisecond}, before: func(e *env) {
		e.typed.PrependReactor("get", "roles", func(k8stesting.Action) (bool, k8sruntime.Object, error) {
			reads.Add(1)
			if fail.Load() {
				return true, nil, errors.New("the API is down")
			}
			return false, nil, nil
		})
	}}, readRole("default", "reader", readRule("", "pods", "get")), pod("default", "web", nil))
	s := e.session(t)
	roles := e.resource(t, "rbac.authorization.k8s.io", "roles")

	t.Run("the role is not there", func(t *testing.T) {
		of := ObjectRef{Resource: roles, Namespace: "default", Name: "missing"}
		w := watchOf(t, s, Query{Resource: Rules, Of: &of})
		eventually(t, "Err", func() bool {
			tab := w.Table(time.Now())
			return tab.Err != nil && len(tab.Rows) == 0
		})
	})

	t.Run("not a role", func(t *testing.T) {
		for _, bad := range []*ObjectRef{nil, {Resource: e.resource(t, "", "pods"), Namespace: "default", Name: "web"}} {
			if _, err := s.Watch(Query{Resource: Rules, Of: bad}); !errors.Is(err, ErrNoRelation) {
				t.Errorf("Of %v: got %v, want ErrNoRelation", bad, err)
			}
		}
	})

	t.Run("a read that fails later", func(t *testing.T) {
		of := ObjectRef{Resource: roles, Namespace: "default", Name: "reader"}
		w := watchOf(t, s, Query{Resource: Rules, Of: &of})
		readRulesWhen(t, w, readRule("", "pods", "get"))
		fail.Store(true)
		defer fail.Store(false)
		eventually(t, "stale, with the rows kept", func() bool {
			tab := w.Table(time.Now())
			return tab.Stale && tab.Err == nil && len(tab.Rows) > 0
		})
		fail.Store(false)
		eventually(t, "fresh again", func() bool { return !w.Table(time.Now()).Stale })
	})

	t.Run("after Close", func(t *testing.T) {
		of := ObjectRef{Resource: roles, Namespace: "default", Name: "reader"}
		w := watchOf(t, s, Query{Resource: Rules, Of: &of})
		readRulesWhen(t, w, readRule("", "pods", "get"))
		w.Close()
		w.Close()
		n := reads.Load()
		time.Sleep(50 * time.Millisecond) // five polls
		if got := reads.Load(); got != n {
			t.Errorf("%d reads after Close", got-n)
		}
	})
}

func TestPseudoWatchesEndWithTheSession(t *testing.T) {
	e := newEnv(t, envConfig{opts: Options{IdleStop: 10 * time.Millisecond, TablePoll: 10 * time.Millisecond}},
		readPodWith("default", "web", nil, "app"), readRole("default", "reader", readRule("", "pods", "get")))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := e.c.Session(ctx, fullGrant)
	pod := ObjectRef{Resource: e.resource(t, "", "pods"), Namespace: "default", Name: "web"}
	containers, err := s.Watch(Query{Resource: Containers, Of: &pod})
	if err != nil {
		t.Fatal(err)
	}
	rowsWhen(t, containers, "default/app")
	role := ObjectRef{Resource: e.resource(t, "rbac.authorization.k8s.io", "roles"), Namespace: "default", Name: "reader"}
	rules, err := s.Watch(Query{Resource: Rules, Of: &role})
	if err != nil {
		t.Fatal(err)
	}
	readRulesWhen(t, rules, readRule("", "pods", "get"))

	cancel()
	eventually(t, "the pods' cache is released and stopped", func() bool { return !e.c.running(render.Pods) })
	eventually(t, "the rules' poll has ended", func() bool {
		select {
		case <-rules.(*rulesWatch).done:
			return true
		default:
			return false
		}
	})
}

func TestRulesOfABindingWhoseRoleIsGone(t *testing.T) {
	e := newEnv(t, envConfig{resources: readResources()}, &rbacv1.RoleBinding{ObjectMeta: objMeta("default", "orphan"),
		RoleRef: rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "Role", Name: "gone"}})
	of := ObjectRef{Resource: e.resource(t, "rbac.authorization.k8s.io", "rolebindings"), Namespace: "default", Name: "orphan"}
	w := watchOf(t, e.session(t), Query{Resource: Rules, Of: &of})
	eventually(t, "Err", func() bool {
		tab := w.Table(time.Now())
		return tab.Err != nil && len(tab.Rows) == 0
	})
	if err := w.Table(time.Now()).Err; !apierrors.IsNotFound(err) {
		t.Errorf("Err %v, want the role's not found", err)
	}
}
