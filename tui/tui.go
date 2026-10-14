// Package tui is kosh's terminal UI: a Bubble Tea program for one
// session that behaves like k9s. It sees the cluster only as a
// kube.Session.
package tui

import (
	"context"
	"io"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/fables-for-robots/kosh/kube"
)

// Options are the settings of a program.
type Options struct {
	// Version is kosh's version, for the header.
	Version string
	// Now returns the time. Nil means time.Now. Tests give their own.
	Now func() time.Time
	// Handover returns a command that gives the session's terminal to
	// run until run returns, and then gives the program done's message
	// with what run returned: sshd.Session.Exec, which says what it
	// promises. Nil means the session has no terminal to hand over, and
	// the UI then offers no shell.
	Handover func(run func(context.Context, Terminal) error, done func(error) tea.Msg) tea.Cmd
}

// Terminal is the session's terminal for the time a shell has it:
// sshd.Terminal, field for field, which says what each one does.
type Terminal struct {
	In      io.Reader
	Out     io.Writer
	Size    func() (width, height int)
	Resized <-chan struct{}
}

// New returns the program of a session.
func New(sess kube.Session, opts Options) tea.Model {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return newRoot(env{sess: sess, now: now, handover: opts.Handover}, opts.Version)
}
