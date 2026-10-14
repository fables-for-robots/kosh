package kube

import (
	"context"
	"fmt"
	"reflect"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/fables-for-robots/kosh/render"
)

const rbacGroup = "rbac.authorization.k8s.io"

// watchPseudo opens the table of Containers, whose Of is a pod, or of
// Rules, whose Of is a role, a clusterrole or a binding of one. Any
// other Of is ErrNoRelation.
func (s *session) watchPseudo(q Query) (Watch, error) {
	if q.Of == nil {
		return nil, ErrNoRelation
	}
	of := *q.Of
	if err := s.c.checkRef(of); err != nil {
		return nil, err
	}
	switch {
	case q.Resource.Is(Containers.GVR.Group, Containers.GVR.Resource) && of.Resource.Is("", "pods"):
		return s.watchContainers(of)
	case q.Resource.Is(Rules.GVR.Group, Rules.GVR.Resource) &&
		(of.Resource.Is(rbacGroup, "roles") || of.Resource.Is(rbacGroup, "clusterroles") ||
			of.Resource.Is(rbacGroup, "rolebindings") || of.Resource.Is(rbacGroup, "clusterrolebindings")):
		return s.watchRules(of)
	}
	return nil, ErrNoRelation
}

// containersWatch is the table of a pod's containers, as the pods' cache
// has the pod, metered while the API serves the metrics of pods.
type containersWatch struct {
	c         *Cluster
	cache     *kindCache
	namespace string
	name      string
	changed   changes
	metrics   *metricsHold

	unsubscribe func()
	stopCache   func() // ends the forwarding of the cache's signals
	closeOnce   sync.Once

	mu        sync.Mutex
	stopAfter func() bool // ends the Close that comes with the session's end
}

func (s *session) watchContainers(pod ObjectRef) (Watch, error) {
	k, err := s.c.acquire(pod.Resource.GVR)
	if err != nil {
		return nil, err
	}
	changed, unsubscribe := k.subscribe()
	// The cache's signals and the poller's are one Changed.
	ch := newChanges()
	w := &containersWatch{
		c:           s.c,
		cache:       k,
		namespace:   pod.Namespace,
		name:        pod.Name,
		changed:     ch,
		metrics:     &metricsHold{c: s.c, gvr: PodMetrics, changed: ch},
		unsubscribe: unsubscribe,
		stopCache:   ch.forward(changed, s.c.ctx.Done()),
	}
	w.mu.Lock()
	w.stopAfter = context.AfterFunc(s.ctx, w.Close)
	w.mu.Unlock()
	return w, nil
}

// Table returns the rows of the pod's containers; none when the pod is
// gone. As the tables of the typed kinds: Err when the cache's first
// list failed, and the last rows, Stale, when a later one did; the
// metric columns while the API serves the metrics of pods, and not
// without.
func (w *containersWatch) Table(now time.Time) Table {
	poller := w.metrics.follow()
	synced, stale, err := w.cache.status()
	if err != nil && !synced {
		return Table{Err: err}
	}
	t := Table{Columns: render.ContainerColumns(), Stale: stale || err != nil}
	if synced {
		if o, ok := w.cache.get(w.namespace, w.name); ok {
			if p, ok := o.(*corev1.Pod); ok {
				t.Rows = render.ContainerRows(p, now)
				if usage := readingOf(poller); usage != nil {
					render.MeterContainers(t.Rows, p, usage[metricsKey(p)])
				}
			}
		}
	} else {
		t.Stale = true
	}
	if poller == nil {
		t.Columns, t.Rows = render.StripMetrics(t.Columns, t.Rows)
	}
	return t
}

func (w *containersWatch) Changed() <-chan struct{} { return w.changed }

func (w *containersWatch) Close() {
	w.closeOnce.Do(func() {
		w.mu.Lock()
		stop := w.stopAfter
		w.mu.Unlock()
		stop()
		w.metrics.close()
		w.stopCache()
		w.unsubscribe()
		w.c.release(w.cache)
	})
}

// rulesWatch is the table of the rules of a role, read with the typed
// client now and every opts.TablePoll by a goroutine of its own.
type rulesWatch struct {
	c         *Cluster
	of        ObjectRef
	changed   chan struct{}
	cancel    context.CancelFunc
	done      chan struct{} // closed when the poll has ended
	closeOnce sync.Once

	mu        sync.Mutex
	stopAfter func() bool // ends the Close that comes with the session's end
	rules     []rbacv1.PolicyRule
	known     bool  // a read has worked
	err       error // of the last read, when none has worked
	stale     bool  // the last read failed after one that worked
}

