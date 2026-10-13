package tui

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/fables-for-robots/kosh/kube/kubetest"
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

// dialogCase opens a dialog of an action in a fresh harness of w by h,
// with marked rows when there are several, and returns the harness and
// the labels of the dialog's buttons.
type dialogCase struct {
	name    string
	plural  string
	rows    int
	key     string
	buttons []string
}

func (c dialogCase) open(t *testing.T, w, h int) (*harness, *kubetest.Session) {
	t.Helper()
	sess := newSession()
	var names []string
	for i := range c.rows {
		names = append(names, fmt.Sprintf("x-%02d", i))
	}
	if c.plural == "pods" {
		sess.SetTable(query("pods", "default"), podsNamed(names...))
	}
	hs := newHarness(t, sess, w, h)
	switch c.plural {
	case "deployments":
		for _, n := range names {
			sess.ReplicasOf["deployments/default/"+n] = 2
		}
		actionOpen(hs, sess, "deployments", simpleTable(names...))
	case "nodes":
		actionOpen(hs, sess, "nodes", clusterTable(names...))
	}
	if c.rows > 1 {
		hs.press("space", "G", "ctrl+space")
	}
	hs.press(c.key)
	if dialogOpen(hs) == nil {
		t.Fatalf("%s did not open a dialog at %dx%d:\n%s", c.key, w, h, hs.frame())
	}
	return hs, sess
}

func TestDialogShowsItsButtons(t *testing.T) {
	cases := []dialogCase{
		{"delete 1", "pods", 1, "ctrl+d", []string{"Cancel", "Delete"}},
		{"delete 12", "pods", 12, "ctrl+d", []string{"Cancel", "Delete"}},
		{"delete 60", "pods", 60, "ctrl+d", []string{"Cancel", "Delete"}},
		{"kill", "pods", 1, "ctrl+k", []string{"Cancel", "Kill"}},
		{"scale", "deployments", 1, "s", []string{"Cancel", "Scale"}},
		{"drain 1", "nodes", 1, "r", []string{"Cancel", "Drain"}},
		{"drain 8", "nodes", 8, "r", []string{"Cancel", "Drain"}},
	}
	for _, size := range [][2]int{{80, 24}, {100, 30}, {120, 40}} {
		for _, c := range cases {
			t.Run(fmt.Sprintf("%s at %dx%d", c.name, size[0], size[1]), func(t *testing.T) {
				hs, _ := c.open(t, size[0], size[1])
				// frame checks that no line is wider and the frame not
				// taller than the screen.
				hs.wantFrame(c.buttons...)
				focused := dialogFocused(hs)
				if d := dialogOpen(hs); d.focus < len(d.fields) {
					if hs.line("▸ "+focused) == "" {
						t.Errorf("the field %s has the focus and is not drawn so:\n%s", focused, hs.frame())
					}
				} else if !strings.Contains(hs.drawnLine("[ "+focused+" ]"), sgr(styleCursor)) {
					t.Errorf("the button %s has the focus and is not drawn so:\n%s", focused, hs.frame())
				}
				// The safe button is seen when the focus is on it.
				dialogFocusOn(hs, "Cancel")
				if !strings.Contains(hs.drawnLine("[ Cancel ]"), sgr(styleCursor)) {
					t.Errorf("Cancel has the focus and is not drawn so:\n%s", hs.frame())
				}
				if size[0] != 80 || c.rows < 12 {
					return
				}
				f := hs.frame()
				m := regexp.MustCompile(`… and (\d+) more`).FindStringSubmatch(f)
				if m == nil {
					t.Fatalf("the dialog does not say how many more there are:\n%s", f)
				}
				more, _ := strconv.Atoi(m[1])
				shown := strings.Count(f, "default/x-")
				if shown+more != c.rows {
					t.Errorf("%d names are shown and %d more counted, of %d marked:\n%s", shown, more, c.rows, f)
				}
			})
		}
	}
}

// The delete dialog of 12 marked pods at the standard size.
func TestDialogDeleteOf12At80x24(t *testing.T) {
	hs, _ := dialogCase{"delete 12", "pods", 12, "ctrl+d", nil}.open(t, 80, 24)
	hs.golden()
}

func TestDialogOnATinyScreen(t *testing.T) {
	sess := newSession()
	sess.SetTable(query("pods", "default"), podsNamed("web-1"))
	hs := newHarness(t, sess, 80, 8)
	hs.press("ctrl+d")
	if hs.root().dlg != nil {
		t.Fatal("ctrl+d opened a dialog on a screen of 8 lines")
	}
	// Larger, the screen shows the flash.
	hs.send(tea.WindowSizeMsg{Width: 80, Height: 24})
	hs.wantFrame(dialogTextTooSmall)
	if got := sess.Called("Delete"); len(got) != 0 {
		t.Errorf("Delete was called: %v", got)
	}

	// A dialog that no longer fits when the window is made smaller is
	// closed, and nothing is done.
	hs.press("ctrl+d")
	if hs.root().dlg == nil {
		t.Fatal("ctrl+d opened nothing at 80x24")
	}
	hs.send(tea.WindowSizeMsg{Width: 80, Height: 8})
	if hs.root().dlg != nil {
		t.Error("the dialog stays open on a screen of 8 lines")
	}
	hs.send(tea.WindowSizeMsg{Width: 80, Height: 24})
	hs.wantFrame(dialogTextTooSmall)
	if got := sess.Called("Delete"); len(got) != 0 {
		t.Errorf("Delete was called: %v", got)
	}
}

// A dialog that is not opened frees the keys of actions its read held.
func TestDialogNotOpenedFreesTheKeys(t *testing.T) {
	sess := newSession()
	sess.ReplicasOf["deployments/default/web"] = 3
	hs := newHarness(t, sess, 100, 30)
	actionOpen(hs, sess, "deployments", simpleTable("web"))
	hs.send(tea.WindowSizeMsg{Width: 80, Height: 8})
	hs.press("s")
	if hs.root().dlg != nil {
		t.Fatal("s opened a dialog on a screen of 8 lines")
	}
	if actionBusy(sess) {
		t.Error("the keys of actions are held after the dialog was not opened")
	}
}

// Every frame with a dialog, at every height, is as high as the screen
// and shows the buttons once the dialog is open.
func TestDialogAtEveryHeight(t *testing.T) {
	c := dialogCase{"drain 8", "nodes", 8, "r", nil}
	hs, _ := c.open(t, 80, 40)
	for h := 40; h >= 0; h-- {
		hs.send(tea.WindowSizeMsg{Width: 80, Height: h})
		f := hs.frame()
		if h >= minHeight && hs.root().dlg != nil && !strings.Contains(f, "Drain  ") {
			t.Errorf("at 80x%d the open dialog does not show its buttons:\n%s", h, f)
		}
		if n := len(strings.Split(f, "\n")); h > 0 && n != h && h >= minHeight {
			t.Errorf("at 80x%d the frame has %d lines", h, n)
		}
	}
}
