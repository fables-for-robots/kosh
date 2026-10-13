package tui

import (
	"strings"
	"testing"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/fables-for-robots/kosh/kube"
)

func TestCommandRefusesANamespaceThatIsNoName(t *testing.T) {
	proxy := "../../../../api/v1/namespaces/prod/services/http:billing:80/proxy/admin"
	for _, c := range []struct {
		name string
		// text is the command; typed is whether it is typed at the
		// prompt or comes as a command message, as enter on a row and
		// the history send it.
		text  string
		typed bool
		word  string
	}{
		{"proxy path", "configmaps " + proxy, true, proxy},
		{"slash", "configmaps a/b", true, "a/b"},
		{"dots", "configmaps ..", true, ".."},
		{"capital", "configmaps A", true, "A"},
		{"64 characters", "configmaps " + strings.Repeat("a", 64), true, strings.Repeat("a", 64)},
		{"leading dash", "configmaps -a", true, "-a"},
		{"trailing dash", "configmaps a-", true, "a-"},
		{"space", "configmaps a b", true, "a b"},
		{"control character", "configmaps a\x07b", false, "a\x07b"},
		{"escape", "configmaps a\x1b[2Jb", false, "a\x1b[2Jb"},
	} {
		t.Run(c.name, func(t *testing.T) {
			sess := newSession()
			hs := newHarness(t, sess, 100, 30)
			top := hs.root().top()
			watches := len(sess.Called("Watch"))
			if c.typed {
				hs.command(c.text)
			} else {
				hs.send(commandMsg{text: c.text})
			}
			want := "not a namespace: " + ansi.Truncate(cleanLine(c.word), 40, "…")
			if l := hs.line("not a namespace"); strings.TrimSpace(l) != want {
				t.Errorf("the flash is %q, want %q", strings.TrimSpace(l), want)
			}
			if !strings.Contains(hs.drawnLine("not a namespace"), sgr(styleError)) {
				t.Error("the flash is not an error")
			}
			if len(hs.root().stack) != 1 || hs.root().top() != top {
				t.Errorf("a view was opened: %q", hs.title())
			}
			if got := sess.Called("Watch"); len(got) != watches {
				t.Errorf("Watch was called: %v", got[watches:])
			}
			if hs.root().ns.current != "default" {
				t.Errorf("the session's namespace is %q", hs.root().ns.current)
			}
		})
	}
	// Names still work.
	for _, ns := range []string{"kube-system", "default", "a", strings.Repeat("a", 63), "0-9"} {
		sess := newSession()
		hs := newHarness(t, sess, 100, 30)
		hs.command("configmaps " + ns)
		if want := "Configmaps(" + ns + ")["; !strings.HasPrefix(hs.title(), want) {
			t.Errorf(":configmaps %s has the title %q", ns, hs.title())
		}
		calls := sess.Called("Watch")
		if q := calls[len(calls)-1].Args[0].(kube.Query); q.Namespace != ns {
			t.Errorf(":configmaps %s watches %q", ns, q.Namespace)
		}
	}
}

// A namespace that is named all is chosen from the namespaces' table as
// the namespace it is, not as all of them.
func TestANamespaceNamedAll(t *testing.T) {
	sess := newSession()
	sess.SetTable(query("namespaces", ""), clusterTable("all"))
	hs := newHarness(t, sess, 100, 30)
	hs.command("ns")
	hs.press("enter")
	if hs.root().ns.current != "all" {
		t.Errorf("the session's namespace is %q", hs.root().ns.current)
	}
	calls := sess.Called("Watch")
	if q := calls[len(calls)-1].Args[0].(kube.Query); q.Namespace != "all" || q.Resource.GVR.Resource != "pods" {
		t.Errorf("enter on the namespace all watches %s in %q", q.Resource.GVR.Resource, q.Namespace)
	}
}

// mib is a mebibyte of text, as a terminal pastes it.
var mib = strings.Repeat("abcdefgh", 1<<17)

func TestPromptTakesAtMostItsLimit(t *testing.T) {
	for _, open := range []string{":", "/"} {
		hs := newHarness(t, newSession(), 100, 30)
		hs.press(open)
		hs.send(tea.PasteMsg{Content: mib})
		if n := utf8.RuneCountInString(hs.root().prompt.in.Value()); n == 0 || n > inputMaxChars {
			t.Errorf("%s: the prompt holds %d characters", open, n)
		}
		hs.frame()
		// Typed, too.
		hs.typeText("xyz")
		if n := utf8.RuneCountInString(hs.root().prompt.in.Value()); n > inputMaxChars {
			t.Errorf("%s: the prompt holds %d characters after typing", open, n)
		}
	}
}

