package tui

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fables-for-robots/kosh/kube"
	"github.com/fables-for-robots/kosh/kube/kubetest"
)

// actionSpySession is a kubetest.Session that can hold a method's calls
// until the test lets them go, remembers how long each call's context
// had left, and notes every call made from the model's Update or View.
type actionSpySession struct {
	*kubetest.Session

	mu sync.Mutex
	// hold makes the calls of a method wait until the channel is closed.
	hold map[string]chan struct{}
	// left is what the contexts of a method's calls had left: -1 for a
	// context without a deadline.
	left map[string][]time.Duration
	// inUpdateCalls are the methods called from Update or View.
	inUpdateCalls []string
	// fail is what Kill returns for an object, by its reference.
	fail map[string]error
}

func newActionSpySession() *actionSpySession { return newActionSpyOf(newSession()) }

// newActionSpyOf returns a spy over sess.
func newActionSpyOf(sess *kubetest.Session) *actionSpySession {
	return &actionSpySession{Session: sess, hold: map[string]chan struct{}{}, left: map[string][]time.Duration{}}
}

// holdCalls makes the calls of method wait, and returns what lets them
// go.
func (s *actionSpySession) holdCalls(method string) (release func()) {
	ch := make(chan struct{})
	s.mu.Lock()
	s.hold[method] = ch
	s.mu.Unlock()
	var once sync.Once
	return func() { once.Do(func() { close(ch) }) }
}

// enter notes a call of method and waits while it is held.
func (s *actionSpySession) enter(ctx context.Context, method string) {
	left := time.Duration(-1)
	if d, ok := ctx.Deadline(); ok {
		left = time.Until(d)
	}
	s.mu.Lock()
	s.left[method] = append(s.left[method], left)
	if actionInUpdate() {
		s.inUpdateCalls = append(s.inUpdateCalls, method)
	}
	ch := s.hold[method]
	s.mu.Unlock()
	if ch != nil {
		<-ch
	}
}

// leftOf returns what the contexts of method's calls had left.
func (s *actionSpySession) leftOf(method string) []time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.left[method])
}

func (s *actionSpySession) calledInUpdate() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.inUpdateCalls)
}

func (s *actionSpySession) YAML(ctx context.Context, ref kube.ObjectRef, mf bool) (string, error) {
	s.enter(ctx, "YAML")
	return s.Session.YAML(ctx, ref, mf)
}

func (s *actionSpySession) Images(ctx context.Context, ref kube.ObjectRef) ([]kube.ContainerImage, error) {
	s.enter(ctx, "Images")
	return s.Session.Images(ctx, ref)
}

func (s *actionSpySession) Replicas(ctx context.Context, ref kube.ObjectRef) (int32, error) {
	s.enter(ctx, "Replicas")
	return s.Session.Replicas(ctx, ref)
}

func (s *actionSpySession) Delete(ctx context.Context, ref kube.ObjectRef, opts kube.DeleteOptions) error {
	s.enter(ctx, "Delete")
	return s.Session.Delete(ctx, ref, opts)
}

func (s *actionSpySession) Kill(ctx context.Context, ref kube.ObjectRef) error {
	s.enter(ctx, "Kill")
	if err := s.fail[ref.String()]; err != nil {
		return err
	}
	return s.Session.Kill(ctx, ref)
}

func (s *actionSpySession) Update(ctx context.Context, ref kube.ObjectRef, yaml string) error {
	s.enter(ctx, "Update")
	return s.Session.Update(ctx, ref, yaml)
}

func (s *actionSpySession) Scale(ctx context.Context, ref kube.ObjectRef, n int32) error {
	s.enter(ctx, "Scale")
	return s.Session.Scale(ctx, ref, n)
}

func (s *actionSpySession) Restart(ctx context.Context, ref kube.ObjectRef) error {
	s.enter(ctx, "Restart")
	return s.Session.Restart(ctx, ref)
}

func (s *actionSpySession) SetImages(ctx context.Context, ref kube.ObjectRef, images map[string]string) error {
	s.enter(ctx, "SetImages")
	return s.Session.SetImages(ctx, ref, images)
}

func (s *actionSpySession) Rollback(ctx context.Context, ref kube.ObjectRef) error {
	s.enter(ctx, "Rollback")
	return s.Session.Rollback(ctx, ref)
}

func (s *actionSpySession) Trigger(ctx context.Context, ref kube.ObjectRef) (string, error) {
	s.enter(ctx, "Trigger")
	return s.Session.Trigger(ctx, ref)
}

func (s *actionSpySession) SetSuspended(ctx context.Context, ref kube.ObjectRef, on bool) error {
	s.enter(ctx, "SetSuspended")
	return s.Session.SetSuspended(ctx, ref, on)
}

