package tui

import (
	"regexp"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/fables-for-robots/kosh/keys"
	"github.com/fables-for-robots/kosh/kube"
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

// usageLines returns lines 3 and 4 of the header, where the CPU and MEM
// lines are, up to the first key right of the left block.
func usageLines(plain []string) (cpu, mem string) {
	left := func(l string) string {
		if i := strings.Index(l, "<"); i >= 0 {
			l = l[:i]
		}
		return strings.TrimRight(l, " ")
	}
	return left(plain[3]), left(plain[4])
}

func TestHeaderShowsUsageWhenServed(t *testing.T) {
	sess := headerSession("alice@kosh-e2e")
	hs := newHarness(t, sess, 80, 24)

	sess.SetUsage(kube.ClusterUsage{CPU: 12, Mem: 47, Served: true}, true)
	hs.awaitFrame("CPU:     12%")
	_, plain := headerAt(hs, 80)
	if cpu, mem := usageLines(plain); cpu != "CPU:     12%" || mem != "MEM:     47%" {
		t.Errorf("the first reading: the usage lines are %q and %q, want %q and %q", cpu, mem, "CPU:     12%", "MEM:     47%")
	}

	sess.SetUsage(kube.ClusterUsage{CPU: 14, Mem: 40, Served: true}, true)
	hs.awaitFrame("CPU:     14%")
	drawn, plain := headerAt(hs, 80)
	if cpu, mem := usageLines(plain); cpu != "CPU:     14% ↑" || mem != "MEM:     40% ↓" {
		t.Errorf("a rise and a fall: the usage lines are %q and %q, want %q and %q", cpu, mem, "CPU:     14% ↑", "MEM:     40% ↓")
	}
	if !strings.Contains(drawn[3], sgr(styleUp)+"↑") {
		t.Errorf("the arrow up is not drawn in styleUp: %q", drawn[3])
	}
	if !strings.Contains(drawn[4], sgr(styleDown)+"↓") {
		t.Errorf("the arrow down is not drawn in styleDown: %q", drawn[4])
	}

	// The same reading again: nothing changed since the one before.
	sess.SetUsage(kube.ClusterUsage{CPU: 14, Mem: 40, Served: true}, true)
	hs.await("the arrows to go", func() bool {
		cpu, mem := usageLines(hs.lines())
		return !strings.ContainsAny(cpu+mem, "↑↓")
	})
	_, plain = headerAt(hs, 80)
	if cpu, mem := usageLines(plain); cpu != "CPU:     14%" || mem != "MEM:     40%" {
		t.Errorf("the same reading again: the usage lines are %q and %q, want %q and %q", cpu, mem, "CPU:     14%", "MEM:     40%")
	}

	// A reading that is not ok says n/a, and the next ok one is compared
	// with the last ok one before it, across the gap.
	sess.SetUsage(kube.ClusterUsage{Served: true}, false)
	hs.awaitFrame("CPU:     n/a")
	sess.SetUsage(kube.ClusterUsage{CPU: 20, Mem: 30, Served: true}, true)
	hs.awaitFrame("CPU:     20%")
	_, plain = headerAt(hs, 80)
	if cpu, mem := usageLines(plain); cpu != "CPU:     20% ↑" || mem != "MEM:     30% ↓" {
		t.Errorf("14 and 40, n/a, then 20 and 30: the usage lines are %q and %q, want %q and %q", cpu, mem, "CPU:     20% ↑", "MEM:     30% ↓")
	}

	// The lines do not move the columns right of the left block, and
	// every line is as wide as the window, at every width.
	for w := 40; w <= 200; w++ {
		drawn, plain := headerAt(hs, w)
		for i, l := range drawn {
			if got := ansi.StringWidth(l); got != w {
				t.Errorf("width %d: line %d of the header is %d cells wide: %q", w, i, got, plain[i])
			}
		}
	}
	_, plain = headerAt(hs, 80)
	cell := func(l, key string) int { return ansi.StringWidth(l[:strings.Index(l, key)]) }
	if at, want := cell(plain[3], "<shift-o>"), cell(plain[0], "<shift-n>"); at != want {
		t.Errorf("the hints of line 3 start at %d, those of line 0 at %d:\n%s", at, want, strings.Join(plain, "\n"))
	}
}

func TestHeaderSaysNAWhileServedWithoutAReading(t *testing.T) {
	sess := headerSession("alice@kosh-e2e")
	hs := newHarness(t, sess, 80, 24)
	sess.SetUsage(kube.ClusterUsage{Served: true}, false)
	hs.awaitFrame("CPU:     n/a")
	_, plain := headerAt(hs, 80)
	if cpu, mem := usageLines(plain); cpu != "CPU:     n/a" || mem != "MEM:     n/a" {
		t.Errorf("the usage lines are %q and %q, want %q and %q", cpu, mem, "CPU:     n/a", "MEM:     n/a")
	}

	// A reading after none has no arrow: there is no number before it.
	sess.SetUsage(kube.ClusterUsage{CPU: 30, Mem: 60, Served: true}, true)
	hs.awaitFrame("CPU:     30%")
	_, plain = headerAt(hs, 80)
	if cpu, mem := usageLines(plain); cpu != "CPU:     30%" || mem != "MEM:     60%" {
		t.Errorf("a reading after n/a: the usage lines are %q and %q, want %q and %q", cpu, mem, "CPU:     30%", "MEM:     60%")
	}
}

func TestHeaderHasNoUsageLinesWithoutTheAPI(t *testing.T) {
	sess := headerSession("alice@kosh-e2e")
	hs := newHarness(t, sess, 80, 24)
	check := func(when string) {
		t.Helper()
		drawn, plain := headerAt(hs, 80)
		for i, l := range drawn {
			if got := ansi.StringWidth(l); got != 80 {
				t.Errorf("%s: line %d of the header is %d cells wide: %q", when, i, got, plain[i])
			}
		}
		if cpu, mem := usageLines(plain); cpu != "" || mem != "" {
			t.Errorf("%s: lines 3 and 4 of the header are not blank on the left: %q and %q", when, cpu, mem)
		}
		if strings.Contains(strings.Join(plain, "\n"), "CPU:") {
			t.Errorf("%s: the header has a CPU line:\n%s", when, strings.Join(plain, "\n"))
		}
	}
	check("the default fake")

	// The API served, and then no longer: the lines go with it.
	sess.SetUsage(kube.ClusterUsage{CPU: 12, Mem: 47, Served: true}, true)
	hs.awaitFrame("CPU:     12%")
	sess.SetUsage(kube.ClusterUsage{}, false)
	hs.await("the usage lines to go", func() bool { return !strings.Contains(hs.plain(), "CPU:") })
	check("the API gone")
}

func TestHeaderWithUsage80(t *testing.T) {
	sess := headerSession("alice@kosh-e2e")
	hs := newHarness(t, sess, 80, 24)
	sess.SetUsage(kube.ClusterUsage{CPU: 12, Mem: 47, Served: true}, true)
	hs.awaitFrame("CPU:     12%")
	sess.SetUsage(kube.ClusterUsage{CPU: 14, Mem: 40, Served: true}, true)
	hs.awaitFrame("CPU:     14% ↑")
	hs.golden()
}
