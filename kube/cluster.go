package kube

import (
	"context"
	"log/slog"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"

	"github.com/fables-for-robots/kosh/keys"
	"github.com/fables-for-robots/kosh/render"
)

// Options are the settings of a Cluster. The zero value is what kosh
// runs with.
type Options struct {
	// Log receives what the cluster has to say, the actions included.
	// Nil means slog.Default().
	Log *slog.Logger
	// IdleStop is how long an informer that no watch uses is kept
	// before it is stopped and its cache dropped. Zero means 10
	// minutes.
	IdleStop time.Duration
	// TablePoll is how often the table of a resource that is not typed
	// is read again while a watch shows it. Zero means 2 seconds.
	TablePoll time.Duration
	// DiscoveryEvery is how often the API's resources are read again.
	// Zero means 5 minutes.
	DiscoveryEvery time.Duration
	// Renderers returns the renderer of a resource. Nil means
	// render.For. Tests give their own.
	Renderers func(schema.GroupVersionResource) (render.Renderer, bool)
	// UserAgent is what the API server is told kosh is. Empty means
	// "kosh".
	UserAgent string
}

// Cluster is the cluster for the whole process.
type Cluster struct {
	_ noCopy
}

type noCopy struct{}

// RESTConfig returns how to reach the API: from the kubeconfig at path,
// or, when path is empty, from the ServiceAccount of the pod kosh runs
// in.
func RESTConfig(kubeconfig string) (*rest.Config, error) {
	panic("not implemented")
}

// New connects to the cluster cfg names, reads the API server's version
// and its resources, and returns the Cluster. ctx bounds that first
// reading: the caller gives it 10 seconds. The Cluster lives until
// Close.
func New(ctx context.Context, cfg *rest.Config, opts Options) (*Cluster, error) {
	panic("not implemented")
}

// Session returns the handle of one SSH session. Everything it opens
// ends when ctx is done.
func (c *Cluster) Session(ctx context.Context, grant keys.Grant) Session {
	panic("not implemented")
}

// Close stops every informer and everything else the Cluster runs.
func (c *Cluster) Close() {
	panic("not implemented")
}
