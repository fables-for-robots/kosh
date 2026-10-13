package kube

import (
	"context"
	"errors"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
	"k8s.io/kubectl/pkg/describe"
)

const (
	// describeChunkSize is how many objects a describer lists at a
	// time, as kubectl's default.
	describeChunkSize = 500
	// describeTimeout bounds each request of a Describe whose context
	// has no deadline.
	describeTimeout = 30 * time.Second
)

// errNoConfig is what Describe returns for a Cluster made without a
// config: kubectl's describers make their own clients from one.
var errNoConfig = errors.New("describe needs a connection to the API server, and this cluster was made without one")

// Describe returns what kubectl describe prints of the object, events
// included: kubectl's describer of its kind, or for a kind that has
// none, the generic describer. A Secret is refused to a read-only grant
// before anything is read. The describers take no context: each of
// their requests, not the describe as a whole, is bounded as
// describeConfig says.
func (s *session) Describe(ctx context.Context, ref ObjectRef) (string, error) {
	if err := s.c.checkRef(ref); err != nil {
		return "", err
	}
	if err := s.maySee(ref); err != nil {
		return "", err
	}
	if s.c.cfg == nil {
		return "", errNoConfig
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	cfg := describeConfig(ctx, s.c.cfg)
	gvr := ref.Resource.GVR
	d, ok := describe.DescriberFor(schema.GroupKind{Group: gvr.Group, Kind: ref.Resource.Kind}, cfg)
	if !ok {
		scope := meta.RESTScopeRoot
		if ref.Resource.Namespaced {
			scope = meta.RESTScopeNamespace
		}
		mapping := &meta.RESTMapping{
			Resource:         gvr,
			GroupVersionKind: gvr.GroupVersion().WithKind(ref.Resource.Kind),
			Scope:            scope,
		}
		if d, ok = describe.GenericDescriberFor(mapping, cfg); !ok {
			return "", fmt.Errorf("kosh cannot describe %s", ref)
		}
	}
	return d.Describe(ref.Namespace, ref.Name, describe.DescriberSettings{ShowEvents: true, ChunkSize: describeChunkSize})
}

// describeConfig is the config the describers of one Describe are made
// with: cfg, with a timeout that is the time left until ctx's deadline,
// or describeTimeout when ctx has none, or cfg's own timeout when that
// is shorter. The timeout bounds each request of the describe, not the
// describe as a whole, which may make several. The describers take no
// context, so this is what bounds a request that hangs.
func describeConfig(ctx context.Context, cfg *rest.Config) *rest.Config {
	own := cfg.Timeout
	cfg = rest.CopyConfig(cfg)
	cfg.Timeout = describeTimeout
	if deadline, ok := ctx.Deadline(); ok {
		cfg.Timeout = max(time.Until(deadline), time.Millisecond)
	}
	if own > 0 {
		cfg.Timeout = min(cfg.Timeout, own)
	}
	return cfg
}
