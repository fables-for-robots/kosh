package kube

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	"k8s.io/streaming/pkg/httpstream/wsstream"
)

// execFixture is a Cluster whose clients reach a server of the test's
// own, which stands for the API, and a session of it with a full grant.
type execFixture struct {
	c        *Cluster
	s        *session
	end      context.CancelFunc // ends the session
	log      *auditLog
	pods     Resource
	requests atomic.Int32 // what the server was sent
}

// execEnv starts a server that answers every request with h and makes
// the fixture on it. Everything ends with the test.
func execEnv(t *testing.T, h http.HandlerFunc) *execFixture {
	t.Helper()
	f := &execFixture{log: newAuditLog()}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.requests.Add(1)
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	cfg := &rest.Config{Host: srv.URL}
	typed, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	e := newEnv(t, envConfig{})
	f.pods = e.resource(t, "", "pods")
	f.c = newCluster(cfg, typed, dyn, nil, e.c.resourceList(), Options{Log: f.log.logger()})
	t.Cleanup(f.c.Close)
	ctx, end := context.WithCancel(context.Background())
	t.Cleanup(end)
	f.s, f.end = f.c.Session(ctx, fullGrant).(*session), end
	return f
}

// resource returns the resource group/plural of the Cluster.
func (f *execFixture) resource(t *testing.T, group, plural string) Resource {
	t.Helper()
	for _, r := range f.c.resourceList() {
		if r.Is(group, plural) {
			return r
		}
	}
	t.Fatalf("no resource %s.%s", plural, group)
	return Resource{}
}

// pod returns the reference of the pod default/name.
func (f *execFixture) pod(name string) ObjectRef {
	return ObjectRef{Resource: f.pods, Namespace: "default", Name: name}
}

// execStreams are the server's ends of an exec stream, as the kubelet
// has them.
type execStreams struct {
	stdin  io.Reader
	stdout io.Writer
	// sizes has every size that came on the resize channel.
	sizes  <-chan remotecommand.TerminalSize
	errors io.Writer
}

// exit writes status to the error channel: how the command ended.
func (st *execStreams) exit(status metav1.Status) error {
	b, err := json.Marshal(status)
	if err != nil {
		return err
	}
	_, err = st.errors.Write(b)
	return err
}

// execSuccess is the status of a command that ended with 0.
var execSuccess = metav1.Status{Status: metav1.StatusSuccess}

// execExitCode is the status of a command that ended with code.
func execExitCode(code string) metav1.Status {
	return metav1.Status{
		Status: metav1.StatusFailure,
		Reason: "NonZeroExitCode",
		Details: &metav1.StatusDetails{Causes: []metav1.StatusCause{
			{Type: "ExitCode", Message: code},
		}},
	}
}

// execInternal is the status of a stream that failed, with text as the
// API words it.
func execInternal(text string) metav1.Status {
	return metav1.Status{Status: metav1.StatusFailure, Reason: metav1.StatusReasonInternalError, Code: 500, Message: text}
}

// serveExec upgrades the request to the exec stream of protocol v5 with
// k8s.io/streaming's server side, which is the kubelet's, sends the empty
// message the kubelet sends to say it is there, and runs run on the
// streams. The connection is closed when run returns.
func serveExec(t *testing.T, run func(st *execStreams)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		conn := wsstream.NewConn(map[string]wsstream.ChannelProtocolConfig{
			"v5.channel.k8s.io": {Binary: true, Channels: []wsstream.ChannelType{
				wsstream.ReadChannel,   // 0 stdin
				wsstream.WriteChannel,  // 1 stdout
				wsstream.IgnoreChannel, // 2 stderr: none with a terminal
				wsstream.WriteChannel,  // 3 error
				wsstream.ReadChannel,   // 4 resize
			}},
		})
		_, streams, err := conn.Open(w, r)
		if err != nil {
			t.Errorf("the exec stream did not open: %v", err)
			return
		}
		defer conn.Close()
		sizes := make(chan remotecommand.TerminalSize, 16)
		go func() {
			defer close(sizes)
			dec := json.NewDecoder(streams[4])
			for {
				var s remotecommand.TerminalSize
				if dec.Decode(&s) != nil {
					return
				}
				sizes <- s
			}
		}()
		if _, err := streams[1].Write(nil); err != nil {
			t.Errorf("writing the first message: %v", err)
			return
		}
		run(&execStreams{stdin: streams[0], stdout: streams[1], sizes: sizes, errors: streams[3]})
	}
}

// execSucceeds is a command that ends at once, with 0.
func execSucceeds(t *testing.T) http.HandlerFunc {
	return serveExec(t, func(st *execStreams) {
		if err := st.exit(execSuccess); err != nil {
			t.Errorf("writing the status: %v", err)
		}
	})
}

// execRun calls Exec in a goroutine and returns what it returns, or
// fails the test when it has not returned within a minute.
func execRun(t *testing.T, ctx context.Context, s *session, pod ObjectRef, opts ExecOptions) (ExecResult, error) {
	t.Helper()
	type out struct {
		res ExecResult
		err error
	}
	done := make(chan out, 1)
	go func() {
		res, err := s.Exec(ctx, pod, opts)
		done <- out{res, err}
	}()
	select {
	case o := <-done:
		return o.res, o.err
	case <-time.After(time.Minute):
		t.Fatal("Exec has not returned within a minute")
		return ExecResult{}, nil
	}
}

