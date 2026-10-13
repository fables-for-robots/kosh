package sshd

import (
	"errors"
	"io"
	"net"
	"os"
	"strings"
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
