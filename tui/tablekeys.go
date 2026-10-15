package tui

import (
	"fmt"
	"slices"

	tea "charm.land/bubbletea/v2"

	"github.com/fables-for-robots/kosh/kube"
)

// tableKeys are the keys a table handles itself, and the root's keys
// besides, which a table never sees. A binding may not take one of them
// (checkBindings).
var tableKeys = []string{
	"up", "k", "down", "j", "pgup", "ctrl+b", "pgdown", "ctrl+f",
	"g", "home", "G", "end", "left", "right",
	"N", "A", "S", "O", "ctrl+w", "ctrl+z",
	"space", "ctrl+space", "ctrl+\\",
	"esc", "ctrl+r", "enter", "J",
	"0", "1", "2", "3", "4", "5", "6", "7", "8", "9",
	// The root's.
	":", "/", "?", "ctrl+a", "ctrl+e", "[", "]", "-", "ctrl+c",
}

// checkBindings returns an error naming every binding of bs whose key is
// one the table handles itself: such a binding would never run.
func checkBindings(bs []binding) error {
	var taken []string
	for _, b := range bs {
		if slices.Contains(tableKeys, b.Key) {
			taken = append(taken, fmt.Sprintf("%q (%s)", b.Key, b.Hint.Text))
		}
	}
	if len(taken) > 0 {
		return fmt.Errorf("bindings of keys the table handles itself: %v", taken)
	}
	return nil
}

// key handles a key press: the table's own keys first, then the keys
// other parts bind.
func (v *tableView) key(k tea.KeyPressMsg) tea.Cmd {
	switch k.String() {
	case "up", "k":
		v.setCursor(v.cursor - 1)
	case "down", "j":
		v.setCursor(v.cursor + 1)
	case "pgup", "ctrl+b":
		v.setCursor(v.cursor - v.page)
	case "pgdown", "ctrl+f":
		v.setCursor(v.cursor + v.page)
	case "g", "home":
		v.setCursor(0)
	case "G", "end":
		v.setCursor(len(v.shown) - 1)
	case "left":
		v.colAt = max(v.colAt-1, 0)
	case "right":
		v.colAt = max(min(v.colAt+1, len(v.vis)-1), 0)
	case "N":
		v.sortBy("NAME")
	case "A":
		v.sortBy("AGE")
	case "S":
		v.sortBy("STATUS")
	case "O":
		if v.colAt < len(v.vis) {
			v.sortBy(v.data.cols[v.vis[v.colAt]].Name)
		}
	case "ctrl+w":
		v.wide = !v.wide
		v.rebuild()
	case "ctrl+z":
		v.faults = !v.faults
		v.rebuild()
	case "space":
		v.toggleMark()
	case "ctrl+space":
		v.markRange()
	case "ctrl+\\":
		v.marks = map[rowKey]bool{}
		v.lastMark = nil
	case "esc":
		if v.filter != "" {
			return v.SetFilter("")
		}
		return popView()
	case "ctrl+r":
		// The rights too, the answers the session keeps forgotten: a
		// question the API did not answer was answered no, and a key's
		// binding may have been made, or its role changed, since.
		v.forgetRights()
		return tea.Batch(v.readNow(), v.askRights())
	case "enter":
		return v.enter()
	case "J":
		return v.owner()
	case "0", "1", "2", "3", "4", "5", "6", "7", "8", "9":
		return v.useShortcut(int(k.String()[0] - '0'))
	default:
		return v.runBinding(k.String())
	}
	return nil
}

// runBinding runs the binding of key, when the table has one, with the
// marked rows that are shown, or with the cursor's row when none of them
// is. A table without rows runs nothing.
func (v *tableView) runBinding(key string) tea.Cmd {
	b, ok := v.bindingOf(key)
	if !ok || len(v.shown) == 0 {
		return nil
	}
	var refs []kube.ObjectRef
	for _, i := range v.shown {
		if r := &v.data.rows[i]; v.marks[r.key] {
			refs = append(refs, v.ref(r))
		}
	}
	if len(refs) == 0 || b.Handover {
		refs = []kube.ObjectRef{v.ref(&v.data.rows[v.shown[v.cursor]])}
	}
	return b.Run(v.e, v.q, refs)
}

