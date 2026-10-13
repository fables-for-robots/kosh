package tui

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/fables-for-robots/kosh/kube"
	"github.com/fables-for-robots/kosh/kube/kubetest"
)

// editorYAML is the YAML of the configmap of the tests.
const editorYAML = `apiVersion: v1
data:
  greeting: hello
kind: ConfigMap
metadata:
  name: cfg
  namespace: default
`

// editorRef is the configmap of the tests.
var editorRef = kubetest.Ref("configmaps", "default", "cfg")

// editorStart opens the configmaps of sess, with cfg and its YAML, and
// presses e when open is true.
func editorStart(t *testing.T, sess *kubetest.Session, open bool) *harness {
	t.Helper()
	if _, ok := sess.YAMLs[editorRef.String()]; !ok {
		sess.YAMLs[editorRef.String()] = editorYAML
	}
	hs := newHarness(t, sess, 100, 30)
	actionOpen(hs, sess, "configmaps", simpleTable("cfg"))
	if open {
		hs.press("e")
		editorShown(hs)
	}
	return hs
}

// editorShown returns the editor on top of the stack, or fails.
func editorShown(hs *harness) *editorView {
	hs.t.Helper()
	v, ok := hs.root().top().(*editorView)
	if !ok {
		hs.t.Fatalf("the top view is a %T, not the editor", hs.root().top())
	}
	return v
}

// editorClosed reports whether the table is on top again.
func editorClosed(hs *harness) bool {
	_, ok := hs.root().top().(*tableView)
	return ok && len(hs.root().stack) == 1
}

func TestEditorOpens(t *testing.T) {
	sess := newSession()
	hs := editorStart(t, sess, true)
	if hs.title() != "Edit(configmaps/default/cfg)" {
		t.Errorf("the title is %q", hs.title())
	}
	hs.wantFrame("<configmaps> <edit>", "1 apiVersion: v1", "7   namespace: default")
	if got := sess.Called("YAML"); len(got) != 1 || got[0].Args[1] != false {
		t.Errorf("YAML was called %v", got)
	}
	hs.golden()
}

func TestEditorTakesEveryKey(t *testing.T) {
	hs := editorStart(t, newSession(), true)
	hs.press(":", "?", "/", "ctrl+a", "ctrl+e", "[", "-", "0")
	v := editorShown(hs)
	if hs.root().prompt.open() {
		t.Error("a prompt opened")
	}
	// ctrl+a and ctrl+e are the text area's: the start and the end of
	// the line.
	if first, _, _ := strings.Cut(v.ta.Value(), "\n"); first != ":?/apiVersion: v1[-0" {
		t.Errorf("the first line is %q", first)
	}
	if hs.title() != "Edit(configmaps/default/cfg) [modified]" {
		t.Errorf("the title is %q", hs.title())
	}
	// A key gives no command back: no cursor that blinks, no clipboard.
	if _, cmd := v.Update(keyPress("x")); cmd != nil {
		t.Error("a key in the editor returns a command")
	}
	if v.Init() != nil {
		t.Error("the editor's Init returns a command")
	}
	// ctrl+c still ends the session.
	hs.press("ctrl+c")
	if !hs.quit {
		t.Error("ctrl+c in the editor did not end the session")
	}
}

// No key of the editor reads or writes the clipboard of the machine kosh
// runs on.
func TestEditorUsesNoClipboardOfTheServer(t *testing.T) {
	hs := editorStart(t, newSession(), true)
	v := editorShown(hs)
	if v.ta.KeyMap.Paste.Enabled() || v.ta.KeyMap.CopySelection.Enabled() {
		t.Error("a key of the editor uses the clipboard of the server")
	}
}

func TestEditorSaveUnchanged(t *testing.T) {
	sess := newSession()
	hs := editorStart(t, sess, true)
	hs.press("ctrl+s")
	hs.wantFrame("nothing changed")
	if !editorClosed(hs) {
		t.Error("the editor is open")
	}
	if got := sess.Called("Update"); len(got) != 0 {
		t.Errorf("Update was called %v", got)
	}
}

