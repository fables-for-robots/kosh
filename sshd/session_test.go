package sshd

import (
	"context"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func timeAfterWait() <-chan time.Time { return time.After(waitTime) }

// contextEnds waits for s's context to be done, and fails the test when
// it is not.
func contextEnds(t *testing.T, s Session, what string) {
	t.Helper()
	select {
	case <-s.Context.Done():
	case <-timeAfterWait():
		t.Fatalf("Session.Context is not done %s", what)
	}
}

func TestSessionGetsItsGrant(t *testing.T) {
	h := newHarness(t)
	key := h.key("guest", true)
	raw := h.dialRaw()
	cl, err := connect(raw, h.addr, clientConfig("kosh", ssh.PublicKeys(key)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cl.Close() })
	tm := openTerm(t, cl)
	tm.waitFor("grant:read-only")
	s := h.nextSession()
	if !s.Grant.ReadOnly || s.Grant.Name != "guest" || s.Grant.Fingerprint != ssh.FingerprintSHA256(key.PublicKey()) {
		t.Errorf("Session.Grant = %+v, want guest, read-only, with the key's fingerprint", s.Grant)
	}
	if s.Transport != "tcp" {
		t.Errorf("Session.Transport = %q, want tcp", s.Transport)
	}
	if want := raw.LocalAddr().String(); s.Remote != want {
		t.Errorf("Session.Remote = %q, want the client's address %q", s.Remote, want)
	}
	if s.Context.Err() != nil {
		t.Error("Session.Context is done while the session runs")
	}
	tm.press("q")
	tm.exit()
}

func TestContextEndsWithTheSession(t *testing.T) {
	h := newHarness(t)
	key := h.key("alice", false)

	tm := openTerm(t, h.dial(key, "kosh"))
	tm.waitFor(testWord)
	s := h.nextSession()
	tm.press("q")
	tm.exit()
	contextEnds(t, s, "after q")

	cl := h.dial(key, "kosh")
	tm = openTerm(t, cl)
	tm.waitFor(testWord)
	s = h.nextSession()
	_ = cl.Close()
	contextEnds(t, s, "after the client closed")
}

// otherKey is a key in the list that no session of the test uses, so
// that a list without the test's key is not empty.
func otherKey(t *testing.T) string { return keyLine(newSigner(t), "other", false) }

func TestKeyRemovedEndsTheSession(t *testing.T) {
	h := newHarness(t)
	alice, bob := newSigner(t), newSigner(t)
	setKeys(t, h.list, keyLine(alice, "alice", false), keyLine(bob, "bob", false))
	ta := openTerm(t, h.dial(alice, "kosh"))
	ta.waitFor(testWord)
	tb := openTerm(t, h.dial(bob, "kosh"))
	tb.waitFor(testWord)

	setKeys(t, h.list, keyLine(bob, "bob", false))
	if st := ta.exit(); st != 1 {
		t.Errorf("the removed key's session: exit status %d, want 1", st)
	}
	// The line comes after the program has given the terminal back.
	if out := ta.stdout.String(); !strings.HasSuffix(out, MsgKeyRemoved+"\r\n") {
		t.Errorf("the removed key's session ends with %q, want the line %q", out[max(0, len(out)-80):], MsgKeyRemoved)
	}
	rec := h.log.waitFor(t, "of the end of alice's session", func(r logRecord) bool {
		return r.Msg == "session end" && r.Attrs["key"] == "alice"
	})
	if rec.Attrs["reason"] != MsgKeyRemoved || rec.Attrs["duration"] == "" || rec.Attrs["transport"] != "tcp" ||
		rec.Attrs["remote"] == "" || rec.Attrs["fingerprint"] != ssh.FingerprintSHA256(alice.PublicKey()) || rec.Attrs["read_only"] != "false" {
		t.Errorf("the end of the session is logged as %v", rec)
	}

	if tb.ended() {
		t.Fatal("the session of the key that stayed has ended")
	}
	tb.press("q")
	if st := tb.exit(); st != 0 {
		t.Errorf("the other session: exit status %d after q, want 0", st)
	}
}

func TestGrantChangedEndsTheSession(t *testing.T) {
	h := newHarness(t)
	alice := newSigner(t)
	for _, ro := range []bool{false, true} {
		setKeys(t, h.list, keyLine(alice, "alice", ro))
		tm := openTerm(t, h.dial(alice, "kosh"))
		tm.waitFor(testWord)
		setKeys(t, h.list, keyLine(alice, "alice", !ro))
		if st := tm.exit(); st != 1 {
			t.Errorf("read-only %v to %v: exit status %d, want 1", ro, !ro, st)
		}
		if out := tm.stdout.String(); !strings.HasSuffix(out, MsgGrantChanged+"\r\n") {
			t.Errorf("read-only %v to %v: the session did not end with %q", ro, !ro, MsgGrantChanged)
		}
	}

	setKeys(t, h.list, keyLine(alice, "alice", false))
	tm := openTerm(t, h.dial(alice, "kosh"))
	tm.waitFor(testWord)
	setKeys(t, h.list, keyLine(alice, "alice-renamed", false))
	tm.press("q")
	if st := tm.exit(); st != 0 {
		t.Errorf("after a change of the name alone: exit status %d after q, want 0", st)
	}
	if strings.Contains(tm.stdout.String(), "kosh:") {
		t.Errorf("after a change of the name alone the session was told %q", tm.stdout.String())
	}
}

// A connection whose key leaves the list before it opened a session is
// closed: it gets no session. (A session that begins while the list
// changes is refused by register, as TestKeyRemovedEndsTheSession's.)
func TestKeyRemovedBetweenHandshakeAndSession(t *testing.T) {
	h := newHarness(t)
	alice := newSigner(t)
	setKeys(t, h.list, keyLine(alice, "alice", false))
	cl := h.dial(alice, "kosh")
	setKeys(t, h.list, otherKey(t))
	select {
	case <-closed(cl):
	case <-timeAfterWait():
		t.Fatal("the connection is still open")
	}
	if _, err := cl.NewSession(); err == nil {
		t.Error("a session was opened")
	}
	if h.models.Load() != 0 {
		t.Error("a model was made")
	}
	fp := ssh.FingerprintSHA256(alice.PublicKey())
	h.log.waitFor(t, "of the closed connection", func(r logRecord) bool {
		return r.Msg == "connection closed" && r.Attrs["reason"] == reasonKeyRemoved && r.Attrs["fingerprint"] == fp
	})
}

// countingConn counts the bytes read from it.
type countingConn struct {
	net.Conn
	read atomic.Int64
}

func (c *countingConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	c.read.Add(int64(n))
	return n, err
}

func TestReplaceDoesNotBlockOnSessions(t *testing.T) {
	h := newHarness(t, withServer(func(s *Server) { s.endTimeout = 200 * time.Millisecond }))
	alice := newSigner(t)
	setKeys(t, h.list, keyLine(alice, "alice", false))
	cc := &countingConn{Conn: h.dialRaw()}
	cl, err := connect(cc, h.addr, clientConfig("kosh", ssh.PublicKeys(alice)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cl.Close() })

	// A session whose output nobody reads, and a program that draws a
	// screen of 100 kB every millisecond: its output stops when the
	// channel's window is full, and the program waits on it.
	sess, err := cl.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	if _, err := sess.StdoutPipe(); err != nil {
		t.Fatal(err)
	}
	stdin, err := sess.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.RequestPty("xterm-256color", 200, 500, ssh.TerminalModes{}); err != nil {
		t.Fatal(err)
	}
	if err := sess.Shell(); err != nil {
		t.Fatal(err)
	}
	s := h.nextSession()
	if _, err := stdin.Write([]byte("f")); err != nil {
		t.Fatal(err)
	}
	// Stalled: nothing more arrives for 200 ms, after more than the
	// channel's window of 2 MB. Drawing 2 MB of screens is work: with the
	// race detector it takes seconds on a fast machine and more than the
	// usual wait on a shared runner, so this wait is a long one.
	last, since := cc.read.Load(), time.Now()
	eventuallyWithin(t, time.Minute, "the output to stall", func() bool {
		if n := cc.read.Load(); n != last {
			last, since = n, time.Now()
		}
		return last > 2<<20 && time.Since(since) > 200*time.Millisecond
	})

	start := time.Now()
	setKeys(t, h.list, otherKey(t))
	if d := time.Since(start); d > 100*time.Millisecond {
		t.Errorf("Replace took %v while a session's client was not reading", d)
	}
	// The session cannot be told; its connection is closed instead.
	contextEnds(t, s, "after its key was removed and it could not end")
	rec := h.log.waitFor(t, "of the session that did not end", func(r logRecord) bool {
		return strings.HasPrefix(r.Msg, "session did not end in time")
	})
	if rec.Attrs["key"] != "alice" || rec.Attrs["fingerprint"] != ssh.FingerprintSHA256(alice.PublicKey()) {
		t.Errorf("the line %v does not name the key by its name and fingerprint", rec)
	}
}

func TestShutdown(t *testing.T) {
	h := newHarness(t)
	alice, bob := newSigner(t), newSigner(t)
	setKeys(t, h.list, keyLine(alice, "alice", false), keyLine(bob, "bob", false))
	ca, cb := h.dial(alice, "kosh"), h.dial(bob, "kosh")
	ta, tb := openTerm(t, ca), openTerm(t, cb)
	ta.waitFor(testWord)
	tb.waitFor(testWord)

	ctx, cancel := context.WithTimeout(context.Background(), waitTime)
	defer cancel()
	if err := h.srv.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	for name, tm := range map[string]*term{"alice": ta, "bob": tb} {
		if st := tm.exit(); st != 1 {
			t.Errorf("%s: exit status %d, want 1", name, st)
		}
		if out := tm.stdout.String(); !strings.HasSuffix(out, MsgShuttingDown+"\r\n") {
			t.Errorf("%s: the session did not end with %q", name, MsgShuttingDown)
		}
	}
	// Shutdown has returned: the connections are closed.
	for name, cl := range map[string]*ssh.Client{"alice": ca, "bob": cb} {
		done := make(chan struct{})
		go func() { _ = cl.Wait(); close(done) }()
		select {
		case <-done:
		case <-timeAfterWait():
			t.Errorf("%s: the connection is still open after Shutdown", name)
		}
	}
	if err := h.serveResult(); err != nil {
		t.Errorf("Serve returned %v, want nil", err)
	}
	if c, err := net.Dial("tcp", h.addr); err == nil {
		_ = c.Close()
		t.Error("a new connection was accepted after Shutdown")
	}
}

func TestClosingTheChannelEndsTheSession(t *testing.T) {
	const idle = 200 * time.Millisecond
	h := newHarness(t, withConfig(func(c *Config) { c.IdleConnection = idle }))
	cl := h.dial(h.key("alice", false), "kosh")
	tm := openTerm(t, cl)
	tm.waitFor(testWord)
	s := h.nextSession()

	// The session channel is closed; the connection stays, as a client
	// of a multiplexing master leaves it.
	_ = tm.sess.Close()
	closedAt := time.Now()
	contextEnds(t, s, "after the client closed the session channel")
	eventually(t, "Sessions() to be 0 after the channel was closed", func() bool { return h.srv.Sessions() == 0 })
	rec := h.log.waitFor(t, "of the end of the session", func(r logRecord) bool { return r.Msg == "session end" })
	if rec.Attrs["reason"] != reasonChannelClosed {
		t.Errorf("the end of a session whose channel the client closed is logged as %v", rec)
	}
	if d := closedByTheServer(t, cl, "a connection whose session channel was closed").Sub(closedAt); d < idle/2 {
		t.Errorf("the connection was closed %v after its session channel, want about %v", d, idle)
	}
}

// windowChange sends a window-change request for cols by rows, without
// waiting for an answer.
func windowChange(t *testing.T, sess *ssh.Session, cols, rows uint32) {
	t.Helper()
	payload := ssh.Marshal(struct{ Columns, Rows, Width, Height uint32 }{cols, rows, 0, 0})
	if _, err := sess.SendRequest("window-change", false, payload); err != nil {
		t.Fatalf("window-change: %v", err)
	}
}

func TestWindowChangeBeforeShell(t *testing.T) {
	h := newHarness(t)
	cl := h.dial(h.key("alice", false), "kosh")
	sess, err := cl.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	out := &syncBuffer{}
	sess.Stdout = out
	stdin, err := sess.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.RequestPty("xterm-256color", 24, 80, ssh.TerminalModes{}); err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		windowChange(t, sess, 100+uint32(i), 30)
	}
	shell := make(chan error, 1)
	go func() { shell <- sess.Shell() }()
	select {
	case err := <-shell:
		if err != nil {
			t.Fatalf("shell after window changes: %v", err)
		}
	case <-timeAfterWait():
		t.Fatal("the shell request has no answer after window changes")
	}
	eventually(t, "the program", func() bool { return strings.Contains(out.String(), testWord) })
	if _, err := stdin.Write([]byte("q")); err != nil {
		t.Fatal(err)
	}
	if err := sess.Wait(); err != nil {
		t.Errorf("the session: %v", err)
	}
}

