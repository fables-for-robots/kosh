package tui

import (
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode"

	tea "charm.land/bubbletea/v2"

	"github.com/fables-for-robots/kosh/keys"
	"github.com/fables-for-robots/kosh/kube/kubetest"
)

func TestCommandAllNamespaces(t *testing.T) {
	for _, word := range []string{"all", "-A"} {
		t.Run(word, func(t *testing.T) {
			sess := newSession()
			sess.SetTable(query("pods", ""), podTable(pod{ns: "default", name: "web-1"}, pod{ns: "kube-system", name: "coredns-1"}))
			hs := newHarness(t, sess, 100, 30)
			hs.command("pods " + word)
			if hs.title() != "Pods(all)[2]" {
				t.Errorf(":pods %s has the title %q", word, hs.title())
			}
			hs.wantFrame("web-1", "kube-system", "coredns-1")
			if hs.root().ns.current != "" {
				t.Errorf("the session's namespace is %q", hs.root().ns.current)
			}
			// As 0 does: the next command without a namespace is in all.
			hs.command("pods")
			if hs.title() != "Pods(all)[2]" {
				t.Errorf(":pods after :pods %s has the title %q", word, hs.title())
			}
		})
	}
}

// helpTokens returns every key the help view shows, as it names them:
// "j/down" gives j, down and j/down, "0-9" each digit.
func helpTokens(hs *harness) map[string]bool {
	hs.t.Helper()
	out := map[string]bool{}
	for _, m := range regexp.MustCompile(`<([^ >]+|>)>`).FindAllStringSubmatch(hs.frame(), -1) {
		tok := m[1]
		out[tok] = true
		for _, part := range strings.Split(tok, "/") {
			out[part] = true
		}
		if tok == "0-9" {
			for _, d := range "0123456789" {
				out[string(d)] = true
			}
		}
	}
	return out
}

// helpName is how the help names a key that tea.KeyPressMsg.String calls
// key: ctrl+r is ctrl-r, N is shift-n.
func helpName(key string) string {
	if rest, ok := strings.CutPrefix(key, "ctrl+"); ok {
		return "ctrl-" + rest
	}
	if rest, ok := strings.CutPrefix(key, "shift+"); ok {
		return "shift-" + rest
	}
	if r := []rune(key); len(r) == 1 && unicode.IsUpper(r[0]) {
		return "shift-" + strings.ToLower(key)
	}
	return key
}

func TestHelpListsEveryKey(t *testing.T) {
	// A table: its own keys and the root's (tableKeys), and its bindings.
	sess := newSession()
	sess.SetTable(query("pods", "default"), podsNamed("web-1"))
	hs := newHarness(t, sess, 120, 40)
	keysOf := append([]string(nil), tableKeys...)
	for _, b := range bindingsFor(kubetest.Resource("pods"), keys.Grant{Name: "alice@laptop"}) {
		if b.Handover {
			// This program has no terminal to hand over, and so not the
			// key (TestShellBinding has the help with one).
			continue
		}
		keysOf = append(keysOf, b.Key)
	}
	hs.press("?")
	got := helpTokens(hs)
	for _, k := range keysOf {
		if !got[helpName(k)] {
			t.Errorf("the help of a table does not show %s (%s):\n%s", k, helpName(k), hs.frame())
		}
	}
	// The header keeps its short list.
	hs.press("esc")
	header := strings.Join(hs.lines()[:headerHeight], "\n")
	for _, k := range []string{"<ctrl-space>", "<ctrl-r>", "<enter>"} {
		if strings.Contains(header, k) {
			t.Errorf("the header shows %s:\n%s", k, header)
		}
	}

	// A text view: the keys it handles.
	sess.Descriptions[kubetest.Ref("pods", "default", "web-1").String()] = "Name: web-1\n"
	hs.press("d")
	textTop(hs)
	hs.press("?")
	got = helpTokens(hs)
	textKeys := []string{
		"up", "k", "down", "j", "pgup", "ctrl+b", "pgdown", "ctrl+f", "space",
		"g", "home", "G", "end", "left", "right",
		"w", "f", "c", "n", "N", "/", "esc", "q", "ctrl+r",
	}
	for _, k := range textKeys {
		if !got[helpName(k)] {
			t.Errorf("the help of a text view does not show %s (%s):\n%s", k, helpName(k), hs.frame())
		}
	}
}

