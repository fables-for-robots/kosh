package tui

import (
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/fables-for-robots/kosh/kube/kubetest"
)

func TestCommandGoesToAResource(t *testing.T) {
	sess := newSession()
	hs := newHarness(t, sess, 100, 30)
	first := hs.root().top()
	hs.command("deploy")
	if !strings.HasPrefix(hs.title(), "Deployments(default)[") {
		t.Errorf("the title is %q", hs.title())
	}
	if l := hs.lines()[len(hs.lines())-2]; words(l) != "<deployments>" {
		t.Errorf("the crumbs are %q", l)
	}
	if len(hs.root().stack) != 1 || hs.root().top() == first {
		t.Errorf("the stack is not the deployments' table alone: %d views", len(hs.root().stack))
	}
	hs.notFrame(":deploy")
	if got := sess.OpenWatches(); len(got) != 1 || got[0] != kubetest.Key(query("deployments", "default")) {
		t.Errorf("the open watches are %q", got)
	}
}

func TestCommandClosesTheOldStack(t *testing.T) {
	hs := newHarness(t, newSession(), 100, 30)
	a := &testView{name: "a"}
	hs.send(pushViewMsg{a})
	hs.command("svc")
	if a.closed != 1 {
		t.Errorf("the view of the old stack was closed %d times", a.closed)
	}
	if len(hs.root().stack) != 1 || !strings.HasPrefix(hs.title(), "Services(default)[") {
		t.Errorf("the stack has %d views, the title is %q", len(hs.root().stack), hs.title())
	}
}

func TestCommandWithNamespace(t *testing.T) {
	hs := newHarness(t, newSession(), 100, 30)
	hs.command("po kube-system")
	if !strings.HasPrefix(hs.title(), "Pods(kube-system)[") {
		t.Errorf("the title is %q", hs.title())
	}
	if l := hs.line("<2>"); !strings.Contains(l, "<2> kube-system") {
		t.Errorf("the header's shortcut 2 is %q", l)
	}
	hs.wantFrame("<0> all", "<1> default")
	// A command without a namespace goes to the current one.
	hs.command("deploy")
	if !strings.HasPrefix(hs.title(), "Deployments(kube-system)[") {
		t.Errorf("the title is %q", hs.title())
	}
	// A resource that is not namespaced has no namespace and no
	// shortcuts.
	hs.command("nodes")
	if !strings.HasPrefix(hs.title(), "Nodes(all)[") {
		t.Errorf("the title is %q", hs.title())
	}
	hs.notFrame("<0> all")
}

func TestCommandUnknown(t *testing.T) {
	hs := newHarness(t, newSession(), 100, 30)
	before := hs.title()
	hs.command("nothing")
	hs.wantFrame("no such resource: nothing")
	if hs.title() != before {
		t.Errorf("the title changed to %q", hs.title())
	}
	if l := hs.drawnLine("no such resource"); !strings.Contains(l, sgr(styleError)) {
		t.Errorf("the flash is not in the error style: %q", l)
	}
}

func TestCommandSuggests(t *testing.T) {
	hs := newHarness(t, newSession(), 100, 30)
	hs.press(":")
	hs.typeText("dep")
	if l := hs.line(":dep"); !strings.Contains(l, ":deployments") {
		t.Errorf("the prompt is %q", l)
	}
	if l := hs.drawnLine(":dep"); !strings.Contains(l, styleDim.Render("oyments")) {
		t.Errorf("the suggestion is not in the dim style: %q", l)
	}
	hs.press("tab")
	if v := hs.root().prompt.in.Value(); v != "deployments" {
		t.Errorf("tab made the text %q", v)
	}
	hs.press("esc")
	hs.notFrame(":dep")
	// → takes it too; the short names come after the plural ones.
	hs.press(":")
	hs.typeText("cj")
	hs.press("right")
	if v := hs.root().prompt.in.Value(); v != "cj" {
		t.Errorf("→ made the text %q", v)
	}
	hs.press("backspace", "backspace")
	hs.typeText("cro")
	hs.press("right")
	if v := hs.root().prompt.in.Value(); v != "cronjobs" {
		t.Errorf("→ made the text %q", v)
	}
	hs.press("enter")
	if !strings.HasPrefix(hs.title(), "Cronjobs(default)[") {
		t.Errorf("the title is %q", hs.title())
	}
	// A short name is suggested when no plural name begins with the text.
	hs.press(":")
	hs.typeText("sv")
	if l := hs.line(":sv"); !strings.Contains(l, ":svc") {
		t.Errorf("the prompt is %q", l)
	}
}

