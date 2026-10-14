package tui

// The harness drives a model as Bubble Tea would, in the test's
// goroutine: it feeds it messages, runs the commands that come back one
// after the other and feeds their messages in turn, until nothing is
// pending. A command that waits (one made with waitCmd: the wait for a
// watch's Changed, the flash's timer) is left running in a goroutine of
// its own, and its message is taken only when the test awaits it, with a
// deadline. Everything else the harness does is in the order the test
// does it, so that a frame is the same at every run.
//
// A command that waits, or that re-arms itself (tea.Tick, tea.Every),
// must be made with waitCmd. One that is not would keep a settle going
// for ever; settle fails the test instead, after maxSettle commands or
// settleFor, whichever comes first, naming that rule.
//
// A tea.Sequence runs in order, as Bubble Tea runs it: each command runs
// and its message is delivered before the next one starts. A waitCmd in
// a Sequence fails the test at once: the harness does not wait in the
// middle of a sequence.

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/fables-for-robots/kosh/keys"
	"github.com/fables-for-robots/kosh/kube"
	"github.com/fables-for-robots/kosh/kube/kubetest"
	"github.com/fables-for-robots/kosh/render"
)

var update = flag.Bool("update", false, "rewrite the golden frames in testdata")

// deadline is how long the harness waits for a command, for a message a
// test awaits, and for a settle to end.
const deadline = 10 * time.Second

// maxSettle is how many commands one settle runs at most.
const maxSettle = 10000

// ruleOfWaiting is what a test that keeps the harness busy is told.
const ruleOfWaiting = "a command that waits or re-arms itself (tea.Tick, tea.Every, a loop of commands) must be made with waitCmd"

// t0 is when every test starts.
var t0 = time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)

func TestMain(m *testing.M) {
	// A table reads as soon as its watch says it changed, and not by
	// itself: a frame then depends only on what the test does.
	tableReadMin = time.Millisecond
	tableReadMax = time.Hour
	os.Exit(m.Run())
}

// clock is the time of a test, which only the test moves.
type clock struct {
	mu sync.Mutex
	at time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	c.at = c.at.Add(d)
	c.mu.Unlock()
}

// harness holds a model under test.
type harness struct {
	t     testing.TB
	clock *clock
	m     tea.Model
	w, h  int
	queue []tea.Cmd
	async chan tea.Msg
	done  chan struct{}
	quit  bool
	// settleFor bounds a settle.
	settleFor time.Duration
}

// newHarness starts the program of sess in a window of w by h: it runs
// Init, gives the size, and runs what follows.
func newHarness(t testing.TB, sess kube.Session, w, h int) *harness {
	t.Helper()
	c := &clock{at: t0}
	hs := &harness{
		t:         t,
		clock:     c,
		m:         New(sess, Options{Version: "v0.1.0", Now: c.now}),
		async:     make(chan tea.Msg, 64),
		done:      make(chan struct{}),
		settleFor: deadline,
	}
	t.Cleanup(func() { close(hs.done) })
	hs.queue = append(hs.queue, hs.m.Init())
	hs.send(tea.WindowSizeMsg{Width: w, Height: h})
	return hs
}

// newSession returns a session of a full key named alice@laptop.
func newSession() *kubetest.Session {
	return kubetest.New(keys.Grant{Name: "alice@laptop", Fingerprint: "SHA256:alice"})
}

// root returns the model as the root it is.
func (hs *harness) root() *root { return hs.m.(*root) }

// send feeds msg to the model and runs what follows.
func (hs *harness) send(msg tea.Msg) {
	hs.t.Helper()
	if size, ok := msg.(tea.WindowSizeMsg); ok {
		hs.w, hs.h = size.Width, size.Height
	}
	hs.deliver(msg)
	hs.settle()
}

// press presses keys by the names tea.KeyPressMsg.String gives them.
func (hs *harness) press(names ...string) {
	hs.t.Helper()
	for _, n := range names {
		hs.send(keyPress(n))
	}
}

// typeText types text, a key for each character.
func (hs *harness) typeText(text string) {
	hs.t.Helper()
	for _, r := range text {
		if r == ' ' {
			hs.press("space")
			continue
		}
		hs.press(string(r))
	}
}

// command types a command at the prompt and runs it.
func (hs *harness) command(text string) {
	hs.t.Helper()
	hs.press(":")
	hs.typeText(text)
	hs.press("enter")
}

