package tui

import (
	"strings"
	"testing"
)

func TestHelp(t *testing.T) {
	sess := newSession()
	sess.SetTable(query("pods", "default"), podsNamed("web-1", "web-2"))
	hs := newHarness(t, sess, 100, 30)
	hs.press("?")
	if hs.title() != "Help" {
		t.Errorf("the title is %q", hs.title())
	}
	hs.golden()
	for _, h := range append(hs.table2(0).Hints(), rootHints()...) {
		if !strings.Contains(hs.frame(), "<"+h.Key+">") || !strings.Contains(hs.frame(), h.Text) {
			t.Errorf("the help does not show %s %s", h.Key, h.Text)
		}
	}
	for _, key := range []string{"esc", "q", "?"} {
		hs.press(key)
		if hs.title() != "Pods(default)[2]" {
			t.Errorf("%s does not return from the help: the title is %q", key, hs.title())
		}
		hs.press("?")
	}
	// The commands open it too.
	for _, cmd := range []string{"h", "help", "?"} {
		hs = newHarness(t, sess, 100, 30)
		hs.command(cmd)
		if hs.title() != "Help" {
			t.Errorf(":%s opens %q", cmd, hs.title())
		}
	}
	// A view's hints are in its help.
	hs = newHarness(t, sess, 100, 30)
	hs.send(pushViewMsg{&testView{name: "v", hints: []hint{{"ctrl-d", "Delete Everything"}}}})
	hs.press("?")
	hs.wantFrame("<ctrl-d>", "Delete Everything", "<ctrl-a>", "Aliases")
}

// table2 returns the view at i of the stack as the table it is.
func (hs *harness) table2(i int) *tableView {
	hs.t.Helper()
	tv, ok := hs.root().stack[i].(*tableView)
	if !ok {
		hs.t.Fatalf("view %d is a %T", i, hs.root().stack[i])
	}
	return tv
}