func TestEditorSave(t *testing.T) {
	sess := newCountingSession()
	sess.YAMLs[editorRef.String()] = editorYAML
	hs := newHarness(t, sess, 100, 30)
	q := actionOpen(hs, sess.Session, "configmaps", simpleTable("cfg"))
	reads := len(sess.readsOf(q))
	hs.press("e")
	hs.typeText("# x")
	hs.press("ctrl+s")
	hs.awaitFrame("Updated configmaps/default/cfg")
	got := sess.Called("Update")
	if len(got) != 1 {
		t.Fatalf("Update was called %d times", len(got))
	}
	if ref := got[0].Args[0].(kube.ObjectRef); ref.String() != editorRef.String() || ref.Resource.GVR != editorRef.Resource.GVR {
		t.Errorf("Update was called for %v", ref)
	}
	if text := got[0].Args[1].(string); text != "# x"+editorYAML {
		t.Errorf("Update was given %q", text)
	}
	if !editorClosed(hs) {
		t.Error("the editor is open")
	}
	if len(sess.readsOf(q)) <= reads {
		t.Error("the table did not read again")
	}
}

func TestEditorSaveRefused(t *testing.T) {
	sess := newSession()
	sess.Errs["Update"] = errors.New("Operation cannot be fulfilled: the object has been modified\x1b[2J")
	hs := editorStart(t, sess, true)
	hs.typeText("x: y")
	hs.press("enter")
	hs.press("ctrl+s")
	hs.awaitFrame("the object has been modified")
	v := editorShown(hs)
	if want := "x: y\n" + editorYAML; v.ta.Value() != want {
		t.Errorf("the text is %q, want %q", v.ta.Value(), want)
	}
	if strings.Contains(hs.drawn(), "\x1b[2J") {
		t.Error("the error is drawn as the session gave it")
	}
	if !strings.Contains(hs.drawnLine("the object has been modified"), sgr(styleError)) {
		t.Error("the error is not in the error style")
	}
	if got := sess.Called("Update"); len(got) != 1 || got[0].Args[1] != v.ta.Value() {
		t.Errorf("Update was called %v", got)
	}
	// The error stays while the text is edited (TestEditorErrorStays).
	hs.press("right")
	hs.wantFrame("the object has been modified")
	if v.ta.Value() != "x: y\n"+editorYAML {
		t.Error("right changed the text")
	}
}

func TestEditorEscape(t *testing.T) {
	sess := newSession()
	hs := editorStart(t, sess, true)
	hs.press("esc")
	if !editorClosed(hs) {
		t.Fatal("esc on unchanged text did not close the editor")
	}

	hs.press("e")
	hs.typeText("zz")
	hs.press("esc")
	hs.wantFrame("Discard the changes to configmaps/default/cfg?", "Discard")
	if dialogFocused(hs) != "Cancel" {
		t.Errorf("the focus starts on %q", dialogFocused(hs))
	}
	hs.press("enter")
	v := editorShown(hs)
	if hs.root().dlg != nil || v.ta.Value() != "zz"+editorYAML {
		t.Errorf("Cancel: the dialog is open %v, the text is %q", hs.root().dlg != nil, v.ta.Value())
	}
	hs.press("esc", "right", "enter")
	if !editorClosed(hs) {
		t.Error("Discard did not close the editor")
	}
	if got := sess.Called("Update"); len(got) != 0 {
		t.Errorf("Update was called %v", got)
	}
}

func TestEditorYAMLErrorIsAFlash(t *testing.T) {
	sess := newSession()
	sess.Errs["YAML"] = errors.New("configmaps \"cfg\" not found")
	hs := editorStart(t, sess, false)
	hs.press("e")
	hs.wantFrame(`configmaps "cfg" not found`)
	if !editorClosed(hs) {
		t.Error("an editor opened")
	}
}

// editorCannotKeep is the flash of a YAML the editor would change.
const editorCannotKeep = "this object holds characters the editor cannot keep (a tab or a control character outside a quoted string): edit it with kubectl"

