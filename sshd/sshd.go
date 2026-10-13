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
	"log/slog"
	"net"
	"time"

	tea "charm.land/bubbletea/v2"

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

// Server is the SSH server.
type Server struct {
	_ noCopy
}

type noCopy struct{}

// New returns a server. It fails when the host key does not parse, or
// when Keys or Model is missing.
func New(cfg Config) (*Server, error) {
	panic("not implemented")
}

// Serve accepts connections from l until l is closed or Shutdown is
// called, and returns nil then. transport names l in the log and in
// Session.Transport. Serve may be called for several listeners.
func (s *Server) Serve(l net.Listener, transport string) error {
	panic("not implemented")
}

// Shutdown closes every listener Serve was given, tells every session
// MsgShuttingDown and closes it, and returns when all connections are
// closed or ctx is done, whichever is first.
func (s *Server) Shutdown(ctx context.Context) error {
	panic("not implemented")
}

// Sessions returns how many sessions are open.
func (s *Server) Sessions() int {
	panic("not implemented")
}
