package sshd

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"golang.org/x/crypto/ssh"

	"github.com/fables-for-robots/kosh/keys"
)

// waitTime is how long a test waits for something that should happen
// soon. It is a deadline, not a sleep: every wait ends as soon as its
// condition holds.
const waitTime = 5 * time.Second

// eventually waits until cond holds, and fails the test when it does not
// within waitTime.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	eventuallyWithin(t, waitTime, what, cond)
}

// eventuallyWithin is eventually with a wait of its own, for what takes
// the machine real work and not only a turn of the scheduler.
func eventuallyWithin(t *testing.T, wait time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(wait)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("waited %v: %s", wait, what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// The log.

type logRecord struct {
	Level slog.Level
	Msg   string
	Attrs map[string]string
}

func (r logRecord) String() string {
	return fmt.Sprintf("%s %q %v", r.Level, r.Msg, r.Attrs)
}

// logRecorder keeps what the server logs, for the tests to look at.
type logRecorder struct {
	mu   sync.Mutex
	recs []logRecord
}

func (r *logRecorder) records() []logRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]logRecord(nil), r.recs...)
}

// find returns the first record for which match is true.
func (r *logRecorder) find(match func(logRecord) bool) (logRecord, bool) {
	for _, rec := range r.records() {
		if match(rec) {
			return rec, true
		}
	}
	return logRecord{}, false
}

// waitFor waits for a record for which match is true and returns it.
func (r *logRecorder) waitFor(t *testing.T, what string, match func(logRecord) bool) logRecord {
	t.Helper()
	var rec logRecord
	deadline := time.Now().Add(waitTime)
	for {
		var ok bool
		if rec, ok = r.find(match); ok {
			return rec
		}
		if time.Now().After(deadline) {
			t.Fatalf("no log record %s in:\n%s", what, r.dump())
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func (r *logRecorder) dump() string {
	var b strings.Builder
	for _, rec := range r.records() {
		b.WriteString(rec.String())
		b.WriteByte('\n')
	}
	return b.String()
}

type recordingHandler struct {
	r     *logRecorder
	attrs []slog.Attr
}

func (h recordingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h recordingHandler) Handle(_ context.Context, rec slog.Record) error {
	attrs := map[string]string{}
	for _, a := range h.attrs {
		attrs[a.Key] = a.Value.String()
	}
	rec.Attrs(func(a slog.Attr) bool {
		attrs[a.Key] = a.Value.String()
		return true
	})
	h.r.mu.Lock()
	h.r.recs = append(h.r.recs, logRecord{Level: rec.Level, Msg: rec.Message, Attrs: attrs})
	h.r.mu.Unlock()
	return nil
}

func (h recordingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return recordingHandler{r: h.r, attrs: append(append([]slog.Attr(nil), h.attrs...), attrs...)}
}

func (h recordingHandler) WithGroup(string) slog.Handler { return h }

// Keys.

// hostKeyPEM returns a new ed25519 host key in OpenSSH's format.
func hostKeyPEM(t *testing.T) []byte {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(block)
}

// newSigner returns a new ed25519 client key.
func newSigner(t *testing.T) ssh.Signer {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	s, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// keyLine is a line of a key list for signer, named name.
func keyLine(signer ssh.Signer, name string, readOnly bool) string {
	line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey()))) + " " + name
	if readOnly {
		line = keys.OptionReadOnly + " " + line
	}
	return line
}

// setKeys makes lines the key list.
func setKeys(t *testing.T, list *keys.List, lines ...string) {
	t.Helper()
	set, rejected := keys.Parse(strings.NewReader(strings.Join(lines, "\n")))
	if len(rejected) > 0 {
		t.Fatalf("rejected lines: %v", rejected)
	}
	list.Replace(set)
}

// The test model.

// testWord is what the test model draws first.
const testWord = "KOSHWORD"

// panicInitName is the key name whose sessions' models panic in Init.
const panicInitName = "panic-init"

// testModel draws testWord and the session's grant. q quits; p panics in
// Update, v makes View panic, c runs a command that panics; f makes it
// draw a full screen of new text every millisecond.
type testModel struct {
	s             Session
	width, height int
	panicView     bool
	flood         bool
	frame         int
}

type floodMsg struct{}

func flood() tea.Cmd {
	return tea.Tick(time.Millisecond, func(time.Time) tea.Msg { return floodMsg{} })
}

func (m testModel) Init() tea.Cmd {
	if m.s.Grant.Name == panicInitName {
		panic("test model: panic in Init")
	}
	return nil
}

func (m testModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
	case floodMsg:
		m.frame++
		return m, flood()
	case tea.KeyPressMsg:
		switch msg.String() {
		case "q":
			return m, tea.Quit
		case "p":
			panic("test model: panic in Update")
		case "v":
			m.panicView = true
		case "c":
			return m, func() tea.Msg { panic("test model: panic in a command") }
		case "f":
			m.flood = true
			return m, flood()
		}
	}
	return m, nil
}

