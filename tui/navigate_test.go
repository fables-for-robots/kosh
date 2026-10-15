package tui

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/fables-for-robots/kosh/kube"
	"github.com/fables-for-robots/kosh/kube/kubetest"
	"github.com/fables-for-robots/kosh/render"
)

// ran is what a binding of a test was run with.
type ran struct {
	key  string
	q    kube.Query
	refs []kube.ObjectRef
}

// withBindings adds bindings for the test, and takes them away after
// it. Each records its runs in *runs.
func withBindings(t *testing.T, runs *[]ran, bs ...binding) {
	t.Helper()
	saved := slices.Clone(bindings)
	t.Cleanup(func() { bindings = saved })
	for _, b := range bs {
		b := b
		if b.Run == nil {
			b.Run = func(e env, q kube.Query, refs []kube.ObjectRef) tea.Cmd {
				*runs = append(*runs, ran{key: b.Key, q: q, refs: refs})
				return nil
			}
		}
		bind(b)
	}
}

// lastWatch returns the query of the last call of Watch.
func lastWatch(t *testing.T, sess *kubetest.Session) kube.Query {
	t.Helper()
	calls := sess.Called("Watch")
	if len(calls) == 0 {
		t.Fatal("Watch was not called")
	}
	return calls[len(calls)-1].Args[0].(kube.Query)
}

// sameQuery reports whether two queries ask for the same rows.
func sameQuery(a, b kube.Query) bool {
	if a.Resource.GVR != b.Resource.GVR || a.Namespace != b.Namespace || (a.Of == nil) != (b.Of == nil) {
		return false
	}
	return a.Of == nil || a.Of.Resource.GVR == b.Of.Resource.GVR && a.Of.Namespace == b.Of.Namespace && a.Of.Name == b.Of.Name
}

// sameRefs reports whether got are the references want.
func sameRefs(got []kube.ObjectRef, want ...kube.ObjectRef) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i].String() != want[i].String() || got[i].Resource.GVR != want[i].Resource.GVR {
			return false
		}
	}
	return true
}

func showQuery(q kube.Query) string {
	s := fmt.Sprintf("%s in %q", q.Resource.GVR.Resource, q.Namespace)
	if q.Of != nil {
		s += " of " + q.Of.String()
	}
	return s
}

func TestNamespaceShortcuts(t *testing.T) {
	sess := newSession()
	for _, ns := range []string{"", "default", "kube-system", "monitoring"} {
		sess.SetTable(query("pods", ns), podTable(pod{ns: "default", name: "in-" + ns}))
	}
	hs := newHarness(t, sess, 100, 30)
	open := func(ns string) {
		t.Helper()
		if got := sess.OpenWatches(); !slices.Equal(got, []string{kubetest.Key(query("pods", ns))}) {
			t.Errorf("the open watches are %q, want the pods' of %q alone", got, ns)
		}
	}
	hs.press("0")
	if hs.title() != "Pods(all)[1]" {
		t.Errorf("0 opens %q", hs.title())
	}
	open("")
	hs.press("1")
	if hs.title() != "Pods(default)[1]" {
		t.Errorf("1 opens %q", hs.title())
	}
	open("default")
	hs.command("po kube-system")
	hs.command("po monitoring")
	hs.wantFrame("<2> monitoring", "<3> kube-system")
	hs.press("3")
	if hs.title() != "Pods(kube-system)[1]" {
		t.Errorf("3 opens %q", hs.title())
	}
	open("kube-system")
	hs.wantFrame("<2> kube-system", "<3> monitoring")
	hs.press("2")
	if hs.title() != "Pods(kube-system)[1]" {
		t.Errorf("2 opens %q", hs.title())
	}
	// A shortcut without a namespace does nothing.
	hs.press("9")
	if hs.title() != "Pods(kube-system)[1]" {
		t.Errorf("9 opens %q", hs.title())
	}
	// The current namespace is the one a command goes to.
	hs.press("3")
	hs.command("deploy")
	if hs.title() != "Deployments(monitoring)[0]" {
		t.Errorf("the deployments are %q", hs.title())
	}
	// Not in a table of a resource without namespaces.
	hs.command("nodes")
	hs.press("2")
	if hs.title() != "Nodes(all)[0]" {
		t.Errorf("2 in the nodes opens %q", hs.title())
	}
}

