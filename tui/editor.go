package tui

import (
	"context"
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"

	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/fables-for-robots/kosh/kube"
)

// The editor of an object's YAML (spec §11.2): the text area of bubbles,
// in kosh, with the YAML as it was read; ctrl+s sends it as an update,
// esc leaves and asks first when the text was changed.

// The largest YAML the editor opens: editorMaxLines lines and
// editorMaxBytes bytes. What it holds while it is edited is bounded by
// editorMaxText and editorTextMaxRows. The text area of bubbles draws all its text, in
// every update and every frame, and costs about 25µs a row it draws: a
// row a line, and a row more for each width of the window a long line
// wraps over. These keep a key and its frame under 50ms on an Apple M4
// Pro, in the worst case (lines of 73 cells, two rows each at 80
// columns) as well (TestEditorKeyPressIsFast).
const (
	editorMaxLines = 1000
	editorMaxBytes = 48 << 10
)

// editorView is the editor of one object's YAML.
type editorView struct {
	id  int64
	e   env
	ref kube.ObjectRef
	ta  textarea.Model
	// was is the text as it was read, which the text area holds as it is.
	was string
	// firstOf says that ref is the first of several marked rows:
	// " (the first of 3 marked)".
	firstOf  string
	onOpen   func()
	modified bool
	// size is the length of the text in bytes.
	size   int
	saving bool
	// why is the error of a refused update, cleaned, shown under the
	// text until the next update is sent; whyWrap keeps its lines.
	why     string
	whyWrap wrapped
}

// editorSavedMsg is what came of the update of the editor id.
type editorSavedMsg struct {
	id  int64
	err error
}

// editorMaxText is the most bytes the editor's text grows to by what is
// typed or pasted, 16 KiB more than editorMaxBytes, where a key still
// costs about what it costs at the largest object the editor opens
// (about 40ms on an Apple M4 Pro, BenchmarkEditorKeyPress).
// Input that would take the text further is refused, with the flash
// editorTextTooLong, and the text and the cursor stay as they were.
const editorMaxText = 64 << 10

// editorTextMaxRows is the most lines the text area holds, which input
// that would add more is refused for as editorMaxText refuses it: the
// ceiling of bubbles' text area, which holds 10,000 lines at most, and
// the width of the line numbers, four digits.
const editorTextMaxRows = 9999

// editorTextTooLong is the flash of input the editor refuses.
const editorTextTooLong = "the text is too long to edit here"

// The flashes of a YAML the editor will not open. editorTextCannotHold
// names editorMaxLines and editorMaxBytes, which
// TestEditorRefusesWhatItCannotHold keeps it in step with.
const (
	editorTextCannotKeep = "this object holds characters the editor cannot keep (a tab or a control character outside a quoted string): edit it with kubectl"
	editorTextCannotHold = "this object's YAML is larger than the editor opens (1000 lines, 48 KiB) or has characters it cannot hold: edit it with kubectl"
)

// editorOpen reads the YAML of the first row in a command and opens the
// editor with it; an error is a flash. The read holds the session's keys
// of actions until the editor has opened or the read has failed.
func editorOpen(e env, _ kube.Query, refs []kube.ObjectRef) tea.Cmd {
	sess, ref := e.sess, refs[0]
	return actionRead(e, func(ctx context.Context, opened func()) (tea.Msg, error) {
		text, err := sess.YAML(ctx, ref, false)
		if err != nil {
			return nil, err
		}
		v, why := editorNew(e, ref, text)
		if why != "" {
			return nil, errors.New(why)
		}
		v.firstOf = actionFirstOf(refs)
		v.onOpen = opened
		return pushViewMsg{v}, nil
	})
}

