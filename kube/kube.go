// Package kube is the cluster as kosh sees it: what there is, the tables
// of every kind, an object's YAML and description, logs, and the actions
// that change something.
//
// A Cluster is the whole process's: its clients, what it knows of the
// API's resources, and the informers all sessions share. A Session is one
// SSH session's handle on it and carries that session's grant. The
// Session is the one place where a grant is checked: every method that
// changes the cluster, and every one that returns a Secret's contents,
// refuses a read-only grant with ErrReadOnly.
//
// The terminal UI sees the cluster only through the Session interface,
// so that it is tested against kube/kubetest.
package kube

import (
	"context"
	"errors"
	"io"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/fables-for-robots/kosh/keys"
	"github.com/fables-for-robots/kosh/render"
)

var (
	// ErrReadOnly is what a read-only grant gets from a method that
	// changes the cluster or returns a Secret's contents.
	ErrReadOnly = errors.New("this key is read-only")
	// ErrUnknownResource is what Resolve returns for a name that no
	// resource of the cluster answers to.
	ErrUnknownResource = errors.New("no such resource")
	// ErrInvalidNamespace is what a Query or an ObjectRef gets whose
	// namespace is not the name a namespace can have, or that names a
	// namespace for a resource that has none, before anything is sent to
	// the API.
	ErrInvalidNamespace = errors.New("no such namespace")
	// ErrNoOwner is what Owner returns for an object that has no owner.
	ErrNoOwner = errors.New("no owner")
	// ErrNoRelation is what Watch returns for a Query whose Of names an
	// object that the resource has no rows for.
	ErrNoRelation = errors.New("nothing of that kind belongs to such an object")
	// ErrExecNotStarted is what Exec returns, wrapped around the
	// reason as the container's runtime or the API worded it, when the
	// command could not be started in the container: the image has no
	// such program, or the container is not running.
	ErrExecNotStarted = errors.New("the command could not be started")
)

// Resource is one resource type of the cluster.
type Resource struct {
	GVR schema.GroupVersionResource
	// Kind is the kind as the API spells it: "Pod".
	Kind string
	// Singular is the singular name: "pod". It may be empty.
	Singular   string
	ShortNames []string
	Namespaced bool
	// Verbs are what the API offers for it: "get", "list", "delete".
	Verbs []string
	// Typed: kosh has a renderer for it. A resource that is not typed
	// is shown as the API server's table.
	Typed bool
}

// Can reports whether the API offers verb for the resource.
func (r Resource) Can(verb string) bool {
	for _, v := range r.Verbs {
		if v == verb {
			return true
		}
	}
	return false
}

// Is reports whether the resource is group/resource, whatever its
// version: Is("apps", "deployments"), Is("", "pods").
func (r Resource) Is(group, resource string) bool {
	return r.GVR.Group == group && r.GVR.Resource == resource
}

// The two tables that are no resource of the API. They are rows of
// another object and are asked for with Query.Of.
var (
	// Containers are the containers of one pod: Query.Of is the pod.
	Containers = Resource{
		GVR:        schema.GroupVersionResource{Group: "kosh", Version: "v1", Resource: "containers"},
		Kind:       "Container",
		Singular:   "container",
		Namespaced: true,
		Typed:      true,
	}
	// Rules are the rules of one role: Query.Of is a role, a
	// clusterrole, or a binding, whose role's rules they then are.
	Rules = Resource{
		GVR:      schema.GroupVersionResource{Group: "kosh", Version: "v1", Resource: "rules"},
		Kind:     "Rule",
		Singular: "rule",
		Typed:    true,
	}
)

// ObjectRef names one object.
type ObjectRef struct {
	Resource  Resource
	Namespace string // empty for an object that has none
	Name      string
}

// String returns "resource/name" or "resource/namespace/name", for the
// flash line and the log.
func (r ObjectRef) String() string {
	if r.Namespace == "" {
		return r.Resource.GVR.Resource + "/" + r.Name
	}
	return r.Resource.GVR.Resource + "/" + r.Namespace + "/" + r.Name
}

