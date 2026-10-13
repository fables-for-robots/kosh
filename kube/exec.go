package kube

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"unicode/utf8"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/remotecommand"
	utilexec "k8s.io/client-go/util/exec"
	"k8s.io/streaming/pkg/httpstream"
)

// execMaxReason is the longest reason, in bytes, that ErrExecNotStarted
// is wrapped around.
const execMaxReason = 1 << 10

// Exec begins as every action does; checks the pod's reference, the
// container's name and the command; and runs the command on the API's
// exec stream with a terminal, over WebSocket alone: kubectl's fallback
// to SPDY would turn the API's typed refusals into plain text.
func (s *session) Exec(ctx context.Context, pod ObjectRef, opts ExecOptions) (res ExecResult, err error) {
	ctx, release, err := s.begin(ctx, actExec, pod)
	if err != nil {
		return ExecResult{}, err
	}
	defer release()
	if err := s.checkExec(pod, opts); err != nil {
		s.logAction(ctx, actExec, pod, err, slog.String("phase", "end"))
		return ExecResult{}, err
	}
	return s.exec(ctx, pod, opts)
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
	if s.c.cfg == nil {
		return errors.New("the cluster has no API to run a command through")
	}
	return nil
}

// exec runs the command on the pod's exec stream until it ends or ctx
// does, and says how it ended.
func (s *session) exec(ctx context.Context, pod ObjectRef, opts ExecOptions) (ExecResult, error) {
	in := &execInput{r: opts.Stdin}
	out := &execOutput{w: opts.Stdout}
	stream := remotecommand.StreamOptions{
		Stdout:            out,
		Tty:               true,
		TerminalSizeQueue: execSizes(ctx, opts),
	}
	if opts.Stdin != nil {
		stream.Stdin = in
	}
	err := s.stream(ctx, pod, opts.Container, opts.Command, stream)
	// What the stream still runs after this neither reads the terminal
	// anew nor writes to it.
	in.letGo()
	out.letGo()
	return execOutcome(ctx, err, out.written())
}

// stream asks the API for the exec subresource of the pod, with the
// container and the command, as kubectl exec builds the request, and
// runs the stream over WebSocket until it ends.
func (s *session) stream(ctx context.Context, pod ObjectRef, container string, command []string, opts remotecommand.StreamOptions) error {
	req := s.c.typed.CoreV1().RESTClient().Post().
		Namespace(pod.Namespace).Resource("pods").Name(pod.Name).SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: container,
			Command:   command,
			Stdin:     true,
			Stdout:    true,
			TTY:       true,
		}, scheme.ParameterCodec)
	ex, err := remotecommand.NewWebSocketExecutor(s.c.cfg, "GET", req.URL().String())
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
// Without opts.Size there is none.
func execSizes(ctx context.Context, opts ExecOptions) remotecommand.TerminalSizeQueue {
	if opts.Size == nil {
		return nil
	}
	return &execSizeQueue{ctx: ctx, size: opts.Size, resized: opts.Resized}
}

type execSizeQueue struct {
	ctx     context.Context
	size    func() (width, height int)
	resized <-chan struct{} // nil: the size never changes
	asked   bool            // the size was asked once
}

// Next returns the next size to send, or nil when ctx is done. The
// stream calls it from one goroutine.
func (q *execSizeQueue) Next() *remotecommand.TerminalSize {
	for {
		if q.asked {
			select {
			case <-q.ctx.Done():
				return nil
			case <-q.resized:
			}
		}
		q.asked = true
		if q.ctx.Err() != nil {
			return nil
		}
		w, h := q.size()
		if w > 0 && h > 0 {
			return &remotecommand.TerminalSize{Width: uint16(min(w, math.MaxUint16)), Height: uint16(min(h, math.MaxUint16))}
		}
	}
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

// letGo ends the writes to the terminal: it waits for one that is under
// way.
func (o *execOutput) letGo() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.closed = true
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
	r      io.Reader
	closed atomic.Bool
}

func (i *execInput) Read(p []byte) (int, error) {
	if i.closed.Load() {
		return 0, io.EOF
	}
	return i.r.Read(p)
}

func (i *execInput) letGo() { i.closed.Store(true) }
