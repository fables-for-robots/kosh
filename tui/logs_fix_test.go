package tui

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/fables-for-robots/kosh/kube"
	"github.com/fables-for-robots/kosh/kube/kubetest"
)

// logKeepLast keeps the last lines in the array it was given and lets go
// of the others.
func TestLogKeepLastLetsGo(t *testing.T) {
	s := make([]logLine, 10)
	for i := range s {
		s[i].text = fmt.Sprint("line ", i)
	}
	r := logKeepLast(s, 3)
	if len(r) != 3 || r[0].text != "line 7" || r[2].text != "line 9" {
		t.Fatalf("kept %v", r)
	}
	// Whatever the array still holds is a line that is kept.
	for i, l := range s {
		if l.text != "" && (i >= len(r) || l.text != r[i].text) {
			t.Errorf("the array still holds %q at %d", l.text, i)
		}
	}
	if r := logKeepLast(s[:2], 3); len(r) != 2 {
		t.Errorf("keeping more than there are gives %d", len(r))
	}
}

func TestLogBufferStaysBounded(t *testing.T) {
	sess := newSession()
	hs := newHarness(t, sess, 100, 30)
	v, s := logOpenIn(t, hs, sess, logPod, kube.LogOptions{})
	go func() {
		for i := range 100 {
			s.Send(logTexts(i*1000+1, 1000)...)
		}
	}()
	most := 0
	hs.await("100 000 lines", func() bool {
		most = max(most, cap(v.lines), cap(v.shown))
		return logHas(v, "line 100000")
	})
	if most > 2*kube.LogBufferLines {
		t.Errorf("the capacity of the buffer reached %d", most)
	}
	if len(v.lines) != kube.LogBufferLines || v.lines[0].text != "line 95001" {
		t.Errorf("the buffer holds %d lines from %q", len(v.lines), v.lines[0].text)
	}
	for i, l := range v.lines[len(v.lines):cap(v.lines)] {
		if l.text != "" {
			t.Errorf("beyond the buffer, at %d, is %q", i, l.text)
		}
	}
}

// logWrapSetup opens a log of n lines of the length long, wrapped, in a
// body of 98 by 20.
func logWrapSetup(t *testing.T, n, long int) (*harness, *logView) {
	t.Helper()
	sess := newSession()
	hs := newHarness(t, sess, 100, 30)
	v, s := logOpenIn(t, hs, sess, logPod, kube.LogOptions{})
	var lines []kube.LogLine
	for i := 1; i <= n; i++ {
		text := fmt.Sprintf("line %04d ", i)
		for j := 0; len(text) < long; j++ {
			text += string(rune('a' + j%26))
		}
		lines = append(lines, kube.LogLine{Pod: "web-1", Container: "app", Text: text})
	}
	s.Send(lines...)
	hs.await("the lines", func() bool { return len(v.lines) == n })
	hs.press("w")
	return hs, v
}

func TestLogWrappedEnd(t *testing.T) {
	// 30 lines of 3 rows: the end is 2 rows of one line and 6 whole.
	hs, _ := logWrapSetup(t, 30, 250)
	end := logBody(hs)
	if len(end) != 20 || !strings.HasPrefix(end[2], "line 0025 ") || !strings.HasSuffix(end[19], "def") {
		t.Fatalf("the end is %q", end)
	}
	same := func(what string) {
		t.Helper()
		if got := logBody(hs); strings.Join(got, "\n") != strings.Join(end, "\n") {
			t.Errorf("%s shows %q, not the end %q", what, got, end)
		}
	}
	hs.press("s")
	same("s off at the end")
	hs.press("s")
	same("s on again")
	hs.press("g", "G")
	same("G")
	hs.press("end")
	same("end")
	// Up moves one row.
	hs.press("up")
	if got := logBody(hs); strings.Join(got[1:], "\n") != strings.Join(end[:19], "\n") {
		t.Errorf("up shows %q", got)
	}
	hs.press("down")
	same("down at the end")
	hs.press("down")
	same("down beyond the end")
}

func TestLogTallLastLine(t *testing.T) {
	// One line of 25 rows in a body of 20: the end is its last 20.
	hs, _ := logWrapSetup(t, 1, 98*25)
	end := logBody(hs)
	if len(end) != 20 || strings.HasPrefix(end[0], "line 0001") || !strings.HasSuffix(end[19], "v") {
		t.Fatalf("the end is %q", end)
	}
	hs.press("g")
	if got := logBody(hs); !strings.HasPrefix(got[0], "line 0001") {
		t.Errorf("g shows %q", got[0])
	}
	hs.press("G")
	if got := logBody(hs); strings.Join(got, "\n") != strings.Join(end, "\n") {
		t.Errorf("G shows %q", got)
	}
}

