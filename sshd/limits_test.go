package sshd

import (
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// readBanner reads the server's version line from c, byte by byte so as
// to read nothing after it, and fails the test when there is none.
func readBanner(t *testing.T, c net.Conn) string {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(waitTime))
	defer c.SetReadDeadline(time.Time{})
	var line []byte
	b := make([]byte, 1)
	for {
		if _, err := c.Read(b); err != nil {
			t.Fatalf("reading the server's version, after %q: %v", line, err)
		}
		line = append(line, b[0])
		if b[0] == '\n' {
			return string(line)
		}
	}
}

// waitClosed waits until the server closes c, reading and discarding
// what it sends, and returns how many bytes that was.
func waitClosed(t *testing.T, c net.Conn, what string) int {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(waitTime))
	n, err := io.Copy(io.Discard, c)
	if errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("%s: the server has not closed the connection", what)
	}
	return int(n)
}

// replayConn is a connection of which some bytes have been read already:
// they are read again first.
type replayConn struct {
	net.Conn
	r io.Reader
}

func (c replayConn) Read(p []byte) (int, error) { return c.r.Read(p) }

func replay(c net.Conn, read string) net.Conn {
	return replayConn{Conn: c, r: io.MultiReader(strings.NewReader(read), c)}
}

// refusedByTheLimit checks that the server closed c at once without a
// word, and logged it.
func (h *harness) refusedByTheLimit(c net.Conn) {
	h.t.Helper()
	if n := waitClosed(h.t, c, "a connection over the limit"); n != 0 {
		h.t.Errorf("a connection over the limit got %d bytes", n)
	}
	remote := c.LocalAddr().String()
	rec := h.log.waitFor(h.t, "of the connection over the limit", func(r logRecord) bool { return r.Attrs["remote"] == remote })
	if rec.Attrs["reason"] != reasonTooManyHandshakes || rec.Attrs["transport"] != "tcp" {
		h.t.Errorf("the connection over the limit is logged as %v", rec)
	}
}

// closedByTheDeadline checks that the log says the deadline closed c.
func (h *harness) closedByTheDeadline(c net.Conn) {
	h.t.Helper()
	remote := c.LocalAddr().String()
	rec := h.log.waitFor(h.t, "of the connection closed by the deadline", func(r logRecord) bool { return r.Attrs["remote"] == remote })
	if rec.Attrs["reason"] != reasonHandshakeDeadline || rec.Attrs["transport"] != "tcp" {
		h.t.Errorf("the connection closed by the deadline is logged as %v", rec)
	}
}

func TestSilentPeerIsClosedByTheDeadline(t *testing.T) {
	h := newHarness(t, withConfig(func(c *Config) { c.HandshakeTimeout = 100 * time.Millisecond }))
	c := h.dialRaw()
	if got := readBanner(t, c); got != "SSH-2.0-kosh_test-1.2.3\r\n" {
		t.Errorf("the server's version line is %q", got)
	}
	waitClosed(t, c, "a peer that sends nothing")
	h.closedByTheDeadline(c)
}

