package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	tea "charm.land/bubbletea/v2"
	ucli "github.com/urfave/cli/v2"
	"k8s.io/klog/v2"

	"github.com/fables-for-robots/kosh/irohnet"
	"github.com/fables-for-robots/kosh/keys"
	"github.com/fables-for-robots/kosh/kube"
	"github.com/fables-for-robots/kosh/sshd"
	"github.com/fables-for-robots/kosh/state"
	"github.com/fables-for-robots/kosh/tui"
)

// serveOptions are the settings of kosh serve: its flags, and what
// tests set besides them.
type serveOptions struct {
	sshListen      string // TCP address of the SSH server; empty for none
	iroh           bool   // accept SSH over iroh
	irohBind       string // UDP address of the endpoint; empty for every address, any port
	irohRelay      bool   // the default relays and the pkarr record
	irohMDNS       bool   // mDNS
	stateDir       string // the two keys
	authorizedKeys string // the key list
	kubeconfig     string // empty for the pod's ServiceAccount

	// What tests set; the command line sets none of it.

	// log is the log. Nil means text on standard error, at info level.
	log *slog.Logger
	// irohLocal makes the iroh endpoint irohnet's Local one: bound to
	// 127.0.0.1, without relay and without announcing anything.
	irohLocal bool
	// idleStop is kube.Options.IdleStop. Zero means its default.
	idleStop time.Duration
	// wrapTCP, when it is set, is given the TCP listener and returns
	// the listener the SSH server is given in its place.
	wrapTCP func(net.Listener) net.Listener
	// started, when it is set, is called once kosh serves, with what it
	// serves on.
	started func(serving)
}

// serving is what serve tells a test once it serves.
type serving struct {
	// ssh is the TCP address of the SSH server, nil without one.
	ssh net.Addr
	// iroh is the iroh endpoint, nil without one.
	iroh *irohnet.Listener
}

// serveCommand is kosh serve, which runs runServe with its flags.
func serveCommand(runServe func(context.Context, serveOptions) error) *ucli.Command {
	return &ucli.Command{
		Name:  "serve",
		Usage: "serve the terminal UI over SSH and iroh",
		Flags: []ucli.Flag{
			&ucli.StringFlag{Name: "ssh-listen", EnvVars: []string{"KOSH_SSH_LISTEN"}, Value: ":2222",
				Usage: "TCP address of the SSH server; empty for none"},
			&ucli.BoolFlag{Name: "iroh", EnvVars: []string{"KOSH_IROH"}, Value: true,
				Usage: "accept SSH over iroh"},
			&ucli.StringFlag{Name: "iroh-bind", EnvVars: []string{"KOSH_IROH_BIND"},
				Usage: "UDP address of the iroh endpoint (default: every address, any port)"},
			&ucli.BoolFlag{Name: "iroh-relay", EnvVars: []string{"KOSH_IROH_RELAY"}, Value: true,
				Usage: "use the default relays and publish a pkarr record with number0's DNS"},
			&ucli.BoolFlag{Name: "iroh-mdns", EnvVars: []string{"KOSH_IROH_MDNS"}, Value: true,
				Usage: "announce the iroh endpoint on the local network"},
			&ucli.StringFlag{Name: "state-dir", EnvVars: []string{"KOSH_STATE_DIR"}, Value: "/var/lib/kosh",
				Usage: "the directory of the iroh key and the SSH host key"},
			&ucli.StringFlag{Name: "authorized-keys", EnvVars: []string{"KOSH_AUTHORIZED_KEYS"}, Value: "/etc/kosh/authorized_keys",
				Usage: "the key list"},
			&ucli.StringFlag{Name: "kubeconfig", EnvVars: []string{"KUBECONFIG"},
				Usage: "a kubeconfig instead of the pod's ServiceAccount, for running kosh outside a cluster"},
		},
		Action: func(c *ucli.Context) error {
			return runServe(c.Context, serveOptions{
				sshListen:      c.String("ssh-listen"),
				iroh:           c.Bool("iroh"),
				irohBind:       c.String("iroh-bind"),
				irohRelay:      c.Bool("iroh-relay"),
				irohMDNS:       c.Bool("iroh-mdns"),
				stateDir:       c.String("state-dir"),
				authorizedKeys: c.String("authorized-keys"),
				kubeconfig:     c.String("kubeconfig"),
			})
		},
	}
}