func TestEnter(t *testing.T) {
	pods := kubetest.Resource("pods")
	jobs := kubetest.Resource("jobs")
	podOf := func(plural, ns string) kube.Query {
		return queryOf(pods, kubetest.Ref(plural, ns, "x"))
	}
	for _, tt := range []struct {
		plural string
		ns     string // of the row
		want   kube.Query
		key    string // of the binding that runs, instead
	}{
		{plural: "deployments", ns: "default", want: podOf("deployments", "default")},
		{plural: "replicasets", ns: "default", want: podOf("replicasets", "default")},
		{plural: "statefulsets", ns: "default", want: podOf("statefulsets", "default")},
		{plural: "daemonsets", ns: "default", want: podOf("daemonsets", "default")},
		{plural: "jobs", ns: "default", want: podOf("jobs", "default")},
		{plural: "services", ns: "default", want: podOf("services", "default")},
		{plural: "nodes", want: podOf("nodes", "")},
		{plural: "cronjobs", ns: "default", want: queryOf(jobs, kubetest.Ref("cronjobs", "default", "x"))},
		{plural: "pods", ns: "default", want: queryOf(kube.Containers, kubetest.Ref("pods", "default", "x"))},
		{plural: "roles", ns: "default", want: queryOf(kube.Rules, kubetest.Ref("roles", "default", "x"))},
		{plural: "clusterroles", want: queryOf(kube.Rules, kubetest.Ref("clusterroles", "", "x"))},
		{plural: "rolebindings", ns: "default", want: queryOf(kube.Rules, kubetest.Ref("rolebindings", "default", "x"))},
		{plural: "clusterrolebindings", want: queryOf(kube.Rules, kubetest.Ref("clusterrolebindings", "", "x"))},
		{plural: "configmaps", ns: "default", key: "d"},
		{plural: "secrets", ns: "default", key: "d"},
		{plural: "widgets", ns: "default", key: "d"},
	} {
		t.Run(tt.plural, func(t *testing.T) {
			var runs []ran
			withBindings(t, &runs, binding{Key: "d", Hint: hint{"d", "Describe"}}, binding{Key: "l", Hint: hint{"l", "Logs"}})
			sess := newSession()
			res := kube.Resource{}
			for _, r := range sess.ResourceList {
				if r.GVR.Resource == tt.plural {
					res = r
				}
			}
			q := kube.Query{Resource: res}
			tb := clusterTable("x")
			if res.Namespaced {
				q.Namespace = "default"
				tb = simpleTable("x")
			}
			sess.SetTable(q, tb)
			hs := newHarness(t, sess, 100, 30)
			hs.command(tt.plural)
			watches := len(sess.Called("Watch"))
			hs.press("enter")
			if tt.key != "" {
				if len(sess.Called("Watch")) != watches {
					t.Errorf("enter opened %s", showQuery(lastWatch(t, sess)))
				}
				want := kube.ObjectRef{Resource: res, Namespace: tt.ns, Name: "x"}
				if len(runs) != 1 || runs[0].key != tt.key || len(runs[0].refs) != 1 || runs[0].refs[0].String() != want.String() {
					t.Errorf("enter ran %+v, want %s with %s", runs, tt.key, want)
				}
				return
			}
			if got := lastWatch(t, sess); !sameQuery(got, tt.want) {
				t.Errorf("enter opened %s, want %s", showQuery(got), showQuery(tt.want))
			}
			if len(hs.root().stack) != 2 {
				t.Errorf("the stack has %d views", len(hs.root().stack))
			}
			if len(runs) != 0 {
				t.Errorf("enter ran %+v", runs)
			}
		})
	}
}

func TestEnterOnAContainer(t *testing.T) {
	var runs []ran
	withBindings(t, &runs, binding{Key: "d", Hint: hint{"d", "Describe"}}, binding{Key: "l", Hint: hint{"l", "Logs"}})
	sess := newSession()
	sess.SetTable(query("pods", "default"), podsNamed("web-1"))
	pod := kubetest.Ref("pods", "default", "web-1")
	cq := queryOf(kube.Containers, pod)
	sess.SetTable(cq, kube.Table{
		Columns: []render.Column{{Name: "NAME"}, {Name: "IMAGE"}},
		Rows:    []render.Row{{Name: "app", Cells: []string{"app", "nginx"}}},
	})
	hs := newHarness(t, sess, 100, 30)
	hs.press("enter")
	if hs.title() != "Containers(pods/default/web-1)[1]" {
		t.Fatalf("the title is %q", hs.title())
	}
	// A container has no owner.
	hs.notFrame("Owner")
	hs.press("J")
	if len(sess.Called("Owner")) != 0 {
		t.Error("the owner of a container was asked for")
	}
	hs.press("enter")
	want := kube.ObjectRef{Resource: kube.Containers, Namespace: "default", Name: "app"}
	if len(runs) != 1 || runs[0].key != "l" || len(runs[0].refs) != 1 || runs[0].refs[0].String() != want.String() || runs[0].refs[0].Resource.GVR != kube.Containers.GVR || !sameQuery(runs[0].q, cq) {
		t.Errorf("enter ran %+v", runs)
	}
}

