package kube

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"sync"
	"time"
	"unicode/utf8"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
)

const (
	// logMaxLine is the longest line of a log, in bytes, that is
	// delivered whole (Review Focus 4). A longer one is cut and marked
	// with logCut.
	logMaxLine = 64 << 10
	logCut     = "…"
	// A container's lines are delivered when logBatchLines are in, when
	// they hold logBatchBytes of text, or logBatchWait after the first
	// of them, whichever is first: a batch holds at most logBatchBytes
	// and one line.
	logBatchLines = 256
	logBatchBytes = 1 << 20
	logBatchWait  = 50 * time.Millisecond
)

// logSource is one container whose log is read.
type logSource struct {
	pod, container string
}

// Logs reads the log of a pod, or of every container of every pod a
// workload or service selects now. It reads at most LogMaxContainers
// containers, of a workload its pods' containers before their ephemeral
// and init containers; the stream ends with Close, with the session's context
// and with the Cluster.
func (s *session) Logs(ctx context.Context, ref ObjectRef, opts LogOptions) (LogStream, error) {
	if err := checkRef(ref); err != nil {
		return nil, err
	}
	var (
		namespace string
		sources   []logSource
		previous  bool
	)
	switch {
	case ref.Resource.Is("", "pods"):
		p, err := s.c.typed.CoreV1().Pods(ref.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		namespace, previous = p.Namespace, opts.Previous
		switch {
		case opts.AllContainers:
			sources = containersOf(p)
		case opts.Container != "":
			sources = []logSource{{pod: p.Name, container: opts.Container}}
		case len(p.Spec.Containers) > 0:
			sources = []logSource{{pod: p.Name, container: p.Spec.Containers[0].Name}}
		default:
			return nil, fmt.Errorf("pod %s/%s has no containers", p.Namespace, p.Name)
		}
	case ref.Resource.Is("apps", "deployments"), ref.Resource.Is("apps", "replicasets"),
		ref.Resource.Is("apps", "statefulsets"), ref.Resource.Is("apps", "daemonsets"),
		ref.Resource.Is("batch", "jobs"), ref.Resource.Is("", "services"):
		ns, sel, err := s.c.podSelector(ctx, ref)
		if err != nil {
			return nil, err
		}
		list, err := s.c.typed.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: sel.String()})
		if err != nil {
			return nil, err
		}
		pods := list.Items
		sort.Slice(pods, func(i, j int) bool { return pods[i].Name < pods[j].Name })
		namespace = ns
		var selected []corev1.Pod
		for _, p := range pods {
			// labels.Nothing() is "" as a string, which the API reads as
			// everything.
			if sel.Matches(labels.Set(p.Labels)) {
				selected = append(selected, p)
			}
		}
		sources = workloadContainers(selected)
	default:
		return nil, fmt.Errorf("%s has no log: logs are of pods, and of the pods of a deployment, replicaset, statefulset, daemonset, job or service", ref)
	}

	skipped := 0
	if len(sources) > LogMaxContainers {
		skipped = len(sources) - LogMaxContainers
		sources = sources[:LogMaxContainers]
	}
	ls, err := s.c.startLogs(s.ctx, namespace, sources, podLogOptions(opts, previous), opts.From == LogHead, skipped)
	if err != nil {
		return nil, err
	}
	return ls, nil
}

// containersOf returns every container of a pod: its init containers,
// its containers and its ephemeral containers.
func containersOf(p *corev1.Pod) []logSource {
	var out []logSource
	for _, c := range p.Spec.InitContainers {
		out = append(out, logSource{pod: p.Name, container: c.Name})
	}
	for _, c := range p.Spec.Containers {
		out = append(out, logSource{pod: p.Name, container: c.Name})
	}
	for _, c := range p.Spec.EphemeralContainers {
		out = append(out, logSource{pod: p.Name, container: c.Name})
	}
	return out
}

