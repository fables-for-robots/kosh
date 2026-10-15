package tui

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/fables-for-robots/kosh/keys"
	"github.com/fables-for-robots/kosh/kube"
	"github.com/fables-for-robots/kosh/kube/kubetest"
)

// The header names the key, whose name ends in its role: the role says
// what the session may do, and there is no mark of read-only.
func TestHeaderShowsNoReadOnly(t *testing.T) {
	view := kubetest.New(viewGrant)
	allowView(view)
	hs := newHarness(t, view, 100, 30)
	if l := hs.line("Key:"); !strings.Contains(l, "Key:     bob@laptop/view") || strings.Contains(l, "read-only") {
		t.Errorf("the header of a view key says %q", l)
	}
	hs = newHarness(t, newSession(), 100, 30)
	if l := hs.line("Key:"); !strings.Contains(l, "alice@laptop") || strings.Contains(l, "read-only") {
		t.Errorf("the header of a cluster-admin key says %q", l)
	}
	hs.wantFrame("Cluster: v1.33.1", "kosh:    v0.1.0")
}

func TestTooSmall(t *testing.T) {
	for _, size := range [][2]int{{79, 12}, {80, 11}, {40, 30}} {
		hs := newHarness(t, newSession(), size[0], size[1])
		// The line is cut to the window.
		want := fit(fmt.Sprintf("kosh needs a terminal of at least 80 by 12 (this one is %dx%d)", size[0], size[1]), size[0])
		if got := hs.lines(); len(got) != 1 || got[0] != ansi.Strip(want) {
			t.Errorf("at %dx%d the frame is %q, want the one line %q", size[0], size[1], got, want)
		}
	}
	if got := newHarness(t, newSession(), 80, 11).frame(); !strings.HasPrefix(got, "kosh needs a terminal of at least 80 by 12 (this one is 80x11)") {
		t.Errorf("at 80x11 the frame is %q", got)
	}
	for _, size := range [][2]int{{0, 0}, {1, 1}, {5, 2}, {0, 30}, {100, 0}, {1, 100}} {
		hs := newHarness(t, newSession(), size[0], size[1])
		hs.drawn() // checks the width and the height
		hs.press("ctrl+e")
		hs.drawn()
	}
	if f := newHarness(t, newSession(), 0, 0).frame(); f != "" {
		t.Errorf("a window of no size shows %q", f)
	}
	hs := newHarness(t, newSession(), 80, 12)
	if got := len(hs.lines()); got != 12 {
		t.Errorf("at 80x12 the frame has %d lines", got)
	}
	if !strings.HasPrefix(hs.lines()[6], "┌") {
		t.Errorf("at 80x12 the frame is not drawn:\n%s", hs.frame())
	}
}

func TestHeaderToggles(t *testing.T) {
	hs := newHarness(t, newSession(), 100, 30)
	inner := func() int {
		for i, l := range hs.lines() {
			if strings.HasPrefix(l, "┌") {
				return len(hs.lines()) - i
			}
		}
		return 0
	}
	with := inner()
	hs.press("ctrl+e")
	if got := inner(); got != with+6 {
		t.Errorf("without the header the body has %d lines, with it %d", got, with)
	}
	if strings.Contains(hs.frame(), "Key:") {
		t.Error("the header is still shown")
	}
	hs.press("ctrl+e")
	if got := inner(); got != with {
		t.Errorf("with the header again the body has %d lines, want %d", got, with)
	}
}

func TestFlashExpires(t *testing.T) {
	hs := newHarness(t, newSession(), 100, 30)
	hs.send(flashMsg{text: "scaled web\nto 3"})
	last := hs.lines()[len(hs.lines())-1]
	if !strings.Contains(last, "scaled web to 3") {
		t.Errorf("the flash line is %q", last)
	}
	hs.clock.advance(4 * time.Second)
	hs.wantFrame("scaled web to 3")
	hs.clock.advance(time.Second)
	hs.notFrame("scaled web")
	// An error is in the error style, and is cleaned.
	hs.send(flashMsg{text: "denied \x1b[2J", err: true})
	if l := hs.drawnLine("denied"); !strings.Contains(l, sgr(styleError)) || strings.Contains(l, "\x1b[2J") {
		t.Errorf("the flash of an error is drawn as %q", l)
	}
	// The next flash replaces it.
	hs.send(flashMsg{text: "next"})
	hs.notFrame("denied")
	hs.wantFrame("next")
}

