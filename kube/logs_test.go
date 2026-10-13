package kube

import (
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
)

// readStream is a log stream of the tests: it reads from r, and
// remembers that it was closed. When block is set it does not end at the
// end of r, but waits to be closed, as a followed log does.
type readStream struct {
	r     io.Reader
	block bool

	closeOnce sync.Once
	closedCh  chan struct{}
	closed    atomic.Bool
}

func readNewStream(r io.Reader, block bool) *readStream {
	return &readStream{r: r, block: block, closedCh: make(chan struct{})}
}

func (s *readStream) Read(p []byte) (int, error) {
	if s.closed.Load() {
		return 0, errors.New("read of a closed stream")
	}
	n, err := s.r.Read(p)
	if err == io.EOF && s.block {
		if n > 0 {
			return n, nil
		}
		<-s.closedCh
		return 0, errors.New("read of a closed stream")
	}
	return n, err
}

func (s *readStream) Close() error {
	s.closeOnce.Do(func() {
		s.closed.Store(true)
		close(s.closedCh)
		if c, ok := s.r.(io.Closer); ok {
			c.Close()
		}
	})
	return nil
}

// readOpened is one call of the opener.
type readOpened struct {
	namespace, pod string
	opts           corev1.PodLogOptions
}

// readFakeLogs is the opener of the tests. give returns the stream of a
// container, or the error of opening it; nil gives an empty stream.
type readFakeLogs struct {
	give func(pod, container string) (io.ReadCloser, error)

	mu      sync.Mutex
	opened  []readOpened
	streams []*readStream
}

func (f *readFakeLogs) open(_ context.Context, namespace, pod string, opts *corev1.PodLogOptions) (io.ReadCloser, error) {
	f.mu.Lock()
	f.opened = append(f.opened, readOpened{namespace: namespace, pod: pod, opts: *opts})
	f.mu.Unlock()
	var (
		rc  io.ReadCloser
		err error
	)
	if f.give != nil {
		rc, err = f.give(pod, opts.Container)
	}
	if err != nil {
		return nil, err
	}
	if rc == nil {
		rc = readNewStream(strings.NewReader(""), false)
	}
	if s, ok := rc.(*readStream); ok {
		f.mu.Lock()
		f.streams = append(f.streams, s)
		f.mu.Unlock()
	}
	return rc, nil
}

func (f *readFakeLogs) calls() []readOpened {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]readOpened(nil), f.opened...)
}

func (f *readFakeLogs) allClosed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range f.streams {
		if !s.closed.Load() {
			return false
		}
	}
	return true
}

// readLogEnv is a Cluster on the fakes whose logs come from f.
func readLogEnv(t *testing.T, f *readFakeLogs, objs ...k8sruntime.Object) *env {
	t.Helper()
	e := newEnv(t, envConfig{}, objs...)
	e.c.openLog = f.open
	return e
}

// readCollect receives every batch until Lines is closed, at most 5
// seconds.
func readCollect(t *testing.T, ls LogStream) [][]LogLine {
	t.Helper()
	var batches [][]LogLine
	deadline := time.After(5 * time.Second)
	for {
		select {
		case b, ok := <-ls.Lines():
			if !ok {
				return batches
			}
			batches = append(batches, b)
		case <-deadline:
			t.Fatalf("Lines is not closed within 5s; %d batches so far", len(batches))
		}
	}
}

func readFlatten(batches [][]LogLine) []LogLine {
	var out []LogLine
	for _, b := range batches {
		out = append(out, b...)
	}
	return out
}

// readNext waits for the next batch, at most d.
func readNext(t *testing.T, ls LogStream, d time.Duration) []LogLine {
	t.Helper()
	select {
	case b, ok := <-ls.Lines():
		if !ok {
			t.Fatal("Lines is closed")
		}
		return b
	case <-time.After(d):
		t.Fatalf("no lines within %v", d)
	}
	return nil
}