func TestEnterOnANamespace(t *testing.T) {
	sess := newSession()
	sess.SetTable(query("namespaces", ""), clusterTable("default", "kube-system"))
	sess.SetTable(query("pods", "kube-system"), podTable(pod{ns: "kube-system", name: "dns-1"}))
	hs := newHarness(t, sess, 100, 30)
	hs.send(pushViewMsg{&testView{name: "a"}})
	hs.command("ns")
	hs.press("down", "enter")
	if hs.title() != "Pods(kube-system)[1]" {
		t.Errorf("the title is %q", hs.title())
	}
	if len(hs.root().stack) != 1 {
		t.Errorf("the stack has %d views", len(hs.root().stack))
	}
	if hs.root().ns.current != "kube-system" {
		t.Errorf("the current namespace is %q", hs.root().ns.current)
	}
	hs.wantFrame("<2> kube-system")
	hs.command("deploy")
	if !strings.HasPrefix(hs.title(), "Deployments(kube-system)[") {
		t.Errorf("the title is %q", hs.title())
	}
}

func TestEnterOnACRD(t *testing.T) {
	sess := newSession()
	sess.SetTable(query("customresourcedefinitions", ""), clusterTable("widgets.example.com"))
	widgets := kube.Query{Resource: sess.ResourceList[len(sess.ResourceList)-1]}
	for _, r := range sess.ResourceList {
		if r.GVR.Resource == "widgets" {
			widgets.Resource = r
		}
	}
	widgets.Namespace = "default"
	sess.SetTable(widgets, simpleTable("w-1", "w-2"))
	hs := newHarness(t, sess, 100, 30)
	hs.command("crd")
	hs.press("enter")
	calls := sess.Called("Resolve")
	if last := calls[len(calls)-1]; last.Args[0] != "widgets.example.com" {
		t.Errorf("Resolve was asked for %v", last.Args)
	}
	if hs.title() != "Widgets(default)[2]" {
		t.Errorf("the title is %q", hs.title())
	}
	if len(hs.root().stack) != 2 {
		t.Errorf("the stack has %d views", len(hs.root().stack))
	}
}

func TestOwnerJump(t *testing.T) {
	sess := newSession()
	sess.SetTable(query("pods", "default"), podsNamed("orphan", "web-1"))
	sess.Owners["pods/default/web-1"] = kubetest.Ref("replicasets", "default", "web-7d9")
	sess.SetTable(query("replicasets", "default"), simpleTable("a-rs", "web-7d9", "z-rs"))
	hs := newHarness(t, sess, 100, 30)
	hs.press("J")
	if l := hs.lines()[len(hs.lines())-1]; !strings.Contains(l, "no owner") {
		t.Errorf("the flash is %q", l)
	}
	if l := hs.drawnLine("no owner"); strings.Contains(l, sgr(styleError)) {
		t.Errorf("no owner is drawn as an error: %q", l)
	}
	if len(hs.root().stack) != 1 {
		t.Errorf("the stack has %d views", len(hs.root().stack))
	}
	hs.press("down", "J")
	if hs.title() != "Replicasets(default)[3]" {
		t.Errorf("the title is %q", hs.title())
	}
	if got := hs.cursorRow(); !strings.HasPrefix(got, "web-7d9 ") {
		t.Errorf("the cursor is on %q", got)
	}
	if len(hs.root().stack) != 2 {
		t.Errorf("the stack has %d views", len(hs.root().stack))
	}
}

func TestEscPopsAndClosesTheWatch(t *testing.T) {
	sess := newCountingSession()
	dq := query("deployments", "default")
	sess.SetTable(dq, simpleTable("web"))
	hs := newHarness(t, sess, 100, 30)
	hs.command("deploy")
	hs.press("enter")
	if !strings.HasPrefix(hs.title(), "Pods(deployments/default/web)[") {
		t.Fatalf("the title is %q", hs.title())
	}
	reads := len(sess.readsOf(dq))
	hs.press("esc")
	if got := sess.OpenWatches(); !slices.Equal(got, []string{kubetest.Key(dq)}) {
		t.Errorf("the open watches are %q", got)
	}
	if hs.title() != "Deployments(default)[1]" {
		t.Errorf("the title is %q", hs.title())
	}
	if got := len(sess.readsOf(dq)); got != reads+1 {
		t.Errorf("the deployments were read %d times after esc", got-reads)
	}
}