func (m testModel) View() tea.View {
	if m.panicView {
		panic("test model: panic in View")
	}
	if m.flood {
		line := strings.Repeat(string(rune('a'+m.frame%26)), max(m.width, 1))
		return tea.NewView(strings.Repeat(line+"\n", max(m.height, 1)))
	}
	grant := "full"
	if m.s.Grant.ReadOnly {
		grant = "read-only"
	}
	return tea.NewView(testWord + "\ngrant:" + grant + "\nname:" + m.s.Grant.Name + "\n")
}

// The server.

// harness is a server on a loopback TCP listener with a key list the
// test controls.
type harness struct {
	t    *testing.T
	srv  *Server
	list *keys.List
	log  *logRecorder
	addr string

	// lines is the key list, as key wrote it.
	lines []string

	// sessions receives every Session the model maker was given.
	sessions chan Session
	// models counts the models made.
	models atomic.Int32

	served   chan struct{} // closed when Serve has returned
	serveErr error         // what it returned
}

// serveResult waits for Serve to return, and returns what it returned.
func (h *harness) serveResult() error {
	h.t.Helper()
	select {
	case <-h.served:
		return h.serveErr
	case <-time.After(waitTime):
		h.t.Fatal("Serve has not returned")
		return nil
	}
}

// option changes the Config of a harness's server before New, or the
// server after New and before Serve.
type option struct {
	cfg func(*Config)
	srv func(*Server)
}

func withConfig(f func(*Config)) option { return option{cfg: f} }
func withServer(f func(*Server)) option { return option{srv: f} }

// newHarness starts a server with short limits, changed by opts. After
// the test the server is shut down and the goroutines it started are
// checked to be gone.
func newHarness(t *testing.T, opts ...option) *harness {
	t.Helper()
	t.Cleanup(func() { checkNoServerGoroutines(t) })

	h := &harness{
		t:        t,
		list:     keys.NewList(nil),
		log:      &logRecorder{},
		sessions: make(chan Session, 100),
	}
	cfg := Config{
		HostKeyPEM: hostKeyPEM(t),
		Keys:       h.list,
		Model: func(s Session) tea.Model {
			h.models.Add(1)
			h.sessions <- s
			return testModel{s: s}
		},
		Version:          "test-1.2.3",
		Log:              slog.New(recordingHandler{r: h.log}),
		HandshakeTimeout: 2 * time.Second,
	}
	for _, o := range opts {
		if o.cfg != nil {
			o.cfg(&cfg)
		}
	}
	srv, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range opts {
		if o.srv != nil {
			o.srv(srv)
		}
	}
	h.srv = srv

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	h.addr = l.Addr().String()
	h.served = make(chan struct{})
	go func() {
		h.serveErr = srv.Serve(l, "tcp")
		close(h.served)
	}()

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), waitTime)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			t.Errorf("Shutdown: %v", err)
		}
		if err := h.serveResult(); err != nil {
			t.Errorf("Serve: %v", err)
		}
	})
	return h
}

// key adds a new key to the key list, and returns it.
func (h *harness) key(name string, readOnly bool) ssh.Signer {
	h.t.Helper()
	s := newSigner(h.t)
	h.lines = append(h.lines, keyLine(s, name, readOnly))
	setKeys(h.t, h.list, h.lines...)
	return s
}

// nextSession returns the next Session the model maker was given.
func (h *harness) nextSession() Session {
	h.t.Helper()
	select {
	case s := <-h.sessions:
		return s
	case <-time.After(waitTime):
		h.t.Fatal("no session was made")
		return Session{}
	}
}

// The client.

func clientConfig(user string, auth ...ssh.AuthMethod) *ssh.ClientConfig {
	return &ssh.ClientConfig{
		User:            user,
		Auth:            auth,
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         waitTime,
	}
}

// dialRaw opens a TCP connection to the server, closed after the test.
func (h *harness) dialRaw() net.Conn {
	h.t.Helper()
	c, err := net.DialTimeout("tcp", h.addr, waitTime)
	if err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(func() { _ = c.Close() })
	return c
}

// connect makes an SSH connection over c with cfg.
func connect(c net.Conn, addr string, cfg *ssh.ClientConfig) (*ssh.Client, error) {
	sc, chans, reqs, err := ssh.NewClientConn(c, addr, cfg)
	if err != nil {
		return nil, err
	}
	return ssh.NewClient(sc, chans, reqs), nil
}

