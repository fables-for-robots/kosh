package tui

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/fables-for-robots/kosh/kube"
	"github.com/fables-for-robots/kosh/kube/kubetest"
)

// logPod is the pod of the tests.
var logPod = kubetest.Ref("pods", "default", "web-1")

// logOpenIn opens the log of ref in hs as the keys do, and returns its
// view and the fake's stream.
func logOpenIn(t *testing.T, hs *harness, sess *kubetest.Session, ref kube.ObjectRef, opts kube.LogOptions) (*logView, *kubetest.LogStream) {
	t.Helper()
	hs.send(logOpen(hs.root().e, ref, opts)())
	v, ok := hs.root().top().(*logView)
	if !ok {
		t.Fatalf("the top view is %T, not the log. The frame:\n%s", hs.root().top(), hs.frame())
	}
	s := sess.LogOf(ref)
	if s == nil {
		t.Fatalf("Logs was not called for %s", ref)
	}
	return v, s
}

// logTexts returns n lines of web-1/app, "line 0001" for from 1.
func logTexts(from, n int) []kube.LogLine {
	out := make([]kube.LogLine, n)
	for i := range out {
		out[i] = kube.LogLine{Pod: "web-1", Container: "app", Text: fmt.Sprintf("line %04d", from+i)}
	}
	return out
}

// logHas reports whether the last line of v's buffer says text.
func logHas(v *logView, text string) bool {
	return len(v.lines) > 0 && v.lines[len(v.lines)-1].text == text
}

// logBody returns the lines of the frame's body, without the frame and
// without the empty lines at its end.
func logBody(hs *harness) []string {
	hs.t.Helper()
	var out []string
	for _, l := range hs.lines() {
		if strings.HasPrefix(l, "│") {
			out = append(out, strings.TrimRight(strings.Trim(l, "│"), " "))
		}
	}
	for len(out) > 0 && out[len(out)-1] == "" {
		out = out[:len(out)-1]
	}
	return out
}

func TestLogBufferKeepsTheLastLines(t *testing.T) {
	sess := newSession()
	hs := newHarness(t, sess, 100, 30)
	v, s := logOpenIn(t, hs, sess, logPod, kube.LogOptions{})
	for i := range 60 {
		s.Send(logTexts(i*100+1, 100)...)
	}
	hs.await("the 6000th line", func() bool { return logHas(v, "line 6000") })
	if len(v.lines) != kube.LogBufferLines {
		t.Errorf("the buffer holds %d lines", len(v.lines))
	}
	if got := v.lines[0].text; got != "line 1001" {
		t.Errorf("the first line held is %q", got)
	}
	hs.wantFrame("line 6000")
	hs.notFrame("line 1000")
}

func TestLogDrawingIsThrottled(t *testing.T) {
	sess := newSession()
	hs := newHarness(t, sess, 100, 30)
	below := &testView{name: "below"}
	hs.send(pushViewMsg{below})
	v, s := logOpenIn(t, hs, sess, logPod, kube.LogOptions{})
	start := time.Now()
	go func() {
		for i := 1; i <= 200; i++ {
			s.Send(logTexts(i, 1)...)
		}
	}()
	hs.await("every line", func() bool { return len(v.lines) == 200 })
	elapsed := time.Since(start)
	hs.wantFrame("line 0200")
	// Every message reaches every view: the one below counts them.
	var reads []logReadMsg
	for _, m := range below.msgs {
		if r, ok := m.(logReadMsg); ok {
			reads = append(reads, r)
		}
	}
	if most := int(elapsed/logRedrawEvery) + 2; len(reads) > most || len(reads) > 20 {
		t.Errorf("200 batches in %v were %d redraws, not at most %d", elapsed, len(reads), most)
	}
	for i := 1; i < len(reads); i++ {
		if d := reads[i].at.Sub(reads[i-1].at); d < logRedrawEvery-5*time.Millisecond {
			t.Errorf("redraw %d came %v after the one before", i, d)
		}
	}
}

