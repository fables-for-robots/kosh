package tui

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/fables-for-robots/kosh/kube/kubetest"
)

// textPod is the pod the text views of the tests are about.
var textPod = kubetest.Ref("pods", "default", "web-1")

// textOpenWith opens a text view of kind with text, made by test code,
// over the table of pods, in a window of 100 by 30: its body is 98 by
// 20.
func textOpenWith(t *testing.T, kind textKind, text string) (*harness, *textView) {
	t.Helper()
	sess := newSession()
	sess.SetTable(query("pods", "default"), podsNamed("web-1"))
	hs := newHarness(t, sess, 100, 30)
	v := textNew(hs.root().e, kind, textPod, text)
	hs.send(pushViewMsg{v})
	return hs, v
}

// textNumbered returns n lines "line 000", "line 001" and so on.
func textNumbered(n int) string {
	lines := make([]string, n)
	for i := range lines {
		lines[i] = fmt.Sprintf("line %03d", i)
	}
	return strings.Join(lines, "\n")
}

// textBody returns the lines of the body inside its frame, plain and
// without the spaces at their ends.
func textBody(hs *harness) []string {
	hs.t.Helper()
	var out []string
	for _, l := range hs.lines() {
		if strings.HasPrefix(l, "│") {
			out = append(out, strings.TrimRight(strings.TrimSuffix(strings.TrimPrefix(l, "│"), "│"), " "))
		}
	}
	return out
}

// textFirst returns the first line of the body.
func textFirst(hs *harness) string {
	hs.t.Helper()
	b := textBody(hs)
	if len(b) == 0 {
		hs.t.Fatalf("the frame has no body:\n%s", hs.frame())
	}
	return b[0]
}

// textLoaded is the message of a reading of v that brought text.
func textLoaded(v *textView, text string) textLoadedMsg {
	return textLoadedMsg{id: v.id, gen: v.loads, lines: textLines(text)}
}

// textMsgs runs cmd and the commands of its batches, and returns their
// messages.
func textMsgs(cmd tea.Cmd) []tea.Msg {
	if cmd == nil {
		return nil
	}
	msg := cmd()
	if b, ok := msg.(tea.BatchMsg); ok {
		var out []tea.Msg
		for _, c := range b {
			out = append(out, textMsgs(c)...)
		}
		return out
	}
	return []tea.Msg{msg}
}

// textClipboard returns the text of the clipboard message among msgs,
// and whether there is one.
func textClipboard(msgs []tea.Msg) (string, bool) {
	for _, m := range msgs {
		if fmt.Sprintf("%T", m) == "tea.setClipboardMsg" {
			return reflect.ValueOf(m).String(), true
		}
	}
	return "", false
}

func TestScrolling(t *testing.T) {
	hs, v := textOpenWith(t, textDescribe, textNumbered(200))
	if got := len(textBody(hs)); got != 20 {
		t.Fatalf("the body has %d lines, want 20", got)
	}
	for _, step := range []struct {
		keys  []string
		first string
	}{
		{nil, "line 000"},
		{[]string{"up"}, "line 000"},
		{[]string{"k"}, "line 000"},
		{[]string{"down"}, "line 001"},
		{[]string{"j", "j"}, "line 003"},
		{[]string{"up"}, "line 002"},
		{[]string{"k"}, "line 001"},
		{[]string{"pgdown"}, "line 021"},
		{[]string{"ctrl+f"}, "line 041"},
		{[]string{"space"}, "line 061"},
		{[]string{"pgup"}, "line 041"},
		{[]string{"ctrl+b"}, "line 021"},
		{[]string{"ctrl+b", "ctrl+b"}, "line 000"},
		{[]string{"G"}, "line 180"},
		{[]string{"down", "j", "pgdown", "space", "ctrl+f"}, "line 180"},
		{[]string{"g"}, "line 000"},
		{[]string{"end"}, "line 180"},
		{[]string{"home"}, "line 000"},
		{[]string{"pgup", "up"}, "line 000"},
	} {
		hs.press(step.keys...)
		if got := textFirst(hs); got != step.first {
			t.Errorf("after %q the first line is %q, want %q", step.keys, got, step.first)
		}
	}
	hs.press("G")
	if b := textBody(hs); b[len(b)-1] != "line 199" {
		t.Errorf("after G the last line of the body is %q", b[len(b)-1])
	}
	// The content shrinks under the position: it is brought back in.
	hs.send(textLoaded(v, textNumbered(30)))
	b := textBody(hs)
	if b[0] != "line 010" || b[len(b)-1] != "line 029" {
		t.Errorf("after the content shrank the body is %q … %q", b[0], b[len(b)-1])
	}
	hs.send(textLoaded(v, textNumbered(5)))
	if b := textBody(hs); b[0] != "line 000" || b[4] != "line 004" || b[5] != "" {
		t.Errorf("after the content shrank below a page the body is %q", b)
	}
	hs.press("G", "pgdown", "down")
	if got := textFirst(hs); got != "line 000" {
		t.Errorf("a content shorter than a page scrolls to %q", got)
	}
	// Before it is first drawn, the view keeps the position in the
	// content too.
	v2 := textNew(hs.root().e, textDescribe, textPod, textNumbered(3))
	for _, k := range []string{"G", "pgdown", "down", "down"} {
		v2.Update(keyPress(k))
	}
	if v2.top < 0 || v2.top > 2 {
		t.Errorf("before it is drawn the position is %d in 3 lines", v2.top)
	}
}

