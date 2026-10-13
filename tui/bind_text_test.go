package tui

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/fables-for-robots/kosh/keys"
	"github.com/fables-for-robots/kosh/kube"
	"github.com/fables-for-robots/kosh/kube/kubetest"
)

// textSecret is the secret of the tests.
var textSecret = kubetest.Ref("secrets", "default", "creds")

// textPodsAndSecrets returns a session of grant with a table of the pod
// web-1 and one of the secret creds, both in default.
func textPodsAndSecrets(grant keys.Grant) *kubetest.Session {
	sess := kubetest.New(grant)
	sess.SetTable(query("pods", "default"), podsNamed("web-1"))
	sess.SetTable(query("secrets", "default"), simpleTable("creds"))
	return sess
}

// textTop returns the top view as the text view it is.
func textTop(hs *harness) *textView {
	hs.t.Helper()
	v, ok := hs.root().top().(*textView)
	if !ok {
		hs.t.Fatalf("the top view is a %T", hs.root().top())
	}
	return v
}

// textFlash returns the flash line, plain.
func textFlash(hs *harness) string {
	hs.t.Helper()
	l := hs.lines()
	return l[len(l)-1]
}

// textPause lets d pass while the messages of the waiting commands are
// taken as they come.
func textPause(hs *harness, d time.Duration) {
	hs.t.Helper()
	var done atomic.Bool
	hs.queue = append(hs.queue, waitCmd(func() tea.Msg {
		time.Sleep(d)
		done.Store(true)
		return nil
	}))
	hs.settle()
	hs.await("a pause", done.Load)
}

// textHasBinding reports whether bs has a binding of key with the hint
// and the order.
func textHasBinding(bs []binding, key string, h hint, order int) bool {
	for _, b := range bs {
		if b.Key == key {
			return b.Hint == h && b.Order == order && b.Run != nil
		}
	}
	return false
}

func TestBindingsAreRegistered(t *testing.T) {
	full := keys.Grant{Name: "alice"}
	ro := keys.Grant{Name: "bob", ReadOnly: true}
	for _, g := range []keys.Grant{full, ro} {
		pods := bindingsFor(kubetest.Resource("pods"), g)
		if !textHasBinding(pods, "d", hint{"d", "Describe"}, 10) || !textHasBinding(pods, "y", hint{"y", "YAML"}, 11) {
			t.Errorf("read-only %v: the pods have not d and y as they should: %+v", g.ReadOnly, pods)
		}
		if textHasBinding(pods, "x", hint{"x", "Decode"}, 12) {
			t.Errorf("read-only %v: the pods have x", g.ReadOnly)
		}
		secrets := bindingsFor(kubetest.Resource("secrets"), g)
		for _, b := range []struct {
			key   string
			h     hint
			order int
		}{{"d", hint{"d", "Describe"}, 10}, {"y", hint{"y", "YAML"}, 11}, {"x", hint{"x", "Decode"}, 12}} {
			if !textHasBinding(secrets, b.key, b.h, b.order) {
				t.Errorf("read-only %v: the secrets have not %s as they should: %+v", g.ReadOnly, b.key, secrets)
			}
		}
	}
}

func TestDescribeOpens(t *testing.T) {
	sess := textPodsAndSecrets(keys.Grant{Name: "alice@laptop"})
	sess.Descriptions[textPod.String()] = strings.Join([]string{
		"Name:             web-1",
		"Namespace:        default",
		"Priority:         0",
		"Node:             kind-control-plane/172.18.0.2",
		"Status:           Running",
		"IP:               10.244.0.5",
		"Containers:",
		"  web:",
		"    Image:          nginx:1.27",
		"    State:          Running",
		"Events:            <none>",
	}, "\n") + "\n"
	hs := newHarness(t, sess, 100, 30)
	hs.press("d")
	if got := hs.title(); got != "Describe(pods/default/web-1)" {
		t.Errorf("the title is %q", got)
	}
	if l := hs.lines()[28]; !strings.HasPrefix(l, "<pods> <describe>") {
		t.Errorf("the crumbs are %q", l)
	}
	hs.wantFrame("Node:             kind-control-plane/172.18.0.2", "Events:            <none>")
	hs.golden()
	hs.press("esc")
	if got := hs.title(); got != "Pods(default)[1]" || len(hs.root().stack) != 1 {
		t.Errorf("after esc the title is %q, the stack %d views", got, len(hs.root().stack))
	}
	hs.press("d", "q")
	if len(hs.root().stack) != 1 {
		t.Errorf("q leaves %d views", len(hs.root().stack))
	}
}

