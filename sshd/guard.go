package sshd

import (
	"runtime/debug"
	"sync/atomic"

	tea "charm.land/bubbletea/v2"
)

// guard is the model of a session, with the panics of its Init, Update
// and View caught: a panic there ends the session it happened in and no
// other. A panic in a command's goroutine is Bubble Tea's to catch.
//
// Bubble Tea calls Init, Update and View from one goroutine, the
// program's, so failed needs no lock.
type guard struct {
	inner tea.Model
	// stopped is set when kosh ends the session: from then on the model
	// is given no message, so that a key pressed after its key was
	// removed does nothing.
	stopped *atomic.Bool
	// panicked is told of a panic, with the stack where it happened.
	panicked func(in string, value any, stack []byte)
	// failed is set after a panic: the model is not called again.
	failed bool
}

// makeModel calls maker, and returns its model guarded. A panic in
// maker, or a nil model, is told to panicked and gives nil.
func makeModel(maker func() tea.Model, stopped *atomic.Bool, panicked func(string, any, []byte)) (m tea.Model) {
	defer func() {
		if r := recover(); r != nil {
			panicked("Model", r, debug.Stack())
			m = nil
		}
	}()
	inner := maker()
	if inner == nil {
		panicked("Model", "the maker of the session's model returned nil", debug.Stack())
		return nil
	}
	return &guard{inner: inner, stopped: stopped, panicked: panicked}
}

// fail records a panic, recovered in a deferred call of in.
func (g *guard) fail(in string, r any) {
	g.failed = true
	// Still in the deferred call: the stack is the one of the panic.
	g.panicked(in, r, debug.Stack())
}

// Init calls the model's Init.
func (g *guard) Init() (cmd tea.Cmd) {
	defer func() {
		if r := recover(); r != nil {
			g.fail("Init", r)
			cmd = nil
		}
	}()
	return g.inner.Init()
}

// Update calls the model's Update, unless the model has failed or the
// session is being ended.
func (g *guard) Update(msg tea.Msg) (m tea.Model, cmd tea.Cmd) {
	if g.failed || g.stopped.Load() {
		return g, nil
	}
	defer func() {
		if r := recover(); r != nil {
			g.fail("Update", r)
			m, cmd = g, nil
		}
	}()
	inner, cmd := g.inner.Update(msg)
	g.inner = inner
	return g, cmd
}

// View calls the model's View, unless the model has failed: then the
// view is empty.
func (g *guard) View() (v tea.View) {
	if g.failed {
		return tea.View{}
	}
	defer func() {
		if r := recover(); r != nil {
			g.fail("View", r)
			v = tea.View{}
		}
	}()
	return g.inner.View()
}