func TestExecChecksBeforeItSends(t *testing.T) {
	sh := []string{"sh"}
	tests := []struct {
		name string
		ref  func(f *execFixture) ObjectRef
		opts ExecOptions
	}{
		{"a configmap", func(f *execFixture) ObjectRef {
			return ObjectRef{Resource: f.resource(t, "", "configmaps"), Namespace: "default", Name: "web"}
		}, ExecOptions{Command: sh}},
		{"a deployment", func(f *execFixture) ObjectRef {
			return ObjectRef{Resource: f.resource(t, "apps", "deployments"), Namespace: "default", Name: "web"}
		}, ExecOptions{Command: sh}},
		{"a resource the cluster does not have", func(f *execFixture) ObjectRef {
			r := f.pods
			r.GVR.Group = "example.org"
			return ObjectRef{Resource: r, Namespace: "default", Name: "web"}
		}, ExecOptions{Command: sh}},
		{"a pod name with a slash", func(f *execFixture) ObjectRef { return f.pod("a/b") }, ExecOptions{Command: sh}},
		{"a pod of no namespace", func(f *execFixture) ObjectRef {
			return ObjectRef{Resource: f.pods, Name: "web"}
		}, ExecOptions{Command: sh}},
		{"a container a/b", func(f *execFixture) ObjectRef { return f.pod("web") }, ExecOptions{Container: "a/b", Command: sh}},
		{"a container of 64 bytes", func(f *execFixture) ObjectRef { return f.pod("web") }, ExecOptions{Container: strings.Repeat("a", 64), Command: sh}},
		{"an empty command", func(f *execFixture) ObjectRef { return f.pod("web") }, ExecOptions{Container: "web"}},
		{"a command of no words", func(f *execFixture) ObjectRef { return f.pod("web") }, ExecOptions{Container: "web", Command: []string{}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := execEnv(t, execSucceeds(t))
			ref := tt.ref(f)
			res, err := execRun(t, context.Background(), f.s, ref, tt.opts)
			if err == nil {
				t.Fatal("no error")
			}
			if res != (ExecResult{}) {
				t.Errorf("result %+v", res)
			}
			if n := f.requests.Load(); n != 0 {
				t.Errorf("the API was sent %d requests", n)
			}
			recs := f.log.with("action")
			if len(recs) != 1 || len(f.log.all()) != 1 {
				t.Fatalf("records %+v, want the action's failure", f.log.all())
			}
			if a, _ := recs[0].attr("action"); a != "exec" {
				t.Errorf("action %q", a)
			}
			if _, ok := recs[0].attr("error"); !ok {
				t.Errorf("the record has no error: %+v", recs[0])
			}
			if o, _ := recs[0].attr("object"); o != ref.String() {
				t.Errorf("object %q, want %q", o, ref.String())
			}
			for k, v := range map[string]string{"phase": "end", "started": "false", "container": tt.opts.Container} {
				if got, _ := recs[0].attr(k); got != v {
					t.Errorf("%s = %q, want %q", k, got, v)
				}
			}
		})
	}
}

func TestExecRequest(t *testing.T) {
	got := make(chan *http.Request, 1)
	f := execEnv(t, func(w http.ResponseWriter, r *http.Request) {
		got <- r.Clone(context.Background())
		execSucceeds(t)(w, r)
	})
	res, err := execRun(t, context.Background(), f.s, f.pod("web"), ExecOptions{
		Container: "web",
		Command:   []string{"sh", "-c", "a b"},
		Stdin:     strings.NewReader(""),
		Stdout:    io.Discard,
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res != (ExecResult{Started: true}) {
		t.Errorf("result %+v", res)
	}
	var r *http.Request
	select {
	case r = <-got:
	default:
		t.Fatal("the API was sent nothing")
	}
	if r.Method != http.MethodGet {
		t.Errorf("method %s, want GET", r.Method)
	}
	if r.URL.Path != "/api/v1/namespaces/default/pods/web/exec" {
		t.Errorf("path %s", r.URL.Path)
	}
	q := r.URL.Query()
	if c := q["container"]; !slices.Equal(c, []string{"web"}) {
		t.Errorf("container %q", c)
	}
	if c := q["command"]; !slices.Equal(c, []string{"sh", "-c", "a b"}) {
		t.Errorf("command %q", c)
	}
	for _, k := range []string{"stdin", "stdout", "tty"} {
		if v := q[k]; !slices.Equal(v, []string{"true"}) {
			t.Errorf("%s %q, want true", k, v)
		}
	}
	if v := q.Get("stderr"); v == "true" {
		t.Errorf("stderr %q", v)
	}
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		t.Errorf("Upgrade %q, want websocket", r.Header.Get("Upgrade"))
	}
	protocols := strings.Split(r.Header.Get("Sec-Websocket-Protocol"), ",")
	if strings.TrimSpace(protocols[0]) != "v5.channel.k8s.io" {
		t.Errorf("protocols %q, want v5.channel.k8s.io first", protocols)
	}

	t.Run("no container", func(t *testing.T) {
		got := make(chan *http.Request, 1)
		f := execEnv(t, func(w http.ResponseWriter, r *http.Request) {
			got <- r.Clone(context.Background())
			execSucceeds(t)(w, r)
		})
		if _, err := execRun(t, context.Background(), f.s, f.pod("web"), ExecOptions{Command: []string{"sh"}}); err != nil {
			t.Fatalf("Exec: %v", err)
		}
		r := <-got
		if _, ok := r.URL.Query()["container"]; ok {
			t.Errorf("a container was asked for: %s", r.URL.RawQuery)
		}
	})
}

// execOut is a terminal's output: it keeps what was written to it and
// says when something was.
type execOut struct {
	mu    sync.Mutex
	b     bytes.Buffer
	wrote chan struct{}
}

func newExecOut() *execOut { return &execOut{wrote: make(chan struct{}, 1)} }

func (o *execOut) Write(p []byte) (int, error) {
	o.mu.Lock()
	o.b.Write(p)
	o.mu.Unlock()
	select {
	case o.wrote <- struct{}{}:
	default:
	}
	return len(p), nil
}

func (o *execOut) String() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.b.String()
}

