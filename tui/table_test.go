package tui

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/fables-for-robots/kosh/kube"
	"github.com/fables-for-robots/kosh/kube/kubetest"
	"github.com/fables-for-robots/kosh/render"
)

// countingSession is a kubetest.Session that remembers when each watch's
// table was read.
type countingSession struct {
	*kubetest.Session
	mu    sync.Mutex
	reads map[string][]time.Time
}

func newCountingSession() *countingSession {
	return &countingSession{Session: newSession(), reads: map[string][]time.Time{}}
}

func (s *countingSession) Watch(q kube.Query) (kube.Watch, error) {
	w, err := s.Session.Watch(q)
	if err != nil {
		return nil, err
	}
	return &countingWatch{Watch: w, s: s, key: kubetest.Key(q)}, nil
}

// readsOf returns when the table of q was read.
func (s *countingSession) readsOf(q kube.Query) []time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.reads[kubetest.Key(q)])
}

type countingWatch struct {
	kube.Watch
	s   *countingSession
	key string
}

func (w *countingWatch) Table(now time.Time) kube.Table {
	w.s.mu.Lock()
	w.s.reads[w.key] = append(w.s.reads[w.key], time.Now())
	w.s.mu.Unlock()
	return w.Watch.Table(now)
}

// table returns the top view as the table it is.
func (hs *harness) table() *tableView {
	hs.t.Helper()
	tv, ok := hs.root().top().(*tableView)
	if !ok {
		hs.t.Fatalf("the top view is a %T", hs.root().top())
	}
	return tv
}

// order returns the names of names in the order the body shows them.
func (hs *harness) order(names ...string) []string {
	hs.t.Helper()
	var out []string
	for _, l := range hs.lines() {
		if !strings.HasPrefix(l, "│") {
			continue
		}
		for _, f := range strings.Fields(strings.Trim(l, "│")) {
			if slices.Contains(names, f) {
				out = append(out, f)
			}
		}
	}
	return out
}

// header returns the table's header line, plain.
func (hs *harness) tableHeader() string {
	hs.t.Helper()
	for i, l := range hs.lines() {
		if strings.HasPrefix(l, "┌") {
			return words(strings.Trim(hs.lines()[i+1], "│"))
		}
	}
	return ""
}

// drawnRow returns the drawn line of the body whose first word is name,
// or "".
func (hs *harness) drawnRow(name string) string {
	hs.t.Helper()
	for _, l := range strings.Split(hs.drawn(), "\n") {
		plain := ansi.Strip(l)
		if !strings.HasPrefix(plain, "│") {
			continue
		}
		if f := strings.Fields(strings.Trim(plain, "│")); len(f) > 0 && f[0] == name {
			return l
		}
	}
	return ""
}

// drawnAs reports whether the row of name is drawn in style.
func (hs *harness) drawnAs(name string, style interface{ Render(...string) string }) bool {
	hs.t.Helper()
	return strings.Contains(hs.drawnRow(name), sgr(style)+name)
}

// shows reports whether the body has a row of name.
func (hs *harness) shows(name string) bool { return len(hs.order(name)) > 0 }

// cursorRow returns the plain text of the row drawn in the cursor's
// style.
func (hs *harness) cursorRow() string {
	hs.t.Helper()
	for _, l := range strings.Split(hs.drawn(), "\n") {
		if strings.Contains(l, sgr(styleCursor)) {
			return words(strings.Trim(ansi.Strip(l), "│"))
		}
	}
	return ""
}

func TestStartsInThePodsOfDefault(t *testing.T) {
	sess := newSession()
	sess.SetTable(query("pods", "default"), podTable(
		pod{name: "web-7d9f8b6c4-2xkqp", age: 3 * time.Hour},
		pod{name: "web-7d9f8b6c4-9zt4m", age: 3 * time.Hour},
		pod{name: "db-0", status: "Pending", health: render.Pending, age: 2 * time.Minute},
	))
	hs := newHarness(t, sess, 100, 30)
	if hs.title() != "Pods(default)[3]" {
		t.Errorf("the title is %q", hs.title())
	}
	if got := sess.OpenWatches(); !slices.Equal(got, []string{kubetest.Key(query("pods", "default"))}) {
		t.Errorf("the open watches are %q", got)
	}
	hs.golden()
}

