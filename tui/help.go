package tui

import (
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// helpView lists the keys of the view it was opened from and the root's.
type helpView struct {
	sections []helpSection
	// first is the first column drawn, which left and right move when
	// the columns do not all fit; cols is how many there were when the
	// view was last drawn, and cut whether they did not all fit then.
	first, cols int
	cut         bool
}

// helpSection is a heading and its keys.
type helpSection struct {
	title string
	hints []hint
}

// helpful is a view whose help lists more keys than its hints, which
// are the header's short list.
type helpful interface {
	helpHints() []hint
}

// openHelp pushes the help of the top view: every key of it, and the
// root's.
func (r *root) openHelp() tea.Cmd {
	var sections []helpSection
	if top := r.top(); top != nil {
		hs := top.Hints()
		if h, ok := top.(helpful); ok {
			hs = h.helpHints()
		}
		if len(hs) > 0 {
			sections = append(sections, helpSection{strings.ToUpper(cleanLine(top.Crumb())), hs})
		}
	}
	sections = append(sections, helpSection{"GENERAL", rootHints()})
	return pushView(&helpView{sections: sections})
}

func (v *helpView) Init() tea.Cmd { return nil }

// Update pops the help on esc and q; the root pops it on ?. left and
// right move the columns sideways.
func (v *helpView) Update(msg tea.Msg) (view, tea.Cmd) {
	if k, ok := msg.(tea.KeyPressMsg); ok {
		switch k.String() {
		case "esc", "q":
			return v, popView()
		case "left":
			if v.cut {
				v.first = max(v.first-1, 0)
			}
		case "right":
			if v.cut {
				v.first = max(min(v.first+1, v.cols-1), 0)
			}
		}
	}
	return v, nil
}

// View draws the sections side by side, each in as many columns of h
// lines as its keys need, from the column first, as far as they fit.
func (v *helpView) View(w, h int) string {
	if w <= 0 || h <= 0 {
		return ""
	}
	if h == 1 {
		return fit(styleSection.Render("HELP"), w)
	}
	type column struct {
		lines []string
		width int
	}
	var cols []column
	for _, s := range v.sections {
		for i, hints := range columnsOf(s.hints, h-1) {
			keyW := 0
			for _, hn := range hints {
				keyW = max(keyW, ansi.StringWidth(cleanLine(hn.Key))+2)
			}
			part := helpLines(hints, keyW)
			title := s.title
			if i > 0 {
				title = ""
			}
			c := column{lines: append([]string{styleSection.Render(title)}, part...)}
			c.width = max(colWidth(c.lines), ansi.StringWidth(title))
			cols = append(cols, c)
		}
	}
	v.cols = len(cols)
	total := 0
	for i, c := range cols {
		if i > 0 {
			total += len(headerGap)
		}
		total += c.width
	}
	v.cut = total > w
	v.first = max(min(v.first, len(cols)-1), 0)
	if !v.cut {
		v.first = 0
	}
	if len(cols) > 0 {
		cols = cols[v.first:]
	}
	lines := make([]string, h)
	for _, c := range cols {
		for i := range lines {
			cell := ""
			if i < len(c.lines) {
				cell = c.lines[i]
			}
			lines[i] += fit(cell, c.width) + headerGap
		}
	}
	for i := range lines {
		lines[i] = fit(lines[i], w)
	}
	return strings.Join(lines, "\n")
}

// columnsOf cuts hints into columns of rows hints.
func columnsOf(hints []hint, rows int) [][]hint {
	var out [][]hint
	for len(hints) > 0 {
		n := min(max(rows, 1), len(hints))
		out = append(out, hints[:n])
		hints = hints[n:]
	}
	return out
}

// helpLines draws hints as "<key> text", the texts in a column.
func helpLines(hints []hint, keyW int) []string {
	out := make([]string, len(hints))
	for i, h := range hints {
		key := "<" + cleanLine(h.Key) + ">"
		out[i] = styleKey.Render(key) + strings.Repeat(" ", max(keyW-ansi.StringWidth(key), 0)+1) + styleHint.Render(cleanLine(h.Text))
	}
	return out
}

func (v *helpView) Title() string { return "Help" }
func (v *helpView) Crumb() string { return "help" }

// Hints are esc, and left and right while a column is cut at the width
// the help was last drawn in.
func (v *helpView) Hints() []hint {
	if !v.cut {
		return []hint{{"esc", "Back"}}
	}
	return []hint{{"esc", "Back"}, {"left/right", "More Columns"}}
}
func (v *helpView) Capturing() bool  { return false }
func (v *helpView) Fullscreen() bool { return false }
func (v *helpView) Close()           {}
