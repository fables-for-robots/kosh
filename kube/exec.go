package kube

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/remotecommand"
	utilexec "k8s.io/client-go/util/exec"
	"k8s.io/streaming/pkg/httpstream"
)

const (
	// execMaxReason is the longest reason, in bytes, that
	// ErrExecNotStarted is wrapped around.
	execMaxReason = 1 << 10
	// execMaxLogged is the most the log holds, in bytes, of the
	// container's name and of the command's words together.
	execMaxLogged = 1 << 10
)

// Exec begins as every action does; checks the pod's reference, the
// container's name and the command; and runs the command on the API's
// exec stream with a terminal, over WebSocket alone: kubectl's fallback
// to SPDY would turn the API's typed refusals into plain text.
//
// It is logged twice: when it starts, with the container and the
// command, and when it ends, with how. Nothing of what passes on the
// terminal is logged. A call that the checks refuse is logged once, as
// the action's failure.
//
// The ctx Exec is given is its session's turn, which ends only when the
// session does: an end of the stream by ctx is logged as
// ended="the session ended", as an end by the session's own context
// is. What Exec returns still tells them apart: the context's error,
// with the session's end as its cause when it was that.
//
// When ctx or the session ends, the stream is closed at once, but Exec
// returns only once a write to opts.Stdout that is under way has
// returned: it waits for that write, so that nothing of the command's
// output reaches the terminal after Exec returned. That wait is bounded
// by the terminal's owner, not by Exec: the write ends when the client
// reads again or its connection closes, which at a session's end is
// after the end's timeout at the latest. Until then Exec has not
// returned.
func (s *session) Exec(ctx context.Context, pod ObjectRef, opts ExecOptions) (ExecResult, error) {
	ctx, release, err := s.begin(ctx, actExec, pod)
	if err != nil {
		return ExecResult{}, err
	}
	defer release()
	container := slog.String("container", logText(opts.Container, execMaxLogged))
	if err := s.checkExec(pod, opts); err != nil {
		s.logAction(ctx, actExec, pod, err, slog.String("phase", "end"), container, slog.Bool("started", false))
		return ExecResult{}, err
	}
	return s.execWith(ctx, pod, opts, newExecTerm(opts), s.stream)
}

// execStreamFunc runs the exec stream of a pod's container with the
// command until it ends or ctx does: session.stream, or a test's.
type execStreamFunc func(ctx context.Context, pod ObjectRef, container string, command []string, opts remotecommand.StreamOptions) error

// execWith is Exec once its checks have passed: it logs the start, runs
// stream on term until it ends or ctx does, lets the terminal go, and
// logs the end. The stream runs on a context of its own (streamContext).
// A panic under the stream is logged as the end, with panicked=true and
// none of its value, after the terminal was let go, and goes on as it
// was.
func (s *session) execWith(ctx context.Context, pod ObjectRef, opts ExecOptions, term *execTerm, stream execStreamFunc) (res ExecResult, err error) {
	container := slog.String("container", logText(opts.Container, execMaxLogged))
	s.logAction(ctx, actExec, pod, nil, slog.String("phase", "start"), container,
		slog.String("command", logCommand(opts.Command, execMaxLogged)))
	began := time.Now()
	sctx, stop := streamContext(ctx)
	term.sizes = execSizes(sctx, opts)
	returned := false
	defer func() {
		stop()
		term.letGo()
		attrs := []slog.Attr{slog.String("phase", "end"), container, slog.Duration("duration", time.Since(began))}
		switch {
		case !returned:
			attrs = append(attrs, slog.Bool("started", term.out.written() > 0),
				slog.String("error", actExec+" failed"), slog.Bool("panicked", true))
		case err == nil:
			attrs = append(attrs, slog.Bool("started", res.Started), slog.Int("status", res.Status))
		default:
			attrs = append(attrs, slog.Bool("started", res.Started))
			attrs = append(attrs, execLogged(err, ctx.Err() != nil)...)
		}
		s.logAction(ctx, actExec, pod, nil, attrs...)
	}()
	so := remotecommand.StreamOptions{Stdout: term.out, Tty: true}
	if opts.Stdin != nil {
		so.Stdin = term.in
	}
	if term.sizes != nil {
		// Not a nil *execSizeQueue in the interface: client-go would ask
		// it.
		so.TerminalSizeQueue = term.sizes
	}
	serr := stream(sctx, pod, opts.Container, opts.Command, so)
	term.letGo()
	res, err = execOutcome(ctx, serr, term.out.written())
	returned = true
	return res, err
}