// waitFor waits a minute at most for the output to be want.
func (o *execOut) waitFor(t *testing.T, want string) {
	t.Helper()
	deadline := time.After(time.Minute)
	for {
		got := o.String()
		if got == want {
			return
		}
		if len(got) >= len(want) {
			t.Fatalf("output %q, want %q", got, want)
		}
		select {
		case <-o.wrote:
		case <-deadline:
			t.Fatalf("output %q within a minute, want %q", got, want)
		}
	}
}

// receive returns what comes from c within a minute, or fails the test.
func receive[T any](t *testing.T, what string, c <-chan T) T {
	t.Helper()
	select {
	case v := <-c:
		return v
	case <-time.After(time.Minute):
		t.Fatalf("not within a minute: %s", what)
		var zero T
		return zero
	}
}

// execTerminal is the terminal Exec is given in a test: its input is a
// pipe the test writes to, its size is what the test sets.
type execTerminal struct {
	in      *io.PipeReader
	type_   *io.PipeWriter
	out     *execOut
	resized chan struct{}
	asked   chan remotecommand.TerminalSize // each size Size returned

	mu   sync.Mutex
	w, h int
}

func newExecTerminal(t *testing.T, w, h int) *execTerminal {
	pr, pw := io.Pipe()
	// The caller's reader ends with its turn: what Exec's stream may
	// still read is ended with the test.
	t.Cleanup(func() { pw.Close() })
	return &execTerminal{in: pr, type_: pw, out: newExecOut(), resized: make(chan struct{}, 1), asked: make(chan remotecommand.TerminalSize, 16), w: w, h: h}
}

func (term *execTerminal) size() (int, int) {
	term.mu.Lock()
	w, h := term.w, term.h
	term.mu.Unlock()
	term.asked <- remotecommand.TerminalSize{Width: uint16(max(w, 0)), Height: uint16(max(h, 0))}
	return w, h
}

// resize sets the size and says it changed, and waits until Size was
// asked for it.
func (term *execTerminal) resize(t *testing.T, w, h int) {
	t.Helper()
	term.mu.Lock()
	term.w, term.h = w, h
	term.mu.Unlock()
	term.resized <- struct{}{}
	receive(t, "the size is asked for", term.asked)
}

func (term *execTerminal) options(command ...string) ExecOptions {
	return ExecOptions{Container: "web", Command: command, Stdin: term.in, Stdout: term.out, Size: term.size, Resized: term.resized}
}

// execStart calls Exec in a goroutine; what it returns comes on the
// channel.
type execReturn struct {
	res ExecResult
	err error
}

func execStart(ctx context.Context, s *session, pod ObjectRef, opts ExecOptions) <-chan execReturn {
	done := make(chan execReturn, 1)
	go func() {
		res, err := s.Exec(ctx, pod, opts)
		done <- execReturn{res, err}
	}()
	return done
}

