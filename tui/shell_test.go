package tui

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/fables-for-robots/kosh/keys"
	"github.com/fables-for-robots/kosh/kube"
	"github.com/fables-for-robots/kosh/kube/kubetest"
	"github.com/fables-for-robots/kosh/render"
)

// shellTerm is the hand-over of the tests: its command runs run in the
// command itself, with a terminal the test holds, and returns done's
// message. The program takes no message while run has the terminal, as
// the real hand-over promises.
type shellTerm struct {
	in      io.Reader
	out     bytes.Buffer
	w, h    int
	resized chan struct{}
	// refuse, when set, is what done is told without run being called:
	// a hand-over that could not be made.
	refuse error
	runs   int
}

func newShellTerm() *shellTerm {
	return &shellTerm{in: strings.NewReader("ls\n"), w: 132, h: 43, resized: make(chan struct{}, 1)}
}

func (f *shellTerm) size() (int, int) { return f.w, f.h }

func (f *shellTerm) handover(run func(context.Context, Terminal) error, done func(error) tea.Msg) tea.Cmd {
	return func() tea.Msg {
		if f.refuse != nil {
			return done(f.refuse)
		}
		f.runs++
		return done(run(context.Background(), Terminal{In: f.in, Out: &f.out, Size: f.size, Resized: f.resized}))
	}
}

// firstLine is the first line run wrote to the terminal, with its end.
func (f *shellTerm) firstLine() string {
	s := f.out.String()
	if i := strings.Index(s, "\n"); i >= 0 {
		return s[:i+1]
	}
	return s
}

// newShellHarness is newHarness with term's hand-over; a nil term gives
// the program none.
func newShellHarness(t *testing.T, sess kube.Session, term *shellTerm, w, h int) *harness {
	t.Helper()
	c := &clock{at: t0}
	opts := Options{Version: "v0.1.0", Now: c.now}
	if term != nil {
		opts.Handover = term.handover
	}
	hs := &harness{
		t:         t,
		clock:     c,
		m:         New(sess, opts),
		async:     make(chan tea.Msg, 64),
		done:      make(chan struct{}),
		settleFor: deadline,
	}
	t.Cleanup(func() { close(hs.done) })
	hs.queue = append(hs.queue, hs.m.Init())
	hs.send(tea.WindowSizeMsg{Width: w, Height: h})
	return hs
}

// shellCtr is a container of a pod's YAML and its state: running,
// waiting or completed.
type shellCtr struct{ name, state string }

func running(names ...string) []shellCtr {
	var out []shellCtr
	for _, n := range names {
		out = append(out, shellCtr{n, "running"})
	}
	return out
}

// shellPodYAML is the YAML of the pod default/web as the API gives it,
// less what the dialog does not read.
func shellPodYAML(annotation string, main, init, eph []shellCtr) string {
	var b strings.Builder
	b.WriteString("apiVersion: v1\nkind: Pod\nmetadata:\n  name: web\n  namespace: default\n")
	if annotation != "" {
		fmt.Fprintf(&b, "  annotations:\n    kubectl.kubernetes.io/default-container: %q\n", annotation)
	}
	list := func(key string, cs []shellCtr) {
		if len(cs) == 0 {
			return
		}
		fmt.Fprintf(&b, "  %s:\n", key)
		for _, c := range cs {
			fmt.Fprintf(&b, "  - name: %q\n    image: busybox\n", c.name)
		}
	}
	statuses := func(key string, cs []shellCtr) {
		if len(cs) == 0 {
			return
		}
		fmt.Fprintf(&b, "  %s:\n", key)
		for _, c := range cs {
			fmt.Fprintf(&b, "  - name: %q\n    ready: true\n    state:\n", c.name)
			switch c.state {
			case "running":
				b.WriteString("      running:\n        startedAt: \"2026-10-13T11:00:00Z\"\n")
			case "waiting":
				b.WriteString("      waiting:\n        reason: ContainerCreating\n")
			case "completed":
				b.WriteString("      terminated:\n        exitCode: 0\n        reason: Completed\n")
			}
		}
	}
	b.WriteString("spec:\n")
	list("containers", main)
	list("initContainers", init)
	list("ephemeralContainers", eph)
	b.WriteString("status:\n  phase: Running\n")
	statuses("containerStatuses", main)
	statuses("initContainerStatuses", init)
	statuses("ephemeralContainerStatuses", eph)
	return b.String()
}

var shellWeb = kubetest.Ref("pods", "default", "web")

// shellSetup returns a full key's session whose pods are web and zeta,
// web's YAML being yaml, and a program with a hand-over in the pods.
func shellSetup(t *testing.T, yaml string) (*kubetest.Session, *shellTerm, *harness) {
	t.Helper()
	sess := newSession()
	sess.SetTable(query("pods", "default"), podsNamed("web", "zeta"))
	sess.YAMLs[shellWeb.String()] = yaml
	term := newShellTerm()
	hs := newShellHarness(t, sess, term, 100, 30)
	return sess, term, hs
}

// shellExec is one call of Exec.
type shellExec struct {
	pod  kube.ObjectRef
	opts kube.ExecOptions
}

func shellExecs(sess *kubetest.Session) []shellExec {
	var out []shellExec
	for _, c := range sess.Called("Exec") {
		out = append(out, shellExec{c.Args[0].(kube.ObjectRef), c.Args[1].(kube.ExecOptions)})
	}
	return out
}

// shellDialog returns the dialog shown, which must be the shell's.
func shellDialog(hs *harness) *formDialog {
	hs.t.Helper()
	d := dialogOpen(hs)
	if d == nil || d.title != "Shell" {
		hs.t.Fatalf("the shell's dialog is not shown:\n%s", hs.frame())
	}
	return d
}

// shellHints reports whether the top view's hints have <s> Shell.
func shellHints(hs *harness) bool {
	return slices.Contains(hs.root().top().Hints(), hint{"s", "Shell"})
}

