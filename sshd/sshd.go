// Package sshd is kosh's SSH server: who gets in, and the life of a
// session. It serves connections from any net.Listener, which is how the
// TCP port and the iroh endpoint come to the same server.
//
// A key is accepted when the key list holds it. A session gets a terminal
// UI, made by the function the server was given, and nothing else: no
// command, no forwarding, no subsystem. The server knows nothing of
// Kubernetes.
package sshd

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/ssh"
	"charm.land/wish/v2"
	gossh "golang.org/x/crypto/ssh"

	"github.com/fables-for-robots/kosh/keys"
)

// What a session is told when it is ended from kosh's side.
const (
	MsgNeedsTerminal = "kosh needs a terminal: connect with ssh -t"
	MsgNoCommands    = "kosh runs no commands"
	MsgKeyRemoved    = "kosh: this key is no longer accepted"
	MsgGrantChanged  = "kosh: what this key may do has changed; connect again"
	MsgShuttingDown  = "kosh is shutting down"
	MsgCrashed       = "kosh: this session has failed and was closed"
)

// Session is what the server tells the maker of a session's program.
type Session struct {
	// Context is done when the session ends.
	Context context.Context
	// Grant is the grant of the session's key, as the key list had it
	// when the session began. The session is closed when it changes.
	Grant keys.Grant
	// Transport is what Serve was told its listener is: "tcp" or
	// "iroh".
	Transport string
	// Remote is the client's address: an IP address and port, or an
	// endpoint ID.
	Remote string
}

// Config configures a Server.
type Config struct {
	// HostKeyPEM is the SSH host key, in OpenSSH's private key format.
	HostKeyPEM []byte
	// Keys is the key list.
	Keys *keys.List
	// Model returns the program of a session. It is called once for
	// each session that has a terminal.
	Model func(Session) tea.Model
	// Version is kosh's version, for the SSH version string.
	Version string
	// Log receives connections, sessions and what goes wrong. Nil means
	// slog.Default().
	Log *slog.Logger

	// HandshakeTimeout is how long a connection may take to
	// authenticate. Zero means 30 seconds.
	HandshakeTimeout time.Duration
	// MaxHandshakes is how many connections may be authenticating at
	// once, over every listener. One more is closed at once. Zero
	// means 64.
	MaxHandshakes int
	// KeepaliveInterval is how often a connection is sent a keepalive
	// request. Zero means 30 seconds.
	KeepaliveInterval time.Duration
	// KeepaliveMisses is how many keepalives in a row may go
	// unanswered before the connection is closed. Zero means 3.
	KeepaliveMisses int
}

// maxAuthTries is how many times a connection may fail to authenticate.
// iroh-ssh lets ssh offer every key of the user's agent before the one
// named with -i, and each key offered is a try.
const maxAuthTries = 20

// Server is the SSH server.
type Server struct {
	_ noCopy

	cfg Config // with the defaults in place of zeros
	log *slog.Logger
	ssh *ssh.Server

	// endTimeout is how long a session that kosh ends may take to stop
	// its program and say why. Then its connection is closed: a client
	// that does not read cannot keep a session kosh has ended.
	endTimeout time.Duration

	mu        sync.Mutex
	closing   bool // Shutdown has been called
	listeners map[net.Listener]struct{}
	conns     map[*conn]struct{}
	sessions  map[*session]struct{}
	running   int           // sessions whose program runs
	changed   chan struct{} // closed, and replaced, when conns or sessions change
}

type noCopy struct{}