// editorNew returns the editor of ref with its YAML as it was read, and
// the cursor at its start. The editor never sends anything but what was
// read and what the user typed, so it returns why instead when it could
// not keep the text as it is: a line that clean would change (a raw tab,
// a control character, an escape sequence, bytes that are not UTF-8),
// more lines than editorMaxLines or bytes than editorMaxBytes, or
// anything the text area would not hold.
func editorNew(e env, ref kube.ObjectRef, yaml string) (v *editorView, why string) {
	if len(yaml) > editorMaxBytes {
		return nil, editorTextCannotHold
	}
	lines := strings.Split(yaml, "\n")
	if len(lines) > editorMaxLines {
		return nil, editorTextCannotHold
	}
	for _, l := range lines {
		if clean(l) != l {
			return nil, editorTextCannotKeep
		}
	}

	ta := textarea.New()
	ta.Prompt = ""
	ta.ShowLineNumbers = true
	ta.MaxHeight = editorTextMaxRows
	ta.MaxWidth = 0
	// input refuses what would take the text past editorMaxText bytes
	// before the text area sees it; this is the text area's own bound,
	// in characters, which are never more than the bytes.
	ta.CharLimit = editorMaxText
	// ctrl+v and the copy of a selection would use the clipboard of the
	// machine kosh runs on, by starting a program there. A paste of the
	// user's terminal still comes, as a PasteMsg.
	ta.KeyMap.Paste.SetEnabled(false)
	ta.KeyMap.CopySelection.SetEnabled(false)
	st := textarea.DefaultDarkStyles()
	// A cursor that blinks would need a timer that re-arms itself.
	st.Cursor.Blink = false
	plain := lipgloss.NewStyle()
	st.Focused.Base, st.Focused.Text, st.Focused.CursorLine = plain, plain, plain
	st.Focused.LineNumber, st.Focused.CursorLineNumber = styleDim, styleKey
	st.Focused.EndOfBuffer = styleDim
	st.Blurred = st.Focused
	ta.SetStyles(st)
	ta.SetValue(yaml)
	if ta.Value() != yaml {
		return nil, editorTextCannotHold
	}
	ta.MoveToBegin()
	ta.Focus()
	return &editorView{id: newID(), e: e, ref: ref, ta: ta, was: yaml, size: len(yaml)}, ""
}

// editorTab is what the text area makes of a tab.
const editorTab = "    "