func TestEveryLineFits(t *testing.T) {
	long := strings.Repeat("x", 290)
	var pods []pod
	for i := range 5000 {
		pods = append(pods, pod{name: fmt.Sprintf("%s-%04d", long, i), status: long + "名前", health: render.Health(i % 4)})
	}
	sess := newSession()
	sess.SetTable(query("pods", "default"), podTable(pods...))
	for _, size := range [][2]int{{80, 12}, {100, 30}, {250, 70}} {
		hs := newHarness(t, sess, size[0], size[1])
		hs.drawn()
		hs.press("ctrl+w")
		hs.drawn()
		hs.press("G")
		hs.drawn()
		hs.press("pgup", "space", "right", "right", "O")
		hs.drawn()
		hs.press("/")
		hs.typeText("xx")
		hs.drawn()
		hs.press("enter", "ctrl+e")
		hs.drawn()
		if hs.title() != "Pods(default)[5000] </xx>" {
			t.Errorf("the title is %q", hs.title())
		}
	}
}

func TestSort(t *testing.T) {
	sess := newSession()
	sess.SetTable(query("pods", "default"), podTable(
		pod{name: "pod-c", age: time.Hour},
		pod{name: "pod-a", age: 10 * 24 * time.Hour},
		pod{name: "pod-b", age: 9 * 24 * time.Hour},
	))
	// READY sorts otherwise than NAME.
	tb := podTable(
		pod{name: "pod-c", age: time.Hour},
		pod{name: "pod-a", age: 10 * 24 * time.Hour},
		pod{name: "pod-b", age: 9 * 24 * time.Hour},
	)
	for i, ready := range []string{"1/1", "2/2", "0/1"} {
		tb.Rows[i].Cells[2] = ready
	}
	sess.SetTable(query("pods", "default"), tb)
	hs := newHarness(t, sess, 100, 30)
	names := []string{"pod-a", "pod-b", "pod-c"}
	want := func(what string, order ...string) {
		t.Helper()
		if got := hs.order(names...); !slices.Equal(got, order) {
			t.Errorf("%s: the order is %q, want %q", what, got, order)
		}
	}
	want("at first", "pod-a", "pod-b", "pod-c")
	hs.press("N")
	want("by name", "pod-a", "pod-b", "pod-c")
	if !strings.Contains(hs.tableHeader(), "NAME↑") {
		t.Errorf("the header is %q", hs.tableHeader())
	}
	hs.press("N")
	want("by name again", "pod-c", "pod-b", "pod-a")
	if !strings.Contains(hs.tableHeader(), "NAME↓") {
		t.Errorf("the header is %q", hs.tableHeader())
	}
	hs.press("A")
	// By the number: 9d before 10d, which as text sorts first.
	want("by age", "pod-c", "pod-b", "pod-a")
	if !strings.Contains(hs.tableHeader(), "AGE↑") {
		t.Errorf("the header is %q", hs.tableHeader())
	}
	hs.press("A")
	want("by age again", "pod-a", "pod-b", "pod-c")
	hs.press("S")
	if !strings.Contains(hs.tableHeader(), "STATUS↑") {
		t.Errorf("the header is %q", hs.tableHeader())
	}
	hs.press("right", "O")
	want("by READY", "pod-b", "pod-c", "pod-a")
	if !strings.Contains(hs.tableHeader(), "READY↑") {
		t.Errorf("the header is %q", hs.tableHeader())
	}
	// The column cursor is marked in the header.
	if l := hs.drawnLine("READY"); !strings.Contains(l, styleColumnAt.Render("READY")) {
		t.Errorf("the column under the cursor is not marked: %q", l)
	}
	// A table without STATUS does not sort by it.
	sess.SetTable(query("deployments", "default"), simpleTable("x", "y"))
	hs.command("deploy")
	hs.press("S")
	if strings.Contains(hs.tableHeader(), "↑") && !strings.Contains(hs.tableHeader(), "NAME↑") {
		t.Errorf("the header is %q", hs.tableHeader())
	}
}

