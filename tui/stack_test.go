package tui

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fables-for-robots/kosh/keys"
	"github.com/fables-for-robots/kosh/kube"
	"github.com/fables-for-robots/kosh/kube/kubetest"
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

// A view that holds something when it is pushed gives it back when the
// push is refused: the editor's hold on the action keys.
func TestRefusedEditorGivesTheKeysBack(t *testing.T) {
	sess := newSession()
	hs := editorStart(t, sess, false)
	for i := range 29 {
		hs.send(pushViewMsg{&testView{name: fmt.Sprint("v", i)}})
	}
	// Two tables on top: after esc, the one below has the keys.
	hs.send(pushViewMsg{newTableView(hs.root().e, query("configmaps", "default"), "")})
	hs.send(pushViewMsg{newTableView(hs.root().e, query("configmaps", "default"), "")})
	if n := len(hs.root().stack); n != 32 || !hs.shows("cfg") {
		t.Fatalf("the stack has %d views, the frame:\n%s", n, hs.frame())
	}
	hs.press("e")
	if _, ok := hs.root().top().(*tableView); !ok || len(hs.root().stack) != 32 {
		t.Fatalf("e on the 32nd view leaves %d views, the top a %T", len(hs.root().stack), hs.root().top())
	}
	if l := hs.lines()[len(hs.lines())-1]; !strings.Contains(l, "too many views open: go back with esc") {
		t.Errorf("the flash says %q", l)
	}
	hs.press("esc")
	hs.press("e")
	if _, ok := hs.root().top().(*editorView); !ok {
		t.Errorf("after esc e opens no editor: the top is a %T, the flash %q", hs.root().top(), hs.lines()[len(hs.lines())-1])
	}
	hs.press("esc")
	hs.press("ctrl+d")
	if hs.root().dlg == nil {
		t.Errorf("ctrl+d opens no dialog: the flash %q", hs.lines()[len(hs.lines())-1])
	}
}

// gateSession is a kubetest.Session whose watch of one query counts its
// readings and, while a gate is set, waits at it in Table.
type gateSession struct {
	*kubetest.Session
	key string

	mu      sync.Mutex
	reads   int
	gate    chan struct{} // Table waits for it to close
	entered chan struct{} // Table says it is waiting
}

func (s *gateSession) Watch(q kube.Query) (kube.Watch, error) {
	w, err := s.Session.Watch(q)
	if err != nil || kubetest.Key(q) != s.key {
		return w, err
	}
	return &gateWatch{Watch: w, s: s}, nil
}

func (s *gateSession) readCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reads
}

type gateWatch struct {
	kube.Watch
	s *gateSession
}

func (w *gateWatch) Table(now time.Time) kube.Table {
	w.s.mu.Lock()
	w.s.reads++
	gate, entered := w.s.gate, w.s.entered
	w.s.gate, w.s.entered = nil, nil
	w.s.mu.Unlock()
	if gate != nil {
		close(entered)
		<-gate
	}
	return w.Watch.Table(now)
}

// A reading under way when the table was buried is taken when it comes,
// and the table waits for no other.
func TestBuriedTableTakesTheReadingUnderWay(t *testing.T) {
	q := query("pods", "kube-system")
	sess := &gateSession{Session: newSession(), key: kubetest.Key(q)}
	sess.SetTable(q, podsNamed("web-1"))
	hs := newHarness(t, sess, 100, 30)
	v := newTableView(hs.root().e, q, "").(*tableView)
	v.readMin, v.readMax = time.Millisecond, time.Hour
	hs.send(pushViewMsg{v})
	if !hs.shows("web-1") {
		t.Fatalf("the table does not show web-1:\n%s", hs.frame())
	}

	gate, entered := make(chan struct{}), make(chan struct{})
	sess.mu.Lock()
	sess.gate, sess.entered = gate, entered
	sess.mu.Unlock()
	sess.SetTable(q, podsNamed("web-1", "web-2"))
	select {
	case <-entered:
	case <-time.After(deadline):
		t.Fatal("the table did not read the change")
	}
	hs.send(pushViewMsg{&testView{name: "cover"}})
	close(gate)
	hs.await("the reading under way to be taken", func() bool { return len(v.shown) == 2 })

	n := sess.readCount()
	sess.SetTable(q, podsNamed("web-1", "web-2", "web-3"))
	hs.pumpFor(50 * time.Millisecond)
	if got := sess.readCount() - n; got != 0 {
		t.Errorf("after the reading under way the buried table read %d times", got)
	}
	hs.send(popViewMsg{})
	if !hs.shows("web-3") {
		t.Errorf("on top again the table does not show web-3:\n%s", hs.frame())
	}
}

// A dialog is not a push: the table under it goes on reading.
func TestTableUnderADialogReads(t *testing.T) {
	sess := newCountingSession()
	q := query("pods", "kube-system")
	sess.SetTable(q, podsNamed("web-1"))
	hs := newHarness(t, sess, 100, 30)
	v := newTableView(hs.root().e, q, "").(*tableView)
	v.readMin, v.readMax = time.Millisecond, 20*time.Millisecond
	hs.send(pushViewMsg{v})
	hs.send(openDialogMsg{&testDialog{}})
	n := len(sess.readsOf(q))
	hs.await("the table under the dialog to read on", func() bool { return len(sess.readsOf(q)) >= n+3 })
}

// A text view that reads itself again rests while it is buried.
func TestBuriedTextDoesNotRead(t *testing.T) {
	sess := textPodsAndSecrets(keys.Grant{Name: "alice@laptop"})
	sess.YAMLs[textPod.String()] = "first"
	hs := newHarness(t, sess, 100, 30)
	hs.press("y")
	v := textTop(hs)
	v.every = 5 * time.Millisecond
	hs.press("r")
	hs.await("the YAML to be read every 5ms", func() bool { return len(sess.Called("YAML")) >= 4 })

	hs.send(pushViewMsg{&testView{name: "cover"}})
	textPause(hs, 20*time.Millisecond)
	n := len(sess.Called("YAML"))
	sess.YAMLs[textPod.String()] = "second"
	textPause(hs, 100*time.Millisecond)
	if got := len(sess.Called("YAML")) - n; got != 0 {
		t.Errorf("covered for 20 periods, the YAML was read %d times", got)
	}

	hs.send(popViewMsg{})
	if got := len(sess.Called("YAML")) - n; got != 1 {
		t.Errorf("on top again the YAML was read %d times at once", got)
	}
	hs.wantFrame("second")
	hs.await("the YAML to be read every 5ms again", func() bool { return len(sess.Called("YAML")) >= n+4 })
	if got := hs.title(); !strings.HasSuffix(got, "(auto)") {
		t.Errorf("the title is %q", got)
	}
}
