package sshd

import (
	"errors"
	"net"
	"os"
	"sync"

	gossh "golang.org/x/crypto/ssh"
)

// Why a connection is closed before it authenticated, in the log.
const (
	reasonTooManyHandshakes = "too many connections are in the handshake"
	reasonHandshakeDeadline = "the handshake took too long"
)

// conn is a connection Serve accepted, as the library is given it: what
// the server knows of it from the accept to the close.
//
// The library hands its callbacks a connection of its own, or x/crypto's
// metadata of one, not this; but their RemoteAddr is this one's, which
// is a connAddr that leads back here. Its String is the real address.
type conn struct {
	net.Conn
	srv       *Server
	transport string
	network   string
	remote    string

	// left is done when the connection leaves the handshake: when it has
	// authenticated, or when it is closed, whichever is first.
	left sync.Once
}

// newConn makes nc a conn. It holds one of the server's places in the
// handshake, which the caller has taken.
func newConn(srv *Server, nc net.Conn, transport string) *conn {
	c := &conn{Conn: nc, srv: srv, transport: transport}
	if a := nc.RemoteAddr(); a != nil {
		c.network, c.remote = a.Network(), a.String()
	}
	return c
}

// enterHandshake takes a place in the handshake. It reports false when
// MaxHandshakes connections hold one already.
func (s *Server) enterHandshake() bool {
	for {
		n := s.handshakes.Load()
		if n >= int64(s.cfg.MaxHandshakes) {
			return false
		}
		if s.handshakes.CompareAndSwap(n, n+1) {
			return true
		}
	}
}

// leaveHandshake gives the connection's place in the handshake back, the
// first time it is called.
func (c *conn) leaveHandshake() {
	c.left.Do(func() { c.srv.handshakes.Add(-1) })
}

// authenticated is called when the client has proved that it holds an
// accepted key.
func (c *conn) authenticated() {
	c.leaveHandshake()
}

// Close closes the connection, and gives its place in the handshake back
// if it still holds one.
func (c *conn) Close() error {
	err := c.Conn.Close()
	c.leaveHandshake()
	return err
}

// authLog is x/crypto's AuthLogCallback, called after every attempt to
// authenticate; err is nil once the client has authenticated. A public
// key handler that said yes is not that: x/crypto asks it also when a
// client only asks whether a key would do, before it has proved
// anything.
func authLog(cm gossh.ConnMetadata, _ string, err error) {
	if err != nil {
		return
	}
	if c := connOf(cm.RemoteAddr()); c != nil {
		c.authenticated()
	}
}

// failureReason is why a connection that did not authenticate was
// closed, for the log.
func failureReason(err error) string {
	var ne net.Error
	if errors.Is(err, os.ErrDeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()) {
		return reasonHandshakeDeadline
	}
	return err.Error()
}

// RemoteAddr returns the client's address, which leads back to c.
func (c *conn) RemoteAddr() net.Addr { return connAddr{c} }

// connAddr is the RemoteAddr of a conn.
type connAddr struct{ c *conn }

func (a connAddr) Network() string { return a.c.network }
func (a connAddr) String() string  { return a.c.remote }

// connOf returns the conn whose RemoteAddr addr is, or nil.
func connOf(addr net.Addr) *conn {
	if a, ok := addr.(connAddr); ok {
		return a.c
	}
	return nil
}
