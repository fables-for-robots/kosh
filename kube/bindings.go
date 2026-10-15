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

	authorizationv1 "k8s.io/api/authorization/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/fables-for-robots/kosh/keys"
)

// What marks a binding as kosh's (§4 of the roles design): the labels by
// which Prune finds an instance's own, and the annotation that says whose
// role it binds.
const (
	managedByLabel = "app.kubernetes.io/managed-by"
	managedByKosh  = "kosh"
	instanceLabel  = "kosh.fables-for-robots.dev/instance"
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
	// msgMayEditList is the warning of §4.1 of the roles design.
	msgMayEditList = "a key's role may edit the key list, and so grant itself any role"
)

// Bindings makes the ClusterRoleBindings and RoleBindings the roles of the
// key list need, and deletes those of kosh's that no role needs any more.
// It is the one place kosh writes RBAC, and does so as the ServiceAccount.
// All methods may be called from any goroutine; one Make or Prune runs at
// a time.
type Bindings struct {
	c *Cluster
	// instance is the Cluster's instance, whose name is in the bindings'
	// names, groups and labels.
	instance string
	roles    func() []keys.RoleKeys
	log      *slog.Logger
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
	// foreign holds the bindings at kosh's names that another manager,
	// or no one, has, which the log has named: each is named once.
	foreign map[string]bool

	// edMu guards what WarnEditors has said once.
	edMu sync.Mutex
	// noNamespace is set once the log has said that kosh's namespace is
	// not known; editorsUnchecked once it has said that a role's rights
	// on the key list could not be asked.
	noNamespace, editorsUnchecked bool
}

// NewBindings returns the reconciler of c's bindings, which reads the roles
// to bind, with their keys, from roles at every Make and Prune: the key
// list's Roles.
func NewBindings(c *Cluster, roles func() []keys.RoleKeys, log *slog.Logger) *Bindings {
	return &Bindings{c: c, instance: c.opts.Instance, roles: roles, log: log, retry: retryEvery, timeout: reconcileTimeout, foreign: map[string]bool{}}
}

// Make makes the bindings every role of roles() needs: it creates a missing
// one, replaces one whose roleRef is another (a roleRef cannot be changed),
// and sets the subjects and kosh's annotation of one that has others. A
// binding at kosh's name that another manager's label, or another
// instance's, claims is left as it is; one that nobody's label claims is
// adopted when it already binds what kosh would, and otherwise left as it
// is. It logs a role that does not exist, with its keys, and binds it all
// the same. It goes on past a failure, and returns them all.
func (b *Bindings) Make(ctx context.Context) error {
	return b.locked(ctx, 0, b.makeReading, &b.makeFailure)
}

// Prune deletes the bindings that carry kosh's labels with this
// instance's name and that no role of roles() needs: the
// ClusterRoleBindings, and the RoleBindings of every namespace. A binding
// without the labels, or another instance's, is never deleted, whatever
// its name.
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

// WarnEditors logs, for each role of the key list but the ClusterRole
// cluster-admin, whether the role may update or patch ConfigMaps in kosh's
// own namespace: a key of such a role can edit the key list, and so name
// any role kosh may bind (§4.1 of the roles design). It asks the API
// server as the role, as the role's caches read. Without kosh's
// namespace it says once, at debug level, that it checks nothing; a
// question the API does not answer is said once, and is no failure.
func (b *Bindings) WarnEditors(ctx context.Context) {
	ns := b.c.opts.Namespace
	if ns == "" {
		b.edMu.Lock()
		first := !b.noNamespace
		b.noNamespace = true
		b.edMu.Unlock()
		if first {
			b.log.Debug("kosh's namespace is not known: whether a key's role may edit the key list is not checked")
		}
		return
	}
	for _, rk := range b.roles() {
		r := rk.Role
		if r.Kind == keys.KindClusterRole && r.Name == "cluster-admin" {
			continue
		}
		may, err := b.mayEditList(ctx, r, ns)
		if err != nil {
			b.edMu.Lock()
			first := !b.editorsUnchecked
			b.editorsUnchecked = true
			b.edMu.Unlock()
			if first {
				b.log.Warn("whether a key's role may edit the key list could not be asked", "role", r.String(),
					"err", canError(err).Error())
			}
			continue
		}
		if may {
			b.log.Warn(msgMayEditList, "role", r.String(), "keys", strings.Join(rk.Keys, ", "), "namespace", ns)
		}
	}
}

// WarnEditorsNow is WarnEditors within reconcileTimeout: what the key
// list's change calls after PruneNow, and serve at start.
func (b *Bindings) WarnEditorsNow() {
	ctx, cancel := context.WithTimeout(b.c.ctx, b.timeout)
	defer cancel()
	b.WarnEditors(ctx)
}

