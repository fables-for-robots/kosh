package tui

import (
	"slices"
	"strconv"
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// hintRows is how many hints, and shortcuts, a column of the header
// holds.
const hintRows = 5

// headerGap is the space between the header's columns.
const headerGap = "   "

// header draws the header's lines: who and what on the left, the
// namespace shortcuts in the middle, the top view's keys on the right.
// Every line is exactly as wide as the window. The left block is cut
// when it is wider than the window; a column right of it is shown only
// when it fits whole, with the gap before it, and the columns are shown
// in order up to the first that does not. When not every hint fits, the
// last place shown is the hint of ?, so that the help is always among
// the hints the header shows. When no hint would fit right of every
// column of shortcuts, only their first column is shown, when that makes
// room for the hints.
func (r *root) header() []string {
	name := cleanLine(r.grant.Name)
	if r.grant.ReadOnly {
		name += " [read-only]"
	}
	info := [][2]string{
		{"Key:", name},
		{"Cluster:", cleanLine(r.cluster)},
		{"kosh:", cleanLine(r.version)},
	}
	var left []string
	for _, kv := range info {
		left = append(left, styleInfoLabel.Render(kv[0]+strings.Repeat(" ", 9-len(kv[0])))+styleInfoValue.Render(kv[1]))
	}
	cols := [][]string{left}
	widths := []int{min(colWidth(left), max(r.w, 0))}
	used := widths[0]

	var shortcuts [][]string
	if tv, ok := r.top().(*tableView); ok && tv.hasShortcuts() {
		var sc []string
		for i, ns := range r.ns.shortcuts() {
			if ns == "" {
				ns = "all"
			}
			sc = append(sc, styleNumKey.Render("<"+strconv.Itoa(i)+">")+" "+styleHint.Render(cleanLine(ns)))
		}
		shortcuts = columns(sc, hintRows)
	}
	// place returns how many of the columns of shortcuts sc fit, and the
	// columns of hints that fit right of them when all of them do.
	place := func(sc [][]string) (n int, hints [][]string) {
		n = fitting(sc, used, r.w)
		if n < len(sc) {
			return n, nil
		}
		u := used
		for _, c := range sc {
			u += len(headerGap) + colWidth(c)
		}
		return n, r.hintColumns(u)
	}
	n, hints := place(shortcuts)
	if len(hints) == 0 && len(shortcuts) > 1 {
		// The help rather than a second column of shortcuts: the first
		// column, 0 to 4, and the hints with <?> Help right of it.
		if n1, h1 := place(shortcuts[:1]); len(h1) > 0 {
			shortcuts, n, hints = shortcuts[:1], n1, h1
		}
	}
	for _, c := range append(shortcuts[:n:n], hints...) {
		cols = append(cols, c)
		widths = append(widths, colWidth(c))
	}

	lines := make([]string, headerHeight)
	for i := range lines {
		var b strings.Builder
		for j, c := range cols {
			if j > 0 {
				b.WriteString(headerGap)
			}
			cell := ""
			if i < len(c) {
				cell = c[i]
			}
			b.WriteString(fit(cell, widths[j]))
		}
		lines[i] = fit(b.String(), r.w)
	}
	return lines
}

// hintColumns returns the columns of the top view's hints that fit
// whole right of used cells. When not all of them do, and the hint of ?
// is not among those that do, it takes the last place shown.
func (r *root) hintColumns(used int) [][]string {
	top := r.top()
	if top == nil {
		return nil
	}
	var hints []string
	help := -1
	for i, h := range top.Hints() {
		if h.Key == "?" {
			help = i
		}
		hints = append(hints, styleKey.Render("<"+cleanLine(h.Key)+">")+" "+styleHint.Render(cleanLine(h.Text)))
	}
	cols := columns(hints, hintRows)
	n := fitting(cols, used, r.w)
	if n == len(cols) || help < 0 || help < n*hintRows {
		return cols[:n]
	}
	for ; n > 0; n-- {
		shown := append(slices.Clone(hints[:n*hintRows-1]), hints[help])
		if c := columns(shown, hintRows); fitting(c, used, r.w) == n {
			return c
		}
	}
	return nil
}

// fitting is how many of cols, from the first, fit whole in a line w
// wide right of used cells, each with the gap before it.
func fitting(cols [][]string, used, w int) int {
	for i, c := range cols {
		used += len(headerGap) + colWidth(c)
		if used > w {
			return i
		}
	}
	return len(cols)
}

// columns cuts entries into columns of rows entries.
func columns(entries []string, rows int) [][]string {
	var out [][]string
	for len(entries) > 0 {
		n := min(rows, len(entries))
		out = append(out, entries[:n])
		entries = entries[n:]
	}
	return out
}

// colWidth is the width of the widest entry of a column.
func colWidth(c []string) int {
	w := 0
	for _, e := range c {
		w = max(w, ansi.StringWidth(e))
	}
	return w
}
