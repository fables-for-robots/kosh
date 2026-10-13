// Package irohnet is where kosh meets iroh: one endpoint that accepts
// connections for iroh-ssh's protocol and hands each out as a net.Conn
// that SSH runs in.
//
// The protocol has no handshake of its own: a client connects for the
// ALPN, opens one bidirectional stream and speaks SSH in it. This
// package knows nothing of SSH.
package irohnet

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/tmc/go-iroh/iroh"
	irohkey "github.com/tmc/go-iroh/key"
	"github.com/tmc/go-iroh/relay"
)

// ALPN is iroh-ssh's protocol.
const ALPN = "/iroh/ssh"

// StreamTimeout is how long an accepted connection may take to open its
// stream before it is closed.
const StreamTimeout = 30 * time.Second

// Config configures the endpoint.
type Config struct {
	// Key is the identity of the endpoint. Its public half is the
	// endpoint ID that clients dial.
	Key irohkey.SecretKey
	// Bind is the UDP address to bind. The zero value: every address, a
	// port the system picks.
	Bind netip.AddrPort
	// Relay: use the default relays and keep a pkarr record with
	// number0's DNS, so that the endpoint ID is all a client needs,
	// wherever it is.
	Relay bool
	// MDNS: announce the endpoint on the local network.
	MDNS bool
	// Local is for tests: no relay, nothing announced, bound to
	// 127.0.0.1 unless Bind says otherwise. Relay and MDNS are not
	// looked at.
	Local bool
	// Log receives what binding and announcing have to say. Nil means
	// slog.Default().
	Log *slog.Logger
}

// Listener is the endpoint as a net.Listener. Every connection Accept
// returns is the one stream of one iroh connection; closing it closes
// that iroh connection. Its RemoteAddr is an Addr with the client's
// endpoint ID.
type Listener struct {
	_ noCopy

	ep  *iroh.Endpoint
	id  string
	log *slog.Logger
	// tm is defaultTimeouts, unless a test shortens them.
	tm timeouts

	// ctx is done once Close was called; everything the listener
	// started ends with it.
	ctx    context.Context
	cancel context.CancelFunc
	// ready hands connections whose stream is open to Accept.
	ready chan *conn
	// slots holds a token for every connection that is waiting for its
	// stream or for Accept; it holds maxWaiting at most.
	slots chan struct{}
	// wg counts the goroutines the listener started.
	wg sync.WaitGroup

	mu     sync.Mutex
	closed bool
	// conns are the connections whose stream is open and that are not
	// closed yet, handed out or on their way to Accept.
	conns map[*conn]struct{}

	closeOnce sync.Once
	closeErr  error
}

type noCopy struct{}

var _ net.Listener = (*Listener)(nil)

// Addr is the address of a peer or of the endpoint itself: an endpoint
// ID. Network returns "iroh" and String the ID's 64 hex characters.
type Addr struct {
	ID string
}

func (Addr) Network() string  { return "iroh" }
func (a Addr) String() string { return a.ID }

// Listen binds the endpoint and starts announcing it. ctx bounds Listen
// alone: the endpoint lives until Close. With cfg.Relay it waits for the
// relay, 15 seconds at most, and goes on with a warning when the relay
// has not answered. mDNS that cannot be had is a warning too. Neither
// Relay nor MDNS, without Local, is an error.
func Listen(ctx context.Context, cfg Config) (*Listener, error) {
	return listen(ctx, cfg, defaultTimeouts)
}

// closeLinger is how long closing a connection waits for the peer to
// close it, so that what was written last is not dropped: a few round
// trips through a relay.
const closeLinger = 2 * time.Second

// maxWaiting is how many connections may wait for their stream, or for
// Accept, at once. One more is closed at once.
const maxWaiting = 64

// timeouts are the times a Listener waits, which tests shorten.
type timeouts struct {
	// stream is how long a connection may take to open its stream.
	stream time.Duration
	// linger is how long closing a connection waits for the peer.
	linger time.Duration
}

var defaultTimeouts = timeouts{stream: StreamTimeout, linger: closeLinger}

