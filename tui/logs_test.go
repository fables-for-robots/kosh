package tui

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

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