// New returns a server. It fails when the host key does not parse, or
// when Keys or Model is missing.
func New(cfg Config) (*Server, error) {
	if cfg.Keys == nil {
		return nil, errors.New("sshd: no key list")
	}
	if cfg.Model == nil {
		return nil, errors.New("sshd: no maker of a session's model")
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.HandshakeTimeout <= 0 {
		cfg.HandshakeTimeout = 30 * time.Second
	}
	if cfg.MaxHandshakes <= 0 {
		cfg.MaxHandshakes = 64
	}
	if cfg.KeepaliveInterval <= 0 {
		cfg.KeepaliveInterval = 30 * time.Second
	}
	if cfg.KeepaliveMisses <= 0 {
		cfg.KeepaliveMisses = 3
	}
	s := &Server{
		cfg:        cfg,
		log:        cfg.Log,
		endTimeout: 2 * time.Second,
		listeners:  map[net.Listener]struct{}{},
		conns:      map[*conn]struct{}{},
		sessions:   map[*session]struct{}{},
		changed:    make(chan struct{}),
	}
	// The host key goes first: without it wish would make one and write
	// it to a file.
	srv, err := wish.NewServer(
		wish.WithHostKeyPEM(cfg.HostKeyPEM),
		wish.WithVersion("kosh_"+cfg.Version),
		wish.WithPublicKeyAuth(s.keyAccepted),
	)
	if err != nil {
		return nil, fmt.Errorf("sshd: the host key: %w", err)
	}
	// No password or keyboard-interactive handler, and no channel,
	// request or subsystem handler but the library's session: what is
	// not handled is refused. The library fills these maps in only in
	// its own Serve, which kosh does not use.
	srv.ChannelHandlers = map[string]ssh.ChannelHandler{"session": ssh.DefaultSessionHandler}
	srv.RequestHandlers = map[string]ssh.RequestHandler{}
	srv.SubsystemHandlers = map[string]ssh.SubsystemHandler{}
	srv.Handler = s.handle
	srv.HandshakeTimeout = cfg.HandshakeTimeout
	srv.ServerConfigCallback = s.serverConfig
	srv.ConnectionFailedCallback = s.connectionFailed
	s.ssh = srv
	cfg.Keys.OnChange(s.keysChanged)
	return s, nil
}

// keyAccepted is the public key handler: a key is accepted when the key
// list holds it now. The user name is not looked at.
func (s *Server) keyAccepted(_ ssh.Context, key ssh.PublicKey) bool {
	_, ok := s.cfg.Keys.Lookup(key)
	return ok
}

// serverConfig is the library's configuration of x/crypto's server, for
// one connection.
func (s *Server) serverConfig(ssh.Context) *gossh.ServerConfig {
	return &gossh.ServerConfig{
		MaxAuthTries: maxAuthTries,
	}
}

// connectionFailed logs a connection that ended before it authenticated.
func (s *Server) connectionFailed(nc net.Conn, err error) {
	c := connOf(nc.RemoteAddr())
	if c == nil {
		return
	}
	s.logRefused(c.transport, c.remote, err.Error())
}

// logRefused logs a connection that is closed without having
// authenticated.
func (s *Server) logRefused(transport, remote, reason string) {
	s.log.Info("connection closed before authenticating", "transport", transport, "remote", remote, "reason", reason)
}

// Serve accepts connections from l until l is closed or Shutdown is
// called, and returns nil then. transport names l in the log and in
// Session.Transport. Serve may be called for several listeners.
func (s *Server) Serve(l net.Listener, transport string) error {
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		_ = l.Close()
		return nil
	}
	s.listeners[l] = struct{}{}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.listeners, l)
		s.mu.Unlock()
	}()

	var delay time.Duration
	for {
		nc, err := l.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) || s.isClosing() {
				return nil
			}
			// As net/http does: an error the system may get over, such as
			// too many open files, is waited out.
			if ne, ok := err.(net.Error); ok && ne.Temporary() { //nolint:staticcheck // SA1019: as net/http
				delay = min(max(2*delay, 5*time.Millisecond), time.Second)
				time.Sleep(delay)
				continue
			}
			return err
		}
		delay = 0
		c := newConn(nc, transport)
		s.mu.Lock()
		if s.closing {
			s.mu.Unlock()
			_ = nc.Close()
			continue
		}
		s.conns[c] = struct{}{}
		s.notifyLocked()
		s.mu.Unlock()
		go s.serveConn(c)
	}
}

// serveConn hands c to the library and forgets it when the library is
// done with it.
func (s *Server) serveConn(c *conn) {
	s.ssh.HandleConn(c)
	_ = c.Close()
	s.mu.Lock()
	delete(s.conns, c)
	s.notifyLocked()
	s.mu.Unlock()
}

func (s *Server) isClosing() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closing
}

// notifyLocked wakes whoever waits for conns or sessions to change. s.mu
// is held.
func (s *Server) notifyLocked() {
	close(s.changed)
	s.changed = make(chan struct{})
}

// waitFor waits until cond, called with s.mu held, is true, or ctx is
// done.
func (s *Server) waitFor(ctx context.Context, cond func() bool) error {
	s.mu.Lock()
	for !cond() {
		changed := s.changed
		s.mu.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
			return ctx.Err()
		}
		s.mu.Lock()
	}
	s.mu.Unlock()
	return nil
}

// Shutdown closes every listener Serve was given, tells every session
// MsgShuttingDown and closes it, and returns when all connections are
// closed or ctx is done, whichever is first.
func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	s.closing = true
	var listeners []net.Listener
	for l := range s.listeners {
		listeners = append(listeners, l)
	}
	var sessions []*session
	for sess := range s.sessions {
		sessions = append(sessions, sess)
	}
	s.mu.Unlock()
	for _, l := range listeners {
		_ = l.Close()
	}

	// A session that begins from now on is ended as it begins.
	for _, sess := range sessions {
		sess.end(MsgShuttingDown)
	}
	err := s.waitFor(ctx, func() bool { return len(s.sessions) == 0 })

	// The sessions have said why they end. The library's server closes
	// the connections it has, which are those that have authenticated;
	// the others are closed here.
	_ = s.ssh.Close()
	s.closeConns()
	if err != nil {
		return err
	}
	return s.waitFor(ctx, func() bool { return len(s.conns) == 0 && len(s.sessions) == 0 })
}

// closeConns closes every connection.
func (s *Server) closeConns() {
	s.mu.Lock()
	var conns []*conn
	for c := range s.conns {
		conns = append(conns, c)
	}
	s.mu.Unlock()
	for _, c := range conns {
		_ = c.Close()
	}
}

// Sessions returns how many sessions are open.
func (s *Server) Sessions() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running
}
