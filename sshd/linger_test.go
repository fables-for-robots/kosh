package sshd

import (
	"context"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// writeWatch is a client's connection that notes the errors of its
// writes.
type writeWatch struct {
	net.Conn
	mu   sync.Mutex
	errs []error
}

func (w *writeWatch) Write(p []byte) (int, error) {
	n, err := w.Conn.Write(p)
	if err != nil {
		w.mu.Lock()
		w.errs = append(w.errs, err)
		w.mu.Unlock()
	}
	return n, err
}

func (w *writeWatch) failed() []error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]error(nil), w.errs...)
}

// keyRemovedLines returns how many times the log says that a connection
// was closed because its key is gone.
func (h *harness) keyRemovedLines() int {
	n := 0
	for _, r := range h.log.records() {
		if r.Msg == "connection closed" && r.Attrs["reason"] == reasonKeyRemoved {
			n++
		}
	}
	return n
}

// connections returns how many connections the server knows of.
func (h *harness) connections() int {
	h.srv.mu.Lock()
	defer h.srv.mu.Unlock()
	return len(h.srv.conns)
}

// removedKeySession opens a connection over w with a new key and a
// session on it, removes the key, and waits for the session to end with
// MsgKeyRemoved.
func removedKeySession(t *testing.T, h *harness) (*ssh.Client, *writeWatch) {
	t.Helper()
	w := &writeWatch{Conn: h.dialRaw()}
	cl, err := connect(w, h.addr, clientConfig("kosh", ssh.PublicKeys(h.key("ann", false))))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cl.Close() })
	tm := openTerm(t, cl)
	tm.waitFor(testWord)
	setKeys(t, h.list)
	if st := tm.exit(); st != 1 {
		t.Errorf("exit status %d, want 1", st)
	}
	if out := tm.stdout.String(); !strings.HasSuffix(out, MsgKeyRemoved+"\r\n") {
		t.Errorf("the session did not end with %q", MsgKeyRemoved)
	}
	return cl, w
}

