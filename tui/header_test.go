package tui

import (
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
				if strings.Contains(all, key) && !strings.Contains(all, key+" "+h.Text) {
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
				if h.Key != "?" && strings.Contains(all, "<"+h.Key+"> "+h.Text) {
					any = true
				}
			}
			if any && !strings.Contains(all, "<?> Help") {
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