func TestSuggestionsAreCleaned(t *testing.T) {
	sess := newSession()
	evil := kubetest.Resource("pods")
	evil.GVR.Resource = "zz\x1b]0;owned\x07evil"
	sess.ResourceList = append(sess.ResourceList, evil)
	hs := newHarness(t, sess, 100, 30)
	hs.press(":")
	hs.typeText("zz")
	if d := hs.drawn(); strings.Contains(d, "\x1b]0;") || strings.Contains(d, "\x07") {
		t.Errorf("the suggestion was drawn raw: %q", hs.drawnLine(":zz"))
	}
	hs.wantFrame(":zzevil")
}

func TestCommandQuits(t *testing.T) {
	for _, cmd := range []string{"q", "q!", "qa", "Q"} {
		hs := newHarness(t, newSession(), 100, 30)
		hs.command(cmd)
		if !hs.quit {
			t.Errorf(":%s does not end the session", cmd)
		}
	}
	hs := newHarness(t, newSession(), 100, 30)
	hs.press("ctrl+c")
	if !hs.quit {
		t.Error("ctrl+c does not end the session")
	}
	// ctrl+c ends it from the prompt too.
	hs = newHarness(t, newSession(), 100, 30)
	hs.press(":", "ctrl+c")
	if !hs.quit {
		t.Error("ctrl+c does not end the session from the prompt")
	}
}

func TestHistory(t *testing.T) {
	sess := newSession()
	hs := newHarness(t, sess, 100, 30)
	hs.command("pods")
	hs.command("deploy")
	hs.command("svc")
	resolves := len(sess.Called("Resolve"))
	for _, step := range []struct{ key, want string }{
		{"[", "Deployments"},
		{"[", "Pods"},
		{"]", "Deployments"},
		{"-", "Pods"},
		{"-", "Deployments"},
	} {
		hs.press(step.key)
		if !strings.HasPrefix(hs.title(), step.want+"(") {
			t.Errorf("after %s the title is %q, want %s", step.key, hs.title(), step.want)
		}
	}
	if got := len(sess.Called("Resolve")) - resolves; got != 5 {
		t.Errorf("going through the history resolved %d times", got)
	}
	// Going about the history adds nothing to it; two equal commands in
	// a row are one entry.
	if got := strings.Join(hs.root().hist.entries, ","); got != "pods,deploy,svc" {
		t.Errorf("the history is %s", got)
	}
	hs.command("svc")
	hs.command("svc")
	if got := strings.Join(hs.root().hist.entries, ","); got != "pods,deploy,svc" {
		t.Errorf("after svc twice the history is %s", got)
	}
	hs.command("po")
	hs.command("po")
	hs.command("svc")
	if got := strings.Join(hs.root().hist.entries, ","); got != "pods,deploy,svc,po,svc" {
		t.Errorf("after po twice and svc the history is %s", got)
	}
	// Beyond the ends nothing happens.
	hs = newHarness(t, newSession(), 100, 30)
	hs.press("]", "[", "-")
	if !strings.HasPrefix(hs.title(), "Pods(default)[") {
		t.Errorf("the title is %q", hs.title())
	}
}

// filterView is a view that / filters in.
type filterView struct {
	testView
	filters []string
}

func (v *filterView) SetFilter(text string) tea.Cmd {
	v.filters = append(v.filters, text)
	return nil
}

func (v *filterView) Update(msg tea.Msg) (view, tea.Cmd) {
	v.testView.Update(msg)
	return v, nil
}

