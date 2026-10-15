// Package kubetest is a kube.Session for tests: it shows what a test
// puts into it and remembers what was asked of it. It talks to no
// cluster.
package kubetest

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/fables-for-robots/kosh/keys"
	"github.com/fables-for-robots/kosh/kube"
	"github.com/fables-for-robots/kosh/render"
)

// Call is one call of a method of kube.Actions, or one that a test
// wants to see: its name and its arguments after the context.
type Call struct {
	Method string
	Args   []any
}

// Session is a kube.Session that a test fills. Its exported fields are
// set before the session is used; the Set methods may be called at any
// time, from any goroutine.
type Session struct {
	// GrantValue is what Grant returns. The fake refuses nothing by
	// it: the API server is what refuses a role, and a test that wants
	// a refusal puts it into Errs.
	GrantValue keys.Grant
	// Version is what ClusterVersion returns.
	Version string
	// ResourceList is what Resources returns and Resolve looks in.
	ResourceList []kube.Resource

	// What the reading methods return, by ObjectRef.String().
	YAMLs        map[string]string
	Descriptions map[string]string
	Secrets      map[string][]kube.SecretEntry
	Owners       map[string]kube.ObjectRef
	ImagesOf     map[string][]kube.ContainerImage
	ReplicasOf   map[string]int32
	// Errs is the error a method returns instead of its result, by the
	// method's name: "Delete", "YAML", "Watch".
	Errs map[string]error
	// TriggerName is the job name Trigger returns.
	TriggerName string
	// ExecResult is what Exec returns when Errs has no error for it and
	// ExecDo is nil.
	ExecResult kube.ExecResult
	// ExecDo, when set, is what Exec does after its call was recorded,
	// with the options it was given: a test that types into a shell
	// and reads what it prints sets it.
	ExecDo func(ctx context.Context, pod kube.ObjectRef, opts kube.ExecOptions) (kube.ExecResult, error)

	mu      sync.Mutex
	watches map[string]*Watch
	tables  map[string]kube.Table
	logs    map[string]*LogStream
	calls   []Call
	usage   kube.ClusterUsage
	usageOK bool
	usages  []*UsageWatch
	allowed []right
}

// right is one right Allow gave: each field is a value or "*" for any.
type right struct {
	verb, group, resource, subresource, namespace string
}

var _ kube.Session = (*Session)(nil)

// New returns a session with the grant, the version "v1.33.1" and the
// resources of Standard.
func New(grant keys.Grant) *Session {
	return &Session{
		GrantValue:   grant,
		Version:      "v1.33.1",
		ResourceList: Standard(),
		YAMLs:        map[string]string{},
		Descriptions: map[string]string{},
		Secrets:      map[string][]kube.SecretEntry{},
		Owners:       map[string]kube.ObjectRef{},
		ImagesOf:     map[string][]kube.ContainerImage{},
		ReplicasOf:   map[string]int32{},
		Errs:         map[string]error{},
		TriggerName:  "manual-job",
		watches:      map[string]*Watch{},
		tables:       map[string]kube.Table{},
		logs:         map[string]*LogStream{},
	}
}