// mayEditList asks the API server, as r's identity, whether r may update
// or patch ConfigMaps in namespace.
func (b *Bindings) mayEditList(ctx context.Context, r keys.Role, namespace string) (bool, error) {
	api, err := b.c.clientsFor(roleIdentity(r, b.instance))
	if err != nil {
		return false, err
	}
	for _, verb := range []string{"update", "patch"} {
		review := &authorizationv1.SelfSubjectAccessReview{Spec: authorizationv1.SelfSubjectAccessReviewSpec{
			ResourceAttributes: &authorizationv1.ResourceAttributes{Verb: verb, Resource: "configmaps", Namespace: namespace},
		}}
		got, err := api.typed.AuthorizationV1().SelfSubjectAccessReviews().Create(ctx, review, metav1.CreateOptions{})
		if err != nil {
			return false, err
		}
		if got.Status.Allowed {
			return true, nil
		}
	}
	return false, nil
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
	instance := b.instance
	meta := metav1.ObjectMeta{
		Name:        bindingName(r, instance),
		Namespace:   r.Namespace,
		Labels:      map[string]string{managedByLabel: managedByKosh, instanceLabel: instance},
		Annotations: map[string]string{roleAnnotation: r.String()},
	}
	ref := rbacv1.RoleRef{APIGroup: rbacGroup, Kind: r.Kind, Name: r.Name}
	subjects := []rbacv1.Subject{{APIGroup: rbacGroup, Kind: rbacv1.GroupKind, Name: r.Group(instance)}}
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
	var left *notKoshs
	switch {
	case errors.As(err, &other):
		if !b.foreign[path] {
			b.foreign[path] = true
			b.log.Warn("a binding at kosh's name has another manager: it is left as it is", "binding", path,
				"manager", other.manager, "role", r.String())
		}
		return nil
	case errors.As(err, &left):
		if !b.foreign[path] {
			b.foreign[path] = true
			b.log.Warn("a binding at kosh's name is not kosh's: it is left as it is", "binding", path,
				"reason", left.reason, "role", r.String())
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

// bindingName is the name of r's binding for the kosh instance named
// instance: "kosh:<instance>:clusterrole:<role>" for a ClusterRole,
// "kosh:<instance>:role:<role>" for a Role, in its namespace.
func bindingName(r keys.Role, instance string) string {
	if r.Kind == keys.KindRole {
		return "kosh:" + instance + ":role:" + r.Name
	}
	return "kosh:" + instance + ":clusterrole:" + r.Name
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
	instance := want.GetLabels()[instanceLabel]
	manager, managed := got.GetLabels()[managedByLabel]
	theirs := got.GetLabels()[instanceLabel]
	switch {
	case managed && manager != managedByKosh:
		return "", &otherManager{manager: manager}
	case managed && theirs != "" && theirs != instance:
		return "", &otherManager{manager: "kosh, instance " + theirs}
	}
	gotRef, gotSubjects := parts(got)
	wantRef, wantSubjects := parts(want)
	if !managed || theirs == "" {
		// A binding at kosh's name that nobody marked is kosh's to take
		// when it already binds what kosh would: someone made it by hand,
		// as one may for a role kosh may not bind. Any other is left to
		// whoever made it.
		if *gotRef != *wantRef || !slices.Equal(*gotSubjects, *wantSubjects) {
			return "", &notKoshs{reason: "it binds another role or other subjects, and has no kosh label"}
		}
		mark(got, want)
		_, err = api.Update(ctx, got, metav1.UpdateOptions{})
		if apierrors.IsForbidden(err) {
			return "", &notKoshs{reason: "kosh may not update it to put its labels on it; it binds what kosh would"}
		}
		return "a binding at kosh's name is adopted", err
	}
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
	if got.GetAnnotations()[roleAnnotation] == want.GetAnnotations()[roleAnnotation] && slices.Equal(*gotSubjects, *wantSubjects) {
		return "", nil
	}
	// The subjects are set, and kosh's marks added to whatever labels
	// and annotations it has.
	*gotSubjects = *wantSubjects
	mark(got, want)
	_, err = api.Update(ctx, got, metav1.UpdateOptions{})
	return "a binding of the key list is set right", err
}

// mark adds want's labels and annotations, kosh's marks, to whatever
// labels and annotations got has.
func mark(got, want metav1.Object) {
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
}

func (b *Bindings) prune(ctx context.Context) error {
	instance := b.instance
	needed := map[string]bool{} // by bindingPath
	for _, rk := range b.roles() {
		needed[bindingPath(rk.Role.Namespace, bindingName(rk.Role, instance))] = true
	}
	rbac := b.c.typed.RbacV1()
	// This instance's own: another kosh's bindings are never touched.
	byLabel := metav1.ListOptions{LabelSelector: managedByLabel + "=" + managedByKosh + "," + instanceLabel + "=" + instance}
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
	what string // "making the binding kosh:kosh:clusterrole:view"
	err  error
}

func (e *bindingError) Error() string { return e.what + ": " + e.err.Error() }
func (e *bindingError) Unwrap() error { return e.err }

// otherManager is what reconcile answers for a binding at kosh's name
// whose label names another manager, such as helm or argocd: kosh leaves
// it as it is rather than fight that manager, and Prune never sees it.
type otherManager struct{ manager string }

func (e *otherManager) Error() string { return "the binding is managed by " + e.manager }

// notKoshs is what reconcile answers for a binding at kosh's name that
// nobody's label claims and that kosh did not adopt, and why: it binds
// something else, or kosh may not label it. kosh leaves it as it is.
type notKoshs struct{ reason string }

func (e *notKoshs) Error() string { return "the binding is not kosh's: " + e.reason }

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