func TestExecStream(t *testing.T) {
	const printed = "hello\nworld\r\n\x1b[2J\x00\xff"
	const typed = "ls -l\r\x03\x04"
	firstSize := make(chan remotecommand.TerminalSize, 1)
	nextSize := make(chan remotecommand.TerminalSize, 1)
	gotInput := make(chan string, 1)
	f := execEnv(t, serveExec(t, func(st *execStreams) {
		s, ok := <-st.sizes
		if !ok {
			t.Error("no first size")
			return
		}
		firstSize <- s
		if _, err := st.stdout.Write([]byte(printed)); err != nil {
			t.Errorf("writing stdout: %v", err)
			return
		}
		b := make([]byte, len(typed))
		if _, err := io.ReadFull(st.stdin, b); err != nil {
			t.Errorf("reading stdin: %v", err)
			return
		}
		gotInput <- string(b)
		s, ok = <-st.sizes
		if !ok {
			t.Error("no second size")
			return
		}
		nextSize <- s
		if err := st.exit(execSuccess); err != nil {
			t.Errorf("writing the status: %v", err)
		}
	}))
	term := newExecTerminal(t, 80, 24)
	done := execStart(context.Background(), f.s, f.pod("web"), term.options("sh"))

	if s := receive(t, "the first size", firstSize); s != (remotecommand.TerminalSize{Width: 80, Height: 24}) {
		t.Errorf("first size %+v, want 80x24", s)
	}
	term.out.waitFor(t, printed)
	if _, err := term.type_.Write([]byte(typed)); err != nil {
		t.Fatal(err)
	}
	if in := receive(t, "the input", gotInput); in != typed {
		t.Errorf("stdin %q, want %q", in, typed)
	}
	receive(t, "the first size was asked for", term.asked)
	// A size of zero is not sent; the one after it is.
	term.resize(t, 0, 0)
	term.resize(t, 100, 30)
	if s := receive(t, "the next size", nextSize); s != (remotecommand.TerminalSize{Width: 100, Height: 30}) {
		t.Errorf("next size %+v, want 100x30", s)
	}
	out := receive(t, "Exec returns", done)
	if out.err != nil {
		t.Fatalf("Exec: %v", out.err)
	}
	if out.res != (ExecResult{Started: true, Status: 0}) {
		t.Errorf("result %+v", out.res)
	}
}

func TestExecSizes(t *testing.T) {
	size := func(w, h int) func() (int, int) { return func() (int, int) { return w, h } }
	next := func(q remotecommand.TerminalSizeQueue) <-chan *remotecommand.TerminalSize {
		c := make(chan *remotecommand.TerminalSize, 1)
		go func() { c <- q.Next() }()
		return c
	}

	t.Run("no Size sends no size", func(t *testing.T) {
		if q := execSizes(context.Background(), ExecOptions{}); q != nil {
			t.Errorf("a queue %v", q)
		}
	})

	t.Run("no Resized sends the first size only", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		q := execSizes(ctx, ExecOptions{Size: size(80, 24)})
		if s := receive(t, "the first size", next(q)); s == nil || *s != (remotecommand.TerminalSize{Width: 80, Height: 24}) {
			t.Fatalf("first %v", s)
		}
		second := next(q)
		select {
		case s := <-second:
			t.Fatalf("a second size %v", s)
		default:
		}
		cancel()
		if s := receive(t, "the queue ends", second); s != nil {
			t.Errorf("after the end %v", s)
		}
	})

	t.Run("sizes of zero or less are not sent", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		var mu sync.Mutex
		sizes := [][2]int{{0, 24}, {80, 0}, {-1, -1}, {81, 25}}
		resized := make(chan struct{}, len(sizes))
		for range sizes[1:] {
			resized <- struct{}{}
		}
		q := execSizes(ctx, ExecOptions{Resized: resized, Size: func() (int, int) {
			mu.Lock()
			defer mu.Unlock()
			s := sizes[0]
			sizes = sizes[1:]
			return s[0], s[1]
		}})
		if s := receive(t, "a size", next(q)); s == nil || *s != (remotecommand.TerminalSize{Width: 81, Height: 25}) {
			t.Errorf("size %v, want 81x25", s)
		}
	})

	t.Run("a size too large for the protocol is its largest", func(t *testing.T) {
		q := execSizes(context.Background(), ExecOptions{Size: size(1<<20, 70000)})
		if s := receive(t, "a size", next(q)); s == nil || *s != (remotecommand.TerminalSize{Width: 65535, Height: 65535}) {
			t.Errorf("size %v", s)
		}
	})

	t.Run("the queue ends with the context while it waits for a change", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		q := execSizes(ctx, ExecOptions{Size: size(0, 0), Resized: make(chan struct{})})
		first := next(q)
		cancel()
		if s := receive(t, "the queue ends", first); s != nil {
			t.Errorf("size %v", s)
		}
	})
}

func TestExecStatus(t *testing.T) {
	f := execEnv(t, serveExec(t, func(st *execStreams) {
		if _, err := st.stdout.Write([]byte("bye\r\n")); err != nil {
			t.Errorf("writing stdout: %v", err)
		}
		if err := st.exit(execExitCode("7")); err != nil {
			t.Errorf("writing the status: %v", err)
		}
	}))
	term := newExecTerminal(t, 80, 24)
	res, err := execRun(t, context.Background(), f.s, f.pod("web"), term.options("sh"))
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res != (ExecResult{Started: true, Status: 7}) {
		t.Errorf("result %+v, want started with 7", res)
	}
}

// The texts of the probe's runs on containerd 2.2.0, as the WebSocket
// stream gave them.
const (
	execNoShell     = `Internal error occurred: Internal error occurred: error executing command in container: failed to exec in container: failed to start exec "80c33c691f999d15757aae25dad85857e38b44fe3f1e4008648e46a3a2716f75": OCI runtime exec failed: exec failed: unable to start container process: exec: "sh": executable file not found in $PATH`
	execNoContainer = `Internal error occurred: unable to upgrade connection: container not found ("web")`
)

