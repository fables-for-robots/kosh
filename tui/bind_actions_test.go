package tui

import (
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/fables-for-robots/kosh/keys"
	"github.com/fables-for-robots/kosh/kube"
	"github.com/fables-for-robots/kosh/kube/kubetest"
)

// actionKeys are the keys of this part of the UI, as the brief's table
// has them.
var actionKeys = []string{"ctrl+d", "e", "ctrl+k", "s", "r", "i", "ctrl+l", "t", "c", "u"}

// actionOwn returns the bindings of bs that are this part's.
func actionOwn(bs []binding) []binding {
	var out []binding
	for _, b := range bs {
		for _, a := range actionBindings {
			if a.Key == b.Key && a.Order == b.Order && a.Hint == b.Hint {
				out = append(out, b)
			}
		}
	}
	return out
}

// actionOpen opens the table of plural in the harness, with t as its
// rows. Pods are open from the start.
func actionOpen(hs *harness, sess *kubetest.Session, plural string, t kube.Table) kube.Query {
	hs.t.Helper()
	res := kubetest.Resource(plural)
	q := kube.Query{Resource: res}
	if res.Namespaced {
		q.Namespace = "default"
	}
	sess.SetTable(q, t)
	if plural != "pods" {
		hs.command(plural)
	}
	if !strings.HasPrefix(hs.title(), strings.ToUpper(plural[:1])+plural[1:]) {
		hs.t.Fatalf("the title is %q", hs.title())
	}
	return q
}

// actionEntered returns every method the spy saw called, sorted.
func (s *actionSpySession) entered() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Sorted(maps.Keys(s.left))
}

func TestReadOnlyHasNoActionKeys(t *testing.T) {
	ro := keys.Grant{Name: "bob", ReadOnly: true}
	for _, res := range append(kubetest.Standard(), kube.Containers, kube.Rules) {
		if got := actionOwn(bindingsFor(res, ro)); len(got) > 0 {
			t.Errorf("a read-only key has %d keys of actions in %s", len(got), res.GVR.Resource)
		}
	}
	tables := map[string]kube.Table{
		"pods":        podsNamed("web-1"),
		"deployments": simpleTable("web"),
		"nodes":       clusterTable("n1"),
	}
	for _, plural := range []string{"pods", "deployments", "nodes"} {
		sess := actionSpyOf(kubetest.New(ro))
		hs := newHarness(t, sess, 100, 30)
		actionOpen(hs, sess.Session, plural, tables[plural])
		hs.notFrame("Delete", "Kill", "Scale", "Restart", "Drain", "Cordon", "Edit")
		for _, k := range actionKeys {
			hs.press(k)
			if hs.root().dlg != nil {
				t.Fatalf("%s in %s opened a dialog for a read-only key", k, plural)
			}
			if _, ok := hs.root().top().(*tableView); !ok {
				t.Fatalf("%s in %s opened a %T", k, plural, hs.root().top())
			}
		}
		if got := sess.entered(); len(got) > 0 {
			t.Errorf("keys in %s called %v for a read-only key", plural, got)
		}
		for _, c := range sess.Calls() {
			if c.Method != "Watch" && c.Method != "Resolve" {
				t.Errorf("keys in %s called %s for a read-only key", plural, c.Method)
			}
		}
	}
}

