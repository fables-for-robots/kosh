package kube

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
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
// none, the generic describer, made with the session's configuration,
// so that every request of it is the key's. The API refuses a role that
// may not get the object, and the refusal is logged. The describers
// take no context: each of their requests, not the describe as a whole,
// is bounded as describeConfig says.
func (s *session) Describe(ctx context.Context, ref ObjectRef) (string, error) {
	ctx, release := s.bound(ctx)
	defer release()
	if err := s.c.checkRef(ref); err != nil {
		return "", err
	}
	api, err := s.api()
	if err != nil {
		return "", err
	}
	if api.cfg == nil {
		return "", errNoConfig
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	cfg := describeConfig(ctx, api.cfg)
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
	out, err := d.Describe(ref.Namespace, ref.Name, describe.DescriberSettings{ShowEvents: true, ChunkSize: describeChunkSize})
	if err != nil {
		s.logReadRefusal(ctx, readDescribe, ref, err)
	}
	return out, err
}

// describeConfig is the config the describers of one Describe are made
// with: cfg, with a timeout that is the smallest of describeTimeout, the
// time left until ctx's deadline (at least a millisecond) when it has
// one, and cfg's own timeout when it has one. The timeout bounds each
// request of the describe, not the describe as a whole, which may make
// several. The describers take no context, so this is what bounds a
// request that hangs; and every request's transport ends it when ctx
// ends, which ends the describe with the session.
func describeConfig(ctx context.Context, cfg *rest.Config) *rest.Config {
	own := cfg.Timeout
	cfg = rest.CopyConfig(cfg)
	cfg.Wrap(func(rt http.RoundTripper) http.RoundTripper { return ctxTransport{rt: rt, ctx: ctx} })
	cfg.Timeout = describeTimeout
	if deadline, ok := ctx.Deadline(); ok {
		cfg.Timeout = min(cfg.Timeout, max(time.Until(deadline), time.Millisecond))
	}
	if own > 0 {
		cfg.Timeout = min(cfg.Timeout, own)
	}
	return cfg
}

// ctxTransport sends each request with a context that ends when the
// request's own does or when ctx does, for clients that take none. The
// response's body holds it until it is closed.
type ctxTransport struct {
	rt  http.RoundTripper
	ctx context.Context
}

func (t ctxTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := t.ctx.Err(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(req.Context())
	stop := context.AfterFunc(t.ctx, cancel)
	release := func() {
		stop()
		cancel()
	}
	resp, err := t.rt.RoundTrip(req.WithContext(ctx))
	if err != nil {
		release()
		return nil, err
	}
	resp.Body = &releasingBody{ReadCloser: resp.Body, release: release}
	return resp, nil
}

// releasingBody calls release once, when it is closed.
type releasingBody struct {
	io.ReadCloser
	once    sync.Once
	release func()
}

func (b *releasingBody) Close() error {
	err := b.ReadCloser.Close()
	b.once.Do(b.release)
	return err
}