// The editor sends nothing but what was read and what was typed: a YAML
// that cleaning would change opens no editor, and its flash draws
// nothing of it raw.
func TestEditorRefusesWhatCleaningWouldChange(t *testing.T) {
	for name, yaml := range map[string]string{
		"a tab in a block scalar":  "data:\n  Makefile: |\n    all:\n    \techo hi\n",
		"an escape sequence":       "data:\n  title: \x1b]0;x\x07owned\n",
		"a bell":                   "data:\n  bell: \x07\n",
		"a carriage return":        "data:\n  a: b\r\n",
		"bytes that are not UTF-8": "data:\n  a: \xff\n",
	} {
		t.Run(name, func(t *testing.T) {
			sess := newSession()
			sess.YAMLs[editorRef.String()] = yaml
			hs := editorStart(t, sess, false)
			hs.press("e")
			if !editorClosed(hs) {
				t.Fatal("an editor opened")
			}
			hs.wantFrame("this object holds characters the editor cannot keep")
			if !strings.Contains(hs.drawnLine("cannot keep"), sgr(styleError)) {
				t.Error("the flash is not in the error style")
			}
			for _, bad := range []string{"\x1b]0;", "\x07", "\t", "\r"} {
				if strings.Contains(hs.drawn(), bad) {
					t.Errorf("the frame draws %q", bad)
				}
			}
			if got := sess.Called("Update"); len(got) != 0 {
				t.Errorf("Update was called %v", got)
			}
		})
	}
	if !strings.Contains(editorCannotKeep, "edit it with kubectl") {
		t.Fatal("the test's text is wrong")
	}
}

// Tabs written as \t in a double-quoted string, as the session's YAML
// writes them, are kept: the save sends what was read and the character
// typed, byte for byte.
func TestEditorKeepsWhatWasRead(t *testing.T) {
	sess := newSession()
	yaml := "apiVersion: v1\ndata:\n  Makefile: \"all:\\n\\techo hi\\n\"\n  note: caf\u00e9 \u2603\nkind: ConfigMap\n"
	sess.YAMLs[editorRef.String()] = yaml
	hs := editorStart(t, sess, true)
	if v := editorShown(hs); v.ta.Value() != yaml || v.was != yaml {
		t.Errorf("the text area holds %q", v.ta.Value())
	}
	hs.typeText("x")
	hs.press("ctrl+s")
	hs.awaitFrame("Updated configmaps/default/cfg")
	got := sess.Called("Update")
	if len(got) != 1 || got[0].Args[1].(string) != "x"+yaml {
		t.Errorf("Update was called %v", got)
	}
}

// A YAML the text area cannot hold as it is, as one of more lines than
// it takes, is not edited: what was sent would not be what was read.
func TestEditorRefusesWhatItCannotHold(t *testing.T) {
	sess := newSession()
	sess.YAMLs[editorRef.String()] = strings.Repeat("a: b\n", editorMaxLines+1)
	hs := editorStart(t, sess, false)
	hs.press("e")
	hs.wantFrame("more lines than the editor holds")
	if !editorClosed(hs) {
		t.Error("an editor opened")
	}

	// Nor one of more bytes than it takes, in few lines.
	sess = newSession()
	sess.YAMLs[editorRef.String()] = "a: " + strings.Repeat("b", editorMaxBytes) + "\n"
	hs = editorStart(t, sess, false)
	hs.press("e")
	hs.wantFrame("more bytes")
	if !editorClosed(hs) {
		t.Error("an editor opened for a YAML of more bytes than it holds")
	}
}

func TestEditorWhileSaving(t *testing.T) {
	sess := actionSpy()
	release := sess.holdCalls("Update")
	t.Cleanup(release)
	sess.YAMLs[editorRef.String()] = editorYAML
	hs := newHarness(t, sess, 100, 30)
	actionOpen(hs, sess.Session, "configmaps", simpleTable("cfg"))
	hs.press("e")
	hs.typeText("x")
	hs.press("ctrl+s")
	if hs.title() != "Edit(configmaps/default/cfg) [saving]" {
		t.Errorf("the title is %q", hs.title())
	}
	v := editorShown(hs)
	hs.press("y", "enter", "esc", "ctrl+s", "backspace")
	hs.send(keyPress("z"))
	if v.ta.Value() != "x"+editorYAML || hs.root().dlg != nil || editorShown(hs) != v {
		t.Errorf("keys changed something while saving: the text is %q", v.ta.Value())
	}
	release()
	hs.awaitFrame("Updated configmaps/default/cfg")
	if got := sess.Called("Update"); len(got) != 1 {
		t.Errorf("Update was called %d times", len(got))
	}
	if !editorClosed(hs) {
		t.Error("the editor is open")
	}
}

