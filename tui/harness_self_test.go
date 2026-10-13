package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

// fakeTB is a testing.TB whose Fatalf ends what the harness does, as a
// real test's does, so that a test of the harness can see it fail.
type fakeTB struct {
	testing.TB
	fatal  string
	errors []string
}

// fakeFatal is what fakeTB's Fatalf panics with.
type fakeFatal struct{}

func (f *fakeTB) Helper() {}
func (f *fakeTB) Fatalf(format string, args ...any) {
	f.fatal = fmt.Sprintf(format, args...)
	panic(fakeFatal{})
}
func (f *fakeTB) Errorf(format string, args ...any) {
	f.errors = append(f.errors, fmt.Sprintf(format, args...))
}

// fails runs f, and returns what made the fake test fail, or "".
func (f *fakeTB) fails(run func()) (fatal string) {
	defer func() {
		if r := recover(); r != nil {
			if _, ok := r.(fakeFatal); !ok {
				panic(r)
			}
		}
		fatal = f.fatal
	}()
	run()
	return ""
}

// scriptModel is a model whose Update runs a function of the test.
type scriptModel struct {
	update func(tea.Msg) tea.Cmd
}

func (m scriptModel) Init() tea.Cmd                           { return nil }
func (m scriptModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) { return m, m.update(msg) }
func (m scriptModel) View() tea.View                          { return tea.NewView("") }

// fakeHarness returns a harness of the model, on a fake test.
func fakeHarness(t *testing.T, update func(tea.Msg) tea.Cmd) (*harness, *fakeTB) {
	ft := &fakeTB{TB: t}
	hs := &harness{t: ft, m: scriptModel{update}, async: make(chan tea.Msg, 64), done: make(chan struct{}), settleFor: deadline}
	t.Cleanup(func() { close(hs.done) })
	return hs, ft
}

type rearmMsg struct{}

// A command that re-arms itself without waitCmd fails the test, within
// the harness's bound, and the failure names the rule.
func TestHarnessStopsACommandThatRearms(t *testing.T) {
	rearm := func() tea.Msg { return rearmMsg{} }
	hs, ft := fakeHarness(t, func(tea.Msg) tea.Cmd { return rearm })
	start := time.Now()
	got := ft.fails(func() { hs.send(rearmMsg{}) })
	if !strings.Contains(got, "waitCmd") || !strings.Contains(got, "tea.Tick") {
		t.Errorf("the harness failed with %q, want the rule about waitCmd", got)
	}
	if d := time.Since(start); d > deadline {
		t.Errorf("it took %v to stop", d)
	}

	// One that re-arms slowly, as a tea.Tick does, is stopped by the
	// time.
	slow := func() tea.Msg { time.Sleep(5 * time.Millisecond); return rearmMsg{} }
	hs, ft = fakeHarness(t, func(tea.Msg) tea.Cmd { return slow })
	hs.settleFor = 100 * time.Millisecond
	start = time.Now()
	got = ft.fails(func() { hs.send(rearmMsg{}) })
	if !strings.Contains(got, "waitCmd") {
		t.Errorf("the harness failed with %q, want the rule about waitCmd", got)
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("it took %v to stop", d)
	}
}

type seqMsg string

// A tea.Sequence runs in order: each command's message is delivered
// before the next command runs.
func TestHarnessRunsASequenceInOrder(t *testing.T) {
	var log []string
	step := func(name string) tea.Cmd {
		return func() tea.Msg { log = append(log, "run "+name); return seqMsg(name) }
	}
	hs, ft := fakeHarness(t, func(msg tea.Msg) tea.Cmd {
		switch msg := msg.(type) {
		case seqMsg:
			log = append(log, "got "+string(msg))
		case testMsg:
			return tea.Sequence(step("a"), step("b"), step("c"))
		}
		return nil
	})
	if f := ft.fails(func() { hs.send(testMsg{}) }); f != "" {
		t.Fatalf("the harness failed: %s", f)
	}
	if got := strings.Join(log, ", "); got != "run a, got a, run b, got b, run c, got c" {
		t.Errorf("the sequence ran as %s", got)
	}

	// A waitCmd in a sequence fails the test at once: the harness cannot
	// wait in the middle of one.
	hs, ft = fakeHarness(t, func(msg tea.Msg) tea.Cmd {
		if _, ok := msg.(testMsg); ok {
			return tea.Sequence(step("a"), waitCmd(func() tea.Msg { return nil }))
		}
		return nil
	})
	if got := ft.fails(func() { hs.send(testMsg{}) }); !strings.Contains(got, "tea.Sequence") {
		t.Errorf("a waitCmd in a sequence failed with %q", got)
	}
}