// shellHelpHasIt reports whether a line of the body of the help that is
// shown has the key s with the text Shell.
func shellHelpHasIt(hs *harness) bool {
	hs.t.Helper()
	if hs.title() != "Help" {
		hs.t.Fatalf("the help is not shown: %q", hs.title())
	}
	for _, l := range hs.lines() {
		if strings.HasPrefix(l, "│") && shellHelpLine.MatchString(l) {
			return true
		}
	}
	return false
}

var shellHelpLine = regexp.MustCompile(`<s>\s+Shell\b`)

var shellDefaultWords = []string{"sh", "-c", "command -v bash >/dev/null && exec bash || exec sh"}

func TestShellBinding(t *testing.T) {
	yaml := shellPodYAML("", running("app"), nil, nil)
	open := func(t *testing.T, grant keys.Grant, term *shellTerm) (*kubetest.Session, *harness) {
		sess := kubetest.New(grant)
		sess.SetTable(query("pods", "default"), podsNamed("web"))
		sess.YAMLs[shellWeb.String()] = yaml
		sess.SetTable(queryOf(kube.Containers, shellWeb), kube.Table{
			Columns: []render.Column{{Name: "NAME"}},
			Rows:    []render.Row{{Namespace: "default", Name: "app", Cells: []string{"app"}}},
		})
		sess.SetTable(query("deployments", "default"), simpleTable("web"))
		sess.SetTable(query("widgets", "default"), simpleTable("w"))
		sess.SetTable(kube.Query{Resource: kubetest.Resource("nodes")}, clusterTable("n1"))
		return sess, newShellHarness(t, sess, term, 200, 40)
	}
	full := keys.Grant{Name: "alice@laptop"}
	readOnly := keys.Grant{Name: "bob@laptop", ReadOnly: true}

	t.Run("a full key with a hand-over", func(t *testing.T) {
		sess, hs := open(t, full, newShellTerm())
		if !shellHints(hs) {
			t.Errorf("the pods have no <s> Shell: %v", hs.root().top().Hints())
		}
		hs.wantFrame("<s> Shell")
		// Beside the logs in the header's hints.
		var order []string
		for _, h := range hs.root().top().Hints() {
			order = append(order, h.Text)
		}
		if i := slices.Index(order, "Shell"); i < 1 || order[i-1] != "Logs Previous" {
			t.Errorf("Shell is not beside the logs: %q", order)
		}
		hs.press("?")
		if !shellHelpHasIt(hs) {
			t.Errorf("the help's body has no line with <s> Shell:\n%s", hs.frame())
		}
		hs.press("esc", "s")
		shellDialog(hs)
		hs.press("esc", "enter")
		if !shellHints(hs) {
			t.Errorf("a pod's containers have no <s> Shell: %v", hs.root().top().Hints())
		}
		hs.press("s")
		shellDialog(hs)
		hs.press("esc", "esc")
		for _, plural := range []string{"deployments", "nodes", "widgets"} {
			hs.command(plural)
			if shellHints(hs) {
				t.Errorf("%s have <s> Shell", plural)
			}
			if plural == "nodes" || plural == "widgets" {
				hs.press("s")
				if hs.root().dlg != nil {
					t.Errorf("s opened a dialog on %s", plural)
				}
			}
		}
		if n := len(sess.Called("Exec")); n != 0 {
			t.Errorf("Exec was called %d times", n)
		}
	})

	for _, c := range []struct {
		name  string
		grant keys.Grant
		term  *shellTerm
	}{
		{"a read-only key", readOnly, newShellTerm()},
		{"no hand-over", full, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			sess, hs := open(t, c.grant, c.term)
			if shellHints(hs) {
				t.Error("the pods have <s> Shell")
			}
			hs.notFrame("Shell")
			hs.press("s")
			if hs.root().dlg != nil {
				t.Error("s opened a dialog")
			}
			hs.press("?")
			if shellHelpHasIt(hs) {
				t.Error("the help lists <s> Shell")
			}
			hs.notFrame("Shell")
			hs.press("esc", "enter")
			if shellHints(hs) {
				t.Error("a pod's containers have <s> Shell")
			}
			hs.press("s")
			if hs.root().dlg != nil {
				t.Error("s opened a dialog on a pod's containers")
			}
			if n := len(sess.Called("YAML")) + len(sess.Called("Exec")); n != 0 {
				t.Errorf("s read or ran something: %v", sess.Calls())
			}
		})
	}
}