// workloadContainers returns every container of pods in the order the
// limit of LogMaxContainers cuts the least interesting first: every
// pod's containers, then their ephemeral containers, then their init
// containers.
func workloadContainers(pods []corev1.Pod) []logSource {
	var main, ephemeral, init []logSource
	for _, p := range pods {
		for _, c := range p.Spec.Containers {
			main = append(main, logSource{pod: p.Name, container: c.Name})
		}
		for _, c := range p.Spec.EphemeralContainers {
			ephemeral = append(ephemeral, logSource{pod: p.Name, container: c.Name})
		}
		for _, c := range p.Spec.InitContainers {
			init = append(init, logSource{pod: p.Name, container: c.Name})
		}
	}
	return append(append(main, ephemeral...), init...)
}

// podLogOptions are the options every container's log is opened with,
// but for the container.
func podLogOptions(opts LogOptions, previous bool) corev1.PodLogOptions {
	o := corev1.PodLogOptions{
		Timestamps: true,
		Previous:   previous,
		Follow:     !previous && opts.From != LogHead,
	}
	switch opts.From {
	case LogTail:
		n := int64(LogTailLines)
		o.TailLines = &n
	case LogSince:
		// The API counts in whole seconds, at least one.
		sec := max(int64((opts.Since+time.Second-1)/time.Second), 1)
		o.SinceSeconds = &sec
	}
	return o
}

// logStream is the LogStream of some containers' logs. Each container
// has a reader, which opens its log and passes its lines on one at a
// time, and a batcher, which delivers them in batches. A goroutine of
// the stream's own closes lines once they have all ended.
type logStream struct {
	c       *Cluster
	lines   chan []LogLine
	skipped int
	cancel  context.CancelFunc
	done    chan struct{} // closed with lines

	mu     sync.Mutex
	err    error // of the first stream that broke
	head   bool  // the stream ends after budget lines
	budget int
}

// startLogs starts reading the logs of sources, each opened with opts
// and its container; with head the whole stream ends after
// LogBufferLines lines. The stream ends with Close, with ctx and with
// the Cluster.
func (c *Cluster) startLogs(ctx context.Context, namespace string, sources []logSource, opts corev1.PodLogOptions, head bool, skipped int) (*logStream, error) {
	readCtx, cancel := context.WithCancel(c.ctx)
	ls := &logStream{
		c:       c,
		lines:   make(chan []LogLine),
		skipped: skipped,
		cancel:  cancel,
		done:    make(chan struct{}),
		head:    head,
		budget:  LogBufferLines,
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		cancel()
		return nil, errClosed
	}
	c.wg.Add(1 + 2*len(sources))
	c.mu.Unlock()

	stopAfter := context.AfterFunc(ctx, cancel)
	var wg sync.WaitGroup
	wg.Add(2 * len(sources))
	for _, src := range sources {
		in := make(chan LogLine)
		o := opts
		o.Container = src.container
		go func() {
			defer c.wg.Done()
			defer wg.Done()
			ls.read(readCtx, namespace, src, &o, in)
		}()
		go func() {
			defer c.wg.Done()
			defer wg.Done()
			ls.batch(readCtx, in)
		}()
	}
	go func() {
		defer c.wg.Done()
		wg.Wait()
		stopAfter()
		cancel()
		close(ls.lines)
		close(ls.done)
	}()
	return ls, nil
}

func (ls *logStream) Lines() <-chan []LogLine { return ls.lines }

func (ls *logStream) Skipped() int { return ls.skipped }

func (ls *logStream) Err() error {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	return ls.err
}

// Close ends every container's reading and returns when Lines is
// closed.
func (ls *logStream) Close() {
	ls.cancel()
	<-ls.done
}

// fail records the first error of a stream that broke.
func (ls *logStream) fail(err error) {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	if ls.err == nil {
		ls.err = err
	}
}

// take returns how many of n lines may still be delivered: all of them,
// but for LogHead what is left of LogBufferLines.
func (ls *logStream) take(n int) (int, bool) {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	if !ls.head {
		return n, false
	}
	n = min(n, ls.budget)
	ls.budget -= n
	return n, ls.budget == 0
}

