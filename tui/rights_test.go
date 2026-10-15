package tui

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/fables-for-robots/kosh/keys"
	"github.com/fables-for-robots/kosh/kube"
	"github.com/fables-for-robots/kosh/kube/kubetest"
)

// carolGrant is a key whose role is the Role edit of the namespace
// team-a.
var carolGrant = keys.Grant{Name: "carol/edit@team-a", Fingerprint: "SHA256:carol", Role: keys.Role{Kind: keys.KindRole, Namespace: "team-a", Name: "edit"}}

// rightsKeys returns the texts of the hints of the top table's bindings.
func rightsKeys(hs *harness) []string {
	hs.t.Helper()
	var out []string
	for _, b := range hs.table().bindings() {
		out = append(out, b.Hint.Text)
	}
	return out
}

// rightsWant fails the test unless the top table binds every key of has
// and none of hasNot, by their hints' texts.
func rightsWant(t *testing.T, hs *harness, has, hasNot []string) {
	t.Helper()
	got := rightsKeys(hs)
	for _, k := range has {
		if !slices.Contains(got, k) {
			t.Errorf("%s has not got %s: %q", hs.title(), k, got)
		}
	}
	for _, k := range hasNot {
		if slices.Contains(got, k) {
			t.Errorf("%s has %s: %q", hs.title(), k, got)
		}
	}
}

// rightsNeed writes a need as the needs table of the spec has it: the
// verb, and the resource with its group, or "·" for the table's, with
// /sub.
func rightsNeed(n need) string {
	res := "·"
	if n.Resource != nil {
		res = n.Resource.GVR.GroupResource().String()
	}
	if n.Sub != "" {
		res += "/" + n.Sub
	}
	return n.Verb + " " + res
}

// Every key of a table needs what the needs table of the roles' spec (§5)
// says, and no key is without needs.
func TestNeedsAreTheSpecs(t *testing.T) {
	want := map[string][]string{
		"Delete":         {"delete ·"},
		"Edit":           {"get ·", "update ·"},
		"Kill":           {"delete pods"},
		"Scale":          {"get ·/scale", "update ·/scale"},
		"Restart":        {"patch ·"},
		"Set Image":      {"patch ·"},
		"Rollback":       {"get replicasets.apps", "patch deployments.apps"},
		"Trigger":        {"get cronjobs.batch", "create jobs.batch"},
		"Suspend/Resume": {"patch cronjobs.batch"},
		"Cordon":         {"patch nodes"},
		"Uncordon":       {"patch nodes"},
		"Drain":          {"patch nodes", "list pods", "create pods/eviction"},
		"Shell":          {"get pods", "create pods/exec"},
		"Logs":           {"get pods/log"},
		"Logs Previous":  {"get pods/log"},
		"Decode":         {"get secrets"},
		"Describe":       {"get ·"},
		"YAML":           {"get ·"},
	}
	seen := map[string]bool{}
	for _, b := range bindings {
		w, ok := want[b.Hint.Text]
		if !ok {
			t.Errorf("the key %s (%s) is not in the needs table", b.Key, b.Hint.Text)
			continue
		}
		seen[b.Hint.Text] = true
		var got []string
		for _, n := range b.Needs {
			got = append(got, rightsNeed(n))
		}
		if !slices.Equal(got, w) {
			t.Errorf("%s needs %q, want %q", b.Hint.Text, got, w)
		}
	}
	for k := range want {
		if !seen[k] {
			t.Errorf("no key is %s", k)
		}
	}
}

