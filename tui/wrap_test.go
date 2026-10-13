package tui

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/fables-for-robots/kosh/kube"
)

// longError returns an error text of n characters, of words as the API
// words them.
func longError(n int) string {
	var b strings.Builder
	b.WriteString(`Deployment.apps "web" is invalid:`)
	for i := 0; b.Len() < n; i++ {
		fmt.Fprintf(&b, " spec.template.spec.containers[%d].livenessProbe.httpGet.port: Invalid value: 0", i)
	}
	return b.String()[:n]
}

// wantWords fails the test unless the words of lines are those of text,
// or the first of them when the last line ends in "…".
func wantWords(t *testing.T, lines []string, text string) {
	t.Helper()
	got := words(strings.Join(lines, " "))
	if cut, ok := strings.CutSuffix(got, "…"); ok {
		if !strings.HasPrefix(words(text), cut) {
			t.Errorf("the lines\n%s\nare not the start of %q", strings.Join(lines, "\n"), text)
		}
		return
	}
	if got != words(text) {
		t.Errorf("the lines\n%s\nare not %q", strings.Join(lines, "\n"), text)
	}
}

// flashLines returns the lines of the frame under the crumbs, without
// those that are empty.
func flashLines(hs *harness) []string {
	hs.t.Helper()
	lines := hs.lines()
	if len(lines) != hs.h {
		hs.t.Errorf("the frame has %d lines in a window of %d", len(lines), hs.h)
	}
	var out []string
	for i := len(lines) - 1; i >= 0 && !strings.HasPrefix(lines[i], "<"); i-- {
		if l := strings.TrimSpace(lines[i]); l != "" {
			out = append([]string{l}, out...)
		}
	}
	return out
}

// bodyLines returns the lines inside the frame of the body, from the
// first that holds text to the last that is not empty.
func bodyLines(hs *harness, text string) []string {
	hs.t.Helper()
	var out []string
	in := false
	for _, l := range hs.lines() {
		if !strings.HasPrefix(l, "│") {
			if in {
				break
			}
			continue
		}
		inner := strings.TrimSpace(strings.Trim(l, "│"))
		if strings.Contains(inner, text) {
			in = true
		}
		if in && inner != "" {
			out = append(out, inner)
		}
	}
	return out
}