func TestEventsStartNewestFirst(t *testing.T) {
	sess := newSession()
	tb := kube.Table{
		Columns:    []render.Column{{Name: "NAMESPACE"}, {Name: "LAST SEEN", Numeric: true}, {Name: "TYPE"}, {Name: "REASON"}, {Name: "OBJECT"}, {Name: "MESSAGE"}},
		Namespaced: true,
	}
	for _, e := range []struct {
		name string
		ago  time.Duration
	}{{"ev-old", time.Hour}, {"ev-new", 10 * time.Second}, {"ev-mid", 5 * time.Minute}} {
		tb.Rows = append(tb.Rows, render.Row{
			Namespace: "default", Name: e.name,
			Cells: []string{"default", ageText(e.ago), "Normal", "Pulled", "pod/" + e.name, "pulled"},
			Order: []float64{0, e.ago.Seconds(), 0, 0, 0, 0},
		})
	}
	sess.SetTable(query("events", "default"), tb)
	hs := newHarness(t, sess, 100, 30)
	hs.command("events")
	names := []string{"pod/ev-old", "pod/ev-new", "pod/ev-mid"}
	if got := hs.order(names...); !slices.Equal(got, []string{"pod/ev-new", "pod/ev-mid", "pod/ev-old"}) {
		t.Errorf("the events are in the order %q", got)
	}
	if !strings.Contains(hs.tableHeader(), "LAST SEEN↑") {
		t.Errorf("the header is %q", hs.tableHeader())
	}
}

func TestWideAndFaults(t *testing.T) {
	sess := newSession()
	sess.SetTable(query("pods", "default"), podTable(
		pod{name: "ok-1"}, pod{name: "bad-1", status: "CrashLoopBackOff", health: render.Error}, pod{name: "ok-2"},
	))
	hs := newHarness(t, sess, 100, 30)
	if strings.Contains(hs.tableHeader(), "IP") || strings.Contains(hs.frame(), "10.0.0.1") {
		t.Errorf("a wide column is shown: %q", hs.tableHeader())
	}
	hs.press("ctrl+w")
	if !strings.Contains(hs.tableHeader(), "IP") {
		t.Errorf("ctrl+w does not show the wide columns: %q", hs.tableHeader())
	}
	hs.wantFrame("10.0.0.1")
	hs.press("ctrl+w")
	hs.notFrame("10.0.0.1")
	hs.press("ctrl+z")
	if hs.title() != "Pods(default)[1]" {
		t.Errorf("with the faults only the title is %q", hs.title())
	}
	if got := hs.order("ok-1", "ok-2", "bad-1"); !slices.Equal(got, []string{"bad-1"}) {
		t.Errorf("with the faults only the rows are %q", got)
	}
	hs.press("ctrl+z")
	if hs.title() != "Pods(default)[3]" {
		t.Errorf("with all rows again the title is %q", hs.title())
	}
}

func TestNamespaceColumnIsLeftOut(t *testing.T) {
	sess := newSession()
	sess.SetTable(query("pods", "default"), podsNamed("web-1"))
	all := podTable(pod{ns: "kube-system", name: "dns-1"}, pod{name: "web-1"})
	sess.SetTable(query("pods", ""), all)
	sess.SetTable(query("nodes", ""), clusterTable("node-1"))
	hs := newHarness(t, sess, 100, 30)
	if h := hs.tableHeader(); strings.Contains(h, "NAMESPACE") || !strings.HasPrefix(h, "NAME") {
		t.Errorf("Pods(default) has the header %q", h)
	}
	hs.send(pushViewMsg{newTableView(hs.root().e, query("pods", ""), "")})
	if hs.title() != "Pods(all)[2]" {
		t.Errorf("the title is %q", hs.title())
	}
	if h := hs.tableHeader(); !strings.HasPrefix(h, "NAMESPACE↑ NAME") {
		t.Errorf("Pods(all) has the header %q", h)
	}
	hs.wantFrame("kube-system")
	// Of an object of one namespace, the column is left out too.
	of := kubetest.Ref("deployments", "default", "web")
	sess.SetTable(queryOf(kubetest.Resource("pods"), of), podsNamed("web-1"))
	hs.send(pushViewMsg{newTableView(hs.root().e, queryOf(kubetest.Resource("pods"), of), "")})
	if hs.title() != "Pods(deployments/default/web)[1]" {
		t.Errorf("the title is %q", hs.title())
	}
	if h := hs.tableHeader(); strings.Contains(h, "NAMESPACE") {
		t.Errorf("the pods of a deployment have the header %q", h)
	}
	hs.command("nodes")
	if h := hs.tableHeader(); h != "NAME↑ AGE" {
		t.Errorf("the nodes have the header %q", h)
	}
}