func TestLogViewSizes(t *testing.T) {
	sess := newSession()
	hs := newHarness(t, sess, 100, 30)
	v, s := logOpenIn(t, hs, sess, logPod, kube.LogOptions{})
	check := func(what string) {
		t.Helper()
		for _, w := range []int{1, 2, 7, 20, 98} {
			for _, h := range []int{1, 2, 5, 20} {
				for _, wrap := range []bool{false, true} {
					v.wrap = wrap
					lines := strings.Split(v.View(w, h), "\n")
					if len(lines) != h {
						t.Errorf("%s, %dx%d, wrap %v: %d lines", what, w, h, wrap, len(lines))
					}
					for i, l := range lines {
						if ansi.StringWidth(l) != w {
							t.Errorf("%s, %dx%d, wrap %v: line %d is %d cells wide", what, w, h, wrap, i, ansi.StringWidth(l))
						}
					}
				}
			}
		}
		v.wrap = false
	}
	check("empty")
	s.Send(kube.LogLine{Pod: "web-1", Container: "app", Text: strings.Repeat("x", 300)}, kube.LogLine{Pod: "web-1", Container: "app", Text: "short"})
	hs.await("the lines", func() bool { return logHas(v, "short") })
	check("two lines")
	hs.press("s")
	check("autoscroll off")
}

// logBlockingSession is a session whose Logs waits until its context is
// done.
type logBlockingSession struct{ *kubetest.Session }