// The keys of a table are those whose needs Can says yes to, for the
// table's resource in its namespace.
func TestActionKeysFollowCan(t *testing.T) {
	t.Run("delete on pods in default", func(t *testing.T) {
		sess := kubetest.New(viewGrant)
		allowView(sess)
		sess.Allow("delete", "", "pods", "", "default")
		sess.SetTable(query("pods", "default"), podsNamed("web-1"))
		sess.SetTable(query("pods", "kube-system"), podsNamed("dns"))
		hs := newHarness(t, sess, 200, 30)
		rightsWant(t, hs, []string{"Delete", "Kill", "Describe", "YAML", "Logs"}, []string{"Edit", "Shell"})
		hs.wantHint("ctrl-d", "Delete")
		hs.press("ctrl+d")
		if hs.root().dlg == nil {
			t.Error("ctrl+d opened no dialog")
		}
		hs.press("esc")
		// The right is of default alone.
		hs.command("pods kube-system")
		rightsWant(t, hs, []string{"Describe"}, []string{"Delete", "Kill"})
		hs.notFrame("Delete", "Kill")
		hs.press("ctrl+d")
		if hs.root().dlg != nil {
			t.Error("ctrl+d opened a dialog in kube-system")
		}
	})
	t.Run("not allowed", func(t *testing.T) {
		sess := kubetest.New(viewGrant)
		allowView(sess)
		sess.SetTable(query("pods", "default"), podsNamed("web-1"))
		hs := newHarness(t, sess, 200, 30)
		rightsWant(t, hs, []string{"Describe", "YAML", "Logs", "Logs Previous"}, []string{"Delete", "Edit", "Kill", "Shell"})
		hs.notFrame("Delete", "Edit", "Kill")
		hs.press("ctrl+d", "e", "ctrl+k")
		if hs.root().dlg != nil {
			t.Error("a key opened a dialog")
		}
	})
	t.Run("x needs get on secrets", func(t *testing.T) {
		for _, get := range []bool{false, true} {
			sess := kubetest.New(viewGrant)
			allowView(sess)
			sess.Allow("list", "", "secrets", "", "")
			sess.Allow("watch", "", "secrets", "", "")
			if get {
				sess.Allow("get", "", "secrets", "", "default")
			}
			sess.SetTable(query("secrets", "default"), simpleTable("creds"))
			hs := newHarness(t, sess, 200, 30)
			hs.command("secrets")
			if get {
				rightsWant(t, hs, []string{"Describe", "YAML", "Decode"}, nil)
				hs.wantHint("x", "Decode")
			} else {
				rightsWant(t, hs, nil, []string{"Describe", "YAML", "Decode"})
			}
		}
	})
	t.Run("s needs get on pods and create on pods/exec", func(t *testing.T) {
		for _, c := range []struct {
			name  string
			allow func(*kubetest.Session)
			shell bool
		}{
			{"view", allowView, false},
			{"view and exec", func(s *kubetest.Session) {
				allowView(s)
				s.Allow("create", "", "pods", "exec", "default")
			}, true},
			{"exec without get", func(s *kubetest.Session) {
				s.Allow("list", "", "pods", "", "")
				s.Allow("watch", "", "pods", "", "")
				s.Allow("create", "", "pods", "exec", "")
			}, false},
		} {
			sess := kubetest.New(viewGrant)
			c.allow(sess)
			sess.SetTable(query("pods", "default"), podsNamed("web"))
			hs := newShellHarness(t, sess, newShellTerm(), 200, 30)
			if got := slices.Contains(rightsKeys(hs), "Shell"); got != c.shell {
				t.Errorf("%s: the pods have Shell %v, want %v: %q", c.name, got, c.shell, rightsKeys(hs))
			}
		}
	})
	t.Run("l needs get on pods/log", func(t *testing.T) {
		for _, logs := range []bool{false, true} {
			sess := kubetest.New(viewGrant)
			for _, r := range []string{"pods", "deployments"} {
				group := kubetest.Resource(r).GVR.Group
				sess.Allow("get", group, r, "", "")
				sess.Allow("list", group, r, "", "")
				sess.Allow("watch", group, r, "", "")
			}
			if logs {
				sess.Allow("get", "", "pods", "log", "")
			}
			sess.SetTable(query("pods", "default"), podsNamed("web"))
			sess.SetTable(query("deployments", "default"), simpleTable("web"))
			hs := newHarness(t, sess, 200, 30)
			want := func(keys ...string) {
				t.Helper()
				if logs {
					rightsWant(t, hs, keys, nil)
				} else {
					rightsWant(t, hs, []string{"Describe"}, keys)
				}
			}
			want("Logs", "Logs Previous")
			// A deployment's logs are its pods'.
			hs.command("deployments")
			want("Logs")
		}
	})
}