func TestLogAutoscroll(t *testing.T) {
	sess := newSession()
	hs := newHarness(t, sess, 100, 30)
	v, s := logOpenIn(t, hs, sess, logPod, kube.LogOptions{})
	s.Send(logTexts(1, 50)...)
	hs.await("50 lines", func() bool { return logHas(v, "line 0050") })
	// The body is 20 lines high: the last 20 are shown.
	body := logBody(hs)
	if len(body) != 20 || body[0] != "line 0031" || body[19] != "line 0050" {
		t.Errorf("the body is %q", body)
	}
	if !strings.Contains(hs.title(), "[autoscroll:on]") {
		t.Errorf("the title is %q", hs.title())
	}
	// Up turns autoscroll off, and the view stays where it is.
	hs.press("up")
	if !strings.Contains(hs.title(), "[autoscroll:off]") {
		t.Errorf("after up the title is %q", hs.title())
	}
	body = logBody(hs)
	if body[0] != "line 0030" || body[19] != "line 0049" {
		t.Errorf("after up the body is %q", body)
	}
	s.Send(logTexts(51, 10)...)
	hs.await("60 lines", func() bool { return logHas(v, "line 0060") })
	if after := logBody(hs); strings.Join(after, "\n") != strings.Join(body, "\n") {
		t.Errorf("the view moved while lines came: %q", after)
	}
	// s turns it on: the end is shown again.
	hs.press("s")
	body = logBody(hs)
	if !strings.Contains(hs.title(), "[autoscroll:on]") || body[19] != "line 0060" {
		t.Errorf("after s the title is %q and the body %q", hs.title(), body)
	}
	// The other keys move.
	for _, step := range []struct {
		keys []string
		top  string
	}{
		{[]string{"g"}, "line 0001"},
		{[]string{"j", "down"}, "line 0003"},
		{[]string{"k"}, "line 0002"},
		{[]string{"pgdown"}, "line 0022"},
		{[]string{"ctrl+f", "ctrl+f"}, "line 0041"},
		{[]string{"pgup"}, "line 0021"},
		{[]string{"ctrl+b"}, "line 0001"},
		{[]string{"G"}, "line 0041"},
		{[]string{"home"}, "line 0001"},
		{[]string{"end"}, "line 0041"},
	} {
		hs.press(step.keys...)
		if got := logBody(hs)[0]; got != step.top {
			t.Errorf("after %v the top line is %q, want %q", step.keys, got, step.top)
		}
	}
	if !strings.Contains(hs.title(), "[autoscroll:off]") {
		t.Errorf("after g the title is %q", hs.title())
	}
}

func TestLogEndOfLog(t *testing.T) {
	sess := newSession()
	hs := newHarness(t, sess, 100, 30)
	_, s := logOpenIn(t, hs, sess, logPod, kube.LogOptions{})
	s.Send(logTexts(1, 1)...)
	s.End(nil)
	hs.awaitFrame("— the log has ended —")
	body := logBody(hs)
	if body[len(body)-2] != "line 0001" || body[len(body)-1] != "— the log has ended —" {
		t.Errorf("the body ends in %q", body[len(body)-2:])
	}

	other := kubetest.Ref("pods", "default", "web-2")
	_, s = logOpenIn(t, hs, sess, other, kube.LogOptions{})
	s.End(errors.New("the connection broke"))
	hs.awaitFrame("— the log broke: the connection broke —")
	if l := hs.drawnLine("the log broke"); !strings.Contains(l, sgr(styleError)) {
		t.Errorf("the line is not an error: %q", l)
	}
}

func TestLogSkippedContainers(t *testing.T) {
	sess := newSession()
	hs := newHarness(t, sess, 100, 30)
	deploy := kubetest.Ref("deployments", "default", "web")
	v, s := logOpenIn(t, hs, sess, deploy, kube.LogOptions{})
	if strings.Contains(hs.title(), "left out") {
		t.Errorf("the title is %q", hs.title())
	}
	s.SetSkipped(4)
	s.Send(logTexts(1, 1)...)
	hs.await("a line", func() bool { return logHas(v, "line 0001") })
	if !strings.HasSuffix(hs.title(), " (4 containers left out)") {
		t.Errorf("the title is %q", hs.title())
	}
}

func TestLogCloseClosesTheStream(t *testing.T) {
	sess := newSession()
	hs := newHarness(t, sess, 100, 30)
	v, s := logOpenIn(t, hs, sess, logPod, kube.LogOptions{})
	hs.press("esc")
	if !s.Closed() {
		t.Error("esc left the stream open")
	}
	if v.ctx.Err() == nil {
		t.Error("esc left the context of the view alive")
	}
	if _, ok := hs.root().top().(*logView); ok {
		t.Error("esc did not pop the view")
	}
	// q pops it too.
	_, s = logOpenIn(t, hs, sess, logPod, kube.LogOptions{})
	hs.press("q")
	if !s.Closed() {
		t.Error("q left the stream open")
	}
}

// logClipboard returns what the last tea.SetClipboard that reached v
// set, and whether one did.
func logClipboard(v *testView) (string, bool) {
	for i := len(v.msgs) - 1; i >= 0; i-- {
		m := v.msgs[i]
		if fmt.Sprintf("%T", m) == fmt.Sprintf("%T", tea.SetClipboard("")()) {
			return reflect.ValueOf(m).String(), true
		}
	}
	return "", false
}

