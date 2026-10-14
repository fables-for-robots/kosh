package sshd

import (
	"context"
	"errors"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	gossh "golang.org/x/crypto/ssh"
)

// Why a connection is closed before it authenticated, in the log.
const (
	reasonTooManyHandshakes = "too many connections are in the handshake"
	reasonHandshakeDeadline = "the handshake took too long"
	// Why a connection is closed once it has authenticated.
	reasonTooManyConnectionsOfKey = "too many connections of this key are open"
	reasonKeyRemoved              = "its key is no longer in the key list"
)

// maxSessionChannels is how many session channels one connection may
// have open at once. Each lets its client send up to x/crypto's channel
// window, 2 MiB, that nothing reads, and costs goroutines; and while one
// session of a connection runs, the rule of IdleConnection does not
// close it. Without a bound, any key, a read-only one too, could exhaust
// the server's memory through one connection, or run any number of
// programs on it.
const maxSessionChannels = 10

// maxConnectionsPerKey is how many authenticated connections one key may
// have open at once. The handshake limit bounds the connections that have
// not authenticated; this bounds those that have, which maxSessionChannels
// bounds one at a time.
const maxConnectionsPerKey = 16

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
	// keepalive is done when the connection's keepalive has started.
	keepalive sync.Once

	// key is the key the client authenticated with, from
	// VerifiedPublicKeyCallback, in the goroutine of the handshake.
	key gossh.PublicKey
	// keyFingerprint is the key's fingerprint while the connection is
	// counted among its key's, under srv.mu.
	keyFingerprint string
	// overKeyLimit is set when the connection is closed for its key's
	// limit, which is logged then and not again as a failed connection.
	overKeyLimit atomic.Bool
	// read counts the bytes read from the client.
	read atomic.Int64

	mu       sync.Mutex
	authed   bool
	closed   bool
	keyGone  bool        // its key left the key list: no new session, and closed when it has none
	linger   *time.Timer // closes the connection of a key that is gone, after its last session
	ended    time.Time   // when its last session ended, or zero
	sessions int         // sessions whose handler runs
	channels int         // session channels open
	idle     *time.Timer // closes the connection when it has no session
	idleGen  uint64      // which arming of idle is current
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
// accepted key. A connection beyond its key's limit, or whose key has left
// the list since it was accepted, is closed now; any other is closed from
// now on when it has no session for IdleConnection.
func (c *conn) authenticated() {
	c.leaveHandshake()
	if reason := c.srv.countKey(c); reason != "" {
		c.overKeyLimit.Store(true)
		g, _ := c.srv.cfg.Keys.Lookup(c.key)
		c.srv.log.Warn("connection closed after authenticating", "transport", c.transport, "remote", c.remote,
			"key", g.Name, "fingerprint", fingerprint(c.key), "reason", reason)
		_ = c.Close()
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.authed = true
	if c.sessions == 0 && !c.closed {
		c.armIdleLocked()
	}
}

// countKey counts c among the connections of its key, and returns why
// it does not: the key has maxConnectionsPerKey already, or it is no
// longer in the key list (looked up under s.mu, as keysChanged looks: a
// change of the list is seen either here or there). A connection that is
// closed is not counted.
func (s *Server) countKey(c *conn) string {
	if c.key == nil {
		return "" // cannot be: only keys authenticate
	}
	fp := gossh.FingerprintSHA256(c.key)
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.cfg.Keys.Lookup(c.key); !ok {
		return reasonKeyRemoved
	}
	if s.perKey[fp] >= maxConnectionsPerKey {
		return reasonTooManyConnectionsOfKey
	}
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	if !closed {
		s.perKey[fp]++
		c.keyFingerprint = fp
	}
	return ""
}

// keyRemoved is called when c's key has left the key list, after its
// sessions were told to end. From now on c gets no session channel. When
// it has no session it is closed now: its client is idle. Else it is
// closed after its last session has ended, and so is one whose last
// session has only just ended, perhaps of this very change: see
// lingerForKey.
func (c *conn) keyRemoved() {
	c.mu.Lock()
	c.keyGone = true
	now := false
	switch {
	case c.sessions != 0 || c.closed:
	case !c.ended.IsZero() && time.Since(c.ended) < c.srv.keyLinger:
		if c.linger == nil {
			c.lingerForKey()
		}
	default:
		now = true
	}
	c.mu.Unlock()
	if now {
		_ = c.Close()
	}
}

// lingerForKey is called when the last session of a connection whose
// key is gone has ended. The session has told its client why, sent its
// exit status and closed its channel; the client now answers: what its
// terminal replies to the session's last output, its own close of the
// channel, its disconnect. A connection that is closed under those
// writes answers the first with a reset, and the next fails: an ssh
// client does not mind, but what carries the connection may, and a
// kubectl port-forward ends for all its connections on such an error.
//
// So the client is left to close the connection, which is the ordinary
// end of an SSH connection and is what clients do when their session has
// ended; one that does not is closed after srv.keyLinger. In that time
// the connection can do nothing: openChannel refuses it a channel, and
// the server has no handler for any other request. c.mu is held, and
// c.ended is when the last session ended.
func (c *conn) lingerForKey() {
	c.disarmIdleLocked()
	c.linger = time.AfterFunc(c.srv.keyLinger-time.Since(c.ended), func() { _ = c.Close() })
}

// uncountKey takes c from the connections of its key, if it is counted.
func (s *Server) uncountKey(c *conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if fp := c.keyFingerprint; fp != "" {
		c.keyFingerprint = ""
		if s.perKey[fp]--; s.perKey[fp] <= 0 {
			delete(s.perKey, fp)
		}
	}
}

// openChannel counts a new session channel, and reports false when the
// connection has maxSessionChannels already, or its key left the list. closeChannel takes it back.
func (c *conn) openChannel() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.channels >= maxSessionChannels || c.keyGone {
		return false
	}
	c.channels++
	return true
}

