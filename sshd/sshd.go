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
	"io"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
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

	// Exec returns a command for the session's program that gives the
	// session's terminal to run: the program leaves its screen, run is
	// called with the terminal, and when run has returned the program
	// has the terminal again, draws its screen, and is given done's
	// message with what run returned. The program takes no message
	// while run has not returned.
	//
	// run must return when its context is done, which it is when the
	// session ends: the client gone, the channel closed, the key removed
	// or changed, kosh shutting down. A run that waits then for a write
	// to Terminal.Out that the client does not take is released with
	// the connection: see there. done is told ErrTerminalBusy, and
	// run is not called, when a command has the terminal already and
	// still has it 200 ms later; it is told ErrSessionEnded when the
	// session ended before run began.
	//
	// When run has returned the terminal is reset to its normal state,
	// whatever run left it in: screen, colours, character set, margins,
	// the modes of keys, mouse and paste. The program has the terminal
	// back no sooner than 10 ms after run was called, however soon run
	// returns. A panic in run ends the
	// session as a crash of its program does: the client is told
	// MsgCrashed, and done is not called.
	Exec func(run func(context.Context, Terminal) error, done func(error) tea.Msg) tea.Cmd
}

// Terminal is a session's terminal for the time a command has it.
type Terminal struct {
	// In is what the client sends, from the hand-over on, the first
	// byte included. It ends with io.EOF when the command's turn ends:
	// nothing of what the client sends after that is lost to a reader
	// that is still in Read. It ends so, too, when the session ends
	// during the turn, which is when run's context is done.
	In io.Reader
	// Out is the client's terminal, byte for byte: the translation of
	// line feeds that the program's own output gets is not applied. A
	// write after the turn, or after the session's end during it, fails
	// with io.ErrClosedPipe.
	//
	// A write that waits because the client is not reading cannot be
	// ended from here: it is released when the client reads again or
	// closes the channel, or when the connection is closed, and fails
	// then with the channel's error. At the session's end the connection
	// is closed after the end's timeout at the latest: a command that
	// waits for such a write before it returns, and a turn that waits
	// for one after its command has returned (it does, so that nothing
	// of the command's is written after it), are cut off with the
	// connection then, the client is not told why the session ended,
	// and the log says that the write was what was waited for.
	Out io.Writer
	// Size returns the window's size now, in cells.
	Size func() (width, height int)
	// Resized has a value when the size has changed since Size was last
	// asked.
	Resized <-chan struct{}
}

var (
	// ErrTerminalBusy is what Session.Exec's done is told when a command
	// has the terminal already.
	ErrTerminalBusy = errors.New("sshd: a command has the terminal")
	// ErrSessionEnded is what Session.Exec's done is told when the
	// session ended before the command began.
	ErrSessionEnded = errors.New("sshd: the session has ended")
)

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

	// IdleConnection is how long a connection may go without a session:
	// from the moment it authenticated, and from the end of its last
	// session. Then it is closed. A connection that has a session is not
	// closed by this, however long the session lasts. Zero means 5
	// seconds.
	IdleConnection time.Duration
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
	// minTurn is how long a command's turn with a session's terminal
	// lasts at least: minTurn, but for a test.
	minTurn time.Duration
	// repairTurn is how long the turn lasts that repairs a session's
	// screen: minTurn, but for a test.
	repairTurn time.Duration

	handshakes atomic.Int64 // connections in the handshake, over every listener

	mu        sync.Mutex
	closing   bool                    // Shutdown has been called
	listeners map[net.Listener]string // the transport of each
	conns     map[*conn]struct{}
	sessions  map[*session]struct{}
	windows   map[ssh.Session]*window // of session channels that asked for a terminal
	perKey    map[string]int          // authenticated connections by key fingerprint
	running   int                     // sessions whose program runs
	changed   chan struct{}           // closed, and replaced, when conns or sessions change
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
	if cfg.IdleConnection <= 0 {
		cfg.IdleConnection = 5 * time.Second
	}
	s := &Server{
		cfg:        cfg,
		log:        cfg.Log,
		endTimeout: 2 * time.Second,
		minTurn:    minTurn,
		repairTurn: minTurn,
		listeners:  map[net.Listener]string{},
		conns:      map[*conn]struct{}{},
		sessions:   map[*session]struct{}{},
		windows:    map[ssh.Session]*window{},
		perKey:     map[string]int{},
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
	srv.ChannelHandlers = map[string]ssh.ChannelHandler{"session": s.sessionChannel}
	srv.RequestHandlers = map[string]ssh.RequestHandler{}
	srv.SubsystemHandlers = map[string]ssh.SubsystemHandler{}
	srv.Handler = s.handle
	// The library's emulated terminal, which is its default, made
	// explicit so that its window can be drained from the start.
	if err := srv.SetOption(ssh.EmulatePty()); err != nil {
		return nil, fmt.Errorf("sshd: %w", err)
	}
	srv.PtyHandler = s.ptyHandler(srv.PtyHandler)
	srv.HandshakeTimeout = cfg.HandshakeTimeout
	srv.ServerConfigCallback = s.serverConfig
	srv.ConnectionFailedCallback = s.connectionFailed
	s.ssh = srv
	cfg.Keys.OnChange(s.keysChanged)
	return s, nil
}