func TestPushAndPop(t *testing.T) {
	hs := newHarness(t, newSession(), 100, 30)
	a, b := &testView{name: "a"}, &testView{name: "b"}
	hs.send(pushViewMsg{a})
	hs.send(pushViewMsg{b})
	if a.inits != 1 || b.inits != 1 {
		t.Errorf("Init was called %d and %d times", a.inits, b.inits)
	}
	if hs.title() != "Test(b)" {
		t.Errorf("the title is %q", hs.title())
	}
	if l := hs.lines()[len(hs.lines())-2]; words(l) != "<pods> <a> <b>" {
		t.Errorf("the crumbs are %q", l)
	}
	if l := hs.drawnLine("<b>"); !strings.Contains(l, styleTitle.Render("<b>")) {
		t.Errorf("the top crumb is not in the title style: %q", l)
	}
	hs.send(popViewMsg{})
	if b.closed != 1 || a.closed != 0 {
		t.Errorf("closed: a %d, b %d", a.closed, b.closed)
	}
	if a.got(refreshMsg{}) != 1 {
		t.Errorf("the view below got %d refreshMsg", a.got(refreshMsg{}))
	}
	if hs.title() != "Test(a)" {
		t.Errorf("the title is %q", hs.title())
	}
	hs.send(popViewMsg{})
	hs.send(popViewMsg{})
	hs.send(popViewMsg{})
	if len(hs.root().stack) != 1 {
		t.Errorf("the stack has %d views", len(hs.root().stack))
	}
	if a.closed != 1 {
		t.Errorf("a was closed %d times", a.closed)
	}
}

func TestEveryMessageGoesToEveryView(t *testing.T) {
	hs := newHarness(t, newSession(), 100, 30)
	a, b := &testView{name: "a"}, &testView{name: "b"}
	d := &testDialog{}
	hs.send(pushViewMsg{a})
	hs.send(pushViewMsg{b})
	hs.send(openDialogMsg{d})
	hs.send(testMsg{7})
	hs.send(tea.WindowSizeMsg{Width: 120, Height: 40})
	if a.got(tea.WindowSizeMsg{Width: 120, Height: 40}) != 1 || b.got(tea.WindowSizeMsg{Width: 120, Height: 40}) != 1 {
		t.Error("the size did not reach every view")
	}
	if a.got(testMsg{7}) != 1 || b.got(testMsg{7}) != 1 || countMsgs(d.msgs, testMsg{7}) != 1 {
		t.Errorf("the message reached a %d, b %d, the dialog %d times", a.got(testMsg{7}), b.got(testMsg{7}), countMsgs(d.msgs, testMsg{7}))
	}
	hs.send(closeDialogMsg{})
	hs.press("x")
	if len(a.keys) != 0 || strings.Join(b.keys, " ") != "x" {
		t.Errorf("the keys went to a %q and b %q", a.keys, b.keys)
	}
}

