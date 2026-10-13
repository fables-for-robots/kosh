package irohnet

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tmc/go-iroh/iroh"
	irohkey "github.com/tmc/go-iroh/key"
	"github.com/tmc/go-iroh/netaddr"
	"github.com/tmc/go-iroh/relay"
)

// wait is how long a test waits for something that should happen at
// once. It is generous, for -race on a busy machine; a test that passes
// waits only as long as the thing takes.
const wait = 10 * time.Second

func newKey(t *testing.T) irohkey.SecretKey {
	t.Helper()
	k, err := irohkey.GenerateSecretKey()
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// syncBuffer is a log sink the listener's goroutines and the test share.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// testListener binds a local listener that waits as tm says, and closes
// it when the test ends. Its log goes to the returned buffer, at debug
// level.
func testListener(t *testing.T, tm timeouts) (*Listener, *syncBuffer) {
	t.Helper()
	logs := &syncBuffer{}
	log := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	l, err := listen(ctx, Config{Key: newKey(t), Local: true, Log: log}, tm)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { l.Close() })
	return l, logs
}

// dial connects to l with DialLocal and closes the connection when the
// test ends.
func dial(t *testing.T, l *Listener) net.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	c, err := DialLocal(ctx, l.ID(), l.LocalAddrs())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

type acceptResult struct {
	conn net.Conn
	err  error
}

// acceptAsync calls l.Accept in a goroutine and returns where its result
// arrives. A call still waiting when the test ends returns when the
// listener is closed, and a connection it returned that the test did not
// take is closed by the listener's Close, as every connection it handed
// out.
func acceptAsync(t *testing.T, l *Listener) <-chan acceptResult {
	t.Helper()
	ch := make(chan acceptResult, 1)
	go func() {
		c, err := l.Accept()
		ch <- acceptResult{c, err}
	}()
	return ch
}

// awaitAccept waits for the result of acceptAsync and fails the test when
// it is an error or does not come. The connection is closed when the test
// ends.
func awaitAccept(t *testing.T, ch <-chan acceptResult) net.Conn {
	t.Helper()
	select {
	case r := <-ch:
		if r.err != nil {
			t.Fatalf("accept: %v", r.err)
		}
		t.Cleanup(func() { r.conn.Close() })
		return r.conn
	case <-time.After(wait):
		t.Fatal("accept did not return")
		return nil
	}
}

// accept writes msg on c, and returns the listener's connection for it
// once it has read msg.
func accept(t *testing.T, l *Listener, c net.Conn, msg string) net.Conn {
	t.Helper()
	ch := acceptAsync(t, l)
	if _, err := c.Write([]byte(msg)); err != nil {
		t.Fatalf("client write: %v", err)
	}
	s := awaitAccept(t, ch)
	readExactly(t, s, msg)
	return s
}

// readExactly reads len(want) bytes from c, with a deadline, and fails the
// test unless they are want.
func readExactly(t *testing.T, c net.Conn, want string) {
	t.Helper()
	c.SetReadDeadline(time.Now().Add(wait))
	defer c.SetReadDeadline(time.Time{})
	got := make([]byte, len(want))
	if _, err := io.ReadFull(c, got); err != nil {
		t.Fatalf("read: %v (got %q)", err, got)
	}
	if string(got) != want {
		t.Fatalf("read %q, want %q", got, want)
	}
}

// readEnds reads from c until it fails, and fails the test unless that
// happens within wait.
func readEnds(t *testing.T, c net.Conn) error {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		buf := make([]byte, 64)
		for {
			if _, err := c.Read(buf); err != nil {
				done <- err
				return
			}
		}
	}()
	select {
	case err := <-done:
		return err
	case <-time.After(wait):
		t.Fatal("read did not end")
		return nil
	}
}

func isHexID(s string) bool {
	b, err := hex.DecodeString(s)
	return err == nil && len(b) == 32 && len(s) == 64
}