// streamContext returns the context the stream runs on: made from
// context.Background(), not from ctx, so that it holds none of ctx's
// values, deadline or parents, which a stream that client-go leaves
// behind would otherwise keep alive with it (see execTerm). It is
// cancelled, with ctx's cause, when ctx is done, and by the function
// returned, which the caller defers. What Exec returns and logs is
// still decided by ctx.
func streamContext(ctx context.Context) (context.Context, func()) {
	sctx, cancel := context.WithCancelCause(context.Background())
	stopAfter := context.AfterFunc(ctx, func() { cancel(context.Cause(ctx)) })
	return sctx, func() {
		stopAfter()
		cancel(nil)
	}
}

// execLogged is how the end of an Exec logs its error, in words that
// hold nothing of the error's text, which may be a webhook's or the
// runtime's and quote the command. An end by the context (byContext:
// the action's context was done, and err is the context's) is
// ended="the session ended", whether the caller's context or the
// session's own ended first: in kosh the caller is a session's turn,
// whose context ends with the session, and whoever revokes a key looks
// for this line. ErrExecNotStarted is logged as itself, without the
// runtime's reason; an API error as its reason and code; any other as
// "exec failed".
func execLogged(err error, byContext bool) []slog.Attr {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ErrExecNotStarted):
		return []slog.Attr{slog.String("error", ErrExecNotStarted.Error())}
	case byContext && (errors.Is(err, errSessionEnded) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)):
		return []slog.Attr{slog.String("ended", "the session ended")}
	}
	return []slog.Attr{slog.String("error", loggableError(actExec, err).Error())}
}

// logCommand returns the command as the log holds it: each word
// cleaned by logText and quoted as Go quotes a string, the words joined
// by one space, so that they stay apart whatever they hold:
// "sh" "-c" "a b". A command that would be longer than max bytes is
// cut: the last word that is there is cut before it is quoted, after a
// whole character, so that its quoted form and the mark fit; its quote
// is closed, and the mark stands after it, outside any word:
// "sh" "-c" "xxxx" …. No escape is cut in half, and a cut is not a word
// that ends in the mark.
func logCommand(words []string, max int) string {
	quoted := make([]string, len(words))
	for i, w := range words {
		quoted[i] = strconv.Quote(logText(w, len(w)))
	}
	if all := strings.Join(quoted, " "); len(all) <= max {
		return all
	}
	const mark = " " + logCut
	budget := max - len(mark)
	var b strings.Builder
	for i, q := range quoted {
		sep := ""
		if i > 0 {
			sep = " "
		}
		if b.Len()+len(sep)+len(q) <= budget {
			b.WriteString(sep + q)
			continue
		}
		// The longest start of the word whose quoted form fits.
		room := budget - b.Len() - len(sep) - 2 // the two quotes
		var part strings.Builder
		for _, r := range logText(words[i], len(words[i])) {
			e := strconv.Quote(string(r))
			e = e[1 : len(e)-1] // as it stands inside the quotes
			if len(e) > room {
				break
			}
			part.WriteString(e)
			room -= len(e)
		}
		if part.Len() > 0 {
			b.WriteString(sep + `"` + part.String() + `"`)
		}
		break
	}
	return b.String() + mark
}

// logText returns s as the log holds it: without control characters,
// with U+FFFD for what is not UTF-8, cut to max bytes after a whole
// character.
func logText(s string, max int) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
	return cutText(s, max)
}

// checkExec refuses what Exec cannot ask the API for: a reference that
// is not a pod's, or not one the Cluster knows; a container's name that
// is not a DNS-1123 label; and a command of no words. The words are
// not looked at otherwise: they are the container's runtime's.
func (s *session) checkExec(pod ObjectRef, opts ExecOptions) error {
	if err := s.c.checkRef(pod); err != nil {
		return err
	}
	if err := kindIs("exec in", pod, podKinds...); err != nil {
		return err
	}
	if c := opts.Container; c != "" {
		if len(c) > validation.DNS1123LabelMaxLength {
			return fmt.Errorf("a name of %d bytes is no container's: at most %d", len(c), validation.DNS1123LabelMaxLength)
		}
		if len(validation.IsDNS1123Label(c)) > 0 {
			return fmt.Errorf("%q is not the name of a container", c)
		}
	}
	if len(opts.Command) == 0 {
		return errors.New("there is no command to run")
	}
	if s.clients.cfg == nil {
		return errors.New("the cluster has no API to run a command through")
	}
	return nil
}