func TestCursorMoves(t *testing.T) {
	sess := newSession()
	var names []string
	for i := range 60 {
		names = append(names, fmt.Sprintf("pod-%02d", i))
	}
	sess.SetTable(query("pods", "default"), podsNamed(names...))
	hs := newHarness(t, sess, 100, 30)
	at := func(key, want string) {
		t.Helper()
		hs.press(key)
		if got := hs.cursorRow(); !strings.HasPrefix(got, want+" ") {
			t.Errorf("after %s the cursor is on %q, want %s", key, got, want)
		}
	}
	if got := hs.cursorRow(); !strings.HasPrefix(got, "pod-00 ") {
		t.Errorf("the cursor starts on %q", got)
	}
	at("down", "pod-01")
	at("j", "pod-02")
	at("k", "pod-01")
	at("up", "pod-00")
	at("up", "pod-00")
	at("G", "pod-59")
	at("down", "pod-59")
	at("g", "pod-00")
	at("end", "pod-59")
	at("home", "pod-00")
	at("pgdown", "pod-19")
	at("ctrl+f", "pod-38")
	at("ctrl+b", "pod-19")
	at("pgup", "pod-00")
	hs.press("G")
	hs.wantFrame("pod-59")
	hs.notFrame("pod-00 ")
}

func TestCursorStaysOnItsObject(t *testing.T) {
	sess := newSession()
	q := query("pods", "default")
	sess.SetTable(q, podsNamed("web-1", "web-2", "web-3"))
	hs := newHarness(t, sess, 100, 30)
	hs.press("down")
	if got := hs.cursorRow(); !strings.HasPrefix(got, "web-2 ") {
		t.Fatalf("the cursor is on %q", got)
	}
	sess.SetTable(q, podsNamed("web-1", "web-2", "web-3", "web-0"))
	hs.awaitFrame("web-0")
	if got := hs.cursorRow(); !strings.HasPrefix(got, "web-2 ") {
		t.Errorf("after a row was sorted in before it the cursor is on %q", got)
	}
	sess.SetTable(q, podsNamed("web-0", "web-1", "web-3"))
	hs.await("web-2 to go", func() bool { return !strings.Contains(hs.plain(), "web-2") })
	if got := hs.cursorRow(); !strings.HasPrefix(got, "web-3 ") {
		t.Errorf("after its row went the cursor is on %q, want the same position", got)
	}
	sess.SetTable(q, podsNamed("web-0"))
	hs.await("web-3 to go", func() bool { return !strings.Contains(hs.plain(), "web-3") })
	if got := hs.cursorRow(); !strings.HasPrefix(got, "web-0 ") {
		t.Errorf("after the rows below went the cursor is on %q", got)
	}
}

func TestSelectedRowHasTheCursor(t *testing.T) {
	sess := newSession()
	sess.SetTable(query("pods", "default"), podsNamed("web-1", "web-2", "web-3"))
	hs := newHarness(t, sess, 100, 30)
	hs.send(pushViewMsg{newTableView(hs.root().e, query("pods", "default"), "web-3")})
	if got := hs.cursorRow(); !strings.HasPrefix(got, "web-3 ") {
		t.Errorf("the cursor is on %q", got)
	}
}