// check refuses settings with which kosh cannot serve anybody, before
// anything is started.
func (o serveOptions) check() (bind netip.AddrPort, err error) {
	if o.sshListen == "" && !o.iroh {
		return bind, errors.New("kosh serve: both transports are off: set --ssh-listen or --iroh")
	}
	if o.iroh && !o.irohRelay && !o.irohMDNS {
		return bind, errors.New("kosh serve: iroh is on and nobody could find it: set --iroh-relay or --iroh-mdns")
	}
	if o.irohBind != "" {
		if bind, err = netip.ParseAddrPort(o.irohBind); err != nil {
			return bind, fmt.Errorf("kosh serve: --iroh-bind %q is not an address and port: %w", o.irohBind, err)
		}
	}
	return bind, nil
}

// How long serve waits.
const (
	// clusterTimeout bounds the first reading of the API server.
	clusterTimeout = 10 * time.Second
	// irohTimeout bounds the binding of the iroh endpoint, the wait for
	// a relay included.
	irohTimeout = 30 * time.Second
	// keysInterval is how often the key list is read again.
	keysInterval = 2 * time.Second
	// stopTimeout bounds the stop, from the moment it begins.
	stopTimeout = 10 * time.Second
)

// serve is kosh serve. It starts, in order, what kosh needs: the state
// directory, the key list, the cluster, the SSH server and its
// listeners. Each failure is one error that says which step failed, and
// what was started before it is stopped again before serve returns.
// serve then runs until ctx is done, and returns nil, or until a
// listener fails, and returns its error. Either way it stops everything
// it started, within stopTimeout.
func serve(ctx context.Context, o serveOptions) error {
	bind, err := o.check()
	if err != nil {
		return err
	}
	log := o.log
	if log == nil {
		log = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	}
	routeKlog(log)

	r := &running{log: log, failed: make(chan error, 2)}
	if err := r.start(ctx, o, bind); err != nil {
		r.stop()
		return err
	}

	var fail error
	select {
	case <-ctx.Done():
	case fail = <-r.failed:
	}
	r.stop()
	log.Info("kosh has stopped")
	return fail
}

// running is what serve has started, for stop to stop.
type running struct {
	log *slog.Logger

	stopKeys context.CancelFunc // ends the key list's Watch
	cluster  *kube.Cluster
	server   *sshd.Server
	tcp      net.Listener
	iroh     *irohnet.Listener

	// wg counts the goroutines start started: the key list's Watch and
	// each listener's Serve.
	wg sync.WaitGroup
	// failed receives the error of a Serve that failed.
	failed chan error
}

// start starts what kosh needs, in order, and stops at the first step
// that fails, with an error that says which. What was started before
// is in r.
func (r *running) start(ctx context.Context, o serveOptions, bind netip.AddrPort) error {
	log := r.log

	st, err := state.Load(o.stateDir)
	if err != nil {
		return fmt.Errorf("kosh serve: the state directory could not be loaded: %w", err)
	}

	list, err := keys.Open(o.authorizedKeys, log)
	if err != nil {
		return fmt.Errorf("kosh serve: the key list could not be read: %w", err)
	}
	watchCtx, stopKeys := context.WithCancel(context.Background())
	r.stopKeys = stopKeys
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		list.Watch(watchCtx, keysInterval)
	}()

	cfg, err := kube.RESTConfig(o.kubeconfig)
	if err != nil {
		return fmt.Errorf("kosh serve: the cluster's configuration could not be read: %w", err)
	}
	kctx, cancel := context.WithTimeout(ctx, clusterTimeout)
	r.cluster, err = kube.New(kctx, cfg, kube.Options{Log: log, UserAgent: "kosh/" + Version, IdleStop: o.idleStop})
	cancel()
	if err != nil {
		return fmt.Errorf("kosh serve: the cluster could not be reached: %w", err)
	}
	cluster := r.cluster

	r.server, err = sshd.New(sshd.Config{
		HostKeyPEM: st.HostKeyPEM,
		Keys:       list,
		Version:    Version,
		Log:        log,
		Model: func(s sshd.Session) tea.Model {
			return tui.New(cluster.Session(s.Context, s.Grant), tui.Options{Version: Version})
		},
	})
	if err != nil {
		return fmt.Errorf("kosh serve: the SSH server could not be made: %w", err)
	}

	if o.sshListen != "" {
		r.tcp, err = net.Listen("tcp", o.sshListen)
		if err != nil {
			return fmt.Errorf("kosh serve: the SSH port could not be bound: %w", err)
		}
		l := r.tcp
		if o.wrapTCP != nil {
			l = o.wrapTCP(l)
		}
		r.serve(l, "tcp")
	}

	if o.iroh {
		ictx, cancel := context.WithTimeout(ctx, irohTimeout)
		r.iroh, err = irohnet.Listen(ictx, irohnet.Config{
			Key:   st.IrohKey,
			Bind:  bind,
			Relay: o.irohRelay,
			MDNS:  o.irohMDNS,
			Local: o.irohLocal,
			Log:   log,
		})
		cancel()
		if err != nil {
			return fmt.Errorf("kosh serve: the iroh endpoint could not be bound: %w", err)
		}
		r.serve(r.iroh, "iroh")
	}

	// The Cluster tells the API server's version only through a
	// Session; one that is asked nothing else opens nothing.
	args := []any{"version", Version}
	if r.iroh != nil {
		args = append(args, "endpoint", r.iroh.ID())
	}
	if r.tcp != nil {
		args = append(args, "ssh", r.tcp.Addr().String())
	}
	args = append(args, "hostkey", st.HostKeyFingerprint(),
		"cluster", cluster.Session(ctx, keys.Grant{}).ClusterVersion())
	log.Info("kosh is serving", args...)

	if o.started != nil {
		s := serving{iroh: r.iroh}
		if r.tcp != nil {
			s.ssh = r.tcp.Addr()
		}
		o.started(s)
	}
	return nil
}