func TestListenLocal(t *testing.T) {
	key := newKey(t)
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	l, err := Listen(ctx, Config{Key: key, Local: true, Log: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer l.Close()

	want := key.Public().EndpointID().String()
	if !isHexID(l.ID()) || l.ID() != want {
		t.Errorf("ID() = %q, want %q", l.ID(), want)
	}
	found := false
	for _, ap := range l.LocalAddrs() {
		if ap.Addr() == netip.MustParseAddr("127.0.0.1") && ap.Port() != 0 {
			found = true
		}
	}
	if !found {
		t.Errorf("LocalAddrs() = %v, want an address on 127.0.0.1 with a port", l.LocalAddrs())
	}
	if a := l.Addr(); a.Network() != "iroh" || a.String() != want {
		t.Errorf("Addr() = %s %q, want iroh %q", a.Network(), a.String(), want)
	}
}

func TestListenNeedsAWayToBeFound(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	l, err := Listen(ctx, Config{Key: newKey(t)})
	if err == nil {
		l.Close()
		t.Fatal("Listen without Local, Relay and MDNS succeeded")
	}
}

func TestAcceptHandsOutTheStream(t *testing.T) {
	l, _ := testListener(t, defaultTimeouts)
	c := dial(t, l)
	s := accept(t, l, c, "hello")

	// Both directions at once: each side writes a block larger than
	// a stream's first window while the other writes too.
	big := strings.Repeat("kosh", 1<<18)
	var wg sync.WaitGroup
	for _, w := range []net.Conn{c, s} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := io.WriteString(w, big); err != nil {
				t.Errorf("write: %v", err)
			}
		}()
	}
	readExactly(t, s, big)
	readExactly(t, c, big)
	wg.Wait()

	if _, err := s.Write([]byte("bye")); err != nil {
		t.Fatalf("server write: %v", err)
	}
	readExactly(t, c, "bye")
}

func TestRemoteAddrIsTheClientsID(t *testing.T) {
	l, _ := testListener(t, defaultTimeouts)
	c := dial(t, l)
	s := accept(t, l, c, "hi")

	ra := s.RemoteAddr()
	if ra.Network() != "iroh" || !isHexID(ra.String()) {
		t.Fatalf("RemoteAddr() = %s %q, want iroh and 64 hex characters", ra.Network(), ra.String())
	}
	if ra.String() == l.ID() {
		t.Fatalf("RemoteAddr() is the listener's own ID")
	}
	if ra.String() != c.LocalAddr().String() {
		t.Fatalf("RemoteAddr() = %q, the client is %q", ra.String(), c.LocalAddr().String())
	}
	if la := s.LocalAddr(); la.Network() != "iroh" || la.String() != l.ID() {
		t.Fatalf("LocalAddr() = %s %q, want iroh %q", la.Network(), la.String(), l.ID())
	}
}

func TestStreamIsSeenOnlyAfterAWrite(t *testing.T) {
	l, _ := testListener(t, defaultTimeouts)
	c := dial(t, l)
	ch := acceptAsync(t, l)
	select {
	case r := <-ch:
		t.Fatalf("Accept returned before the client wrote: %v %v", r.conn, r.err)
	case <-time.After(200 * time.Millisecond):
	}
	if _, err := c.Write([]byte("now")); err != nil {
		t.Fatal(err)
	}
	s := awaitAccept(t, ch)
	readExactly(t, s, "now")
}

func TestCloseOfAConnClosesTheIrohConnection(t *testing.T) {
	l, _ := testListener(t, timeouts{stream: StreamTimeout, linger: 100 * time.Millisecond})
	c := dial(t, l)
	s := accept(t, l, c, "hello")
	if _, err := s.Write([]byte("last words")); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	readExactly(t, c, "last words")
	readEnds(t, c)

	deadline := time.Now().Add(wait)
	for {
		c.SetWriteDeadline(time.Now().Add(wait))
		if _, err := c.Write([]byte("anyone?")); err != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the client can still write after the server closed")
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case <-c.(*conn).qc.Context().Done():
	case <-time.After(wait):
		t.Fatal("the client's iroh connection is still open after the server closed")
	}
}

