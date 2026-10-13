package sshd

import (
	"bytes"
	"encoding/binary"
	"io"
	"log/slog"
	"runtime"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	cssh "charm.land/ssh"
	"golang.org/x/crypto/ssh"
)

// envPayload is the payload of an env request.
func envPayload(name, value string) []byte {
	return ssh.Marshal(struct{ Name, Value string }{name, value})
}

// heapInUse returns the heap in use after a collection.
func heapInUse() uint64 {
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapInuse
}

func TestEnvRequestsAreBounded(t *testing.T) {
	envs := make(chan []string, 10)
	h := newHarness(t, withServer(func(s *Server) {
		handle := s.ssh.Handler
		s.ssh.Handler = func(ss cssh.Session) {
			envs <- ss.Environ()
			handle(ss)
		}
	}))
	key := h.key("alice", false)
	cl := h.dial(key, "kosh")

	// A session on the first channel, so that the connection is not idle.
	tm := openTerm(t, cl)
	tm.waitFor(testWord)
	<-envs

	// A second channel that is sent 3,000 env requests of 30 KiB.
	ch, reqs, err := cl.OpenChannel("session", nil)
	if err != nil {
		t.Fatal(err)
	}
	go ssh.DiscardRequests(reqs)
	t.Cleanup(func() { _ = ch.Close() })
	before := heapInUse()
	value := strings.Repeat("v", 30<<10)
	accepted := 0
	for i := 0; i < 3000; i++ {
		ok, err := ch.SendRequest("env", true, envPayload("VAR", value))
		if err != nil {
			t.Fatalf("env request %d: %v", i, err)
		}
		if ok {
			accepted++
		}
	}
	if accepted != 0 {
		t.Errorf("%d env requests of 30 KiB were accepted, want none: 16 KiB in all", accepted)
	}
	if grew := int64(heapInUse()) - int64(before); grew > 16<<20 {
		t.Errorf("the heap grew by %d MiB", grew>>20)
	}

	// The first session is undisturbed.
	tm.press("q")
	if st := tm.exit(); st != 0 {
		t.Errorf("the first session ended with %d", st)
	}

	// A normal client's handful of env requests reach the session; more
	// than 64 do not.
	sess, err := cl.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	for _, kv := range [][2]string{{"LANG", "C.UTF-8"}, {"LC_ALL", "C"}, {"TERM_PROGRAM", "test"}} {
		if err := sess.Setenv(kv[0], kv[1]); err != nil {
			t.Errorf("Setenv %s: %v", kv[0], err)
		}
	}
	for i := 3; i < 70; i++ {
		ok, err := sess.SendRequest("env", true, envPayload("N"+strings.Repeat("x", i), "1"))
		if err != nil {
			t.Fatal(err)
		}
		if ok != (i < 64) {
			t.Errorf("env request %d: accepted %v", i+1, ok)
		}
	}
	if err := sess.RequestPty("xterm-256color", 24, 80, ssh.TerminalModes{}); err != nil {
		t.Fatal(err)
	}
	if err := sess.Shell(); err != nil {
		t.Fatal(err)
	}
	select {
	case env := <-envs:
		got := strings.Join(env, " ")
		for _, want := range []string{"LANG=C.UTF-8", "LC_ALL=C", "TERM_PROGRAM=test"} {
			if !strings.Contains(got, want) {
				t.Errorf("the session's environment %q has no %s", got, want)
			}
		}
		if len(env) != 64 {
			t.Errorf("the session has %d variables, want 64", len(env))
		}
	case <-timeAfterWait():
		t.Fatal("the session did not start")
	}
}

func TestOnlyKnownRequests(t *testing.T) {
	h := newHarness(t)
	key := h.key("alice", false)
	cl := h.dial(key, "kosh")
	sess, err := cl.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	for _, r := range []struct {
		name    string
		payload []byte
	}{
		{"auth-agent-req@openssh.com", nil},
		{"subsystem", ssh.Marshal(struct{ Name string }{"sftp"})},
		{"x11-req", ssh.Marshal(struct {
			Single           bool
			Protocol, Cookie string
			Screen           uint32
		}{false, "MIT-MAGIC-COOKIE-1", "00", 0})},
		{"signal", ssh.Marshal(struct{ Name string }{"INT"})},
		{"made-up", nil},
		{"pty-req", bytes.Repeat([]byte{0}, 65<<10+1)}, // larger than 64 KiB
	} {
		ok, err := sess.SendRequest(r.name, true, r.payload)
		if err != nil {
			t.Fatalf("%s: %v", r.name, err)
		}
		if ok {
			t.Errorf("%s was accepted", r.name)
		}
	}
	tm := &term{t: t, sess: sess, stdout: &syncBuffer{}, stderr: &syncBuffer{}, done: make(chan error, 1)}
	sess.Stdout, sess.Stderr = tm.stdout, tm.stderr
	if tm.stdin, err = sess.StdinPipe(); err != nil {
		t.Fatal(err)
	}
	if err := sess.RequestPty("xterm-256color", 24, 80, ssh.TerminalModes{}); err != nil {
		t.Fatal(err)
	}
	if err := sess.Shell(); err != nil {
		t.Fatal(err)
	}
	go func() { tm.done <- sess.Wait() }()
	tm.waitFor(testWord)
	if err := sess.WindowChange(30, 100); err != nil {
		t.Errorf("window-change: %v", err)
	}
	tm.press("q")
	if st := tm.exit(); st != 0 {
		t.Errorf("exit status %d", st)
	}

	ex := openSession(t, cl, false, "ls")
	if st := ex.exit(); st != 1 || !strings.Contains(ex.stderr.String(), MsgNoCommands) {
		t.Errorf("exec: status %d, %q", st, ex.stderr.String())
	}
}