func TestDialogFieldTakesAtMostItsLimit(t *testing.T) {
	hs := newHarness(t, newSession(), 100, 30)
	d := dialogForm("Do it?", nil, []*formField{formTextField("Name", "")}, formButton{label: "Go"})
	hs.send(openDialogMsg{d})
	hs.send(tea.PasteMsg{Content: mib})
	if n := utf8.RuneCountInString(d.fields[0].value()); n == 0 || n > inputMaxChars {
		t.Errorf("the field holds %d characters", n)
	}
	hs.frame()
	for _, f := range []*formField{formTextField("a", ""), formNumberField("b", "", false)} {
		if f.in.CharLimit != inputMaxChars {
			t.Errorf("the field %s takes %d characters", f.label, f.in.CharLimit)
		}
	}
	if newInput(80).CharLimit != inputMaxChars {
		t.Error("the prompt has no limit")
	}
}

func TestEditorRefusesTooMuchText(t *testing.T) {
	sess := newSession()
	hs := editorStart(t, sess, true)
	v := editorShown(hs)
	before := v.ta.Value()
	at := func() [2]int { return [2]int{v.ta.Line(), v.ta.Column()} }
	hs.send(tea.PasteMsg{Content: mib + mib})
	if v.ta.Value() != before {
		t.Errorf("a paste of 2 MiB changed the text to %d bytes", len(v.ta.Value()))
	}
	hs.wantFrame(editorTextTooLong)
	if v.modified {
		t.Error("the editor says the text was changed")
	}
	hs.press("left") // the flash, an error, goes with a key and its time
	hs.clock.advance(flashFor)

	// A paste that would take the text one byte past the limit is
	// refused; one that takes it to the limit is taken.
	hs.press("x")
	room := editorMaxText - len(v.ta.Value())
	text, cursor := v.ta.Value(), at()
	hs.send(tea.PasteMsg{Content: strings.Repeat("y", room+1)})
	if v.ta.Value() != text || at() != cursor {
		t.Fatalf("a paste past the limit was taken: %d bytes, the cursor at %v, was %v", len(v.ta.Value()), at(), cursor)
	}
	hs.wantFrame(editorTextTooLong)
	hs.send(tea.PasteMsg{Content: strings.Repeat("y", room)})
	if got := len(v.ta.Value()); got != editorMaxText {
		t.Fatalf("a paste to the limit left %d bytes, want %d", got, editorMaxText)
	}
	hs.clock.advance(flashFor)
	hs.press("left")
	hs.notFrame(editorTextTooLong)

	// At the limit a key that adds is refused, and enter too.
	text, cursor = v.ta.Value(), at()
	for _, k := range []string{"z", "enter", "space"} {
		hs.press(k)
		if v.ta.Value() != text || at() != cursor {
			t.Errorf("%s at the limit was taken", k)
		}
		hs.wantFrame(editorTextTooLong)
	}
	// A key that adds nothing is taken.
	hs.press("backspace")
	if len(v.ta.Value()) != editorMaxText-1 {
		t.Errorf("backspace at the limit left %d bytes", len(v.ta.Value()))
	}
}

// The text area has more rows than the editor opens: at the largest
// object it opens, enter adds a line. Rows past what the text area can
// hold are refused with the flash, not in silence.
func TestEditorEnterAtTheLimit(t *testing.T) {
	for i, yaml := range editorLargest() {
		sess := newSession()
		sess.YAMLs[editorRef.String()] = yaml
		hs := newHarness(t, sess, 80, 24)
		actionOpen(hs, sess, "configmaps", simpleTable("cfg"))
		hs.press("e")
		v := editorShown(hs)
		n := v.ta.LineCount()
		hs.press("enter")
		if got := v.ta.LineCount(); got != n+1 {
			t.Errorf("YAML %d of %d lines: enter left %d lines", i, n, got)
		}
		hs.typeText("k: v")
		if !strings.HasPrefix(v.ta.Value(), "\nk: v") {
			t.Errorf("YAML %d: the text starts %q", i, v.ta.Value()[:10])
		}
	}

	sess := newSession()
	hs := editorStart(t, sess, true)
	v := editorShown(hs)
	text := v.ta.Value()
	hs.send(tea.PasteMsg{Content: strings.Repeat("\n", editorTextMaxRows)})
	if v.ta.Value() != text {
		t.Errorf("a paste of more rows than the text area holds was taken: %d rows", v.ta.LineCount())
	}
	hs.wantFrame(editorTextTooLong)
}
