package tui

import (
	"fmt"
	"reflect"
	"runtime/debug"
	"strings"
	"testing"
	"time"

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
	hs.drawn() // as the program draws it before the next key
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
	c := textPrepare(text, v.kind, "", nil, 0)
	return textLoadedMsg{id: v.id, gen: v.loads, content: &c}
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
// and fails the test when there is none.
//
// The message tea.SetClipboard makes is of a type Bubble Tea does not
// export (setClipboardMsg, a string), and the root does not keep it: it
// goes on to the views like any message the root does not know. So the
// test finds it by the name of its type and reads its string through
// reflection. When Bubble Tea renames the type, this fails and says so.
func textClipboard(t *testing.T, msgs []tea.Msg) string {
	t.Helper()
	for _, m := range msgs {
		if fmt.Sprintf("%T", m) == "tea.setClipboardMsg" {
			return reflect.ValueOf(m).String()
		}
	}
	version := "of unknown version"
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, d := range bi.Deps {
			if d.Path == "charm.land/bubbletea/v2" {
				version = d.Version
			}
		}
	}
	t.Fatalf("no tea.setClipboardMsg among %#v: has Bubble Tea (charm.land/bubbletea/v2 %s) renamed the message of tea.SetClipboard?", msgs, version)
	return ""
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
	// A narrow window keeps the wrapped position: the row of the cell
	// that was at the top.
	hs.press("w", "down", "down")
	hs.send(tea.WindowSizeMsg{Width: 80, Height: 30})
	if got := textFirst(hs); got != long[156:234] {
		t.Errorf("after the window narrowed the first line is %q", got)
	}
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
	got := textClipboard(t, msgs)
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

func TestSearch(t *testing.T) {
	lines := strings.Split(textNumbered(80), "\n")
	lines[5] = "error one"
	lines[30] = "an Error, and nothing else"
	lines[50] = "stderr"
	lines[60] = "a [ bracket"
	hs, _ := textOpenWith(t, textDescribe, strings.Join(lines, "\n"))
	base := "Describe(pods/default/web-1)"
	hs.press("/")
	hs.typeText("err")
	hs.press("enter")
	if got := hs.title(); got != base+" </err> 1/3" {
		t.Errorf("after /err the title is %q", got)
	}
	if got := textFirst(hs); got != "error one" {
		t.Errorf("after /err the first line is %q", got)
	}
	if l := hs.drawnLine("error one"); !strings.Contains(l, styleMatch.Render("err")) {
		t.Errorf("the match is not drawn in its style: %q", l)
	}
	if l := hs.drawnLine("line 006"); strings.Contains(l, sgr(styleMatch)) {
		t.Errorf("a line without a match is drawn with one: %q", l)
	}
	for _, step := range []struct {
		key, title, first string
	}{
		{"n", "2/3", "an Error, and nothing else"},
		{"n", "3/3", "stderr"},
		{"n", "1/3", "error one"},
		{"N", "3/3", "stderr"},
		{"shift+n", "2/3", "an Error, and nothing else"},
		{"N", "1/3", "error one"},
	} {
		hs.press(step.key)
		if got := hs.title(); got != base+" </err> "+step.title {
			t.Errorf("after %s the title is %q, want %s", step.key, got, step.title)
		}
		if got := textFirst(hs); got != step.first {
			t.Errorf("after %s the first line is %q, want %q", step.key, got, step.first)
		}
	}
	hs.press("n")
	if l := hs.drawnLine("an Error"); !strings.Contains(l, styleMatch.Render("Err")) {
		t.Errorf("the match without regard to case is not drawn in its style: %q", l)
	}

	// The search starts at the top line.
	hs.press("g", "/")
	hs.typeText("line 04")
	hs.press("enter")
	if got, first := hs.title(), textFirst(hs); got != base+" </line 04> 1/10" || first != "line 040" {
		t.Errorf("after /line 04 from the top the title is %q and the first line %q", got, first)
	}
	hs.press("/")
	hs.typeText("line 0")
	hs.press("enter")
	if got, first := hs.title(), textFirst(hs); got != base+" </line 0> 39/76" || first != "line 040" {
		t.Errorf("after /line 0 from line 40 the title is %q and the first line %q", got, first)
	}

	// No regular expression: the text as it stands.
	hs.press("/")
	hs.typeText("[")
	hs.press("enter")
	if got := hs.title(); got != base+" </[> 1/1" {
		t.Errorf("after /[ the title is %q", got)
	}
	if got := textFirst(hs); got != "a [ bracket" {
		t.Errorf("after /[ the first line is %q", got)
	}
	hs.press("/")
	hs.typeText("nothing like it")
	hs.press("enter")
	if got := hs.title(); got != base+" </nothing like it> 0/0" {
		t.Errorf("without a match the title is %q", got)
	}
	hs.press("n", "N")

	// The empty text clears the search.
	hs.press("/")
	hs.typeText("err")
	hs.press("backspace", "backspace", "backspace")
	if got := hs.title(); got != base {
		t.Errorf("after the text is taken back the title is %q", got)
	}
	if strings.Contains(hs.drawn(), sgr(styleMatch)) {
		t.Error("a match is drawn after the search was cleared")
	}
	hs.press("esc")
	hs.press("/")
	hs.typeText("err")
	hs.press("esc")
	if got := hs.title(); got != base {
		t.Errorf("after esc in the prompt the title is %q", got)
	}

	// esc clears the search first and pops the view second.
	hs.press("/")
	hs.typeText("err")
	hs.press("enter", "esc")
	if got := hs.title(); got != base || len(hs.root().stack) != 2 {
		t.Errorf("after esc the title is %q and the stack %d views", got, len(hs.root().stack))
	}
	hs.press("esc")
	if len(hs.root().stack) != 1 {
		t.Errorf("after esc again the stack has %d views", len(hs.root().stack))
	}
}

func TestSearchInAWrappedYAML(t *testing.T) {
	long := "message: " + strings.Repeat("x", 150) + "needle" + strings.Repeat("y", 50)
	hs, _ := textOpenWith(t, textYAML, "kind: Pod\n"+textNumbered(40)+"\n"+long+"\n"+textNumbered(40))
	hs.press("w", "/")
	hs.typeText("NEEDLE")
	hs.press("enter")
	first := textFirst(hs)
	if !strings.Contains(first, "needle") {
		t.Errorf("the wrapped row of the match is not at the top: %q", first)
	}
	if l := hs.drawnLine("needle"); !strings.Contains(l, styleMatch.Render("needle")) {
		t.Errorf("the match in a wrapped line is not drawn in its style: %q", l)
	}
	// Unwrapped, the view goes sideways to the match.
	hs.press("w", "n")
	if l := hs.drawnLine("needle"); !strings.Contains(l, styleMatch.Render("needle")) {
		t.Errorf("the match far in a line is not shown: %q", l)
	}
	// The key keeps its colour under the search.
	hs.press("/")
	hs.typeText("ind")
	hs.press("enter", "g")
	if l := hs.drawnLine("kind: Pod"); !strings.Contains(l, styleKey.Render("k")+styleMatch.Render("ind")) {
		t.Errorf("a match in a key is drawn %q", l)
	}
}

// textTime fails the test when do takes longer than limit times the
// race detector's factor.
func textTime(t *testing.T, what string, limit time.Duration, do func()) {
	t.Helper()
	start := time.Now()
	do()
	if took, most := time.Since(start), limit*textRaceFactor; took > most {
		t.Errorf("%s took %v, more than %v", what, took, most)
	}
}

func TestLongLinesDrawFast(t *testing.T) {
	// One line of a megabyte, of a hundred thousand matches.
	long := "x: " + strings.Repeat("abcdefghij", 100_000)
	hs, v := textOpenWith(t, textYAML, "kind: Pod\n"+long+"\nend: here\n")
	hs.press("/", "a", "enter")
	if len(v.matches) != 100_000 {
		t.Fatalf("/a has %d matches", len(v.matches))
	}
	redraw := func(what string) {
		t.Helper()
		textTime(t, "a hundred redraws "+what, 200*time.Millisecond, func() {
			for range 100 {
				v.View(98, 20)
			}
		})
		out := v.View(98, 20)
		if !strings.Contains(out, styleMatch.Render("a")) {
			t.Errorf("%s: the matches are not drawn: %q", what, ansi.Strip(out))
		}
		if lines := strings.Split(out, "\n"); len(lines) != 20 {
			t.Errorf("%s: the view draws %d lines", what, len(lines))
		}
	}
	redraw("unwrapped")
	hs.press("n")
	for range 1000 {
		v.Update(keyPress("right"))
	}
	redraw("far to the right")
	if l := strings.Split(ansi.Strip(v.View(98, 20)), "\n")[1]; len(l) < 90 || !strings.Contains(long[8000:], strings.TrimSuffix(l, "…")) {
		t.Errorf("far to the right the long line is drawn %q", l)
	}
	hs.press("w")
	redraw("wrapped")
	hs.press("pgdown", "pgdown", "pgdown")
	redraw("wrapped, three pages down")
	if l := strings.Split(ansi.Strip(v.View(98, 20)), "\n")[0]; !strings.Contains(long, l) || len(l) != 98 {
		t.Errorf("three pages down the wrapped line is drawn %q", l)
	}
	at := v.at
	textTime(t, "a hundred n", 200*time.Millisecond, func() {
		for range 100 {
			v.Update(keyPress("n"))
		}
	})
	if v.at != at+100 {
		t.Errorf("a hundred n went from match %d to %d", at, v.at)
	}
	hs.press("G")
	redraw("wrapped, at the end")
	if b := textBody(hs); b[len(b)-1] != "end: here" {
		t.Errorf("at the end the last line is %q", b[len(b)-1])
	}
}

func TestReadingIsPreparedInTheCommand(t *testing.T) {
	hs, v := textOpenWith(t, textYAML, "kind: Pod\n")
	sess := hs.root().e.sess.(*kubetest.Session)
	long := "x: " + strings.Repeat("abcdefghij", 100_000) // 1 MB
	sess.YAMLs[textPod.String()] = "kind: Pod\n" + long + "\n"
	hs.press("w", "/", "a", "enter")
	_, cmd := v.Update(keyPress("m"))
	msgs := textMsgs(cmd)
	if len(msgs) != 1 {
		t.Fatalf("m gives %v", msgs)
	}
	msg, ok := msgs[0].(textLoadedMsg)
	if !ok || msg.content == nil {
		t.Fatalf("m gives %#v", msgs[0])
	}
	c := msg.content
	if c.width != len(long) || len(c.lines) != 2 || c.lines[1].width != len(long) {
		t.Errorf("the reading brings the width %d of %d lines", c.width, len(c.lines))
	}
	if c.search != "a" || len(c.matches) != 100_000 {
		t.Errorf("the reading brings %d matches of %q", len(c.matches), c.search)
	}
	if c.wrapW != 98 || len(c.rows) != 1+(len(long)+97)/98 {
		t.Errorf("the reading brings %d rows wrapped at %d", len(c.rows), c.wrapW)
	}
	// Measuring, finding or wrapping a megabyte again would take more.
	textTime(t, "taking a reading of 1 MB", 10*time.Millisecond, func() {
		v.Update(msg)
		v.View(98, 20)
	})
	if got := hs.title(); got != "YAML(pods/default/web-1) </a> 0/100000" {
		t.Errorf("after the reading the title is %q", got)
	}
	// A reading for another search, or another width, is brought up to
	// date when it is taken.
	_, cmd = v.Update(keyPress("m"))
	msg = textMsgs(cmd)[0].(textLoadedMsg)
	hs.press("/", "b", "enter", "w")
	v.Update(msg)
	if len(v.matches) != 100_000 || v.lines[1].text[v.matches[0].from:v.matches[0].to] != "b" {
		t.Errorf("a reading of another search is taken with the matches of %q", v.lines[1].text[v.matches[0].from:v.matches[0].to])
	}
	if len(v.layout()) != 2 {
		t.Errorf("a reading of another width is taken with %d rows", len(v.layout()))
	}
}

func TestViewIsExactlyItsHeight(t *testing.T) {
	_, v := textOpenWith(t, textDescribe, "one\ntwo")
	for _, size := range [][2]int{{98, 20}, {10, 3}, {1, 1}, {98, 1}} {
		lines := strings.Split(v.View(size[0], size[1]), "\n")
		if len(lines) != size[1] {
			t.Errorf("at %dx%d the view draws %d lines", size[0], size[1], len(lines))
		}
		for i, l := range lines {
			if w := ansi.StringWidth(l); w != size[0] {
				t.Errorf("at %dx%d line %d is %d wide", size[0], size[1], i, w)
			}
		}
	}
	if got := v.View(0, 5); got != "" {
		t.Errorf("at 0x5 the view draws %q", got)
	}
	empty := textNew(v.e, textDescribe, textPod, "")
	if lines := strings.Split(empty.View(10, 4), "\n"); len(lines) != 4 {
		t.Errorf("an empty text draws %d lines", len(lines))
	}
}
