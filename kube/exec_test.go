package kube

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
	req    *http.Request
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

// The statuses of the error channel, as the API sends them.
var (
	execSuccess = metav1.Status{Status: metav1.StatusSuccess}
)

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
		run(&execStreams{req: r, stdin: streams[0], stdout: streams[1], sizes: sizes, errors: streams[3]})
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
