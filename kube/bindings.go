package kube

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/fables-for-robots/kosh/keys"
)

// What marks a binding as kosh's (§4 of the roles design): the label by
// which Prune finds them, and the annotation that says whose role it binds.
const (
	managedByLabel = "app.kubernetes.io/managed-by"
	managedByKosh  = "kosh"
	roleAnnotation = "kosh.fables-for-robots.dev/role"
)

const (
	// retryEvery is how often a reconcile that failed is tried again.
	retryEvery = 30 * time.Second
	// reconcileTimeout bounds a Make or Prune that the key list's change
	// calls: the sessions of a changed key end after it, so an API that
	// does not answer is not waited for longer.
	reconcileTimeout = 10 * time.Second
)

// msgBindingsFailed is logged when a Make or Prune fails.
const msgBindingsFailed = "the bindings of the key list could not be made"

// Bindings makes the ClusterRoleBindings and RoleBindings the roles of the
// key list need, and deletes those of kosh's that no role needs any more.
// It is the one place kosh writes RBAC, and does so as the ServiceAccount.
// All methods may be called from any goroutine; one Make or Prune runs at
// a time.
type Bindings struct {
	c     *Cluster
	roles func() []keys.Role
	log   *slog.Logger
	retry time.Duration // retryEvery; tests set less

	mu          sync.Mutex
	makeFailed  bool // the last Make failed
	pruneFailed bool // the last Prune failed
}

// NewBindings returns the reconciler of c's bindings, which reads the roles
// to bind from roles at every Make and Prune: the key list's Roles.
func NewBindings(c *Cluster, roles func() []keys.Role, log *slog.Logger) *Bindings {
	return &Bindings{c: c, roles: roles, log: log, retry: retryEvery}
}

// Make makes the bindings every role of roles() needs: it creates a missing
// one, replaces one whose roleRef is another (a roleRef cannot be changed),
// and sets the subjects and kosh's marks of one that has others. It logs a
// role that does not exist, and binds it all the same. It goes on past a
// failure, and returns them all.
func (b *Bindings) Make(ctx context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	var errs []error
	for _, r := range b.roles() {
		if err := b.checkRole(ctx, r); err != nil {
			errs = append(errs, err)
		}
		if err := b.bind(ctx, r); err != nil {
			errs = append(errs, err)
		}
	}
	err := errors.Join(errs...)
	b.makeFailed = err != nil
	return err
}

// Prune deletes the bindings that carry kosh's label and that no role of
// roles() needs: the ClusterRoleBindings, and the RoleBindings of every
// namespace. A binding without the label is never deleted, whatever its
// name.
func (b *Bindings) Prune(ctx context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	err := b.prune(ctx)
	b.pruneFailed = err != nil
	return err
}

// MakeNow is Make within reconcileTimeout, with a failure logged and left
// to Run: what the key list's change calls first, before the sessions of
// the keys that changed end.
func (b *Bindings) MakeNow() {
	ctx, cancel := context.WithTimeout(b.c.ctx, reconcileTimeout)
	defer cancel()
	if err := b.Make(ctx); err != nil {
		b.logFailure(err)
	}
}

// PruneNow is Prune within reconcileTimeout, with a failure logged and
// left to Run: what the key list's change calls last, after the sessions
// of the keys that changed have been ended.
func (b *Bindings) PruneNow() {
	ctx, cancel := context.WithTimeout(b.c.ctx, reconcileTimeout)
	defer cancel()
	if err := b.Prune(ctx); err != nil {
		b.logFailure(err)
	}
}

// Run makes and prunes again every 30 seconds while the last Make or Prune
// failed, until ctx is done. The reconciles of the key list's changes are
// MakeNow and PruneNow; Run only retries.
func (b *Bindings) Run(ctx context.Context) {
	tick := time.NewTicker(b.retry)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		b.mu.Lock()
		failed := b.makeFailed || b.pruneFailed
		b.mu.Unlock()
		if !failed {
			continue
		}
		rctx, cancel := context.WithTimeout(ctx, reconcileTimeout)
		err := errors.Join(b.Make(rctx), b.Prune(rctx))
		cancel()
		if err != nil {
			b.logFailure(err)
		}
	}
}