func TestActionKeysByKind(t *testing.T) {
	full := keys.Grant{Name: "alice"}
	del := binding{Key: "ctrl+d", Hint: hint{"ctrl-d", "Delete"}, Order: 30}
	edit := binding{Key: "e", Hint: hint{"e", "Edit"}, Order: 31}
	kill := binding{Key: "ctrl+k", Hint: hint{"ctrl-k", "Kill"}, Order: 32}
	scale := binding{Key: "s", Hint: hint{"s", "Scale"}, Order: 33}
	restart := binding{Key: "r", Hint: hint{"r", "Restart"}, Order: 34}
	image := binding{Key: "i", Hint: hint{"i", "Set Image"}, Order: 35}
	rollback := binding{Key: "ctrl+l", Hint: hint{"ctrl-l", "Rollback"}, Order: 36}
	trigger := binding{Key: "t", Hint: hint{"t", "Trigger"}, Order: 37}
	suspend := binding{Key: "s", Hint: hint{"s", "Suspend/Resume"}, Order: 38}
	cordon := binding{Key: "c", Hint: hint{"c", "Cordon"}, Order: 39}
	uncordon := binding{Key: "u", Hint: hint{"u", "Uncordon"}, Order: 40}
	drain := binding{Key: "r", Hint: hint{"r", "Drain"}, Order: 41}

	noDelete := kubetest.Resource("configmaps")
	noDelete.Verbs = []string{"get", "list", "update"}
	noUpdate := kubetest.Resource("configmaps")
	noUpdate.Verbs = []string{"get", "list", "delete"}

	for _, c := range []struct {
		res  kube.Resource
		want []binding
	}{
		{kubetest.Resource("pods"), []binding{del, edit, kill}},
		{kubetest.Resource("deployments"), []binding{del, edit, scale, restart, image}},
		{kubetest.Resource("statefulsets"), []binding{del, edit, scale, restart, image}},
		{kubetest.Resource("daemonsets"), []binding{del, edit, restart, image}},
		{kubetest.Resource("replicasets"), []binding{del, edit, scale, rollback}},
		{kubetest.Resource("cronjobs"), []binding{del, edit, trigger, suspend}},
		{kubetest.Resource("nodes"), []binding{del, edit, cordon, uncordon, drain}},
		{kubetest.Resource("configmaps"), []binding{del, edit}},
		{kubetest.Resource("widgets"), []binding{del, edit}},
		{kube.Containers, nil},
		{kube.Rules, nil},
		{noDelete, []binding{edit}},
		{noUpdate, []binding{del}},
	} {
		got := actionOwn(bindingsFor(c.res, full))
		var gotS, wantS []string
		for _, b := range got {
			gotS = append(gotS, fmt.Sprintf("%s %s %s %d", b.Key, b.Hint.Key, b.Hint.Text, b.Order))
		}
		for _, b := range c.want {
			wantS = append(wantS, fmt.Sprintf("%s %s %s %d", b.Key, b.Hint.Key, b.Hint.Text, b.Order))
		}
		if !slices.Equal(gotS, wantS) {
			t.Errorf("%s (verbs %v) has\n%q, want\n%q", c.res.GVR.Resource, c.res.Verbs, gotS, wantS)
		}
	}
}

func TestNoKeyTwiceInATable(t *testing.T) {
	full := keys.Grant{Name: "alice"}
	for _, res := range append(kubetest.Standard(), kube.Containers, kube.Rules) {
		own := actionOwn(bindingsFor(res, full))
		seen := map[string]bool{}
		for _, b := range own {
			if seen[b.Key] {
				t.Errorf("%s has two keys of actions on %s", res.GVR.Resource, b.Key)
			}
			seen[b.Key] = true
		}
		if err := checkBindings(own); err != nil {
			t.Errorf("%s: %v", res.GVR.Resource, err)
		}
	}
}

func TestConfirmStartsOnCancel(t *testing.T) {
	sess := newSession()
	sess.SetTable(query("pods", "default"), podsNamed("web-1", "web-2"))
	hs := newHarness(t, sess, 100, 30)
	hs.press("ctrl+k")
	hs.wantFrame("Kill pods/default/web-1?")
	if dialogFocused(hs) != "Cancel" {
		t.Errorf("the focus starts on %q", dialogFocused(hs))
	}
	hs.golden()
	hs.press("enter")
	if hs.root().dlg != nil {
		t.Error("enter at once did not close the dialog")
	}
	if got := sess.Called("Kill"); len(got) != 0 {
		t.Errorf("enter at once killed: %v", got)
	}
}

func TestConfirmNeedsTwoKeys(t *testing.T) {
	sess := newSession()
	sess.SetTable(query("pods", "default"), podsNamed("web-1"))
	hs := newHarness(t, sess, 100, 30)
	hs.press("ctrl+k", "y")
	if hs.root().dlg == nil || len(sess.Called("Kill")) != 0 {
		t.Error("y answered the dialog")
	}
	hs.press("esc")
	if hs.root().dlg != nil || len(sess.Called("Kill")) != 0 {
		t.Error("esc did not cancel")
	}
	hs.press("ctrl+k", "right", "enter")
	hs.awaitFrame("Killed pods/default/web-1")
	if got := sess.Called("Kill"); len(got) != 1 || got[0].Args[0].(kube.ObjectRef).String() != "pods/default/web-1" {
		t.Errorf("Kill was called %v", got)
	}
}

