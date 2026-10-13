package tui

import (
	"errors"
	"strings"
	"testing"

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
	hs.press("right")
	hs.notFrame("the object has been modified")
	if v.ta.Value() != "x: y\n"+editorYAML {
		t.Error("the key that took the error away changed the text")
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

func TestEditorCleansWhatItShows(t *testing.T) {
	sess := newSession()
	sess.YAMLs[editorRef.String()] = "data:\n  title: \x1b]0;x\x07owned\n  bell: \x07\n"
	hs := editorStart(t, sess, true)
	v := editorShown(hs)
	if strings.Contains(v.ta.Value(), "\x1b") || strings.Contains(v.ta.Value(), "\x07") {
		t.Errorf("the text area holds %q", v.ta.Value())
	}
	if strings.Contains(hs.drawn(), "\x1b]0;") || strings.Contains(hs.drawn(), "\x07") {
		t.Error("the frame draws the escape sequence")
	}
	hs.wantFrame("title: owned", "bell: ␇")
}

// A YAML the text area cannot hold as it is, as one of more lines than
// it takes, is not edited: what was sent would not be what was read.
func TestEditorRefusesWhatItCannotHold(t *testing.T) {
	sess := newSession()
	sess.YAMLs[editorRef.String()] = strings.Repeat("a: b\n", editorMaxLines+1)
	hs := editorStart(t, sess, false)
	hs.press("e")
	hs.wantFrame("cannot be edited here")
	if !editorClosed(hs) {
		t.Error("an editor opened")
	}
}

func TestEditorWhileSaving(t *testing.T) {
	sess := newActionSpySession()
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
