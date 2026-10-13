package tui

import (
	"context"
	"errors"
	"strings"

	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/fables-for-robots/kosh/kube"
)

// The editor of an object's YAML (spec §11.2): the text area of bubbles,
// in kosh, with the YAML as it was read; ctrl+s sends it as an update,
// esc leaves and asks first when the text was changed.

// editorMaxLines is the most lines the editor holds. Its line numbers
// are as wide as this number.
const editorMaxLines = 9999

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
	saving   bool
	// why is the error of a refused update, shown until the next key.
	why string
}

// editorSavedMsg is what came of the update of the editor id.
type editorSavedMsg struct {
	id  int64
	err error
}

// The flashes of a YAML the editor will not open.
const (
	editorTextCannotKeep = "this object holds characters the editor cannot keep (a tab or a control character outside a quoted string): edit it with kubectl"
	editorTextCannotHold = "this object's YAML has more lines than the editor holds, or characters it cannot hold: edit it with kubectl"
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
// more lines than editorMaxLines, or anything the text area would not
// hold.
func editorNew(e env, ref kube.ObjectRef, yaml string) (v *editorView, why string) {
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
	ta.MaxHeight = editorMaxLines
	ta.MaxWidth = 0
	ta.CharLimit = 0
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
	return &editorView{id: newID(), e: e, ref: ref, ta: ta, was: yaml}, ""
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
			v.why = cleanLine(msg.err.Error())
			return v, nil
		}
		return v, tea.Sequence(popView(), flash("Updated "+v.ref.String()), refresh())
	case tea.KeyPressMsg:
		if v.saving {
			return v, nil
		}
		v.why = ""
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
		v.why = ""
		return v, v.input(msg)
	}
	return v, nil
}

// input gives msg to the text area.
func (v *editorView) input(msg tea.Msg) tea.Cmd {
	var cmd tea.Cmd
	v.ta, cmd = v.ta.Update(msg)
	v.modified = v.ta.Value() != v.was
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
// refused update.
func (v *editorView) View(w, h int) string {
	if w <= 0 || h <= 0 {
		return ""
	}
	th := h
	if v.why != "" {
		th--
	}
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
	if v.why != "" {
		out = append(out, styleError.Render(fit(v.why, w)))
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
func (v *editorView) Close()           {}