// A server that writes its last bytes and closes at once: Close returns
// at once, the client still reads the bytes, and a client that never
// closes finds the connection closed about the grace later.
func TestCloseOfAConnGivesThePeerAMoment(t *testing.T) {
	const grace = 200 * time.Millisecond
	l, _ := testListener(t, timeouts{stream: StreamTimeout, linger: grace})
	c := dial(t, l)
	s := accept(t, l, c, "hello")

	// The server writes more than the stream's window holds and closes
	// right after the last Write, while the client is still reading.
	big := strings.Repeat("kosh", 1<<18)
	type result struct {
		start time.Time
		took  time.Duration
		err   error
	}
	closed := make(chan result, 1)
	go func() {
		if _, err := io.WriteString(s, big); err != nil {
			closed <- result{err: err}
			return
		}
		start := time.Now()
		err := s.Close()
		closed <- result{start, time.Since(start), err}
	}()
	readExactly(t, c, big)
	readEnds(t, c)
	var r result
	select {
	case r = <-closed:
	case <-time.After(wait):
		t.Fatal("the server's Write and Close did not return")
	}
	if r.err != nil {
		t.Fatalf("write or close: %v", r.err)
	}
	if r.took >= grace/2 {
		t.Fatalf("Close took %v; it should not wait for the peer", r.took)
	}
	start := r.start
	select {
	case <-c.(*conn).qc.Context().Done():
		if d := time.Since(start); d < grace*3/4 {
			t.Fatalf("the connection was closed %v after Close, before the grace of %v", d, grace)
		}
	case <-time.After(wait):
		t.Fatal("the client's iroh connection is still open after the grace")
	}
}

func TestCloseUnblocksRead(t *testing.T) {
	l, _ := testListener(t, defaultTimeouts)
	c := dial(t, l)
	s := accept(t, l, c, "hello")

	done := make(chan error, 1)
	go func() {
		_, err := s.Read(make([]byte, 8))
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("Read returned before Close: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	s.Close()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Read after Close returned no error")
		}
	case <-time.After(wait):
		t.Fatal("Read did not return after Close")
	}
	s.Close()
}

