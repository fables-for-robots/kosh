package sshd

import (
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func timeAfterWait() <-chan time.Time { return time.After(waitTime) }

// done reports whether s's context is done, waiting for it.
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