func (s logBlockingSession) Logs(ctx context.Context, ref kube.ObjectRef, opts kube.LogOptions) (kube.LogStream, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestLogOpenTimesOut(t *testing.T) {
	hs := newHarness(t, logBlockingSession{newSession()}, 100, 30)
	done := make(chan tea.Msg, 1)
	go func() { done <- logOpenWithin(hs.root().e, logPod, kube.LogOptions{}, 20*time.Millisecond)() }()
	select {
	case msg := <-done:
		hs.send(msg)
	case <-time.After(deadline):
		t.Fatal("the opening did not end")
	}
	if _, ok := hs.root().top().(*logView); ok {
		t.Error("a view was pushed")
	}
	if l := hs.drawnLine("did not open"); !strings.Contains(l, "the log of pods/default/web-1 did not open in 20ms") || !strings.Contains(l, sgr(styleError)) {
		t.Errorf("the flash is %q", l)
	}

	// Once open, the stream's context outlives the timeout.
	sess := newSession()
	hs = newHarness(t, sess, 100, 30)
	hs.send(logOpenWithin(hs.root().e, logPod, kube.LogOptions{}, 20*time.Millisecond)())
	v, ok := hs.root().top().(*logView)
	if !ok {
		t.Fatal("the log was not opened")
	}
	time.Sleep(60 * time.Millisecond)
	if v.box.ctx.Err() != nil {
		t.Errorf("the stream's context ended: %v", v.box.ctx.Err())
	}
}

func TestLogCopyIsBounded(t *testing.T) {
	sess := newSession()
	hs := newHarness(t, sess, 100, 30)
	below := &testView{name: "below"}
	hs.send(pushViewMsg{below})
	v, s := logOpenIn(t, hs, sess, logPod, kube.LogOptions{})
	for i := range 50 {
		var lines []kube.LogLine
		for j := 1; j <= 100; j++ {
			lines = append(lines, kube.LogLine{Pod: "web-1", Container: "app", Text: fmt.Sprintf("line %04d %s", i*100+j, strings.Repeat("x", 290))})
		}
		s.Send(lines...)
	}
	hs.await("5000 lines", func() bool { return len(v.lines) == 5000 && strings.HasPrefix(v.lines[4999].text, "line 5000 ") })
	hs.press("c")
	got, ok := logClipboard(below)
	if !ok || len(got) > logCopyMax || len(got) < logCopyMax-301 {
		t.Fatalf("c copied %d bytes", len(got))
	}
	n := strings.Count(got, "\n") + 1
	first := 5000 - n + 1
	if !strings.HasPrefix(got, fmt.Sprintf("line %04d ", first)) || !strings.HasSuffix(got, "\n"+v.lines[4999].text) {
		t.Errorf("the copy is not cut at a line's start, or does not end with the last line: %q…", got[:40])
	}
	hs.wantFrame(fmt.Sprintf("copied the last %d of 5000 lines", n))
}

// logLong returns the line i of n bytes: "line 0001 aaa…".
func logLong(i, n int) kube.LogLine {
	text := fmt.Sprintf("line %04d ", i)
	return kube.LogLine{Pod: "web-1", Container: "app", Text: text + strings.Repeat("a", n-len(text))}
}

// logHeld returns the bytes of text v's buffer holds, and fails the test
// when its array holds a line beyond the buffer.
func logHeld(t *testing.T, v *logView) int {
	t.Helper()
	n := 0
	for _, l := range v.lines {
		n += len(l.text) + len(l.src)
	}
	for i, l := range v.lines[len(v.lines):cap(v.lines)] {
		if l.text != "" {
			t.Errorf("beyond the buffer, at %d, is %q", i, l.text[:min(len(l.text), 20)])
		}
	}
	return n
}

func TestLogBufferHasABudget(t *testing.T) {
	const long = 64 << 10
	const budget = 16 << 20

	// 300 lines of 64 KiB: the oldest go, so that 16 MiB is held at
	// most, and no more go than must.
	sess := newSession()
	hs := newHarness(t, sess, 100, 30)
	v, s := logOpenIn(t, hs, sess, logPod, kube.LogOptions{})
	go func() {
		for i := 1; i <= 300; i++ {
			s.Send(logLong(i, long))
		}
	}()
	hs.await("the 300th line", func() bool {
		return len(v.lines) > 0 && strings.HasPrefix(v.lines[len(v.lines)-1].text, "line 0300 ")
	})
	if held := logHeld(t, v); held > budget {
		t.Errorf("the buffer holds %d bytes in %d lines", held, len(v.lines))
	}
	if n := len(v.lines); n < budget/long-1 {
		t.Errorf("the buffer holds %d lines of 64 KiB", n)
	}
	for i, l := range v.lines {
		if want := fmt.Sprintf("line %04d ", 300-len(v.lines)+1+i); !strings.HasPrefix(l.text, want) {
			t.Fatalf("line %d of the buffer is %q, want %q", i, l.text[:20], want)
		}
	}
	hs.wantFrame("line 0300")

	// 6000 short lines: 5000 are held.
	hs = newHarness(t, sess, 100, 30)
	v, s = logOpenIn(t, hs, sess, logPod, kube.LogOptions{})
	for i := range 60 {
		s.Send(logTexts(i*100+1, 100)...)
	}
	hs.await("the 6000th line", func() bool { return logHas(v, "line 6000") })
	if len(v.lines) != kube.LogBufferLines || v.lines[0].text != "line 1001" {
		t.Errorf("the buffer holds %d lines from %q", len(v.lines), v.lines[0].text)
	}

	// A user who scrolled up keeps their place while the oldest go.
	hs = newHarness(t, sess, 100, 30)
	v, s = logOpenIn(t, hs, sess, logPod, kube.LogOptions{})
	go func() {
		for i := 1; i <= 200; i++ {
			s.Send(logLong(i, long))
		}
	}()
	hs.await("the 200th line", func() bool {
		return len(v.lines) == 200 && strings.HasPrefix(v.lines[199].text, "line 0200 ")
	})
	logBody(hs) // drawn, as the user sees it before the key
	hs.press("up")
	before := logBody(hs)
	if !strings.HasPrefix(before[0], "line 0180 ") {
		t.Fatalf("after up the body starts with %q", before[0][:20])
	}
	go func() {
		for i := 201; i <= 300; i++ {
			s.Send(logLong(i, long))
		}
	}()
	hs.await("the 300th line", func() bool {
		return len(v.lines) > 0 && strings.HasPrefix(v.lines[len(v.lines)-1].text, "line 0300 ")
	})
	if strings.HasPrefix(v.lines[0].text, "line 0001 ") {
		t.Fatal("no line was dropped")
	}
	if held := logHeld(t, v); held > budget {
		t.Errorf("the buffer holds %d bytes", held)
	}
	if after := logBody(hs); strings.Join(after, "\n") != strings.Join(before, "\n") {
		t.Errorf("the view moved while the oldest lines went: it starts with %q", after[0][:20])
	}
	if !strings.Contains(hs.title(), "[autoscroll:off]") {
		t.Errorf("the title is %q", hs.title())
	}
}