func TestCloseUnblocksWrite(t *testing.T) {
	l, _ := testListener(t, timeouts{stream: StreamTimeout, linger: time.Minute})
	c := dial(t, l)
	s := accept(t, l, c, "hello")

	// The client never reads: the server's Write blocks once the flow
	// control window is full.
	written := make(chan error, 1)
	go func() {
		_, err := s.Write(make([]byte, 64<<20))
		written <- err
	}()
	select {
	case err := <-written:
		t.Fatalf("a Write of 64 MiB to a client that does not read returned: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	start := time.Now()
	s.Close()
	select {
	case err := <-written:
		if err == nil {
			t.Fatal("the blocked Write returned no error after Close")
		}
		if d := time.Since(start); d > time.Second {
			t.Fatalf("the blocked Write returned %v after Close", d)
		}
	case <-time.After(wait):
		t.Fatal("the blocked Write did not return after Close")
	}
	if _, err := s.Write([]byte("more")); err == nil {
		t.Fatal("a Write after Close succeeded")
	}
}

// The SSH library sets the deadline of the connection at every Read and
// Write, clearing it when it has no idle timeout: that must not undo the
// deadline Close sets to wake a blocked Write.
func TestCloseUnblocksWriteWhileDeadlinesAreCleared(t *testing.T) {
	l, _ := testListener(t, timeouts{stream: StreamTimeout, linger: time.Minute})
	c := dial(t, l)
	s := accept(t, l, c, "hello")

	written := make(chan error, 1)
	go func() {
		_, err := s.Write(make([]byte, 64<<20))
		written <- err
	}()
	select {
	case err := <-written:
		t.Fatalf("a Write of 64 MiB to a client that does not read returned: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	stop := make(chan struct{})
	clearing := make(chan struct{})
	go func() {
		defer close(clearing)
		for {
			select {
			case <-stop:
				return
			default:
			}
			s.SetDeadline(time.Time{})
			s.SetWriteDeadline(time.Time{})
		}
	}()
	defer func() {
		close(stop)
		<-clearing
	}()

	closed := make(chan error, 1)
	go func() { closed <- s.Close() }()
	for _, what := range []string{"Close", "the blocked Write"} {
		ch := closed
		if what == "the blocked Write" {
			ch = written
		}
		select {
		case err := <-ch:
			if what == "the blocked Write" && err == nil {
				t.Fatal("the blocked Write returned no error after Close")
			}
		case <-time.After(wait):
			t.Fatalf("%s did not return while deadlines were being cleared", what)
		}
	}
	if err := s.SetDeadline(time.Time{}); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("SetDeadline after Close: %v, want net.ErrClosed", err)
	}
}

func TestDeadlines(t *testing.T) {
	l, _ := testListener(t, defaultTimeouts)
	c := dial(t, l)
	s := accept(t, l, c, "hello")

	s.SetReadDeadline(time.Now().Add(-time.Second))
	_, err := s.Read(make([]byte, 8))
	var ne net.Error
	if !errors.Is(err, os.ErrDeadlineExceeded) && !(errors.As(err, &ne) && ne.Timeout()) {
		t.Fatalf("Read past the deadline: %v, want a timeout", err)
	}

	s.SetReadDeadline(time.Now().Add(wait))
	if _, err := c.Write([]byte("again")); err != nil {
		t.Fatal(err)
	}
	readExactly(t, s, "again")

	s.SetDeadline(time.Now().Add(-time.Second))
	if _, err := s.Read(make([]byte, 8)); !errors.Is(err, os.ErrDeadlineExceeded) && !(errors.As(err, &ne) && ne.Timeout()) {
		t.Fatalf("Read past SetDeadline: %v, want a timeout", err)
	}
	s.SetWriteDeadline(time.Now().Add(-time.Second))
	if _, err := s.Write([]byte("late")); !errors.Is(err, os.ErrDeadlineExceeded) && !(errors.As(err, &ne) && ne.Timeout()) {
		t.Fatalf("Write past the deadline: %v, want a timeout", err)
	}
}

func TestClientClosing(t *testing.T) {
	l, _ := testListener(t, defaultTimeouts)
	c := dial(t, l)
	s := accept(t, l, c, "hello")
	c.Close()
	readEnds(t, s)
}

func TestSecondStreamIsIgnored(t *testing.T) {
	l, _ := testListener(t, defaultTimeouts)

	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	ep, err := iroh.Bind(ctx,
		iroh.WithSecretKey(newKey(t)),
		iroh.WithRelayMode(relay.ModeDisabled()),
		iroh.WithBindAddr(netip.MustParseAddrPort("127.0.0.1:0")))
	if err != nil {
		t.Fatal(err)
	}
	defer ep.Shutdown(context.Background())
	id, err := irohkey.ParseEndpointID(l.ID())
	if err != nil {
		t.Fatal(err)
	}
	qc, err := ep.Connect(ctx, netaddr.NewEndpointAddr(id, netaddr.IPAddr{Addr: l.LocalAddrs()[0]}), ALPN)
	if err != nil {
		t.Fatal(err)
	}
	defer qc.Close()
	first, err := qc.OpenStreamConn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	s := accept(t, l, first, "one")

	// The listener allows one stream at a time: a second cannot be
	// opened while the first is open.
	openCtx, stopOpen := context.WithTimeout(ctx, 200*time.Millisecond)
	defer stopOpen()
	if second, err := qc.OpenStreamConn(openCtx); err == nil {
		second.Write([]byte("two"))
		t.Fatal("a second stream was opened beside the first")
	}
	ch := acceptAsync(t, l)
	select {
	case r := <-ch:
		t.Fatalf("Accept returned a second connection: %v %v", r.conn, r.err)
	case <-time.After(200 * time.Millisecond):
	}

	if _, err := s.Write([]byte("still here")); err != nil {
		t.Fatal(err)
	}
	readExactly(t, first, "still here")
	if _, err := first.Write([]byte("me too")); err != nil {
		t.Fatal(err)
	}
	readExactly(t, s, "me too")
}

func TestManyClients(t *testing.T) {
	l, _ := testListener(t, defaultTimeouts)
	go func() {
		for {
			s, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer s.Close()
				io.Copy(s, s)
			}()
		}
	}()

	const n = 20
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), wait)
			defer cancel()
			c, err := DialLocal(ctx, l.ID(), l.LocalAddrs())
			if err != nil {
				t.Errorf("client %d: dial: %v", i, err)
				return
			}
			defer c.Close()
			msg := fmt.Sprintf("client %d says hello", i)
			if _, err := io.WriteString(c, msg); err != nil {
				t.Errorf("client %d: write: %v", i, err)
				return
			}
			c.SetReadDeadline(time.Now().Add(wait))
			got := make([]byte, len(msg))
			if _, err := io.ReadFull(c, got); err != nil || string(got) != msg {
				t.Errorf("client %d: read %q, %v; want %q", i, got, err, msg)
			}
		}()
	}
	wg.Wait()
}