// Until the answers of Can are in, a table binds no key that needs one
// and shows no hint of it; then it binds the keys they allow.
func TestNoKeysUntilCanAnswers(t *testing.T) {
	sess := newSession()
	sess.SetTable(query("pods", "default"), podsNamed("web-1"))
	sess.SetTable(query("deployments", "default"), simpleTable("web"))
	sess.ReplicasOf[kubetest.Ref("deployments", "default", "web").String()] = 2
	hs := newHarness(t, sess, 200, 30)
	hs.hold = func(msg tea.Msg) bool { _, ok := msg.(rightsMsg); return ok }
	hs.command("deployments")
	hs.wantFrame("web")
	if len(hs.held) != 1 {
		t.Fatalf("%d answers are held", len(hs.held))
	}
	if got := rightsKeys(hs); len(got) > 0 {
		t.Errorf("the deployments bind %q before the answers", got)
	}
	hs.notFrame("Delete", "Edit", "Scale", "Restart", "Describe", "YAML", "Logs")
	hs.press("s", "ctrl+d", "d")
	if hs.root().dlg != nil || len(hs.root().stack) != 1 {
		t.Error("a key did something before the answers")
	}
	hs.press("?")
	hs.notFrame("Delete", "Scale", "Describe")
	hs.press("esc")

	hs.release(hs.held...)
	rightsWant(t, hs, []string{"Delete", "Edit", "Scale", "Restart", "Set Image", "Describe", "YAML", "Logs"}, nil)
	hs.wantHint("ctrl-d", "Delete")
	hs.press("s")
	if hs.root().dlg == nil {
		t.Error("s opened no dialog after the answers")
	}
}

// A table whose namespace changes asks again, and drops the answers of
// the namespace it left when they come late.
func TestNamespaceSwitchAsksAgain(t *testing.T) {
	sess := kubetest.New(carolGrant)
	sess.Allow("*", "*", "*", "*", "team-a")
	sess.SetTable(query("pods", "team-a"), podsNamed("web-1"))
	sess.SetTable(query("pods", "default"), podsNamed("other"))
	hs := newHarness(t, sess, 200, 30)
	if hs.title() != "Pods(team-a)[1]" {
		t.Fatalf("the title is %q", hs.title())
	}
	rightsWant(t, hs, []string{"Delete", "Edit", "Kill", "Describe"}, nil)

	hs.command("pods default")
	if hs.title() != "Pods(default)[1]" {
		t.Fatalf("the title is %q", hs.title())
	}
	if got := rightsKeys(hs); len(got) > 0 {
		t.Errorf("an edit@team-a key has %q in default", got)
	}
	hs.notFrame("Delete")

	// The shortcut of team-a switches this table back, and it asks again.
	hs.press("1")
	if hs.title() != "Pods(team-a)[1]" {
		t.Fatalf("the title is %q", hs.title())
	}
	rightsWant(t, hs, []string{"Delete", "Edit", "Kill"}, nil)

	// The answers of team-a held back while the table goes to all of
	// them: when they come after those of all, they are dropped.
	hs.hold = func(msg tea.Msg) bool { _, ok := msg.(rightsMsg); return ok }
	hs.press("1") // team-a again: answers held
	hs.press("0") // all: answers held
	if len(hs.held) != 2 {
		t.Fatalf("%d answers are held", len(hs.held))
	}
	if got := rightsKeys(hs); len(got) > 0 {
		t.Errorf("the table binds %q before its answers", got)
	}
	teamA, all := hs.held[0], hs.held[1]
	hs.release(all, teamA)
	if got := rightsKeys(hs); len(got) > 0 {
		t.Errorf("all the namespaces bind %q for an edit@team-a key", got)
	}
}