func TestTextViewsReadAgain(t *testing.T) {
	sess := newSession()
	ref := kubetest.Ref("pods", "default", "web-1")
	sess.SetTable(query("pods", "default"), podsNamed("web-1"))
	sess.Descriptions[ref.String()] = "Name: web-1\nStatus: Pending\n"
	sess.YAMLs[ref.String()] = "kind: Pod\nphase: Pending\n"
	hs := newHarness(t, sess, 100, 30)
	for _, c := range []struct{ key, method, before, after string }{
		{"d", "Describe", "Status: Pending", "Status: Running"},
		{"y", "YAML", "phase: Pending", "phase: Running"},
	} {
		hs.press(c.key)
		hs.wantFrame(c.before)
		n := len(sess.Called(c.method))
		if c.method == "Describe" {
			sess.Descriptions[ref.String()] = "Name: web-1\nStatus: Running\n"
		} else {
			sess.YAMLs[ref.String()] = "kind: Pod\nphase: Running\n"
		}
		hs.press("ctrl+r")
		hs.awaitFrame(c.after)
		if got := len(sess.Called(c.method)); got != n+1 {
			t.Errorf("ctrl+r called %s %d times", c.method, got-n)
		}
		hs.press("esc")
	}
}

func TestHeaderPrefersHelpToMoreShortcuts(t *testing.T) {
	hs := newHarness(t, headerSession("alice@kosh-e2e"), 80, 24)
	// Namespaces as clusters have them, oldest first.
	for _, ns := range []string{"kube-system", "monitoring", "cert-manager", "ingress-nginx", "observability", "payments-prod", "argocd", "staging"} {
		hs.command("pods " + ns)
	}
	hs.command("pods default")
	if got := len(hs.root().ns.recent); got != 8 {
		t.Fatalf("%d recent namespaces", got)
	}
	header := strings.Join(hs.lines()[:headerHeight], "\n")
	if !strings.Contains(header, "<?> Help") {
		t.Errorf("the header does not show <?> Help:\n%s", header)
	}
	// The first column of shortcuts: 0 to 4; not the second.
	for _, want := range []string{"<0> all", "<1> default", "<2> staging", "<4> payments-prod"} {
		if !strings.Contains(header, want) {
			t.Errorf("the header does not show %s:\n%s", want, header)
		}
	}
	if strings.Contains(header, "<5>") {
		t.Errorf("the header shows a second column of shortcuts:\n%s", header)
	}
	// Wide enough, every shortcut is shown, and the hints too.
	hs.send(tea.WindowSizeMsg{Width: 200, Height: 24})
	header = strings.Join(hs.lines()[:headerHeight], "\n")
	for _, want := range []string{"<9> kube-system", "<?> Help"} {
		if !strings.Contains(header, want) {
			t.Errorf("at 200 the header does not show %s:\n%s", want, header)
		}
	}
}

// At the standard size the help does not fit across: right shows the
// columns that did not fit, and left goes back.
func TestHelpMovesSideways(t *testing.T) {
	sess := newSession()
	sess.SetTable(query("pods", "default"), podsNamed("web-1"))
	hs := newHarness(t, sess, 80, 24)
	hs.press("?")
	hs.wantFrame("<j/down>")
	seen := false
	for range 6 {
		if strings.Contains(hs.frame(), "<ctrl-c> Quit") {
			seen = true
			break
		}
		hs.press("right")
	}
	if !seen {
		t.Errorf("right does not reach the column of <ctrl-c>:\n%s", hs.frame())
	}
	for range 6 {
		hs.press("left")
	}
	hs.wantFrame("<j/down>", "<left/right> More Columns")

	// Where every column fits, there is nothing to move, and no hint of
	// it.
	hs = newHarness(t, sess, 120, 40)
	hs.press("?")
	before := hs.frame()
	hs.notFrame("More Columns")
	hs.press("right")
	if hs.frame() != before {
		t.Errorf("right moved a help that fits:\n%s", hs.frame())
	}
}

// ctrl+r reads again with r on, and right after the view came back on
// top, and r goes on reading after it.
func TestTextViewReadsAgainAfterItWasCovered(t *testing.T) {
	sess := newSession()
	ref := kubetest.Ref("pods", "default", "web-1")
	sess.SetTable(query("pods", "default"), podsNamed("web-1"))
	sess.YAMLs[ref.String()] = "phase: Pending\n"
	hs := newHarness(t, sess, 100, 30)
	hs.press("y")
	v := textTop(hs)
	v.every = 20 * time.Millisecond * slowdown
	hs.press("r")
	hs.press("?") // the help covers the view
	if _, ok := hs.root().top().(*helpView); !ok {
		t.Fatalf("the top is a %T", hs.root().top())
	}
	hs.press("?") // and is gone
	if textTop(hs) != v {
		t.Fatal("the YAML view is not on top again")
	}
	n := len(sess.Called("YAML"))
	sess.YAMLs[ref.String()] = "phase: Running\n"
	hs.press("ctrl+r")
	hs.awaitFrame("phase: Running")
	if got := len(sess.Called("YAML")); got <= n {
		t.Errorf("ctrl+r did not read: %d readings before, %d after", n, got)
	}
	// r still reads by itself.
	sess.YAMLs[ref.String()] = "phase: Succeeded\n"
	hs.awaitFrame("phase: Succeeded")
}
