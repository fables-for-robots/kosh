package kube

import (
	"bytes"
	"context"
	"io"
	"sync"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubedrain "k8s.io/kubectl/pkg/drain"
)

func (s *session) Cordon(ctx context.Context, node ObjectRef, on bool) error {
	action := actUncordon
	if on {
		action = actCordon
	}
	if err := s.begin(ctx, action, node); err != nil {
		return err
	}
	if err := kindIs(action, node, nodeKinds...); err != nil {
		return err
	}
	return s.cordon(ctx, &kubedrain.Helper{Ctx: ctx, Client: s.c.typed, Out: io.Discard, ErrOut: io.Discard}, node.Name, on)
}

// cordon sets or clears the unschedulable flag of the node with h.
func (s *session) cordon(ctx context.Context, h *kubedrain.Helper, name string, on bool) error {
	n, err := s.c.typed.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	return kubedrain.RunCordonOrUncordon(h, n, on)
}

func (s *session) Drain(ctx context.Context, node ObjectRef, opts DrainOptions) error {
	if err := s.begin(ctx, actDrain, node); err != nil {
		return err
	}
	if err := kindIs(actDrain, node, nodeKinds...); err != nil {
		return err
	}
	out := &lockedBuffer{}
	h := &kubedrain.Helper{
		Ctx:                 ctx,
		Client:              s.c.typed,
		Force:               opts.Force,
		GracePeriodSeconds:  opts.GracePeriod,
		IgnoreAllDaemonSets: opts.IgnoreDaemonSets,
		Timeout:             opts.Timeout,
		DeleteEmptyDirData:  opts.DeleteEmptyDirData,
		DisableEviction:     opts.DisableEviction,
		Out:                 out,
		ErrOut:              out,
	}
	if err := s.cordon(ctx, h, node.Name, true); err != nil {
		return err
	}
	return kubedrain.RunNodeDrain(h, node.Name)
}

// lockedBuffer is a buffer that the drain helper's goroutines may write
// to at once.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