func TestSidewaysAndWrap(t *testing.T) {
	long := strings.Repeat("0123456789", 29) + "abcdefghij"
	hs, v := textOpenWith(t, textDescribe, "short\n"+long+"\nend\n"+textNumbered(30))
	line := func() string { return textBody(hs)[1] }
	check := func(when string) {
		t.Helper()
		out := v.View(98, 20)
		lines := strings.Split(out, "\n")
		if len(lines) > 20 {
			t.Errorf("%s: the view draws %d lines in 20", when, len(lines))
		}
		for i, l := range lines {
			if w := ansi.StringWidth(l); w > 98 {
				t.Errorf("%s: line %d of the view is %d wide in 98", when, i, w)
			}
		}
	}
	check("at first")
	if got := line(); !strings.HasPrefix(got, "0123456789") || !strings.HasSuffix(got, "…") {
		t.Errorf("the long line is drawn %q", got)
	}
	hs.press("right")
	got := line()
	if strings.HasPrefix(got, "0123456789") || !strings.Contains(long, strings.TrimSuffix(got, "…")) {
		t.Errorf("after → the long line is drawn %q", got)
	}
	check("after →")
	for range 100 {
		hs.press("right")
	}
	if got := line(); !strings.HasSuffix(got, "abcdefghij") || strings.Contains(got, "…") || len(got) != 98 {
		t.Errorf("at its right end the long line is drawn %q", got)
	}
	check("at the right end")
	if b := textBody(hs); b[0] != "" || b[2] != "" {
		t.Errorf("the short lines scrolled out are drawn %q and %q", b[0], b[2])
	}
	for range 100 {
		hs.press("left")
	}
	if got := line(); !strings.HasPrefix(got, "0123456789") {
		t.Errorf("back at the left the long line is drawn %q", got)
	}

	hs.press("right", "w")
	b := textBody(hs)
	if b[0] != "short" || b[1]+b[2]+b[3]+b[4] != long || len(b[1]) != 98 || b[5] != "end" {
		t.Errorf("wrapped, the body is %q", b[:6])
	}
	check("wrapped")
	hs.press("right")
	if after := textBody(hs); strings.Join(after, "\n") != strings.Join(b, "\n") {
		t.Errorf("→ moved a wrapped text: %q", after[:6])
	}
	// Scrolling goes by the lines as they are drawn.
	hs.press("down", "down")
	if got := textFirst(hs); got != long[98:196] {
		t.Errorf("two lines down in the wrapped text the first line is %q", got)
	}
	hs.press("w")
	if got := textFirst(hs); !strings.HasPrefix(got, "0123456789") {
		t.Errorf("unwrapped again, the first line is %q", got)
	}
	check("unwrapped")
	// A narrow window.
	hs.send(tea.WindowSizeMsg{Width: 80, Height: 12})
	hs.press("w")
	hs.drawn()
	hs.press("w", "right", "right")
	hs.drawn()
}

func TestFullscreen(t *testing.T) {
	hs, v := textOpenWith(t, textDescribe, textNumbered(100))
	if v.Fullscreen() {
		t.Fatal("the view starts in full screen")
	}
	hs.wantFrame("Key:", "┌", "<describe>")
	hs.press("f")
	if !v.Fullscreen() {
		t.Fatal("f does not make it full screen")
	}
	lines := hs.lines()
	if len(lines) != 30 || strings.TrimRight(lines[0], " ") != "line 000" || strings.TrimRight(lines[29], " ") != "line 029" {
		t.Errorf("in full screen the frame is %d lines from %q to %q", len(lines), lines[0], lines[len(lines)-1])
	}
	hs.notFrame("Key:", "┌", "<describe>")
	hs.press("pgdown")
	if got := strings.TrimRight(hs.lines()[0], " "); got != "line 030" {
		t.Errorf("a page in full screen goes to %q", got)
	}
	hs.press("f")
	hs.wantFrame("Key:", "┌", "<describe>")
	if got := textFirst(hs); got != "line 030" {
		t.Errorf("out of full screen the first line is %q", got)
	}
}

func TestCopy(t *testing.T) {
	hs, v := textOpenWith(t, textDescribe, "one\n\x1b[2Jtwo\x1b]0;x\x07\nthree\x00\n")
	_, cmd := v.Update(keyPress("c"))
	msgs := textMsgs(cmd)
	got, ok := textClipboard(msgs)
	if !ok {
		t.Fatalf("c gives no clipboard command, but %v", msgs)
	}
	if want := "one\ntwo\nthree␀"; got != want {
		t.Errorf("c copies %q, want %q", got, want)
	}
	found := false
	for _, m := range msgs {
		if m == (flashMsg{text: "copied 3 lines"}) {
			found = true
		}
	}
	if !found {
		t.Errorf("c flashes nothing of the lines it copied: %v", msgs)
	}
	hs.press("c")
	if l := hs.lines()[29]; !strings.Contains(l, "copied 3 lines") {
		t.Errorf("the flash says %q", l)
	}
}
