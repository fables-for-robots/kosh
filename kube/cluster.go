package kube

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/metadata"
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
	// meta reads objects' metadata alone; nil without cfg, until a test
	// sets its fake.
	meta metadata.Interface
	// openLog opens the log of one container of a pod: the typed
	// client's GetLogs(…).Stream. Tests give streams of their own.
	openLog func(ctx context.Context, namespace, pod string, opts *corev1.PodLogOptions) (io.ReadCloser, error)

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
		openLog: func(ctx context.Context, namespace, pod string, opts *corev1.PodLogOptions) (io.ReadCloser, error) {
			return typed.CoreV1().Pods(namespace).GetLogs(pod, opts).Stream(ctx)
		},
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
		if c.meta, err = metadata.NewForConfig(cfg); err != nil {
			c.log.Error("the API server's metadata cannot be read", "err", err)
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
// in. A path that is a list, as KUBECONFIG may be, is read as kubectl
// reads KUBECONFIG: the files that are there are merged, and the current
// context is taken from them; a list from which no configuration comes
// is an error that names it. A single path that is not there is an
// error.
func RESTConfig(kubeconfig string) (*rest.Config, error) {
	if kubeconfig == "" {
		return rest.InClusterConfig()
	}
	paths := filepath.SplitList(kubeconfig)
	if len(paths) <= 1 {
		return clientcmd.BuildConfigFromFlags("", kubeconfig)
	}
	// Loaded and then made a client config directly: the deferred one
	// would fall back on the ServiceAccount when the files give nothing.
	rules := &clientcmd.ClientConfigLoadingRules{Precedence: paths}
	merged, err := rules.Load()
	if err != nil {
		return nil, fmt.Errorf("the kubeconfig files %q could not be read: %w", kubeconfig, err)
	}
	cfg, err := clientcmd.NewNonInteractiveClientConfig(*merged, merged.CurrentContext, &clientcmd.ConfigOverrides{}, rules).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("the kubeconfig files %q give no configuration: %w", kubeconfig, err)
	}
	return cfg, nil
}

// New connects to the cluster cfg names, reads the API server's version
// and its resources, and returns the Cluster. ctx bounds that first
// reading: the caller gives it 10 seconds. The Cluster lives until
// Close.
func New(ctx context.Context, cfg *rest.Config, opts Options) (*Cluster, error) {
	opts = opts.withDefaults()
	cfg = rest.CopyConfig(cfg)
	cfg.UserAgent = opts.UserAgent
	quietWarnings(cfg, opts.Log)
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

// Version returns the version of the API server, as New read it: "v1.33.1".
func (c *Cluster) Version() string { return c.version }

// Close stops every informer and everything else the Cluster runs.
func (c *Cluster) Close() {
	c.stopCaches()
	c.cancel()
	c.wg.Wait()
}