// listen is Listen with the times the listener waits.
func listen(ctx context.Context, cfg Config, tm timeouts) (*Listener, error) {
	if !cfg.Local && !cfg.Relay && !cfg.MDNS {
		return nil, errors.New("irohnet: neither relay nor mDNS: nobody could find the endpoint")
	}
	if cfg.Key.IsZero() {
		return nil, errors.New("irohnet: no key")
	}
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}

	opts := []iroh.Option{iroh.WithSecretKey(cfg.Key), iroh.WithALPNs(ALPN)}
	bind := cfg.Bind
	if cfg.Local && !bind.IsValid() {
		bind = netip.AddrPortFrom(netip.AddrFrom4([4]byte{127, 0, 0, 1}), 0)
	}
	if bind.IsValid() {
		opts = append(opts, iroh.WithBindAddr(bind))
	}
	if cfg.Local || !cfg.Relay {
		opts = append(opts, iroh.WithRelayMode(relay.ModeDisabled()))
	} else {
		opts = append(opts, iroh.WithRelayMode(relay.ModeDefault()))
	}
	ep, err := iroh.Bind(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("irohnet: bind: %w", err)
	}

	lctx, cancel := context.WithCancel(context.Background())
	l := &Listener{
		ep:     ep,
		id:     ep.ID().String(),
		log:    log,
		tm:     tm,
		ctx:    lctx,
		cancel: cancel,
		ready:  make(chan *conn),
		slots:  make(chan struct{}, maxWaiting),
		conns:  make(map[*conn]struct{}),
	}
	log.Info("iroh endpoint bound", "id", l.id, "addr", ep.LocalAddr(),
		"local", cfg.Local, "relay", cfg.Relay && !cfg.Local, "mdns", cfg.MDNS && !cfg.Local)

	l.wg.Add(1)
	go l.acceptLoop()
	if !cfg.Local {
		if err := l.announce(ctx, cfg); err != nil {
			l.Close()
			return nil, fmt.Errorf("irohnet: %w", err)
		}
	}
	return l, nil
}

// acceptLoop takes every connection the endpoint accepts and waits for its
// stream in a goroutine of its own. It ends when the listener is closed.
func (l *Listener) acceptLoop() {
	defer l.wg.Done()
	for {
		qc, err := l.ep.Accept(l.ctx)
		if err != nil {
			if l.ctx.Err() != nil || l.endpointClosed() {
				return
			}
			l.log.Debug("iroh connection failed before it was accepted", "error", err)
			continue
		}
		select {
		case l.slots <- struct{}{}:
		default:
			l.log.Debug("iroh connection closed: too many are waiting for their stream",
				"peer", qc.RemoteID().String(), "waiting", maxWaiting)
			qc.CloseWithError(0, "busy")
			continue
		}
		l.wg.Add(1)
		go l.awaitStream(qc)
	}
}

// endpointClosed reports whether the endpoint has been shut down.
func (l *Listener) endpointClosed() bool {
	select {
	case <-l.ep.Closed():
		return true
	default:
		return false
	}
}

// awaitStream waits for the stream of qc, the stream timeout at most,
// and hands it to Accept. A connection whose stream does not come is
// closed. It holds a slot until then.
func (l *Listener) awaitStream(qc *iroh.Conn) {
	defer l.wg.Done()
	defer func() { <-l.slots }()
	peer := qc.RemoteID().String()
	ctx, cancel := context.WithTimeout(l.ctx, l.tm.stream)
	stream, err := qc.AcceptStreamConn(ctx)
	cancel()
	if err != nil {
		qc.CloseWithError(0, "no stream")
		if l.ctx.Err() == nil {
			l.log.Debug("iroh connection closed: it opened no stream", "peer", peer, "error", err)
		}
		return
	}
	c := &conn{
		stream: stream,
		qc:     qc,
		local:  Addr{ID: l.id},
		remote: Addr{ID: peer},
		linger: l.tm.linger,
		stop:   l.ctx.Done(),
	}
	c.onClose = l.forget

	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		c.Close()
		return
	}
	l.conns[c] = struct{}{}
	l.mu.Unlock()

	select {
	case l.ready <- c:
	case <-l.ctx.Done():
		c.Close()
	}
}

// forget drops c from the connections Close closes.
func (l *Listener) forget(c *conn) {
	l.mu.Lock()
	delete(l.conns, c)
	l.mu.Unlock()
}

// ID returns the endpoint ID.
func (l *Listener) ID() string {
	return l.id
}

// Accept returns the next connection whose stream has been opened. A
// connection that opens no stream within StreamTimeout is closed and
// never returned. After Close it returns net.ErrClosed.
func (l *Listener) Accept() (net.Conn, error) {
	if l.ctx.Err() != nil {
		return nil, l.errClosed()
	}
	select {
	case c := <-l.ready:
		if l.ctx.Err() != nil {
			c.Close()
			return nil, l.errClosed()
		}
		return c, nil
	case <-l.ctx.Done():
		return nil, l.errClosed()
	}
}

func (l *Listener) errClosed() error {
	return &net.OpError{Op: "accept", Net: "iroh", Addr: l.Addr(), Err: net.ErrClosed}
}

// Close closes the endpoint and every connection of it.
func (l *Listener) Close() error {
	l.closeOnce.Do(func() {
		l.mu.Lock()
		l.closed = true
		conns := make([]*conn, 0, len(l.conns))
		for c := range l.conns {
			conns = append(conns, c)
		}
		l.mu.Unlock()

		l.cancel()
		for _, c := range conns {
			c.Close()
		}
		l.closeErr = l.ep.Shutdown(context.Background())
		l.wg.Wait()
	})
	return l.closeErr
}

// Addr returns the endpoint's own Addr.
func (l *Listener) Addr() net.Addr {
	return Addr{ID: l.id}
}

// LocalAddrs returns the UDP addresses the endpoint is bound to. It is
// what a test hands to DialLocal.
func (l *Listener) LocalAddrs() []netip.AddrPort {
	return []netip.AddrPort{l.ep.LocalAddr()}
}