func TestDeleteDialog(t *testing.T) {
	sess := newSession()
	sess.SetTable(query("pods", "default"), podsNamed("web-1"))
	hs := newHarness(t, sess, 100, 30)
	hs.press("ctrl+d")
	hs.wantFrame("Delete pods/default/web-1?")
	if dialogFocused(hs) != "Propagation" {
		t.Errorf("the focus starts on %q", dialogFocused(hs))
	}
	hs.golden()
	dialogFocusOn(hs, "Delete")
	hs.press("enter")
	hs.awaitFrame("Deleted pods/default/web-1")

	hs.press("ctrl+d", "space", "tab", "space")
	dialogFocusOn(hs, "Delete")
	hs.press("enter")
	hs.awaitFrame("Deleted pods/default/web-1")

	// Cancel, and esc.
	hs.press("ctrl+d")
	dialogFocusOn(hs, "Cancel")
	hs.press("enter", "ctrl+d", "esc")

	calls := sess.Called("Delete")
	want := []kube.DeleteOptions{
		{Propagation: kube.PropagationBackground},
		{Propagation: kube.PropagationForeground, Force: true},
	}
	if len(calls) != len(want) {
		t.Fatalf("Delete was called %d times", len(calls))
	}
	for i, c := range calls {
		if c.Args[0].(kube.ObjectRef).String() != "pods/default/web-1" || c.Args[1] != want[i] {
			t.Errorf("Delete %d: %v, want %v", i, c.Args, want[i])
		}
	}
}

func TestScaleDialog(t *testing.T) {
	sess := newSession()
	sess.ReplicasOf["deployments/default/web"] = 3
	hs := newHarness(t, sess, 100, 30)
	actionOpen(hs, sess, "deployments", simpleTable("web"))
	hs.press("s")
	hs.wantFrame("Scale deployments/default/web?")
	d := dialogOpen(hs)
	if d == nil || d.field("Replicas").value() != "3" {
		t.Fatal("the field does not start with the replicas")
	}
	hs.press("backspace")
	hs.typeText("5abc")
	if got := d.field("Replicas").value(); got != "5" {
		t.Errorf("the field is %q after 5abc", got)
	}
	dialogFocusOn(hs, "Scale")
	hs.press("enter")
	hs.awaitFrame("Scaled deployments/default/web")
	if got := sess.Called("Scale"); len(got) != 1 || got[0].Args[1] != int32(5) {
		t.Errorf("Scale was called %v", got)
	}

	// An empty field says why and stays.
	hs.press("s", "backspace")
	dialogFocusOn(hs, "Scale")
	hs.press("enter")
	if dialogOpen(hs) == nil {
		t.Fatal("an empty field closed the dialog")
	}
	hs.wantFrame("Replicas must be a number")
	hs.press("esc")
	if got := sess.Called("Scale"); len(got) != 1 {
		t.Errorf("Scale was called %v", got)
	}

	// An error reading the replicas is a flash.
	sess.Errs["Replicas"] = errors.New("deployments.apps \"web\" not found")
	hs.press("s")
	if hs.root().dlg != nil {
		t.Error("a dialog opened after an error")
	}
	hs.wantFrame(`deployments.apps "web" not found`)
}