func TestYAMLOpens(t *testing.T) {
	sess := textPodsAndSecrets(keys.Grant{Name: "alice@laptop"})
	sess.YAMLs[textPod.String()] = strings.Join([]string{
		"apiVersion: v1",
		"kind: Pod",
		"metadata:",
		"  name: web-1",
		"  namespace: default",
		"  labels:",
		"    app: web # the app",
		"spec:",
		"  containers:",
		"  - name: web",
		"    image: nginx:1.27",
		"    args:",
		"    - --port=8080",
	}, "\n") + "\n"
	hs := newHarness(t, sess, 100, 30)
	hs.press("y")
	if got := hs.title(); got != "YAML(pods/default/web-1)" {
		t.Errorf("the title is %q", got)
	}
	if l := hs.lines()[28]; !strings.HasPrefix(l, "<pods> <yaml>") {
		t.Errorf("the crumbs are %q", l)
	}
	if calls := sess.Called("YAML"); len(calls) != 1 || calls[0].Args[1] != false {
		t.Errorf("YAML was called %+v", calls)
	}
	hs.golden()
	if l := hs.drawnLine("image: nginx"); !strings.Contains(l, styleKey.Render("image")) {
		t.Errorf("the YAML is not coloured: %q", l)
	}
	if l := hs.drawnLine("app: web"); !strings.Contains(l, styleDim.Render("# the app")) {
		t.Errorf("the comment is not dim: %q", l)
	}
}

