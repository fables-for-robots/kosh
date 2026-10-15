package kube

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	kubedrain "k8s.io/kubectl/pkg/drain"
)

func (s *session) Cordon(ctx context.Context, node ObjectRef, on bool) (err error) {
	action := actUncordon
	if on {
		action = actCordon
	}
	ctx, release, err := s.begin(ctx, action, node)
	if err != nil {
		return err
	}
	defer release()
	defer func() { s.logAction(ctx, action, node, err) }()
	if err := s.c.checkRef(node); err != nil {
		return err
	}
	if err := kindIs(action, node, nodeKinds...); err != nil {
		return err
	}
	return s.cordon(ctx, &kubedrain.Helper{Ctx: ctx, Client: s.clients.typed, Out: io.Discard, ErrOut: io.Discard}, node.Name, on)
}

// cordon sets or clears the unschedulable flag of the node with h.
func (s *session) cordon(ctx context.Context, h *kubedrain.Helper, name string, on bool) error {
	n, err := s.clients.typed.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	return kubedrain.RunCordonOrUncordon(h, n, on)
}

func (s *session) Drain(ctx context.Context, node ObjectRef, opts DrainOptions) (err error) {
	ctx, release, err := s.begin(ctx, actDrain, node)
	if err != nil {
		return err
	}
	defer release()
	out := &drainOutput{}
	defer func() {
		s.logAction(ctx, actDrain, node, err,
			slog.Int("gracePeriod", opts.GracePeriod),
			slog.Duration("timeout", opts.Timeout),
			slog.Bool("ignoreDaemonSets", opts.IgnoreDaemonSets),
			slog.Bool("deleteEmptyDirData", opts.DeleteEmptyDirData),
			slog.Bool("force", opts.Force),
			slog.Bool("disableEviction", opts.DisableEviction),
			slog.String("output", out.String()),
		)
	}()
	if err := s.c.checkRef(node); err != nil {
		return err
	}
	if err := kindIs(actDrain, node, nodeKinds...); err != nil {
		return err
	}
	h := newDrainHelper(ctx, s.clients.typed, opts, out)
	if err := s.cordon(ctx, h, node.Name, true); err != nil {
		return err
	}
	return runUntilDone(ctx, func() error { return kubedrain.RunNodeDrain(h, node.Name) })
}

// runUntilDone runs f, and returns what it returns, or as soon as ctx
// ends, an error that wraps ctx's cause. kubectl's drain sleeps between
// two evictions the API refused for now, and asks discovery whether it
// may evict, without a context: neither can be cut short, so f is left
// to end by itself. Every request it sends after ctx ended carries ctx
// and fails, and its goroutines return at the next look at ctx.
func runUntilDone(ctx context.Context, f func() error) error {
	done := make(chan error, 1)
	go func() { done <- f() }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return fmt.Errorf("stopped: %w", context.Cause(ctx))
	}
}

// evictRetryDelay is how long a drain waits before it asks again to
// evict a pod whose eviction the API refused for now, because of a
// PodDisruptionBudget: kubectl drain's 5 seconds. Tests shorten it.
var evictRetryDelay = 5 * time.Second

// newDrainHelper returns the helper of kubectl's drain package that
// drains with opts, ctx and client, and prints to out.
func newDrainHelper(ctx context.Context, client kubernetes.Interface, opts DrainOptions, out io.Writer) *kubedrain.Helper {
	return &kubedrain.Helper{
		Ctx:                 ctx,
		Client:              client,
		Force:               opts.Force,
		GracePeriodSeconds:  opts.GracePeriod,
		IgnoreAllDaemonSets: opts.IgnoreDaemonSets,
		Timeout:             opts.Timeout,
		DeleteEmptyDirData:  opts.DeleteEmptyDirData,
		DisableEviction:     opts.DisableEviction,
		// Without it, kubectl's loop asks again at once, as often as the
		// client's rate limiter, which every session shares, lets it.
		EvictErrorRetryDelay: evictRetryDelay,
		Out:                  out,
		ErrOut:               out,
	}
}

// drainOutputLimit is how much of what the drain helper prints is kept,
// for the log line.
const drainOutputLimit = 16 << 10

// drainOutputCut ends what was kept of output that was longer.
const drainOutputCut = "\n[the rest was cut]"

// drainOutput keeps the first drainOutputLimit bytes the drain helper
// prints. The helper's goroutines may write to it at once.
type drainOutput struct {
	mu  sync.Mutex
	buf bytes.Buffer
	cut bool
}

// Write keeps what fits of p and says that it took all of it, so that
// the helper goes on.
func (b *drainOutput) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if room := drainOutputLimit - b.buf.Len(); len(p) > room {
		b.buf.Write(p[:room])
		b.cut = true
	} else {
		b.buf.Write(p)
	}
	return len(p), nil
}

// String returns what was kept, and drainOutputCut after it when more
// was printed.
func (b *drainOutput) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.cut {
		return b.buf.String() + drainOutputCut
	}
	return b.buf.String()
}
