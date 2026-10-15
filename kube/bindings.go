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
	// reconcileTimeout bounds each Make or Prune of MakeNow, PruneNow and
	// Run, from the moment it may run: the sessions of a changed key end
	// after MakeNow, so an API that does not answer is not waited for
	// longer.
	reconcileTimeout = 10 * time.Second
)

// What the log says of the reconciles: a failure when it begins or its
// reason changes, and the end of a failure.
const (
	msgBindingsFailed = "the bindings of the key list could not be made"
	msgBindingsMade   = "the bindings of the key list are made"
)

// Bindings makes the ClusterRoleBindings and RoleBindings the roles of the
// key list need, and deletes those of kosh's that no role needs any more.
// It is the one place kosh writes RBAC, and does so as the ServiceAccount.
// All methods may be called from any goroutine; one Make or Prune runs at
// a time.
type Bindings struct {
	c     *Cluster
	roles func() []keys.RoleKeys
	log   *slog.Logger
	// retry and timeout are retryEvery and reconcileTimeout; tests set
	// less.
	retry, timeout time.Duration

	mu sync.Mutex
	// What the last Make and the last Prune failed with, as the log says
	// it; "" when it worked. Run retries while either is set.
	makeFailure, pruneFailure string
	// logged is the failure the log last said, "" when none or once it
	// said that the failure ended.
	logged string
	// uncheckable is set once the log has said that kosh may not read
	// the roles.
	uncheckable bool
	// foreign holds the bindings at kosh's names that another manager
	// has, which the log has named: each is named once.
	foreign map[string]bool
}

// NewBindings returns the reconciler of c's bindings, which reads the roles
// to bind, with their keys, from roles at every Make and Prune: the key
// list's Roles.
func NewBindings(c *Cluster, roles func() []keys.RoleKeys, log *slog.Logger) *Bindings {
	return &Bindings{c: c, roles: roles, log: log, retry: retryEvery, timeout: reconcileTimeout, foreign: map[string]bool{}}
}

// Make makes the bindings every role of roles() needs: it creates a missing
// one, replaces one whose roleRef is another (a roleRef cannot be changed),
// and sets the subjects and kosh's marks of one that has others. A binding
// at kosh's name that another manager's label claims is left as it is. It
// logs a role that does not exist, with its keys, and binds it all the
// same. It goes on past a failure, and returns them all.
func (b *Bindings) Make(ctx context.Context) error {
	return b.locked(ctx, 0, b.makeReading, &b.makeFailure)
}

// Prune deletes the bindings that carry kosh's label and that no role of
// roles() needs: the ClusterRoleBindings, and the RoleBindings of every
// namespace. A binding without the label is never deleted, whatever its
// name.
func (b *Bindings) Prune(ctx context.Context) error {
	return b.locked(ctx, 0, b.prune, &b.pruneFailure)
}

// MakeNow is Make within reconcileTimeout, with a failure left to Run:
// what the key list's change calls first, before the sessions of the keys
// that changed end.
func (b *Bindings) MakeNow() {
	_ = b.locked(b.c.ctx, b.timeout, b.makeReading, &b.makeFailure)
}

// PruneNow is Prune within reconcileTimeout, with a failure left to Run:
// what the key list's change calls last, after the sessions of the keys
// that changed have been ended.
func (b *Bindings) PruneNow() {
	_ = b.locked(b.c.ctx, b.timeout, b.prune, &b.pruneFailure)
}

// Run makes and prunes again on every tick of the retry interval (30
// seconds) while the last Make or Prune failed, until ctx is done. The
// reconciles of the key list's changes are MakeNow and PruneNow; Run only
// retries, and does not log a missing role again, since the list has not
// been read again.
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
		failed := b.makeFailure != "" || b.pruneFailure != ""
		b.mu.Unlock()
		if !failed {
			continue
		}
		_ = b.locked(ctx, b.timeout, b.makeRetry, &b.makeFailure)
		_ = b.locked(ctx, b.timeout, b.prune, &b.pruneFailure)
	}
}