func TestDecodeOpens(t *testing.T) {
	sess := textPodsAndSecrets(keys.Grant{Name: "alice@laptop"})
	sess.Secrets[textSecret.String()] = []kube.SecretEntry{
		{Key: "password", Value: "hunter2"},
		{Key: "tls.crt", Value: "-----BEGIN CERTIFICATE-----\nMIIBszCCAVmgAwIBAgIU\n-----END CERTIFICATE-----\n"},
		{Key: "user", Value: "admin"},
	}
	hs := newHarness(t, sess, 100, 30)
	hs.command("secrets")
	hs.press("x")
	if got := hs.title(); got != "Decode(secrets/default/creds)" {
		t.Errorf("the title is %q", got)
	}
	if l := hs.lines()[28]; !strings.HasPrefix(l, "<secrets> <decode>") {
		t.Errorf("the crumbs are %q", l)
	}
	want := []string{
		"password: hunter2",
		"tls.crt:",
		"  -----BEGIN CERTIFICATE-----",
		"  MIIBszCCAVmgAwIBAgIU",
		"  -----END CERTIFICATE-----",
		"user: admin",
	}
	if got := textBody(hs)[:7]; strings.Join(got, "\n") != strings.Join(want, "\n")+"\n" {
		t.Errorf("the entries are drawn\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	hs.golden()
}

func TestDecodeOnlyOnSecrets(t *testing.T) {
	sess := textPodsAndSecrets(keys.Grant{Name: "alice@laptop"})
	hs := newHarness(t, sess, 100, 30)
	hs.wantFrame("<d> Describe")
	hs.notFrame("Decode")
	for _, h := range hs.table().Hints() {
		if h.Key == "x" {
			t.Errorf("the pods have the hint %v", h)
		}
	}
	hs.press("x")
	if len(hs.root().stack) != 1 || len(sess.Called("SecretData")) != 0 {
		t.Errorf("x on a pod opened %d views and read %d secrets", len(hs.root().stack), len(sess.Called("SecretData")))
	}
	hs.command("secrets")
	found := false
	for _, h := range hs.table().Hints() {
		found = found || h == (hint{"x", "Decode"})
	}
	if !found {
		t.Errorf("the secrets have not the hint of x: %v", hs.table().Hints())
	}
}

func TestNoTextKeysOnContainersAndRules(t *testing.T) {
	for _, g := range []keys.Grant{{Name: "alice"}, {Name: "bob", ReadOnly: true}} {
		for _, res := range []kube.Resource{kube.Containers, kube.Rules} {
			for _, b := range bindingsFor(res, g) {
				if b.Key == "d" || b.Key == "y" || b.Key == "x" {
					t.Errorf("the %s have %s", res.GVR.Resource, b.Key)
				}
			}
		}
	}
	sess := textPodsAndSecrets(keys.Grant{Name: "alice@laptop"})
	sess.SetTable(queryOf(kube.Containers, textPod), simpleTable("web"))
	sess.SetTable(queryOf(kube.Rules, kubetest.Ref("roles", "default", "reader")), simpleTable("pods"))
	sess.SetTable(query("roles", "default"), simpleTable("reader"))
	hs := newHarness(t, sess, 100, 30)
	hs.press("enter")
	if got := hs.title(); got != "Containers(pods/default/web-1)[1]" {
		t.Fatalf("the title is %q", got)
	}
	hs.press("d", "y")
	hs.notFrame("<d>", "<y>")
	hs.command("roles")
	hs.press("enter")
	if got := hs.title(); got != "Rules(roles/default/reader)[1]" {
		t.Fatalf("the title is %q", got)
	}
	hs.press("d", "y")
	hs.notFrame("<d>", "<y>")
	if n := len(sess.Called("Describe")) + len(sess.Called("YAML")); n != 0 {
		t.Errorf("d and y read %d times", n)
	}
	if _, ok := hs.root().top().(*tableView); !ok {
		t.Errorf("the top view is a %T", hs.root().top())
	}
}

func TestFirstLoadErrorIsAFlash(t *testing.T) {
	sess := textPodsAndSecrets(keys.Grant{Name: "alice@laptop"})
	sess.Errs["Describe"] = errors.New("describing went wrong")
	sess.Errs["YAML"] = errors.New("the YAML went wrong")
	sess.Errs["SecretData"] = errors.New("the secret went wrong")
	hs := newHarness(t, sess, 100, 30)
	hs.command("secrets")
	for _, k := range []struct{ key, flash string }{
		{"d", "describing went wrong"},
		{"y", "the YAML went wrong"},
		{"x", "the secret went wrong"},
	} {
		hs.press(k.key)
		if len(hs.root().stack) != 1 {
			t.Errorf("%s pushed a view after an error: %q", k.key, hs.title())
		}
		if l := textFlash(hs); !strings.Contains(l, k.flash) {
			t.Errorf("after %s the flash is %q", k.key, l)
		}
		if l := hs.drawnLine(k.flash); !strings.Contains(l, sgr(styleError)) {
			t.Errorf("the flash of %s is not an error: %q", k.key, l)
		}
	}
}

func TestReadOnlySecret(t *testing.T) {
	sess := textPodsAndSecrets(keys.Grant{Name: "bob@ci", ReadOnly: true})
	sess.YAMLs[textSecret.String()] = "data:\n  password: aHVudGVyMg==\n"
	sess.Descriptions[textSecret.String()] = "Name: creds\n"
	sess.Secrets[textSecret.String()] = []kube.SecretEntry{{Key: "password", Value: "hunter2"}}
	hs := newHarness(t, sess, 100, 30)
	hs.command("secrets")
	hs.wantFrame("<d> Describe", "<y> YAML", "<x> Decode")
	before := hs.frame()
	for _, k := range []string{"d", "y", "x"} {
		hs.clock.advance(time.Minute) // the flash before is gone
		hs.press(k)
		if len(hs.root().stack) != 1 || hs.title() != "Secrets(default)[1]" {
			t.Errorf("%s left the table: %q", k, hs.title())
		}
		if l := textFlash(hs); !strings.Contains(l, "this key is read-only") {
			t.Errorf("after %s the flash is %q", k, l)
		}
		hs.notFrame("hunter2", "aHVudGVy")
	}
	// An error stays after its time until a key is pressed: k, which
	// moves nothing in a table of one row.
	hs.clock.advance(time.Minute)
	hs.press("k")
	if hs.frame() != before {
		t.Errorf("the table is not as it was:\n%s\nwas\n%s", hs.frame(), before)
	}
}

// textBlockingSession is a session whose readings of a text wait until
// release is closed.
type textBlockingSession struct {
	*kubetest.Session
	release chan struct{}
}

func (s *textBlockingSession) Describe(ctx context.Context, ref kube.ObjectRef) (string, error) {
	<-s.release
	return s.Session.Describe(ctx, ref)
}

func (s *textBlockingSession) YAML(ctx context.Context, ref kube.ObjectRef, managed bool) (string, error) {
	<-s.release
	return s.Session.YAML(ctx, ref, managed)
}

func (s *textBlockingSession) SecretData(ctx context.Context, ref kube.ObjectRef) ([]kube.SecretEntry, error) {
	<-s.release
	return s.Session.SecretData(ctx, ref)
}

func TestNothingIsLoadedInUpdate(t *testing.T) {
	for _, k := range []struct{ key, method, title string }{
		{"d", "Describe", "Describe(secrets/default/creds)"},
		{"y", "YAML", "YAML(secrets/default/creds)"},
		{"x", "SecretData", "Decode(secrets/default/creds)"},
	} {
		t.Run(k.key, func(t *testing.T) {
			sess := &textBlockingSession{Session: textPodsAndSecrets(keys.Grant{Name: "alice@laptop"}), release: make(chan struct{})}
			released := false
			release := func() {
				if !released {
					released = true
					close(sess.release)
				}
			}
			t.Cleanup(release)
			hs := newHarness(t, sess, 100, 30)
			hs.command("secrets")
			returned := make(chan tea.Cmd, 1)
			go func() {
				_, cmd := hs.root().Update(keyPress(k.key))
				returned <- cmd
			}()
			var cmd tea.Cmd
			select {
			case cmd = <-returned:
			case <-time.After(5 * time.Second):
				t.Fatalf("Update of %s waits for the session", k.key)
			}
			if calls := sess.Called(k.method); len(calls) != 0 {
				t.Fatalf("%s was called in Update: %+v", k.method, calls)
			}
			if cmd == nil {
				t.Fatalf("%s returns no command", k.key)
			}
			release()
			hs.queue = append(hs.queue, cmd)
			hs.settle()
			if calls := sess.Called(k.method); len(calls) != 1 {
				t.Errorf("%s was called %d times by the command", k.method, len(calls))
			}
			if got := hs.title(); got != k.title {
				t.Errorf("the command opened %q", got)
			}
		})
	}
}

func TestYAMLAutoRefresh(t *testing.T) {
	sess := textPodsAndSecrets(keys.Grant{Name: "alice@laptop"})
	numbered := func(word string) string {
		return strings.ReplaceAll(textNumbered(100), "line", word)
	}
	sess.YAMLs[textPod.String()] = numbered("first")
	hs := newHarness(t, sess, 100, 30)
	hs.press("y")
	hs.drawn() // as the program draws it before the next key: a page is the body's height
	v := textTop(hs)
	v.every = 5 * time.Millisecond
	hs.press("pgdown")
	if got := textFirst(hs); got != "first 020" {
		t.Fatalf("after pgdown the first line is %q", got)
	}
	hs.press("r")
	if got := hs.title(); got != "YAML(pods/default/web-1) (auto)" {
		t.Errorf("after r the title is %q", got)
	}
	sess.YAMLs[textPod.String()] = numbered("second")
	hs.awaitFrame("second 020")
	if got := textFirst(hs); got != "second 020" {
		t.Errorf("after the YAML was read again the first line is %q", got)
	}
	// It goes on.
	sess.YAMLs[textPod.String()] = numbered("third")
	hs.awaitFrame("third 020")
	calls := len(sess.Called("YAML"))
	if calls < 3 {
		t.Errorf("YAML was read %d times", calls)
	}

	hs.press("r")
	if got := hs.title(); got != "YAML(pods/default/web-1)" {
		t.Errorf("after r again the title is %q", got)
	}
	calls = len(sess.Called("YAML"))
	textPause(hs, 50*time.Millisecond)
	if n := len(sess.Called("YAML")); n != calls {
		t.Errorf("YAML was read %d times after r turned it off", n-calls)
	}

	// Close stops it.
	hs.press("r")
	stop := v.autoStop
	sess.YAMLs[textPod.String()] = numbered("fourth")
	hs.awaitFrame("fourth 020")
	hs.press("q")
	if len(hs.root().stack) != 1 {
		t.Fatalf("q left %d views", len(hs.root().stack))
	}
	select {
	case <-stop:
	default:
		t.Error("Close leaves the timer running")
	}
	if _, cmd := v.Update(textTickMsg{id: v.id, autoGen: v.autoGen}); cmd != nil {
		t.Error("a closed view reads again on a tick")
	}
	calls = len(sess.Called("YAML"))
	textPause(hs, 50*time.Millisecond)
	if n := len(sess.Called("YAML")); n != calls {
		t.Errorf("YAML was read %d times after the view was closed", n-calls)
	}
}

func TestYAMLManagedFields(t *testing.T) {
	sess := textPodsAndSecrets(keys.Grant{Name: "alice@laptop"})
	sess.YAMLs[textPod.String()] = "kind: Pod\n"
	hs := newHarness(t, sess, 100, 30)
	hs.press("y")
	last := func() kube.ObjectRef {
		calls := sess.Called("YAML")
		return calls[len(calls)-1].Args[0].(kube.ObjectRef)
	}
	for i, want := range []bool{true, false, true} {
		hs.press("m")
		calls := sess.Called("YAML")
		if len(calls) != i+2 || calls[i+1].Args[1] != want {
			t.Errorf("after m %d times YAML was called %+v", i+1, calls)
		}
	}
	if last().String() != textPod.String() {
		t.Errorf("YAML was read of %s", last())
	}
	// Not in a description.
	hs.press("esc", "d", "m", "r")
	if n := len(sess.Called("YAML")); n != 4 {
		t.Errorf("m in a description read the YAML: %d calls", n)
	}
	if got := hs.title(); got != "Describe(pods/default/web-1)" {
		t.Errorf("r in a description gives the title %q", got)
	}
	for _, h := range textTop(hs).Hints() {
		if h.Key == "m" || h.Key == "r" {
			t.Errorf("a description has the hint %v", h)
		}
	}
}

func TestLaterLoadErrorKeepsTheText(t *testing.T) {
	sess := textPodsAndSecrets(keys.Grant{Name: "alice@laptop"})
	sess.YAMLs[textPod.String()] = "kind: Pod\nmetadata:\n  name: web-1\n"
	hs := newHarness(t, sess, 100, 30)
	hs.press("y")
	sess.Errs["YAML"] = errors.New("the API server is gone")
	hs.press("m")
	if len(sess.Called("YAML")) != 2 {
		t.Fatalf("m did not read again")
	}
	hs.wantFrame("kind: Pod", "name: web-1")
	if got := hs.title(); got != "YAML(pods/default/web-1)" {
		t.Errorf("the title is %q", got)
	}
	if l := textFlash(hs); !strings.Contains(l, "the API server is gone") {
		t.Errorf("the flash is %q", l)
	}
	// So does a reading of its own.
	v := textTop(hs)
	v.every = 5 * time.Millisecond
	hs.clock.advance(time.Minute)
	hs.press("r")
	hs.awaitFrame("the API server is gone")
	hs.wantFrame("kind: Pod", "name: web-1")
}

func TestTextIsCleaned(t *testing.T) {
	evil := "\x1b]0;x\x07\x1b[2J\x00"
	sess := textPodsAndSecrets(keys.Grant{Name: "alice@laptop"})
	sess.Descriptions[textPod.String()] = "Name: web-1" + evil + "\nAnnotations: a=" + evil + "b\n"
	sess.YAMLs[textPod.String()] = "metadata:\n  name: web-1" + evil + "\n  annotations:\n    a: " + evil + "\n"
	sess.Secrets[textSecret.String()] = []kube.SecretEntry{
		{Key: "token", Value: "abc" + evil},
		{Key: "script" + evil, Value: "one" + evil + "\ntwo\nthree"},
	}
	hs := newHarness(t, sess, 100, 30)
	check := func(what string) {
		t.Helper()
		_, cmd := textTop(hs).Update(keyPress("c"))
		copied := textClipboard(t, textMsgs(cmd))
		for _, f := range []string{hs.drawn(), copied} {
			for _, bad := range []string{"\x1b]0;", "\x1b[2J", "\x00", "\x07"} {
				if strings.Contains(f, bad) {
					t.Errorf("%s: %q is drawn or copied", what, bad)
				}
			}
		}
		if !strings.Contains(copied, "␀") {
			t.Errorf("%s: the copy has not the NUL's picture: %q", what, copied)
		}
		hs.press("esc")
	}
	hs.press("d")
	check("describe")
	hs.press("y")
	check("yaml")
	hs.command("secrets")
	hs.press("x")
	hs.wantFrame("token: abc␀", "  two")
	check("decode")
}

func TestSecondKeyWhileOpeningOpensNothing(t *testing.T) {
	sess := &textBlockingSession{Session: textPodsAndSecrets(keys.Grant{Name: "alice@laptop"}), release: make(chan struct{})}
	released := false
	release := func() {
		if !released {
			released = true
			close(sess.release)
		}
	}
	t.Cleanup(release)
	hs := newHarness(t, sess, 100, 30)
	_, first := hs.root().Update(keyPress("d"))
	if first == nil {
		t.Fatal("d gives no command")
	}
	for _, k := range []string{"d", "y"} {
		if _, cmd := hs.root().Update(keyPress(k)); cmd != nil {
			t.Errorf("%s while the first reading is in flight gives a command", k)
		}
	}
	release()
	hs.queue = append(hs.queue, first)
	hs.settle()
	if len(hs.root().stack) != 2 || len(sess.Called("Describe")) != 1 || len(sess.Called("YAML")) != 0 {
		t.Errorf("the stack has %d views; Describe was called %d times, YAML %d", len(hs.root().stack), len(sess.Called("Describe")), len(sess.Called("YAML")))
	}
	// Once it is back, the keys open again.
	hs.press("esc", "y")
	if got := hs.title(); got != "YAML(pods/default/web-1)" {
		t.Errorf("y after the first reading opened %q", got)
	}
	// So they do after a reading that failed.
	hs.press("esc")
	sess.Errs["Describe"] = errors.New("no")
	hs.press("d")
	delete(sess.Errs, "Describe")
	hs.press("d")
	if got := hs.title(); got != "Describe(pods/default/web-1)" {
		t.Errorf("d after a failed reading opened %q", got)
	}
}

// textHoldingSession is a session whose YAML, while hold is set, waits
// until its context is done.
type textHoldingSession struct {
	*kubetest.Session
	hold atomic.Bool
}

func (s *textHoldingSession) YAML(ctx context.Context, ref kube.ObjectRef, managed bool) (string, error) {
	if s.hold.Load() {
		<-ctx.Done()
		return "", ctx.Err()
	}
	return s.Session.YAML(ctx, ref, managed)
}

func TestCloseCancelsTheReading(t *testing.T) {
	sess := &textHoldingSession{Session: textPodsAndSecrets(keys.Grant{Name: "alice@laptop"})}
	sess.YAMLs[textPod.String()] = "kind: Pod\n"
	hs := newHarness(t, sess, 100, 30)
	// run runs cmd in a goroutine and gives its message.
	run := func(cmd tea.Cmd) <-chan tea.Msg {
		out := make(chan tea.Msg, 1)
		go func() { out <- cmd() }()
		return out
	}
	// cancelled waits for a reading to end, which it does only when its
	// context is cancelled.
	cancelled := func(what string, got <-chan tea.Msg) {
		t.Helper()
		select {
		case msg := <-got:
			if m, ok := msg.(textLoadedMsg); !ok || !errors.Is(m.err, context.Canceled) {
				t.Errorf("%s: the reading ended with %#v", what, msg)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: the reading in flight goes on", what)
		}
	}
	hs.press("y")
	v := textTop(hs)
	sess.hold.Store(true)
	_, cmd := v.Update(keyPress("m"))
	first := run(cmd)
	_, cmd = v.Update(keyPress("m"))
	second := run(cmd)
	cancelled("a later reading", first)
	select {
	case msg := <-second:
		t.Fatalf("the later reading ended by itself: %#v", msg)
	default:
	}
	hs.press("q")
	if len(hs.root().stack) != 1 {
		t.Fatalf("q left %d views", len(hs.root().stack))
	}
	cancelled("Close", second)
}