func TestDialogTakesTheKeys(t *testing.T) {
	hs := newHarness(t, newSession(), 100, 30)
	v := &testView{name: "v"}
	hs.send(pushViewMsg{v})
	d := &testDialog{}
	hs.send(openDialogMsg{d})
	if d.inits != 1 {
		t.Errorf("the dialog's Init was called %d times", d.inits)
	}
	hs.wantFrame("║ a dialog ║")
	// It is centred over the body.
	for _, l := range hs.lines() {
		if i := strings.Index(l, "║ a dialog ║"); i >= 0 {
			w := ansi.StringWidth(l[:i])
			if w < 40 || w > 50 {
				t.Errorf("the dialog starts at column %d: %q", w, l)
			}
		}
	}
	hs.press("j", ":", "?", "esc", "ctrl+e", "/", "enter")
	if got := strings.Join(d.keys, " "); got != "j : ? esc ctrl+e / enter" {
		t.Errorf("the dialog got %q", got)
	}
	if len(v.keys) != 0 {
		t.Errorf("the view got %q", v.keys)
	}
	hs.wantFrame("Key:") // ctrl+e was not the root's
	hs.press("ctrl+c")
	if !hs.quit {
		t.Error("ctrl+c did not end the session over a dialog")
	}
	hs.send(closeDialogMsg{})
	hs.notFrame("a dialog")
	hs.press("j")
	if strings.Join(v.keys, " ") != "j" {
		t.Errorf("after the dialog the view got %q", v.keys)
	}
}

func TestCapturingViewTakesTheKeys(t *testing.T) {
	hs := newHarness(t, newSession(), 100, 30)
	v := &testView{name: "v", capturing: true}
	hs.send(pushViewMsg{v})
	hs.press(":", "?", "/", "ctrl+e", "[", "ctrl+a")
	if got := strings.Join(v.keys, " "); got != ": ? / ctrl+e [ ctrl+a" {
		t.Errorf("the capturing view got %q", got)
	}
	hs.wantFrame("Key:")
	hs.press("ctrl+c")
	if !hs.quit {
		t.Error("ctrl+c did not end the session")
	}
}

func TestFullscreenViewIsDrawnAlone(t *testing.T) {
	hs := newHarness(t, newSession(), 100, 30)
	v := &testView{name: "full", fullscreen: true}
	hs.send(pushViewMsg{v})
	lines := hs.lines()
	if v.w != 100 || v.h != 30 {
		t.Errorf("the fullscreen view was given %dx%d", v.w, v.h)
	}
	if len(lines) != 30 || !strings.HasPrefix(lines[0], "full line 0") || !strings.HasPrefix(lines[29], "full line 29") {
		t.Errorf("the frame is not the view alone:\n%s", hs.frame())
	}
	hs.notFrame("Key:", "<pods>", "┌")
	v.fullscreen = false
	hs.send(testMsg{})
	hs.frame()
	if v.w != 98 || v.h >= 30 {
		t.Errorf("in the frame the view was given %dx%d", v.w, v.h)
	}
}

func TestTheStartCanFail(t *testing.T) {
	sess := newSession()
	sess.Errs["Resolve"] = errors.New("the API is down")
	hs := newHarness(t, sess, 100, 30)
	if l := hs.drawnLine("the API is down"); !strings.Contains(l, sgr(styleError)) {
		t.Errorf("the error is not shown in the body in the error style: %q\n%s", l, hs.frame())
	}
}

// What the frame shows of a view, a grant and the cluster is cleaned and
// kept to its line: the title, the crumb, the hints, the help, the key's
// name, the version, the namespaces.
func TestEverythingDrawnIsCleaned(t *testing.T) {
	evil := "\x1b]0;x\x07\x1b[2J\nsecond line"
	sess := kubetest.New(keys.Grant{Name: "alice" + evil})
	sess.Version = "v1" + evil
	hs := newHarness(t, sess, 100, 30)
	hs.command("po kube" + "\x1b[2J" + "system")
	v := &namedView{testView: testView{name: "plain", hints: []hint{{"k" + evil, "text" + evil}}}, as: "v" + evil}
	hs.send(pushViewMsg{v})
	check := func(what string) {
		t.Helper()
		f := hs.drawn()
		for _, bad := range []string{"\x1b]0;", "\x1b[2J", "\x07"} {
			if strings.Contains(f, bad) {
				t.Errorf("%s: the frame holds %q", what, bad)
			}
		}
		if lines := hs.lines(); len(lines) != 30 {
			t.Errorf("%s: the frame has %d lines", what, len(lines))
		}
	}
	check("a view")
	if !strings.HasPrefix(hs.title(), "Test(v second line)") {
		t.Errorf("the title is %q", hs.title())
	}
	hs.press("?")
	check("the help")
	hs.wantFrame("V SECOND LINE")
	hs.press("esc", "esc")
	check("the pods")
}