// execTerm is the terminal as the stream is given it: the caller's
// input, output and sizes, each behind a guard that Exec closes before
// it returns. execWith makes the size queue, on the stream's context.
//
// What the stream still runs after that must hold nothing of the
// session. It usually ends soon: client-go's heartbeat at its next
// beat, its stdin copier with the caller's reader. But client-go
// v0.37.1 has a race that leaves the stream's goroutines blocked for
// good. In remotecommand's WebSocket executor (websocket.go), when the
// context ends between the upgrade and the moment the read loop has set
// its first read deadline (microseconds, as the stream opens), the
// executor has returned and closed the connection, readDemuxLoop fails
// to set the deadline on it and returns without closeAllStreamReaders,
// and the error stream's reader, the stdout copier, the stdin waiter and
// the stream's own goroutine wait on their pipes forever: four
// goroutines, their pipes and a buffer of 32 KiB, for each time it
// happens. It wants a fix upstream. Until then, nothing they hold is
// the session's: letGo drops what the guards hold of it (its reader,
// its writer, its size), and the context they run on is made from
// context.Background() (streamContext), not from the session's turn,
// so that it keeps neither the session's context chain nor what hangs
// from it. What stays is client-go's memory, the executor's REST
// configuration (the process's), and that context.
type execTerm struct {
	in    *execInput
	out   *execOutput
	sizes *execSizeQueue // nil without a Size
}

// newExecTerm returns the terminal of opts, without its size queue.
func newExecTerm(opts ExecOptions) *execTerm {
	return &execTerm{in: &execInput{r: opts.Stdin}, out: &execOutput{w: opts.Stdout}}
}

// letGo ends the stream's hold on the terminal: no read of it begins
// after this, no write reaches it, no size is asked; and drops it.
func (t *execTerm) letGo() {
	t.in.letGo()
	t.out.letGo()
	if t.sizes != nil {
		t.sizes.letGo()
	}
}

// stream asks the API for the exec subresource of the pod, with the
// container and the command, as kubectl exec builds the request, and
// runs the stream over WebSocket until it ends.
func (s *session) stream(ctx context.Context, pod ObjectRef, container string, command []string, opts remotecommand.StreamOptions) error {
	req := s.clients.typed.CoreV1().RESTClient().Post().
		Namespace(pod.Namespace).Resource("pods").Name(pod.Name).SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: container,
			Command:   command,
			Stdin:     true,
			Stdout:    true,
			TTY:       true,
		}, scheme.ParameterCodec)
	ex, err := remotecommand.NewWebSocketExecutor(s.clients.cfg, "GET", req.URL().String())
	if err != nil {
		return err
	}
	return ex.StreamWithContext(ctx, opts)
}

// execOutcome turns how the stream ended into what Exec returns, given
// the context it ran with and how many bytes of output passed.
func execOutcome(ctx context.Context, err error, written int64) (ExecResult, error) {
	started := written > 0
	if err == nil {
		return ExecResult{Started: true}, nil
	}
	var exit utilexec.CodeExitError
	if errors.As(err, &exit) {
		return ExecResult{Started: true, Status: exit.Code}, nil
	}
	if ctx.Err() != nil {
		return ExecResult{Started: started}, contextError(ctx)
	}
	var upgrade *httpstream.UpgradeFailureError
	if errors.As(err, &upgrade) {
		var status apierrors.APIStatus
		if errors.As(upgrade.Cause, &status) {
			return ExecResult{}, upgrade.Cause
		}
	}
	if !started {
		return ExecResult{}, fmt.Errorf("%w: %s", ErrExecNotStarted, notStartedReason(err.Error()))
	}
	return ExecResult{Started: true}, err
}