func TestMarks(t *testing.T) {
	sess := newSession()
	q := query("pods", "default")
	sess.SetTable(q, podsNamed("a", "b", "c", "d", "e"))
	hs := newHarness(t, sess, 100, 30)
	marked := func() []string {
		var out []string
		for _, l := range strings.Split(hs.drawn(), "\n") {
			if strings.Contains(l, sgr(styleMark)) && !strings.Contains(l, sgr(styleCursor)) || strings.Contains(l, sgr(styleCursor)) && hs.table().marks[rowKey{"default", strings.Fields(strings.Trim(ansi.Strip(l), "│"))[0]}] {
				out = append(out, strings.Fields(strings.Trim(ansi.Strip(l), "│"))[0])
			}
		}
		return out
	}
	hs.press("space")
	hs.press("down")
	if got := marked(); !slices.Equal(got, []string{"a"}) {
		t.Errorf("space marked %q", got)
	}
	hs.press("down", "down", "ctrl+space", "down")
	if got := marked(); !slices.Equal(got, []string{"a", "b", "c", "d"}) {
		t.Errorf("ctrl+space marked %q", got)
	}
	hs.press("ctrl+\\")
	if got := marked(); len(got) != 0 {
		t.Errorf("ctrl+\\ left %q marked", got)
	}
	hs.press("space", "space", "down")
	if got := marked(); len(got) != 0 {
		t.Errorf("space twice left %q marked", got)
	}
	hs.press("g", "down", "space", "down", "down")
	if got := marked(); !slices.Equal(got, []string{"b"}) {
		t.Errorf("space marked %q", got)
	}
	sess.SetTable(q, podsNamed("a", "c", "d", "e"))
	hs.await("b to go", func() bool { return !hs.shows("b") })
	sess.SetTable(q, podsNamed("a", "b", "c", "d", "e"))
	hs.await("b to come back", func() bool { return hs.shows("b") })
	if got := marked(); len(got) != 0 {
		t.Errorf("a row that went and came back is marked: %q", got)
	}
}

func TestNewAndChangedRowsAreColoured(t *testing.T) {
	sess := newSession()
	q := query("pods", "default")
	sess.SetTable(q, podsNamed("a", "b"))
	hs := newHarness(t, sess, 100, 30)
	hs.press("down") // the cursor on b
	for _, n := range []string{"a", "b"} {
		if hs.drawnAs(n, styleRowNew) || hs.drawnAs(n, styleRowChanged) || !hs.drawnAs(n, styleCursor) && !hs.drawnAs(n, styleOfHealth(render.OK)) {
			t.Errorf("in the first table %s is drawn as %q", n, hs.drawnRow(n))
		}
	}
	next := podTable(pod{name: "a", status: "Terminating"}, pod{name: "b"}, pod{name: "c"})
	sess.SetTable(q, next)
	hs.await("c to come", func() bool { return hs.shows("c") })
	if !hs.drawnAs("c", styleRowNew) {
		t.Errorf("the new row is drawn as %q", hs.drawnRow("c"))
	}
	if !hs.drawnAs("a", styleRowChanged) {
		t.Errorf("the changed row is drawn as %q", hs.drawnRow("a"))
	}
	if !hs.drawnAs("b", styleCursor) {
		t.Errorf("the row under the cursor is drawn as %q", hs.drawnRow("b"))
	}
	hs.clock.advance(time.Second)
	if !hs.drawnAs("c", styleRowNew) {
		t.Errorf("a second later the new row is drawn as %q", hs.drawnRow("c"))
	}
	hs.clock.advance(time.Second)
	for _, n := range []string{"a", "c"} {
		if !hs.drawnAs(n, styleOfHealth(render.OK)) {
			t.Errorf("two seconds later %s is drawn as %q", n, hs.drawnRow(n))
		}
	}
}

func TestHealthColours(t *testing.T) {
	sess := newSession()
	sess.SetTable(query("pods", "default"), podTable(
		pod{name: "a-cursor"},
		pod{name: "b-ok"},
		pod{name: "c-pending", health: render.Pending},
		pod{name: "d-error", health: render.Error},
		pod{name: "e-completed", health: render.Completed},
	))
	hs := newHarness(t, sess, 100, 30)
	for _, tt := range []struct {
		name  string
		style string
	}{
		{"a-cursor", sgr(styleCursor)},
		{"b-ok", sgr(styleOfHealth(render.OK))},
		{"c-pending", sgr(styleOfHealth(render.Pending))},
		{"d-error", sgr(styleOfHealth(render.Error))},
		{"e-completed", sgr(styleOfHealth(render.Completed))},
	} {
		if l := hs.drawnRow(tt.name); !strings.Contains(l, tt.style+tt.name) {
			t.Errorf("%s is drawn as %q, want %q", tt.name, l, tt.style)
		}
	}
	if sgr(styleOfHealth(render.Error)) == sgr(styleOfHealth(render.OK)) {
		t.Error("the styles of the healths are not different")
	}
}