func (c *conn) closeChannel() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.channels--
}

// sessionStarted and sessionEnded count the connection's sessions.
func (c *conn) sessionStarted() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sessions++
	c.disarmIdleLocked()
}

func (c *conn) sessionEnded() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sessions--
	if c.sessions == 0 {
		c.ended = time.Now()
	}
	switch {
	case c.sessions != 0 || c.closed:
	case c.keyGone:
		if c.linger == nil {
			c.lingerForKey()
		}
	case c.authed:
		c.armIdleLocked()
	}
}

// armIdleLocked closes the connection after IdleConnection, unless a
// session starts before. c.mu is held.
func (c *conn) armIdleLocked() {
	c.disarmIdleLocked()
	gen := c.idleGen
	c.idle = time.AfterFunc(c.srv.cfg.IdleConnection, func() {
		c.mu.Lock()
		idle := gen == c.idleGen && c.sessions == 0 && !c.closed
		c.mu.Unlock()
		if idle {
			c.srv.log.Debug("connection closed: it has no session", "transport", c.transport, "remote", c.remote)
			_ = c.Close()
		}
	})
}

// disarmIdleLocked stops the idle timer. c.mu is held.
func (c *conn) disarmIdleLocked() {
	// A timer that fires as it is stopped sees another generation.
	c.idleGen++
	if c.idle != nil {
		c.idle.Stop()
		c.idle = nil
	}
}

// Read reads from the client, and counts what it read.
func (c *conn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 {
		c.read.Add(int64(n))
	}
	return n, err
}

// Close closes the connection, and gives its place in the handshake back
// if it still holds one. The first Close of a connection whose key has
// left the list says so in the log, whoever closes it: the server at
// once or after the linger, the client, the end of a session that did
// not end, a shutdown.
func (c *conn) Close() error {
	err := c.Conn.Close()
	c.leaveHandshake()
	c.mu.Lock()
	forKey := c.keyGone && !c.closed
	c.closed = true
	c.disarmIdleLocked()
	if c.linger != nil {
		c.linger.Stop()
		c.linger = nil
	}
	c.mu.Unlock()
	if forKey {
		c.srv.log.Info("connection closed", "transport", c.transport, "remote", c.remote,
			"fingerprint", fingerprint(c.key), "reason", reasonKeyRemoved)
	}
	// After closed is set: countKey counts no closed connection.
	c.srv.uncountKey(c)
	return err
}

// startKeepalive starts the keepalive of the connection, the first time
// it is called. ctx is the connection's context: the keepalive ends with
// it.
func (c *conn) startKeepalive(ctx context.Context, sc gossh.Conn) {
	c.keepalive.Do(func() { go c.keepaliveLoop(ctx, sc) })
}

// keepaliveLoop sends a keepalive request every KeepaliveInterval and
// closes the connection when KeepaliveMisses in a row have no reply
// within the interval. Any reply, success or failure, is an answer. A
// request is not sent while the one before has no answer.
func (c *conn) keepaliveLoop(ctx context.Context, sc gossh.Conn) {
	tick := time.NewTicker(c.srv.cfg.KeepaliveInterval)
	defer tick.Stop()
	misses := 0
	var answered chan struct{}
	for {
		if answered == nil {
			answered = make(chan struct{})
			go func(answered chan struct{}) {
				// SendRequest waits for the reply, or for the connection
				// to close: a dead peer holds this goroutine, not the loop.
				_, _, _ = sc.SendRequest("keepalive@openssh.com", true, nil)
				close(answered)
			}(answered)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		select {
		case <-answered:
			misses, answered = 0, nil
		default:
			misses++
			if misses >= c.srv.cfg.KeepaliveMisses {
				c.srv.log.Info("connection closed: keepalive unanswered", "transport", c.transport, "remote", c.remote, "misses", misses)
				_ = c.Close()
				return
			}
		}
	}
}

// authLog is x/crypto's AuthLogCallback, called after every attempt to
// authenticate; err is nil once the client has authenticated. A public
// key handler that said yes is not that: x/crypto asks it also when a
// client only asks whether a key would do, before it has proved
// anything.
// verifiedKey is x/crypto's VerifiedPublicKeyCallback, called when a
// client has signed with a key the public key handler accepted: it notes
// the key, for authLog. The permissions are passed on as they are.
func verifiedKey(cm gossh.ConnMetadata, key gossh.PublicKey, perms *gossh.Permissions, _ string) (*gossh.Permissions, error) {
	if c := connOf(cm.RemoteAddr()); c != nil {
		c.key = key
	}
	return perms, nil
}

func authLog(cm gossh.ConnMetadata, _ string, err error) {
	if err != nil {
		return
	}
	if c := connOf(cm.RemoteAddr()); c != nil {
		c.authenticated()
	}
}

// failureReason is why a connection that did not authenticate was
// closed, for the log: the library's error, cut by cutReason, as it may
// repeat what the peer sent.
func failureReason(err error) string {
	var ne net.Error
	if errors.Is(err, os.ErrDeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()) {
		return reasonHandshakeDeadline
	}
	return cutReason(err.Error())
}

// maxReason is how many bytes of a reason the log holds of a connection
// that has not authenticated.
const maxReason = 256

// cutReason returns s, or when it is longer than maxReason bytes, its
// first maxReason bytes or fewer, cut at the start of a rune, and "…".
func cutReason(s string) string {
	if len(s) <= maxReason {
		return s
	}
	n := maxReason
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
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