// read opens the log of src and passes its lines to out, which it
// closes when the log has ended. A log that cannot be opened is one
// line that says why.
func (ls *logStream) read(ctx context.Context, namespace string, src logSource, opts *corev1.PodLogOptions, out chan<- LogLine) {
	defer close(out)
	send := func(l LogLine) bool {
		l.Pod, l.Container = src.pod, src.container
		select {
		case out <- l:
			return true
		case <-ctx.Done():
			return false
		}
	}
	rc, err := ls.c.openLog(ctx, namespace, src.pod, opts)
	if err != nil {
		if ctx.Err() == nil {
			send(LogLine{Text: fmt.Sprintf("the log of container %s of pod %s cannot be read: %v", src.container, src.pod, err)})
		}
		return
	}
	// Closing the stream is what ends a read that waits for more.
	closed := make(chan struct{})
	stopClose := context.AfterFunc(ctx, func() {
		rc.Close()
		close(closed)
	})
	defer func() {
		if stopClose() {
			rc.Close()
		} else {
			<-closed
		}
	}()

	br := bufio.NewReaderSize(rc, logMaxLine+1)
	discarding := false // the rest of a line that was cut
	for {
		b, err := br.ReadSlice('\n')
		switch {
		case errors.Is(err, bufio.ErrBufferFull):
			// A line longer than logMaxLine: it is delivered cut now,
			// and what follows of it up to the next line feed is
			// dropped as it comes.
			if !discarding {
				discarding = true
				if !send(cutLine(b)) {
					return
				}
			}
			continue
		case err == nil:
			if !discarding && !send(parseLine(b[:len(b)-1])) {
				return
			}
			discarding = false
			continue
		}
		if len(b) > 0 && !discarding {
			send(parseLine(b))
		}
		if err != io.EOF && ctx.Err() == nil {
			ls.fail(err)
		}
		return
	}
}

// batch delivers what comes from in in batches: when logBatchLines are
// in, when they hold logBatchBytes, or logBatchWait after the first of
// them.
func (ls *logStream) batch(ctx context.Context, in <-chan LogLine) {
	var (
		pending []LogLine
		size    int // of the text of pending
		timer   = time.NewTimer(logBatchWait)
		wait    <-chan time.Time
	)
	timer.Stop()
	defer timer.Stop()
	flush := func() bool {
		timer.Stop()
		wait = nil
		if len(pending) == 0 {
			return true
		}
		n, last := ls.take(len(pending))
		b := pending[:n]
		pending, size = nil, 0
		if n > 0 {
			select {
			case ls.lines <- b:
			case <-ctx.Done():
				return false
			}
		}
		if last {
			// LogHead has all it keeps: the whole stream ends.
			ls.cancel()
			return false
		}
		return true
	}
	for {
		select {
		case l, ok := <-in:
			if !ok {
				flush()
				return
			}
			pending = append(pending, l)
			size += len(l.Text)
			if len(pending) == 1 {
				timer.Reset(logBatchWait)
				wait = timer.C
			}
			if (len(pending) >= logBatchLines || size >= logBatchBytes) && !flush() {
				return
			}
		case <-wait:
			if !flush() {
				return
			}
		case <-ctx.Done():
			return
		}
	}
}

// parseLine splits "<RFC 3339 time> <text>". A line without a time that
// parses is all text, with a zero time.
func parseLine(b []byte) LogLine {
	if i := bytes.IndexByte(b, ' '); i > 0 {
		if t, err := time.Parse(time.RFC3339Nano, string(b[:i])); err == nil {
			return LogLine{Time: t, Text: string(b[i+1:])}
		}
	}
	return LogLine{Text: string(b)}
}

// cutLine is the line that begins with b, which is longer than
// logMaxLine: cut so that with logCut it is logMaxLine bytes at most,
// after a whole character.
func cutLine(b []byte) LogLine {
	b = b[:logMaxLine-len(logCut)]
	// Leave out a character that the cut split.
	for i := 1; i <= utf8.UTFMax && i <= len(b); i++ {
		if utf8.RuneStart(b[len(b)-i]) {
			if !utf8.FullRune(b[len(b)-i:]) {
				b = b[:len(b)-i]
			}
			break
		}
	}
	l := parseLine(b)
	l.Text += logCut
	return l
}
