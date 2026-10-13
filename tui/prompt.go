package tui

import (
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
)

// promptKind is what the prompt is open for.
type promptKind int

const (
	promptClosed promptKind = iota
	promptCommand
	promptFilter
)

// inputMaxChars is how many characters the prompt and a field of a
// dialog take at most, typed or pasted.
const inputMaxChars = 1024

// prompt is the line in which a command or a filter is typed.
type prompt struct {
	kind promptKind
	in   textinput.Model
}

// newInput returns the text input of a prompt: no blinking cursor, and
// the suggestion in the dim style.
func newInput(w int) textinput.Model {
	in := textinput.New()
	in.Prompt = ""
	in.ShowSuggestions = true
	st := textinput.DefaultDarkStyles()
	st.Cursor.Blink = false
	st.Focused.Text = stylePromptText
	st.Focused.Suggestion = styleDim
	in.SetStyles(st)
	in.SetWidth(max(w-3, 1))
	in.CharLimit = inputMaxChars
	// Paste would read the clipboard by starting a program on the server.
	// What the user pastes comes from their terminal as a tea.PasteMsg.
	in.KeyMap.Paste.SetEnabled(false)
	in.Focus()
	return in
}

func (p *prompt) open() bool { return p.kind != promptClosed }

func (p *prompt) close() { *p = prompt{} }

func (p *prompt) resize(w int) {
	if p.open() {
		p.in.SetWidth(max(w-3, 1))
	}
}

// suggest gives the command prompt the names it suggests.
func (p *prompt) suggest(names []string) {
	if p.kind == promptCommand {
		p.in.SetSuggestions(names)
	}
}

// line draws the prompt, w cells wide.
func (p *prompt) line(w int) string {
	lead := stylePromptCmd.Render(":")
	if p.kind == promptFilter {
		lead = stylePromptFind.Render("/")
	}
	return fit(lead+p.in.View(), w)
}

// suggestionsMsg brings the names the command prompt suggests.
type suggestionsMsg struct{ names []string }

// openCommand opens the command prompt and fetches the names of the
// resources for its suggestions: the plural names, then the short names.
func (r *root) openCommand() tea.Cmd {
	r.prompt = prompt{kind: promptCommand, in: newInput(r.w)}
	sess := r.e.sess
	return func() tea.Msg {
		rr := sess.Resources()
		var names []string
		for _, res := range rr {
			names = append(names, cleanLine(res.GVR.Resource))
		}
		for _, res := range rr {
			for _, s := range res.ShortNames {
				names = append(names, cleanLine(s))
			}
		}
		return suggestionsMsg{names}
	}
}

// openFilter opens the filter prompt over the top view.
func (r *root) openFilter() tea.Cmd {
	r.prompt = prompt{kind: promptFilter, in: newInput(r.w)}
	return nil
}

// promptKey handles a key while the prompt is open.
func (r *root) promptKey(k tea.KeyPressMsg) tea.Cmd {
	kind := r.prompt.kind
	switch k.String() {
	case "enter":
		text := r.prompt.in.Value()
		r.prompt.close()
		if kind == promptCommand {
			return r.runCommand(text, true, false)
		}
		return nil
	case "esc":
		r.prompt.close()
		if kind == promptFilter {
			return r.setFilter("")
		}
		return nil
	case "right":
		in := &r.prompt.in
		if kind == promptCommand && in.Position() == len([]rune(in.Value())) {
			if s := in.CurrentSuggestion(); s != "" {
				in.SetValue(s)
				in.CursorEnd()
				return nil
			}
		}
	}
	return r.promptInput(k)
}

// promptPaste puts what the terminal pasted into the prompt as typed
// text, cleaned, on one line, and cut to what the prompt takes.
func (r *root) promptPaste(p tea.PasteMsg) tea.Cmd {
	return r.promptInput(tea.PasteMsg{Content: inputCut(cleanLine(p.Content))})
}

// inputCut returns the first inputMaxChars characters of s.
func inputCut(s string) string {
	n := 0
	for i := range s {
		if n == inputMaxChars {
			return s[:i]
		}
		n++
	}
	return s
}

// promptInput gives msg to the text input, and a changed filter to the
// top view.
func (r *root) promptInput(msg tea.Msg) tea.Cmd {
	before := r.prompt.in.Value()
	var cmd tea.Cmd
	r.prompt.in, cmd = r.prompt.in.Update(msg)
	if after := r.prompt.in.Value(); r.prompt.kind == promptFilter && after != before {
		return tea.Batch(cmd, r.setFilter(after))
	}
	return cmd
}

// setFilter gives text to the top view when it filters.
func (r *root) setFilter(text string) tea.Cmd {
	if f, ok := r.top().(filterable); ok {
		return f.SetFilter(text)
	}
	return nil
}

// history is the commands that went to a resource, the oldest first.
type history struct {
	entries []string
	// at is the entry shown now, prev the one shown before it.
	at, prev int
}

// add puts cmd at the end, unless it is the last entry already, and
// makes it the current one.
func (h *history) add(cmd string) {
	if n := len(h.entries); n == 0 || h.entries[n-1] != cmd {
		h.entries = append(h.entries, cmd)
	}
	h.prev, h.at = h.at, len(h.entries)-1
}

// move makes entry i the current one and returns it.
func (h *history) move(i int) (string, bool) {
	if i < 0 || i >= len(h.entries) || i == h.at {
		return "", false
	}
	h.prev, h.at = h.at, i
	return h.entries[i], true
}

// historyBack runs the command before the current one.
func (r *root) historyBack() tea.Cmd { return r.historyGo(r.hist.at - 1) }

// historyForward runs the command after the current one.
func (r *root) historyForward() tea.Cmd { return r.historyGo(r.hist.at + 1) }

// historyLast runs the command that was current before this one.
func (r *root) historyLast() tea.Cmd { return r.historyGo(r.hist.prev) }

func (r *root) historyGo(i int) tea.Cmd {
	cmd, ok := r.hist.move(i)
	if !ok {
		return nil
	}
	return r.runCommand(cmd, false, false)
}