// locked runs step under b.mu, within timeout of taking it when timeout is
// not zero, keeps what it failed with in failure, and logs what the
// failures of Make and Prune now are: an error when one begins or its
// reason changes, and the end of one.
func (b *Bindings) locked(ctx context.Context, timeout time.Duration, step func(context.Context) error, failure *string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	err := step(ctx)
	*failure = ""
	if err != nil {
		*failure = loggableBindingsError(err)
	}
	now := strings.Join(slices.DeleteFunc([]string{b.makeFailure, b.pruneFailure}, func(f string) bool { return f == "" }), "; ")
	switch {
	case now == b.logged:
		if err != nil {
			b.log.Debug(msgBindingsFailed, "err", now, "again", true)
		}
	case now == "":
		b.log.Info(msgBindingsMade)
	default:
		b.log.Error(msgBindingsFailed, "err", now)
	}
	b.logged = now
	return err
}

// makeReading is Make at a reading of the key list, which logs the roles
// that do not exist; makeRetry is Make on Run's retry, which does not.
func (b *Bindings) makeReading(ctx context.Context) error { return b.make(ctx, true) }
func (b *Bindings) makeRetry(ctx context.Context) error   { return b.make(ctx, false) }

func (b *Bindings) make(ctx context.Context, reading bool) error {
	var errs []error
	for _, rk := range b.roles() {
		if err := b.checkRole(ctx, rk, reading); err != nil {
			errs = append(errs, err)
		}
		if err := b.makeBinding(ctx, rk.Role); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// checkRole logs, at a reading, a role that does not exist, with its keys.
// That the ServiceAccount may not read the roles is no failure, since it
// may bind them all the same, and is logged once: a missing role then
// goes unsaid.
func (b *Bindings) checkRole(ctx context.Context, rk keys.RoleKeys, reading bool) error {
	r := rk.Role
	rbac := b.c.typed.RbacV1()
	var err error
	if r.Kind == keys.KindRole {
		_, err = rbac.Roles(r.Namespace).Get(ctx, r.Name, metav1.GetOptions{})
	} else {
		_, err = rbac.ClusterRoles().Get(ctx, r.Name, metav1.GetOptions{})
	}
	switch {
	case err == nil:
		return nil
	case apierrors.IsForbidden(err):
		if !b.uncheckable {
			b.uncheckable = true
			b.log.Warn("the role of a key cannot be checked", "role", r.String(), "keys", strings.Join(rk.Keys, ", "),
				"reason", "kosh may not read the roles: a role that does not exist is not reported")
		}
		return nil
	case apierrors.IsNotFound(err):
		if reading {
			b.log.Warn("the role of a key does not exist", "role", r.String(), "keys", strings.Join(rk.Keys, ", "))
		}
		return nil
	default:
		return &bindingError{what: "reading the " + r.String(), err: err}
	}
}

// makeBinding makes the binding of r right, unless another manager has it.
func (b *Bindings) makeBinding(ctx context.Context, r keys.Role) error {
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
	path := bindingPath(r.Namespace, meta.Name)
	var other *otherManager
	if errors.As(err, &other) {
		if !b.foreign[path] {
			b.foreign[path] = true
			b.log.Warn("a binding at kosh's name has another manager: it is left as it is", "binding", path,
				"manager", other.manager, "role", r.String())
		}
		return nil
	}
	delete(b.foreign, path)
	if err != nil {
		return &bindingError{what: "making the binding " + path, err: err}
	}
	if done != "" {
		b.log.Info(done, "binding", path, "role", r.String())
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
	if m, ok := got.GetLabels()[managedByLabel]; ok && m != managedByKosh {
		return "", &otherManager{manager: m}
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
	// A binding without the label is kosh's to take: §4 gives it the name.
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
	for _, rk := range b.roles() {
		needed[bindingPath(rk.Role.Namespace, bindingName(rk.Role))] = true
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

// otherManager is what reconcile answers for a binding at kosh's name
// whose label names another manager, such as helm or argocd: kosh leaves
// it as it is rather than fight that manager, and Prune never sees it.
type otherManager struct{ manager string }

func (e *otherManager) Error() string { return "the binding is managed by " + e.manager }

// loggableBindingsError returns a failed Make or Prune as the log says it:
// each failure with what was asked, and of an error of the API only its
// reason and code, as kosh logs every error of the API (loggableError).
func loggableBindingsError(err error) string {
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
	return strings.Join(failures, "; ")
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
