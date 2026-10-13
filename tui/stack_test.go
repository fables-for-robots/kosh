package tui

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// pumpFor takes the messages of waiting commands for d, as await does.
// A message of its own every few milliseconds keeps it from waiting on
// views that have stopped sending.
func (hs *harness) pumpFor(d time.Duration) {
	hs.t.Helper()
	end := time.Now().Add(d)
	stop := make(chan struct{})
	go func() {
		tick := time.NewTicker(2 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-hs.done:
				return
			case <-tick.C:
			}
			select {
			case hs.async <- testMsg{}:
			case <-stop:
				return
			case <-hs.done:
				return
			}
		}
	}()
	hs.await(fmt.Sprintf("%v to pass", d), func() bool { return time.Now().After(end) })
	close(stop)
}

func TestStackHasADepth(t *testing.T) {
	sess := newSession()
	// The start fails: the stack is empty, and every view is a push.
	sess.Errs = map[string]error{"Resolve": errors.New("no pods today")}
	hs := newHarness(t, sess, 100, 30)
	if n := len(hs.root().stack); n != 0 {
		t.Fatalf("the stack starts with %d views", n)
	}
	for i := range 31 {
		hs.send(pushViewMsg{&testView{name: fmt.Sprint("v", i)}})
	}
	top := newTableView(hs.root().e, query("pods", "default"), "").(*tableView)
	hs.send(pushViewMsg{top})
	if n := len(hs.root().stack); n != 32 || hs.root().top() != view(top) {
		t.Fatalf("after 32 pushes the stack has %d views, the top is %T", n, hs.root().top())
	}

	// The 33rd is refused, and closed.
	extra := &testView{name: "extra"}
	hs.send(pushViewMsg{extra})
	if n := len(hs.root().stack); n != 32 {
		t.Errorf("after the 33rd push the stack has %d views", n)
	}
	if hs.root().top() != view(top) {
		t.Errorf("after the 33rd push the top is %T", hs.root().top())
	}
	if extra.closed != 1 || extra.inits != 0 {
		t.Errorf("the refused view was closed %d times and started %d times", extra.closed, extra.inits)
	}
	if l := hs.lines()[len(hs.lines())-1]; !strings.Contains(l, "too many views open: go back with esc") {
		t.Errorf("the flash says %q", l)
	}
	if hs.title() != "Pods(default)[0]" {
		t.Errorf("the title is %q", hs.title())
	}

	// esc goes back, and a push is accepted again.
	hs.press("esc")
	if n := len(hs.root().stack); n != 31 {
		t.Fatalf("after esc the stack has %d views", n)
	}
	again := &testView{name: "again"}
	hs.send(pushViewMsg{again})
	if n := len(hs.root().stack); n != 32 || hs.root().top() != view(again) || again.closed != 0 || again.inits != 1 {
		t.Errorf("after esc a push leaves %d views, the top %T, the view closed %d times and started %d times",
			n, hs.root().top(), again.closed, again.inits)
	}
}

func TestBuriedTableDoesNotRead(t *testing.T) {
	sess := newCountingSession()
	q := query("pods", "kube-system")
	sess.SetTable(q, podsNamed("web-1"))
	hs := newHarness(t, sess, 100, 30)
	v := newTableView(hs.root().e, q, "").(*tableView)
	v.readMin, v.readMax = time.Millisecond, 20*time.Millisecond
	hs.send(pushViewMsg{v})
	hs.await("the table to read every 20ms", func() bool { return len(sess.readsOf(q)) >= 3 })

	// Covered, it reads no more, however long and whatever changes. A
	// reading under way when it was covered may still end.
	hs.send(pushViewMsg{&testView{name: "cover"}})
	hs.pumpFor(30 * time.Millisecond)
	n := len(sess.readsOf(q))
	for i := range 5 {
		sess.SetTable(q, podsNamed("web-1", fmt.Sprint("new-", i)))
		hs.pumpFor(40 * time.Millisecond)
	}
	if got := len(sess.readsOf(q)) - n; got != 0 {
		t.Errorf("covered for 10 periods, the table read %d times", got)
	}

	// On top again, it reads at once, and then every period again.
	hs.send(popViewMsg{})
	if got := len(sess.readsOf(q)) - n; got != 1 {
		t.Errorf("on top again the table read %d times at once", got)
	}
	if !hs.shows("new-4") {
		t.Errorf("on top again the table does not show the newest rows:\n%s", hs.frame())
	}
	hs.await("the table to read every 20ms again", func() bool { return len(sess.readsOf(q)) >= n+4 })
}