// serve runs the server's Serve for l in a goroutine. An error of it is
// sent to r.failed.
func (r *running) serve(l net.Listener, transport string) {
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		if err := r.server.Serve(l, transport); err != nil {
			r.failed <- fmt.Errorf("kosh serve: the %s listener failed: %w", transport, err)
		}
	}()
}

// stop stops what start started: the sessions are told and closed,
// stopTimeout at most, the listeners closed, the key list no longer
// read, and the cluster closed. It returns when every goroutine start
// started has ended.
func (r *running) stop() {
	ctx, cancel := context.WithTimeout(context.Background(), stopTimeout)
	defer cancel()
	if r.server != nil {
		if err := r.server.Shutdown(ctx); err != nil {
			r.log.Warn("the sessions did not end in time: their connections are closed", "err", err)
		}
	}
	// Shutdown has closed the listeners that Serve was given; these
	// close those it was not given yet, and do nothing more.
	if r.tcp != nil {
		_ = r.tcp.Close()
	}
	if r.iroh != nil {
		if err := r.iroh.Close(); err != nil {
			r.log.Warn("the iroh endpoint did not close cleanly", "err", err)
		}
	}
	if r.stopKeys != nil {
		r.stopKeys()
	}
	r.wg.Wait()
	if r.cluster != nil {
		r.cluster.Close()
	}
}

// klogTo is the log that klog's lines go to: that of the serve that
// started last.
var (
	klogTo   atomic.Pointer[slog.Logger]
	klogOnce sync.Once
)

// routeKlog sends what client-go logs through klog to log, at debug
// level, instead of to standard error. klog is set once for the
// process, before any of client-go runs; after that only the log it
// goes to changes.
func routeKlog(log *slog.Logger) {
	klogTo.Store(log)
	klogOnce.Do(func() {
		klog.SetSlogLogger(slog.New(klogHandler{}))
	})
}

// klogHandler hands every record to the handler of klogTo at debug
// level: what client-go has to say, such as a reflector's failed list
// or watch, is noise next to what kosh logs, and the table that list
// was for shows its error.
type klogHandler struct {
	// with are the WithAttrs and WithGroup made of this handler, applied
	// to the handler of klogTo when a record is handled.
	with []func(slog.Handler) slog.Handler
}

func (h klogHandler) target() slog.Handler {
	t := klogTo.Load().Handler()
	for _, f := range h.with {
		t = f(t)
	}
	return t
}

func (h klogHandler) Enabled(ctx context.Context, _ slog.Level) bool {
	return klogTo.Load().Handler().Enabled(ctx, slog.LevelDebug)
}

func (h klogHandler) Handle(ctx context.Context, rec slog.Record) error {
	// logr hands an error over without asking Enabled first.
	t := h.target()
	if !t.Enabled(ctx, slog.LevelDebug) {
		return nil
	}
	rec.Level = slog.LevelDebug
	rec.AddAttrs(slog.String("from", "klog"))
	return t.Handle(ctx, rec)
}

func (h klogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return klogHandler{with: append(slices.Clip(h.with), func(t slog.Handler) slog.Handler { return t.WithAttrs(attrs) })}
}

func (h klogHandler) WithGroup(name string) slog.Handler {
	return klogHandler{with: append(slices.Clip(h.with), func(t slog.Handler) slog.Handler { return t.WithGroup(name) })}
}