// A key of a Role starts in the Role's namespace, whose shortcut is 1;
// default is no shortcut until it is visited.
func TestRoleKeyStartsInItsNamespace(t *testing.T) {
	sess := kubetest.New(carolGrant)
	sess.Allow("*", "*", "*", "*", "team-a")
	sess.SetTable(query("pods", "team-a"), podsNamed("web-1"))
	hs := newHarness(t, sess, 200, 30)
	if hs.title() != "Pods(team-a)[1]" {
		t.Errorf("the first table is %q", hs.title())
	}
	if w := sess.Called("Watch"); len(w) == 0 || w[0].Args[0].(kube.Query).Namespace != "team-a" {
		t.Errorf("the first watch is %v", w)
	}
	f := hs.frame()
	if !showsHint(f, "0", "all") || !showsHint(f, "1", "team-a") {
		t.Errorf("the shortcuts are not <0> all <1> team-a:\n%s", f)
	}
	hs.notFrame("default")

	hs.command("pods kube-system")
	hs.command("pods default")
	f = hs.frame()
	if !showsHint(f, "1", "team-a") || !showsHint(f, "2", "default") || !showsHint(f, "3", "kube-system") {
		t.Errorf("the shortcuts after kube-system and default:\n%s", f)
	}
	hs.command("pods team-a")
	f = hs.frame()
	if !showsHint(f, "1", "team-a") || !showsHint(f, "2", "default") || showsHint(f, "4", "team-a") {
		t.Errorf("team-a is not the fixed shortcut:\n%s", f)
	}

	// A key of a ClusterRole starts in default, as before.
	hs = newHarness(t, newSession(), 200, 30)
	if f := hs.frame(); !strings.Contains(hs.title(), "Pods(default)") || !showsHint(f, "1", "default") {
		t.Errorf("a cluster-admin key starts in %q:\n%s", hs.title(), f)
	}
}

// The help of a table lists the keys it binds, and no other.
func TestHelpListsOnlyBoundKeys(t *testing.T) {
	sess := kubetest.New(viewGrant)
	allowView(sess)
	sess.SetTable(query("pods", "default"), podsNamed("web-1"))
	hs := newShellHarness(t, sess, newShellTerm(), 120, 40)
	hs.press("?")
	hs.wantFrame("Describe", "YAML", "Logs Previous")
	hs.notFrame("Delete", "Edit", "Kill", "Shell")
}

// viewKeyHarness opens a session of a view key on two pods in default,
// with a terminal to hand over.
func viewKeyHarness(t *testing.T, w, h int) *harness {
	sess := kubetest.New(viewGrant)
	allowView(sess)
	sess.SetTable(query("pods", "default"), podsNamed("web-1", "web-2"))
	return newShellHarness(t, sess, newShellTerm(), w, h)
}

// The table of pods of a view key: the keys of reading alone.
func TestViewKeyPodsFrame(t *testing.T) {
	viewKeyHarness(t, 160, 16).golden()
}

// The help of a view key's table of pods.
func TestViewKeyHelpFrame(t *testing.T) {
	hs := viewKeyHarness(t, 120, 40)
	hs.press("?")
	hs.golden()
}

// askingSession is a session that records the rights Can is asked, and
// answers every one no while down is set, as when the API does not
// answer.
type askingSession struct {
	*kubetest.Session
	mu    sync.Mutex
	asked []right
	down  bool
}