func TestTableErrorAndEmpty(t *testing.T) {
	q := query("pods", "default")
	for _, tt := range []struct {
		name  string
		table kube.Table
		body  string
		title string
	}{
		{"error", kube.Table{Err: errors.New(`pods is forbidden: User "kosh" cannot list`)}, `pods is forbidden: User "kosh" cannot list`, "Pods(default)[0]"},
		{"empty", kube.Table{Columns: podColumns(), Namespaced: true}, "No resources found", "Pods(default)[0]"},
		{"stale and never any rows", kube.Table{Columns: podColumns(), Namespaced: true, Stale: true}, "Waiting for the first list…", "Pods(default)[0] (stale)"},
		{"stale with rows", func() kube.Table { t := podsNamed("web-1"); t.Stale = true; return t }(), "web-1", "Pods(default)[1] (stale)"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			sess := newSession()
			sess.SetTable(q, tt.table)
			hs := newHarness(t, sess, 100, 30)
			hs.wantFrame(tt.body)
			if hs.title() != tt.title {
				t.Errorf("the title is %q, want %q", hs.title(), tt.title)
			}
			if tt.name == "error" {
				if l := hs.drawnLine("forbidden"); !strings.Contains(l, sgr(styleError)) {
					t.Errorf("the error is drawn as %q", l)
				}
			}
		})
	}
	// A table that had rows and then is stale without them is empty,
	// not waiting.
	sess := newSession()
	sess.SetTable(q, podsNamed("web-1"))
	hs := newHarness(t, sess, 100, 30)
	sess.SetTable(q, kube.Table{Columns: podColumns(), Namespaced: true, Stale: true})
	hs.awaitFrame("No resources found")
}

func TestWatchErrorIsShown(t *testing.T) {
	sess := newSession()
	sess.Errs["Watch"] = errors.New("watch refused")
	hs := newHarness(t, sess, 100, 30)
	if l := hs.drawnLine("watch refused"); !strings.HasPrefix(ansi.Strip(l), "│") || !strings.Contains(l, sgr(styleError)) {
		t.Errorf("the error of Watch is drawn as %q", l)
	}
}

// Every view draws exactly the lines it is given, none wider than its
// width, whatever it shows: rows, none, an error, the help, the names.
func TestViewsAreExactlyTheirSize(t *testing.T) {
	sess := newSession()
	sess.SetTable(query("pods", "default"), podsNamed("web-1", "web-2"))
	sess.SetTable(query("pods", "empty"), podTable())
	sess.SetTable(query("pods", "broken"), kube.Table{Err: errors.New("forbidden\nand more")})
	hs := newHarness(t, sess, 100, 30)
	views := map[string]view{
		"rows":  newTableView(hs.root().e, query("pods", "default"), ""),
		"none":  newTableView(hs.root().e, query("pods", "empty"), ""),
		"error": newTableView(hs.root().e, query("pods", "broken"), ""),
		"names": newNamesView(hs.root().e),
	}
	unread := newTableView(hs.root().e, query("pods", "never"), "")
	for _, v := range views {
		hs.send(pushViewMsg{v})
	}
	views["unread"] = unread
	hs.press("?")
	views["help"] = hs.root().top()
	sess.Errs["Watch"] = errors.New("watch refused")
	broken := newTableView(hs.root().e, query("pods", "x"), "")
	hs.send(pushViewMsg{broken})
	views["watch error"] = broken
	for name, v := range views {
		for _, size := range [][2]int{{1, 1}, {10, 1}, {10, 2}, {80, 1}, {80, 3}, {98, 20}} {
			out := v.View(size[0], size[1])
			lines := strings.Split(out, "\n")
			if len(lines) != size[1] {
				t.Errorf("%s at %dx%d draws %d lines", name, size[0], size[1], len(lines))
			}
			for i, l := range lines {
				if w := ansi.StringWidth(l); w > size[0] {
					t.Errorf("%s at %dx%d: line %d is %d wide", name, size[0], size[1], i, w)
				}
			}
		}
	}
}

