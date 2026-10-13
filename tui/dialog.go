package tui

import (
	"fmt"
	"strconv"
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// The dialogs that ask before something is done. A dialog has a
// question, the names it is about, fields, and buttons, of which the
// first is always Cancel: the focus starts on the first field, or on
// Cancel when there is none, so that neither a key pressed by mistake
// nor enter held down changes anything. An answer is always two keys:
// one to move the focus to the button, enter to press it.

// dialogWidth is the width of a dialog in a window that has room for it.
const dialogWidth = 64

// dialogNames is how many names a dialog lists; the others are counted.
const dialogNames = 10

// textCancel is the label of the button that changes nothing.
const textCancel = "Cancel"

// formKind is the kind of a field of a form.
type formKind int

const (
	formText   formKind = iota // a line of text
	formNumber                 // a whole number
	formChoice                 // one of several options
	formCheck                  // a checkbox
)

// formField is one field of a form dialog.
type formField struct {
	kind  formKind
	label string
	// in holds a text or number field's text.
	in textinput.Model
	// signed: a number field may be negative.
	signed bool
	// options and at are a choice and the option chosen.
	options []string
	at      int
	// on is a checkbox's state.
	on bool
}

// newTextField returns a field of a line of text, filled with value.
func newTextField(label, value string) *formField {
	return &formField{kind: formText, label: label, in: formInput(value)}
}

// newNumberField returns a field that takes only digits, and a minus at
// its start when signed.
func newNumberField(label, value string, signed bool) *formField {
	return &formField{kind: formNumber, label: label, in: formInput(value), signed: signed}
}

// newChoiceField returns a choice of options, the first one chosen.
func newChoiceField(label string, options ...string) *formField {
	return &formField{kind: formChoice, label: label, options: options}
}

// newCheckField returns a checkbox.
func newCheckField(label string, on bool) *formField {
	return &formField{kind: formCheck, label: label, on: on}
}

// formInput returns the text input of a field: no blinking cursor, and
// no ctrl+v, which would read the clipboard of the machine kosh runs on
// rather than the user's (a paste of the user's terminal still comes).
func formInput(value string) textinput.Model {
	in := textinput.New()
	in.Prompt = ""
	st := textinput.DefaultDarkStyles()
	st.Cursor.Blink = false
	st.Focused.Text = stylePromptText
	st.Blurred.Text = stylePromptText
	in.SetStyles(st)
	in.KeyMap.Paste.SetEnabled(false)
	in.SetValue(value)
	return in
}

// value returns the field's value: the text, the option chosen, or
// "true" or "false".
func (f *formField) value() string {
	switch f.kind {
	case formChoice:
		if f.at < len(f.options) {
			return f.options[f.at]
		}
		return ""
	case formCheck:
		return strconv.FormatBool(f.on)
	}
	return f.in.Value()
}

// takes reports whether a number field takes s.
func (f *formField) takes(s string) bool {
	if f.kind != formNumber {
		return true
	}
	if f.signed {
		s = strings.TrimPrefix(s, "-")
	}
	return !strings.ContainsFunc(s, func(r rune) bool { return r < '0' || r > '9' })
}

// input gives msg to a text or number field. A number field keeps its
// text when msg would put into it what it does not take.
func (f *formField) input(msg tea.Msg) tea.Cmd {
	if f.kind != formText && f.kind != formNumber {
		return nil
	}
	before, pos := f.in.Value(), f.in.Position()
	var cmd tea.Cmd
	f.in, cmd = f.in.Update(msg)
	if !f.takes(f.in.Value()) {
		f.in.SetValue(before)
		f.in.SetCursor(pos)
	}
	return cmd
}

// formButton is a button of a dialog.
type formButton struct {
	label string
	// press is what the button does: a command, after which the dialog
	// closes, or why the form's values are not good, which the dialog
	// shows and stays open. Nil changes nothing and closes the dialog.
	press func(d *formDialog) (tea.Cmd, string)
}

// formDialog is a dialog with fields: the confirm dialog is one without.
type formDialog struct {
	question string
	names    []string
	more     int
	fields   []*formField
	// buttons start with Cancel.
	buttons []formButton
	// focus is a field's index, or len(fields) plus a button's.
	focus int
	// why is why the values are not good, after a button said so.
	why string
}

// newConfirmDialog returns a dialog that asks question about names,
// with the buttons Cancel and then buttons. Cancel has the focus.
func newConfirmDialog(question string, names []string, buttons ...formButton) *formDialog {
	return newFormDialog(question, names, nil, buttons...)
}

// newFormDialog returns a dialog with fields, and the buttons Cancel and
// then buttons. The first field has the focus, or Cancel when there is
// none. The question and the names are cleaned; ten names are listed
// and the others counted.
func newFormDialog(question string, names []string, fields []*formField, buttons ...formButton) *formDialog {
	d := &formDialog{
		question: cleanLine(question),
		fields:   fields,
		buttons:  append([]formButton{{label: textCancel}}, buttons...),
	}
	for i, n := range names {
		if i == dialogNames {
			d.more = len(names) - dialogNames
			break
		}
		d.names = append(d.names, cleanLine(n))
	}
	d.setFocus(0)
	return d
}

// field returns the field of label, or nil.
func (d *formDialog) field(label string) *formField {
	for _, f := range d.fields {
		if f.label == label {
			return f
		}
	}
	return nil
}

// focused returns the field that has the focus, or nil when a button
// has it.
func (d *formDialog) focused() *formField {
	if d.focus < len(d.fields) {
		return d.fields[d.focus]
	}
	return nil
}

// setFocus moves the focus to i, around the ends.
func (d *formDialog) setFocus(i int) {
	n := len(d.fields) + len(d.buttons)
	d.focus = ((i % n) + n) % n
	for j, f := range d.fields {
		if f.kind != formText && f.kind != formNumber {
			continue
		}
		// The cursor does not blink (formInput), so Focus returns no
		// command.
		if j == d.focus {
			f.in.Focus()
		} else {
			f.in.Blur()
		}
	}
}

func (d *formDialog) Init() tea.Cmd { return nil }

// Update takes the dialog's keys and pastes; it has no messages of its
// own, and every other message is not for it.
func (d *formDialog) Update(msg tea.Msg) (dialog, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		return d, d.key(msg)
	case tea.PasteMsg:
		if f := d.focused(); f != nil {
			return d, f.input(msg)
		}
	}
	return d, nil
}