// sessionChannel is the handler of session channels: the library's,
// for at most maxSessionChannels of a connection at once, given only the
// requests filterRequests passes on. A channel beyond them is rejected
// before it is accepted. The library's handler
// returns when the channel's request loop ends, which is the life of
// what the channel holds.
func (s *Server) sessionChannel(srv *ssh.Server, sc *gossh.ServerConn, nc gossh.NewChannel, ctx ssh.Context) {
	c := connOf(sc.RemoteAddr())
	if c == nil || !c.openChannel() {
		_ = nc.Reject(gossh.ResourceShortage, "too many sessions on this connection")
		return
	}
	defer c.closeChannel()
	// The session finds the channel the library accepts in its context:
	// see rawWriter.
	a := &accepted{}
	ssh.DefaultSessionHandler(srv, sc, filteredChannel{nc, a}, channelContext{ctx, a})
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
		MaxAuthTries:              maxAuthTries,
		VerifiedPublicKeyCallback: verifiedKey,
		AuthLogCallback:           authLog,
	}
}

// connectionFailed logs a connection that ended before it authenticated.
// One that ended before the client sent a byte, and not by the deadline
// of the handshake, is logged at debug level: it is what a TCP probe
// does, the kubelet's twice every 10 seconds.
func (s *Server) connectionFailed(nc net.Conn, err error) {
	c := connOf(nc.RemoteAddr())
	if c == nil || c.overKeyLimit.Load() {
		return
	}
	reason := failureReason(err)
	level := slog.LevelInfo
	if c.read.Load() == 0 && reason != reasonHandshakeDeadline {
		level = slog.LevelDebug
	}
	s.logRefused(level, c.transport, c.remote, reason)
}

// logRefused logs a connection that is closed without having
// authenticated, its reason cut by cutReason.
func (s *Server) logRefused(level slog.Level, transport, remote, reason string) {
	s.log.Log(context.Background(), level, "connection closed before authenticating",
		"transport", transport, "remote", remote, "reason", cutReason(reason))
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
	s.listeners[l] = transport
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
		if !s.enterHandshake() {
			_ = nc.Close()
			remote := ""
			if a := nc.RemoteAddr(); a != nil {
				remote = a.String()
			}
			s.logRefused(slog.LevelWarn, transport, remote, reasonTooManyHandshakes)
			continue
		}
		c := newConn(s, nc, transport)
		s.mu.Lock()
		if s.closing {
			s.mu.Unlock()
			_ = c.Close()
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

// Shutdown stops the server: from now on Serve closes every connection
// it accepts; every session is told MsgShuttingDown and ended, and
// Shutdown waits for the sessions to end; then every connection is
// closed, and only then every listener Serve was given, the iroh ones
// last. A session over iroh would lose its transport with the listener,
// before it was told. Shutdown returns when all connections are closed
// and the listeners too, or when ctx is done, whichever is first; when
// ctx is done first, every connection and listener is closed at once.
func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	s.closing = true
	var sessions []*session
	for sess := range s.sessions {
		sessions = append(sessions, sess)
	}
	s.mu.Unlock()

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
	if err == nil {
		err = s.waitFor(ctx, func() bool { return len(s.conns) == 0 && len(s.sessions) == 0 })
	}
	s.closeListeners()
	return err
}

// closeListeners closes every listener Serve was given: those over iroh
// last, whose Close waits for the connections it carried to finish their
// last bytes.
func (s *Server) closeListeners() {
	s.mu.Lock()
	var first, last []net.Listener
	for l, transport := range s.listeners {
		if transport == "iroh" {
			last = append(last, l)
		} else {
			first = append(first, l)
		}
	}
	s.mu.Unlock()
	for _, l := range append(first, last...) {
		_ = l.Close()
	}
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