func TestExecNotStarted(t *testing.T) {
	long := "Internal error occurred: " + strings.Repeat("é", 2048)
	tests := []struct {
		name    string
		handler func(t *testing.T) http.HandlerFunc
		want    string // the end of the error's text
	}{
		{"no such program", func(t *testing.T) http.HandlerFunc {
			return serveExec(t, func(st *execStreams) { _ = st.exit(execInternal(execNoShell)) })
		}, `: exec: "sh": executable file not found in $PATH`},
		{"the container is not running", func(t *testing.T) http.HandlerFunc {
			return serveExec(t, func(st *execStreams) { _ = st.exit(execInternal(execNoContainer)) })
		}, `: unable to upgrade connection: container not found ("web")`},
		{"a reason of 4 KiB", func(t *testing.T) http.HandlerFunc {
			return serveExec(t, func(st *execStreams) { _ = st.exit(execInternal(long)) })
		}, strings.Repeat("é", 512)},
		{"an upgrade that fails without a status", func(t *testing.T) http.HandlerFunc {
			return func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, "the proxy is down", http.StatusBadGateway)
			}
		}, "the proxy is down"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := execEnv(t, tt.handler(t))
			term := newExecTerminal(t, 80, 24)
			res, err := execRun(t, context.Background(), f.s, f.pod("web"), term.options("sh"))
			if !errors.Is(err, ErrExecNotStarted) {
				t.Fatalf("error %v, want ErrExecNotStarted", err)
			}
			if res.Started {
				t.Errorf("result %+v, want not started", res)
			}
			text := err.Error()
			if !strings.HasSuffix(text, tt.want) {
				t.Errorf("error %q, want it to end with %q", text, tt.want)
			}
			if strings.Contains(text, "Internal error occurred") {
				t.Errorf("error %q", text)
			}
			if strings.Contains(text, "80c33c691f999d15757aae25dad85857e38b44fe3f1e4008648e46a3a2716f75") {
				t.Errorf("error %q holds the exec's id", text)
			}
			if reason := strings.TrimPrefix(text, ErrExecNotStarted.Error()+": "); len(reason) > 1024 || !utf8.ValidString(reason) {
				t.Errorf("a reason of %d bytes, valid %v", len(reason), utf8.ValidString(reason))
			}
		})
	}

	t.Run("the same after output", func(t *testing.T) {
		f := execEnv(t, serveExec(t, func(st *execStreams) {
			if _, err := st.stdout.Write([]byte("$ ")); err != nil {
				t.Errorf("writing stdout: %v", err)
			}
			_ = st.exit(execInternal(execNoContainer))
		}))
		term := newExecTerminal(t, 80, 24)
		res, err := execRun(t, context.Background(), f.s, f.pod("web"), term.options("sh"))
		if err == nil || errors.Is(err, ErrExecNotStarted) {
			t.Fatalf("error %v, want the stream's", err)
		}
		if !strings.Contains(err.Error(), `container not found ("web")`) {
			t.Errorf("error %q", err)
		}
		if res != (ExecResult{Started: true}) {
			t.Errorf("result %+v, want started", res)
		}
	})
}

// apiRefuses answers every request with err's status, as the API
// answers an upgrade it refuses.
func apiRefuses(t *testing.T, err *apierrors.StatusError) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		st := err.ErrStatus
		st.TypeMeta = metav1.TypeMeta{Kind: "Status", APIVersion: "v1"}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(int(st.Code))
		if err := json.NewEncoder(w).Encode(st); err != nil {
			t.Errorf("writing the status: %v", err)
		}
	}
}

func TestExecRefusedByTheAPI(t *testing.T) {
	pods := schema.GroupResource{Resource: "pods"}
	tests := []struct {
		name string
		err  *apierrors.StatusError
		is   func(error) bool
	}{
		{"forbidden", apierrors.NewForbidden(pods, "web", errors.New(`User "system:serviceaccount:kosh:kosh" cannot get resource "pods/exec"`)), apierrors.IsForbidden},
		{"not found", apierrors.NewNotFound(pods, "web"), apierrors.IsNotFound},
		{"bad request", apierrors.NewBadRequest("container nosuch is not valid for pod web"), apierrors.IsBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := execEnv(t, apiRefuses(t, tt.err))
			term := newExecTerminal(t, 80, 24)
			res, err := execRun(t, context.Background(), f.s, f.pod("web"), term.options("sh"))
			if !tt.is(err) {
				t.Fatalf("error %v (%T)", err, err)
			}
			if errors.Is(err, ErrExecNotStarted) {
				t.Errorf("error %v is ErrExecNotStarted", err)
			}
			if res != (ExecResult{}) {
				t.Errorf("result %+v", res)
			}
		})
	}
}

// execHolds is a command that prints a prompt, says on open that it
// has, and runs until its connection ends.
func execHolds(t *testing.T, open chan<- struct{}) http.HandlerFunc {
	return serveExec(t, func(st *execStreams) {
		if _, err := st.stdout.Write([]byte("$ ")); err != nil {
			t.Errorf("writing stdout: %v", err)
			return
		}
		open <- struct{}{}
		_, _ = io.Copy(io.Discard, st.stdin)
	})
}

