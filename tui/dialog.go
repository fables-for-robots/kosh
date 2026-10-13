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

// dialogNames is how many names a dialog lists at most; the others are
// counted. A screen with fewer lines lists fewer.
const dialogNames = 10

// dialogWhyLines is how many lines the reason a dialog gives is drawn on
// at most: the last one ends in "…" when there were more. A reason worded
// by the cluster may be of any length.
const dialogWhyLines = flashMaxLines

// dialogCancel is the label of the button that changes nothing.
const dialogCancel = "Cancel"

// dialogTextTooSmall is the flash of a dialog that the screen has too few
// lines for: it is not opened, or it is closed, and nothing is done.
const dialogTextTooSmall = "the window is too small for this dialog: nothing was done"

// formKind is the kind of a field of a form.
type formKind int

const (
	formIsText   formKind = iota // a line of text
	formIsNumber                 // a whole number
	formIsChoice                 // one of several options
	formIsCheck                  // a checkbox
	formIsFixed                  // a value shown that cannot be changed
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
	// pick: a choice drawn as the option chosen alone, ‹ like this ›,
	// which left and right change as space does.
	pick bool
	// fixed is a fixed field's value. The focus passes such a field by.
	fixed string
	// presses is the label of the button that enter presses in a text
	// field. Empty, enter moves the focus to what follows.
	presses string
	// scrolls: a text field longer than its width shows the part about
	// its cursor from the first frame on, and not only after a key.
	scrolls bool
}

// formTextField returns a field of a line of text, filled with value.
func formTextField(label, value string) *formField {
	return &formField{kind: formIsText, label: label, in: formInput(value)}
}

// formNumberField returns a field that takes only digits, and a minus at
// its start when signed.
func formNumberField(label, value string, signed bool) *formField {
	return &formField{kind: formIsNumber, label: label, in: formInput(value), signed: signed}
}

// formChoiceField returns a choice of options, the first one chosen.
func formChoiceField(label string, options ...string) *formField {
	return &formField{kind: formIsChoice, label: label, options: options}
}

// formPickField returns a choice of options, the one at at chosen,
// drawn as the option chosen alone and changed with left and right.
func formPickField(label string, at int, options ...string) *formField {
	return &formField{kind: formIsChoice, label: label, options: options, at: at, pick: true}
}

// formFixedField returns a field that shows value and cannot be
// changed; the focus passes it by.
func formFixedField(label, value string) *formField {
	return &formField{kind: formIsFixed, label: label, fixed: value}
}

// formCheckField returns a checkbox.
func formCheckField(label string, on bool) *formField {
	return &formField{kind: formIsCheck, label: label, on: on}
}

// formInput returns the text input of a field: no blinking cursor, at
// most inputMaxChars characters, and no ctrl+v, which would read the
// clipboard of the machine kosh runs on rather than the user's (a paste
// of the user's terminal still comes).
func formInput(value string) textinput.Model {
	in := textinput.New()
	in.Prompt = ""
	st := textinput.DefaultDarkStyles()
	st.Cursor.Blink = false
	st.Focused.Text = stylePromptText
	st.Blurred.Text = stylePromptText
	in.SetStyles(st)
	in.CharLimit = inputMaxChars
	in.KeyMap.Paste.SetEnabled(false)
	in.SetValue(value)
	return in
}

// value returns the field's value: the text, the option chosen, or
// "true" or "false".
func (f *formField) value() string {
	switch f.kind {
	case formIsChoice:
		if f.at < len(f.options) {
			return f.options[f.at]
		}
		return ""
	case formIsCheck:
		return strconv.FormatBool(f.on)
	case formIsFixed:
		return f.fixed
	}
	return f.in.Value()
}