// keyPress returns the key press that String names name.
func keyPress(name string) tea.KeyPressMsg {
	named := map[string]rune{
		"enter": tea.KeyEnter, "esc": tea.KeyEscape, "tab": tea.KeyTab,
		"backspace": tea.KeyBackspace, "up": tea.KeyUp, "down": tea.KeyDown,
		"left": tea.KeyLeft, "right": tea.KeyRight, "pgup": tea.KeyPgUp,
		"pgdown": tea.KeyPgDown, "home": tea.KeyHome, "end": tea.KeyEnd,
	}
	if code, ok := named[name]; ok {
		return tea.KeyPressMsg{Code: code}
	}
	switch name {
	case "space":
		return tea.KeyPressMsg{Code: tea.KeySpace, Text: " "}
	case "ctrl+space":
		return tea.KeyPressMsg{Code: tea.KeySpace, Mod: tea.ModCtrl}
	}
	if rest, ok := strings.CutPrefix(name, "ctrl+"); ok {
		r, _ := utf8.DecodeRuneInString(rest)
		return tea.KeyPressMsg{Code: r, Mod: tea.ModCtrl}
	}
	r, _ := utf8.DecodeRuneInString(name)
	return tea.KeyPressMsg{Code: r, Text: name}
}

// isWait reports whether cmd was made by waitCmd.
func isWait(cmd tea.Cmd) bool {
	return reflect.ValueOf(cmd).Pointer() == reflect.ValueOf(waiter{}.run).Pointer()
}

// deliver feeds msg to the model, or takes it apart when it is one of
// Bubble Tea's own.
func (hs *harness) deliver(msg tea.Msg) {
	hs.t.Helper()
	switch msg := msg.(type) {
	case nil:
		return
	case tea.BatchMsg:
		hs.queue = append(hs.queue, msg...)
		return
	case tea.QuitMsg:
		hs.quit = true
		return
	}
	if v := reflect.ValueOf(msg); v.Kind() == reflect.Slice && v.Type().Elem() == reflect.TypeOf(tea.Cmd(nil)) {
		hs.sequence(v)
		return
	}
	m, cmd := hs.m.Update(msg)
	hs.m = m
	hs.queue = append(hs.queue, cmd)
}

// sequence runs the commands of a tea.Sequence, held in v, in order:
// each one's message is delivered before the next one runs.
func (hs *harness) sequence(v reflect.Value) {
	hs.t.Helper()
	for i := range v.Len() {
		cmd := v.Index(i).Interface().(tea.Cmd)
		if cmd == nil {
			continue
		}
		if isWait(cmd) {
			hs.t.Fatalf("a waitCmd is in a tea.Sequence, which the harness does not run: it cannot wait in the middle of a sequence")
		}
		hs.deliver(hs.run(cmd))
	}
}

// settle runs the pending commands, and the commands they lead to, until
// none is left but those that wait. It fails the test when that does not
// end within maxSettle commands or settleFor.
func (hs *harness) settle() {
	hs.t.Helper()
	end := time.Now().Add(hs.settleFor)
	for n := 0; len(hs.queue) > 0; n++ {
		if n >= maxSettle || time.Now().After(end) {
			hs.t.Fatalf("the commands did not settle after %d of them in %v: %s", n, hs.settleFor, ruleOfWaiting)
		}
		cmd := hs.queue[0]
		hs.queue = hs.queue[1:]
		if cmd == nil {
			continue
		}
		if isWait(cmd) {
			go func() {
				msg := cmd()
				select {
				case hs.async <- msg:
				case <-hs.done:
				}
			}()
			continue
		}
		hs.deliver(hs.run(cmd))
	}
}

// run runs a command that does not wait and returns its message. It fails
// the test when the command takes longer than deadline.
func (hs *harness) run(cmd tea.Cmd) tea.Msg {
	hs.t.Helper()
	got := make(chan tea.Msg, 1)
	go func() { got <- cmd() }()
	select {
	case msg := <-got:
		return msg
	case <-time.After(deadline):
		hs.t.Fatalf("a command did not return in %v: %s", deadline, ruleOfWaiting)
		return nil
	}
}

// await takes the messages of waiting commands as they come until cond
// holds.
func (hs *harness) await(what string, cond func() bool) {
	hs.t.Helper()
	end := time.After(deadline)
	for !cond() {
		select {
		case msg := <-hs.async:
			hs.deliver(msg)
			hs.settle()
		case <-end:
			hs.t.Fatalf("timed out waiting for %s. The frame:\n%s", what, hs.frame())
		}
	}
}

// awaitFrame waits until the frame holds text.
func (hs *harness) awaitFrame(text string) {
	hs.t.Helper()
	hs.await(fmt.Sprintf("the frame to show %q", text), func() bool { return strings.Contains(hs.plain(), text) })
}