func TestExecEndsWithItsContext(t *testing.T) {
	for _, by := range []string{"the caller", "the session"} {
		t.Run(by, func(t *testing.T) {
			open := make(chan struct{}, 1)
			f := execEnv(t, execHolds(t, open))
			term := newExecTerminal(t, 80, 24)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := execStart(ctx, f.s, f.pod("web"), term.options("sh"))
			receive(t, "the stream is open", open)
			term.out.waitFor(t, "$ ")
			if by == "the caller" {
				cancel()
			} else {
				f.end()
			}
			var out execReturn
			select {
			case out = <-done:
			case <-time.After(time.Second):
				t.Fatal("Exec has not returned a second after its context ended")
			}
			if !errors.Is(out.err, context.Canceled) {
				t.Errorf("error %v, want context.Canceled", out.err)
			}
			if ended := errors.Is(out.err, errSessionEnded); ended != (by == "the session") {
				t.Errorf("error %v: that the session ended is %v", out.err, ended)
			}
			if out.res != (ExecResult{Started: true}) {
				t.Errorf("result %+v, want started", out.res)
			}
		})
	}
}

// streamGoroutines returns how many goroutines run code of a stream:
// client-go's exec stream, the WebSocket library, or Exec's own
// terminal.
func streamGoroutines() int {
	buf := make([]byte, 4<<20)
	n := 0
	for _, g := range strings.Split(string(buf[:runtime.Stack(buf, true)]), "\n\n") {
		if strings.Contains(g, "k8s.io/client-go/tools/remotecommand") ||
			strings.Contains(g, "github.com/gorilla/websocket") ||
			strings.Contains(g, "kube.(*execSizeQueue)") ||
			strings.Contains(g, "kube.(*execOutput)") ||
			strings.Contains(g, "kube.(*execInput)") {
			n++
		}
	}
	return n
}

// goroutinesBefore waits a minute at most for the streams of tests run
// before to have ended, and returns how many goroutines run then.
func goroutinesBefore(t *testing.T) int {
	t.Helper()
	deadline := time.Now().Add(time.Minute)
	for streamGoroutines() > 0 {
		if time.Now().After(deadline) {
			t.Fatal("the streams of tests run before have not ended within a minute")
		}
		time.Sleep(10 * time.Millisecond)
	}
	return runtime.NumGoroutine()
}

