package sshd

import (
	"net"
)

// conn is a connection Serve accepted, as the library is given it: what
// the server knows of it from the accept to the close.
//
// The library hands its callbacks a connection of its own, or x/crypto's
// metadata of one, not this; but their RemoteAddr is this one's, which
// is a connAddr that leads back here. Its String is the real address.
type conn struct {
	net.Conn
	transport string
	network   string
	remote    string
}

func newConn(nc net.Conn, transport string) *conn {
	c := &conn{Conn: nc, transport: transport}
	if a := nc.RemoteAddr(); a != nil {
		c.network, c.remote = a.Network(), a.String()
	}
	return c
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