// contextError is the error of ctx, which is done: its cause, which for
// the session's end is errSessionEnded, joined to context.Canceled.
func contextError(ctx context.Context) error {
	cause := context.Cause(ctx)
	if errors.Is(cause, ctx.Err()) {
		return cause
	}
	return fmt.Errorf("%w: %w", cause, ctx.Err())
}

// notStartedReason is the reason a command could not be started, from
// the text of the stream's error: without the API's "Internal error
// occurred: " before it, and from the runtime's own words on when they
// are there (exec: "sh": executable file not found in $PATH), which
// leaves out the exec's id; cut to execMaxReason bytes.
func notStartedReason(text string) string {
	const internal = "Internal error occurred: "
	for strings.HasPrefix(text, internal) {
		text = text[len(internal):]
	}
	if i := strings.Index(text, `exec: "`); i >= 0 {
		text = text[i:]
	}
	return cutText(text, execMaxReason)
}

// cutText cuts s to at most n bytes, after a whole character.
func cutText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// execSizes is the terminal's sizes as the stream asks for them: the
// size now, and then the size each time opts.Resized has a value, until
// ctx is done. A size of zero or less in either direction is not sent.
// A closed Resized means the size never changes again. Without
// opts.Size there is none: nil.
func execSizes(ctx context.Context, opts ExecOptions) *execSizeQueue {
	if opts.Size == nil {
		return nil
	}
	return &execSizeQueue{ctx: ctx, size: opts.Size, resized: opts.Resized}
}

type execSizeQueue struct {
	ctx     context.Context
	resized <-chan struct{} // nil: the size never changes
	asked   bool            // the size was asked once

	mu   sync.Mutex
	size func() (width, height int) // nil once Exec let go
}

// Next returns the next size to send, or nil when ctx is done or Exec
// let go of the terminal. The stream calls it from one goroutine.
func (q *execSizeQueue) Next() *remotecommand.TerminalSize {
	for {
		if q.ctx.Err() != nil {
			return nil
		}
		if q.asked {
			select {
			case <-q.ctx.Done():
				return nil
			case _, ok := <-q.resized:
				if !ok {
					q.resized = nil
					continue
				}
			}
		}
		q.asked = true
		if w, h, ok := q.ask(); !ok {
			return nil
		} else if w > 0 && h > 0 {
			return &remotecommand.TerminalSize{Width: uint16(min(w, math.MaxUint16)), Height: uint16(min(h, math.MaxUint16))}
		}
	}
}

// ask returns the terminal's size now; false once Exec let go of it.
func (q *execSizeQueue) ask() (w, h int, ok bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.size == nil || q.ctx.Err() != nil {
		return 0, 0, false
	}
	w, h = q.size()
	return w, h, true
}

// letGo drops the terminal's size: no call of it begins after this, and
// one under way has returned.
func (q *execSizeQueue) letGo() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.size = nil
}

// execOutput is the writer the stream is given: it passes the command's
// output to the terminal, and counts it, until Exec lets the terminal
// go; then it takes nothing more.
type execOutput struct {
	w io.Writer // nil: the output is counted and dropped

	mu     sync.Mutex
	n      int64
	closed bool
}

// errLetGo is what the stream gets when it writes after Exec returned.
var errLetGo = errors.New("the terminal was let go")

func (o *execOutput) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return 0, errLetGo
	}
	if o.w == nil {
		o.n += int64(len(p))
		return len(p), nil
	}
	n, err := o.w.Write(p)
	o.n += int64(n)
	return n, err
}

// letGo ends the writes to the terminal, and drops it: it waits for one
// that is under way.
func (o *execOutput) letGo() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.closed = true
	o.w = nil
}

// written returns how many bytes of output passed.
func (o *execOutput) written() int64 {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.n
}

// execInput is the reader the stream is given: the terminal's input,
// until Exec lets the terminal go. A read that is under way then is the
// terminal's to end (ExecOptions.Stdin); no read begins after it.
type execInput struct {
	mu sync.Mutex
	r  io.Reader // nil once Exec let go
}

func (i *execInput) Read(p []byte) (int, error) {
	i.mu.Lock()
	r := i.r
	i.mu.Unlock()
	if r == nil {
		return 0, io.EOF
	}
	return r.Read(p)
}

// letGo drops the terminal's input: no read of it begins after this.
func (i *execInput) letGo() {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.r = nil
}