// Query says which rows a table shows.
type Query struct {
	Resource Resource
	// Namespace limits the rows to one namespace. Empty is all of them.
	// One that is not a namespace's name, or one given for a resource
	// that is not namespaced, is ErrInvalidNamespace.
	Namespace string
	// Of limits the rows to those that belong to one object:
	//
	//	Resource     Of                                  the rows
	//	pods         deployment, replicaset, statefulset,
	//	             daemonset, job, service             the pods its selector matches
	//	pods         node                                the pods on it
	//	jobs         cronjob                             the jobs it owns
	//	Containers   pod                                 its containers
	//	Rules        role, clusterrole                   its rules
	//	Rules        rolebinding, clusterrolebinding     the rules of its role
	//
	// Any other pair is ErrNoRelation. Namespace is not looked at when
	// Of is set.
	Of *ObjectRef
}

// Table is the rows of a Query at one moment.
type Table struct {
	Columns []render.Column
	Rows    []render.Row
	// Namespaced: the first column is NAMESPACE, which a view of one
	// namespace leaves out.
	Namespaced bool
	// Stale: the rows may be out of date, because the watch behind them
	// is broken at the moment.
	Stale bool
	// Err is why there are no rows: the API refused the list, or could
	// not be reached. Columns and Rows are then empty.
	Err error
}

// Watch is a table that is kept up to date for as long as it is open.
type Watch interface {
	// Table returns the rows now. now is the time ages are counted
	// from.
	Table(now time.Time) Table
	// Changed receives, without blocking the sender, after the rows
	// have changed since Table was last called. It has room for one
	// signal: many changes are one.
	Changed() <-chan struct{}
	// Close ends the watch. It may be called more than once.
	Close()
}

// UsageWatch is the cluster's use of its nodes, kept up to date.
type UsageWatch interface {
	// Current returns the last reading. ok is false while there is
	// none: the API does not serve the metrics of nodes, no list has
	// worked yet, or the last one failed.
	Current() (u ClusterUsage, ok bool)
	// Changed receives after Current may answer differently. It has
	// room for one signal: many changes are one.
	Changed() <-chan struct{}
	// Close ends the watch. It may be called more than once.
	Close()
}

// ClusterUsage is the cluster's use of its nodes, in whole percents of
// what they can give, as k9s computes it: the sum over the nodes of what
// metrics-server measured, over the sum of their allocatable, floored.
type ClusterUsage struct {
	CPU, Mem int
	// Served is whether the API serves the metrics of nodes now: the
	// header's lines exist while it does.
	Served bool
}

// LogFrom says where a log starts.
type LogFrom int

const (
	// LogTail: the last LogTailLines lines, then what follows.
	LogTail LogFrom = iota
	// LogHead: the beginning. The stream ends after LogBufferLines
	// lines.
	LogHead
	// LogSince: what was written in the last LogOptions.Since, then
	// what follows.
	LogSince
)

const (
	// LogTailLines is how many lines LogTail starts with.
	LogTailLines = 100
	// LogBufferLines is how many lines a log view keeps.
	LogBufferLines = 5000
	// LogMaxContainers is how many containers one log stream reads at
	// most.
	LogMaxContainers = 30
)

// LogOptions says which log to read.
type LogOptions struct {
	// Container is the one container to read. Empty: the first
	// container of a pod, or with AllContainers every one.
	Container     string
	AllContainers bool
	// Previous: the log of the container's previous run. It is not
	// followed.
	Previous bool
	From     LogFrom
	Since    time.Duration // read when From is LogSince
}

// LogLine is one line of a log.
type LogLine struct {
	Pod       string
	Container string
	// Time is when the container wrote the line, as the API says. It is
	// zero when the API gave none.
	Time time.Time
	Text string // without the time and without the line feed
}

// LogStream is a log that is being read.
type LogStream interface {
	// Lines receives the lines as they come, some at a time. It is
	// closed when every container's log has ended, or after Close.
	Lines() <-chan []LogLine
	// Skipped returns how many containers were left out because there
	// were more than LogMaxContainers.
	Skipped() int
	// Err returns why the stream ended, once Lines is closed: nil when
	// the logs ended by themselves or Close was called.
	Err() error
	// Close ends the reading. It may be called more than once.
	Close()
}