// checkRole logs r when it does not exist. That the ServiceAccount may not
// read it is no failure: one with the least rights (§7) may bind a role
// without reading it, and whether it exists is then not known.
func (b *Bindings) checkRole(ctx context.Context, r keys.Role) error {
	rbac := b.c.typed.RbacV1()
	var err error
	if r.Kind == keys.KindRole {
		_, err = rbac.Roles(r.Namespace).Get(ctx, r.Name, metav1.GetOptions{})
	} else {
		_, err = rbac.ClusterRoles().Get(ctx, r.Name, metav1.GetOptions{})
	}
	switch {
	case err == nil, apierrors.IsForbidden(err):
		return nil
	case apierrors.IsNotFound(err):
		b.log.Warn("the role of a key does not exist", "role", r.String())
		return nil
	default:
		return &bindingError{what: "reading the " + r.String(), err: err}
	}
}

// bind makes the binding of r right.
func (b *Bindings) bind(ctx context.Context, r keys.Role) error {
	rbac := b.c.typed.RbacV1()
	meta := metav1.ObjectMeta{
		Name:        bindingName(r),
		Namespace:   r.Namespace,
		Labels:      map[string]string{managedByLabel: managedByKosh},
		Annotations: map[string]string{roleAnnotation: r.String()},
	}
	ref := rbacv1.RoleRef{APIGroup: rbacGroup, Kind: r.Kind, Name: r.Name}
	subjects := []rbacv1.Subject{{APIGroup: rbacGroup, Kind: rbacv1.GroupKind, Name: r.Group()}}
	var done string
	var err error
	if r.Kind == keys.KindRole {
		want := &rbacv1.RoleBinding{ObjectMeta: meta, RoleRef: ref, Subjects: subjects}
		done, err = reconcile(ctx, rbac.RoleBindings(r.Namespace), want, roleBindingParts)
	} else {
		want := &rbacv1.ClusterRoleBinding{ObjectMeta: meta, RoleRef: ref, Subjects: subjects}
		done, err = reconcile(ctx, rbac.ClusterRoleBindings(), want, clusterRoleBindingParts)
	}
	if err != nil {
		return &bindingError{what: "making the binding " + bindingPath(r.Namespace, meta.Name), err: err}
	}
	if done != "" {
		b.log.Info(done, "binding", bindingPath(r.Namespace, meta.Name), "role", r.String())
	}
	return nil
}

// bindingName is the name of r's binding: "kosh:clusterrole:<role>" for a
// ClusterRole, "kosh:role:<role>" for a Role, in its namespace.
func bindingName(r keys.Role) string {
	if r.Kind == keys.KindRole {
		return "kosh:role:" + r.Name
	}
	return "kosh:clusterrole:" + r.Name
}

// bindingPath is how the log names a binding: "<namespace>/<name>" for a
// RoleBinding, the name for a ClusterRoleBinding.
func bindingPath(namespace, name string) string {
	if namespace == "" {
		return name
	}
	return namespace + "/" + name
}

// bindingAPI is what reconcile needs of the typed client of
// ClusterRoleBindings or of RoleBindings in a namespace.
type bindingAPI[B any] interface {
	Get(ctx context.Context, name string, opts metav1.GetOptions) (B, error)
	Create(ctx context.Context, b B, opts metav1.CreateOptions) (B, error)
	Update(ctx context.Context, b B, opts metav1.UpdateOptions) (B, error)
	Delete(ctx context.Context, name string, opts metav1.DeleteOptions) error
}

func clusterRoleBindingParts(b *rbacv1.ClusterRoleBinding) (*rbacv1.RoleRef, *[]rbacv1.Subject) {
	return &b.RoleRef, &b.Subjects
}

func roleBindingParts(b *rbacv1.RoleBinding) (*rbacv1.RoleRef, *[]rbacv1.Subject) {
	return &b.RoleRef, &b.Subjects
}

