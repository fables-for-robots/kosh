// Package tui is kosh's terminal UI: a Bubble Tea program for one
// session that behaves like k9s. It sees the cluster only as a
// kube.Session.
package tui

import (
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
}

// New returns the program of a session.
func New(sess kube.Session, opts Options) tea.Model {
	panic("not implemented")
}