func readPodWith(namespace, name string, labels map[string]string, containers ...string) *corev1.Pod {
	p := pod(namespace, name, labels)
	for _, c := range containers {
		p.Spec.Containers = append(p.Spec.Containers, corev1.Container{Name: c, Image: c + ":1"})
	}
	return p
}

func readLogs(t *testing.T, e *env, ref ObjectRef, opts LogOptions) LogStream {
	t.Helper()
	ls, err := e.session(t).Logs(context.Background(), ref, opts)
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	t.Cleanup(ls.Close)
	return ls
}

func readInt64p(n int64) *int64 { return &n }

func TestLogsOfAPod(t *testing.T) {
	p := readPodWith("default", "web", nil, "app", "sidecar")
	p.Spec.InitContainers = []corev1.Container{{Name: "init"}}
	p.Spec.EphemeralContainers = []corev1.EphemeralContainer{{EphemeralContainerCommon: corev1.EphemeralContainerCommon{Name: "debug"}}}

	for _, tc := range []struct {
		name string
		opts LogOptions
		want []corev1.PodLogOptions
	}{
		{"tail", LogOptions{}, []corev1.PodLogOptions{
			{Container: "app", Follow: true, Timestamps: true, TailLines: readInt64p(LogTailLines)}}},
		{"head", LogOptions{From: LogHead}, []corev1.PodLogOptions{
			{Container: "app", Timestamps: true}}},
		{"since", LogOptions{From: LogSince, Since: 5 * time.Minute}, []corev1.PodLogOptions{
			{Container: "app", Follow: true, Timestamps: true, SinceSeconds: readInt64p(300)}}},
		{"previous", LogOptions{Previous: true}, []corev1.PodLogOptions{
			{Container: "app", Previous: true, Timestamps: true, TailLines: readInt64p(LogTailLines)}}},
		{"a named container", LogOptions{Container: "sidecar"}, []corev1.PodLogOptions{
			{Container: "sidecar", Follow: true, Timestamps: true, TailLines: readInt64p(LogTailLines)}}},
		{"all containers", LogOptions{AllContainers: true, From: LogHead}, []corev1.PodLogOptions{
			{Container: "init", Timestamps: true},
			{Container: "app", Timestamps: true},
			{Container: "sidecar", Timestamps: true},
			{Container: "debug", Timestamps: true}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &readFakeLogs{}
			e := readLogEnv(t, f, p)
			ls := readLogs(t, e, ObjectRef{Resource: e.resource(t, "", "pods"), Namespace: "default", Name: "web"}, tc.opts)
			readCollect(t, ls)
			calls := f.calls()
			var got []corev1.PodLogOptions
			for _, c := range calls {
				if c.namespace != "default" || c.pod != "web" {
					t.Errorf("opened %s/%s, want default/web", c.namespace, c.pod)
				}
				got = append(got, c.opts)
			}
			order := map[string]int{"init": 0, "app": 1, "sidecar": 2, "debug": 3}
			sort.Slice(got, func(i, j int) bool { return order[got[i].Container] < order[got[j].Container] })
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("opened with\n%+v\nwant\n%+v", got, tc.want)
			}
			if ls.Skipped() != 0 || ls.Err() != nil {
				t.Errorf("skipped %d, err %v", ls.Skipped(), ls.Err())
			}
		})
	}

	t.Run("a pod that is not there", func(t *testing.T) {
		e := readLogEnv(t, &readFakeLogs{})
		if _, err := e.session(t).Logs(context.Background(), ObjectRef{Resource: e.resource(t, "", "pods"), Namespace: "default", Name: "web"}, LogOptions{}); err == nil {
			t.Error("no error")
		}
	})
}