// reconcile makes the binding at want's name what want is, and returns
// what it did, as the log says it, or "" when the binding was right.
// parts gives a binding's roleRef and subjects.
func reconcile[B metav1.Object](ctx context.Context, api bindingAPI[B], want B, parts func(B) (*rbacv1.RoleRef, *[]rbacv1.Subject)) (done string, err error) {
	got, err := api.Get(ctx, want.GetName(), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = api.Create(ctx, want, metav1.CreateOptions{})
		return "a binding of the key list is made", err
	}
	if err != nil {
		return "", err
	}
	gotRef, gotSubjects := parts(got)
	wantRef, wantSubjects := parts(want)
	if *gotRef != *wantRef {
		// The API refuses to change a roleRef: the binding is deleted,
		// the one it was, and made again.
		err := api.Delete(ctx, got.GetName(), deleteThis(got.GetUID()))
		if err != nil && !apierrors.IsNotFound(err) {
			return "", err
		}
		_, err = api.Create(ctx, want, metav1.CreateOptions{})
		return "a binding of the key list is replaced: its roleRef was another", err
	}
	marked := got.GetLabels()[managedByLabel] == managedByKosh &&
		got.GetAnnotations()[roleAnnotation] == want.GetAnnotations()[roleAnnotation]
	if marked && slices.Equal(*gotSubjects, *wantSubjects) {
		return "", nil
	}
	// The subjects are set, and kosh's marks added to whatever labels
	// and annotations it has.
	*gotSubjects = *wantSubjects
	labels, annotations := maps.Clone(got.GetLabels()), maps.Clone(got.GetAnnotations())
	if labels == nil {
		labels = map[string]string{}
	}
	if annotations == nil {
		annotations = map[string]string{}
	}
	maps.Copy(labels, want.GetLabels())
	maps.Copy(annotations, want.GetAnnotations())
	got.SetLabels(labels)
	got.SetAnnotations(annotations)
	_, err = api.Update(ctx, got, metav1.UpdateOptions{})
	return "a binding of the key list is set right", err
}

func (b *Bindings) prune(ctx context.Context) error {
	needed := map[string]bool{} // by bindingPath
	for _, r := range b.roles() {
		needed[bindingPath(r.Namespace, bindingName(r))] = true
	}
	rbac := b.c.typed.RbacV1()
	byLabel := metav1.ListOptions{LabelSelector: managedByLabel + "=" + managedByKosh}
	var errs []error
	del := func(namespace, name string, api func() error) {
		path := bindingPath(namespace, name)
		if needed[path] {
			return
		}
		if err := api(); err != nil && !apierrors.IsNotFound(err) {
			errs = append(errs, &bindingError{what: "deleting the binding " + path, err: err})
			return
		}
		b.log.Info("a binding no key needs is deleted", "binding", path)
	}
	crbs, err := rbac.ClusterRoleBindings().List(ctx, byLabel)
	if err != nil {
		errs = append(errs, &bindingError{what: "listing kosh's ClusterRoleBindings", err: err})
	} else {
		for _, crb := range crbs.Items {
			del("", crb.Name, func() error {
				return rbac.ClusterRoleBindings().Delete(ctx, crb.Name, deleteThis(crb.UID))
			})
		}
	}
	rbs, err := rbac.RoleBindings("").List(ctx, byLabel)
	if err != nil {
		errs = append(errs, &bindingError{what: "listing kosh's RoleBindings", err: err})
	} else {
		for _, rb := range rbs.Items {
			del(rb.Namespace, rb.Name, func() error {
				return rbac.RoleBindings(rb.Namespace).Delete(ctx, rb.Name, deleteThis(rb.UID))
			})
		}
	}
	return errors.Join(errs...)
}

// deleteThis deletes the binding of that uid only: not one made again at
// its name since it was read, which may not be kosh's.
func deleteThis(uid types.UID) metav1.DeleteOptions {
	return metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}}
}

// bindingError is the failure of one request of a reconcile: what was
// asked, and the error.
type bindingError struct {
	what string // "making the binding kosh:clusterrole:view"
	err  error
}

func (e *bindingError) Error() string { return e.what + ": " + e.err.Error() }
func (e *bindingError) Unwrap() error { return e.err }

// logFailure logs a failed Make or Prune: each failure with what was
// asked, and of an error of the API only its reason and code, as kosh logs
// every error of the API (loggableError).
func (b *Bindings) logFailure(err error) {
	var failures []string
	for _, e := range unjoin(err) {
		var be *bindingError
		var api apierrors.APIStatus
		if errors.As(e, &be) && errors.As(be.err, &api) {
			failures = append(failures, be.what+": "+loggableError(be.what, be.err).Error())
		} else {
			failures = append(failures, e.Error())
		}
	}
	b.log.Error(msgBindingsFailed, "err", strings.Join(failures, "; "))
}

// unjoin returns the errors errors.Join joined in err, or err alone.
func unjoin(err error) []error {
	if j, ok := err.(interface{ Unwrap() []error }); ok {
		var out []error
		for _, e := range j.Unwrap() {
			out = append(out, unjoin(e)...)
		}
		return out
	}
	return []error{err}
}
