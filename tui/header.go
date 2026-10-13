package tui

import (
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
func (r *root) header() []string {
	name := clean(r.grant.Name)
	if r.grant.ReadOnly {
		name += " [read-only]"
	}
	info := [][2]string{
		{"Key:", name},
		{"Cluster:", clean(r.cluster)},
		{"kosh:", clean(r.version)},
	}
	var left []string
	for _, kv := range info {
		left = append(left, styleInfoLabel.Render(kv[0]+strings.Repeat(" ", 9-len(kv[0])))+styleInfoValue.Render(strings.ReplaceAll(kv[1], "\n", " ")))
	}
	cols := [][]string{left}

	if tv, ok := r.top().(*tableView); ok && tv.hasShortcuts() {
		var sc []string
		for i, ns := range r.ns.shortcuts() {
			if ns == "" {
				ns = "all"
			}
			sc = append(sc, styleNumKey.Render("<"+strconv.Itoa(i)+">")+" "+styleHint.Render(clean(ns)))
		}
		cols = append(cols, columns(sc, hintRows)...)
	}

	var hints []string
	if top := r.top(); top != nil {
		for _, h := range top.Hints() {
			hints = append(hints, styleKey.Render("<"+clean(h.Key)+">")+" "+styleHint.Render(clean(h.Text)))
		}
	}
	used := 0
	for _, c := range cols {
		used += colWidth(c) + len(headerGap)
	}
	for _, c := range columns(hints, hintRows) {
		w := colWidth(c)
		if used+w > r.w {
			break
		}
		cols = append(cols, c)
		used += w + len(headerGap)
	}

	lines := make([]string, headerHeight)
	for _, c := range cols {
		w := colWidth(c)
		for i := range lines {
			cell := ""
			if i < len(c) {
				cell = c[i]
			}
			lines[i] += fit(cell, w) + headerGap
		}
	}
	for i := range lines {
		lines[i] = fit(lines[i], r.w)
	}
	return lines
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