// editorLargest returns the largest YAMLs the editor opens: as many
// lines as it holds, and as many lines as its bytes allow that each
// wrap into two rows at 80 columns.
func editorLargest() []string {
	short := strings.Repeat("a: b\n", editorMaxLines-1)
	long := strings.Repeat("a", 72) + "\n"
	n := min(editorMaxLines-1, editorMaxBytes/len(long))
	return []string{short, strings.Repeat(long, n)}
}

// editorTimed is one case of the time a key costs in the editor: its
// name, and the editor at 80x24 with the text of the case.
type editorTimed struct {
	name string
	open func(tb testing.TB) (*harness, *editorView)
}

// editorTimedCases are the largest objects the editor opens, and the
// largest text it holds while it is edited: lines of two rows each up
// to editorMaxText, less a little room for the keys that are timed.
func editorTimedCases() []editorTimed {
	open := func(tb testing.TB, yaml string) (*harness, *editorView) {
		sess := newSession()
		sess.YAMLs[editorRef.String()] = yaml
		hs := newHarness(tb, sess, 80, 24)
		actionOpen(hs, sess, "configmaps", simpleTable("cfg"))
		hs.press("e")
		return hs, editorShown(hs)
	}
	var cases []editorTimed
	for i, yaml := range editorLargest() {
		cases = append(cases, editorTimed{
			name: fmt.Sprintf("opened %d: %d lines, %d bytes", i, strings.Count(yaml, "\n"), len(yaml)),
			open: func(tb testing.TB) (*harness, *editorView) { return open(tb, yaml) },
		})
	}
	cases = append(cases, editorTimed{
		name: fmt.Sprintf("edited to %d bytes", editorMaxText),
		open: func(tb testing.TB) (*harness, *editorView) {
			hs, v := open(tb, editorLargest()[1])
			long := strings.Repeat("a", 72) + "\n"
			room := editorMaxText - v.size - 4096
			hs.send(tea.PasteMsg{Content: strings.Repeat(long, room/len(long))})
			if v.size < editorMaxText-4096-len(long) {
				tb.Fatalf("the text is %d bytes after the paste", v.size)
			}
			return hs, v
		},
	})
	return cases
}

// editorKeyAndFrame presses a key in the editor and draws the frame.
func editorKeyAndFrame(hs *harness) {
	hs.press("x")
	hs.frame()
}

// A key and its frame in the editor at its largest. It measures time,
// so it runs only when KOSH_TIMING_TESTS=1: a slow or loaded machine
// would fail it otherwise. BenchmarkEditorKeyPress measures the same.
func TestEditorKeyPressIsFast(t *testing.T) {
	if os.Getenv("KOSH_TIMING_TESTS") != "1" {
		t.Skip("a test of time: set KOSH_TIMING_TESTS=1 to run it, or run BenchmarkEditorKeyPress")
	}
	bound := 150 * time.Millisecond * slowdown
	for _, c := range editorTimedCases() {
		hs, _ := c.open(t)
		hs.frame()
		// The fastest of a few, so that a pause of the machine is not
		// counted.
		best := time.Hour
		for range 3 {
			start := time.Now()
			editorKeyAndFrame(hs)
			best = min(best, time.Since(start))
		}
		t.Logf("%s: a key and its frame in %v", c.name, best)
		if best > bound {
			t.Errorf("%s: a key and its frame take %v, more than %v", c.name, best, bound)
		}
	}
}

func BenchmarkEditorKeyPress(b *testing.B) {
	for _, c := range editorTimedCases() {
		b.Run(c.name, func(b *testing.B) {
			hs, _ := c.open(b)
			hs.frame()
			for b.Loop() {
				editorKeyAndFrame(hs)
			}
		})
	}
}