func (s *actionSpySession) Cordon(ctx context.Context, ref kube.ObjectRef, on bool) error {
	s.enter(ctx, "Cordon")
	return s.Session.Cordon(ctx, ref, on)
}

func (s *actionSpySession) Drain(ctx context.Context, ref kube.ObjectRef, opts kube.DrainOptions) error {
	s.enter(ctx, "Drain")
	return s.Session.Drain(ctx, ref, opts)
}

// actionInUpdate reports whether the root's Update or View is among
// the callers.
func actionInUpdate() bool {
	buf := make([]byte, 64<<10)
	stack := string(buf[:runtime.Stack(buf, false)])
	return strings.Contains(stack, "tui.(*root).Update(") || strings.Contains(stack, "tui.(*root).View(")
}

// actionKillKey is the key that kills.
const actionKillKey = "ctrl+k"

// actionPods returns the names web-01 to web-<n>.
func actionPods(n int) []string {
	var names []string
	for i := 1; i <= n; i++ {
		names = append(names, fmt.Sprintf("web-%02d", i))
	}
	return names
}

// actionKilled returns the names of the pods Kill was called for, in
// their order.
func actionKilled(sess *kubetest.Session) []string {
	var out []string
	for _, c := range sess.Called("Kill") {
		out = append(out, c.Args[0].(kube.ObjectRef).Name)
	}
	return out
}

// actionMarkAll marks every row of the table.
func actionMarkAll(hs *harness) {
	hs.t.Helper()
	hs.press("g", "space", "G", "ctrl+space")
}

func TestConfirmListsMarkedRows(t *testing.T) {
	sess := newSession()
	names := actionPods(14)
	sess.SetTable(query("pods", "default"), podsNamed(names...))
	hs := newHarness(t, sess, 100, 30)
	actionMarkAll(hs)
	hs.press(actionKillKey)
	hs.wantFrame("Kill 14 pods?", "… and 4 more")
	for i, n := range names {
		shown := strings.Contains(hs.frame(), "default/"+n)
		if shown != (i < 10) {
			t.Errorf("%s is listed: %v", n, shown)
		}
	}
	hs.press("right", "enter")
	hs.awaitFrame("Killed 14 pods")
	if got := actionKilled(sess); !slices.Equal(got, names) {
		t.Errorf("Kill was called for %v", got)
	}
}

func TestActionGoesOnAfterAnError(t *testing.T) {
	sess := newSession()
	sess.Errs["Kill"] = errors.New("pods \"web-01\" is forbidden: \x1b[2Jno")
	sess.SetTable(query("pods", "default"), podsNamed(actionPods(3)...))
	hs := newHarness(t, sess, 100, 30)
	actionMarkAll(hs)
	hs.press(actionKillKey, "right", "enter")
	hs.awaitFrame("Killed 0 of 3 pods")
	if got := actionKilled(sess); len(got) != 3 {
		t.Errorf("Kill was called for %v", got)
	}
	line := hs.drawnLine("Killed 0 of 3 pods")
	if !strings.Contains(line, sgr(styleError)) {
		t.Errorf("the flash is not in the error style: %q", line)
	}
	hs.wantFrame(`Killed 0 of 3 pods: pods "web-01" is forbidden: no`)
	if strings.Contains(hs.drawn(), "\x1b[2J") {
		t.Error("the error is drawn as the session gave it")
	}

	// One row: the error as the session worded it.
	sess.Errs["Kill"] = errors.New("boom")
	hs.press("ctrl+\\", actionKillKey, "right", "enter")
	hs.awaitFrame("boom")
	if !strings.Contains(hs.drawnLine("boom"), sgr(styleError)) {
		t.Error("the error of one row is not in the error style")
	}
}

// Some rows fail: the flash counts those done and gives the first error.
func TestActionFailsForSomeRows(t *testing.T) {
	sess := newActionSpySession()
	sess.fail = map[string]error{
		"pods/default/web-02": errors.New("first"),
		"pods/default/web-03": errors.New("second"),
	}
	sess.SetTable(query("pods", "default"), podsNamed(actionPods(4)...))
	hs := newHarness(t, sess, 100, 30)
	actionMarkAll(hs)
	hs.press(actionKillKey, "right", "enter")
	hs.awaitFrame("Killed 2 of 4 pods: first")
	if !strings.Contains(hs.drawnLine("Killed 2 of 4 pods"), sgr(styleError)) {
		t.Error("the flash is not in the error style")
	}
	if got := sess.leftOf("Kill"); len(got) != 4 {
		t.Errorf("Kill was called %d times", len(got))
	}
}