// editorHeld returns s as the text area holds what is pasted into it,
// as its sanitizer makes it, so that it has nothing left to change: a
// CRLF, and a CR alone, become one line feed; a tab becomes editorTab;
// U+FFFD and bytes that are not UTF-8, and every control character but
// the line feed, are dropped. Bidirectional controls are dropped too,
// which the text area would keep: the editor draws its text as it holds
// it, and opens no text that clean would change.
func editorHeld(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r == '\r' || r == '\n':
			b.WriteByte('\n')
		case r == '\t':
			b.WriteString(editorTab)
		case r == utf8.RuneError, unicode.IsControl(r), isBidiControl(r):
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// Init calls onOpen.
func (v *editorView) Init() tea.Cmd {
	if v.onOpen != nil {
		v.onOpen()
		v.onOpen = nil
	}
	return nil
}

// Update takes the editor's keys, pastes, and what came of its update.
func (v *editorView) Update(msg tea.Msg) (view, tea.Cmd) {
	switch msg := msg.(type) {
	case editorSavedMsg:
		if msg.id != v.id {
			return v, nil
		}
		v.saving = false
		if msg.err != nil {
			v.why = errorText(msg.err.Error())
			return v, nil
		}
		return v, tea.Sequence(popView(), flash("Updated "+v.ref.String()), refresh())
	case tea.KeyPressMsg:
		if v.saving {
			return v, nil
		}
		switch msg.String() {
		case "ctrl+s":
			return v, v.save()
		case "esc":
			return v, v.leave()
		}
		return v, v.input(msg)
	case tea.PasteMsg:
		if v.saving {
			return v, nil
		}
		return v, v.input(msg)
	}
	return v, nil
}

// input gives msg to the text area, unless what it adds would take the
// text past editorMaxText bytes or editorTextMaxRows lines: then the
// text and the cursor stay as they were, and the flash says why. A
// paste is measured, and given to the text area, as the text area holds
// it (editorHeld), so that it is taken whole or not at all.
func (v *editorView) input(msg tea.Msg) tea.Cmd {
	added, rows := 0, 0
	switch m := msg.(type) {
	case tea.PasteMsg:
		m.Content = editorHeld(m.Content)
		msg = m
		added = len(m.Content)
		rows = strings.Count(m.Content, "\n")
	case tea.KeyPressMsg:
		added = len(editorHeld(m.Text))
		if m.String() == "enter" {
			added, rows = max(added, 1), 1
		}
	}
	if v.size+added > editorMaxText || v.ta.LineCount()+rows > editorTextMaxRows {
		return func() tea.Msg { return flashMsg{text: editorTextTooLong, err: true} }
	}
	var cmd tea.Cmd
	v.ta, cmd = v.ta.Update(msg)
	text := v.ta.Value()
	v.size = len(text)
	v.modified = text != v.was
	return cmd
}

// save sends the text, when it was changed, as an update of the object
// in a command; text that was not changed closes the editor.
func (v *editorView) save() tea.Cmd {
	text := v.ta.Value()
	if text == v.was {
		return tea.Batch(popView(), flash(actionTextNothingChanged))
	}
	if !actionBegin(v.e.sess) {
		return flash(actionTextBusy)
	}
	v.saving = true
	v.why = ""
	sess, ref, id := v.e.sess, v.ref, v.id
	return waitCmd(func() tea.Msg {
		defer actionEnd(sess)
		_, err := actionCall(apiTimeout, func(ctx context.Context) (string, error) {
			return "", sess.Update(ctx, ref, text)
		})
		return editorSavedMsg{id: id, err: err}
	})
}

// leave closes the editor, and asks first when the text was changed.
func (v *editorView) leave() tea.Cmd {
	if !v.modified {
		return popView()
	}
	return openDialog(dialogConfirm("Discard the changes to "+v.ref.String()+"?", nil, formButton{
		label: "Discard",
		press: func(*formDialog) (tea.Cmd, string) { return popView(), "" },
	}))
}

// View draws the text area over the body, and under it the error of a
// refused update, wrapped onto up to half the lines.
func (v *editorView) View(w, h int) string {
	if w <= 0 || h <= 0 {
		return ""
	}
	var why []string
	if v.why != "" {
		why = v.whyWrap.get(v.why, w, max(h/2, 1))
	}
	th := h - len(why)
	var out []string
	if th > 0 {
		v.ta.SetWidth(w)
		v.ta.SetHeight(th)
		lines := strings.Split(v.ta.View(), "\n")
		for i := range th {
			l := ""
			if i < len(lines) {
				l = lines[i]
			}
			out = append(out, fit(l, w))
		}
	}
	for _, l := range why {
		out = append(out, styleError.Render(fit(l, w)))
	}
	return strings.Join(out, "\n")
}

// Title is "Edit(<ref>)", with " (the first of 3 marked)" when the
// object is the first of several marked rows, " [modified]" once the text was changed
// and " [saving]" while the update runs.
func (v *editorView) Title() string {
	t := "Edit(" + cleanLine(v.ref.String()) + ")" + v.firstOf
	switch {
	case v.saving:
		t += " [saving]"
	case v.modified:
		t += " [modified]"
	}
	return t
}

func (v *editorView) Crumb() string { return "edit" }

func (v *editorView) Hints() []hint {
	return []hint{{"ctrl-s", "Save"}, {"esc", "Back"}}
}

// Capturing: the editor takes every key, but ctrl+c, which the root
// takes first.
func (v *editorView) Capturing() bool  { return true }
func (v *editorView) Fullscreen() bool { return false }

// Close gives back the hold on the session's keys of actions when the
// editor was never opened, as when the stack had no room for it. Init
// gave it back otherwise, and it is given back once.
func (v *editorView) Close() {
	if v.onOpen != nil {
		v.onOpen()
		v.onOpen = nil
	}
}