// goroutinesBack waits a minute at most for the goroutines to be no
// more than before, and none to run code of a stream. client-go's
// heartbeat of a stream ends at its next beat, 5 seconds after the
// stream.
func goroutinesBack(t *testing.T, before int) {
	t.Helper()
	deadline := time.Now().Add(time.Minute)
	for runtime.NumGoroutine() > before || streamGoroutines() > 0 {
		if time.Now().After(deadline) {
			buf := make([]byte, 1<<20)
			t.Fatalf("%d goroutines a minute after Exec returned, %d before:\n%s", runtime.NumGoroutine(), before, buf[:runtime.Stack(buf, true)])
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestExecLeavesNothing(t *testing.T) {
	tests := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"a normal end", func(t *testing.T) {
			f := execEnv(t, execSucceeds(t))
			term := newExecTerminal(t, 80, 24)
			before := goroutinesBefore(t)
			if _, err := execRun(t, context.Background(), f.s, f.pod("web"), term.options("sh")); err != nil {
				t.Fatalf("Exec: %v", err)
			}
			term.type_.Close() // the caller's reader ends with its turn
			goroutinesBack(t, before)
		}},
		// The stream is cancelled once it runs. Cancelled in the moment
		// between its upgrade and the start of its reading, client-go
		// v0.37.1 leaves the stream's goroutines blocked for good: its
		// readDemuxLoop cannot set its first read deadline on the closed
		// connection and returns without closing the streams' pipes.
		{"a cancelled context", func(t *testing.T) {
			open := make(chan struct{}, 1)
			f := execEnv(t, execHolds(t, open))
			term := newExecTerminal(t, 80, 24)
			before := goroutinesBefore(t)
			ctx, cancel := context.WithCancel(context.Background())
			done := execStart(ctx, f.s, f.pod("web"), term.options("sh"))
			receive(t, "the stream is open", open)
			term.out.waitFor(t, "$ ")
			cancel()
			if out := receive(t, "Exec returns", done); !errors.Is(out.err, context.Canceled) {
				t.Fatalf("Exec: %v", out.err)
			}
			term.type_.Close()
			goroutinesBack(t, before)
		}},
		{"the session's end", func(t *testing.T) {
			open := make(chan struct{}, 1)
			f := execEnv(t, execHolds(t, open))
			term := newExecTerminal(t, 80, 24)
			before := goroutinesBefore(t)
			done := execStart(context.Background(), f.s, f.pod("web"), term.options("sh"))
			receive(t, "the stream is open", open)
			term.out.waitFor(t, "$ ")
			f.end()
			if out := receive(t, "Exec returns", done); !errors.Is(out.err, errSessionEnded) {
				t.Fatalf("Exec: %v", out.err)
			}
			term.type_.Close()
			goroutinesBack(t, before)
		}},
		{"a refused upgrade", func(t *testing.T) {
			f := execEnv(t, apiRefuses(t, apierrors.NewNotFound(schema.GroupResource{Resource: "pods"}, "web")))
			term := newExecTerminal(t, 80, 24)
			before := goroutinesBefore(t)
			if _, err := execRun(t, context.Background(), f.s, f.pod("web"), term.options("sh")); !apierrors.IsNotFound(err) {
				t.Fatalf("Exec: %v", err)
			}
			term.type_.Close()
			goroutinesBack(t, before)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, tt.run)
	}
}

func TestExecTerminalIsLetGoWhenExecReturns(t *testing.T) {
	t.Run("output", func(t *testing.T) {
		out := newExecOut()
		o := &execOutput{w: out}
		if _, err := o.Write([]byte("ab")); err != nil {
			t.Fatal(err)
		}
		o.letGo()
		if _, err := o.Write([]byte("cd")); err == nil {
			t.Error("a write after Exec returned was taken")
		}
		if got := out.String(); got != "ab" {
			t.Errorf("the terminal got %q", got)
		}
		if n := o.written(); n != 2 {
			t.Errorf("counted %d bytes, want 2", n)
		}
	})
	t.Run("no output", func(t *testing.T) {
		o := &execOutput{}
		if n, err := o.Write([]byte("ab")); n != 2 || err != nil {
			t.Errorf("Write %d, %v", n, err)
		}
		if n := o.written(); n != 2 {
			t.Errorf("counted %d bytes, want 2", n)
		}
	})
	t.Run("input", func(t *testing.T) {
		in := &execInput{r: strings.NewReader("abcd")}
		b := make([]byte, 2)
		if n, err := in.Read(b); n != 2 || err != nil {
			t.Fatalf("Read %d, %v", n, err)
		}
		in.letGo()
		if n, err := in.Read(b); n != 0 || err != io.EOF {
			t.Errorf("a read after Exec returned: %d, %v", n, err)
		}
	})
}

// execRecords returns the start and the end record of the one Exec that
// was logged, and fails the test unless there are just these two, at
// info level, with the action's attributes.
func execRecords(t *testing.T, f *execFixture) (start, end auditRecord) {
	t.Helper()
	all := f.log.all()
	if len(all) != 2 {
		t.Fatalf("records %+v, want two", all)
	}
	for i, phase := range []string{"start", "end"} {
		r := all[i]
		if r.Message != "action" || r.Level != slog.LevelInfo {
			t.Errorf("record %d: %q at %v", i, r.Message, r.Level)
		}
		want := map[string]string{
			"key":         fullGrant.Name,
			"fingerprint": fullGrant.Fingerprint,
			"action":      "exec",
			"object":      "pods/default/web",
			"phase":       phase,
			"container":   "web",
		}
		for k, v := range want {
			if got, _ := r.attr(k); got != v {
				t.Errorf("%s record: %s = %q, want %q", phase, k, got, v)
			}
		}
	}
	return all[0], all[1]
}

// command returns the command of a start record.
func (r auditRecord) command(t *testing.T) []string {
	t.Helper()
	v, ok := r.Attrs["command"]
	if !ok {
		t.Fatalf("no command in %+v", r)
	}
	words, ok := v.Any().([]string)
	if !ok {
		t.Fatalf("command %v is a %T", v, v.Any())
	}
	return words
}

// wantAttrs fails the test unless r has each of want, and none of
// absent.
func wantAttrs(t *testing.T, r auditRecord, want map[string]string, absent ...string) {
	t.Helper()
	for k, v := range want {
		if got, ok := r.attr(k); !ok || got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	for _, k := range absent {
		if got, ok := r.attr(k); ok {
			t.Errorf("%s = %q, want none", k, got)
		}
	}
}

// noneHolds fails the test when a record holds one of the texts.
func noneHolds(t *testing.T, f *execFixture, texts ...string) {
	t.Helper()
	for _, r := range f.log.all() {
		for k, v := range r.Attrs {
			for _, text := range texts {
				if strings.Contains(fmt.Sprint(v.Any()), text) || strings.Contains(r.Message, text) {
					t.Errorf("%s = %q holds %q", k, v.String(), text)
				}
			}
		}
	}
}

func TestExecLog(t *testing.T) {
	const typed, printed = "typed-4f1e-marker\r", "printed-9c2b-marker\r\n"

	t.Run("a shell that ran and ended with 0", func(t *testing.T) {
		f := execEnv(t, serveExec(t, func(st *execStreams) {
			b := make([]byte, len(typed))
			if _, err := io.ReadFull(st.stdin, b); err != nil {
				t.Errorf("reading stdin: %v", err)
				return
			}
			if _, err := st.stdout.Write(append(b, printed...)); err != nil {
				t.Errorf("writing stdout: %v", err)
				return
			}
			_ = st.exit(execSuccess)
		}))
		term := newExecTerminal(t, 80, 24)
		done := execStart(context.Background(), f.s, f.pod("web"), term.options("sh", "-c", "exec sh"))
		if _, err := term.type_.Write([]byte(typed)); err != nil {
			t.Fatal(err)
		}
		if out := receive(t, "Exec returns", done); out.err != nil || out.res != (ExecResult{Started: true}) {
			t.Fatalf("Exec: %+v, %v", out.res, out.err)
		}
		start, end := execRecords(t, f)
		if c := start.command(t); !slices.Equal(c, []string{"sh", "-c", "exec sh"}) {
			t.Errorf("command %q", c)
		}
		wantAttrs(t, start, nil, "duration", "started", "status", "error", "ended")
		wantAttrs(t, end, map[string]string{"started": "true", "status": "0"}, "command", "error", "ended")
		if _, ok := end.Attrs["duration"]; !ok {
			t.Error("the end has no duration")
		}
		noneHolds(t, f, "typed-4f1e-marker", "printed-9c2b-marker")
	})

	t.Run("a shell that ended with 7", func(t *testing.T) {
		f := execEnv(t, serveExec(t, func(st *execStreams) { _ = st.exit(execExitCode("7")) }))
		term := newExecTerminal(t, 80, 24)
		if _, err := execRun(t, context.Background(), f.s, f.pod("web"), term.options("sh")); err != nil {
			t.Fatalf("Exec: %v", err)
		}
		_, end := execRecords(t, f)
		wantAttrs(t, end, map[string]string{"started": "true", "status": "7"}, "error", "ended")
	})

	t.Run("a command that did not start", func(t *testing.T) {
		f := execEnv(t, serveExec(t, func(st *execStreams) { _ = st.exit(execInternal(execNoShell)) }))
		term := newExecTerminal(t, 80, 24)
		if _, err := execRun(t, context.Background(), f.s, f.pod("web"), term.options("sh")); !errors.Is(err, ErrExecNotStarted) {
			t.Fatalf("Exec: %v", err)
		}
		_, end := execRecords(t, f)
		wantAttrs(t, end, map[string]string{"started": "false", "error": "the command could not be started"}, "status", "ended")
		noneHolds(t, f, "executable", "$PATH", "80c33c691f99", "Internal error")
	})

	t.Run("an error after output", func(t *testing.T) {
		f := execEnv(t, serveExec(t, func(st *execStreams) {
			_, _ = st.stdout.Write([]byte(printed))
			_ = st.exit(execInternal("the runtime says " + printed))
		}))
		term := newExecTerminal(t, 80, 24)
		if _, err := execRun(t, context.Background(), f.s, f.pod("web"), term.options("sh")); err == nil {
			t.Fatal("no error")
		}
		_, end := execRecords(t, f)
		wantAttrs(t, end, map[string]string{"started": "true", "error": "exec failed"}, "status", "ended")
		noneHolds(t, f, "printed-9c2b-marker")
	})

	t.Run("refused by the API", func(t *testing.T) {
		f := execEnv(t, apiRefuses(t, apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "web", errors.New("a webhook says printed-9c2b-marker"))))
		term := newExecTerminal(t, 80, 24)
		if _, err := execRun(t, context.Background(), f.s, f.pod("web"), term.options("sh")); !apierrors.IsForbidden(err) {
			t.Fatalf("Exec: %v", err)
		}
		_, end := execRecords(t, f)
		wantAttrs(t, end, map[string]string{"started": "false", "error": "the API answered Forbidden (403)"}, "status", "ended")
		noneHolds(t, f, "printed-9c2b-marker")
	})

	for _, by := range []string{"the caller", "the session"} {
		t.Run("ended by "+by, func(t *testing.T) {
			open := make(chan struct{}, 1)
			f := execEnv(t, execHolds(t, open))
			term := newExecTerminal(t, 80, 24)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := execStart(ctx, f.s, f.pod("web"), term.options("sh"))
			receive(t, "the stream is open", open)
			term.out.waitFor(t, "$ ")
			if by == "the caller" {
				cancel()
			} else {
				f.end()
			}
			receive(t, "Exec returns", done)
			_, end := execRecords(t, f)
			if by == "the caller" {
				wantAttrs(t, end, map[string]string{"started": "true", "error": "context canceled"}, "status", "ended")
			} else {
				wantAttrs(t, end, map[string]string{"started": "true", "ended": "the session ended"}, "status", "error")
			}
		})
	}

	t.Run("the command is cleaned and cut", func(t *testing.T) {
		f := execEnv(t, execSucceeds(t))
		term := newExecTerminal(t, 80, 24)
		long := strings.Repeat("x", 4096)
		if _, err := execRun(t, context.Background(), f.s, f.pod("web"), term.options("sh", "-c", "echo \x1b[31mred\x07\r\n\u009b2J\xff", long, "after")); err != nil {
			t.Fatalf("Exec: %v", err)
		}
		start, _ := execRecords(t, f)
		c := start.command(t)
		if len(c) != 4 || c[0] != "sh" || c[1] != "-c" || c[2] != "echo [31mred2J\ufffd" {
			t.Fatalf("command %q", c)
		}
		total := 0
		for _, w := range c {
			total += len(w)
		}
		if total != 1024 || c[3] != long[:1024-len(c[0])-len(c[1])-len(c[2])] {
			t.Errorf("the command's words are %d bytes, want 1024: %q", total, c)
		}
	})
}