func TestServesAsANetListener(t *testing.T) {
	l, _ := testListener(t, defaultTimeouts)
	served := make(chan error, 1)
	go func() {
		served <- http.Serve(l, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, "hello %s from %s", r.RemoteAddr, r.Host)
		}))
	}()

	c := dial(t, l)
	if _, err := io.WriteString(c, "GET / HTTP/1.1\r\nHost: kosh\r\nConnection: close\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	c.SetReadDeadline(time.Now().Add(wait))
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if want := "hello " + c.LocalAddr().String() + " from kosh"; string(body) != want {
		t.Fatalf("body %q, want %q", body, want)
	}

	l.Close()
	select {
	case err := <-served:
		if !errors.Is(err, net.ErrClosed) {
			t.Fatalf("Serve returned %v, want net.ErrClosed", err)
		}
	case <-time.After(wait):
		t.Fatal("Serve did not return after Close")
	}
}

// waitFor polls cond until it holds, and fails the test when it does not
// within wait.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(wait)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestConnectionWithoutAStreamIsClosed(t *testing.T) {
	l, logs := testListener(t, timeouts{stream: 100 * time.Millisecond, linger: closeLinger})
	ch := acceptAsync(t, l)

	silent := dial(t, l)
	start := time.Now()
	readEnds(t, silent)
	if d := time.Since(start); d > wait/2 {
		t.Errorf("the silent client was closed after %v", d)
	}
	select {
	case r := <-ch:
		t.Fatalf("Accept returned the silent client: %v %v", r.conn, r.err)
	default:
	}
	id := silent.LocalAddr().String()
	waitFor(t, "the log to name the silent client", func() bool {
		return strings.Contains(logs.String(), "level=DEBUG") && strings.Contains(logs.String(), id)
	})

	c := dial(t, l)
	if _, err := c.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	s := awaitAccept(t, ch)
	readExactly(t, s, "hello")
	if s.RemoteAddr().String() != c.LocalAddr().String() {
		t.Fatalf("accepted %s, want the second client %s", s.RemoteAddr(), c.LocalAddr())
	}
}

func TestAcceptAfterClose(t *testing.T) {
	l, _ := testListener(t, defaultTimeouts)
	ch := acceptAsync(t, l)
	select {
	case r := <-ch:
		t.Fatalf("Accept returned before Close: %v %v", r.conn, r.err)
	case <-time.After(50 * time.Millisecond):
	}
	l.Close()
	select {
	case r := <-ch:
		if !errors.Is(r.err, net.ErrClosed) {
			t.Fatalf("Accept after Close: %v, want net.ErrClosed", r.err)
		}
	case <-time.After(wait):
		t.Fatal("Accept did not return after Close")
	}
	if _, err := l.Accept(); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("a later Accept: %v, want net.ErrClosed", err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("a second Close: %v", err)
	}
}

func TestCloseClosesEverything(t *testing.T) {
	before := runtime.NumGoroutine()
	l, _ := testListener(t, defaultTimeouts)

	// One accepted, one accepted and closed by the server whose client
	// does not close, one that has not written, one whose stream is open
	// and that nobody accepted.
	accepted := dial(t, l)
	s := accept(t, l, accepted, "hello")
	lingering := dial(t, l)
	accept(t, l, lingering, "hello").Close()
	waiting := dial(t, l)
	unaccepted := dial(t, l)
	if _, err := unaccepted.Write([]byte("anyone?")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the unaccepted stream", func() bool {
		l.mu.Lock()
		defer l.mu.Unlock()
		return len(l.conns) == 2
	})

	start := time.Now()
	if err := l.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	// The connection the server closed is given the rest of its linger,
	// and its client does not close it: Close waits for the linger, and
	// not longer.
	if d := time.Since(start); d > closeLinger+closeLinger/2 {
		t.Fatalf("Close took %v: longer than the linger of a closed connection", d)
	}
	if _, err := s.Read(make([]byte, 8)); err == nil {
		t.Error("the accepted connection reads after Close")
	}
	if _, err := s.Write([]byte("x")); err == nil {
		t.Error("the accepted connection writes after Close")
	}
	readEnds(t, accepted)
	readEnds(t, waiting)
	readEnds(t, unaccepted)
	readEnds(t, lingering)

	for _, c := range []net.Conn{accepted, lingering, waiting, unaccepted} {
		c.Close()
	}
	waitFor(t, "the listener's goroutines to end", func() bool {
		return runtime.NumGoroutine() <= before
	})
}

