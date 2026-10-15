package sshd

import (
	"errors"
	"log/slog"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"

	"github.com/fables-for-robots/kosh/keys"
)

func TestNewValidates(t *testing.T) {
	good := Config{
		HostKeyPEM: hostKeyPEM(t),
		Keys:       keys.NewList(nil),
		Model:      func(Session) tea.Model { return testModel{} },
	}
	s, err := New(good)
	if err != nil {
		t.Fatalf("a good config: %v", err)
	}
	if c := s.cfg; c.HandshakeTimeout != 30*time.Second || c.MaxHandshakes != 64 || c.KeepaliveInterval != 30*time.Second ||
		c.KeepaliveMisses != 3 || c.IdleConnection != 5*time.Second || c.Log != slog.Default() {
		t.Errorf("zero limits are not the defaults: %+v", c)
	}
	for name, change := range map[string]func(*Config){
		"a host key that does not parse": func(c *Config) {
			c.HostKeyPEM = []byte("-----BEGIN OPENSSH PRIVATE KEY-----\nnot a key\n-----END OPENSSH PRIVATE KEY-----\n")
		},
		"no host key": func(c *Config) { c.HostKeyPEM = nil },
		"no key list": func(c *Config) { c.Keys = nil },
		"no model":    func(c *Config) { c.Model = nil },
	} {
		cfg := good
		change(&cfg)
		if _, err := New(cfg); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestAcceptedKeyGetsTheProgram(t *testing.T) {
	h := newHarness(t)
	key := h.key(admin("alice"))
	tm := openTerm(t, h.dial(key, "kosh"))
	tm.waitFor(testWord)
	if n := h.srv.Sessions(); n != 1 {
		t.Errorf("Sessions() = %d with one program running, want 1", n)
	}
	rec := h.log.waitFor(t, "of the start of the session", func(r logRecord) bool { return r.Msg == "session start" })
	if rec.Attrs["key"] != admin("alice") || rec.Attrs["fingerprint"] != ssh.FingerprintSHA256(key.PublicKey()) ||
		rec.Attrs["role"] != "ClusterRole/cluster-admin" || rec.Attrs["transport"] != "tcp" || rec.Attrs["remote"] == "" {
		t.Errorf("the start of the session is logged as %v", rec)
	}
	tm.press("q")
	if st := tm.exit(); st != 0 {
		t.Errorf("exit status %d after q, want 0", st)
	}
	eventually(t, "Sessions() to be 0 after the program ended", func() bool { return h.srv.Sessions() == 0 })
	// The program ended by itself: the end has no reason.
	if rec := h.log.waitFor(t, "of the end of the session", func(r logRecord) bool { return r.Msg == "session end" }); rec.Attrs["reason"] != "" {
		t.Errorf("a session that ended by itself is logged with a reason: %v", rec)
	}
}

func TestRejectedKey(t *testing.T) {
	h := newHarness(t)
	h.key(admin("alice"))
	stranger := newSigner(t)
	raw := h.dialRaw()
	if _, err := connect(raw, h.addr, clientConfig("kosh", ssh.PublicKeys(stranger))); err == nil {
		t.Fatal("a key that is not in the list got in")
	}
	remote := raw.LocalAddr().String()
	rec := h.log.waitFor(t, "for the refused connection", func(r logRecord) bool { return r.Attrs["remote"] == remote })
	if rec.Attrs["reason"] == "" || rec.Attrs["transport"] != "tcp" {
		t.Errorf("the record of the refused connection lacks a reason or the transport: %v", rec)
	}
	if h.models.Load() != 0 {
		t.Error("a model was made")
	}
}

func TestNoPasswordAuth(t *testing.T) {
	h := newHarness(t)
	h.key(admin("alice"))
	var passwords, interactive, publicKeys atomic.Int32
	cfg := clientConfig("kosh",
		ssh.PasswordCallback(func() (string, error) {
			passwords.Add(1)
			return "secret", nil
		}),
		ssh.KeyboardInteractive(func(string, string, []string, []bool) ([]string, error) {
			interactive.Add(1)
			return nil, nil
		}),
		ssh.PublicKeysCallback(func() ([]ssh.Signer, error) {
			publicKeys.Add(1)
			return []ssh.Signer{newSigner(t)}, nil
		}),
	)
	// The client tries only what the server offers.
	if _, err := connect(h.dialRaw(), h.addr, cfg); err == nil {
		t.Fatal("got in without an accepted key")
	}
	if passwords.Load() != 0 || interactive.Load() != 0 {
		t.Errorf("the server offered a password (%d) or keyboard-interactive (%d)", passwords.Load(), interactive.Load())
	}
	if publicKeys.Load() == 0 {
		t.Error("the server did not offer publickey")
	}
}

func TestAnyUserName(t *testing.T) {
	h := newHarness(t)
	key := h.key(admin("alice"))
	for _, user := range []string{"root", "kosh", "x"} {
		tm := openTerm(t, h.dial(key, user))
		tm.waitFor(testWord)
		tm.press("q")
		if st := tm.exit(); st != 0 {
			t.Errorf("user %s: exit status %d", user, st)
		}
	}
}

func TestVersionString(t *testing.T) {
	h := newHarness(t)
	cl := h.dial(h.key(admin("alice")), "kosh")
	if got, want := string(cl.ServerVersion()), "SSH-2.0-kosh_test-1.2.3"; got != want {
		t.Errorf("server version %q, want %q", got, want)
	}
}

func TestTwentyAuthenticationTries(t *testing.T) {
	h := newHarness(t)
	key := h.key(admin("alice"))
	strangers := func(n int) []ssh.Signer {
		var s []ssh.Signer
		for range n {
			s = append(s, newSigner(t))
		}
		return s
	}

	cl, err := connect(h.dialRaw(), h.addr, clientConfig("kosh", ssh.PublicKeys(append(strangers(15), key)...)))
	if err != nil {
		t.Fatalf("15 keys that are not accepted, then the accepted one: %v", err)
	}
	_ = cl.Close()

	// It is a limit: after 20 that fail there is no 21st.
	if _, err := connect(h.dialRaw(), h.addr, clientConfig("kosh", ssh.PublicKeys(append(strangers(20), key)...))); err == nil {
		t.Error("20 keys that are not accepted, then the accepted one: got in")
	}
}

func TestServeTwoListeners(t *testing.T) {
	h := newHarness(t)
	key := h.key(admin("alice"))
	la, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	lb, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	served := map[string]chan error{"a": make(chan error, 1), "b": make(chan error, 1)}
	go func() { served["a"] <- h.srv.Serve(la, "a") }()
	go func() { served["b"] <- h.srv.Serve(lb, "b") }()

	session := func(addr string) Session {
		t.Helper()
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		cl, err := connect(c, addr, clientConfig("kosh", ssh.PublicKeys(key)))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = cl.Close() })
		tm := openTerm(t, cl)
		tm.waitFor(testWord)
		s := h.nextSession()
		tm.press("q")
		tm.exit()
		return s
	}
	if s := session(la.Addr().String()); s.Transport != "a" {
		t.Errorf("a session from listener a has transport %q", s.Transport)
	}
	if s := session(lb.Addr().String()); s.Transport != "b" {
		t.Errorf("a session from listener b has transport %q", s.Transport)
	}

	_ = la.Close()
	select {
	case err := <-served["a"]:
		if err != nil {
			t.Errorf("Serve of a closed listener returned %v, want nil", err)
		}
	case <-timeAfterWait():
		t.Fatal("Serve has not returned after its listener was closed")
	}
	if s := session(lb.Addr().String()); s.Transport != "b" {
		t.Errorf("after a was closed, a session from listener b has transport %q", s.Transport)
	}
}