func (s *askingSession) Can(ctx context.Context, verb string, res kube.Resource, sub, ns string) bool {
	s.mu.Lock()
	s.asked = append(s.asked, right{verb: verb, group: res.GVR.Group, resource: res.GVR.Resource, sub: sub})
	down := s.down
	s.mu.Unlock()
	return !down && s.Session.Can(ctx, verb, res, sub, ns)
}

// takeAsked returns the rights asked since the last call.
func (s *askingSession) takeAsked() []right {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.asked
	s.asked = nil
	return out
}

// A table asks each right once, however many keys need it: Delete and
// Kill both need delete on pods in a table of pods, d, y and Shell get.
func TestEachRightIsAskedOnce(t *testing.T) {
	sess := &askingSession{Session: newSession()}
	sess.SetTable(query("pods", "default"), podsNamed("web-1"))
	hs := newShellHarness(t, sess, newShellTerm(), 200, 30)
	rightsWant(t, hs, []string{"Delete", "Kill", "Edit", "Describe", "Shell", "Logs"}, nil)
	asked := sess.takeAsked()
	want := []right{
		{verb: "delete", resource: "pods"},
		{verb: "get", resource: "pods"},
		{verb: "update", resource: "pods"},
		{verb: "get", resource: "pods", sub: "log"},
		{verb: "create", resource: "pods", sub: "exec"},
	}
	for _, r := range want {
		if n := len(slices.DeleteFunc(slices.Clone(asked), func(a right) bool { return a != r })); n != 1 {
			t.Errorf("%+v was asked %d times", r, n)
		}
	}
	if len(asked) != len(want) {
		t.Errorf("asked %+v, want %+v", asked, want)
	}
}

// A help that is open when the answers come shows the keys they bind.
func TestHelpShowsKeysBoundAfterItOpened(t *testing.T) {
	sess := newSession()
	sess.SetTable(query("pods", "default"), podsNamed("web-1"))
	sess.SetTable(query("deployments", "default"), simpleTable("web"))
	hs := newHarness(t, sess, 120, 40)
	hs.hold = func(msg tea.Msg) bool { _, ok := msg.(rightsMsg); return ok }
	hs.command("deployments")
	hs.press("?")
	if hs.title() != "Help" {
		t.Fatalf("the title is %q", hs.title())
	}
	hs.notFrame("Delete", "Scale")
	hs.release(hs.held...)
	hs.wantFrame("Delete", "Scale", "Describe")
}

// ctrl-r asks the rights again: a question the API did not answer was
// answered no, and the keys come when it answers. The keys the table
// has stay while it asks.
func TestReadAgainAsksTheRightsAgain(t *testing.T) {
	sess := &askingSession{Session: newSession(), down: true}
	sess.SetTable(query("pods", "default"), podsNamed("web-1"))
	hs := newHarness(t, sess, 200, 30)
	if got := rightsKeys(hs); len(got) > 0 {
		t.Errorf("the pods bind %q when the API did not answer", got)
	}
	sess.mu.Lock()
	sess.down = false
	sess.mu.Unlock()
	hs.press("ctrl+r")
	rightsWant(t, hs, []string{"Delete", "Kill", "Describe"}, nil)

	hs.hold = func(msg tea.Msg) bool { _, ok := msg.(rightsMsg); return ok }
	hs.press("ctrl+r")
	if len(hs.held) != 1 {
		t.Fatalf("%d answers are held", len(hs.held))
	}
	rightsWant(t, hs, []string{"Delete", "Kill", "Describe"}, nil)
	hs.release(hs.held...)
	rightsWant(t, hs, []string{"Delete", "Kill", "Describe"}, nil)

	// Each time, the session forgot the answers it keeps to the table's
	// questions: of pods (and of pods/log, pods/exec), in default.
	if got, want := sess.Forgotten(), []string{"pods in default", "pods in default"}; !slices.Equal(got, want) {
		t.Errorf("ctrl-r had the session forget %q, want %q", got, want)
	}
}