func (s *session) watchRules(of ObjectRef) (Watch, error) {
	c := s.c
	ctx, cancel := context.WithCancel(c.ctx)
	w := &rulesWatch{
		c:       c,
		of:      of,
		changed: make(chan struct{}, 1),
		cancel:  cancel,
		done:    make(chan struct{}),
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		cancel()
		return nil, errClosed
	}
	c.wg.Add(1)
	c.mu.Unlock()
	go w.poll(ctx)

	w.mu.Lock()
	w.stopAfter = context.AfterFunc(s.ctx, w.Close)
	w.mu.Unlock()
	return w, nil
}

// poll reads the rules now and every opts.TablePoll until ctx is done.
func (w *rulesWatch) poll(ctx context.Context) {
	defer w.c.wg.Done()
	defer close(w.done)
	t := time.NewTicker(w.c.opts.TablePoll)
	defer t.Stop()
	for {
		w.read(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// read reads the rules once and signals when what the watch shows
// changed.
func (w *rulesWatch) read(ctx context.Context) {
	rules, err := w.c.rulesOf(ctx, w.of)
	if ctx.Err() != nil {
		return
	}
	w.mu.Lock()
	var changed bool
	switch {
	case err != nil && !w.known:
		changed = w.err == nil || w.err.Error() != err.Error()
		w.err = err
	case err != nil:
		changed = !w.stale
		w.stale = true
	default:
		changed = !w.known || w.stale || !reflect.DeepEqual(render.RuleRows(w.rules), render.RuleRows(rules))
		w.rules, w.known, w.err, w.stale = rules, true, nil, false
	}
	w.mu.Unlock()
	if changed {
		select {
		case w.changed <- struct{}{}:
		default:
		}
	}
}

func (w *rulesWatch) Table(time.Time) Table {
	w.mu.Lock()
	rules, known, err, stale := w.rules, w.known, w.err, w.stale
	w.mu.Unlock()
	switch {
	case known:
		return Table{Columns: render.RuleColumns(), Rows: render.RuleRows(rules), Stale: stale}
	case err != nil:
		return Table{Err: err}
	default:
		return Table{Columns: render.RuleColumns(), Stale: true}
	}
}

func (w *rulesWatch) Changed() <-chan struct{} { return w.changed }

func (w *rulesWatch) Close() {
	w.closeOnce.Do(func() {
		w.mu.Lock()
		stop := w.stopAfter
		w.mu.Unlock()
		if stop != nil {
			stop()
		}
		w.cancel()
		<-w.done
	})
}

// rulesOf reads the rules of a role or clusterrole, or of the role or
// clusterrole a binding names.
func (c *Cluster) rulesOf(ctx context.Context, of ObjectRef) ([]rbacv1.PolicyRule, error) {
	get := metav1.GetOptions{}
	rbac := c.typed.RbacV1()
	switch {
	case of.Resource.Is(rbacGroup, "roles"):
		r, err := rbac.Roles(of.Namespace).Get(ctx, of.Name, get)
		if err != nil {
			return nil, err
		}
		return r.Rules, nil
	case of.Resource.Is(rbacGroup, "clusterroles"):
		r, err := rbac.ClusterRoles().Get(ctx, of.Name, get)
		if err != nil {
			return nil, err
		}
		return r.Rules, nil
	case of.Resource.Is(rbacGroup, "rolebindings"):
		b, err := rbac.RoleBindings(of.Namespace).Get(ctx, of.Name, get)
		if err != nil {
			return nil, err
		}
		return c.rulesOfRole(ctx, b.Namespace, b.RoleRef)
	case of.Resource.Is(rbacGroup, "clusterrolebindings"):
		b, err := rbac.ClusterRoleBindings().Get(ctx, of.Name, get)
		if err != nil {
			return nil, err
		}
		return c.rulesOfRole(ctx, "", b.RoleRef)
	}
	return nil, ErrNoRelation
}

// rulesOfRole reads the rules of the role a binding of namespace names:
// a Role of that namespace, or a ClusterRole.
func (c *Cluster) rulesOfRole(ctx context.Context, namespace string, ref rbacv1.RoleRef) ([]rbacv1.PolicyRule, error) {
	switch {
	case ref.Kind == "Role" && namespace != "":
		return c.rulesOf(ctx, ObjectRef{Resource: Resource{GVR: rbacv1.SchemeGroupVersion.WithResource("roles")}, Namespace: namespace, Name: ref.Name})
	case ref.Kind == "ClusterRole":
		return c.rulesOf(ctx, ObjectRef{Resource: Resource{GVR: rbacv1.SchemeGroupVersion.WithResource("clusterroles")}, Name: ref.Name})
	}
	return nil, fmt.Errorf("the binding names a %s, which has no rules kosh can read", ref.Kind)
}