// drawn returns the frame as it is drawn, with its styles. It fails the
// test when a line is wider than the window or there are more lines
// than it is high.
func (hs *harness) drawn() string {
	hs.t.Helper()
	v := hs.m.View()
	if !v.AltScreen {
		hs.t.Error("the program is not on the alternate screen")
	}
	if v.Content == "" {
		return ""
	}
	lines := strings.Split(v.Content, "\n")
	if len(lines) > max(hs.h, 0) {
		hs.t.Errorf("the frame has %d lines in a window of %d", len(lines), hs.h)
	}
	for i, l := range lines {
		if w := ansi.StringWidth(l); w > hs.w {
			hs.t.Errorf("line %d of the frame is %d cells wide in a window of %d: %q", i, w, hs.w, ansi.Strip(l))
		}
	}
	return v.Content
}

// frame returns the frame as plain text.
func (hs *harness) frame() string {
	hs.t.Helper()
	return ansi.Strip(hs.drawn())
}

// plain returns the frame as plain text without checking it.
func (hs *harness) plain() string { return ansi.Strip(hs.m.View().Content) }

// lines returns the lines of the plain frame.
func (hs *harness) lines() []string {
	hs.t.Helper()
	return strings.Split(hs.frame(), "\n")
}

// line returns the first line of the plain frame that holds text, or "".
func (hs *harness) line(text string) string {
	hs.t.Helper()
	for _, l := range hs.lines() {
		if strings.Contains(l, text) {
			return l
		}
	}
	return ""
}

// drawnLine returns the first line of the drawn frame whose text holds
// text, or "".
func (hs *harness) drawnLine(text string) string {
	hs.t.Helper()
	for _, l := range strings.Split(hs.drawn(), "\n") {
		if strings.Contains(ansi.Strip(l), text) {
			return l
		}
	}
	return ""
}

// title returns the title in the frame's top line.
func (hs *harness) title() string {
	hs.t.Helper()
	for _, l := range hs.lines() {
		if strings.HasPrefix(l, "┌") {
			return strings.TrimSpace(strings.Trim(l, "┌─┐"))
		}
	}
	return ""
}

// wantFrame fails the test unless the plain frame holds every text.
func (hs *harness) wantFrame(texts ...string) {
	hs.t.Helper()
	f := hs.frame()
	for _, text := range texts {
		if !strings.Contains(f, text) {
			hs.t.Errorf("the frame does not show %q:\n%s", text, f)
		}
	}
}

// wantHint fails the test unless the plain frame shows the hint of key
// with text after it, however the padding of its column spaces them.
func (hs *harness) wantHint(key, text string) {
	hs.t.Helper()
	if f := hs.frame(); !showsHint(f, key, text) {
		hs.t.Errorf("the frame does not show the hint <%s> %s:\n%s", key, text, f)
	}
}

// notFrame fails the test when the plain frame holds one of texts.
func (hs *harness) notFrame(texts ...string) {
	hs.t.Helper()
	f := hs.frame()
	for _, text := range texts {
		if strings.Contains(f, text) {
			hs.t.Errorf("the frame shows %q:\n%s", text, f)
		}
	}
}

// golden compares the plain frame with testdata/<test>.golden, or
// rewrites it with -update. Spaces at the ends of lines are not
// compared.
func (hs *harness) golden() {
	hs.t.Helper()
	got := trimLines(hs.frame())
	path := filepath.Join("testdata", strings.ReplaceAll(hs.t.Name(), "/", "_")+".golden")
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			hs.t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			hs.t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		hs.t.Fatalf("reading the golden frame (go test ./tui/ -update writes it): %v", err)
	}
	if got != string(want) {
		hs.t.Errorf("the frame is not %s.\nGot:\n%s\nWant:\n%s", path, got, want)
	}
}

func trimLines(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " ")
	}
	return strings.Join(lines, "\n") + "\n"
}

// sgr returns the escape sequences a style starts its text with, to look
// for in a drawn frame.
func sgr(s interface{ Render(...string) string }) string {
	out := s.Render("x")
	return out[:strings.Index(out, "x")]
}

var spaces = regexp.MustCompile(` +`)

// words returns s with every run of spaces made one.
func words(s string) string { return strings.TrimSpace(spaces.ReplaceAllString(s, " ")) }

// The tables of the tests.

// podColumns are the columns of a table of pods, less some.
func podColumns() []render.Column {
	return []render.Column{
		{Name: "NAMESPACE"},
		{Name: "NAME"},
		{Name: "READY"},
		{Name: "STATUS"},
		{Name: "RESTARTS", Right: true, Numeric: true},
		{Name: "IP", Wide: true},
		{Name: "AGE", Right: true, Numeric: true},
	}
}

// pod is one row of a table of pods.
type pod struct {
	ns, name, status string
	age              time.Duration
	health           render.Health
	labels           map[string]string
}