func TestGarbagePeerIsClosed(t *testing.T) {
	h := newHarness(t, withConfig(func(c *Config) { c.HandshakeTimeout = 200 * time.Millisecond }))
	key := h.key("alice", false)

	http := h.dialRaw()
	if _, err := io.WriteString(http, "GET / HTTP/1.1\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	waitClosed(t, http, "a peer that speaks HTTP")

	zeros := h.dialRaw()
	go func() { _, _ = zeros.Write(make([]byte, 1<<20)) }() // fails when the server closes
	waitClosed(t, zeros, "a peer that sends 1 MB of zeros")

	tm := openTerm(t, h.dial(key, "kosh"))
	tm.waitFor(testWord)
	tm.press("q")
	tm.exit()
}

func TestHandshakeLimit(t *testing.T) {
	h := newHarness(t, withConfig(func(c *Config) {
		c.MaxHandshakes = 3
		c.HandshakeTimeout = 500 * time.Millisecond
	}))
	key := h.key("alice", false)

	idle := make([]net.Conn, 3)
	banners := make([]string, 3)
	for i := range idle {
		idle[i] = h.dialRaw()
		banners[i] = readBanner(t, idle[i])
	}
	h.refusedByTheLimit(h.dialRaw())

	// The first one authenticates: it leaves the handshake.
	cl, err := connect(replay(idle[0], banners[0]), h.addr, clientConfig("kosh", ssh.PublicKeys(key)))
	if err != nil {
		t.Fatalf("authenticating on a held connection: %v", err)
	}
	defer cl.Close()
	late := h.dialRaw()
	readBanner(t, late)
	h.refusedByTheLimit(h.dialRaw())

	// The deadline closes the idle ones, and their places are free.
	for _, c := range []net.Conn{idle[1], idle[2], late} {
		waitClosed(t, c, "an idle connection")
		h.closedByTheDeadline(c)
	}
	for range 3 {
		readBanner(t, h.dialRaw())
	}
	h.refusedByTheLimit(h.dialRaw())
}

// blockingSigner is a key whose Sign waits until release is closed, and
// fails then. x/crypto's client asks whether a key would do before it
// signs: the server has said yes when Sign is called.
type blockingSigner struct {
	ssh.Signer
	asked   chan struct{}
	release chan struct{}
}

func (s *blockingSigner) Sign(io.Reader, []byte) (*ssh.Signature, error) {
	close(s.asked)
	<-s.release
	return nil, errors.New("test: not signing")
}

func TestAskingAboutAKeyDoesNotLeaveTheHandshake(t *testing.T) {
	h := newHarness(t, withConfig(func(c *Config) {
		c.MaxHandshakes = 1
		c.HandshakeTimeout = 300 * time.Millisecond
	}))
	signer := &blockingSigner{Signer: h.key("alice", false), asked: make(chan struct{}), release: make(chan struct{})}
	c := h.dialRaw()
	connected := make(chan error, 1)
	go func() {
		_, err := connect(c, h.addr, clientConfig("kosh", ssh.PublicKeys(signer)))
		connected <- err
	}()
	defer func() {
		close(signer.release)
		if err := <-connected; err == nil {
			t.Error("a client that did not sign got in")
		}
	}()
	select {
	case <-signer.asked:
	case <-timeAfterWait():
		t.Fatal("the client was not asked to sign")
	}

	// The server said the key would do; the client has not proved it.
	h.refusedByTheLimit(h.dialRaw())
	h.closedByTheDeadline(c)
	readBanner(t, h.dialRaw())
}

// closed returns a channel that is closed when cl's connection is.
func closed(cl *ssh.Client) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		_ = cl.Wait()
		close(done)
	}()
	return done
}

// closedByTheServer waits for the server to close cl's connection, and
// returns when it did.
func closedByTheServer(t *testing.T, cl *ssh.Client, what string) time.Time {
	t.Helper()
	select {
	case <-closed(cl):
		return time.Now()
	case <-timeAfterWait():
		t.Fatalf("%s: the server has not closed the connection", what)
		return time.Time{}
	}
}

// stillOpen fails the test when cl's connection is closed within d.
func stillOpen(t *testing.T, cl *ssh.Client, d time.Duration, what string) {
	t.Helper()
	select {
	case <-closed(cl):
		t.Fatalf("%s: the connection was closed", what)
	case <-time.After(d):
	}
}

func TestConnectionWithoutASessionIsClosed(t *testing.T) {
	const idle = 100 * time.Millisecond
	h := newHarness(t, withConfig(func(c *Config) { c.IdleConnection = idle }))
	key := h.key("alice", false)

	// No session at all.
	cl := h.dial(key, "kosh")
	connected := time.Now()
	if d := closedByTheServer(t, cl, "a connection that opened no session").Sub(connected); d < idle/2 {
		t.Errorf("a connection without a session was closed after %v, want about %v", d, idle)
	}

	// A session that ended.
	cl = h.dial(key, "kosh")
	tm := openTerm(t, cl)
	tm.waitFor(testWord)
	tm.press("q")
	tm.exit()
	ended := time.Now()
	if d := closedByTheServer(t, cl, "a connection whose session ended").Sub(ended); d < idle/2 {
		t.Errorf("a connection whose session ended was closed after %v, want about %v", d, idle)
	}

	// A session that is open, however long.
	cl = h.dial(key, "kosh")
	tm = openTerm(t, cl)
	tm.waitFor(testWord)
	stillOpen(t, cl, 10*idle, "a connection with an open session")
	tm.press("q")
	tm.exit()

	// A second session, opened soon after the first ended.
	cl = h.dial(key, "kosh")
	tm = openTerm(t, cl)
	tm.waitFor(testWord)
	tm.press("q")
	tm.exit()
	tm = openTerm(t, cl)
	tm.waitFor(testWord)
	stillOpen(t, cl, 3*idle, "a connection whose second session is open")
	tm.press("q")
	if st := tm.exit(); st != 0 {
		t.Errorf("the second session: exit status %d", st)
	}
}

// deafConn swallows what the server sends once deaf is set: the client
// sees no keepalive, and answers none.
type deafConn struct {
	net.Conn
	deaf atomic.Bool
}

