package cli

import (
	"context"
	"errors"
	"fmt"
	stdlog "log"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	ucli "github.com/urfave/cli/v2"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
	// stopTimeout bounds the stop. Zero means the constant stopTimeout.
	stopTimeout time.Duration
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
		return bind, errors.New("both transports are off: set --ssh-listen or --iroh")
	}
	if o.iroh && !o.irohRelay && !o.irohMDNS {
		return bind, errors.New("iroh is on and nobody could find it: set --iroh-relay or --iroh-mdns")
	}
	if o.irohBind != "" {
		if bind, err = netip.ParseAddrPort(o.irohBind); err != nil {
			return bind, fmt.Errorf("--iroh-bind %q is not an address and port: %w", o.irohBind, err)
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
// what was started before it is stopped again before serve returns; a
// step that fails because ctx is done is a stop, and serve returns nil.
// serve then runs until ctx is done, and returns nil, or until a
// listener stops by itself, and returns an error that names it. Either
// way it stops everything it started, and returns after stopTimeout at
// most, whether the stop has finished or not.
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
	routeStdlog(log)
	defer unrouteStdlog(log)

	timeout := o.stopTimeout
	if timeout <= 0 {
		timeout = stopTimeout
	}
	r := &running{log: log, failed: make(chan error, 2)}
	if err := r.start(ctx, o, bind); err != nil {
		r.stop(timeout)
		if ctx.Err() != nil {
			log.Info("kosh has stopped before it served", "err", loggableStartError(err))
			return nil
		}
		return err
	}

	var fail error
	select {
	case <-ctx.Done():
	case fail = <-r.failed:
	}
	r.stop(timeout)
	log.Info("kosh has stopped")
	return fail
}

// loggableStartError returns the error of a start as the log holds it:
// of an error of the API, only its code, after the step that failed, as
// nothing of an API error's text is logged; any other error as it is.
func loggableStartError(err error) string {
	var api apierrors.APIStatus
	if !errors.As(err, &api) {
		return err.Error()
	}
	step, _, _ := strings.Cut(err.Error(), ": ")
	return fmt.Sprintf("%s: the API answered (%d)", step, api.Status().Code)
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
	// stopping is set when stop begins: a Serve that returns after it
	// was asked to.
	stopping atomic.Bool
	// failed receives the error of a Serve that returned by itself.
	failed chan error
}

// start starts what kosh needs, in order, and stops at the first step
// that fails, with an error that says which. What was started before
// is in r.
func (r *running) start(ctx context.Context, o serveOptions, bind netip.AddrPort) error {
	log := r.log

	st, err := state.Load(o.stateDir)
	if err != nil {
		return fmt.Errorf("the state directory could not be loaded: %w", err)
	}

	list, err := keys.Open(o.authorizedKeys, log)
	if err != nil {
		return fmt.Errorf("the key list could not be read: %w", err)
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
		return fmt.Errorf("the cluster's configuration could not be read: %w", err)
	}
	kctx, cancel := context.WithTimeout(ctx, clusterTimeout)
	r.cluster, err = kube.New(kctx, cfg, kube.Options{Log: log, UserAgent: "kosh/" + Version, IdleStop: o.idleStop})
	cancel()
	if err != nil {
		return fmt.Errorf("the cluster could not be reached: %w", err)
	}
	cluster := r.cluster

	r.server, err = sshd.New(sshd.Config{
		HostKeyPEM: st.HostKeyPEM,
		Keys:       list,
		Version:    Version,
		Log:        log,
		Model: func(s sshd.Session) tea.Model {
			return tui.New(cluster.Session(s.Context, s.Grant), tui.Options{
				Version: Version,
				// The session's terminal, for a shell in a pod. tui does
				// not know sshd: its Terminal is sshd's, field for field.
				Handover: func(run func(context.Context, tui.Terminal) error, done func(error) tea.Msg) tea.Cmd {
					return s.Exec(func(ctx context.Context, t sshd.Terminal) error {
						return run(ctx, tui.Terminal(t))
					}, done)
				},
			})
		},
	})
	if err != nil {
		return fmt.Errorf("the SSH server could not be made: %w", err)
	}

	if o.sshListen != "" {
		r.tcp, err = net.Listen("tcp", o.sshListen)
		if err != nil {
			return fmt.Errorf("the SSH port could not be bound: %w", err)
		}
		l := r.tcp
		if o.wrapTCP != nil {
			l = o.wrapTCP(l)
		}
		r.serve(l, "tcp", "ssh")
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
			return fmt.Errorf("the iroh endpoint could not be bound: %w", err)
		}
		r.serve(r.iroh, "iroh", "iroh")
	}

	args := []any{"version", Version}
	if r.iroh != nil {
		args = append(args, "endpoint", r.iroh.ID())
	}
	if r.tcp != nil {
		args = append(args, "ssh", r.tcp.Addr().String())
	}
	args = append(args, "hostkey", st.HostKeyFingerprint(),
		"cluster", cluster.Version())
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

// serve runs the server's Serve for l, the transport transport, in a
// goroutine. A Serve that returns before stop was asked for, with an
// error or without, is sent to r.failed as the listener called name
// having stopped.
func (r *running) serve(l net.Listener, transport, name string) {
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		err := r.server.Serve(l, transport)
		if r.stopping.Load() {
			return
		}
		if err != nil {
			r.failed <- fmt.Errorf("the %s listener stopped: %w", name, err)
		} else {
			r.failed <- fmt.Errorf("the %s listener stopped", name)
		}
	}()
}