// kexInitPacket is a binary packet of a KEXINIT whose key exchange
// methods are a list of names that is n bytes long.
func kexInitPacket(n int) []byte {
	names := strings.Repeat("x", n)
	var payload bytes.Buffer
	payload.WriteByte(20) // SSH_MSG_KEXINIT
	payload.Write(make([]byte, 16))
	for i := 0; i < 10; i++ {
		list := "none"
		switch i {
		case 0:
			list = names
		case 1:
			list = "ssh-ed25519"
		case 2, 3:
			list = "aes128-ctr"
		case 4, 5:
			list = "hmac-sha2-256"
		case 8, 9:
			list = ""
		}
		_ = binary.Write(&payload, binary.BigEndian, uint32(len(list)))
		payload.WriteString(list)
	}
	payload.Write([]byte{0, 0, 0, 0, 0})
	padding := 8 - (payload.Len()+5)%8
	if padding < 4 {
		padding += 8
	}
	var pkt bytes.Buffer
	_ = binary.Write(&pkt, binary.BigEndian, uint32(1+payload.Len()+padding))
	pkt.WriteByte(byte(padding))
	pkt.Write(payload.Bytes())
	pkt.Write(make([]byte, padding))
	return pkt.Bytes()
}

func TestHandshakeErrorIsCut(t *testing.T) {
	h := newHarness(t)
	c := h.dialRaw()
	if _, err := io.WriteString(c, "SSH-2.0-x\r\n"); err != nil {
		t.Fatal(err)
	}
	readBanner(t, c)
	go func() { _, _ = c.Write(kexInitPacket(200 << 10)) }()
	waitClosed(t, c, "a peer whose KEXINIT has 200 KiB of names")
	rec := h.endLogged(c, slog.LevelInfo, "a peer whose KEXINIT has 200 KiB of names")
	reason := rec.Attrs["reason"]
	if len(reason) > maxReason+len("…") || !strings.HasSuffix(reason, "…") || !utf8.ValidString(reason) {
		t.Errorf("the reason has %d bytes: %.300q", len(reason), reason)
	}
}

func TestCutReason(t *testing.T) {
	if got := cutReason("EOF"); got != "EOF" {
		t.Errorf("a short reason: %q", got)
	}
	long := strings.Repeat("é", 200) // 400 bytes
	got := cutReason(long)
	if !utf8.ValidString(got) || !strings.HasSuffix(got, "…") || len(got) > maxReason+len("…") || !strings.HasPrefix(long, strings.TrimSuffix(got, "…")) {
		t.Errorf("cut to %q", got)
	}
}

func TestRemovedKeyLosesItsConnections(t *testing.T) {
	h := newHarness(t, withConfig(func(c *Config) { c.IdleConnection = time.Hour }))
	alice := h.key("alice", false)
	bob := h.key("bob", false)
	busy, idle := h.dial(alice, "kosh"), h.dial(alice, "kosh")
	other := h.dial(bob, "kosh")
	tm := openTerm(t, busy)
	tm.waitFor(testWord)
	eventually(t, "both of alice's connections to be counted", func() bool {
		h.srv.mu.Lock()
		defer h.srv.mu.Unlock()
		return h.srv.perKey[ssh.FingerprintSHA256(alice.PublicKey())] == 2
	})

	h.lines = h.lines[1:] // bob alone
	setKeys(t, h.list, h.lines...)
	if st := tm.exit(); st != 1 || !strings.Contains(tm.stdout.String(), MsgKeyRemoved) {
		t.Errorf("the session ended with %d and %q", st, tm.stdout.String())
	}
	for name, cl := range map[string]*ssh.Client{"busy": busy, "idle": idle} {
		select {
		case <-closed(cl):
		case <-timeAfterWait():
			t.Errorf("alice's %s connection is still open", name)
		}
	}
	select {
	case <-closed(other):
		t.Error("bob's connection was closed")
	case <-time.After(100 * time.Millisecond):
	}
	eventually(t, "alice's connections to be uncounted", func() bool {
		h.srv.mu.Lock()
		defer h.srv.mu.Unlock()
		_, ok := h.srv.perKey[ssh.FingerprintSHA256(alice.PublicKey())]
		return !ok
	})
	ot := openTerm(t, other)
	ot.waitFor(testWord)
}

// A key whose grant changed keeps its connection; its new session has the
// new grant.
func TestGrantChangedKeepsTheConnection(t *testing.T) {
	h := newHarness(t, withConfig(func(c *Config) { c.IdleConnection = time.Hour }))
	alice := h.key("alice", false)
	cl := h.dial(alice, "kosh")
	tm := openTerm(t, cl)
	tm.waitFor("grant:full")
	h.lines = []string{keyLine(alice, "alice", true)}
	setKeys(t, h.list, h.lines...)
	if st := tm.exit(); st != 1 {
		t.Errorf("exit status %d", st)
	}
	select {
	case <-closed(cl):
		t.Fatal("the connection was closed")
	case <-time.After(100 * time.Millisecond):
	}
	again := openTerm(t, cl)
	again.waitFor("grant:read-only")
}