// Standard returns the resources of a plain cluster: the 26 kinds kosh
// has a renderer for, with the names kubectl knows them by, and one
// custom resource that it has none for, widgets.example.com.
func Standard() []kube.Resource {
	all := []string{"create", "delete", "get", "list", "patch", "update", "watch"}
	typed := func(gvr schema.GroupVersionResource, kind, singular string, namespaced bool, short ...string) kube.Resource {
		return kube.Resource{GVR: gvr, Kind: kind, Singular: singular, ShortNames: short, Namespaced: namespaced, Verbs: all, Typed: true}
	}
	rr := []kube.Resource{
		typed(render.Pods, "Pod", "pod", true, "po"),
		typed(render.Services, "Service", "service", true, "svc"),
		typed(render.Nodes, "Node", "node", false, "no"),
		typed(render.Namespaces, "Namespace", "namespace", false, "ns"),
		typed(render.Events, "Event", "event", true, "ev"),
		typed(render.ConfigMaps, "ConfigMap", "configmap", true, "cm"),
		typed(render.Secrets, "Secret", "secret", true),
		typed(render.PersistentVolumeClaims, "PersistentVolumeClaim", "persistentvolumeclaim", true, "pvc"),
		typed(render.PersistentVolumes, "PersistentVolume", "persistentvolume", false, "pv"),
		typed(render.ServiceAccounts, "ServiceAccount", "serviceaccount", true, "sa"),
		typed(render.Deployments, "Deployment", "deployment", true, "deploy"),
		typed(render.ReplicaSets, "ReplicaSet", "replicaset", true, "rs"),
		typed(render.StatefulSets, "StatefulSet", "statefulset", true, "sts"),
		typed(render.DaemonSets, "DaemonSet", "daemonset", true, "ds"),
		typed(render.Jobs, "Job", "job", true),
		typed(render.CronJobs, "CronJob", "cronjob", true, "cj"),
		typed(render.Ingresses, "Ingress", "ingress", true, "ing"),
		typed(render.NetworkPolicies, "NetworkPolicy", "networkpolicy", true, "netpol"),
		typed(render.StorageClasses, "StorageClass", "storageclass", false, "sc"),
		typed(render.Roles, "Role", "role", true),
		typed(render.RoleBindings, "RoleBinding", "rolebinding", true),
		typed(render.ClusterRoles, "ClusterRole", "clusterrole", false),
		typed(render.ClusterRoleBindings, "ClusterRoleBinding", "clusterrolebinding", false),
		typed(render.HorizontalPodAutoscalers, "HorizontalPodAutoscaler", "horizontalpodautoscaler", true, "hpa"),
		typed(render.PodDisruptionBudgets, "PodDisruptionBudget", "poddisruptionbudget", true, "pdb"),
		typed(render.CustomResourceDefinitions, "CustomResourceDefinition", "customresourcedefinition", false, "crd", "crds"),
		{
			GVR:        schema.GroupVersionResource{Group: "example.com", Version: "v1", Resource: "widgets"},
			Kind:       "Widget",
			Singular:   "widget",
			ShortNames: []string{"wd"},
			Namespaced: true,
			Verbs:      all,
		},
	}
	sort.Slice(rr, func(i, j int) bool { return rr[i].GVR.Resource < rr[j].GVR.Resource })
	return rr
}

// Resource returns the resource of Standard with the plural name. It
// panics when there is none: a test that asks for one has misspelled it.
func Resource(plural string) kube.Resource {
	for _, r := range Standard() {
		if r.GVR.Resource == plural {
			return r
		}
	}
	panic("kubetest: no resource " + plural)
}

// Ref returns the reference of an object of a resource of Standard.
func Ref(plural, namespace, name string) kube.ObjectRef {
	return kube.ObjectRef{Resource: Resource(plural), Namespace: namespace, Name: name}
}

// Key returns what a query's table is kept under: SetTable and Watch
// agree on it.
func Key(q kube.Query) string {
	key := q.Resource.GVR.Resource + "." + q.Resource.GVR.Group
	if q.Of != nil {
		return key + " of " + q.Of.String()
	}
	return key + " in " + q.Namespace
}

// SetTable makes t the table of q, and tells the watch of q, when one is
// open, that it has changed.
func (s *Session) SetTable(q kube.Query, t kube.Table) {
	s.mu.Lock()
	s.tables[Key(q)] = t
	w := s.watches[Key(q)]
	s.mu.Unlock()
	if w != nil {
		w.signal()
	}
}

// SetUsage makes u, with ok, what the usage watches' Current returns,
// and tells the open ones that it has changed.
func (s *Session) SetUsage(u kube.ClusterUsage, ok bool) {
	s.mu.Lock()
	s.usage, s.usageOK = u, ok
	ws := append([]*UsageWatch(nil), s.usages...)
	s.mu.Unlock()
	for _, w := range ws {
		w.signal()
	}
}

// Calls returns the calls so far, in their order.
func (s *Session) Calls() []Call {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Call(nil), s.calls...)
}

// Called returns the calls of one method.
func (s *Session) Called(method string) []Call {
	var out []Call
	for _, c := range s.Calls() {
		if c.Method == method {
			out = append(out, c)
		}
	}
	return out
}