func (p pod) row() render.Row {
	ns := p.ns
	if ns == "" {
		ns = "default"
	}
	status := p.status
	if status == "" {
		status = "Running"
	}
	age := p.age
	if age == 0 {
		age = time.Hour
	}
	return render.Row{
		Namespace: ns,
		Name:      p.name,
		Cells:     []string{ns, p.name, "1/1", status, "0", "10.0.0.1", ageText(age)},
		Order:     []float64{0, 0, 0, 0, 0, 0, age.Seconds()},
		Health:    p.health,
		Labels:    p.labels,
	}
}

func ageText(d time.Duration) string {
	switch {
	case d >= 24*time.Hour:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	case d >= time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
}

// podTable returns a table of pods.
func podTable(pods ...pod) kube.Table {
	t := kube.Table{Columns: podColumns(), Namespaced: true}
	for _, p := range pods {
		t.Rows = append(t.Rows, p.row())
	}
	return t
}

// podsNamed returns a table of running pods in default.
func podsNamed(names ...string) kube.Table {
	var pods []pod
	for _, n := range names {
		pods = append(pods, pod{name: n})
	}
	return podTable(pods...)
}

// simpleTable returns a table of a namespaced resource with the rows
// NAMESPACE NAME AGE, each in default.
func simpleTable(names ...string) kube.Table {
	t := kube.Table{
		Columns:    []render.Column{{Name: "NAMESPACE"}, {Name: "NAME"}, {Name: "AGE", Right: true, Numeric: true}},
		Namespaced: true,
	}
	for _, n := range names {
		t.Rows = append(t.Rows, render.Row{Namespace: "default", Name: n, Cells: []string{"default", n, "1h"}, Order: []float64{0, 0, 3600}})
	}
	return t
}

// clusterTable returns a table of a resource that is not namespaced.
func clusterTable(names ...string) kube.Table {
	t := kube.Table{Columns: []render.Column{{Name: "NAME"}, {Name: "AGE", Right: true, Numeric: true}}}
	for _, n := range names {
		t.Rows = append(t.Rows, render.Row{Name: n, Cells: []string{n, "1h"}, Order: []float64{0, 3600}})
	}
	return t
}

// query returns the query of a resource of kubetest.Standard in a
// namespace.
func query(plural, namespace string) kube.Query {
	return kube.Query{Resource: kubetest.Resource(plural), Namespace: namespace}
}

// queryOf returns the query of a resource's rows of an object.
func queryOf(res kube.Resource, of kube.ObjectRef) kube.Query {
	return kube.Query{Resource: res, Of: &of}
}

// The views and the dialog of the tests.

// testView is a view that does what a test sets and remembers what it
// was given.
type testView struct {
	name       string
	hints      []hint
	capturing  bool
	fullscreen bool

	keys   []string
	msgs   []tea.Msg
	inits  int
	closed int
	w, h   int
}

func (v *testView) Init() tea.Cmd { v.inits++; return nil }
func (v *testView) Update(msg tea.Msg) (view, tea.Cmd) {
	if k, ok := msg.(tea.KeyPressMsg); ok {
		v.keys = append(v.keys, k.String())
	} else {
		v.msgs = append(v.msgs, msg)
	}
	return v, nil
}
func (v *testView) View(w, h int) string {
	v.w, v.h = w, h
	lines := make([]string, h)
	for i := range lines {
		lines[i] = fmt.Sprintf("%s line %d", v.name, i)
	}
	return strings.Join(lines, "\n")
}
func (v *testView) Title() string     { return "Test(" + v.name + ")" }
func (v *testView) Crumb() string     { return v.name }
func (v *testView) Hints() []hint     { return v.hints }
func (v *testView) Capturing() bool   { return v.capturing }
func (v *testView) Fullscreen() bool  { return v.fullscreen }
func (v *testView) Close()            { v.closed++ }
func (v *testView) got(m tea.Msg) int { return countMsgs(v.msgs, m) }

func countMsgs(msgs []tea.Msg, m tea.Msg) int {
	n := 0
	for _, g := range msgs {
		if reflect.DeepEqual(g, m) {
			n++
		}
	}
	return n
}

// testDialog is a dialog that remembers what it was given.
type testDialog struct {
	keys  []string
	msgs  []tea.Msg
	inits int
}

func (d *testDialog) Init() tea.Cmd { d.inits++; return nil }
func (d *testDialog) Update(msg tea.Msg) (dialog, tea.Cmd) {
	if k, ok := msg.(tea.KeyPressMsg); ok {
		d.keys = append(d.keys, k.String())
	} else {
		d.msgs = append(d.msgs, msg)
	}
	return d, nil
}
func (d *testDialog) View(width, height int) string {
	return "╔══════════╗\n║ a dialog ║\n╚══════════╝"
}
func (d *testDialog) Fits(width, height int) bool { return height >= 3 }
func (d *testDialog) Drop()                       {}

// testMsg is a message of the tests that no part knows.
type testMsg struct{ n int }