func TestSetImageDialog(t *testing.T) {
	sess := newSession()
	sess.ImagesOf["deployments/default/web"] = []kube.ContainerImage{
		{Name: "setup", Image: "busybox:1.36", Init: true},
		{Name: "app", Image: "nginx:1.27"},
		{Name: "sidecar", Image: "envoyproxy/envoy:v1.31"},
	}
	hs := newHarness(t, sess, 100, 30)
	actionOpen(hs, sess, "deployments", simpleTable("web"))
	hs.press("i")
	hs.wantFrame("setup (init)", "busybox:1.36", "app", "nginx:1.27", "sidecar")
	hs.golden()
	hs.press("tab", "ctrl+u")
	hs.typeText("nginx:1.28")
	dialogFocusOn(hs, "Set")
	hs.press("enter")
	hs.awaitFrame("Set the images of deployments/default/web")
	calls := sess.Called("SetImages")
	if len(calls) != 1 || !reflect.DeepEqual(calls[0].Args[1], map[string]string{"app": "nginx:1.28"}) {
		t.Errorf("SetImages was called %v", calls)
	}

	// None changed.
	hs.press("i", "tab", "backspace")
	hs.typeText("7")
	dialogFocusOn(hs, "Set")
	hs.press("enter")
	if hs.root().dlg != nil {
		t.Error("the dialog is open")
	}
	hs.wantFrame("nothing changed")
	if got := sess.Called("SetImages"); len(got) != 1 {
		t.Errorf("SetImages was called %v", got)
	}

	sess.Errs["Images"] = errors.New("forbidden")
	hs.press("i")
	if hs.root().dlg != nil {
		t.Error("a dialog opened after an error")
	}
	hs.wantFrame("forbidden")
}

func TestDrainDialog(t *testing.T) {
	sess := actionSpy()
	hs := newHarness(t, sess, 100, 30)
	actionOpen(hs, sess.Session, "nodes", clusterTable("n1", "n2"))
	hs.press("r")
	hs.wantFrame("Drain nodes/n1?", "Grace Period", "-1", "Timeout", "5s", "Ignore DaemonSets", "Delete EmptyDir Data", "Force", "Disable Eviction")
	hs.golden()
	dialogFocusOn(hs, "Drain")
	hs.press("enter")
	hs.awaitFrame("Drained nodes/n1")

	// A timeout that is not a duration.
	hs.press("r", "tab", "ctrl+u")
	hs.typeText("abc")
	dialogFocusOn(hs, "Drain")
	hs.press("enter")
	if dialogOpen(hs) == nil {
		t.Fatal("a timeout of abc closed the dialog")
	}
	hs.wantFrame("Timeout must be a duration")
	hs.press("esc")

	// A drain cannot be stopped once it runs: no timeout, or one above
	// an hour, says why and stays.
	for _, bad := range []string{"0", "0s", "61m", "2h"} {
		hs.press("r", "tab", "ctrl+u")
		hs.typeText(bad)
		dialogFocusOn(hs, "Drain")
		hs.press("enter")
		if dialogOpen(hs) == nil {
			t.Fatalf("a timeout of %s closed the dialog", bad)
		}
		if why := dialogOpen(hs).why; why != "a drain cannot be stopped once it runs: give it a timeout, at most 1h" {
			t.Errorf("a timeout of %s: the dialog says %q", bad, why)
		}
		hs.wantFrame("a drain cannot be stopped once it runs:", "at most 1h")
		hs.press("esc")
	}

	// Two boxes, a grace period of 30, and a timeout of an hour.
	hs.press("r", "ctrl+u")
	hs.typeText("30")
	hs.press("tab", "ctrl+u")
	hs.typeText("1h")
	hs.press("tab", "space", "tab", "space")
	dialogFocusOn(hs, "Drain")
	hs.press("enter")
	hs.awaitFrame("Drained nodes/n1")

	calls := sess.Called("Drain")
	want := []kube.DrainOptions{
		kube.DefaultDrainOptions(),
		{GracePeriod: 30, Timeout: time.Hour, IgnoreDaemonSets: true, DeleteEmptyDirData: true},
	}
	if len(calls) != len(want) {
		t.Fatalf("Drain was called %d times", len(calls))
	}
	for i, c := range calls {
		if c.Args[0].(kube.ObjectRef).String() != "nodes/n1" || c.Args[1] != want[i] {
			t.Errorf("Drain %d: %v, want %v", i, c.Args, want[i])
		}
	}
	// The context: the timeout and apiTimeout.
	left := sess.leftOf("Drain")
	if len(left) != 2 || left[0] <= apiTimeout || left[0] > apiTimeout+5*time.Second ||
		left[1] <= time.Hour+apiTimeout-5*time.Second || left[1] > time.Hour+apiTimeout {
		t.Errorf("Drain's contexts had %v left", left)
	}
}