func TestShellDialogOneContainer(t *testing.T) {
	for _, size := range [][2]int{{80, 24}, {120, 40}} {
		t.Run(fmt.Sprintf("%dx%d", size[0], size[1]), func(t *testing.T) {
			sess, _, hs := shellSetup(t, shellPodYAML("", running("web"), nil, nil))
			hs.send(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
			hs.press("s")
			d := shellDialog(hs)
			hs.wantFrame("Shell", "A shell in pod default/web", "Container", "Command", "Cancel")
			if got := d.field("Container"); got == nil || got.kind != formIsFixed || got.value() != "web" {
				t.Errorf("the container is not the fixed line web: %+v", got)
			}
			cmd := d.field("Command")
			if cmd.value() != shellDefaultCommand {
				t.Errorf("the command is %q", cmd.value())
			}
			if cmd.in.Position() != len([]rune(shellDefaultCommand)) {
				t.Errorf("the cursor is at %d, not at the end", cmd.in.Position())
			}
			if dialogFocused(hs) != "Command" {
				t.Errorf("the focus is on %q", dialogFocused(hs))
			}
			if !dialogFocusDrawn(hs) {
				t.Errorf("the focus is not drawn in Command:\n%s", hs.frame())
			}
			// The end of the command shows, the text scrolled sideways.
			hs.wantFrame("exec bash || exec sh")
			hs.golden()
			// The focus never stops on the container.
			var seen []string
			for range 6 {
				hs.press("tab")
				seen = append(seen, dialogFocused(hs))
			}
			if want := []string{"Cancel", "Shell", "Command", "Cancel", "Shell", "Command"}; !slices.Equal(seen, want) {
				t.Errorf("tab goes %q, want %q", seen, want)
			}
			hs.press("shift+tab", "shift+tab", "shift+tab")
			if dialogFocused(hs) != "Command" {
				t.Errorf("shift+tab stops on %q", dialogFocused(hs))
			}
			if n := len(sess.Called("Exec")); n != 0 {
				t.Errorf("Exec was called %d times", n)
			}
		})
	}
}

func TestShellDialogChoosesTheDefaultContainer(t *testing.T) {
	for _, c := range []struct {
		annotation string
		want       string
	}{
		{"sidecar", "sidecar"},
		{"", "app"},
		{"gone", "app"},
	} {
		t.Run(c.annotation, func(t *testing.T) {
			_, _, hs := shellSetup(t, shellPodYAML(c.annotation, running("app", "sidecar", "proxy"), nil, nil))
			hs.press("s")
			d := shellDialog(hs)
			f := d.field("Container")
			if f.kind != formIsChoice || !slices.Equal(f.options, []string{"app", "sidecar", "proxy"}) {
				t.Fatalf("the container is not a choice of the three: %+v", f)
			}
			if f.value() != c.want {
				t.Errorf("%q is chosen first, want %q", f.value(), c.want)
			}
			hs.wantFrame("‹ " + c.want + " ›")
			if dialogFocused(hs) != "Command" {
				t.Errorf("the focus is on %q", dialogFocused(hs))
			}
			hs.press("shift+tab")
			if dialogFocused(hs) != "Container" {
				t.Fatalf("shift+tab from Command goes to %q", dialogFocused(hs))
			}
			at := slices.Index(f.options, c.want)
			hs.press("right")
			if want := f.options[(at+1)%3]; f.value() != want {
				t.Errorf("right chose %q, want %q", f.value(), want)
			}
			hs.wantFrame("‹ " + f.value() + " ›")
			hs.press("left", "left")
			if want := f.options[(at+2)%3]; f.value() != want {
				t.Errorf("left twice chose %q, want %q", f.value(), want)
			}
		})
	}
}

func TestShellDialogListsWhatRuns(t *testing.T) {
	yaml := shellPodYAML("",
		[]shellCtr{{"app", "running"}, {"late", "waiting"}},
		[]shellCtr{{"setup", "completed"}, {"sidecar", "running"}},
		[]shellCtr{{"debugger", "running"}})
	sess, term, hs := shellSetup(t, yaml)
	hs.press("s")
	f := shellDialog(hs).field("Container")
	if want := []string{"app", "sidecar", "debugger"}; !slices.Equal(f.options, want) {
		t.Errorf("the choice is %q, want %q", f.options, want)
	}
	// The pod was read once, without its managed fields.
	if got := sess.Called("YAML"); len(got) != 1 || got[0].Args[1] != false {
		t.Errorf("the pod was read so: %v", got)
	}
	// The one chosen is the one Exec gets.
	hs.press("shift+tab", "right", "right", "tab", "enter")
	if ex := shellExecs(sess); len(ex) != 1 || ex[0].opts.Container != "debugger" {
		t.Errorf("Exec was called so: %+v", ex)
	}
	if term.runs != 1 {
		t.Errorf("the terminal was handed over %d times", term.runs)
	}
}

func TestShellNoRunningContainer(t *testing.T) {
	for name, yaml := range map[string]string{
		"waiting": shellPodYAML("", []shellCtr{{"app", "waiting"}}, []shellCtr{{"setup", "completed"}}, nil),
		"empty":   "",
	} {
		t.Run(name, func(t *testing.T) {
			sess, term, hs := shellSetup(t, yaml)
			hs.press("s")
			if hs.root().dlg != nil {
				t.Fatal("a dialog opened")
			}
			hs.wantFrame("pod default/web has no running container")
			if strings.Contains(hs.drawnLine("has no running container"), sgr(styleError)) {
				t.Error("the flash is an error")
			}
			if len(sess.Called("Exec")) != 0 || term.runs != 0 {
				t.Error("something was run")
			}
			if actionBusy(sess) {
				t.Error("the action keys are still held")
			}
		})
	}
	// A reading that fails, and a YAML that does not decode, are the
	// error flash.
	sess, _, hs := shellSetup(t, "")
	sess.Errs["YAML"] = errors.New(`pods "web" is forbidden`)
	hs.press("s")
	hs.wantFrame(`pods "web" is forbidden`)
	if hs.root().dlg != nil || actionBusy(sess) {
		t.Error("a failed reading opened a dialog or kept the keys")
	}
	delete(sess.Errs, "YAML")
	sess.YAMLs[shellWeb.String()] = "spec: [unclosed\n"
	hs.press("s")
	if !strings.Contains(hs.drawnLine("could not be read"), sgr(styleError)) {
		t.Errorf("a YAML that does not decode is not an error flash:\n%s", hs.frame())
	}
	if hs.root().dlg != nil || actionBusy(sess) {
		t.Error("a YAML that does not decode opened a dialog or kept the keys")
	}
}

func TestShellFromTheContainersTable(t *testing.T) {
	sess, _, hs := shellSetup(t, shellPodYAML("app", running("app", "sidecar", "proxy"), nil, nil))
	sess.SetTable(queryOf(kube.Containers, shellWeb), kube.Table{
		Columns: []render.Column{{Name: "NAME"}},
		Rows: []render.Row{
			{Namespace: "default", Name: "app", Cells: []string{"app"}},
			{Namespace: "default", Name: "sidecar", Cells: []string{"sidecar"}},
			{Namespace: "default", Name: "proxy", Cells: []string{"proxy"}},
		},
	})
	hs.press("enter")
	if hs.title() != "Containers(pods/default/web)[3]" {
		t.Fatalf("the title is %q", hs.title())
	}
	// Marks do not make it several: the cursor's row is the one.
	hs.press("space", "down", "down")
	tv := hs.table2(1)
	want := tv.ref(tv.cursorRow()).Name
	if want == "app" {
		t.Fatal("the cursor did not move")
	}
	hs.press("s")
	d := shellDialog(hs)
	hs.wantFrame("A shell in pod default/web")
	if f := d.field("Container"); f.kind != formIsFixed || f.value() != want {
		t.Errorf("the container is %+v, want %s and fixed", f, want)
	}
	hs.press("shift+tab", "shift+tab", "shift+tab")
	if dialogFocused(hs) == "Container" {
		t.Error("the focus stops on the container")
	}
	dialogFocusOn(hs, "Command")
	hs.press("enter")
	ex := shellExecs(sess)
	if len(ex) != 1 || ex[0].pod.String() != shellWeb.String() || ex[0].pod.Resource.GVR != shellWeb.Resource.GVR || ex[0].opts.Container != want {
		t.Errorf("Exec was called so: %+v", ex)
	}
}

func TestShellEnterStartsIt(t *testing.T) {
	sess, term, hs := shellSetup(t, shellPodYAML("", running("app"), nil, nil))
	hs.press("s", "enter")
	if hs.root().dlg != nil {
		t.Error("the dialog is still open")
	}
	ex := shellExecs(sess)
	if len(ex) != 1 {
		t.Fatalf("Exec was called %d times", len(ex))
	}
	if ex[0].pod.String() != shellWeb.String() || ex[0].pod.Resource.GVR != shellWeb.Resource.GVR {
		t.Errorf("Exec was called on %s", ex[0].pod)
	}
	o := ex[0].opts
	if o.Container != "app" || !slices.Equal(o.Command, shellDefaultWords) {
		t.Errorf("Exec was called with %q %q", o.Container, o.Command)
	}
	if o.Stdin != term.in || o.Stdout != io.Writer(&term.out) {
		t.Error("Exec did not get the terminal's input and output")
	}
	if want := "kosh: a shell in pod default/web, container app. It ends with exit or ctrl-d.\r\n"; term.firstLine() != want {
		t.Errorf("the first line written is %q, want %q", term.firstLine(), want)
	}
	if actionBusy(sess) {
		t.Error("the action keys are still held after the return")
	}
}

func TestShellEditedCommand(t *testing.T) {
	sess, _, hs := shellSetup(t, shellPodYAML("", running("app"), nil, nil))
	hs.press("s")
	for range len(shellDefaultCommand) {
		hs.press("backspace")
	}
	if v := shellDialog(hs).field("Command").value(); v != "" {
		t.Fatalf("the field holds %q", v)
	}
	hs.typeText("/bin/bash -l")
	hs.press("enter")
	if ex := shellExecs(sess); len(ex) != 1 || !slices.Equal(ex[0].opts.Command, []string{"/bin/bash", "-l"}) {
		t.Errorf("Exec was called so: %+v", ex)
	}
}

func TestShellBadCommandStays(t *testing.T) {
	sess, term, hs := shellSetup(t, shellPodYAML("", running("app"), nil, nil))
	hs.press("s")
	hs.typeText(` "unclosed`)
	hs.press("enter")
	d := shellDialog(hs)
	hs.wantFrame(commandTextUnclosed)
	if d.field("Command").value() != shellDefaultCommand+` "unclosed` {
		t.Errorf("the field holds %q", d.field("Command").value())
	}
	// The same from the button.
	hs.press("tab", "tab")
	if dialogFocused(hs) != "Shell" {
		t.Fatalf("the focus is on %q", dialogFocused(hs))
	}
	hs.press("enter")
	hs.wantFrame(commandTextUnclosed)
	hs.press("tab")
	for range 100 {
		hs.press("backspace")
	}
	hs.typeText("   ")
	hs.press("enter")
	shellDialog(hs)
	hs.wantFrame(commandTextEmpty)
	if len(sess.Called("Exec")) != 0 || term.runs != 0 {
		t.Error("something was run")
	}
	if actionBusy(sess) {
		t.Error("the action keys are held while the dialog is open")
	}
}

func TestShellCancel(t *testing.T) {
	for _, keys := range [][]string{{"esc"}, {"tab", "enter"}} {
		t.Run(strings.Join(keys, " "), func(t *testing.T) {
			sess, term, hs := shellSetup(t, shellPodYAML("", running("app", "sidecar"), nil, nil))
			before := hs.frame()
			hs.press("s")
			if dialogFocused(hs) != "Command" {
				t.Fatalf("the focus is on %q", dialogFocused(hs))
			}
			hs.press(keys...)
			if hs.root().dlg != nil {
				t.Fatal("the dialog is still open")
			}
			if len(sess.Called("Exec")) != 0 || term.runs != 0 {
				t.Error("something was run")
			}
			if hs.frame() != before {
				t.Errorf("the table is not as before:\n%s\nwas:\n%s", hs.frame(), before)
			}
			if actionBusy(sess) {
				t.Error("the action keys are still held")
			}
			hs.press("ctrl+k")
			if d := dialogOpen(hs); d == nil || !strings.Contains(d.question, "Kill") {
				t.Error("another action key does not work")
			}
		})
	}
}

// A dialog the window no longer has room for is closed, and gives the
// keys back; so does one the window never had room for.
func TestShellDialogTooSmall(t *testing.T) {
	sess, _, hs := shellSetup(t, shellPodYAML("", running("app"), nil, nil))
	hs.press("s")
	shellDialog(hs)
	hs.send(tea.WindowSizeMsg{Width: 80, Height: 8})
	if hs.root().dlg != nil {
		t.Fatal("the dialog stays open on 8 lines")
	}
	if actionBusy(sess) {
		t.Error("the action keys are held after the dialog was closed")
	}
	hs.send(tea.WindowSizeMsg{Width: 70, Height: 30})
	hs.press("s")
	if hs.root().dlg != nil {
		t.Fatal("a dialog opened in a window too narrow")
	}
	if actionBusy(sess) {
		t.Error("the action keys are held after the dialog was refused")
	}
	for _, size := range [][2]int{{80, 12}, {80, 13}, {81, 12}, {200, 60}} {
		hs.send(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		hs.press("s")
		shellDialog(hs)
		hs.wantFrame("Cancel", "Shell", "Command")
		if !dialogFocusDrawn(hs) {
			t.Errorf("at %v the focus is not drawn:\n%s", size, hs.frame())
		}
		hs.press("esc")
	}
	if len(sess.Called("Exec")) != 0 {
		t.Error("Exec was called")
	}
}

func TestShellSizes(t *testing.T) {
	sess, term, hs := shellSetup(t, shellPodYAML("", running("app"), nil, nil))
	var w, h int
	resized := false
	sess.ExecDo = func(_ context.Context, _ kube.ObjectRef, o kube.ExecOptions) (kube.ExecResult, error) {
		w, h = o.Size()
		term.w, term.h = 90, 33
		term.resized <- struct{}{}
		select {
		case <-o.Resized:
			resized = true
		default:
		}
		if w2, h2 := o.Size(); w2 != 90 || h2 != 33 {
			return kube.ExecResult{}, fmt.Errorf("the size after the change is %dx%d", w2, h2)
		}
		return kube.ExecResult{Started: true}, nil
	}
	hs.press("s", "enter")
	if w != 132 || h != 43 {
		t.Errorf("Exec read the size %dx%d, want 132x43", w, h)
	}
	if !resized {
		t.Error("the value put on Resized was not read from ExecOptions.Resized")
	}
	hs.notFrame("the size after the change")
}

func TestShellStatus(t *testing.T) {
	sess, _, hs := shellSetup(t, shellPodYAML("", running("app"), nil, nil))
	sess.ExecResult = kube.ExecResult{Started: true, Status: 3}
	hs.press("s", "enter")
	hs.wantFrame("the shell ended with status 3")
	if strings.Contains(hs.drawnLine("the shell ended with status 3"), sgr(styleError)) {
		t.Error("the status is an error flash")
	}
	hs.clock.advance(flashFor * 2)
	hs.notFrame("the shell ended")
	sess.ExecResult = kube.ExecResult{Started: true, Status: 0}
	hs.press("s", "enter")
	hs.notFrame("the shell ended", "status")
	if n := len(sess.Called("Exec")); n != 2 {
		t.Errorf("Exec was called %d times", n)
	}
}

func TestShellDidNotStart(t *testing.T) {
	sess, term, hs := shellSetup(t, shellPodYAML("", running("app", "sidecar"), nil, nil))
	reason := `exec: "sh": executable file not found in $PATH`
	sess.Errs["Exec"] = fmt.Errorf("%w: %s", kube.ErrExecNotStarted, reason)
	hs.press("s", "shift+tab", "right", "tab")
	for range len(shellDefaultCommand) {
		hs.press("backspace")
	}
	hs.typeText("bash")

	// Shell closes the dialog first, and then hands the terminal over: a
	// dialog opened again on the return is not closed by a close that
	// came late.
	pressed := shellDialog(hs)
	_, cmd := pressed.Update(keyPress("enter"))
	msg := cmd()
	seq := reflect.ValueOf(msg)
	if _, batch := msg.(tea.BatchMsg); batch || seq.Kind() != reflect.Slice || seq.Len() != 2 {
		t.Fatalf("Shell does not close and then hand over in a sequence: %T", msg)
	}
	// The press took the keys: the hand-over holds them from here.
	if !actionBusy(sess) {
		t.Fatal("Shell did not take the keys")
	}
	if _, ok := seq.Index(0).Interface().(tea.Cmd)().(closeDialogMsg); !ok {
		t.Fatal("what Shell does first is not to close the dialog")
	}
	hs.deliver(seq.Interface())
	hs.settle()

	d := shellDialog(hs)
	hs.wantFrame("the command could not be started", "executable", "file not found in $PATH")
	if !strings.Contains(d.why, reason) {
		t.Errorf("the reason is %q", d.why)
	}
	if d.field("Command").value() != "bash" {
		t.Errorf("the command is %q, not as typed", d.field("Command").value())
	}
	if d.field("Container").value() != "sidecar" {
		t.Errorf("the container is %q, not as chosen", d.field("Container").value())
	}
	if dialogFocused(hs) != "Command" {
		t.Errorf("the focus is on %q", dialogFocused(hs))
	}
	if actionBusy(sess) {
		t.Error("the action keys are held while the dialog is open again")
	}
	// Edited, and enter: Exec again with the new words.
	delete(sess.Errs, "Exec")
	for range 4 {
		hs.press("backspace")
	}
	hs.typeText("/bin/ash -l")
	hs.press("enter")
	ex := shellExecs(sess)
	if len(ex) != 2 || !slices.Equal(ex[0].opts.Command, []string{"bash"}) ||
		!slices.Equal(ex[1].opts.Command, []string{"/bin/ash", "-l"}) || ex[1].opts.Container != "sidecar" {
		t.Errorf("Exec was called so: %+v", ex)
	}
	if hs.root().dlg != nil || term.runs != 2 || actionBusy(sess) {
		t.Errorf("after the second start: dialog %v, runs %d, held %v", hs.root().dlg != nil, term.runs, actionBusy(sess))
	}
	// Dropped while it is open again, it gives the keys back.
	sess.Errs["Exec"] = fmt.Errorf("%w: %s", kube.ErrExecNotStarted, reason)
	hs.press("s", "enter")
	shellDialog(hs)
	hs.send(tea.WindowSizeMsg{Width: 80, Height: 8})
	if hs.root().dlg != nil || actionBusy(sess) {
		t.Error("the dialog opened again does not give the keys back when it is closed")
	}
}

func TestShellRefusedByTheAPI(t *testing.T) {
	sess, _, hs := shellSetup(t, shellPodYAML("", running("app"), nil, nil))
	sess.Errs["Exec"] = errors.New(`pods "web" is forbidden: User "system:serviceaccount:kosh:kosh" cannot create resource "pods/exec"`)
	hs.press("s", "enter")
	if hs.root().dlg != nil {
		t.Error("a dialog is open")
	}
	hs.wantFrame(`pods "web" is forbidden`)
	if !strings.Contains(hs.drawnLine(`pods "web" is forbidden`), sgr(styleError)) {
		t.Error("the refusal is not an error flash")
	}
	if actionBusy(sess) {
		t.Error("the action keys are still held")
	}
}

// The hand-over refused (a command has the terminal, the session ended):
// done is told an error, run is not called.
func TestShellHandoverRefused(t *testing.T) {
	sess, term, hs := shellSetup(t, shellPodYAML("", running("app"), nil, nil))
	term.refuse = errors.New("the terminal is \x1b[2Jbusy")
	hs.press("s", "space", "enter")
	// The dialog again, with the command as it was and the reason: enter
	// once more is the way in.
	d := shellDialog(hs)
	hs.wantFrame("the terminal is busy")
	if strings.Contains(hs.drawn(), "\x1b[2J") {
		t.Error("the reason is not cleaned")
	}
	if want := shellDefaultCommand + " "; d.field("Command").value() != want {
		t.Errorf("the command is %q", d.field("Command").value())
	}
	if dialogFocused(hs) != "Command" {
		t.Errorf("the focus is on %q", dialogFocused(hs))
	}
	if len(sess.Called("Exec")) != 0 || term.out.Len() != 0 {
		t.Error("something ran")
	}
	if actionBusy(sess) {
		t.Error("the keys are held")
	}
	term.refuse = nil
	hs.press("enter")
	if ex := shellExecs(sess); len(ex) != 1 || !slices.Equal(ex[0].opts.Command, shellDefaultWords) {
		t.Errorf("Exec was called so: %+v", ex)
	}
	if hs.root().dlg != nil || actionBusy(sess) {
		t.Error("a dialog is open or the keys are held")
	}
}

// The dialog holds none of the session's keys of actions while it is
// open, so a program that ends with it open leaves nothing held. Shell
// takes them; when something else has them then, it says so and closes.
func TestShellDialogHoldsNothing(t *testing.T) {
	sess, term, hs := shellSetup(t, shellPodYAML("", running("app"), nil, nil))
	inRunning := func() bool {
		actionsRunning.Lock()
		defer actionsRunning.Unlock()
		_, ok := actionsRunning.m[sess]
		return ok
	}
	hs.press("s")
	shellDialog(hs)
	if inRunning() {
		t.Error("the session is in actionsRunning while the dialog is open")
	}
	// Something takes the keys meanwhile: Shell says so, and closes.
	if !actionBegin(sess) {
		t.Fatal("the keys were held")
	}
	hs.press("enter")
	hs.wantFrame(actionTextBusy)
	if hs.root().dlg != nil || len(sess.Called("Exec")) != 0 || term.runs != 0 {
		t.Error("Shell ran or stayed open while the keys were held")
	}
	actionEnd(sess)
	// The program ends with the dialog open: nothing is left.
	hs.press("s")
	shellDialog(hs)
	hs.press("ctrl+c")
	if !hs.quit {
		t.Fatal("ctrl+c did not end the program")
	}
	if inRunning() {
		t.Error("the session is left in actionsRunning")
	}
}

func TestShellReadOnlyKeyRefused(t *testing.T) {
	// The session refuses Exec whatever the UI does; the UI says so as
	// everywhere.
	sess, _, hs := shellSetup(t, shellPodYAML("", running("app"), nil, nil))
	sess.Errs["Exec"] = kube.ErrReadOnly
	hs.press("s", "enter")
	hs.wantFrame("this key is read-only")
	if actionBusy(sess) {
		t.Error("the action keys are still held")
	}
}

func TestShellErrorTextIsCleaned(t *testing.T) {
	evil := "evil\x1b]0;owned\x07\x1b[2J\r"
	sess := newSession()
	sess.SetTable(query("pods", "default"), podsNamed("web"))
	// The table gives its rows' names cleaned; whoever gives the binding
	// a name as the API has it, the dialog and the line clean it.
	ref := kubetest.Ref("pods", "default", "web"+evil)
	sess.YAMLs[ref.String()] = shellPodYAML("", running("app"+evil), nil, nil)
	term := newShellTerm()
	hs := newShellHarness(t, sess, term, 100, 30)
	e := env{sess: sess, now: hs.clock.now, handover: term.handover}
	q := query("pods", "default")
	var shell binding
	for _, b := range bindingsFor(q.Resource, sess.Grant()) {
		if b.Key == "s" {
			shell = b
		}
	}
	bad := []string{"\x1b]0;", "\x1b[2J", "\r", "\x07"}
	check := func(what, s string) {
		t.Helper()
		for _, b := range bad {
			if strings.Contains(s, b) {
				t.Errorf("%s holds %q", what, b)
			}
		}
	}
	sess.Errs["Exec"] = fmt.Errorf("%w: exec: %s: not found", kube.ErrExecNotStarted, evil)
	hs.send(shell.Run(e, q, []kube.ObjectRef{ref})())
	shellDialog(hs)
	check("the dialog", hs.drawn())
	hs.wantFrame("A shell in pod default/webevil", "Container  appevil")
	hs.press("enter")
	shellDialog(hs)
	check("the dialog's reason", hs.drawn())
	hs.wantFrame("not found")
	if ex := shellExecs(sess); len(ex) != 1 || ex[0].opts.Container != "app"+evil || ex[0].pod.Name != "web"+evil {
		t.Errorf("Exec did not get the names as the API has them: %+v", ex)
	}
	line := term.firstLine()
	if !strings.HasPrefix(line, "kosh: a shell in pod default/webevil") || !strings.HasSuffix(line, ".\r\n") ||
		strings.Count(line, "\r") != 1 || strings.Contains(line, "\x1b") || strings.Contains(line, "\x07") {
		t.Errorf("the line written at the hand-over is %q", line)
	}
	hs.press("esc")
	sess.Errs["Exec"] = errors.New("refused: " + evil)
	hs.send(shell.Run(e, q, []kube.ObjectRef{ref})())
	hs.press("enter")
	check("the flash", hs.drawn())
	hs.wantFrame("refused: evil")
	if actionBusy(sess) {
		t.Error("the action keys are still held")
	}
}

func TestShellHoldsTheActionKeys(t *testing.T) {
	sess := actionSpyOf(newSession())
	sess.SetTable(query("pods", "default"), podsNamed("web", "zeta"))
	sess.YAMLs[shellWeb.String()] = shellPodYAML("", running("app"), nil, nil)
	term := newShellTerm()
	hs := newShellHarness(t, sess, term, 100, 30)
	e := env{sess: sess, now: hs.clock.now, handover: term.handover}
	q := query("pods", "default")
	var kill, shell binding
	for _, b := range bindingsFor(q.Resource, sess.Grant()) {
		switch b.Key {
		case "ctrl+k":
			kill = b
		case "s":
			shell = b
		}
	}
	busy := func() bool {
		t.Helper()
		msg := kill.Run(e, q, []kube.ObjectRef{shellWeb})()
		f, ok := msg.(flashMsg)
		return ok && f.text == actionTextBusy
	}

	// The reading holds them.
	read := shell.Run(e, q, []kube.ObjectRef{shellWeb})
	if !busy() {
		t.Error("the keys are free while the pod is read")
	}
	hs.send(read())
	// The dialog holds nothing: it has the keys itself.
	shellDialog(hs)
	if busy() {
		t.Error("the keys are held while the dialog is open")
	}
	// From the press, the shell holds them, and the return gives them
	// back.
	var during bool
	sess.ExecDo = func(context.Context, kube.ObjectRef, kube.ExecOptions) (kube.ExecResult, error) {
		during = busy()
		return kube.ExecResult{Started: true, Status: 1}, nil
	}
	hs.press("enter")
	if !during {
		t.Error("the keys are free while the shell runs")
	}
	hs.wantFrame("the shell ended with status 1")
	if busy() {
		t.Error("the keys are held after the return")
	}
	hs.press("ctrl+k")
	if d := dialogOpen(hs); d == nil || !strings.Contains(d.question, "Kill") {
		t.Error("ctrl+k does not open its dialog after the return")
	}
	hs.press("esc")
	// While they are held, s says so.
	if !actionBegin(sess) {
		t.Fatal("the keys are held")
	}
	hs.press("s")
	hs.wantFrame(actionTextBusy)
	if hs.root().dlg != nil {
		t.Error("s opened a dialog while an action runs")
	}
	actionEnd(sess)
}

func TestShellTableIsReadAgain(t *testing.T) {
	sess := newCountingSession()
	q := query("pods", "default")
	sess.SetTable(q, podsNamed("a", "b", "web", "z"))
	sess.YAMLs[shellWeb.String()] = shellPodYAML("", running("app"), nil, nil)
	term := newShellTerm()
	hs := newShellHarness(t, sess, term, 100, 30)
	hs.press("down", "down")
	if r := hs.table2(0).cursorRow(); r == nil || r.Name != "web" {
		t.Fatal("the cursor is not on web")
	}
	for _, c := range []struct {
		name string
		set  func()
	}{
		{"status 0", func() { sess.ExecResult = kube.ExecResult{Started: true} }},
		{"status 2", func() { sess.ExecResult = kube.ExecResult{Started: true, Status: 2} }},
		{"refused", func() { sess.Errs["Exec"] = errors.New("forbidden") }},
		{"not started", func() { sess.Errs["Exec"] = fmt.Errorf("%w: no", kube.ErrExecNotStarted) }},
	} {
		c.set()
		reads := len(sess.readsOf(q))
		hs.press("s", "enter")
		if got := len(sess.readsOf(q)); got <= reads {
			t.Errorf("%s: the table was not read again", c.name)
		}
		if hs.root().dlg != nil {
			hs.press("esc")
		}
		if r := hs.table2(0).cursorRow(); r == nil || r.Name != "web" {
			t.Errorf("%s: the cursor is not on web", c.name)
		}
		delete(sess.Errs, "Exec")
	}
}

func TestShellReadOnlyGrant(t *testing.T) {
	sess := kubetest.New(keys.Grant{Name: "bob@laptop", ReadOnly: true})
	sess.SetTable(query("pods", "default"), podsNamed("web"))
	sess.YAMLs[shellWeb.String()] = shellPodYAML("", running("app"), nil, nil)
	term := newShellTerm()
	hs := newShellHarness(t, sess, term, 100, 30)
	before := hs.frame()
	hs.press("s")
	if hs.root().dlg != nil {
		t.Error("s opened a dialog")
	}
	if hs.frame() != before {
		t.Errorf("the frame changed:\n%s\nwas:\n%s", hs.frame(), before)
	}
	for _, c := range sess.Calls() {
		if c.Method != "Resolve" && c.Method != "Watch" {
			t.Errorf("s called the session: %v", c)
		}
	}
	if term.runs != 0 {
		t.Error("the terminal was handed over")
	}
	// Called whatever the UI does, the session refuses it.
	if _, err := sess.Exec(context.Background(), shellWeb, kube.ExecOptions{Command: []string{"sh"}}); !errors.Is(err, kube.ErrReadOnly) {
		t.Errorf("the session's Exec gives %v to a read-only key", err)
	}
}

// A paste into Command that holds a line break or another control
// character is refused, and the field keeps its text: a paste does not
// smuggle a second line in. A paste of one line is taken, cut to what a
// field takes.
func TestShellPaste(t *testing.T) {
	sess, _, hs := shellSetup(t, shellPodYAML("", running("app"), nil, nil))
	hs.press("s")
	for _, paste := range []string{
		"ls\nrm -rf /",
		"ls\r",
		"ls\x1b[2J",
		"ls\x00",
		"ls\u009b",
		"ls\x7f",
		"ls\n\x1b[2J" + strings.Repeat("rm -rf / ;\r\n", 100000),
	} {
		hs.send(tea.PasteMsg{Content: paste})
		d := shellDialog(hs)
		if v := d.field("Command").value(); v != shellDefaultCommand {
			t.Errorf("a paste of %q changed the field to %q", paste[:min(len(paste), 20)], v[:min(len(v), 80)])
		}
		hs.wantFrame(shellTextPasteRefused)
		hs.press("end")
		hs.notFrame(shellTextPasteRefused)
	}
	// One line, of a megabyte, with tabs: taken, and cut.
	for range len(shellDefaultCommand) {
		hs.press("backspace")
	}
	hs.send(tea.PasteMsg{Content: "ls\t-l " + strings.Repeat("x", 1<<20)})
	v := shellDialog(hs).field("Command").value()
	if n := len([]rune(v)); n != inputMaxChars || !strings.HasPrefix(v, "ls") {
		t.Errorf("the field holds %d characters: %q", n, v[:min(len(v), 20)])
	}
	hs.notFrame(shellTextPasteRefused)
	hs.press("enter")
	ex := shellExecs(sess)
	if len(ex) != 1 || len(ex[0].opts.Command) != 3 || ex[0].opts.Command[0] != "ls" || ex[0].opts.Command[1] != "-l" {
		t.Fatalf("Exec was called so: %d calls", len(ex))
	}
	// The other dialogs take a paste as they did: on one line.
	other := dialogForm("q", nil, []*formField{formTextField("Name", "")})
	other.Update(tea.PasteMsg{Content: "a\nb"})
	if v := other.fields[0].value(); v != "a b" {
		t.Errorf("another dialog's field took a paste as %q", v)
	}
}

// A reason of any length (a webhook's, a runtime's) is drawn on a few
// lines at most: the dialog that comes back fits, with its buttons.
func TestShellLongReason(t *testing.T) {
	sess, _, hs := shellSetup(t, shellPodYAML("", running("app"), nil, nil))
	hs.send(tea.WindowSizeMsg{Width: 80, Height: 24})
	sess.Errs["Exec"] = fmt.Errorf("%w: %s", kube.ErrExecNotStarted, strings.Repeat("the runtime says no ", 200))
	hs.press("s", "enter")
	d := shellDialog(hs)
	hs.wantFrame("Cancel", "Shell", "the command could not be started", "…")
	if n := strings.Count(hs.frame(), "the runtime says no"); n > dialogWhyLines*3 {
		t.Errorf("the reason is drawn %d times over", n)
	}
	if !dialogFocusDrawn(hs) || dialogFocused(hs) != "Command" || d.field("Command").value() != shellDefaultCommand {
		t.Errorf("the dialog is not as it was:\n%s", hs.frame())
	}
}

// The session ends while a shell has the terminal: the hand-over's
// context is done, Exec returns its error, and done's message never
// comes to a program that is ending. run gives the keys back itself.
func TestShellEndsWithItsSession(t *testing.T) {
	sess, term, hs := shellSetup(t, shellPodYAML("", running("app"), nil, nil))
	ended := false
	handover := func(run func(context.Context, Terminal) error, done func(error) tea.Msg) tea.Cmd {
		return func() tea.Msg {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			run(ctx, Terminal{In: term.in, Out: &term.out, Size: term.size, Resized: term.resized})
			ended = true
			return nil
		}
	}
	sess.ExecDo = func(ctx context.Context, _ kube.ObjectRef, _ kube.ExecOptions) (kube.ExecResult, error) {
		return kube.ExecResult{}, ctx.Err()
	}
	e := env{sess: sess, now: hs.clock.now, handover: handover}
	q := query("pods", "default")
	var shell binding
	for _, b := range bindingsFor(q.Resource, sess.Grant()) {
		if b.Key == "s" {
			shell = b
		}
	}
	hs.send(shell.Run(e, q, []kube.ObjectRef{shellWeb})())
	shellDialog(hs)
	hs.press("enter")
	if !ended || len(sess.Called("Exec")) != 1 {
		t.Fatal("the shell did not run")
	}
	if actionBusy(sess) {
		t.Error("the session's entry is still in actionsRunning")
	}
	actionsRunning.Lock()
	_, held := actionsRunning.m[sess]
	actionsRunning.Unlock()
	if held {
		t.Error("the session is still a key of actionsRunning")
	}
}

// A panic under Exec does not end the program: the shell's error in
// the flash, the table back, the keys free.
func TestShellPanicIsAnError(t *testing.T) {
	sess, _, hs := shellSetup(t, shellPodYAML("", running("app"), nil, nil))
	sess.ExecDo = func(context.Context, kube.ObjectRef, kube.ExecOptions) (kube.ExecResult, error) {
		panic("stream \x1b[2Jbroke" + strings.Repeat(" and broke", 100000))
	}
	hs.press("s", "enter")
	hs.wantFrame("the shell failed: stream broke and broke")
	if !strings.Contains(hs.drawnLine("the shell failed"), sgr(styleError)) {
		t.Error("the panic is not an error flash")
	}
	if strings.Contains(hs.drawn(), "\x1b[2J") {
		t.Error("the panic's text is not cleaned")
	}
	if hs.root().dlg != nil || actionBusy(sess) {
		t.Error("a dialog is open or the keys are held")
	}
	if hs.title() != "Pods(default)[2]" {
		t.Errorf("the table is not back: %q", hs.title())
	}
	hs.press("ctrl+k")
	if d := dialogOpen(hs); d == nil || !strings.Contains(d.question, "Kill") {
		t.Error("another action key does not work")
	}
}

// From a pod's containers, s on a container that does not run says so,
// and opens nothing, while another container of the pod runs.
func TestShellContainerNotRunning(t *testing.T) {
	yaml := shellPodYAML("", []shellCtr{{"app", "running"}, {"late", "waiting"}}, []shellCtr{{"setup", "completed"}}, nil)
	sess, term, hs := shellSetup(t, yaml)
	sess.SetTable(queryOf(kube.Containers, shellWeb), kube.Table{
		Columns: []render.Column{{Name: "NAME"}},
		Rows: []render.Row{
			{Namespace: "default", Name: "app", Cells: []string{"app"}},
			{Namespace: "default", Name: "late", Cells: []string{"late"}},
			{Namespace: "default", Name: "setup", Cells: []string{"setup"}},
		},
	})
	hs.press("enter")
	tv := hs.table2(1)
	for range 3 {
		name := tv.ref(tv.cursorRow()).Name
		hs.press("s")
		if name == "app" {
			shellDialog(hs)
			hs.press("esc")
		} else {
			if hs.root().dlg != nil {
				t.Errorf("s on %s opened a dialog", name)
			}
			text := "container " + name + " of pod default/web is not running"
			hs.wantFrame(text)
			if strings.Contains(hs.drawnLine(text), sgr(styleError)) {
				t.Error("the flash is an error")
			}
		}
		if actionBusy(sess) {
			t.Errorf("the keys are held after s on %s", name)
		}
		hs.press("down")
	}
	if len(sess.Called("Exec")) != 0 || term.runs != 0 {
		t.Error("something was run")
	}
}