// stop stops what start started, in order: the sessions are told and
// ended, the listeners closed, the key list no longer read, and the
// cluster closed last. It returns when all of that has ended, or after
// timeout, whichever comes first; then the log says what it was still
// waiting for, and what has not ended goes on in its goroutine.
func (r *running) stop(timeout time.Duration) {
	r.stopping.Store(true)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	var mu sync.Mutex
	var waiting []string // what has not ended: what failed, then the step
	step := ""
	begin := func(what string) {
		mu.Lock()
		step = what
		mu.Unlock()
	}
	failed := func(what string) {
		mu.Lock()
		waiting = append(waiting, what)
		mu.Unlock()
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		if r.server != nil {
			begin("the SSH server")
			if err := r.server.Shutdown(ctx); err != nil {
				failed("the sessions")
			}
		}
		// Shutdown has closed the listeners that Serve was given; these
		// close those it was not given yet, and do nothing more.
		if r.tcp != nil {
			begin("the SSH port")
			_ = r.tcp.Close()
		}
		if r.iroh != nil {
			begin("the iroh endpoint")
			if err := r.iroh.Close(); err != nil {
				r.log.Warn("the iroh endpoint did not close cleanly", "err", err)
			}
		}
		if r.stopKeys != nil {
			r.stopKeys()
		}
		begin("the listeners and the key list")
		r.wg.Wait()
		if r.cluster != nil {
			begin("the cluster")
			r.cluster.Close()
		}
		begin("")
	}()

	select {
	case <-done:
		mu.Lock()
		defer mu.Unlock()
		if len(waiting) > 0 {
			r.log.Warn("kosh did not stop in time", "waiting_for", strings.Join(waiting, ", "), "timeout", timeout)
		}
	case <-ctx.Done():
		mu.Lock()
		defer mu.Unlock()
		if step != "" {
			waiting = append(waiting, step)
		}
		if len(waiting) == 0 {
			return // it ended as the time ran out
		}
		r.log.Warn("kosh did not stop in time", "waiting_for", strings.Join(waiting, ", "), "timeout", timeout)
	}
}

// klogTo is the log that klog's lines go to: that of the serve that
// started last.
var (
	klogTo   atomic.Pointer[slog.Logger]
	klogOnce sync.Once
)