func TestNoTerminal(t *testing.T) {
	h := newHarness(t)
	tm := openSession(t, h.dial(h.key(admin("alice")), "kosh"), false, "")
	if st := tm.exit(); st != 1 {
		t.Errorf("exit status %d, want 1", st)
	}
	if got := tm.stderr.String(); got != MsgNeedsTerminal+"\n" {
		t.Errorf("error stream %q, want %q", got, MsgNeedsTerminal+"\n")
	}
	h.refusalLogged(MsgNeedsTerminal, admin("alice"))
	if h.models.Load() != 0 {
		t.Error("a model was made")
	}
}

func TestCommandIsRefused(t *testing.T) {
	h := newHarness(t)
	cl := h.dial(h.key(admin("alice")), "kosh")
	defer h.refusalLogged(MsgNoCommands, admin("alice"))
	for _, pty := range []bool{false, true} {
		tm := openSession(t, cl, pty, "ls")
		if st := tm.exit(); st != 1 {
			t.Errorf("terminal %v: exit status %d, want 1", pty, st)
		}
		if got := tm.stderr.String(); !strings.HasPrefix(got, MsgNoCommands+"\r\n") && got != MsgNoCommands+"\n" {
			t.Errorf("terminal %v: error stream %q, want %q and a line feed", pty, got, MsgNoCommands)
		}
	}
	if h.models.Load() != 0 {
		t.Error("a model was made")
	}
}