// namedView is a test view whose title and crumb are what as says.
type namedView struct {
	testView
	as string
}

func (v *namedView) Title() string { return "Test(" + v.as + ")" }
func (v *namedView) Crumb() string { return v.as }
func (v *namedView) Update(msg tea.Msg) (view, tea.Cmd) {
	v.testView.Update(msg)
	return v, nil
}

// The error of the start is wrapped into the body, its line feeds
// breaking its lines, and cleaned.
func TestTheStartErrorIsWrapped(t *testing.T) {
	sess := newSession()
	sess.Errs["Resolve"] = errors.New("down\nfor maintenance \x1b[2J")
	hs := newHarness(t, sess, 100, 30)
	hs.wantFrame("│down ", "│for maintenance ")
	if strings.Contains(hs.drawn(), "\x1b[2J") {
		t.Error("the error is drawn raw")
	}
}

// The prompt of a fullscreen view is its window's last line, and the view
// has the lines above it while the prompt is open.
func TestPromptOverAFullscreenView(t *testing.T) {
	hs := newHarness(t, newSession(), 100, 30)
	v := &filterView{testView: testView{name: "full", fullscreen: true}}
	hs.send(pushViewMsg{v})
	for _, open := range []string{"/", ":"} {
		hs.press(open)
		hs.typeText("web")
		lines := hs.lines()
		if len(lines) != 30 || !strings.HasPrefix(lines[29], open+"web") {
			t.Errorf("with %s open the last line is %q", open, lines[len(lines)-1])
		}
		if v.w != 100 || v.h != 29 || !strings.HasPrefix(lines[28], "full line 28") {
			t.Errorf("with %s open the view was given %dx%d", open, v.w, v.h)
		}
		hs.press("esc")
		lines = hs.lines()
		if v.h != 30 || len(lines) != 30 || !strings.HasPrefix(lines[29], "full line 29") {
			t.Errorf("after %s closed the view was given %dx%d, the last line is %q", open, v.w, v.h, lines[len(lines)-1])
		}
	}
	if got := strings.Join(v.filters, ","); got != "w,we,web," {
		t.Errorf("the filter was given %s", got)
	}
	// No window size panics with the prompt open over it.
	hs.press(":")
	for _, size := range [][2]int{{100, 1}, {80, 1}, {1, 1}, {80, 12}, {0, 0}} {
		hs.send(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		hs.drawn()
	}
}

// Closing the usage watch, as the session's end does, ends the root's
// wait on it: the wait returns nothing, the root waits no more, and the
// header keeps what it last read.
func TestClosingTheUsageWatchEndsTheWait(t *testing.T) {
	sess := newSession()
	hs := newHarness(t, sess, 100, 30)
	sess.SetUsage(kube.ClusterUsage{CPU: 12, Mem: 47, Served: true}, true)
	hs.awaitFrame("CPU:     12%")

	hs.root().usageWatch.Close()
	// The wait in flight ends with a nil message, which is dropped;
	// nothing else arrives, so the frame is as it was and no later
	// reading can be seen.
	select {
	case msg := <-hs.async:
		if msg != nil {
			t.Fatalf("the wait on a closed watch returned %#v, want nil", msg)
		}
	case <-time.After(deadline):
		t.Fatal("the wait on a closed watch did not end")
	}
	hs.wantFrame("CPU:     12%")
	sess.SetUsage(kube.ClusterUsage{CPU: 50, Mem: 50, Served: true}, true)
	select {
	case msg := <-hs.async:
		t.Fatalf("a message came after the watch was closed: %#v", msg)
	case <-time.After(100 * time.Millisecond):
	}
	hs.wantFrame("CPU:     12%")
}