// bindingOf returns the table's binding of key.
func (v *tableView) bindingOf(key string) (binding, bool) {
	for _, b := range v.bindings() {
		if b.Key == key && b.Run != nil {
			return b, true
		}
	}
	return binding{}, false
}

// toggleMark marks the cursor's row, or unmarks it.
func (v *tableView) toggleMark() {
	if v.cursorKey == nil {
		return
	}
	k := *v.cursorKey
	if v.marks[k] {
		delete(v.marks, k)
		if v.lastMark != nil && *v.lastMark == k {
			v.lastMark = nil
		}
		return
	}
	v.marks[k] = true
	v.lastMark = &k
}

// markRange marks every row from the last one marked to the cursor.
func (v *tableView) markRange() {
	if v.cursorKey == nil {
		return
	}
	from := -1
	if v.lastMark != nil {
		from = v.indexOf(*v.lastMark)
	}
	if from < 0 {
		v.toggleMark()
		return
	}
	lo, hi := min(from, v.cursor), max(from, v.cursor)
	for j := lo; j <= hi; j++ {
		v.marks[v.data.rows[v.shown[j]].key] = true
	}
	k := *v.cursorKey
	v.lastMark = &k
}

// Hints are the table's own keys that apply, then its bindings': the
// header's list.
func (v *tableView) Hints() []hint {
	return append(v.ownHints(), v.bindingHints()...)
}

// helpHints are every key of the table, for the help: the movement keys,
// its own keys, and its bindings'.
func (v *tableView) helpHints() []hint {
	hs := []hint{
		{"j/down", "Down"},
		{"k/up", "Up"},
		{"ctrl-f/pgdown", "Page Down"},
		{"ctrl-b/pgup", "Page Up"},
		{"g/home", "Top"},
		{"shift-g/end", "Bottom"},
		{"left/right", "Column"},
	}
	hs = append(hs, v.ownHints()...)
	hs = append(hs,
		hint{"ctrl-space", "Mark Range"},
		hint{"ctrl-\\", "Clear Marks"},
		hint{"enter", "Go Into"},
		hint{"ctrl-r", "Read Again"},
		hint{"esc", "Clear Filter / Back"},
	)
	if v.hasShortcuts() {
		hs = append(hs, hint{"0-9", "Namespace Shortcut"})
	}
	return append(hs, v.bindingHints()...)
}

// ownHints are the hints of the table's own keys that apply.
func (v *tableView) ownHints() []hint {
	var hs []hint
	if v.colIndex("NAME") >= 0 {
		hs = append(hs, hint{"shift-n", "Sort Name"})
	}
	if v.colIndex("AGE") >= 0 {
		hs = append(hs, hint{"shift-a", "Sort Age"})
	}
	if v.colIndex("STATUS") >= 0 {
		hs = append(hs, hint{"shift-s", "Sort Status"})
	}
	hs = append(hs,
		hint{"shift-o", "Sort Column"},
		hint{"ctrl-w", "Wide"},
		hint{"ctrl-z", "Faults"},
		hint{"/", "Filter"},
		hint{"space", "Mark"},
	)
	if v.hasOwner() {
		hs = append(hs, hint{"shift-j", "Owner"})
	}
	return append(hs, hint{"?", "Help"})
}

// bindingHints are the hints of the table's bindings.
func (v *tableView) bindingHints() []hint {
	var hs []hint
	for _, b := range v.bindings() {
		hs = append(hs, b.Hint)
	}
	return hs
}

// bindings are the table's bindings: none for a plain table, none whose
// needs the session's answers do not meet (none with a need before the
// answers are in), and none that hands the terminal over when the
// session has no terminal to hand over.
func (v *tableView) bindings() []binding {
	if v.plain {
		return nil
	}
	var out []binding
	for _, b := range bindingsFor(v.q.Resource, v.grant, v.can) {
		if b.Handover && v.e.handover == nil {
			continue
		}
		out = append(out, b)
	}
	return out
}
