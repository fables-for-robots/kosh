package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// dialogKey returns the key press of name, for the names the harness
// cannot make: "shift+tab".
func dialogKey(name string) tea.KeyPressMsg {
	if name == "shift+tab" {
		return tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift}
	}
	return keyPress(name)
}

// dialogPress presses keys, shift+tab among them.
func dialogPress(hs *harness, names ...string) {
	hs.t.Helper()
	for _, n := range names {
		hs.send(dialogKey(n))
	}
}

// dialogOpen returns the form dialog that is shown, or nil.
func dialogOpen(hs *harness) *formDialog {
	hs.t.Helper()
	d, _ := hs.root().dlg.(*formDialog)
	return d
}

// dialogFocused returns what has the focus in the dialog that is shown:
// a field's label or a button's.
func dialogFocused(hs *harness) string {
	hs.t.Helper()
	d := dialogOpen(hs)
	if d == nil {
		hs.t.Fatal("no dialog is shown")
	}
	if d.focus < len(d.fields) {
		return d.fields[d.focus].label
	}
	return d.buttons[d.focus-len(d.fields)].label
}

// dialogFocusOn presses tab until label has the focus.
func dialogFocusOn(hs *harness, label string) {
	hs.t.Helper()
	for range 20 {
		if dialogFocused(hs) == label {
			return
		}
		hs.press("tab")
	}
	hs.t.Fatalf("tab does not reach %q", label)
}

// dialogPressed records which button of a test's dialog was pressed.
type dialogPressed struct{ got []string }

func (p *dialogPressed) button(label string) formButton {
	return formButton{label: label, press: func(*formDialog) (tea.Cmd, string) {
		p.got = append(p.got, label)
		return nil, ""
	}}
}

func TestConfirmDialogKeys(t *testing.T) {
	hs := newHarness(t, newSession(), 100, 30)
	var p dialogPressed
	open := func() {
		hs.send(openDialogMsg{dialogConfirm("Kill pods/default/web-1?", nil, p.button("Kill"))})
	}
	open()
	hs.wantFrame("Kill pods/default/web-1?", "[ Cancel ]", "Kill")
	if dialogFocused(hs) != "Cancel" {
		t.Errorf("the focus starts on %q", dialogFocused(hs))
	}
	// y and n are nothing.
	hs.press("y", "n")
	if dialogOpen(hs) == nil || len(p.got) != 0 || dialogFocused(hs) != "Cancel" {
		t.Errorf("y and n did something: pressed %v", p.got)
	}
	// The arrows and tab move the focus.
	hs.press("right")
	if dialogFocused(hs) != "Kill" {
		t.Errorf("right moved the focus to %q", dialogFocused(hs))
	}
	hs.wantFrame("[ Kill ]")
	hs.press("left")
	if dialogFocused(hs) != "Cancel" {
		t.Errorf("left moved the focus to %q", dialogFocused(hs))
	}
	hs.press("tab")
	if dialogFocused(hs) != "Kill" {
		t.Errorf("tab moved the focus to %q", dialogFocused(hs))
	}
	dialogPress(hs, "shift+tab")
	if dialogFocused(hs) != "Cancel" {
		t.Errorf("shift+tab moved the focus to %q", dialogFocused(hs))
	}
	// enter on Cancel closes it and presses nothing.
	hs.press("enter")
	if dialogOpen(hs) != nil || len(p.got) != 0 {
		t.Errorf("enter on Cancel: the dialog is open %v, pressed %v", dialogOpen(hs) != nil, p.got)
	}
	// esc cancels.
	open()
	hs.press("right", "esc")
	if dialogOpen(hs) != nil || len(p.got) != 0 {
		t.Errorf("esc: the dialog is open %v, pressed %v", dialogOpen(hs) != nil, p.got)
	}
	// enter on the action presses it and closes the dialog.
	open()
	hs.press("right", "enter")
	if dialogOpen(hs) != nil || strings.Join(p.got, " ") != "Kill" {
		t.Errorf("enter on Kill: the dialog is open %v, pressed %v", dialogOpen(hs) != nil, p.got)
	}
}

func TestConfirmDialogListsNames(t *testing.T) {
	hs := newHarness(t, newSession(), 100, 30)
	var names []string
	for _, n := range []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j", "k", "l"} {
		names = append(names, "web-"+n)
	}
	hs.send(openDialogMsg{dialogConfirm("Kill 12 pods?", names, formButton{label: "Kill"})})
	hs.wantFrame("Kill 12 pods?", "web-a", "web-j", "… and 2 more")
	hs.notFrame("web-k", "web-l")
}