func TestCoveredViewsStillGetTheirMessages(t *testing.T) {
	sess := newSession()
	q := query("pods", "default")
	sess.SetTable(q, podsNamed("web-1"))
	hs := newHarness(t, sess, 100, 30)
	tv := hs.table()
	hs.send(pushViewMsg{&testView{name: "cover"}})
	// A covered table does not read (TestBuriedTableDoesNotRead), but
	// it takes the messages that are for it.
	hs.send(useNamespaceMsg{"kube-system"})
	if tv.ns.current != "kube-system" {
		t.Errorf("the covered table has the namespace %q", tv.ns.current)
	}
}

func TestTableReadsAtIntervals(t *testing.T) {
	sess := newCountingSession()
	q := query("pods", "default")
	sess.SetTable(q, podsNamed("web-1"))
	hs := newHarness(t, sess, 100, 30)
	// At least every readMax, without a change.
	v := newTableView(hs.root().e, query("pods", "kube-system"), "").(*tableView)
	v.readMin, v.readMax = time.Millisecond, 20*time.Millisecond
	hs.send(pushViewMsg{v})
	hs.await("three reads without a change", func() bool { return len(sess.readsOf(query("pods", "kube-system"))) >= 3 })
	hs.send(popViewMsg{})

	// Not sooner than readMin after the last read, however often it
	// changes.
	w := newTableView(hs.root().e, query("pods", "monitoring"), "").(*tableView)
	w.readMin, w.readMax = 50*time.Millisecond, time.Hour
	qm := query("pods", "monitoring")
	hs.send(pushViewMsg{w})
	for i := range 3 {
		n := len(sess.readsOf(qm))
		for j := range 5 {
			sess.SetTable(qm, podsNamed(fmt.Sprintf("p-%d-%d", i, j)))
		}
		hs.await("a read", func() bool { return len(sess.readsOf(qm)) > n })
	}
	reads := sess.readsOf(qm)
	for i := 1; i < len(reads); i++ {
		if d := reads[i].Sub(reads[i-1]); d < 45*time.Millisecond {
			t.Errorf("read %d came %v after the one before", i, d)
		}
	}
	// ctrl+r reads now.
	n := len(sess.readsOf(qm))
	hs.press("ctrl+r")
	if got := len(sess.readsOf(qm)); got != n+1 {
		t.Errorf("ctrl+r read %d times", got-n)
	}
}

func TestFilter(t *testing.T) {
	sess := newSession()
	sess.SetTable(query("pods", "default"), podTable(
		pod{name: "web-1", labels: map[string]string{"app": "web"}},
		pod{name: "web-2", labels: map[string]string{"app": "web"}},
		pod{name: "db-1", labels: map[string]string{"app": "db"}},
		pod{name: "odd", status: "Wait[1]", labels: map[string]string{"app": "odd"}},
	))
	hs := newHarness(t, sess, 100, 30)
	all := []string{"web-1", "web-2", "db-1", "odd"}
	for _, tt := range []struct {
		filter string
		want   []string
		title  string
	}{
		{"web", []string{"web-1", "web-2"}, "Pods(default)[2] </web>"},
		{"WEB", []string{"web-1", "web-2"}, "Pods(default)[2] </WEB>"},
		{"!web", []string{"db-1", "odd"}, "Pods(default)[2] </!web>"},
		{"-l app=web", []string{"web-1", "web-2"}, "Pods(default)[2] </-l app=web>"},
		{"-l app!=web", []string{"db-1", "odd"}, "Pods(default)[2] </-l app!=web>"},
		{"[", []string{"odd"}, "Pods(default)[1] </[>"},
		{"w.b", []string{"web-1", "web-2"}, "Pods(default)[2] </w.b>"},
		{"-l ===", nil, "Pods(default)[0] </-l ===>"},
	} {
		hs.press("/")
		hs.typeText(tt.filter)
		hs.press("enter")
		if got := hs.order(all...); !slices.Equal(got, tt.want) {
			t.Errorf("/%s keeps %q, want %q", tt.filter, got, tt.want)
		}
		if hs.title() != tt.title {
			t.Errorf("/%s: the title is %q, want %q", tt.filter, hs.title(), tt.title)
		}
	}
	hs.wantFrame("===")
	if l := hs.lines()[len(hs.lines())-1]; !strings.Contains(l, "-l ===") {
		t.Errorf("the selector that does not parse is not in the flash: %q", l)
	}
}