func TestFlashAfterAnAction(t *testing.T) {
	sess := newCountingSession()
	q := query("pods", "default")
	sess.SetTable(q, podsNamed("web-1", "web-2", "web-3"))
	hs := newHarness(t, sess, 100, 30)
	reads := len(sess.readsOf(q))
	hs.press(actionKillKey, "right", "enter")
	hs.awaitFrame("Killed pods/default/web-1")
	if got := len(sess.readsOf(q)); got <= reads {
		t.Error("the table did not read again after the action")
	}
	reads = len(sess.readsOf(q))
	actionMarkAll(hs)
	hs.press(actionKillKey, "right", "enter")
	hs.awaitFrame("Killed 3 pods")
	if got := len(sess.readsOf(q)); got <= reads {
		t.Error("the table did not read again after the action over three rows")
	}
}

func TestNoSecondActionWhileOneRuns(t *testing.T) {
	sess := newActionSpySession()
	release := sess.holdCalls("Kill")
	t.Cleanup(release)
	sess.SetTable(query("pods", "default"), podsNamed("web-1", "web-2"))
	hs := newHarness(t, sess, 100, 30)
	hs.press(actionKillKey, "right", "enter")
	// The call is held; a second key opens nothing.
	hs.press("down", actionKillKey)
	hs.wantFrame("an action is still running")
	if hs.root().dlg != nil {
		t.Error("a dialog opened while an action runs")
	}
	release()
	hs.awaitFrame("Killed pods/default/web-1")
	// Another session is not held up by this one's action.
	other := newActionSpySession()
	releaseOther := other.holdCalls("Kill")
	t.Cleanup(releaseOther)
	other.SetTable(query("pods", "default"), podsNamed("web-1"))
	hs2 := newHarness(t, other, 100, 30)
	hs2.press(actionKillKey, "right", "enter")
	hs.press(actionKillKey)
	if hs.root().dlg == nil {
		t.Error("an action of another session held this one up")
	}
	releaseOther()
	hs2.awaitFrame("Killed pods/default/web-1")
}

func TestNothingIsCalledInUpdate(t *testing.T) {
	sess := newActionSpySession()
	sess.SetTable(query("pods", "default"), podsNamed("web-1"))
	hs := newHarness(t, sess, 100, 30)
	hs.press(actionKillKey, "right", "enter")
	hs.awaitFrame("Killed pods/default/web-1")
	if len(sess.Called("Kill")) != 1 {
		t.Fatalf("Kill was called %d times", len(sess.Called("Kill")))
	}
	// The reads before a dialog, and the actions after them.
	sess.ImagesOf["deployments/default/web"] = []kube.ContainerImage{{Name: "app", Image: "nginx:1"}}
	actionOpen(hs, sess.Session, "deployments", simpleTable("web"))
	hs.press("s")
	dialogFocusOn(hs, "Scale")
	hs.press("enter")
	hs.awaitFrame("Scaled deployments/default/web")
	hs.press("i", "ctrl+u")
	hs.typeText("nginx:2")
	dialogFocusOn(hs, "Set")
	hs.press("enter")
	hs.awaitFrame("Set the images of deployments/default/web")
	if got := sess.entered(); !slices.Equal(got, []string{"Images", "Kill", "Replicas", "Scale", "SetImages"}) {
		t.Errorf("the session was called for %v", got)
	}
	if got := sess.calledInUpdate(); len(got) > 0 {
		t.Errorf("called in Update: %v", got)
	}
	// Each call has a context of apiTimeout.
	for _, left := range sess.leftOf("Kill") {
		if left <= apiTimeout-5*time.Second || left > apiTimeout {
			t.Errorf("Kill's context had %v left", left)
		}
	}
}

func TestDialogNamesAreCleaned(t *testing.T) {
	sess := newSession()
	sess.SetTable(query("pods", "default"), podsNamed("web\x1b[2J-1", "web-2"))
	hs := newHarness(t, sess, 100, 30)
	hs.press(actionKillKey)
	if strings.Contains(hs.drawn(), "\x1b[2J") {
		t.Error("the dialog draws the escape sequence of a name")
	}
	hs.press("esc")
	// What the binding is given is cleaned too, whoever gives it.
	e := env{sess: sess, now: hs.clock.now}
	for _, refs := range [][]kube.ObjectRef{
		{kubetest.Ref("pods", "default", "evil\x1b]0;owned\x07-1")},
		{kubetest.Ref("pods", "default", "evil\x1b]0;owned\x07-1"), kubetest.Ref("pods", "def\x1b[2Jault", "evil-2\r")},
	} {
		var b binding
		for _, x := range bindings {
			if x.Key == actionKillKey {
				b = x
			}
		}
		hs.send(b.Run(e, query("pods", "default"), refs)())
		d := hs.drawn()
		for _, bad := range []string{"\x1b]0;", "\x1b[2J", "\r"} {
			if strings.Contains(d, bad) {
				t.Errorf("the dialog draws %q", bad)
			}
		}
		hs.wantFrame("evil-1")
		hs.press("esc")
	}
}