// routeKlog sends what client-go logs through klog at verbosity 0, and
// its errors, to log, at debug level, instead of to standard error.
// klog is set once for the process, before any of client-go runs; after
// that only the log it goes to changes.
func routeKlog(log *slog.Logger) {
	klogTo.Store(log)
	klogOnce.Do(func() {
		klog.SetSlogLogger(slog.New(klogHandler{}))
	})
}

// klogHandler hands the records of klog's verbosity 0, and its errors,
// to the handler of klogTo at debug level: what client-go has to say,
// such as a reflector's failed list or watch, is noise next to what kosh
// logs, and the table that list was for shows its error. A record of a
// higher verbosity, which logr gives a level below info, is dropped
// whatever the level of the log: client-go logs the bodies of requests
// and responses there, and a Secret's YAML or an edit's text must not
// reach the log.
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

func (h klogHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return level >= slog.LevelInfo && klogTo.Load().Handler().Enabled(ctx, slog.LevelDebug)
}

func (h klogHandler) Handle(ctx context.Context, rec slog.Record) error {
	// logr hands an error over without asking Enabled first.
	if rec.Level < slog.LevelInfo {
		return nil
	}
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

// stdlogTo is the log that the lines of Go's standard log package go to:
// that of the serve that started last, while it runs. Nil when none runs.
var (
	stdlogTo   atomic.Pointer[slog.Logger]
	stdlogOnce sync.Once
)

// routeStdlog sends what Go's standard log package prints, as quic-go
// prints a warning about the UDP receive buffer, to log: each line one
// record at warn level, with source=log. The standard logger is set once
// for the process, without its own date; after that only the log it goes
// to changes.
//
// A log whose handler is Go's default slog handler, which writes through
// the standard logger, is never where the standard logger's lines go: a
// line would wait for the standard logger's lock, which its Print holds,
// for ever. The routing is then left as it is, and a debug line says so.
func routeStdlog(log *slog.Logger) {
	if reflect.TypeOf(log.Handler()) == defaultSlogHandler {
		log.Debug("the standard log is not routed: kosh's log is Go's default slog, which writes through it")
		return
	}
	stdlogTo.Store(log)
	stdlogOnce.Do(func() {
		stdlog.SetFlags(0)
		stdlog.SetOutput(stdlogWriter{})
	})
}

// defaultSlogHandler is the type of Go's default slog handler, taken
// before anything could call slog.SetDefault.
var defaultSlogHandler = reflect.TypeOf(slog.Default().Handler())

// unrouteStdlog is called when the serve of log returns: unless another
// serve has started since, the standard log's lines go to standard error
// again, not to the log of a serve that has ended.
func unrouteStdlog(log *slog.Logger) {
	stdlogTo.CompareAndSwap(log, nil)
}

// stdlogWriter is the output of the standard logger, which writes each
// line with one Write. A line is cut by cutLine.
type stdlogWriter struct{}

func (stdlogWriter) Write(p []byte) (int, error) {
	log := stdlogTo.Load()
	if log == nil {
		return os.Stderr.Write(p)
	}
	msg := cutLine(strings.TrimSuffix(string(p), "\n"))
	log.LogAttrs(context.Background(), slog.LevelWarn, msg, slog.String("source", "log"))
	return len(p), nil
}

// maxStdlogLine is how many bytes of a line of the standard logger the
// log holds: what libraries log through Go's default slog comes this way,
// and the SSH library's errors may repeat what a peer sent.
const maxStdlogLine = 1 << 10

// cutLine returns s, or when it is longer than maxStdlogLine bytes, its
// first maxStdlogLine bytes or fewer, cut at the start of a rune, and
// "…".
func cutLine(s string) string {
	if len(s) <= maxStdlogLine {
		return s
	}
	n := maxStdlogLine
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}
