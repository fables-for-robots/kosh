package sshd

import (
	"context"
	"encoding/hex"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	irohkey "github.com/tmc/go-iroh/key"
	"golang.org/x/crypto/ssh"

	"github.com/fables-for-robots/kosh/irohnet"
)

// irohListener is the harness's server served on a local iroh endpoint
// as well, with transport "iroh".
type irohListener struct {
	l      *irohnet.Listener
	served chan error
}

func (h *harness) serveIroh() *irohListener {
	h.t.Helper()
	k, err := irohkey.GenerateSecretKey()
	if err != nil {
		h.t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), waitTime)
	defer cancel()
	l, err := irohnet.Listen(ctx, irohnet.Config{Key: k, Local: true, Log: h.srv.log})
	if err != nil {
		h.t.Fatalf("listening on iroh: %v", err)
	}
	il := &irohListener{l: l, served: make(chan error, 1)}
	go func() { il.served <- h.srv.Serve(l, "iroh") }()
	h.t.Cleanup(func() {
		_ = l.Close()
		select {
		case <-il.served:
		case <-timeAfterWait():
			h.t.Error("Serve of the iroh listener has not returned")
		}
	})
	return il
}

// dial opens a stream to the endpoint, closed after the test.
func (il *irohListener) dial(t *testing.T) net.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), waitTime)
	defer cancel()
	c, err := irohnet.DialLocal(ctx, il.l.ID(), il.l.LocalAddrs())
	if err != nil {
		t.Fatalf("dialing the endpoint: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func (il *irohListener) connect(t *testing.T, signer ssh.Signer) (*ssh.Client, error) {
	t.Helper()
	return connect(il.dial(t), "iroh", clientConfig("kosh", ssh.PublicKeys(signer)))
}

func TestIrohAcceptedKey(t *testing.T) {
	h := newHarness(t)
	key := h.key(admin("alice"))
	il := h.serveIroh()
	cl, err := il.connect(t, key)
	if err != nil {
		t.Fatalf("connecting over iroh: %v", err)
	}
	t.Cleanup(func() { _ = cl.Close() })
	tm := openTerm(t, cl)
	tm.waitFor(testWord)
	s := h.nextSession()
	if s.Transport != "iroh" {
		t.Errorf("Session.Transport = %q, want iroh", s.Transport)
	}
	if b, err := hex.DecodeString(s.Remote); err != nil || len(s.Remote) != 64 || len(b) != 32 {
		t.Errorf("Session.Remote = %q, want an endpoint ID of 64 hex characters", s.Remote)
	}
	tm.press("q")
	if st := tm.exit(); st != 0 {
		t.Errorf("exit status %d after q", st)
	}

	// Serve returns nil when the iroh listener is closed.
	_ = il.l.Close()
	select {
	case err := <-il.served:
		if err != nil {
			t.Errorf("Serve of a closed iroh listener returned %v, want nil", err)
		}
		il.served <- err
	case <-timeAfterWait():
		t.Fatal("Serve has not returned after the iroh listener was closed")
	}
}

func TestIrohRejectedKey(t *testing.T) {
	h := newHarness(t)
	h.key(admin("alice"))
	il := h.serveIroh()
	if cl, err := il.connect(t, newSigner(t)); err == nil {
		_ = cl.Close()
		t.Fatal("a key that is not in the list got in over iroh")
	}
	h.log.waitFor(t, "of the refused connection", func(r logRecord) bool {
		return r.Msg == "connection closed before authenticating" && r.Attrs["transport"] == "iroh"
	})
}

func TestIrohSilentPeer(t *testing.T) {
	h := newHarness(t, withConfig(func(c *Config) { c.HandshakeTimeout = 200 * time.Millisecond }))
	il := h.serveIroh()
	c := il.dial(t)
	// One byte that is not SSH, which makes the stream visible, and then
	// nothing.
	if _, err := c.Write([]byte{0}); err != nil {
		t.Fatal(err)
	}
	_ = c.SetReadDeadline(time.Now().Add(waitTime))
	if _, err := io.Copy(io.Discard, c); isTimeout(err) {
		t.Fatal("the server has not closed a silent peer over iroh")
	}
	rec := h.log.waitFor(t, "of the silent peer", func(r logRecord) bool { return r.Attrs["transport"] == "iroh" })
	if rec.Attrs["reason"] != reasonHandshakeDeadline {
		t.Errorf("the silent peer over iroh is logged as %v", rec)
	}
}

func isTimeout(err error) bool {
	ne, ok := err.(net.Error)
	return ok && ne.Timeout()
}

// The probe found that a client of iroh-ssh often never closes its
// connection: the server does, after IdleConnection.
func TestIrohSessionEndsTheConnection(t *testing.T) {
	const idle = 200 * time.Millisecond
	h := newHarness(t, withConfig(func(c *Config) { c.IdleConnection = idle }))
	key := h.key(admin("alice"))
	il := h.serveIroh()
	cl, err := il.connect(t, key)
	if err != nil {
		t.Fatalf("connecting over iroh: %v", err)
	}
	t.Cleanup(func() { _ = cl.Close() })
	tm := openTerm(t, cl)
	tm.waitFor(testWord)
	tm.press("q")
	if st := tm.exit(); st != 0 {
		t.Errorf("exit status %d after q", st)
	}
	ended := time.Now()
	if d := closedByTheServer(t, cl, "an iroh connection whose session ended").Sub(ended); d < idle/2 {
		t.Errorf("the connection was closed %v after its session ended, want about %v", d, idle)
	}
}

// A stop tells a session over iroh why it ends before it closes what
// carries it, as it does over TCP.
func TestShutdownTellsSessionsOverIroh(t *testing.T) {
	for _, transport := range []string{"iroh", "tcp"} {
		t.Run(transport, func(t *testing.T) {
			h := newHarness(t)
			key := h.key(admin("alice"))
			var (
				cl    *ssh.Client
				il    *irohListener
				err   error
				srvDn = h.served
			)
			if transport == "iroh" {
				il = h.serveIroh()
				if cl, err = il.connect(t, key); err != nil {
					t.Fatalf("connecting over iroh: %v", err)
				}
				t.Cleanup(func() { _ = cl.Close() })
			} else {
				cl = h.dial(key, "kosh")
			}
			tm := openTerm(t, cl)
			tm.waitFor(testWord)

			ctx, cancel := context.WithTimeout(context.Background(), waitTime)
			defer cancel()
			if err := h.srv.Shutdown(ctx); err != nil {
				t.Fatalf("Shutdown: %v", err)
			}
			if st := tm.exit(); st != 1 {
				t.Errorf("exit status %d, want 1", st)
			}
			if out := tm.stdout.String(); !strings.HasSuffix(out, MsgShuttingDown+"\r\n") {
				t.Errorf("the session did not end with %q: %q", MsgShuttingDown, out)
			}
			rec := h.log.waitFor(t, "of the session's end", func(r logRecord) bool { return r.Msg == "session end" })
			if rec.Attrs["reason"] != MsgShuttingDown {
				t.Errorf("the session's end is logged as %v", rec)
			}
			if il != nil {
				select {
				case err := <-il.served:
					if err != nil {
						t.Errorf("Serve of the iroh listener: %v", err)
					}
					il.served <- err
				case <-timeAfterWait():
					t.Fatal("Serve of the iroh listener has not returned")
				}
			}
			select {
			case <-srvDn:
				if h.serveErr != nil {
					t.Errorf("Serve of the TCP listener: %v", h.serveErr)
				}
			case <-timeAfterWait():
				t.Fatal("Serve of the TCP listener has not returned")
			}
		})
	}
}
