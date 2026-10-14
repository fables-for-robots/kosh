package tui

import (
	"regexp"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/fables-for-robots/kosh/keys"
	"github.com/fables-for-robots/kosh/kube/kubetest"
	"github.com/fables-for-robots/kosh/render"
)

// headerSession returns a session of a full key named name, with three
// pods in default.
func headerSession(name string) *kubetest.Session {
	sess := kubetest.New(keys.Grant{Name: name, Fingerprint: "SHA256:" + name})
	sess.SetTable(query("pods", "default"), podTable(
		pod{name: "web-7d9f8b6c4-2xkqp", age: 3 * time.Hour},
		pod{name: "web-7d9f8b6c4-9zt4m", age: 3 * time.Hour},
		pod{name: "db-0", status: "Pending", health: render.Pending, age: 2 * time.Minute},
	))
	return sess
}

// longKeyName is a key's name of 60 characters, some of them wide.
var longKeyName = "alice-" + strings.Repeat("名", 10) + strings.Repeat("x", 43) + "@"

// showsHint reports whether s holds the hint of key with text after it:
// "<key> text", with the padding that lines a column's texts up between
// them.
func showsHint(s, key, text string) bool {
	re := regexp.MustCompile(regexp.QuoteMeta("<"+key+">") + " +" + regexp.QuoteMeta(text))
	return re.MatchString(s)
}

// headerAt returns the header's lines, drawn and plain, in a window w
// wide.
func headerAt(hs *harness, w int) (drawn, plain []string) {
	hs.t.Helper()
	hs.send(tea.WindowSizeMsg{Width: w, Height: 24})
	drawn = hs.root().header()
	for _, l := range drawn {
		plain = append(plain, ansi.Strip(l))
	}
	return drawn, plain
}

func TestHeaderAt80(t *testing.T) {
	hs := newHarness(t, headerSession("alice@kosh-e2e"), 80, 24)
	lines := hs.lines()
	for i, l := range lines {
		if w := ansi.StringWidth(l); w != 80 {
			t.Errorf("line %d of the frame is %d cells wide: %q", i, w, l)
		}
	}
	for i, l := range lines[:headerHeight] {
		if strings.HasSuffix(strings.TrimRight(l, " "), "…") {
			t.Errorf("line %d of the header is cut: %q", i, l)
		}
	}
	hs.golden()
}

func TestHeaderNeverCutsItsPadding(t *testing.T) {
	for _, name := range []string{"al", longKeyName} {
		long := name == longKeyName
		hs := newHarness(t, headerSession(name), 200, 24)
		hints := hs.table().Hints()
		for w := 40; w <= 200; w++ {
			drawn, plain := headerAt(hs, w)
			if len(drawn) != headerHeight {
				t.Fatalf("name %q, width %d: the header has %d lines", name, w, len(drawn))
			}
			for i, l := range drawn {
				if got := ansi.StringWidth(l); got != w {
					t.Errorf("name %q, width %d: line %d of the header is %d cells wide: %q", name, w, i, got, plain[i])
				}
				p := strings.TrimRight(plain[i], " ")
				if strings.TrimLeft(p, " ") == "…" || strings.HasSuffix(p, " …") {
					t.Errorf("name %q, width %d: line %d of the header is cut in its padding: %q", name, w, i, plain[i])
				}
				if strings.Contains(p, "…") && (!long || i != 0) {
					t.Errorf("name %q, width %d: line %d of the header is cut: %q", name, w, i, plain[i])
				}
			}
			// A hint is shown whole or not at all.
			all := strings.Join(plain, "\n")
			for _, h := range hints {
				key := "<" + h.Key + ">"
				if strings.Contains(all, key) && !showsHint(all, h.Key, h.Text) {
					t.Errorf("name %q, width %d: the hint %s is cut:\n%s", name, w, key, all)
				}
			}
		}
	}
}

func TestHeaderShowsHelpWhenItShowsHints(t *testing.T) {
	for _, name := range []string{"al", "alice@kosh-e2e", "a-key-of-twenty-cells"} {
		hs := newHarness(t, headerSession(name), 200, 24)
		hints := hs.table().Hints()
		shownAt := 0
		for w := 40; w <= 200; w++ {
			_, plain := headerAt(hs, w)
			all := strings.Join(plain, "\n")
			any := false
			for _, h := range hints {
				if h.Key != "?" && showsHint(all, h.Key, h.Text) {
					any = true
				}
			}
			if any && !showsHint(all, "?", "Help") {
				t.Errorf("name %q, width %d: the header shows hints but not <?> Help:\n%s", name, w, all)
			}
			if any {
				shownAt++
			}
		}
		if shownAt == 0 {
			t.Errorf("name %q: no width from 40 to 200 shows a hint", name)
		}
	}
}

// textStarts returns, for each column of hints as the header shows
// them in its plain lines, the cells its texts start at, with the keys
// that start there. A column whose texts are lined up has one entry.
func textStarts(t *testing.T, plain []string, hints []hint) []map[int][]string {
	t.Helper()
	var out []map[int][]string
	for _, col := range columnsOf(hints, hintRows) {
		starts := map[int][]string{}
		for i, h := range col {
			key := "<" + h.Key + ">"
			at := strings.Index(plain[i], key)
			if at < 0 {
				t.Fatalf("line %d of the header does not show %s:\n%s", i, key, strings.Join(plain, "\n"))
			}
			rest := plain[i][at+len(key):]
			pad := len(rest) - len(strings.TrimLeft(rest, " "))
			start := ansi.StringWidth(plain[i][:at]) + ansi.StringWidth(key) + pad
			starts[start] = append(starts[start], h.Key)
		}
		out = append(out, starts)
	}
	return out
}

// In every column of the header, the texts of the hints start under
// each other, however wide the keys before them: "<ctrl-w> Wide" under
// "<shift-n> Sort Name" has its text one cell further right than its
// key's width alone would put it. The same holds in the help, which the
// header draws its columns with.
func TestHeaderAlignsTheTextsOfAColumn(t *testing.T) {
	hs := newHarness(t, headerSession("al"), 200, 24)
	// The table, and the describe of a pod, whose hints are another
	// view's: wide enough for every hint, so that the columns are the
	// hints in order, hintRows to a column.
	for _, open := range []string{"", "d"} {
		if open != "" {
			hs.press(open)
		}
		hints := hs.root().top().Hints()
		_, plain := headerAt(hs, 200)
		for c, starts := range textStarts(t, plain, hints) {
			if len(starts) != 1 {
				t.Errorf("view %q, column %d: its texts start at different cells, %v:\n%s", hs.root().top().Crumb(), c, starts, strings.Join(plain, "\n"))
			}
		}
	}
	// The help's columns, drawn by the same function.
	hs.press("esc", "?")
	for _, s := range hs.root().top().(*helpView).sections {
		for c, col := range columnsOf(s.hints, 23) {
			lines := helpLines(col, keyWidth(col))
			starts := map[int]bool{}
			for _, l := range lines {
				p := ansi.Strip(l)
				rest := p[strings.Index(p, ">")+1:]
				starts[len(p)-len(strings.TrimLeft(rest, " "))] = true
			}
			if len(starts) != 1 {
				t.Errorf("help section %q, column %d: its texts start at different cells:\n%s", s.title, c, strings.Join(lines, "\n"))
			}
		}
	}
}