func TestSuspendResume(t *testing.T) {
	sess := newSession()
	hs := newHarness(t, sess, 100, 30)
	actionOpen(hs, sess, "cronjobs", simpleTable("backup"))
	hs.press("s")
	hs.wantFrame("Suspend", "Resume")
	if dialogFocused(hs) != "Cancel" {
		t.Errorf("the focus starts on %q", dialogFocused(hs))
	}
	hs.press("right", "enter")
	hs.awaitFrame("Suspended cronjobs/default/backup")
	hs.press("s", "right", "right", "enter")
	hs.awaitFrame("Resumed cronjobs/default/backup")
	calls := sess.Called("SetSuspended")
	if len(calls) != 2 || calls[0].Args[1] != true || calls[1].Args[1] != false {
		t.Errorf("SetSuspended was called %v", calls)
	}
}

func TestRestartRollbackTriggerCordon(t *testing.T) {
	for _, c := range []struct {
		key, plural string
		rows        kube.Table
		method      string
		ref         kube.ObjectRef
		on          any // the second argument, when there is one
		flash       string
	}{
		{"r", "deployments", simpleTable("web"), "Restart", kubetest.Ref("deployments", "default", "web"), nil, "Restarted deployments/default/web"},
		{"r", "statefulsets", simpleTable("db"), "Restart", kubetest.Ref("statefulsets", "default", "db"), nil, "Restarted statefulsets/default/db"},
		{"r", "daemonsets", simpleTable("agent"), "Restart", kubetest.Ref("daemonsets", "default", "agent"), nil, "Restarted daemonsets/default/agent"},
		{"ctrl+l", "replicasets", simpleTable("web-1"), "Rollback", kubetest.Ref("replicasets", "default", "web-1"), nil, "Rolled back to replicasets/default/web-1"},
		{"t", "cronjobs", simpleTable("backup"), "Trigger", kubetest.Ref("cronjobs", "default", "backup"), nil, "Triggered cronjobs/default/backup as job manual-job"},
		{"c", "nodes", clusterTable("n1"), "Cordon", kubetest.Ref("nodes", "", "n1"), true, "Cordoned nodes/n1"},
		{"u", "nodes", clusterTable("n1"), "Cordon", kubetest.Ref("nodes", "", "n1"), false, "Uncordoned nodes/n1"},
	} {
		t.Run(c.key+" "+c.plural, func(t *testing.T) {
			sess := newSession()
			hs := newHarness(t, sess, 100, 30)
			actionOpen(hs, sess, c.plural, c.rows)
			hs.press(c.key)
			if dialogFocused(hs) != "Cancel" {
				t.Errorf("the focus starts on %q", dialogFocused(hs))
			}
			hs.press("right", "enter")
			hs.awaitFrame(c.flash)
			calls := sess.Called(c.method)
			if len(calls) != 1 || calls[0].Args[0].(kube.ObjectRef).String() != c.ref.String() || calls[0].Args[0].(kube.ObjectRef).Resource.GVR != c.ref.Resource.GVR {
				t.Fatalf("%s was called %v", c.method, calls)
			}
			if c.on != nil && calls[0].Args[1] != c.on {
				t.Errorf("%s was called with %v", c.method, calls[0].Args[1])
			}
		})
	}
}

// A key of an action applies its dialog to every marked row but those
// whose dialog is for the first row.
func TestActionOverMarkedRowsOfAKind(t *testing.T) {
	sess := newSession()
	sess.ReplicasOf["deployments/default/a"] = 2
	hs := newHarness(t, sess, 100, 30)
	actionOpen(hs, sess, "deployments", simpleTable("a", "b", "c"))
	actionMarkAll(hs)
	hs.press("s")
	hs.wantFrame("Scale 3 deployments?", "default/a", "default/c")
	dialogFocusOn(hs, "Scale")
	hs.press("enter")
	hs.awaitFrame("Scaled 3 deployments")
	var got []string
	for _, c := range sess.Called("Scale") {
		got = append(got, c.Args[0].(kube.ObjectRef).Name)
		if c.Args[1] != int32(2) {
			t.Errorf("Scale was called with %v", c.Args[1])
		}
	}
	if !slices.Equal(got, []string{"a", "b", "c"}) {
		t.Errorf("Scale was called for %v", got)
	}
}