func (c *deafConn) Read(p []byte) (int, error) {
	for {
		n, err := c.Conn.Read(p)
		if err != nil || !c.deaf.Load() {
			return n, err
		}
	}
}

func keepaliveFast(c *Config) {
	c.KeepaliveInterval = 20 * time.Millisecond
	c.KeepaliveMisses = 3
}

func TestKeepaliveClosesADeadPeer(t *testing.T) {
	h := newHarness(t, withConfig(keepaliveFast))
	key := h.key("alice", false)
	dc := &deafConn{Conn: h.dialRaw()}
	cl, err := connect(dc, h.addr, clientConfig("kosh", ssh.PublicKeys(key)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cl.Close() })
	tm := openTerm(t, cl)
	tm.waitFor(testWord)
	s := h.nextSession()

	dc.deaf.Store(true)
	closedByTheServer(t, cl, "a peer that stopped answering")
	contextEnds(t, s, "after the keepalive closed the connection")
	eventually(t, "Sessions() to be 0", func() bool { return h.srv.Sessions() == 0 })
}

func TestKeepaliveLeavesALivePeer(t *testing.T) {
	h := newHarness(t, withConfig(keepaliveFast))
	key := h.key("alice", false)
	sc, chans, reqs, err := ssh.NewClientConn(h.dialRaw(), h.addr, clientConfig("kosh", ssh.PublicKeys(key)))
	if err != nil {
		t.Fatal(err)
	}
	// The client answers every request as x/crypto's does by itself,
	// and counts the keepalives.
	var keepalives atomic.Int32
	go func() {
		for r := range reqs {
			if r.Type == "keepalive@openssh.com" {
				keepalives.Add(1)
			}
			_ = r.Reply(false, nil)
		}
	}()
	none := make(chan *ssh.Request)
	close(none)
	cl := ssh.NewClient(sc, chans, none)
	t.Cleanup(func() { _ = cl.Close() })

	tm := openTerm(t, cl)
	tm.waitFor(testWord)
	s := h.nextSession()
	eventually(t, "20 keepalives", func() bool { return keepalives.Load() >= 20 })
	if tm.ended() || s.Context.Err() != nil {
		t.Fatal("the session of a peer that answers has ended")
	}
	tm.press("q")
	if st := tm.exit(); st != 0 {
		t.Errorf("exit status %d after q", st)
	}
}

// serverConn returns the server's conn of the client at remote.
func (h *harness) serverConn(remote string) *conn {
	h.t.Helper()
	var found *conn
	eventually(h.t, "the server's side of the connection", func() bool {
		h.srv.mu.Lock()
		defer h.srv.mu.Unlock()
		for c := range h.srv.conns {
			if c.remote == remote {
				found = c
				return true
			}
		}
		return false
	})
	return found
}

func (c *conn) channelCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.channels
}

// rejectedAsTooMany checks that a new session channel of cl is rejected
// for a shortage of resources.
func rejectedAsTooMany(t *testing.T, cl *ssh.Client) {
	t.Helper()
	sess, err := cl.NewSession()
	if err == nil {
		_ = sess.Close()
		t.Fatalf("session channel %d of a connection was accepted", maxSessionChannels+1)
	}
	var oce *ssh.OpenChannelError
	if !errors.As(err, &oce) || oce.Reason != ssh.ResourceShortage {
		t.Fatalf("session channel %d: %v, want rejected for a shortage of resources", maxSessionChannels+1, err)
	}
}

func TestSessionChannelLimit(t *testing.T) {
	h := newHarness(t)
	raw := h.dialRaw()
	cl, err := connect(raw, h.addr, clientConfig("kosh", ssh.PublicKeys(h.key("alice", false))))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cl.Close() })
	sc := h.serverConn(raw.LocalAddr().String())

	// Channels that never ask for a shell, some with a terminal.
	open := make([]*ssh.Session, maxSessionChannels)
	for i := range open {
		if open[i], err = cl.NewSession(); err != nil {
			t.Fatalf("session channel %d: %v", i+1, err)
		}
		if i%2 == 0 {
			if err := open[i].RequestPty("xterm-256color", 24, 80, ssh.TerminalModes{}); err != nil {
				t.Fatal(err)
			}
		}
	}
	rejectedAsTooMany(t, cl)
	if n := sc.channelCount(); n != maxSessionChannels {
		t.Errorf("the server counts %d session channels, want %d", n, maxSessionChannels)
	}

	// One closes: another is accepted.
	_ = open[0].Close()
	eventually(t, "a session channel to be accepted after one closed", func() bool {
		open[0], err = cl.NewSession()
		return err == nil
	})
	rejectedAsTooMany(t, cl)

	// Three are refused sessions: their places come back.
	for _, sess := range open[:3] {
		if err := sess.Run("ls"); err == nil {
			t.Fatal("a command was run")
		}
	}
	eventually(t, "the refused sessions to be counted out", func() bool { return sc.channelCount() == maxSessionChannels-3 })
	for i := range 3 {
		if open[i], err = cl.NewSession(); err != nil {
			t.Fatalf("a session channel after three were refused: %v", err)
		}
	}
	rejectedAsTooMany(t, cl)

	// The connection drops: every place comes back.
	_ = cl.Close()
	eventually(t, "the channels of a closed connection to be counted out", func() bool { return sc.channelCount() == 0 })
}

