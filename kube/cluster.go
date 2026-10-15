package kube

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
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
	// MetricsPoll is how often the metrics of a kind are read while a
	// watch shows them. Zero means 15 seconds.
	MetricsPoll time.Duration
	// Renderers returns the renderer of a resource. Nil means
	// render.For. Tests give their own.
	Renderers func(schema.GroupVersionResource) (render.Renderer, bool)
	// UserAgent is what the API server is told kosh is. Empty means
	// "kosh".
	UserAgent string
	// Instance is the name of this kosh among those that serve the
	// cluster, which is in every group, binding and cache user it makes
	// (§2 of the roles design). Empty means keys.DefaultInstance. It
	// must pass keys.CheckInstance.
	Instance string
	// Namespace is kosh's own namespace, where its key list is: the roles
	// that may edit ConfigMaps there are warned of (§4.1 of the roles
	// design). Empty means that it is not known, and nothing is checked.
	Namespace string
}

// Cluster is the cluster for the whole process.
type Cluster struct {
	_ noCopy

	// cfg is the ServiceAccount's configuration, from which every
	// identity's is made; nil in unit tests.
	cfg *rest.Config
	// typed and dyn are the ServiceAccount's own clients, for what kosh
	// asks for itself (§3.2 of the roles design). Nothing of a session
	// is read or done with them: a session has its identity's clients.
	typed   kubernetes.Interface
	dyn     dynamic.Interface
	log     *slog.Logger
	opts    Options // with every zero value replaced by its default
	version string

	disc discovery.DiscoveryInterface // nil when the resources were given
	// zeroOf returns a new, empty object of a typed kind, which the
	// informers convert what the API sends into: render.Zero.
	zeroOf func(schema.GroupVersionResource) (runtime.Object, bool)

	// newClients makes the clients of one identity from its
	// configuration: newClients with a cfg, the clients the Cluster was
	// given without one. Tests give a factory of fakes.
	newClients func(*rest.Config) (*clients, error)
	idMu       sync.Mutex
	identities map[identity]*clients

	// ctx is done when Close is called; everything the Cluster runs
	// ends with it, and wg waits for it to have ended.
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	resMu     sync.RWMutex
	resources []Resource // sorted, as Resources returns them

	missMu   sync.Mutex
	lastMiss time.Time // of the last discovery that Resolve asked for

	// nodesRefused holds the roles whose refused list of the nodes the
	// log has said.
	refusedMu    sync.Mutex
	nodesRefused map[identity]bool

	mu      sync.Mutex
	caches  map[cacheKey]*kindCache
	pollers map[cacheKey]*metricsPoller
	closed  bool
}

type noCopy struct{}

// The defaults of Options.
const (
	defaultIdleStop       = 10 * time.Minute
	defaultTablePoll      = 2 * time.Second
	defaultDiscoveryEvery = 5 * time.Minute
	defaultMetricsPoll    = 15 * time.Second
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
	if o.MetricsPoll <= 0 {
		o.MetricsPoll = defaultMetricsPoll
	}
	if o.Renderers == nil {
		o.Renderers = render.For
	}
	if o.UserAgent == "" {
		o.UserAgent = defaultUserAgent
	}
	if o.Instance == "" {
		o.Instance = keys.DefaultInstance
	}
	return o
}

// newCluster makes a Cluster from clients, without connecting: what New
// is made of, and what unit tests use with client-go's fakes. typed and
// dyn are the ServiceAccount's; with cfg, each identity's clients are
// made from it, and without, none are until a test sets newClients.
// resources
// is what discovery would have found; nil means it is read from disc,
// which may be nil only when resources is not.
func newCluster(cfg *rest.Config, typed kubernetes.Interface, dyn dynamic.Interface, disc discovery.DiscoveryInterface, resources []Resource, opts Options) *Cluster {
	opts = opts.withDefaults()
	ctx, cancel := context.WithCancel(context.Background())
	c := &Cluster{
		cfg:        cfg,
		typed:      typed,
		dyn:        dyn,
		log:        opts.Log,
		opts:       opts,
		disc:       disc,
		zeroOf:     render.Zero,
		newClients: newClients,
		identities: map[identity]*clients{},
		ctx:        ctx,
		cancel:     cancel,
		caches:     map[cacheKey]*kindCache{},
		pollers:    map[cacheKey]*metricsPoller{},
	}
	if cfg == nil {
		// Without a configuration there is nothing to impersonate with,
		// and no identity gets clients: handing a session the clients
		// the Cluster was given would make it the ServiceAccount, with
		// the ServiceAccount's rights, without a word. A test that makes
		// a Cluster of fakes sets newClients itself.
		c.newClients = func(*rest.Config) (*clients, error) { return nil, errNoIdentityConfig }
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
	if err := keys.CheckInstance(opts.Instance); err != nil {
		return nil, err
	}
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

// Session returns the handle of one SSH session, which acts as the
// grant's key: every request it makes impersonates the key's user and
// its role's group, and its tables are read from the caches of its role.
// Everything it opens ends when ctx is done.
func (c *Cluster) Session(ctx context.Context, grant keys.Grant) Session {
	s := &session{c: c, ctx: ctx, grant: grant, self: identityOf(grant, c.opts.Instance), role: roleIdentity(grant.Role, c.opts.Instance)}
	s.clients, s.clientsErr = c.clientsFor(s.self)
	if s.clientsErr != nil {
		c.log.Error("the clients of a session cannot be made", "key", grant.Name, "fingerprint", grant.Fingerprint,
			"role", grant.Role.String(), "err", s.clientsErr)
	}
	return s
}

// Version returns the version of the API server, as New read it: "v1.33.1".
func (c *Cluster) Version() string { return c.version }

// Close stops every informer and everything else the Cluster runs.
func (c *Cluster) Close() {
	c.stopCaches()
	c.cancel()
	c.wg.Wait()
}