// pausedConn holds what it reads while it is paused.
type pausedConn struct {
	net.Conn
	mu   sync.Mutex
	gate chan struct{}
}

func (c *pausedConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	c.mu.Lock()
	gate := c.gate
	c.mu.Unlock()
	if gate != nil {
		<-gate
	}
	return n, err
}

func (c *pausedConn) pause() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.gate == nil {
		c.gate = make(chan struct{})
	}
}

// resume lets what was held through; it may be called when not paused.
func (c *pausedConn) resume() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.gate != nil {
		close(c.gate)
		c.gate = nil
	}
}

// After the handler has returned, the session channel stays open until
// the client answers its close; window changes until then are dropped.
func TestWindowChangeAfterTheSession(t *testing.T) {
	h := newHarness(t)
	key := h.key("alice", false)
	pc := &pausedConn{Conn: h.dialRaw()}
	t.Cleanup(pc.resume)
	cl, err := connect(pc, h.addr, clientConfig("kosh", ssh.PublicKeys(key)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cl.Close() })
	sess, err := cl.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	if err := sess.RequestPty("xterm-256color", 24, 80, ssh.TerminalModes{}); err != nil {
		t.Fatal(err)
	}

	// The client reads nothing from now on: it sees neither the answer
	// to exec nor the server's close, and does not answer the close.
	pc.pause()
	started := make(chan error, 1)
	go func() { started <- sess.Start("ls") }()
	h.log.waitFor(t, "of the refused command", func(r logRecord) bool { return r.Msg == "session refused" })
	for i := range 3 {
		windowChange(t, sess, 100+uint32(i), 30)
	}
	// The server has taken all three, after the handler returned and
	// before the channel closed.
	eventually(t, "the server to take the window changes", func() bool { return h.windowWidths()[102] })
	pc.resume()
	if err := <-started; err != nil {
		t.Fatalf("exec: %v", err)
	}
	if st := exitStatus(t, sess.Wait()); st != 1 {
		t.Errorf("exit status %d, want 1", st)
	}
}

// windowWidths returns the widths of the windows of the server's open
// session channels.
func (h *harness) windowWidths() map[int]bool {
	h.srv.mu.Lock()
	ws := make([]*window, 0, len(h.srv.windows))
	for _, w := range h.srv.windows {
		ws = append(ws, w)
	}
	h.srv.mu.Unlock()
	widths := map[int]bool{}
	for _, w := range ws {
		w.mu.Lock()
		widths[w.size.Width] = true
		w.mu.Unlock()
	}
	return widths
}

func TestEndAfterTheSessionFinishedDoesNothing(t *testing.T) {
	h := newHarness(t)
	sess := &session{srv: h.srv, conn: &conn{srv: h.srv}}
	sess.finish()
	if sess.end(MsgGrantChanged) {
		t.Error("end ended a session that had finished")
	}
	if sess.timer != nil || sess.ending() != "" {
		t.Error("end armed its timer, or set a reason, on a session that had finished")
	}
}