func TestLogLinesAreParsed(t *testing.T) {
	f := &readFakeLogs{give: func(_, _ string) (io.ReadCloser, error) {
		return readNewStream(strings.NewReader(
			"2026-10-13T09:00:00.123456789Z hello world\n"+
				"no time here\n"+
				"2026-10-13T09:00:01Z \n"+
				"\n"+
				"13 October text\n"+
				"2026-10-13T09:00:02+02:00 the last, without a line feed"), false), nil
	}}
	e := readLogEnv(t, f, readPodWith("default", "web", nil, "app"))
	ls := readLogs(t, e, ObjectRef{Resource: e.resource(t, "", "pods"), Namespace: "default", Name: "web"}, LogOptions{})
	got := readFlatten(readCollect(t, ls))
	at := func(s string) time.Time {
		ts, err := time.Parse(time.RFC3339Nano, s)
		if err != nil {
			t.Fatal(err)
		}
		return ts
	}
	want := []LogLine{
		{Pod: "web", Container: "app", Time: at("2026-10-13T09:00:00.123456789Z"), Text: "hello world"},
		{Pod: "web", Container: "app", Text: "no time here"},
		{Pod: "web", Container: "app", Time: at("2026-10-13T09:00:01Z"), Text: ""},
		{Pod: "web", Container: "app", Text: ""},
		{Pod: "web", Container: "app", Text: "13 October text"},
		{Pod: "web", Container: "app", Time: at("2026-10-13T09:00:02+02:00"), Text: "the last, without a line feed"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d lines, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i].Pod != want[i].Pod || got[i].Container != want[i].Container || !got[i].Time.Equal(want[i].Time) || got[i].Text != want[i].Text {
			t.Errorf("line %d: %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestLogLongLineIsCut(t *testing.T) {
	long := strings.Repeat("x", 1<<20)
	f := &readFakeLogs{give: func(_, _ string) (io.ReadCloser, error) {
		return readNewStream(strings.NewReader(long+"\nafter\n"), false), nil
	}}
	e := readLogEnv(t, f, readPodWith("default", "web", nil, "app"))
	ls := readLogs(t, e, ObjectRef{Resource: e.resource(t, "", "pods"), Namespace: "default", Name: "web"}, LogOptions{})
	got := readFlatten(readCollect(t, ls))
	if len(got) != 2 {
		t.Fatalf("got %d lines, want 2", len(got))
	}
	if len(got[0].Text) != 64<<10 || !strings.HasSuffix(got[0].Text, "…") || !strings.HasPrefix(got[0].Text, "xxxx") {
		t.Errorf("the long line: %d bytes ending in %q, want 64 KiB ending in …", len(got[0].Text), got[0].Text[max(0, len(got[0].Text)-8):])
	}
	if got[1].Text != "after" {
		t.Errorf("the line after it: %q, want it whole", got[1].Text)
	}
}

func TestLogLineCutKeepsRunesWhole(t *testing.T) {
	long := strings.Repeat("ü", 1<<16) // two bytes each: the cut falls in one
	f := &readFakeLogs{give: func(_, _ string) (io.ReadCloser, error) {
		return readNewStream(strings.NewReader(long+"\n"), false), nil
	}}
	e := readLogEnv(t, f, readPodWith("default", "web", nil, "app"))
	ls := readLogs(t, e, ObjectRef{Resource: e.resource(t, "", "pods"), Namespace: "default", Name: "web"}, LogOptions{})
	got := readFlatten(readCollect(t, ls))
	if len(got) != 1 || len(got[0].Text) > 64<<10 || !strings.HasSuffix(got[0].Text, "ü…") {
		t.Errorf("got %d lines; the first of %d bytes, want one cut after a whole rune", len(got), len(got[0].Text))
	}
}

// readEndless produces size bytes without a line feed as they are read,
// then waits to be closed.
type readEndless struct {
	size     int64
	produced atomic.Int64
}

func (r *readEndless) Read(p []byte) (int, error) {
	left := r.size - r.produced.Load()
	if left <= 0 {
		return 0, io.EOF
	}
	n := min(int64(len(p)), left)
	for i := range p[:n] {
		p[i] = 'x'
	}
	r.produced.Add(n)
	return int(n), nil
}

func TestLogWithoutLineFeedIsBounded(t *testing.T) {
	const size = 8 << 20
	src := &readEndless{size: size}
	f := &readFakeLogs{give: func(_, _ string) (io.ReadCloser, error) {
		return readNewStream(src, true), nil
	}}
	e := readLogEnv(t, f, readPodWith("default", "web", nil, "app"))
	ref := ObjectRef{Resource: e.resource(t, "", "pods"), Namespace: "default", Name: "web"}

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)

	ls := readLogs(t, e, ref, LogOptions{})
	b := readNext(t, ls, 5*time.Second)
	if len(b) != 1 || len(b[0].Text) != 64<<10 || !strings.HasSuffix(b[0].Text, "…") {
		t.Fatalf("got %d lines, the first of %d bytes; want one line cut at 64 KiB", len(b), len(b[0].Text))
	}
	if f.allClosed() {
		t.Fatal("the cut line came only when the stream was closed")
	}
	eventually(t, "the whole stream is read", func() bool { return src.produced.Load() == size })
	select {
	case more, ok := <-ls.Lines():
		t.Fatalf("after the cut line: %d lines more, open %v; want nothing until a line feed", len(more), ok)
	default:
	}

	runtime.GC()
	runtime.ReadMemStats(&after)
	if grown := int64(after.HeapAlloc) - int64(before.HeapAlloc); grown > 1<<20 {
		t.Errorf("the heap grew by %d bytes reading %d bytes without a line feed, want at most 1 MiB", grown, size)
	}
	runtime.KeepAlive(b)
}

func TestLogBatches(t *testing.T) {
	pr, pw := io.Pipe()
	defer pw.Close()
	f := &readFakeLogs{give: func(_, _ string) (io.ReadCloser, error) {
		return readNewStream(pr, false), nil
	}}
	e := readLogEnv(t, f, readPodWith("default", "web", nil, "app"))
	ls := readLogs(t, e, ObjectRef{Resource: e.resource(t, "", "pods"), Namespace: "default", Name: "web"}, LogOptions{})

	var text strings.Builder
	for i := range 1000 {
		fmt.Fprintf(&text, "line %d\n", i)
	}
	go pw.Write([]byte(text.String()))
	n, batches := 0, 0
	for n < 1000 {
		b := readNext(t, ls, 5*time.Second)
		if len(b) > 256 {
			t.Errorf("a batch of %d lines, want at most 256", len(b))
		}
		for i, l := range b {
			if want := fmt.Sprintf("line %d", n+i); l.Text != want {
				t.Fatalf("line %q, want %q", l.Text, want)
			}
		}
		n += len(b)
		batches++
	}
	if n != 1000 || batches < 4 {
		t.Errorf("%d lines in %d batches, want 1000 in at least 4", n, batches)
	}

	start := time.Now()
	go pw.Write([]byte("one more\n"))
	b := readNext(t, ls, time.Second)
	if took := time.Since(start); took > 200*time.Millisecond || len(b) != 1 || b[0].Text != "one more" {
		t.Errorf("a single line: %+v after %v, want it within 200ms", b, took)
	}
}

func TestLogHeadEnds(t *testing.T) {
	var text strings.Builder
	for i := range 6000 {
		fmt.Fprintf(&text, "line %d\n", i)
	}
	f := &readFakeLogs{give: func(_, _ string) (io.ReadCloser, error) {
		// It does not end by itself.
		return readNewStream(strings.NewReader(text.String()), true), nil
	}}
	e := readLogEnv(t, f, readPodWith("default", "web", nil, "app"))
	ls := readLogs(t, e, ObjectRef{Resource: e.resource(t, "", "pods"), Namespace: "default", Name: "web"}, LogOptions{From: LogHead})
	got := readFlatten(readCollect(t, ls))
	if len(got) != LogBufferLines || got[len(got)-1].Text != "line 4999" {
		t.Errorf("%d lines, want %d", len(got), LogBufferLines)
	}
	if ls.Err() != nil {
		t.Errorf("Err %v, want nil", ls.Err())
	}
	if !f.allClosed() {
		t.Error("the stream is not closed")
	}
}

func readDeployment(namespace, name, app string) *appsv1.Deployment {
	return &appsv1.Deployment{ObjectMeta: objMeta(namespace, name), Spec: appsv1.DeploymentSpec{Selector: selector(app)}}
}

func TestLogsOfAWorkload(t *testing.T) {
	f := &readFakeLogs{give: func(pod, container string) (io.ReadCloser, error) {
		return readNewStream(strings.NewReader("from "+pod+"/"+container+"\n"), false), nil
	}}
	e := readLogEnv(t, f,
		readDeployment("default", "web", "web"),
		readPodWith("default", "web-1", map[string]string{"app": "web"}, "app", "sidecar"),
		readPodWith("default", "web-2", map[string]string{"app": "web"}, "app", "sidecar"),
		readPodWith("default", "db", map[string]string{"app": "db"}, "db"),
		readPodWith("other", "web-3", map[string]string{"app": "web"}, "app"),
	)
	ref := ObjectRef{Resource: e.resource(t, "apps", "deployments"), Namespace: "default", Name: "web"}
	// Container and Previous are for a pod.
	ls := readLogs(t, e, ref, LogOptions{Container: "sidecar", Previous: true})
	got := readFlatten(readCollect(t, ls))

	var opened []string
	for _, c := range f.calls() {
		opened = append(opened, c.namespace+"/"+c.pod+"/"+c.opts.Container)
		if c.opts.Previous || !c.opts.Follow {
			t.Errorf("opened %+v: Previous of a workload's log", c.opts)
		}
	}
	sort.Strings(opened)
	if want := []string{"default/web-1/app", "default/web-1/sidecar", "default/web-2/app", "default/web-2/sidecar"}; !reflect.DeepEqual(opened, want) {
		t.Errorf("opened %v, want %v", opened, want)
	}
	if len(got) != 4 {
		t.Fatalf("%d lines, want 4: %+v", len(got), got)
	}
	for _, l := range got {
		if l.Text != "from "+l.Pod+"/"+l.Container {
			t.Errorf("line %q is said to be of %s/%s", l.Text, l.Pod, l.Container)
		}
	}
}

func TestLogsSkipBeyondTheLimit(t *testing.T) {
	objs := []k8sruntime.Object{readDeployment("default", "web", "web")}
	for i := range 20 {
		objs = append(objs, readPodWith("default", fmt.Sprintf("web-%02d", i), map[string]string{"app": "web"}, "a", "b"))
	}
	f := &readFakeLogs{}
	e := readLogEnv(t, f, objs...)
	ls := readLogs(t, e, ObjectRef{Resource: e.resource(t, "apps", "deployments"), Namespace: "default", Name: "web"}, LogOptions{})
	readCollect(t, ls)
	calls := f.calls()
	if len(calls) != LogMaxContainers || ls.Skipped() != 10 {
		t.Errorf("%d opened, %d skipped; want 30 and 10", len(calls), ls.Skipped())
	}
	for _, c := range calls {
		if c.pod >= "web-15" {
			t.Errorf("opened %s/%s, which is beyond the first 30", c.pod, c.opts.Container)
		}
	}
}

func TestLogContainerThatCannotBeOpened(t *testing.T) {
	f := &readFakeLogs{give: func(pod, container string) (io.ReadCloser, error) {
		if container == "a" {
			return nil, errors.New(`container "a" in pod "web" is waiting to start: ContainerCreating`)
		}
		return readNewStream(strings.NewReader("hello from b\n"), false), nil
	}}
	e := readLogEnv(t, f, readPodWith("default", "web", nil, "a", "b"))
	ls := readLogs(t, e, ObjectRef{Resource: e.resource(t, "", "pods"), Namespace: "default", Name: "web"}, LogOptions{AllContainers: true})
	got := readFlatten(readCollect(t, ls))
	var fromA, fromB []LogLine
	for _, l := range got {
		switch l.Container {
		case "a":
			fromA = append(fromA, l)
		case "b":
			fromB = append(fromB, l)
		}
	}
	if len(fromA) != 1 || !strings.Contains(fromA[0].Text, "is waiting to start: ContainerCreating") ||
		!strings.Contains(fromA[0].Text, "web") || fromA[0].Pod != "web" {
		t.Errorf("of a: %+v, want one line with the error", fromA)
	}
	if len(fromB) != 1 || fromB[0].Text != "hello from b" {
		t.Errorf("of b: %+v, want its line", fromB)
	}
	if ls.Err() != nil {
		t.Errorf("Err %v, want nil", ls.Err())
	}
}

// readBreaking gives its text, then fails.
type readBreaking struct {
	text string
	err  error
}

func (r *readBreaking) Read(p []byte) (int, error) {
	if r.text == "" {
		return 0, r.err
	}
	n := copy(p, r.text)
	r.text = r.text[n:]
	return n, nil
}

func TestLogStreamBreaks(t *testing.T) {
	broken := errors.New("connection reset by peer")
	f := &readFakeLogs{give: func(_, _ string) (io.ReadCloser, error) {
		return readNewStream(&readBreaking{text: "one\ntwo\n", err: broken}, false), nil
	}}
	e := readLogEnv(t, f, readPodWith("default", "web", nil, "app"))
	ls := readLogs(t, e, ObjectRef{Resource: e.resource(t, "", "pods"), Namespace: "default", Name: "web"}, LogOptions{})
	got := readFlatten(readCollect(t, ls))
	if len(got) != 2 || got[0].Text != "one" || got[1].Text != "two" {
		t.Errorf("lines %+v, want one and two", got)
	}
	if !errors.Is(ls.Err(), broken) {
		t.Errorf("Err %v, want %v", ls.Err(), broken)
	}
}

func TestLogClose(t *testing.T) {
	for _, how := range []string{"Close", "the session's end"} {
		t.Run(how, func(t *testing.T) {
			f := &readFakeLogs{give: func(_, _ string) (io.ReadCloser, error) {
				pr, _ := io.Pipe() // nothing is ever written
				return readNewStream(pr, false), nil
			}}
			e := readLogEnv(t, f, readPodWith("default", "web", nil, "a", "b"))
			before := runtime.NumGoroutine()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			s := e.c.Session(ctx, fullGrant)
			ls, err := s.Logs(context.Background(), ObjectRef{Resource: e.resource(t, "", "pods"), Namespace: "default", Name: "web"}, LogOptions{AllContainers: true})
			if err != nil {
				t.Fatal(err)
			}
			eventually(t, "both logs are opened", func() bool { return len(f.calls()) == 2 })

			if how == "Close" {
				closed := make(chan struct{})
				go func() {
					ls.Close()
					ls.Close()
					close(closed)
				}()
				select {
				case <-closed:
				case <-time.After(5 * time.Second):
					t.Fatal("Close has not returned within 5s")
				}
			} else {
				cancel()
			}
			readCollect(t, ls)
			eventually(t, "every stream is closed", f.allClosed)
			if ls.Err() != nil {
				t.Errorf("Err %v, want nil", ls.Err())
			}
			eventually(t, "the goroutines of the stream are gone", func() bool {
				return runtime.NumGoroutine() <= before
			})
			ls.Close()
		})
	}
}

func TestLogsOfAnotherKind(t *testing.T) {
	f := &readFakeLogs{}
	e := readLogEnv(t, f, readConfigMap())
	_, err := e.session(t).Logs(context.Background(), ObjectRef{Resource: e.resource(t, "", "configmaps"), Namespace: "default", Name: "settings"}, LogOptions{})
	if err == nil {
		t.Error("the log of a configmap: no error")
	}
	if len(f.calls()) != 0 {
		t.Errorf("opened %+v", f.calls())
	}
}