func TestWaitingConnectionsAreBounded(t *testing.T) {
	const timeout = 1500 * time.Millisecond
	l, _ := testListener(t, timeouts{stream: timeout, linger: closeLinger})
	waitingNow := func() int { return len(l.slots) }

	// Dialled at once, in goroutines that report back here.
	type dialed struct {
		conn net.Conn
		err  error
	}
	results := make(chan dialed, maxWaiting)
	for range maxWaiting {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), wait)
			defer cancel()
			c, err := DialLocal(ctx, l.ID(), l.LocalAddrs())
			results <- dialed{c, err}
		}()
	}
	for range maxWaiting {
		r := <-results
		if r.err != nil {
			t.Fatalf("dial: %v", r.err)
		}
		t.Cleanup(func() { r.conn.Close() })
	}
	waitFor(t, fmt.Sprintf("%d waiting connections", maxWaiting), func() bool { return waitingNow() == maxWaiting })

	one := dial(t, l)
	start := time.Now()
	readEnds(t, one)
	if d := time.Since(start); d >= timeout {
		t.Fatalf("the connection beyond the bound was closed after %v, the timeout is %v", d, timeout)
	}
	if n := waitingNow(); n != maxWaiting {
		t.Fatalf("%d connections wait when the one beyond the bound was closed, want %d", n, maxWaiting)
	}

	waitFor(t, "the waiting connections to time out", func() bool { return waitingNow() == 0 })
	c := dial(t, l)
	accept(t, l, c, "served")
}

func TestAcceptLoopBacksOff(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	l := &Listener{ctx: ctx, cancel: cancel, log: slog.New(slog.DiscardHandler)}
	var calls atomic.Int64
	failing := func(context.Context) (*iroh.Conn, error) {
		calls.Add(1)
		return nil, errors.New("it is always like this")
	}
	l.wg.Add(1)
	go l.acceptLoop(failing, make(chan struct{}))

	time.Sleep(300 * time.Millisecond)
	if n := calls.Load(); n < 2 || n > 20 {
		t.Errorf("%d calls of a failing accept in 300 ms; want a few", n)
	}
	cancel()
	done := make(chan struct{})
	go func() {
		l.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(wait):
		t.Fatal("the accept loop did not end with the listener's context")
	}
}

// A connection its owner closed finishes its grace when the listener is
// closed right after; one its owner did not close is cut at once.
func TestListenerCloseLetsAGraceFinish(t *testing.T) {
	const grace = 300 * time.Millisecond
	l, _ := testListener(t, timeouts{stream: StreamTimeout, linger: grace})
	c := dial(t, l)
	s := accept(t, l, c, "hello")
	open := dial(t, l)
	accept(t, l, open, "hello") // its owner does not close it

	// The server writes more than the stream's window holds, closes the
	// connection right after the last Write, and the listener right
	// after that, while the client is still reading.
	big := strings.Repeat("kosh", 1<<18)
	type result struct {
		start time.Time
		took  time.Duration
		err   error
	}
	closed := make(chan result, 1)
	go func() {
		if _, err := io.WriteString(s, big); err != nil {
			closed <- result{err: err}
			return
		}
		if err := s.Close(); err != nil {
			closed <- result{err: err}
			return
		}
		start := time.Now()
		err := l.Close()
		closed <- result{start, time.Since(start), err}
	}()
	readExactly(t, c, big)
	readEnds(t, c)
	readEnds(t, open)
	var r result
	select {
	case r = <-closed:
	case <-time.After(wait):
		t.Fatal("the server's Write, Close and the listener's Close did not return")
	}
	if r.err != nil {
		t.Fatalf("write or close: %v", r.err)
	}
	if r.took > grace+grace/2 {
		t.Errorf("Close took %v, more than the grace of %v", r.took, grace)
	}
}

// When the listener is closed, a connection its owner did not close is
// cut at once.
func TestListenerCloseCutsAnOpenConnection(t *testing.T) {
	const grace = time.Minute
	l, _ := testListener(t, timeouts{stream: StreamTimeout, linger: grace})
	open := dial(t, l)
	accept(t, l, open, "hello") // its owner does not close it
	start := time.Now()
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("Close took %v", d)
	}
	select {
	case <-open.(*conn).qc.Context().Done():
	case <-time.After(time.Second):
		t.Error("the connection its owner did not close is still open")
	}
}