func TestLogTimes(t *testing.T) {
	sess := newSession()
	hs := newHarness(t, sess, 100, 30)
	v, s := logOpenIn(t, hs, sess, logPod, kube.LogOptions{})
	s.Send(
		kube.LogLine{Pod: "web-1", Container: "app", Time: t0.Add(1500 * time.Millisecond), Text: "with a time"},
		kube.LogLine{Pod: "web-1", Container: "app", Text: "without a time"},
	)
	hs.await("two lines", func() bool { return logHas(v, "without a time") })
	if body := logBody(hs); strings.Join(body, "\n") != "with a time\nwithout a time" {
		t.Errorf("the times are shown at first: %q", body)
	}
	hs.press("t")
	if body := logBody(hs); strings.Join(body, "\n") != "2026-10-13T12:00:01Z with a time\nwithout a time" {
		t.Errorf("after t the body is %q", body)
	}
	hs.press("t")
	if body := logBody(hs); strings.Join(body, "\n") != "with a time\nwithout a time" {
		t.Errorf("t again shows %q", body)
	}
}

func TestLogWrap(t *testing.T) {
	sess := newSession()
	hs := newHarness(t, sess, 100, 30)
	v, s := logOpenIn(t, hs, sess, logPod, kube.LogOptions{})
	var long strings.Builder
	for i := range 300 {
		long.WriteByte(byte('a' + i%26))
	}
	s.Send(kube.LogLine{Pod: "web-1", Container: "app", Time: t0, Text: long.String()}, kube.LogLine{Pod: "web-1", Container: "app", Text: "short"})
	hs.await("the lines", func() bool { return logHas(v, "short") })
	body := logBody(hs)
	if len(body) != 2 || !strings.HasSuffix(body[0], "…") || ansi.StringWidth(body[0]) != 98 {
		t.Errorf("the long line is not cut at the width: %q", body)
	}
	hs.press("w")
	body = logBody(hs)
	if len(body) != 5 || strings.Join(body[:4], "") != long.String() || body[4] != "short" {
		t.Errorf("after w the body is %q", body)
	}
	hs.press("t")
	body = logBody(hs)
	if len(body) != 5 || strings.Join(body[:4], "") != "2026-10-13T12:00:00Z "+long.String() {
		t.Errorf("wrapped with times the body is %q", body)
	}
	// No drawn line is wider than the view, whatever its width.
	for _, w := range []int{1, 2, 7, 20, 98} {
		for _, wrap := range []bool{false, true} {
			v.wrap = wrap
			for i, l := range strings.Split(v.View(w, 20), "\n") {
				if ansi.StringWidth(l) > w {
					t.Errorf("wrap %v: line %d is %d cells wide in a view of %d", wrap, i, ansi.StringWidth(l), w)
				}
			}
		}
	}
}

func TestLogsOfAWorkload(t *testing.T) {
	sess := newSession()
	hs := newHarness(t, sess, 100, 30)
	v, s := logOpenIn(t, hs, sess, kubetest.Ref("deployments", "default", "web"), kube.LogOptions{})
	s.Send(
		kube.LogLine{Pod: "web-1", Container: "app", Text: "one"},
		kube.LogLine{Pod: "web-2", Container: "app", Text: "two"},
		kube.LogLine{Pod: "web-1", Container: "app", Text: "three"},
	)
	hs.await("three lines", func() bool { return logHas(v, "three") })
	if body := logBody(hs); strings.Join(body, "\n") != "web-1/app one\nweb-2/app two\nweb-1/app three" {
		t.Errorf("the body is %q", body)
	}
	one, two := logPalette[logColorOf("web-1/app")], logPalette[logColorOf("web-2/app")]
	if sgr(one) == sgr(two) {
		t.Fatalf("web-1/app and web-2/app have the same colour")
	}
	for _, c := range []struct {
		text, prefix string
		style        lipgloss.Style
	}{{"one", "web-1/app", one}, {"two", "web-2/app", two}, {"three", "web-1/app", one}} {
		if l := hs.drawnLine(c.text); !strings.Contains(l, c.style.Render(c.prefix)+" "+c.text) {
			t.Errorf("the prefix of %q is not in the colour of its pod: %q", c.text, l)
		}
	}
	// A log of one container has no prefixes.
	w, s1 := logOpenIn(t, hs, sess, logPod, kube.LogOptions{})
	s1.Send(logTexts(1, 1)...)
	hs.await("a line", func() bool { return logHas(w, "line 0001") })
	if body := logBody(hs); strings.Join(body, "\n") != "line 0001" {
		t.Errorf("the log of one container is %q", body)
	}
}