// TestRemovedKeysClientMayStillWrite ends a session by removing its key,
// over TCP, and has the client write twice more, with a pause, before it
// closes: what a real client's last messages are. The server has not
// closed the connection under them: no write fails, and the close is
// clean. (A connection closed at once answers the first of such writes
// with a reset, and the second fails; behind a port-forward that ends
// the forward for everybody.)
func TestRemovedKeysClientMayStillWrite(t *testing.T) {
	// The linger as it is served.
	h := newHarness(t, withServer(func(s *Server) { s.keyLinger = time.Second }))
	cl, w := removedKeySession(t, h)
	for i := 1; i <= 2; i++ {
		if _, _, err := cl.SendRequest("still-here@kosh.test", false, nil); err != nil {
			t.Errorf("the client's write %d after its session ended: %v", i, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err := cl.Close(); err != nil {
		t.Errorf("the client's close: %v", err)
	}
	if errs := w.failed(); len(errs) != 0 {
		t.Errorf("writes of the client failed on its connection: %v", errs)
	}
	eventually(t, "the server has forgotten the connection", func() bool { return h.connections() == 0 })
	if n := h.keyRemovedLines(); n != 1 {
		t.Errorf("the log has %d lines of a connection closed for its key, want 1:\n%s", n, h.log.dump())
	}
}

// onlyConn returns the server's one connection.
func (h *harness) onlyConn() *conn {
	h.t.Helper()
	h.srv.mu.Lock()
	defer h.srv.mu.Unlock()
	if len(h.srv.conns) != 1 {
		h.t.Fatalf("the server has %d connections, want 1", len(h.srv.conns))
	}
	for c := range h.srv.conns {
		return c
	}
	return nil
}

// noLinger fails the test when c has a timer of its linger still.
func noLinger(t *testing.T, c *conn) {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.linger != nil || !c.closed {
		t.Errorf("the connection is closed: %v; it has a timer of its linger: %v", c.closed, c.linger != nil)
	}
}

// TestRemovedKeysClientCloses has the client close its connection when
// its session has ended, as clients do: the server forgets the
// connection then, and does not wait out its linger.
func TestRemovedKeysClientCloses(t *testing.T) {
	h := newHarness(t, withServer(func(s *Server) { s.keyLinger = time.Hour }))
	cl, _ := removedKeySession(t, h)
	c := h.onlyConn()
	began := time.Now()
	if err := cl.Close(); err != nil {
		t.Errorf("the client's close: %v", err)
	}
	eventually(t, "the server has forgotten the connection", func() bool { return h.connections() == 0 })
	t.Logf("the server forgot the connection %v after the client closed it", time.Since(began).Round(time.Millisecond))
	noLinger(t, c)
	if n := h.keyRemovedLines(); n != 1 {
		t.Errorf("the log has %d lines of a connection closed for its key, want 1:\n%s", n, h.log.dump())
	}
}

// TestRemovedKeysConnectionIsClosedAfterTheLinger has a client keep its
// connection when its session has ended: the server closes it after the
// linger, and not before. In the linger the connection gets no session,
// and no request of it is served.
func TestRemovedKeysConnectionIsClosedAfterTheLinger(t *testing.T) {
	const linger = 500 * time.Millisecond
	h := newHarness(t, withServer(func(s *Server) { s.keyLinger = linger }))
	cl, w := removedKeySession(t, h)
	ended := time.Now()
	c := h.onlyConn()

	// In the linger.
	if _, err := cl.NewSession(); err == nil {
		t.Error("a session was opened on the connection of a key that is gone")
	} else if !strings.Contains(err.Error(), "too many sessions") {
		// The server's own refusal, and not a connection that is closed.
		t.Errorf("a session on the connection of a key that is gone was not refused by the server: %v", err)
	}
	for _, name := range []string{"tcpip-forward", "keepalive@openssh.com", "no-such-request@kosh.test"} {
		if ok, _, err := cl.SendRequest(name, true, nil); ok || err != nil {
			t.Errorf("the request %s on the connection of a key that is gone: %v, %v; want it refused", name, ok, err)
		}
	}
	if n := h.keyRemovedLines(); n != 0 {
		t.Errorf("the log says %d times that the connection was closed, in its linger", n)
	}
	if d := time.Since(ended); d > linger {
		t.Fatalf("the test was too slow (%v) to look at the connection within its linger of %v", d, linger)
	}

	at := closedByTheServer(t, cl, "after the linger")
	if d := at.Sub(ended); d < linger-50*time.Millisecond || d > linger+2*time.Second {
		t.Errorf("the connection was closed %v after its last session ended, want after the linger of %v", d, linger)
	}
	eventually(t, "the server has forgotten the connection", func() bool { return h.connections() == 0 })
	noLinger(t, c)
	if n := h.keyRemovedLines(); n != 1 {
		t.Errorf("the log has %d lines of a connection closed for its key, want 1:\n%s", n, h.log.dump())
	}
	if errs := w.failed(); len(errs) != 0 {
		t.Errorf("writes of the client failed on its connection: %v", errs)
	}
}

// TestShutdownDoesNotWaitALinger shuts the server down while a
// connection is in its linger: the connection is closed at once, and its
// timer stopped.
func TestShutdownDoesNotWaitALinger(t *testing.T) {
	h := newHarness(t, withServer(func(s *Server) { s.keyLinger = time.Hour }))
	cl, _ := removedKeySession(t, h)
	c := h.onlyConn()
	began := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), waitTime)
	defer cancel()
	if err := h.srv.Shutdown(ctx); err != nil {
		t.Errorf("Shutdown: %v", err)
	}
	if d := time.Since(began); d > 2*time.Second {
		t.Errorf("Shutdown took %v with a connection in its linger", d)
	}
	closedByTheServer(t, cl, "at the shutdown")
	noLinger(t, c)
	if n := h.keyRemovedLines(); n != 1 {
		t.Errorf("the log has %d lines of a connection closed for its key, want 1:\n%s", n, h.log.dump())
	}
}

// TestCutOffConnectionHasNoLinger removes the key of a session that does
// not end: its connection is closed by the end's timer, and when the
// session ends at last, no linger begins for a connection that is
// closed.
func TestCutOffConnectionHasNoLinger(t *testing.T) {
	stuck := make(chan struct{})
	hv := newHandover(func(context.Context, Terminal) error {
		<-stuck
		return nil
	})
	h := handoverHarness(t, hv, withServer(func(s *Server) {
		s.endTimeout = 100 * time.Millisecond
		s.keyLinger = time.Hour
	}))
	cl := h.dial(h.key("ann", false), "u")
	tm := openTerm(t, cl)
	tm.waitFor("LASTLINE")
	tm.press("!")
	hv.wait(t, "the command has begun", func() bool { return hv.began == 1 })
	c := h.onlyConn()
	setKeys(t, h.list)
	closedByTheServer(t, cl, "when the session did not end")
	close(stuck)
	h.log.waitFor(t, "of the session's end", func(r logRecord) bool { return r.Msg == "session end" })
	eventually(t, "the server has forgotten the connection", func() bool { return h.connections() == 0 })
	noLinger(t, c)
	if n := h.keyRemovedLines(); n != 1 {
		t.Errorf("the log has %d lines of a connection closed for its key, want 1:\n%s", n, h.log.dump())
	}
}

// TestKeyRemovedJustAfterASession removes a key when its connection's
// session has just ended by itself: the client is in its last writes as
// much as one whose session the removal ended, and its connection is
// closed after the linger, counted from the session's end.
func TestKeyRemovedJustAfterASession(t *testing.T) {
	const linger = 500 * time.Millisecond
	h := newHarness(t, withServer(func(s *Server) { s.keyLinger = linger }))
	w := &writeWatch{Conn: h.dialRaw()}
	cl, err := connect(w, h.addr, clientConfig("kosh", ssh.PublicKeys(h.key("ann", false))))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cl.Close() })
	tm := openTerm(t, cl)
	tm.waitFor(testWord)
	tm.press("q")
	if st := tm.exit(); st != 0 {
		t.Fatalf("exit status %d", st)
	}
	ended := time.Now()
	setKeys(t, h.list)
	for i := 1; i <= 2; i++ {
		if _, _, err := cl.SendRequest("still-here@kosh.test", false, nil); err != nil {
			t.Errorf("the client's write %d after its session ended: %v", i, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	at := closedByTheServer(t, cl, "after the linger")
	if d := at.Sub(ended); d < linger-100*time.Millisecond || d > linger+2*time.Second {
		t.Errorf("the connection was closed %v after its session ended, want after the linger of %v", d, linger)
	}
	if errs := w.failed(); len(errs) != 0 {
		t.Errorf("writes of the client failed on its connection: %v", errs)
	}
	eventually(t, "the server has forgotten the connection", func() bool { return h.connections() == 0 })
	if n := h.keyRemovedLines(); n != 1 {
		t.Errorf("the log has %d lines of a connection closed for its key, want 1", n)
	}
}