// OpenWatches returns the keys of the watches that are open, sorted.
func (s *Session) OpenWatches() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for k, w := range s.watches {
		if !w.isClosed() {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

func (s *Session) record(method string, args ...any) {
	s.mu.Lock()
	s.calls = append(s.calls, Call{Method: method, Args: args})
	s.mu.Unlock()
}

func (s *Session) err(method string) error {
	return s.Errs[method]
}

func (s *Session) Grant() keys.Grant      { return s.GrantValue }
func (s *Session) ClusterVersion() string { return s.Version }

func (s *Session) Resources() []kube.Resource {
	return append([]kube.Resource(nil), s.ResourceList...)
}

func (s *Session) Resolve(_ context.Context, name string) (kube.Resource, error) {
	s.record("Resolve", name)
	if err := s.err("Resolve"); err != nil {
		return kube.Resource{}, err
	}
	want := strings.ToLower(name)
	for _, r := range s.ResourceList {
		names := append([]string{r.GVR.Resource, r.Singular, strings.ToLower(r.Kind), r.GVR.Resource + "." + r.GVR.Group}, r.ShortNames...)
		for _, n := range names {
			if n != "" && strings.ToLower(n) == want {
				return r, nil
			}
		}
	}
	return kube.Resource{}, fmt.Errorf("%q: %w", name, kube.ErrUnknownResource)
}

// Allow gives the session the right of verb on group/resource, or on
// its subresource ("" for none), in namespace ("" for the cluster scope
// or every namespace), for Can to answer. Any field may be "*", which
// matches every value, "" among them. A session is allowed nothing until
// a test allows it something.
func (s *Session) Allow(verb, group, resource, subresource, namespace string) {
	s.mu.Lock()
	s.allowed = append(s.allowed, right{verb, group, resource, subresource, namespace})
	s.mu.Unlock()
}

// Can reports whether Allow gave a right that matches the question. It is
// not recorded: the tables ask it for every key they bind, and no test
// of what a view asked wants it among the calls.
func (s *Session) Can(_ context.Context, verb string, res kube.Resource, subresource, namespace string) bool {
	match := func(want, got string) bool { return want == "*" || want == got }
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.allowed {
		if match(r.verb, verb) && match(r.group, res.GVR.Group) && match(r.resource, res.GVR.Resource) &&
			match(r.subresource, subresource) && match(r.namespace, namespace) {
			return true
		}
	}
	return false
}

func (s *Session) Watch(q kube.Query) (kube.Watch, error) {
	s.record("Watch", q)
	if err := s.err("Watch"); err != nil {
		return nil, err
	}
	w := &Watch{s: s, key: Key(q), changed: make(chan struct{}, 1)}
	s.mu.Lock()
	s.watches[w.key] = w
	s.mu.Unlock()
	return w, nil
}

// ClusterUsage opens a watch of what SetUsage set. It is not recorded:
// the header opens it, and no test of what a view asked wants it among
// the calls.
func (s *Session) ClusterUsage() kube.UsageWatch {
	w := &UsageWatch{s: s, changed: make(chan struct{}, 1)}
	s.mu.Lock()
	s.usages = append(s.usages, w)
	s.mu.Unlock()
	return w
}

func (s *Session) YAML(_ context.Context, ref kube.ObjectRef, managedFields bool) (string, error) {
	s.record("YAML", ref, managedFields)
	if err := s.err("YAML"); err != nil {
		return "", err
	}
	return s.YAMLs[ref.String()], nil
}

func (s *Session) Describe(_ context.Context, ref kube.ObjectRef) (string, error) {
	s.record("Describe", ref)
	if err := s.err("Describe"); err != nil {
		return "", err
	}
	return s.Descriptions[ref.String()], nil
}

func (s *Session) SecretData(_ context.Context, ref kube.ObjectRef) ([]kube.SecretEntry, error) {
	s.record("SecretData", ref)
	if err := s.err("SecretData"); err != nil {
		return nil, err
	}
	return s.Secrets[ref.String()], nil
}

func (s *Session) Logs(_ context.Context, ref kube.ObjectRef, opts kube.LogOptions) (kube.LogStream, error) {
	s.record("Logs", ref, opts)
	if err := s.err("Logs"); err != nil {
		return nil, err
	}
	l := &LogStream{lines: make(chan []kube.LogLine, 64)}
	s.mu.Lock()
	s.logs[ref.String()] = l
	s.mu.Unlock()
	return l, nil
}

// LogOf returns the stream that Logs last opened for ref, for a test to
// send lines on. It is nil when Logs was not called for ref.
func (s *Session) LogOf(ref kube.ObjectRef) *LogStream {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.logs[ref.String()]
}

func (s *Session) Owner(_ context.Context, ref kube.ObjectRef) (kube.ObjectRef, error) {
	s.record("Owner", ref)
	if err := s.err("Owner"); err != nil {
		return kube.ObjectRef{}, err
	}
	owner, ok := s.Owners[ref.String()]
	if !ok {
		return kube.ObjectRef{}, kube.ErrNoOwner
	}
	return owner, nil
}

func (s *Session) Images(_ context.Context, ref kube.ObjectRef) ([]kube.ContainerImage, error) {
	s.record("Images", ref)
	if err := s.err("Images"); err != nil {
		return nil, err
	}
	return s.ImagesOf[ref.String()], nil
}

func (s *Session) Replicas(_ context.Context, ref kube.ObjectRef) (int32, error) {
	s.record("Replicas", ref)
	if err := s.err("Replicas"); err != nil {
		return 0, err
	}
	return s.ReplicasOf[ref.String()], nil
}

func (s *Session) action(method string, args ...any) error {
	s.record(method, args...)
	return s.err(method)
}

func (s *Session) Delete(_ context.Context, ref kube.ObjectRef, opts kube.DeleteOptions) error {
	return s.action("Delete", ref, opts)
}

func (s *Session) Kill(_ context.Context, pod kube.ObjectRef) error {
	return s.action("Kill", pod)
}

func (s *Session) Update(_ context.Context, ref kube.ObjectRef, yaml string) error {
	return s.action("Update", ref, yaml)
}

func (s *Session) Scale(_ context.Context, ref kube.ObjectRef, replicas int32) error {
	return s.action("Scale", ref, replicas)
}

func (s *Session) Restart(_ context.Context, ref kube.ObjectRef) error {
	return s.action("Restart", ref)
}

func (s *Session) SetImages(_ context.Context, ref kube.ObjectRef, images map[string]string) error {
	return s.action("SetImages", ref, images)
}

func (s *Session) Rollback(_ context.Context, replicaSet kube.ObjectRef) error {
	return s.action("Rollback", replicaSet)
}

func (s *Session) Trigger(_ context.Context, cronJob kube.ObjectRef) (string, error) {
	if err := s.action("Trigger", cronJob); err != nil {
		return "", err
	}
	return s.TriggerName, nil
}

func (s *Session) SetSuspended(_ context.Context, cronJob kube.ObjectRef, suspended bool) error {
	return s.action("SetSuspended", cronJob, suspended)
}

func (s *Session) Cordon(_ context.Context, node kube.ObjectRef, on bool) error {
	return s.action("Cordon", node, on)
}

func (s *Session) Drain(_ context.Context, node kube.ObjectRef, opts kube.DrainOptions) error {
	return s.action("Drain", node, opts)
}

func (s *Session) Exec(ctx context.Context, pod kube.ObjectRef, opts kube.ExecOptions) (kube.ExecResult, error) {
	if err := s.action("Exec", pod, opts); err != nil {
		return kube.ExecResult{}, err
	}
	if s.ExecDo != nil {
		return s.ExecDo(ctx, pod, opts)
	}
	return s.ExecResult, nil
}

// Watch is the watch of a query of a Session.
type Watch struct {
	s       *Session
	key     string
	changed chan struct{}

	mu     sync.Mutex
	closed bool
}

func (w *Watch) Table(time.Time) kube.Table {
	w.s.mu.Lock()
	defer w.s.mu.Unlock()
	return w.s.tables[w.key]
}

func (w *Watch) Changed() <-chan struct{} { return w.changed }

func (w *Watch) Close() {
	w.mu.Lock()
	w.closed = true
	w.mu.Unlock()
}

func (w *Watch) isClosed() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.closed
}

func (w *Watch) signal() {
	select {
	case w.changed <- struct{}{}:
	default:
	}
}

// UsageWatch is the usage watch of a Session: Current is what SetUsage
// set last, and Changed receives after each SetUsage.
type UsageWatch struct {
	s       *Session
	changed chan struct{}
}

func (w *UsageWatch) Current() (kube.ClusterUsage, bool) {
	w.s.mu.Lock()
	defer w.s.mu.Unlock()
	return w.s.usage, w.s.usageOK
}

func (w *UsageWatch) Changed() <-chan struct{} { return w.changed }

// Close forgets the watch, so that SetUsage no longer signals it, and
// closes Changed, as the real watch does.
func (w *UsageWatch) Close() {
	w.s.mu.Lock()
	defer w.s.mu.Unlock()
	for i, u := range w.s.usages {
		if u == w {
			w.s.usages = append(w.s.usages[:i], w.s.usages[i+1:]...)
			close(w.changed)
			return
		}
	}
}

func (w *UsageWatch) signal() {
	select {
	case w.changed <- struct{}{}:
	default:
	}
}

// LogStream is the log a test writes.
type LogStream struct {
	lines chan []kube.LogLine

	mu      sync.Mutex
	closed  bool
	ended   bool
	err     error
	skipped int
}

// Send delivers lines to whoever reads the stream.
func (l *LogStream) Send(lines ...kube.LogLine) {
	l.lines <- lines
}

// End ends the stream with err, as a log that ended by itself (nil) or
// broke.
func (l *LogStream) End(err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.ended {
		return
	}
	l.ended = true
	l.err = err
	close(l.lines)
}

// SetSkipped sets what Skipped returns.
func (l *LogStream) SetSkipped(n int) {
	l.mu.Lock()
	l.skipped = n
	l.mu.Unlock()
}

// Closed reports whether Close was called.
func (l *LogStream) Closed() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.closed
}

func (l *LogStream) Lines() <-chan []kube.LogLine { return l.lines }

func (l *LogStream) Skipped() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.skipped
}

func (l *LogStream) Err() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.err
}

func (l *LogStream) Close() {
	l.mu.Lock()
	l.closed = true
	l.mu.Unlock()
	l.End(nil)
}