// The keys whose dialog or editor is for the first row say so when more
// rows are marked; scale acts on them all and lists them.
func TestFirstOfTheMarkedRows(t *testing.T) {
	sess := newSession()
	sess.ImagesOf["deployments/default/a"] = []kube.ContainerImage{{Name: "app", Image: "nginx:1"}}
	sess.YAMLs["deployments/default/a"] = "a: b\n"
	hs := newHarness(t, sess, 100, 30)
	actionOpen(hs, sess, "deployments", simpleTable("a", "b", "c"))
	hs.press("i")
	hs.wantFrame("Set the images of deployments/default/a?")
	hs.notFrame("the first of")
	hs.press("esc")
	actionMarkAll(hs)
	hs.press("i")
	if q := dialogOpen(hs).question; q != "Set the images of deployments/default/a (the first of 3 marked)?" {
		t.Errorf("the question is %q", q)
	}
	hs.wantFrame("Set the images of deployments/default/a (the first of 3", "marked)?")
	hs.press("esc", "e")
	if got := hs.title(); got != "Edit(deployments/default/a) (the first of 3 marked)" {
		t.Errorf("the editor's title is %q", got)
	}
	hs.typeText("x")
	if got := hs.title(); got != "Edit(deployments/default/a) (the first of 3 marked) [modified]" {
		t.Errorf("the editor's title is %q", got)
	}
	hs.press("esc", "right", "enter", "s")
	hs.wantFrame("Scale 3 deployments?")
	hs.notFrame("the first of")
	hs.press("esc", "ctrl+\\", "e")
	if got := hs.title(); got != "Edit(deployments/default/c)" {
		t.Errorf("the editor's title is %q", got)
	}
}

// The read before a dialog or the editor holds the session's keys of
// actions as an action does, until the dialog or editor has opened or
// the read has failed.
func TestReadBeforeADialogIsAnAction(t *testing.T) {
	for _, c := range []struct {
		key, plural, method string
	}{
		{"s", "deployments", "Replicas"},
		{"i", "deployments", "Images"},
		{"e", "deployments", "YAML"},
	} {
		t.Run(c.key, func(t *testing.T) {
			sess := newSession()
			ref := kubetest.Ref(c.plural, "default", "web")
			sess.ReplicasOf[ref.String()] = 2
			sess.ImagesOf[ref.String()] = []kube.ContainerImage{{Name: "app", Image: "nginx:1"}}
			sess.YAMLs[ref.String()] = "a: b\n"
			hs := newHarness(t, sess, 100, 30)
			q := actionOpen(hs, sess, c.plural, simpleTable("web"))
			var b binding
			for _, x := range bindingsFor(q.Resource, sess.Grant()) {
				if x.Key == c.key {
					b = x
				}
			}
			e := env{sess: sess, now: hs.clock.now}
			opened := func() bool {
				_, editor := hs.root().top().(*editorView)
				return hs.root().dlg != nil || editor
			}

			// The read has begun and not come back: a key flashes.
			read := b.Run(e, q, []kube.ObjectRef{ref})
			hs.press(c.key)
			hs.wantFrame(actionTextBusy)
			if opened() || len(sess.Called(c.method)) != 0 {
				t.Fatal("a second key read or opened something")
			}
			// It comes back: the dialog or the editor opens, and the keys
			// are free.
			hs.send(read())
			if !opened() {
				t.Fatal("nothing opened")
			}
			hs.press("esc")
			if opened() {
				t.Fatal("esc did not close it")
			}
			hs.press(c.key)
			if !opened() {
				t.Fatal("the key is still held after the dialog opened")
			}
			hs.press("esc")

			// A read that fails frees them too.
			sess.Errs[c.method] = errors.New("refused")
			read = b.Run(e, q, []kube.ObjectRef{ref})
			hs.send(read())
			hs.wantFrame("refused")
			delete(sess.Errs, c.method)
			hs.press(c.key)
			if !opened() {
				t.Fatal("the key is still held after a read failed")
			}
		})
	}
}