func TestFilterPrompt(t *testing.T) {
	hs := newHarness(t, newSession(), 100, 30)
	v := &filterView{testView: testView{name: "f"}}
	hs.send(pushViewMsg{v})
	hs.press("/")
	hs.typeText("web")
	if got := strings.Join(v.filters, ","); got != "w,we,web" {
		t.Errorf("SetFilter was given %s", got)
	}
	hs.wantFrame("/web")
	hs.press("enter")
	hs.notFrame("/web")
	if got := strings.Join(v.filters, ","); got != "w,we,web" {
		t.Errorf("enter gave SetFilter %s", got)
	}
	hs.press("/")
	hs.typeText("x")
	hs.press("esc")
	if got := v.filters[len(v.filters)-1]; got != "" {
		t.Errorf("esc left the filter %q", got)
	}
	if len(v.keys) != 0 {
		t.Errorf("the view got the keys %q", v.keys)
	}
	// / is not the root's for a view that does not filter.
	w := &testView{name: "w"}
	hs.send(pushViewMsg{w})
	hs.press("/")
	if strings.Join(w.keys, " ") != "/" {
		t.Errorf("the view got %q", w.keys)
	}
}

func TestTheStartCanFailAndTheCommandsWork(t *testing.T) {
	sess := newSession()
	sess.Errs["Resolve"] = errors.New("the API is down")
	hs := newHarness(t, sess, 100, 30)
	hs.wantFrame("the API is down")
	// A command that fails says so in the flash; the body keeps the
	// error of the start.
	sess.Errs["Resolve"] = errors.New("still down")
	hs.command("svc")
	if l := hs.line("│the API is down"); l == "" {
		t.Errorf("the body lost the error of the start:\n%s", hs.frame())
	}
	// The help and the names leave on esc even as the only view, back to
	// the error.
	for _, keys := range [][]string{{"?", "esc"}, {"?", "q"}, {"?", "?"}, {"ctrl+a", "esc"}} {
		hs.press(keys[0])
		if hs.title() == "" {
			t.Errorf("%s opened nothing", keys[0])
		}
		hs.press(keys[1])
		if len(hs.root().stack) != 0 || hs.line("│the API is down") == "" {
			t.Errorf("%s then %s leaves %d views:\n%s", keys[0], keys[1], len(hs.root().stack), hs.frame())
		}
	}
	delete(sess.Errs, "Resolve")
	hs.command("deploy")
	if !strings.HasPrefix(hs.title(), "Deployments(default)[") {
		t.Errorf("the title is %q", hs.title())
	}
	for _, l := range hs.lines() {
		if strings.HasPrefix(l, "│") && strings.Contains(l, "the API is down") {
			t.Errorf("the body still shows the error: %q", l)
		}
	}
}

// ctrl+v reads no clipboard of the server: bubbles' Paste would start a
// program in the pod to read one. It is not bound, and gives no command.
func TestPromptHasNoClipboardPaste(t *testing.T) {
	for _, open := range []string{":", "/"} {
		hs := newHarness(t, newSession(), 100, 30)
		hs.send(pushViewMsg{&filterView{testView: testView{name: "f"}}})
		hs.press(open)
		hs.typeText("ab")
		// The command is not run: were it bubbles' Paste, it would read
		// the clipboard of the machine the test runs on.
		_, cmd := hs.m.Update(keyPress("ctrl+v"))
		if cmd != nil {
			t.Errorf("ctrl+v at %s gives a command", open)
		}
		if v := hs.root().prompt.in.Value(); v != "ab" {
			t.Errorf("ctrl+v at %s made the text %q", open, v)
		}
		if hs.root().prompt.in.KeyMap.Paste.Enabled() {
			t.Errorf("the prompt of %s has Paste", open)
		}
	}
}

// What the terminal pastes is typed text, cleaned and on one line.
func TestPromptTakesAPaste(t *testing.T) {
	hs := newHarness(t, newSession(), 100, 30)
	v := &filterView{testView: testView{name: "f"}}
	hs.send(pushViewMsg{v})
	hs.press("/")
	hs.send(tea.PasteMsg{Content: "web\x1b[2J\nfront\x07"})
	got := hs.root().prompt.in.Value()
	if got != "web front␇" {
		t.Errorf("the pasted text is %q", got)
	}
	if last := v.filters[len(v.filters)-1]; last != got {
		t.Errorf("the filter was given %q", last)
	}
	if d := hs.drawn(); strings.Contains(d, "\x1b[2J") {
		t.Error("the paste was drawn raw")
	}
}