func TestLogMarkAndClear(t *testing.T) {
	sess := newSession()
	hs := newHarness(t, sess, 100, 30)
	v, s := logOpenIn(t, hs, sess, logPod, kube.LogOptions{})
	s.Send(logTexts(1, 2)...)
	hs.await("two lines", func() bool { return logHas(v, "line 0002") })
	hs.clock.advance(90 * time.Second)
	hs.press("m")
	body := logBody(hs)
	if len(body) != 3 || !strings.HasPrefix(body[2], "──") || !strings.Contains(body[2], " 2026-10-13T12:01:30Z ") || ansi.StringWidth(body[2]) != 98 {
		t.Errorf("after m the body is %q", body)
	}
	s.Send(logTexts(3, 1)...)
	hs.await("the third line", func() bool { return logHas(v, "line 0003") })
	if body := logBody(hs); len(body) != 4 || body[3] != "line 0003" {
		t.Errorf("the line after the mark: %q", body)
	}
	hs.press("C")
	if body := logBody(hs); len(body) != 0 {
		t.Errorf("after C the body is %q", body)
	}
	s.Send(logTexts(4, 1)...)
	hs.await("the fourth line", func() bool { return logHas(v, "line 0004") })
	if body := logBody(hs); strings.Join(body, "\n") != "line 0004" {
		t.Errorf("after C and a line the body is %q", body)
	}
}

func TestLogCopy(t *testing.T) {
	sess := newSession()
	hs := newHarness(t, sess, 100, 30)
	below := &testView{name: "below"}
	hs.send(pushViewMsg{below})
	v, s := logOpenIn(t, hs, sess, kubetest.Ref("deployments", "default", "web"), kube.LogOptions{})
	s.Send(
		kube.LogLine{Pod: "web-1", Container: "app", Time: t0, Text: "one"},
		kube.LogLine{Pod: "web-2", Container: "app", Time: t0.Add(time.Second), Text: strings.Repeat("long ", 40)},
	)
	hs.await("two lines", func() bool { return len(v.lines) == 2 })
	hs.press("t", "c")
	got, ok := logClipboard(below)
	want := "web-1/app 2026-10-13T12:00:00Z one\nweb-2/app 2026-10-13T12:00:01Z " + strings.Repeat("long ", 40)
	if !ok || got != want {
		t.Errorf("c copied %q (%v), want %q", got, ok, want)
	}
	if strings.Contains(got, "\x1b") {
		t.Errorf("the copy has styles: %q", got)
	}
	hs.wantFrame("copied 2 lines")
}

func TestLogFullscreen(t *testing.T) {
	sess := newSession()
	hs := newHarness(t, sess, 100, 30)
	v, s := logOpenIn(t, hs, sess, logPod, kube.LogOptions{})
	s.Send(logTexts(1, 40)...)
	hs.await("40 lines", func() bool { return logHas(v, "line 0040") })
	hs.press("f")
	lines := hs.lines()
	if len(lines) != 30 || strings.TrimSpace(lines[0]) != "line 0011" || strings.TrimSpace(lines[29]) != "line 0040" {
		t.Errorf("full screen is %q", lines)
	}
	hs.notFrame("┌", "<logs>")
	hs.press("f")
	hs.wantFrame("┌", "<logs>")
}

func TestLogLinesAreCleaned(t *testing.T) {
	sess := newSession()
	hs := newHarness(t, sess, 100, 30)
	below := &testView{name: "below"}
	hs.send(pushViewMsg{below})
	v, s := logOpenIn(t, hs, sess, kubetest.Ref("deployments", "default", "web"), kube.LogOptions{})
	s.Send(
		kube.LogLine{Pod: "web-1\x1b[2J", Container: "app", Text: "a\x1b]0;x\x07b"},
		kube.LogLine{Pod: "web-2", Container: "app\x00", Text: "c\x1b[2Jd"},
		kube.LogLine{Pod: "web-2", Container: "app", Text: "e\rf"},
		kube.LogLine{Pod: "web-2", Container: "app", Text: "g\x00h"},
	)
	hs.await("four lines", func() bool { return len(v.lines) == 4 })
	s.End(errors.New("broke \x1b[31mred\x1b]0;title\x07"))
	hs.awaitFrame("— the log broke: broke red —")
	hs.press("c")
	copied, _ := logClipboard(below)
	for what, text := range map[string]string{"the frame": hs.drawn(), "the copy": copied} {
		for _, bad := range []string{"\x1b]", "\x1b[2J", "\x1b[31m", "\r", "\x00", "\x07"} {
			if strings.Contains(text, bad) {
				t.Errorf("%s holds %q: %q", what, bad, text)
			}
		}
	}
	for _, want := range []string{"web-1/app ab", "web-2/app␀ cd", "web-2/app e␍f", "web-2/app g␀h", "— the log broke: broke red —"} {
		if !strings.Contains(copied, want) {
			t.Errorf("the copy does not hold %q: %q", want, copied)
		}
		hs.wantFrame(want)
	}
}
