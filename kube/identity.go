package kube

import (
	"context"
	"errors"
	"fmt"
	"io"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/metadata"
	"k8s.io/client-go/rest"

	"github.com/fables-for-robots/kosh/keys"
)

// identity is one impersonated caller: the user and the role's group.
// Every request of a session, and of the caches and pollers of a role,
// is made by kosh's ServiceAccount as an identity, so that the API
// server allows or refuses it by the role, and its audit log names the
// key or the role.
type identity struct {
	user, group string
}

// identityOf returns the identity a session of grant acts as, for the
// kosh instance named instance: the key's own user, "kosh:<key name>",
// with its role's group, which holds the instance's name.
func identityOf(grant keys.Grant, instance string) identity {
	return identity{user: grant.User(), group: grant.Role.Group(instance)}
}

// roleIdentity returns the identity of a role's caches and pollers, for
// the kosh instance named instance: the role's own user, which is the
// same string as its group, "kosh:<instance>:clusterrole:<name>" for a
// ClusterRole and "kosh:<instance>:role:<namespace>:<name>" for a Role,
// with that group. The reads that fill a cache that every key of the
// role shares are not put down to the key that happened to open it. The
// prefixes are the group's so that a ClusterRole named "team-a:edit" and
// the Role "edit" of team-a are two users, not one.
func roleIdentity(r keys.Role, instance string) identity {
	return identity{user: r.Group(instance), group: r.Group(instance)}
}

// clients are the clients of one identity: what a session or a cache
// talks to the API server with.
type clients struct {
	// cfg is the configuration they were made from, with the identity's
	// impersonation; the describers and exec make their own clients of
	// it. It is nil in unit tests.
	cfg   *rest.Config
	typed kubernetes.Interface
	dyn   dynamic.Interface
	// meta reads objects' metadata alone; nil without cfg, unless a
	// test gives its fake.
	meta metadata.Interface
	// tables reads the API server's tables; nil without cfg, unless a
	// test gives its fake.
	tables tableLister
	// openLog opens the log of one container of a pod: the typed
	// client's GetLogs(…).Stream. Tests give streams of their own.
	openLog func(ctx context.Context, namespace, pod string, opts *corev1.PodLogOptions) (io.ReadCloser, error)
}

// newClients makes the clients of cfg, as New makes the ServiceAccount's:
// what a Cluster with a configuration makes each identity's clients with.
func newClients(cfg *rest.Config) (*clients, error) {
	typed, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	meta, err := metadata.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	tables, err := newRESTTables(cfg)
	if err != nil {
		return nil, err
	}
	return &clients{cfg: cfg, typed: typed, dyn: dyn, meta: meta, tables: tables, openLog: logOpener(typed)}, nil
}

// logOpener returns the opener of a container's log through typed.
func logOpener(typed kubernetes.Interface) func(ctx context.Context, namespace, pod string, opts *corev1.PodLogOptions) (io.ReadCloser, error) {
	return func(ctx context.Context, namespace, pod string, opts *corev1.PodLogOptions) (io.ReadCloser, error) {
		return typed.CoreV1().Pods(namespace).GetLogs(pod, opts).Stream(ctx)
	}
}

// errNoIdentityConfig is what clientsFor returns for a Cluster made
// without a configuration, unless a test gave it a factory of clients.
var errNoIdentityConfig = errors.New("no configuration for an identity's clients")

// clientsFor returns the clients of id, made on first use and kept for
// the Cluster's life: a client is a configuration over the transport
// client-go shares among configurations of the same TLS, and the key
// list, so the identities, is bounded.
//
// Each identity's clients have their own rate limiter, of Options' QPS
// and burst, not one shared by every session. The API server's priority
// and fairness queue the requests by the impersonated user, so that the
// total is bounded there and one user cannot starve the others; a
// shared limiter in kosh would undo that, and let one busy session (a
// drain, a cache that lists again) slow every other.
func (c *Cluster) clientsFor(id identity) (*clients, error) {
	c.idMu.Lock()
	defer c.idMu.Unlock()
	if cl := c.identities[id]; cl != nil {
		return cl, nil
	}
	cfg := &rest.Config{}
	if c.cfg != nil {
		cfg = rest.CopyConfig(c.cfg)
	}
	cfg.Impersonate = rest.ImpersonationConfig{UserName: id.user, Groups: []string{id.group}}
	cl, err := c.newClients(cfg)
	if err != nil {
		return nil, fmt.Errorf("making the clients of %s: %w", id.user, err)
	}
	c.identities[id] = cl
	return cl, nil
}