// takes reports whether a number field takes s.
func (f *formField) takes(s string) bool {
	if f.kind != formIsNumber {
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
	if f.kind != formIsText && f.kind != formIsNumber {
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
	// after: the command runs once the dialog has closed, rather than
	// beside the closing: a command after which the dialog may be opened
	// again needs the close to have been taken first.
	after bool
}

// formDialog is a dialog with fields: the confirm dialog is one without.
type formDialog struct {
	// title, when there is one, is drawn in the box's top line.
	title    string
	question string
	// names are all the names the dialog is about, cleaned; View lists
	// as many as fit and counts the others.
	names  []string
	fields []*formField
	// buttons start with Cancel.
	buttons []formButton
	// focus is a field's index, or len(fields) plus a button's.
	focus int
	// why is why the values are not good, after a button said so.
	why string
	// onOpen, when set, is called once, when the dialog is opened.
	onOpen func()
	// onCancel, when set, is called once, when the dialog closes with
	// nothing done: on esc, on a button without a press (Cancel), and on
	// Drop. It is not called after a button's press has closed it.
	onCancel func()
}

// dialogConfirm returns a dialog that asks question about names,
// with the buttons Cancel and then buttons. Cancel has the focus.
func dialogConfirm(question string, names []string, buttons ...formButton) *formDialog {
	return dialogForm(question, names, nil, buttons...)
}

// dialogForm returns a dialog with fields, and the buttons Cancel and
// then buttons. The first field has the focus, or Cancel when there is
// none. The question and the names are cleaned; at most dialogNames
// names are listed, as many as the screen has lines for, and the others
// counted.
func dialogForm(question string, names []string, fields []*formField, buttons ...formButton) *formDialog {
	d := &formDialog{
		question: cleanLine(question),
		fields:   fields,
		buttons:  append([]formButton{{label: dialogCancel}}, buttons...),
	}
	for _, n := range names {
		d.names = append(d.names, cleanLine(n))
	}
	d.setFocus(0)
	if !d.focusable(0) {
		d.moveFocus(1)
	}
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
		if f.kind != formIsText && f.kind != formIsNumber {
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

// focusable reports whether the focus may stop at i: at a button, or at
// a field that is not fixed.
func (d *formDialog) focusable(i int) bool {
	return i >= len(d.fields) || d.fields[i].kind != formIsFixed
}

// moveFocus moves the focus by step, around the ends, past the fields
// that are fixed.
func (d *formDialog) moveFocus(step int) {
	n := len(d.fields) + len(d.buttons)
	i := d.focus
	for range n {
		i = ((i+step)%n + n) % n
		if d.focusable(i) {
			break
		}
	}
	d.setFocus(i)
}

// Init calls onOpen.
func (d *formDialog) Init() tea.Cmd {
	d.opened()
	return nil
}

// Drop calls onOpen and onCancel, once each: what they release was held
// until the dialog opened, or while it was open, and is released as well
// when it is not opened or is closed by the root.
func (d *formDialog) Drop() {
	d.opened()
	d.cancelled()
}

// opened calls onOpen, once.
func (d *formDialog) opened() {
	if d.onOpen != nil {
		d.onOpen()
		d.onOpen = nil
	}
}

// cancelled calls onCancel, once.
func (d *formDialog) cancelled() {
	if d.onCancel != nil {
		d.onCancel()
		d.onCancel = nil
	}
}

// Update takes the dialog's keys and pastes; it has no messages of its
// own, and every other message is not for it.
func (d *formDialog) Update(msg tea.Msg) (dialog, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		return d, d.key(msg)
	case tea.PasteMsg:
		if f := d.focused(); f != nil {
			// Cleaned, on one line, and cut to what a field takes.
			return d, f.input(tea.PasteMsg{Content: inputCut(cleanLine(msg.Content))})
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
		d.cancelled()
		return closeDialog()
	case "tab", "down":
		d.moveFocus(1)
		return nil
	case "shift+tab", "up":
		d.moveFocus(-1)
		return nil
	case "enter":
		if f != nil && f.presses != "" {
			for _, b := range d.buttons {
				if b.label == f.presses {
					return d.press(b)
				}
			}
		}
		if f != nil {
			d.moveFocus(1)
			return nil
		}
		return d.press(d.buttons[d.focus-len(d.fields)])
	case "left", "right":
		if f != nil && f.pick {
			step := 1
			if k.String() == "left" {
				step = -1
			}
			n := max(len(f.options), 1)
			f.at = ((f.at+step)%n + n) % n
			return nil
		}
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
		case formIsChoice:
			f.at = (f.at + 1) % max(len(f.options), 1)
			return nil
		case formIsCheck:
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
		d.cancelled()
		return closeDialog()
	}
	cmd, why := b.press(d)
	if why != "" {
		d.why = why
		return nil
	}
	// What onCancel would release is the press's now.
	d.onCancel = nil
	if b.after {
		return tea.Sequence(closeDialog(), cmd)
	}
	return tea.Batch(closeDialog(), cmd)
}

// View draws the dialog in a box at most width cells wide and height
// lines high. What gives way when the lines are few: first the names,
// down to "… and N more" alone; then the blank lines; then, from the
// top, the question. The fields and the buttons are always drawn.
func (d *formDialog) View(width, height int) string {
	if width <= 0 || height <= 0 {
		return ""
	}
	bw := max(min(width, dialogWidth), 5)
	iw := bw - 4
	room := max(height-2, 0)
	body, ok := d.body(iw, room, true)
	if !ok {
		body, _ = d.body(iw, room, false)
	}
	if len(body) > room {
		body = body[len(body)-room:]
	}
	side := styleBorder.Render("│")
	lines := []string{d.topLine(bw)}
	for _, l := range body {
		lines = append(lines, side+" "+fit(l, iw)+" "+side)
	}
	lines = append(lines, styleBorder.Render("╰"+strings.Repeat("─", bw-2)+"╯"))
	if len(lines) > height {
		lines = lines[len(lines)-height:]
	}
	return strings.Join(lines, "\n")
}

// topLine is the box's top line, bw cells wide, with the title in its
// middle when the dialog has one.
func (d *formDialog) topLine(bw int) string {
	if d.title == "" {
		return styleBorder.Render("╭" + strings.Repeat("─", bw-2) + "╮")
	}
	text := " " + ansi.Truncate(d.title, max(bw-6, 1), "…") + " "
	tw := ansi.StringWidth(text)
	left := max((bw-2-tw)/2, 0)
	right := max(bw-2-tw-left, 0)
	return styleBorder.Render("╭"+strings.Repeat("─", left)) + styleTitle.Render(text) + styleBorder.Render(strings.Repeat("─", right)+"╮")
}

// Fits reports whether the dialog, drawn at most width cells wide, shows
// its question, its fields and its buttons in height lines, and when it
// has names at least the line that counts them.
func (d *formDialog) Fits(width, height int) bool {
	if width <= 0 || height < 2 {
		return false
	}
	iw := max(min(width, dialogWidth), 5) - 4
	_, ok := d.body(iw, height-2, false)
	return ok
}

// body returns the lines inside the box, iw cells wide, for room lines:
// as many names as fit with the rest, at most dialogNames, and the line
// that counts the others; blank lines between the parts when blanks is
// set. ok is false when the lines are more than room even with no name
// listed.
func (d *formDialog) body(iw, room int, blanks bool) (lines []string, ok bool) {
	blank := func(ls []string) []string {
		if blanks {
			return append(ls, "")
		}
		return ls
	}
	top := blank(nil)
	for _, l := range dialogWrap(d.question, iw) {
		top = append(top, styleTitle.Render(l))
	}
	var bottom []string
	bottom = blank(bottom)
	if len(d.fields) > 0 {
		lw := 0
		for _, f := range d.fields {
			lw = max(lw, ansi.StringWidth(f.label))
		}
		lw = min(lw, iw/2)
		for i, f := range d.fields {
			bottom = append(bottom, d.fieldLine(f, i == d.focus, lw, iw))
		}
		bottom = blank(bottom)
	}
	if d.why != "" {
		why := dialogWrap(d.why, iw)
		if len(why) > dialogWhyLines {
			why = why[:dialogWhyLines]
			why[dialogWhyLines-1] = fit(strings.TrimRight(why[dialogWhyLines-1], " ")+" …", iw)
		}
		for _, l := range why {
			bottom = append(bottom, styleError.Render(l))
		}
		bottom = blank(bottom)
	}
	bottom = blank(append(bottom, d.buttonLine(iw)))

	n := len(d.names)
	shown, more := n, 0
	if left := room - len(top) - len(bottom); n > dialogNames || n > left {
		shown = max(min(n, dialogNames, left-1), 0)
		more = n - shown
	}
	lines = top
	for _, name := range d.names[:shown] {
		lines = append(lines, styleText.Render(fit("  "+name, iw)))
	}
	if more > 0 {
		lines = append(lines, styleText.Render(fit(fmt.Sprintf("  … and %d more", more), iw)))
	}
	lines = append(lines, bottom...)
	return lines, len(lines) <= room
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
	switch {
	case f.kind == formIsFixed:
		value = stylePromptText.Render(f.fixed)
	case f.kind == formIsChoice && f.pick:
		value = stylePromptText.Render("‹ " + f.value() + " ›")
	case f.kind == formIsChoice:
		var opts []string
		for i, o := range f.options {
			box := "( ) "
			if i == f.at {
				box = "(•) "
			}
			opts = append(opts, box+o)
		}
		value = stylePromptText.Render(strings.Join(opts, "  "))
	case f.kind == formIsCheck:
		value = stylePromptText.Render("[ ]")
		if f.on {
			value = stylePromptText.Render("[x]")
		}
	default:
		f.in.SetWidth(max(vw-1, 1))
		if f.scrolls {
			// The text is scrolled to the cursor for this width.
			f.in.SetCursor(f.in.Position())
		}
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

// dialogWrap wraps one line of plain text into lines of w cells at
// most, at spaces where it can and in a word where it must.
func dialogWrap(s string, w int) []string {
	lines := strings.Split(ansi.Wrap(s, w, ""), "\n")
	for i, l := range lines {
		lines[i] = fit(strings.TrimRight(l, " "), w)
	}
	return lines
}
