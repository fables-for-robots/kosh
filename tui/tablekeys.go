package tui

import (
	tea "charm.land/bubbletea/v2"
)

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
		return v.readNow()
	}
	return nil
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

// Hints are the table's own keys that apply, then its bindings'.
func (v *tableView) Hints() []hint {
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
	hs = append(hs, hint{"?", "Help"})
	return hs
}
