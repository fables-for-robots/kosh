package tui

import (
	"slices"
	"strings"
	"testing"
)

func TestNames(t *testing.T) {
	sess := newSession()
	hs := newHarness(t, sess, 100, 30)
	hs.press("ctrl+a")
	if !strings.HasPrefix(hs.title(), "Aliases[27]") {
		t.Errorf("the title is %q", hs.title())
	}
	hs.golden()
	hs.press("/")
	hs.typeText("deploy")
	hs.press("enter")
	if hs.title() != "Aliases[1] </deploy>" {
		t.Errorf("the title is %q", hs.title())
	}
	if got := hs.order("deployments", "pods"); !slices.Equal(got, []string{"deployments"}) {
		t.Errorf("the names are %q", got)
	}
	hs.press("enter")
	if !strings.HasPrefix(hs.title(), "Deployments(default)[") || len(hs.root().stack) != 1 {
		t.Errorf("enter opened %q, the stack has %d views", hs.title(), len(hs.root().stack))
	}
	if got := hs.root().hist.entries; got[len(got)-1] != "deployments" {
		t.Errorf("the history is %q", got)
	}
	// It moves like a table, and esc goes back.
	hs.press("ctrl+a", "G")
	if got := hs.cursorRow(); !strings.HasPrefix(got, "widgets ") {
		t.Errorf("G put the cursor on %q", got)
	}
	hs.press("esc")
	if !strings.HasPrefix(hs.title(), "Deployments(default)[") {
		t.Errorf("esc returned to %q", hs.title())
	}
	// The commands open it too.
	for _, cmd := range []string{"alias", "a"} {
		hs = newHarness(t, sess, 100, 30)
		hs.command(cmd)
		if !strings.HasPrefix(hs.title(), "Aliases[") {
			t.Errorf(":%s opens %q", cmd, hs.title())
		}
	}
}