// key handles a key press.
func (d *formDialog) key(k tea.KeyPressMsg) tea.Cmd {
	d.why = ""
	f := d.focused()
	switch k.String() {
	case "esc":
		return closeDialog()
	case "tab", "down":
		d.setFocus(d.focus + 1)
		return nil
	case "shift+tab", "up":
		d.setFocus(d.focus - 1)
		return nil
	case "enter":
		if f != nil {
			d.setFocus(d.focus + 1)
			return nil
		}
		return d.press(d.buttons[d.focus-len(d.fields)])
	case "left", "right":
		if f == nil {
			step := 1
			if k.String() == "left" {
				step = -1
			}
			first, last := len(d.fields), len(d.fields)+len(d.buttons)-1
			d.setFocus(max(first, min(last, d.focus+step)))
			return nil
		}
	case "space":
		if f == nil {
			return nil
		}
		switch f.kind {
		case formChoice:
			f.at = (f.at + 1) % max(len(f.options), 1)
			return nil
		case formCheck:
			f.on = !f.on
			return nil
		}
	}
	if f != nil {
		return f.input(k)
	}
	return nil
}

// press presses a button.
func (d *formDialog) press(b formButton) tea.Cmd {
	if b.press == nil {
		return closeDialog()
	}
	cmd, why := b.press(d)
	if why != "" {
		d.why = why
		return nil
	}
	return tea.Batch(closeDialog(), cmd)
}

// View draws the dialog in a box at most width cells wide.
func (d *formDialog) View(width int) string {
	bw := min(width, dialogWidth)
	iw := max(bw-4, 1)
	var body []string
	body = append(body, "", styleTitle.Render(fit(d.question, iw)))
	for _, n := range d.names {
		body = append(body, styleText.Render(fit("  "+n, iw)))
	}
	if d.more > 0 {
		body = append(body, styleText.Render(fit(fmt.Sprintf("  … and %d more", d.more), iw)))
	}
	body = append(body, "")
	if len(d.fields) > 0 {
		lw := 0
		for _, f := range d.fields {
			lw = max(lw, ansi.StringWidth(f.label))
		}
		lw = min(lw, iw/2)
		for i, f := range d.fields {
			body = append(body, d.fieldLine(f, i == d.focus, lw, iw))
		}
		body = append(body, "")
	}
	if d.why != "" {
		body = append(body, styleError.Render(fit(d.why, iw)), "")
	}
	body = append(body, d.buttonLine(iw), "")

	side := styleBorder.Render("│")
	lines := []string{styleBorder.Render("╭" + strings.Repeat("─", bw-2) + "╮")}
	for _, l := range body {
		lines = append(lines, side+" "+fit(l, iw)+" "+side)
	}
	lines = append(lines, styleBorder.Render("╰"+strings.Repeat("─", bw-2)+"╯"))
	return strings.Join(lines, "\n")
}

// fieldLine draws a field: a mark when it has the focus, its label, its
// value.
func (d *formDialog) fieldLine(f *formField, focused bool, lw, iw int) string {
	mark := "  "
	if focused {
		mark = styleKey.Render("▸ ")
	}
	label := styleHint.Render(fit(f.label, lw))
	vw := max(iw-2-lw-2, 1)
	var value string
	switch f.kind {
	case formChoice:
		var opts []string
		for i, o := range f.options {
			box := "( ) "
			if i == f.at {
				box = "(•) "
			}
			opts = append(opts, box+o)
		}
		value = stylePromptText.Render(strings.Join(opts, "  "))
	case formCheck:
		value = stylePromptText.Render("[ ]")
		if f.on {
			value = stylePromptText.Render("[x]")
		}
	default:
		f.in.SetWidth(max(vw-1, 1))
		value = f.in.View()
	}
	return mark + label + "  " + fit(value, vw)
}

// buttonLine draws the buttons, centred; the one that has the focus in
// brackets and the cursor's style.
func (d *formDialog) buttonLine(iw int) string {
	var parts []string
	for i, b := range d.buttons {
		if len(d.fields)+i == d.focus {
			parts = append(parts, styleCursor.Render("[ "+b.label+" ]"))
		} else {
			parts = append(parts, styleHint.Render("  "+b.label+"  "))
		}
	}
	line := strings.Join(parts, "  ")
	pad := max((iw-ansi.StringWidth(line))/2, 0)
	return strings.Repeat(" ", pad) + line
}