// dial connects as user with signer, and fails the test when that fails.
// The client is closed after the test.
func (h *harness) dial(signer ssh.Signer, user string) *ssh.Client {
	h.t.Helper()
	cl, err := connect(h.dialRaw(), h.addr, clientConfig(user, ssh.PublicKeys(signer)))
	if err != nil {
		h.t.Fatalf("connecting: %v", err)
	}
	h.t.Cleanup(func() { _ = cl.Close() })
	return cl
}

// syncBuffer is a bytes.Buffer that may be written and read at once.
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

// term is a session of a client, with its output.
type term struct {
	t      *testing.T
	sess   *ssh.Session
	stdin  io.Writer
	stdout *syncBuffer
	stderr *syncBuffer
	done   chan error
}

// openTerm opens a session with a terminal of 80 by 24 and a shell.
func openTerm(t *testing.T, cl *ssh.Client) *term {
	t.Helper()
	return openSession(t, cl, true, "")
}

// openSession opens a session, with a terminal or without, and starts
// cmd in it or, when cmd is "", a shell.
func openSession(t *testing.T, cl *ssh.Client, pty bool, cmd string) *term {
	t.Helper()
	sess, err := cl.NewSession()
	if err != nil {
		t.Fatalf("opening a session: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	tm := &term{t: t, sess: sess, stdout: &syncBuffer{}, stderr: &syncBuffer{}, done: make(chan error, 1)}
	sess.Stdout, sess.Stderr = tm.stdout, tm.stderr
	if tm.stdin, err = sess.StdinPipe(); err != nil {
		t.Fatal(err)
	}
	if pty {
		if err := sess.RequestPty("xterm-256color", 24, 80, ssh.TerminalModes{}); err != nil {
			t.Fatalf("asking for a terminal: %v", err)
		}
	}
	if cmd == "" {
		err = sess.Shell()
	} else {
		err = sess.Start(cmd)
	}
	if err != nil {
		t.Fatalf("starting: %v", err)
	}
	go func() { tm.done <- sess.Wait() }()
	return tm
}

// waitFor waits until the session's output holds text.
func (tm *term) waitFor(text string) {
	tm.t.Helper()
	deadline := time.Now().Add(waitTime)
	for !strings.Contains(tm.stdout.String(), text) {
		if time.Now().After(deadline) {
			tm.t.Fatalf("waited for %q; the session wrote %q and on its error stream %q", text, tm.stdout.String(), tm.stderr.String())
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// press sends keys to the session.
func (tm *term) press(keys string) {
	tm.t.Helper()
	if _, err := io.WriteString(tm.stdin, keys); err != nil {
		tm.t.Fatalf("pressing %q: %v", keys, err)
	}
}

// exit waits for the session to end and returns its exit status.
func (tm *term) exit() int {
	tm.t.Helper()
	select {
	case err := <-tm.done:
		return exitStatus(tm.t, err)
	case <-time.After(waitTime):
		tm.t.Fatalf("the session has not ended; it wrote %q", tm.stdout.String())
		return -1
	}
}

// ended reports whether the session has ended, without waiting.
func (tm *term) ended() bool {
	select {
	case err := <-tm.done:
		tm.done <- err
		return true
	default:
		return false
	}
}

func exitStatus(t *testing.T, err error) int {
	t.Helper()
	if err == nil {
		return 0
	}
	var ee *ssh.ExitError
	if errors.As(err, &ee) {
		return ee.ExitStatus()
	}
	t.Fatalf("the session ended without an exit status: %v", err)
	return -1
}

// Goroutines.

// serverGoroutineMarkers are what the stack of a goroutine of the server,
// its libraries or a test's client holds.
var serverGoroutineMarkers = []string{
	"charm.land/",
	"github.com/charmbracelet/",
	"golang.org/x/crypto/ssh",
	"github.com/fables-for-robots/kosh/sshd.",
}

// serverGoroutines returns the stacks of the goroutines that are the
// server's or its clients', but for the calling one.
func serverGoroutines() []string {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, 2*len(buf))
	}
	var found []string
	for i, g := range strings.Split(string(buf), "\n\n") {
		// The caller, and a parent test waiting for its subtest.
		if i == 0 || strings.Contains(g, "testing.(*T).Run(") {
			continue
		}
		for _, m := range serverGoroutineMarkers {
			if strings.Contains(g, m) {
				found = append(found, g)
				break
			}
		}
	}
	return found
}

// checkNoServerGoroutines fails the test when goroutines of the server
// are still there after waitTime.
func checkNoServerGoroutines(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(waitTime)
	for {
		left := serverGoroutines()
		if len(left) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Errorf("%d goroutines of the server are left:\n\n%s", len(left), strings.Join(left, "\n\n"))
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}