func TestNothingElseIsServed(t *testing.T) {
	h := newHarness(t)
	cl := h.dial(h.key(admin("alice")), "kosh")
	agentChannels := cl.HandleChannelOpen("auth-agent@openssh.com")

	var oce *ssh.OpenChannelError
	if _, err := cl.Dial("tcp", "127.0.0.1:9"); !errors.As(err, &oce) {
		t.Errorf("direct-tcpip: %v, want the channel rejected", err)
	}

	ok, _, err := cl.SendRequest("tcpip-forward", true, ssh.Marshal(struct {
		Addr string
		Port uint32
	}{"127.0.0.1", 0}))
	if err != nil || ok {
		t.Errorf("tcpip-forward: ok %v, err %v; want refused", ok, err)
	}

	sub, err := cl.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	if err := sub.RequestSubsystem("sftp"); err == nil {
		t.Error("the subsystem sftp was started")
	}
	_ = sub.Close()

	sess, err := cl.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	if err := sess.Setenv("LANG", "C.UTF-8"); err != nil {
		t.Errorf("an environment request: %v, want accepted", err)
	}
	if err := agent.RequestAgentForwarding(sess); err != nil {
		t.Logf("agent forwarding: %v", err)
	}
	out := &syncBuffer{}
	sess.Stdout = out
	stdin, err := sess.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.RequestPty("xterm-256color", 24, 80, ssh.TerminalModes{}); err != nil {
		t.Fatal(err)
	}
	if err := sess.Shell(); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the program, after an environment request", func() bool { return strings.Contains(out.String(), testWord) })
	if _, err := stdin.Write([]byte("q")); err != nil {
		t.Fatal(err)
	}
	if err := sess.Wait(); err != nil {
		t.Errorf("the session: %v", err)
	}
	select {
	case ch := <-agentChannels:
		_ = ch.Reject(ssh.Prohibited, "test")
		t.Error("the server opened an agent channel")
	default:
	}
}

// refusalLogged checks that a session refused with reason is logged with
// the key's name and fingerprint.
func (h *harness) refusalLogged(reason, name string) {
	h.t.Helper()
	rec := h.log.waitFor(h.t, "of the refused session", func(r logRecord) bool {
		return r.Msg == "session refused" && r.Attrs["reason"] == reason
	})
	if rec.Attrs["key"] != name || !strings.HasPrefix(rec.Attrs["fingerprint"], "SHA256:") ||
		rec.Attrs["transport"] != "tcp" || rec.Attrs["remote"] == "" {
		h.t.Errorf("the refused session is logged as %v", rec)
	}
}
