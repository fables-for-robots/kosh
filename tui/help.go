package tui

import (
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// helpView lists the keys of the view it was opened from and the root's.
type helpView struct {
	sections []helpSection
}

// helpSection is a heading and its keys.
type helpSection struct {
	title string
	hints []hint
}

// openHelp pushes the help of the top view.
func (r *root) openHelp() tea.Cmd {
	var sections []helpSection
	if top := r.top(); top != nil {
		if hs := top.Hints(); len(hs) > 0 {
			sections = append(sections, helpSection{strings.ToUpper(clean(top.Crumb())), hs})
		}
	}
	sections = append(sections, helpSection{"GENERAL", rootHints()})
	return pushView(&helpView{sections: sections})
}

func (v *helpView) Init() tea.Cmd { return nil }

// Update pops the help on esc and q; the root pops it on ?.
func (v *helpView) Update(msg tea.Msg) (view, tea.Cmd) {
	if k, ok := msg.(tea.KeyPressMsg); ok {
		switch k.String() {
		case "esc", "q":
			return v, popView()
		}
	}
	return v, nil
}

// View draws the sections side by side, each in as many columns of h
// lines as its keys need, as far as they fit.
func (v *helpView) View(w, h int) string {
	if w <= 0 || h <= 1 {
		return ""
	}
	type column struct {
		lines []string
		width int
	}
	var cols []column
	for _, s := range v.sections {
		keyW := 0
		for _, hn := range s.hints {
			keyW = max(keyW, ansi.StringWidth(clean(hn.Key))+2)
		}
		for i, part := range columns(helpLines(s.hints, keyW), h-1) {
			title := s.title
			if i > 0 {
				title = ""
			}
			c := column{lines: append([]string{styleSection.Render(title)}, part...)}
			c.width = max(colWidth(c.lines), ansi.StringWidth(title))
			cols = append(cols, c)
		}
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

// helpLines draws hints as "<key> text", the texts in a column.
func helpLines(hints []hint, keyW int) []string {
	out := make([]string, len(hints))
	for i, h := range hints {
		key := "<" + clean(h.Key) + ">"
		out[i] = styleKey.Render(key) + strings.Repeat(" ", max(keyW-ansi.StringWidth(key), 0)+1) + styleHint.Render(clean(h.Text))
	}
	return out
}

func (v *helpView) Title() string    { return "Help" }
func (v *helpView) Crumb() string    { return "help" }
func (v *helpView) Hints() []hint    { return []hint{{"esc", "Back"}} }
func (v *helpView) Capturing() bool  { return false }
func (v *helpView) Fullscreen() bool { return false }
func (v *helpView) Close()           {}