// SecretEntry is one entry of a Secret, decoded.
type SecretEntry struct {
	Key   string
	Value string
}

// ContainerImage is one container of a workload's pod template.
type ContainerImage struct {
	Name  string
	Image string
	Init  bool // an init container
}

// Propagation says what a delete does to what the object owns.
type Propagation string

const (
	PropagationBackground Propagation = "Background"
	PropagationForeground Propagation = "Foreground"
	PropagationOrphan     Propagation = "Orphan"
)

// DeleteOptions are the choices of the delete dialog.
type DeleteOptions struct {
	Propagation Propagation // empty is PropagationBackground
	// Force: a grace period of zero.
	Force bool
}

// ExecOptions say what Exec runs, where, and on which terminal.
type ExecOptions struct {
	// Container is the container's name. It may be empty for a pod with
	// one container.
	Container string
	// Command is the command and its arguments, as the container's
	// runtime gets them. It has at least one word.
	Command []string
	// Stdin is the terminal's input. Exec reads it until the command
	// ends, and may read once more after that: a reader that must not
	// lose a byte ends with its turn.
	Stdin io.Reader
	// Stdout is the terminal's output. With a terminal the command's
	// standard error is part of it.
	Stdout io.Writer
	// Size returns the terminal's size now, in cells. It is asked at the
	// start and each time Resized has a value.
	Size func() (width, height int)
	// Resized has a value when the size has changed since Size was last
	// asked. Nil means the size never changes.
	Resized <-chan struct{}
}

// ExecResult is how a command that Exec ran has ended.
type ExecResult struct {
	// Started says that the command was started in the container.
	Started bool
	// Status is its exit status: 0 when it ended well, and what the
	// runtime reports otherwise (130 after ctrl-c and exit, 137 when
	// the container was killed under it).
	Status int
}

// DrainOptions are the choices of the drain dialog, as kubectl drain
// has them.
type DrainOptions struct {
	// GracePeriod is the grace period of every pod, in seconds. Negative:
	// each pod's own.
	GracePeriod int
	// Timeout is how long the drain may take. Zero: as long as it takes.
	Timeout            time.Duration
	IgnoreDaemonSets   bool
	DeleteEmptyDirData bool
	Force              bool
	DisableEviction    bool
}

// DefaultDrainOptions are what the drain dialog starts with: k9s's.
func DefaultDrainOptions() DrainOptions {
	return DrainOptions{
		GracePeriod: -1,
		Timeout:     5 * time.Second,
	}
}

// Reader is what a session may see.
type Reader interface {
	// Grant returns the grant of the session's key.
	Grant() keys.Grant
	// ClusterVersion returns the version of the API server: "v1.33.1".
	ClusterVersion() string

	// Resources returns every resource of the cluster that can be
	// listed, sorted by name, as it was last discovered.
	Resources() []Resource
	// Resolve returns the resource that answers to name: its plural
	// name, its singular name, one of its short names or its kind in
	// lower case, in any case, or "name.group" for one of several of a
	// name. A name that is not known is discovered again once before
	// ErrUnknownResource is returned.
	Resolve(ctx context.Context, name string) (Resource, error)

	// Watch opens the table of q. The table of a resource the session
	// may not list is a Watch whose Table has Err, not an error here.
	Watch(q Query) (Watch, error)
	// ClusterUsage opens the cluster's use of its nodes' CPU and memory,
	// for the header: a reading every Options.MetricsPoll while the API
	// serves the metrics of nodes. While it does not, Current is not ok
	// and not Served. It ends when the session's context does, or at
	// Close.
	ClusterUsage() UsageWatch

	// YAML returns the object as YAML, read from the API now, without
	// managedFields unless they are asked for. For a Secret it is
	// refused to a read-only grant.
	YAML(ctx context.Context, ref ObjectRef, managedFields bool) (string, error)
	// Describe returns what kubectl describe prints of the object. For
	// a Secret it is refused to a read-only grant.
	Describe(ctx context.Context, ref ObjectRef) (string, error)
	// SecretData returns the entries of a Secret, decoded and sorted by
	// key. It is refused to a read-only grant.
	SecretData(ctx context.Context, ref ObjectRef) ([]SecretEntry, error)

	// Logs reads the log of a pod, or of the pods of a deployment,
	// replicaset, statefulset, daemonset, job or service, as they are
	// when it is called. opts.Container and opts.Previous are for a
	// pod.
	Logs(ctx context.Context, ref ObjectRef, opts LogOptions) (LogStream, error)

	// Owner returns the object that owns ref: the controller among its
	// owner references, or the first of them. ErrNoOwner when it has
	// none.
	Owner(ctx context.Context, ref ObjectRef) (ObjectRef, error)
	// Images returns the containers of the pod template of a
	// deployment, statefulset or daemonset, init containers first.
	Images(ctx context.Context, ref ObjectRef) ([]ContainerImage, error)
	// Replicas returns the number of replicas a deployment, statefulset
	// or replicaset is set to.
	Replicas(ctx context.Context, ref ObjectRef) (int32, error)
}