func TestBindings(t *testing.T) {
	var runs []ran
	withBindings(t, &runs,
		binding{Key: "z", Hint: hint{"z", "Zap"}, Order: 2},
		binding{Key: "Z", Hint: hint{"shift-z", "Zap Hard"}, Order: 1, Needs: []need{{Verb: "delete"}}},
	)
	sess := newSession()
	q := query("pods", "default")
	sess.SetTable(q, podsNamed("a", "b", "c"))
	hs := newHarness(t, sess, 100, 30)
	hs.wantHint("z", "Zap")
	hs.wantHint("shift-z", "Zap Hard")
	hs.press("?")
	hs.wantFrame("<z>", "Zap", "<shift-z>", "Zap Hard")
	hs.press("esc")
	hints := hs.table().Hints()
	at := func(key string) int {
		return slices.IndexFunc(hints, func(h hint) bool { return h.Key == key })
	}
	if at("shift-z") < 0 || at("shift-z") > at("z") || at("?") > at("shift-z") {
		t.Errorf("the hints are not the table's and then the bindings' by Order: %v", hints)
	}
	hs.press("down", "z")
	ref := func(n string) kube.ObjectRef { return kubetest.Ref("pods", "default", n) }
	if len(runs) != 1 || !sameRefs(runs[0].refs, ref("b")) || !sameQuery(runs[0].q, q) {
		t.Errorf("z ran %+v", runs)
	}
	hs.press("space", "down", "down", "space", "up", "Z")
	if len(runs) != 2 || !sameRefs(runs[1].refs, ref("b"), ref("c")) {
		t.Errorf("Z with two rows marked ran %+v", runs[1:])
	}
	// Marked rows that the filter hides are not run.
	hs.press("/")
	hs.typeText("c")
	hs.press("enter", "z")
	if len(runs) != 3 || !sameRefs(runs[2].refs, ref("c")) {
		t.Errorf("z with a marked row hidden ran %+v", runs[2:])
	}
	// Not in an empty table.
	hs.press("/")
	hs.typeText("nothing")
	hs.press("enter", "z")
	if len(runs) != 3 {
		t.Errorf("z in an empty table ran %+v", runs[3:])
	}

	// A key whose role does not allow what a binding needs has not got
	// it.
	runs = nil
	ro := kubetest.New(viewGrant)
	allowView(ro)
	ro.SetTable(q, podsNamed("a"))
	hs = newHarness(t, ro, 120, 30)
	hs.wantHint("z", "Zap")
	hs.notFrame("Zap Hard")
	hs.press("?")
	hs.notFrame("Zap Hard")
	hs.press("esc")
	hs.press("Z")
	if len(runs) != 0 {
		t.Errorf("Z ran for a view key: %+v", runs)
	}
	hs.press("z")
	if len(runs) != 1 {
		t.Errorf("z ran %d times for a view key", len(runs))
	}
}

func TestTableKeysWinOverBindings(t *testing.T) {
	var runs []ran
	withBindings(t, &runs, binding{Key: "j", Hint: hint{"j", "Jump"}})
	err := checkBindings(bindings)
	if err == nil || !strings.Contains(err.Error(), `"j"`) {
		t.Errorf("a binding of j passes the check: %v", err)
	}
	sess := newSession()
	sess.SetTable(query("pods", "default"), podsNamed("a", "b"))
	hs := newHarness(t, sess, 100, 30)
	hs.press("j")
	if len(runs) != 0 {
		t.Errorf("the binding of j ran: %+v", runs)
	}
	if got := hs.cursorRow(); !strings.HasPrefix(got, "b ") {
		t.Errorf("j did not move the cursor: %q", got)
	}
	for _, k := range []string{"enter", "esc", ":", "?", "0", "ctrl+z", "space"} {
		if checkBindings([]binding{{Key: k}}) == nil {
			t.Errorf("a binding of %s passes the check", k)
		}
	}
	// The keys the other parts of the UI bind are free.
	for _, k := range []string{"d", "y", "x", "l", "p", "e", "s", "r", "i", "t", "c", "u", "ctrl+d", "ctrl+k", "ctrl+l"} {
		if err := checkBindings([]binding{{Key: k}}); err != nil {
			t.Errorf("a binding of %s fails the check: %v", k, err)
		}
	}
}

// The bindings the parts of the UI register take no key of the table.
func TestNoBindingTakesATableKey(t *testing.T) {
	if err := checkBindings(bindings); err != nil {
		t.Error(err)
	}
}
