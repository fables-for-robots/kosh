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

// push puts v on the stack, tells it the namespaces and runs its Init.
func (r *root) push(v view) tea.Cmd {
	if v == nil {
		return nil
	}
	r.stack = append(r.stack, v)
	var cmd tea.Cmd
	r.stack[len(r.stack)-1], cmd = v.Update(r.ns.msg())
	return tea.Batch(cmd, r.stack[len(r.stack)-1].Init())
}

// pop takes the top view off the stack and closes it, and asks the view
// below to read again. The last view stays.
func (r *root) pop() tea.Cmd {
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
