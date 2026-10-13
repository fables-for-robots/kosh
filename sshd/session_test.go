package sshd

import (
	"context"
	"net"
	"strings"
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

func TestKeyRemovedBetweenHandshakeAndSession(t *testing.T) {
	h := newHarness(t)
	alice := newSigner(t)
	setKeys(t, h.list, keyLine(alice, "alice", false))
	cl := h.dial(alice, "kosh")
	setKeys(t, h.list, otherKey(t))
	tm := openTerm(t, cl)
	if st := tm.exit(); st != 1 {
		t.Errorf("exit status %d, want 1", st)
	}
	if got := tm.stdout.String(); got != MsgKeyRemoved+"\r\n" {
		t.Errorf("the session was told %q, want %q", got, MsgKeyRemoved)
	}
	if h.models.Load() != 0 {
		t.Error("a model was made")
	}
	fp := ssh.FingerprintSHA256(alice.PublicKey())
	h.log.waitFor(t, "of the refused session", func(r logRecord) bool {
		return r.Msg == "session refused" && r.Attrs["reason"] == MsgKeyRemoved && r.Attrs["fingerprint"] == fp
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
	// channel's window of 2 MB.
	last, since := cc.read.Load(), time.Now()
	eventually(t, "the output to stall", func() bool {
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