func TestConnectionsPerKeyLimit(t *testing.T) {
	h := newHarness(t, withConfig(func(c *Config) { c.IdleConnection = time.Minute }))
	alice, bob := h.key("alice", false), h.key("bob", false)
	clients := make([]*ssh.Client, maxConnectionsPerKey)
	for i := range clients {
		clients[i] = h.dial(alice, "kosh")
	}

	raw := h.dialRaw()
	if cl, err := connect(raw, h.addr, clientConfig("kosh", ssh.PublicKeys(alice))); err == nil {
		_ = cl.Close()
		t.Fatalf("connection %d of a key got in", maxConnectionsPerKey+1)
	}
	remote := raw.LocalAddr().String()
	rec := h.log.waitFor(t, "of the connection over the key's limit", func(r logRecord) bool { return r.Attrs["remote"] == remote })
	if rec.Attrs["reason"] != reasonTooManyConnectionsOfKey || rec.Attrs["key"] != "alice" ||
		rec.Attrs["fingerprint"] != ssh.FingerprintSHA256(alice.PublicKey()) || rec.Attrs["transport"] != "tcp" {
		t.Errorf("the connection over the key's limit is logged as %v", rec)
	}
	n := 0
	for _, r := range h.log.records() {
		if r.Attrs["remote"] == remote {
			n++
		}
	}
	if n != 1 {
		t.Errorf("the connection over the key's limit is logged in %d records, want 1", n)
	}

	// The others are up.
	for i, cl := range clients {
		select {
		case <-closed(cl):
			t.Fatalf("connection %d of the key was closed", i+1)
		default:
		}
	}
	tm := openTerm(t, clients[0])
	tm.waitFor(testWord)
	tm.press("q")
	tm.exit()

	// Another key is not counted with it.
	_ = h.dial(bob, "kosh")

	// One closes: another gets in.
	_ = clients[1].Close()
	eventually(t, "a connection of the key to get in after one closed", func() bool {
		cl, err := connect(h.dialRaw(), h.addr, clientConfig("kosh", ssh.PublicKeys(alice)))
		if err != nil {
			return false
		}
		t.Cleanup(func() { _ = cl.Close() })
		return true
	})
}

// endLogged waits for the record of the end of c before it authenticated,
// and fails the test when there is more than one or when it is not at
// level.
func (h *harness) endLogged(c net.Conn, level slog.Level, what string) logRecord {
	h.t.Helper()
	remote := c.LocalAddr().String()
	of := func(r logRecord) bool {
		return r.Msg == "connection closed before authenticating" && r.Attrs["remote"] == remote
	}
	rec := h.log.waitFor(h.t, "of "+what, of)
	var n int
	for _, r := range h.log.records() {
		if of(r) {
			n++
		}
	}
	if n != 1 || rec.Level != level || rec.Attrs["transport"] != "tcp" {
		h.t.Errorf("%s: %d records, the first %v; want one at %v", what, n, rec, level)
	}
	return rec
}

// The kubelet's TCP probe connects and closes, twice every 10 seconds.
func TestProbeIsLoggedAtDebug(t *testing.T) {
	h := newHarness(t, withConfig(func(c *Config) { c.HandshakeTimeout = 200 * time.Millisecond }))

	probe := h.dialRaw()
	_ = probe.Close()
	h.endLogged(probe, slog.LevelDebug, "a probe")

	said := h.dialRaw()
	if _, err := io.WriteString(said, "SSH-2.0-x\r\n"); err != nil {
		t.Fatal(err)
	}
	readBanner(t, said)
	_ = said.Close()
	h.endLogged(said, slog.LevelInfo, "a client that said its version")

	silent := h.dialRaw()
	waitClosed(t, silent, "a peer that sends nothing")
	if rec := h.endLogged(silent, slog.LevelInfo, "a peer cut by the deadline"); rec.Attrs["reason"] != reasonHandshakeDeadline {
		t.Errorf("the peer cut by the deadline is logged as %v", rec)
	}
}
