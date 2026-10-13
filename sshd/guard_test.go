package sshd

import (
	"log/slog"
	"strings"
	"testing"
)

// crashes checks that tm, the session of the key name, ends with
// MsgCrashed and status 1, and that the log has the panic of where with
// the key's name and the stack.
func crashes(t *testing.T, h *harness, tm *term, name, where string) {
	t.Helper()
	if st := tm.exit(); st != 1 {
		t.Errorf("exit status %d, want 1", st)
	}
	if out := tm.stdout.String(); !strings.HasSuffix(out, MsgCrashed+"\r\n") {
		t.Errorf("the session did not end with %q", MsgCrashed)
	}
	rec := h.log.waitFor(t, "of the panic in "+where, func(r logRecord) bool {
		return r.Level == slog.LevelError && r.Attrs["key"] == name
	})
	if !strings.Contains(rec.Attrs["stack"], "sshd.testModel."+where) {
		t.Errorf("the panic is logged without the stack of %s: %v", where, rec)
	}
	if !strings.Contains(rec.Attrs["panic"], "panic in "+where) {
		t.Errorf("the panic is logged without its value: %v", rec)
	}
}

// stillAnswers checks that tm's program has not ended and quits on q.
func stillAnswers(t *testing.T, tm *term) {
	t.Helper()
	if tm.ended() {
		t.Fatal("the other session has ended")
	}
	tm.press("q")
	if st := tm.exit(); st != 0 {
		t.Errorf("the other session: exit status %d after q, want 0", st)
	}
}

func TestPanicInUpdateEndsOnlyThatSession(t *testing.T) {
	h := newHarness(t)
	alice, bob := h.key("alice", false), h.key("bob", false)
	ta, tb := openTerm(t, h.dial(alice, "kosh")), openTerm(t, h.dial(bob, "kosh"))
	ta.waitFor(testWord)
	tb.waitFor(testWord)
	ta.press("p")
	crashes(t, h, ta, "alice", "Update")
	stillAnswers(t, tb)
}

func TestPanicInViewAndInit(t *testing.T) {
	t.Run("View", func(t *testing.T) {
		h := newHarness(t)
		alice, bob := h.key("alice", false), h.key("bob", false)
		ta, tb := openTerm(t, h.dial(alice, "kosh")), openTerm(t, h.dial(bob, "kosh"))
		ta.waitFor(testWord)
		tb.waitFor(testWord)
		ta.press("v")
		crashes(t, h, ta, "alice", "View")
		stillAnswers(t, tb)
	})
	t.Run("Init", func(t *testing.T) {
		h := newHarness(t)
		bob := h.key("bob", false)
		tb := openTerm(t, h.dial(bob, "kosh"))
		tb.waitFor(testWord)
		ta := openTerm(t, h.dial(h.key(panicInitName, false), "kosh"))
		crashes(t, h, ta, panicInitName, "Init")
		stillAnswers(t, tb)
	})
}

// A panic in a command's goroutine is Bubble Tea's to catch: the program
// ends with tea.ErrProgramPanic.
func TestPanicInACommand(t *testing.T) {
	h := newHarness(t)
	alice, bob := h.key("alice", false), h.key("bob", false)
	ta, tb := openTerm(t, h.dial(alice, "kosh")), openTerm(t, h.dial(bob, "kosh"))
	ta.waitFor(testWord)
	tb.waitFor(testWord)
	ta.press("c")
	if st := ta.exit(); st != 1 {
		t.Errorf("exit status %d, want 1", st)
	}
	if out := ta.stdout.String(); !strings.HasSuffix(out, MsgCrashed+"\r\n") {
		t.Errorf("the session did not end with %q", MsgCrashed)
	}
	h.log.waitFor(t, "of the panic in a command", func(r logRecord) bool {
		return r.Level == slog.LevelError && r.Attrs["key"] == "alice" && strings.Contains(r.Attrs["err"], "panic")
	})
	stillAnswers(t, tb)
}