func TestErrorFlashWraps(t *testing.T) {
	hs := newHarness(t, newSession(), 80, 24)
	text := longError(300)
	hs.send(flashMsg{text: text, err: true})
	fl := flashLines(hs)
	if len(fl) < 2 || len(fl) > flashMaxLines {
		t.Fatalf("the error is on %d lines:\n%s", len(fl), hs.frame())
	}
	wantWords(t, fl, text)
	for _, l := range strings.Split(hs.drawn(), "\n")[24-len(fl):] {
		if !strings.Contains(l, sgr(styleError)) {
			t.Errorf("a line of the error is not in the error style: %q", l)
		}
	}
	// It stays after its time until a key is pressed.
	hs.clock.advance(flashFor + time.Second)
	if got := flashLines(hs); len(got) != len(fl) {
		t.Errorf("after its time the error is %q", got)
	}
	hs.press("j")
	if got := flashLines(hs); len(got) != 0 {
		t.Errorf("after a key the error is %q", got)
	}

	// A key pressed before its time leaves it for its time.
	hs.send(flashMsg{text: text, err: true})
	hs.clock.advance(time.Second)
	hs.press("j")
	if got := flashLines(hs); len(got) != len(fl) {
		t.Errorf("a key at once took the error away: %q", got)
	}
	hs.clock.advance(flashFor)
	if got := flashLines(hs); len(got) != 0 {
		t.Errorf("after its time and a key the error is %q", got)
	}

	// A short error and an information are one line, as before.
	hs.send(flashMsg{text: "denied", err: true})
	if got := flashLines(hs); len(got) != 1 || got[0] != "denied" {
		t.Errorf("a short error is %q", got)
	}
	hs.send(flashMsg{text: text})
	if got := flashLines(hs); len(got) != 1 || !strings.HasSuffix(got[0], "…") {
		t.Errorf("a long information is %q", got)
	}
	hs.clock.advance(flashFor)
	if got := flashLines(hs); len(got) != 0 {
		t.Errorf("an information outlives its time: %q", got)
	}

	// A line feed breaks the line; escape sequences, controls and bytes
	// that are not UTF-8 are not drawn as they came.
	hs.send(flashMsg{text: "denied\x1b[2J\n\nby the webhook\x1b]0;owned\x07 policy\tA\xff\rB", err: true})
	fl = flashLines(hs)
	if len(fl) != 2 || fl[0] != "denied" || !strings.HasPrefix(fl[1], "by the webhook policy") {
		t.Errorf("the error is %q", fl)
	}
	d := hs.drawn()
	for _, bad := range []string{"\x1b[2J", "\x1b]0;", "owned", "\xff", "\r", "\t"} {
		if strings.Contains(d, bad) {
			t.Errorf("the frame holds %q", bad)
		}
	}

	// Every size keeps the frame's height, an error of 4 lines included.
	for _, size := range [][2]int{{80, 12}, {80, 13}, {120, 40}, {200, 12}} {
		hs.send(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		hs.send(flashMsg{text: longError(2000), err: true})
		fl := flashLines(hs)
		if len(fl) == 0 || len(fl) > flashMaxLines {
			t.Errorf("at %dx%d the error is on %d lines", size[0], size[1], len(fl))
		}
		for _, l := range fl {
			if ansi.StringWidth(l) > size[0] {
				t.Errorf("at %dx%d a line is %d cells", size[0], size[1], ansi.StringWidth(l))
			}
		}
		if !strings.HasSuffix(fl[len(fl)-1], "…") {
			t.Errorf("at %dx%d an error cut short does not end in …: %q", size[0], size[1], fl)
		}
	}
}

func TestEditorErrorStays(t *testing.T) {
	sess := newSession()
	sess.YAMLs[editorRef.String()] = editorYAML
	text := longError(300)
	sess.Errs["Update"] = errors.New(text)
	hs := newHarness(t, sess, 80, 24)
	actionOpen(hs, sess, "configmaps", simpleTable("cfg"))
	hs.press("e")
	editorShown(hs)
	hs.typeText("x")
	hs.press("ctrl+s")
	hs.awaitFrame(`Deployment.apps "web" is invalid:`)
	wantWords(t, bodyLines(hs, `Deployment.apps "web"`), text)
	if l := bodyLines(hs, `Deployment.apps "web"`); strings.HasSuffix(l[len(l)-1], "…") {
		t.Errorf("the error is not shown whole:\n%s", hs.frame())
	}
	// It stays while the text is edited.
	hs.typeText("y")
	hs.press("right")
	wantWords(t, bodyLines(hs, `Deployment.apps "web"`), text)
	if v := editorShown(hs); !strings.HasPrefix(v.ta.Value(), "xy") {
		t.Errorf("the text is %q", v.ta.Value())
	}
	// It goes when the next save is sent.
	delete(sess.Errs, "Update")
	hs.press("ctrl+s")
	hs.notFrame(`Deployment.apps "web"`)
	hs.awaitFrame("Updated configmaps/default/cfg")
}

func TestTableErrorWraps(t *testing.T) {
	text := longError(300)
	q := query("pods", "default")
	sess := newSession()
	sess.SetTable(q, kube.Table{Err: errors.New(text)})
	hs := newHarness(t, sess, 80, 24)
	wantWords(t, bodyLines(hs, `Deployment.apps "web"`), text)
	if l := bodyLines(hs, `Deployment.apps "web"`); strings.HasSuffix(l[len(l)-1], "…") {
		t.Errorf("the error is not shown whole:\n%s", hs.frame())
	}

	// The error of a watch that could not be opened, too.
	sess = newSession()
	sess.Errs["Watch"] = errors.New(text)
	hs = newHarness(t, sess, 80, 24)
	wantWords(t, bodyLines(hs, `Deployment.apps "web"`), text)
}

// wrapText measures in cells, breaks a word longer than a line, and
// draws nothing that clean would not.
func TestWrapText(t *testing.T) {
	for _, tt := range []struct {
		s    string
		w    int
		most int
	}{
		{strings.Repeat("界", 50) + " " + strings.Repeat("x", 100), 7, 0},
		{"a\x1b[31mb\x1b]8;;http://x\x07c \x9b2J\xff\xfe" + strings.Repeat(" é", 40), 9, 3},
		{"\n\n\t\n", 10, 0},
		{"abc", 1, 2},
	} {
		lines := wrapText(tt.s, tt.w, tt.most)
		if tt.most > 0 && len(lines) > tt.most {
			t.Errorf("%q at %d: %d lines, at most %d", tt.s, tt.w, len(lines), tt.most)
		}
		for _, l := range lines {
			if ansi.StringWidth(l) > tt.w {
				t.Errorf("%q at %d: the line %q is %d cells", tt.s, tt.w, l, ansi.StringWidth(l))
			}
			if clean(l) != l || strings.ContainsAny(l, "\n\x1b") {
				t.Errorf("%q at %d: the line %q is not clean", tt.s, tt.w, l)
			}
		}
	}
	if got := wrapText("one two three", 0, 0); got != nil {
		t.Errorf("at width 0: %q", got)
	}
}

// An error's text is cut to errorMaxBytes before it is cleaned and
// wrapped, in the flash, the editor and a table, so that a huge message
// costs a bounded amount at every reading.
func TestHugeErrorIsCut(t *testing.T) {
	huge := strings.Repeat("denied by the webhook ", 1<<16) // 1.4 MB
	bounded := func(where, s string) {
		t.Helper()
		if len(s) > errorMaxBytes+len("…") || !strings.HasSuffix(s, "…") {
			t.Errorf("%s keeps %d bytes, ending %q", where, len(s), s[max(len(s)-10, 0):])
		}
	}
	bounded("errorText", errorText(huge))
	// The logs keep an error on one line.
	if l := errorLine(strings.Repeat("broken\n", 1<<15)); strings.Contains(l, "\n") {
		t.Error("errorLine keeps a line feed")
	} else {
		bounded("errorLine", l)
	}
	if got := errorText("short\x1b[2J"); got != "short" {
		t.Errorf("a short error is %q", got)
	}
	// A cut inside an OSC string still shows: the mark comes after the
	// cleaning, which would take it into the string.
	if got := errorText("denied \x1b]0;" + strings.Repeat("y", errorMaxBytes)); !strings.HasSuffix(got, "…") || strings.Contains(got, "yyy") {
		t.Errorf("a cut inside an OSC string is %q", got[:min(len(got), 40)])
	}
	// A cut in a character of several bytes keeps the text UTF-8.
	if got := errorText(strings.Repeat("é", errorMaxBytes)); !utf8.ValidString(got) {
		t.Error("a cut in a character leaves bytes that are not UTF-8")
	}

	bounded("the table", prepare(kube.Table{Err: errors.New(huge)}).err)

	hs := newHarness(t, newSession(), 80, 24)
	hs.send(flashMsg{text: huge, err: true})
	bounded("the flash", hs.root().flash.text)

	sess := newSession()
	sess.Errs["Update"] = errors.New(huge)
	hs = editorStart(t, sess, true)
	hs.typeText("x")
	hs.press("ctrl+s")
	hs.awaitFrame("denied by the webhook")
	bounded("the editor", editorShown(hs).why)
}