func TestEscClearsTheFilterThenPops(t *testing.T) {
	sess := newSession()
	sess.SetTable(query("pods", "default"), podsNamed("web-1", "db-1"))
	hs := newHarness(t, sess, 100, 30)
	hs.send(pushViewMsg{newTableView(hs.root().e, query("pods", "default"), "")})
	hs.press("/")
	hs.typeText("web")
	hs.press("enter")
	if hs.title() != "Pods(default)[1] </web>" {
		t.Fatalf("the title is %q", hs.title())
	}
	hs.press("esc")
	if hs.title() != "Pods(default)[2]" || len(hs.root().stack) != 2 {
		t.Errorf("after esc the title is %q, the stack %d views", hs.title(), len(hs.root().stack))
	}
	hs.press("esc")
	if len(hs.root().stack) != 1 {
		t.Errorf("after esc again the stack has %d views", len(hs.root().stack))
	}
}

func TestCellsFromTheClusterAreCleaned(t *testing.T) {
	sess := newSession()
	evil := "\x1b]0;x\x07\x1b[2J"
	pods := podTable(pod{name: "web-1", status: evil + "Running"})
	pods.Columns[len(pods.Columns)-1].Name = "AGE" + evil + "\nX"
	sess.SetTable(query("pods", "default"), pods)
	sess.SetTable(query("deployments", "default"), kube.Table{Err: errors.New("denied " + evil)})
	hs := newHarness(t, sess, 100, 30)
	if h := hs.tableHeader(); !strings.HasSuffix(h, "AGE X") {
		t.Errorf("the header is %q, want the column name cleaned and on one line", h)
	}
	for _, f := range []string{hs.drawn(), func() string { hs.command("deploy"); return hs.drawn() }()} {
		for _, bad := range []string{"\x1b]0;", "\x1b[2J", "\x07"} {
			if strings.Contains(f, bad) {
				t.Errorf("the frame holds %q", bad)
			}
		}
	}
	hs.wantFrame("denied")
}

// A pods table with the metric columns, as kube serves it while the
// cluster has metrics-server, sorts by CPU through Sort Column: by the
// number, with n/a as 0.
func TestSortByCPU(t *testing.T) {
	r, _ := render.For(render.Pods)
	m := r.(render.Metered)
	tb := kube.Table{Columns: r.Columns(), Namespaced: true}
	for _, p := range []struct {
		name string
		cpu  string // empty: the reading did not cover the pod
	}{{"pod-a", "250m"}, {"pod-b", ""}, {"pod-c", "9m"}} {
		obj := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: p.name, CreationTimestamp: metav1.NewTime(time.Now().Add(-time.Hour))},
			Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "c1", Image: "busybox"}}},
			Status:     corev1.PodStatus{Phase: corev1.PodRunning},
		}
		row, err := r.Row(obj, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if p.cpu != "" {
			q := resource.MustParse(p.cpu)
			m.Meter(&row, obj, map[string]render.Usage{"c1": {CPU: q.MilliValue(), Mem: 1 << 20}})
		}
		tb.Rows = append(tb.Rows, row)
	}
	sess := newSession()
	sess.SetTable(query("pods", "default"), tb)
	hs := newHarness(t, sess, 200, 30)
	hs.awaitFrame("pod-c")
	for range len(tb.Columns) {
		v := hs.table()
		if v.data.cols[v.vis[v.colAt]].Name == "CPU" {
			break
		}
		hs.press("right")
	}
	hs.press("O")
	if !strings.Contains(hs.tableHeader(), "CPU↑") {
		t.Fatalf("the header is %q", hs.tableHeader())
	}
	// As text the order would be 250, 9, n/a.
	if got, want := hs.order("pod-a", "pod-b", "pod-c"), []string{"pod-b", "pod-c", "pod-a"}; !slices.Equal(got, want) {
		t.Errorf("by CPU: the order is %q, want %q", got, want)
	}
	hs.press("O")
	if got, want := hs.order("pod-a", "pod-b", "pod-c"), []string{"pod-a", "pod-c", "pod-b"}; !slices.Equal(got, want) {
		t.Errorf("by CPU again: the order is %q, want %q", got, want)
	}
}
