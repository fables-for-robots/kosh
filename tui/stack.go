package tui

import tea "charm.land/bubbletea/v2"

// top returns the view on top of the stack, or nil while there is none.
func (r *root) top() view {
	if len(r.stack) == 0 {
		return nil
	}
	return r.stack[len(r.stack)-1]
}

// updateTop gives msg to the top view.
func (r *root) updateTop(msg tea.Msg) tea.Cmd {
	if len(r.stack) == 0 {
		return nil
	}
	i := len(r.stack) - 1
	var cmd tea.Cmd
	r.stack[i], cmd = r.stack[i].Update(msg)
	return cmd
}

// broadcast gives msg to every view of the stack, the top one first,
// and to the dialog.
func (r *root) broadcast(msg tea.Msg) tea.Cmd {
	var cmds []tea.Cmd
	for i := len(r.stack) - 1; i >= 0; i-- {
		var cmd tea.Cmd
		r.stack[i], cmd = r.stack[i].Update(msg)
		cmds = append(cmds, cmd)
	}
	if r.dlg != nil {
		var cmd tea.Cmd
		r.dlg, cmd = r.dlg.Update(msg)
		cmds = append(cmds, cmd)
	}
	return tea.Batch(cmds...)
}

// maxStack is how many views the stack holds at most: a view is not
// given up when another is pushed over it, and a session must not hold
// views without end.
const maxStack = 32

// textTooManyViews is the flash of a push the stack has no room for.
const textTooManyViews = "too many views open: go back with esc"

// buriedMsg tells the top view that another view is pushed over it. It
// is buried until it is the top again, which it learns from the
// refreshMsg that pop gives the new top. A view that does nothing
// while it is not seen rests in between; the others ignore it.
type buriedMsg struct{}

// push puts v on the stack, tells the view below that it is buried,
// tells v the namespaces and runs its Init. When the stack is full, v
// is closed instead, and the flash says so.
func (r *root) push(v view) tea.Cmd {
	if v == nil {
		return nil
	}
	if len(r.stack) >= maxStack {
		v.Close()
		return func() tea.Msg { return flashMsg{text: textTooManyViews, err: true} }
	}
	buried := r.updateTop(buriedMsg{})
	r.stack = append(r.stack, v)
	var cmd tea.Cmd
	r.stack[len(r.stack)-1], cmd = v.Update(r.ns.msg())
	return tea.Batch(buried, cmd, r.stack[len(r.stack)-1].Init())
}

// bury makes a table rest while it is buried: the wait for its next
// reading ends, and it waits for no other until it is the top again. A
// reading under way is taken when it comes. Its watch stays open.
func (v *tableView) bury() tea.Cmd {
	if v.closed {
		return nil
	}
	v.buried = true
	if v.stop != nil {
		close(v.stop)
		v.stop = make(chan struct{})
	}
	return nil
}

// pop takes the top view off the stack and closes it, and asks the view
// below to read again, which also tells it that it is the top again. The
// last view stays, unless it is the help or the names, which leave the
// body empty, as it was when the start failed.
func (r *root) pop() tea.Cmd {
	if len(r.stack) == 1 {
		switch r.stack[0].(type) {
		case *helpView, *namesView:
			r.stack[0].Close()
			r.stack = nil
		}
		return nil
	}
	if len(r.stack) < 2 {
		return nil
	}
	r.stack[len(r.stack)-1].Close()
	r.stack[len(r.stack)-1] = nil
	r.stack = r.stack[:len(r.stack)-1]
	return r.updateTop(refreshMsg{})
}

// replace closes every view of the stack and makes v the only one.
func (r *root) replace(v view) tea.Cmd {
	for i := len(r.stack) - 1; i >= 0; i-- {
		r.stack[i].Close()
	}
	r.stack = nil
	return r.push(v)
}
