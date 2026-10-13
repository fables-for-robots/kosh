package kube

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

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

	cfg     *rest.Config // nil in unit tests
	typed   kubernetes.Interface
	dyn     dynamic.Interface
	log     *slog.Logger
	opts    Options // with every zero value replaced by its default
	version string

	disc discovery.DiscoveryInterface // nil when the resources were given
	// zeroOf returns a new, empty object of a typed kind, which the
	// informers convert what the API sends into: render.Zero.
	zeroOf func(schema.GroupVersionResource) (runtime.Object, bool)
	tables tableLister // nil without cfg, until a test sets its fake

	// ctx is done when Close is called; everything the Cluster runs
	// ends with it, and wg waits for it to have ended.
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	resMu     sync.RWMutex
	resources []Resource // sorted, as Resources returns them

	missMu   sync.Mutex
	lastMiss time.Time // of the last discovery that Resolve asked for

	mu     sync.Mutex
	caches map[schema.GroupVersionResource]*kindCache
	closed bool
}

type noCopy struct{}

// The defaults of Options.
const (
	defaultIdleStop       = 10 * time.Minute
	defaultTablePoll      = 2 * time.Second
	defaultDiscoveryEvery = 5 * time.Minute
	defaultUserAgent      = "kosh"
)

func (o Options) withDefaults() Options {
	if o.Log == nil {
		o.Log = slog.Default()
	}
	if o.IdleStop <= 0 {
		o.IdleStop = defaultIdleStop
	}
	if o.TablePoll <= 0 {
		o.TablePoll = defaultTablePoll
	}
	if o.DiscoveryEvery <= 0 {
		o.DiscoveryEvery = defaultDiscoveryEvery
	}
	if o.Renderers == nil {
		o.Renderers = render.For
	}
	if o.UserAgent == "" {
		o.UserAgent = defaultUserAgent
	}
	return o
}

// newCluster makes a Cluster from clients, without connecting: what New
// is made of, and what unit tests use with client-go's fakes. resources
// is what discovery would have found; nil means it is read from disc,
// which may be nil only when resources is not.
func newCluster(cfg *rest.Config, typed kubernetes.Interface, dyn dynamic.Interface, disc discovery.DiscoveryInterface, resources []Resource, opts Options) *Cluster {
	opts = opts.withDefaults()
	ctx, cancel := context.WithCancel(context.Background())
	c := &Cluster{
		cfg:    cfg,
		typed:  typed,
		dyn:    dyn,
		log:    opts.Log,
		opts:   opts,
		disc:   disc,
		zeroOf: render.Zero,
		ctx:    ctx,
		cancel: cancel,
		caches: map[schema.GroupVersionResource]*kindCache{},
	}
	if cfg != nil {
		tables, err := newRESTTables(cfg)
		if err != nil {
			c.log.Error("the API server's tables cannot be read", "err", err)
		} else {
			c.tables = tables
		}
	}
	if resources == nil {
		c.refresh(ctx)
	} else {
		rr := append([]Resource(nil), resources...)
		sortResources(rr)
		c.setResources(rr)
	}
	if disc != nil {
		c.wg.Add(1)
		go c.discoverEvery()
	}
	return c
}

// RESTConfig returns how to reach the API: from the kubeconfig at path,
// or, when path is empty, from the ServiceAccount of the pod kosh runs
// in.
func RESTConfig(kubeconfig string) (*rest.Config, error) {
	if kubeconfig == "" {
		return rest.InClusterConfig()
	}
	return clientcmd.BuildConfigFromFlags("", kubeconfig)
}

// New connects to the cluster cfg names, reads the API server's version
// and its resources, and returns the Cluster. ctx bounds that first
// reading: the caller gives it 10 seconds. The Cluster lives until
// Close.
func New(ctx context.Context, cfg *rest.Config, opts Options) (*Cluster, error) {
	opts = opts.withDefaults()
	cfg = rest.CopyConfig(cfg)
	cfg.UserAgent = opts.UserAgent
	// An interactive tool, not a controller.
	cfg.QPS = 50
	cfg.Burst = 100

	typed, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("making the client of the API: %w", err)
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("making the client of the API: %w", err)
	}
	disc, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("making the client of the API: %w", err)
	}
	info, err := disc.ServerVersionWithContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("the API server at %s could not be reached: %w", cfg.Host, err)
	}
	resources, err := discover(ctx, disc, opts.Renderers, opts.Log)
	if err != nil {
		return nil, err
	}
	c := newCluster(cfg, typed, dyn, disc, resources, opts)
	c.version = info.GitVersion
	return c, nil
}

// Session returns the handle of one SSH session. Everything it opens
// ends when ctx is done.
func (c *Cluster) Session(ctx context.Context, grant keys.Grant) Session {
	return &session{c: c, ctx: ctx, grant: grant}
}

// Close stops every informer and everything else the Cluster runs.
func (c *Cluster) Close() {
	c.stopCaches()
	c.cancel()
	c.wg.Wait()
}
