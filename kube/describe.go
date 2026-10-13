package kube

import (
	"context"
	"errors"
	"fmt"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/kubectl/pkg/describe"
)

// describeChunkSize is how many objects a describer lists at a time, as
// kubectl's default.
const describeChunkSize = 500

// errNoConfig is what Describe returns for a Cluster made without a
// config: kubectl's describers make their own clients from one.
var errNoConfig = errors.New("describe needs a connection to the API server, and this cluster was made without one")

// Describe returns what kubectl describe prints of the object, events
// included: kubectl's describer of its kind, or for a kind that has
// none, the generic describer. A Secret is refused to a read-only grant
// before anything is read. The describers take no context, so ctx does
// not end a call that has begun.
func (s *session) Describe(ctx context.Context, ref ObjectRef) (string, error) {
	if err := s.maySee(ref); err != nil {
		return "", err
	}
	if s.c.cfg == nil {
		return "", errNoConfig
	}
	gvr := ref.Resource.GVR
	d, ok := describe.DescriberFor(schema.GroupKind{Group: gvr.Group, Kind: ref.Resource.Kind}, s.c.cfg)
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
		if d, ok = describe.GenericDescriberFor(mapping, s.c.cfg); !ok {
			return "", fmt.Errorf("kosh cannot describe %s", ref)
		}
	}
	return d.Describe(ref.Namespace, ref.Name, describe.DescriberSettings{ShowEvents: true, ChunkSize: describeChunkSize})
}