// Actions is what a session with a full key may do. Every method is
// refused to a read-only grant with ErrReadOnly, before anything is sent
// to the API, and every call is logged with the name of the key, what it
// was for and what came of it.
//
// A method of Actions takes a context first and returns an error last.
// The test that goes through all of them relies on it.
type Actions interface {
	// Delete deletes the object.
	Delete(ctx context.Context, ref ObjectRef, opts DeleteOptions) error
	// Kill deletes a pod with a grace period of zero.
	Kill(ctx context.Context, pod ObjectRef) error
	// Update replaces the object with what yaml says, which is the text
	// YAML returned, edited. It fails when the YAML does not parse, when
	// it names another object, when the API finds it invalid, and when
	// the object has changed since it was read.
	Update(ctx context.Context, ref ObjectRef, yaml string) error
	// Scale sets the number of replicas of a deployment, statefulset or
	// replicaset.
	Scale(ctx context.Context, ref ObjectRef, replicas int32) error
	// Restart restarts the pods of a deployment, statefulset or
	// daemonset, as kubectl rollout restart does.
	Restart(ctx context.Context, ref ObjectRef) error
	// SetImages sets the image of the named containers of the pod
	// template of a deployment, statefulset or daemonset. Containers
	// that are not named keep theirs.
	SetImages(ctx context.Context, ref ObjectRef, images map[string]string) error
	// Rollback rolls the deployment that owns the replicaset back to
	// it, as kubectl rollout undo --to-revision does.
	Rollback(ctx context.Context, replicaSet ObjectRef) error
	// Trigger creates a job from the cronjob's template now, as kubectl
	// create job --from does, and returns the job's name.
	Trigger(ctx context.Context, cronJob ObjectRef) (string, error)
	// SetSuspended suspends a cronjob or resumes it.
	SetSuspended(ctx context.Context, cronJob ObjectRef, suspended bool) error
	// Cordon marks a node unschedulable, or schedulable again.
	Cordon(ctx context.Context, node ObjectRef, on bool) error
	// Drain cordons a node and evicts its pods, as kubectl drain does.
	Drain(ctx context.Context, node ObjectRef, opts DrainOptions) error
	// Exec runs a command in a container of a pod with a terminal, as
	// kubectl exec -it does, and returns when the command has ended, or
	// when ctx or the session has: it then closes the stream, and what
	// was running in the container is the container's to end.
	//
	// A command that started and ended is no error, whatever its
	// status: the result has it. The error is ErrExecNotStarted,
	// wrapped around the reason, when the command could not be started;
	// the API's own error when it refused (not found, forbidden: the
	// ServiceAccount needs get and create on pods/exec); and the
	// context's when ctx or the session ended first.
	//
	// ctx is the session's turn, whose end is the session's end: a
	// command that ctx ended is logged as one the session ended.
	Exec(ctx context.Context, pod ObjectRef, opts ExecOptions) (ExecResult, error)
}

// Session is one SSH session's handle on the cluster.
type Session interface {
	Reader
	Actions
}
