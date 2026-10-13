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
	"log/slog"
	"net"
	"net/netip"
	"time"

	irohkey "github.com/tmc/go-iroh/key"
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
	panic("not implemented")
}

// ID returns the endpoint ID.
func (l *Listener) ID() string {
	panic("not implemented")
}

// Accept returns the next connection whose stream has been opened. A
// connection that opens no stream within StreamTimeout is closed and
// never returned. After Close it returns net.ErrClosed.
func (l *Listener) Accept() (net.Conn, error) {
	panic("not implemented")
}

// Close closes the endpoint and every connection of it.
func (l *Listener) Close() error {
	panic("not implemented")
}

// Addr returns the endpoint's own Addr.
func (l *Listener) Addr() net.Addr {
	panic("not implemented")
}

// LocalAddrs returns the UDP addresses the endpoint is bound to. It is
// what a test hands to DialLocal.
func (l *Listener) LocalAddrs() []netip.AddrPort {
	panic("not implemented")
}

// DialLocal connects to the endpoint with the given ID at addrs, without
// a relay and without looking anything up, opens the stream and returns
// it. It is for tests: kosh has no client. The caller writes first, as
// an SSH client does; the listener sees the stream only then.
func DialLocal(ctx context.Context, id string, addrs []netip.AddrPort) (net.Conn, error) {
	panic("not implemented")
}