// The form dialog, with a field of each kind.
func TestFormDialog(t *testing.T) {
	hs := newHarness(t, newSession(), 100, 30)
	var got map[string]string
	ok := formButton{label: "Go", press: func(d *formDialog) (tea.Cmd, string) {
		if d.field("Count").value() == "" {
			return nil, "Count must be a number"
		}
		got = map[string]string{}
		for _, f := range d.fields {
			got[f.label] = f.value()
		}
		return nil, ""
	}}
	open := func() {
		hs.send(openDialogMsg{dialogForm("Do it?", nil, []*formField{
			formTextField("Name", "abc"),
			formNumberField("Count", "7", false),
			formNumberField("Offset", "-1", true),
			formChoiceField("Way", "Here", "There", "Everywhere"),
			formCheckField("Hard", false),
		}, ok)})
	}
	open()
	if dialogFocused(hs) != "Name" {
		t.Errorf("the focus starts on %q", dialogFocused(hs))
	}
	hs.wantFrame("Do it?", "Name", "abc", "Count", "7", "(•) Here", "( ) There", "[ ]", "Cancel", "Go")
	// Text is typed into the field: y and n too, and space.
	hs.typeText("y n")
	// enter on a field goes to the next.
	hs.press("enter")
	if dialogFocused(hs) != "Count" {
		t.Errorf("enter moved the focus to %q", dialogFocused(hs))
	}
	// Only digits are taken.
	hs.press("backspace")
	hs.typeText("x-1.2")
	hs.press("down")
	if dialogFocused(hs) != "Offset" {
		t.Errorf("down moved the focus to %q", dialogFocused(hs))
	}
	// A minus is taken at the start of a field that may be negative.
	hs.press("backspace", "backspace")
	hs.typeText("1-2")
	hs.press("home")
	hs.typeText("-")
	hs.press("tab")
	// space steps through a choice.
	hs.press("space", "space")
	hs.press("tab")
	// space toggles a checkbox.
	hs.press("space")
	hs.wantFrame("[x]", "(•) Everywhere")
	hs.press("up")
	if dialogFocused(hs) != "Way" {
		t.Errorf("up moved the focus to %q", dialogFocused(hs))
	}
	hs.press("space")
	dialogPress(hs, "shift+tab")
	if dialogFocused(hs) != "Offset" {
		t.Errorf("shift+tab moved the focus to %q", dialogFocused(hs))
	}
	hs.press("tab", "tab", "tab")
	if dialogFocused(hs) != "Cancel" {
		t.Errorf("the first button is %q", dialogFocused(hs))
	}
	hs.press("tab", "enter")
	want := map[string]string{"Name": "abcy n", "Count": "12", "Offset": "-12", "Way": "Here", "Hard": "true"}
	if dialogOpen(hs) != nil {
		t.Error("the dialog is open after Go")
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s is %q, want %q (all: %v)", k, got[k], v, got)
		}
	}

	// A form that does not validate says why and stays.
	got = nil
	open()
	hs.press("tab", "ctrl+u")
	dialogFocusOn(hs, "Go")
	hs.press("enter")
	if dialogOpen(hs) == nil || got != nil {
		t.Fatal("a form that does not validate closed")
	}
	hs.wantFrame("Count must be a number")
	if !strings.Contains(hs.drawnLine("Count must be a number"), sgr(styleError)) {
		t.Error("why is not in the error style")
	}
	// enter held down from the first field cancels.
	hs.send(closeDialogMsg{})
	open()
	for range 6 {
		hs.press("enter")
	}
	if dialogOpen(hs) != nil || got != nil {
		t.Errorf("enter held down: the dialog is open %v, pressed with %v", dialogOpen(hs) != nil, got)
	}
}

// A paste goes into a text field, and is filtered as typed text is in a
// number field.
func TestFormDialogPaste(t *testing.T) {
	hs := newHarness(t, newSession(), 100, 30)
	d := dialogForm("Do it?", nil, []*formField{formTextField("Name", ""), formNumberField("Count", "", false)}, formButton{label: "Go"})
	hs.send(openDialogMsg{d})
	hs.send(tea.PasteMsg{Content: "web"})
	hs.press("tab")
	hs.send(tea.PasteMsg{Content: "12a"})
	if d.fields[0].value() != "web" || d.fields[1].value() != "" {
		t.Errorf("the fields are %q and %q", d.fields[0].value(), d.fields[1].value())
	}
}

// No field reads the clipboard of the machine kosh runs on.
func TestFormFieldsUseNoClipboardOfTheServer(t *testing.T) {
	for _, f := range []*formField{formTextField("a", ""), formNumberField("b", "", true)} {
		if f.in.KeyMap.Paste.Enabled() {
			t.Errorf("ctrl+v in %s reads the clipboard of the server", f.label)
		}
	}
}

func TestDialogFitsTheWindow(t *testing.T) {
	hs := newHarness(t, newSession(), 80, 12)
	long := strings.Repeat("x", 200)
	hs.send(openDialogMsg{dialogForm("Do "+long+"?", []string{long, long}, []*formField{formTextField("Name "+long, long)}, formButton{label: "Go"})})
	hs.frame() // checks every line's width and the number of lines
}
