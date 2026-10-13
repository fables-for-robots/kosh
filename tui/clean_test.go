package tui

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// forbidden returns what a string that clean returned may not hold, or
// "" when it holds none of it.
func forbidden(s string) string {
	if !utf8.ValidString(s) {
		return "bytes that are not UTF-8"
	}
	for _, r := range s {
		switch {
		case r == 0x1b:
			return "ESC"
		case r == '\n':
		case r < 0x20:
			return "a C0 control character"
		case r == 0x7f:
			return "DEL"
		case r >= 0x80 && r <= 0x9f:
			return "a C1 control character"
		}
	}
	return ""
}

func TestClean(t *testing.T) {
	for _, tt := range []struct {
		name string
		in   string
		want string
	}{
		{"plain", "web-7d9f8b6c4-2xkqp", "web-7d9f8b6c4-2xkqp"},
		{"SGR", "\x1b[31mred\x1b[0m", "red"},
		{"OSC title ending in BEL", "a\x1b]0;owned\x07b", "ab"},
		{"OSC title ending in ST", "a\x1b]0;owned\x1b\\b", "ab"},
		{"CSI cursor move", "a\x1b[10;20Hb", "ab"},
		{"clear screen", "\x1b[2Jx", "x"},
		{"8-bit CSI", "a\u009b2Jb", "ab"},
		{"NUL", "a\x00b", "a␀b"},
		{"carriage return", "a\rb", "a␍b"},
		{"DEL", "a\x7fb", "a␡b"},
		{"C1 byte", "a\u0085b", "a�b"},
		{"invalid UTF-8", "a\xffb\xc3", "a�b�"},
		{"a raw 8-bit CSI byte", "a\x9b2Jb", "a�2Jb"},
		{"tab", "a\tb", "a       b"},
		{"tab after a line feed", "x\nab\tc", "x\nab      c"},
		{"line feed kept", "one\ntwo", "one\ntwo"},
		{"ESC at the end", "abc\x1b", "abc"},
		{"ESC and one character", "a\x1bcb", "ab"},
		{"unterminated OSC", "a\x1b]0;owned", "a"},
		{"DCS", "a\x1bPq#0;2;0;0;0\x1b\\b", "ab"},
		{"wide characters stay", "名前", "名前"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := clean(tt.in)
			if got != tt.want {
				t.Errorf("clean(%q) = %q, want %q", tt.in, got, tt.want)
			}
			if f := forbidden(got); f != "" {
				t.Errorf("clean(%q) = %q holds %s", tt.in, got, f)
			}
			if again := clean(got); again != got {
				t.Errorf("clean is not idempotent on %q: %q, then %q", tt.in, got, again)
			}
		})
	}
}

// Every byte value, alone and after an ESC, comes out harmless.
func TestCleanEveryByte(t *testing.T) {
	for b := 0; b < 256; b++ {
		for _, in := range []string{string([]byte{byte(b)}), "\x1b" + string([]byte{byte(b)}) + "x", "\x1b[" + string([]byte{byte(b)}) + "x"} {
			got := clean(in)
			if f := forbidden(got); f != "" {
				t.Errorf("clean(%q) = %q holds %s", in, got, f)
			}
			if clean(got) != got {
				t.Errorf("clean is not idempotent on %q", in)
			}
		}
	}
}

func TestFit(t *testing.T) {
	red := lipgloss.NewStyle().Foreground(lipgloss.Color("#FF0000"))
	for _, tt := range []struct {
		name  string
		in    string
		width int
		want  string // plain
	}{
		{"pads", "abc", 5, "abc  "},
		{"exact", "abcde", 5, "abcde"},
		{"cuts with an ellipsis", "abcdefgh", 5, "abcd…"},
		{"zero", "abc", 0, ""},
		{"negative", "abc", -3, ""},
		{"one", "abc", 1, "…"},
		{"wide characters", "名前名前", 5, "名前…"},
		{"a wide character at the edge", "a名前", 4, "a名…"},
		{"styled", red.Render("abcdefgh"), 5, "abcd…"},
		{"a line feed ends the line", "ab\ncd", 4, "ab  "},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := fit(tt.in, tt.width)
			if w := ansi.StringWidth(got); w != max(tt.width, 0) {
				t.Errorf("fit(%q, %d) is %d cells wide: %q", tt.in, tt.width, w, got)
			}
			if plain := ansi.Strip(got); plain != tt.want {
				t.Errorf("fit(%q, %d) = %q, want %q", tt.in, tt.width, plain, tt.want)
			}
			if strings.Contains(got, "\n") {
				t.Errorf("fit(%q, %d) holds a line feed", tt.in, tt.width)
			}
		})
	}
	// The styles stay.
	if got := fit(red.Render("abcdefgh"), 5); !strings.Contains(got, "\x1b[") {
		t.Errorf("fit dropped the style: %q", got)
	}
}

// clean and fit take time in proportion to their input, whatever it
// holds: cluster text can be a megabyte on one line. A megabyte takes
// about a tenth of a second; were clean quadratic in anything, as it once
// was in tabs, it would take hours.
func TestCleanAndFitAreLinear(t *testing.T) {
	const size = 1 << 20
	for _, tt := range []struct {
		name string
		in   string
	}{
		{"tabs", strings.Repeat("a\t", size/2)},
		{"wide characters and tabs", strings.Repeat("名\t", size/4)},
		{"no tabs", strings.Repeat("abcdefgh", size/8)},
		{"escape sequences", strings.Repeat("\x1b[31m", size/5)},
		{"an OSC string without its end", "\x1b]0;" + strings.Repeat("x", size)},
		{"OSC strings", strings.Repeat("\x1b]0;x\x07", size/6)},
		{"invalid UTF-8", strings.Repeat("\xff", size)},
		{"control characters", strings.Repeat("\x00\r", size/2)},
		{"line feeds and tabs", strings.Repeat("ab\tc\n", size/5)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			start := time.Now()
			out := clean(tt.in)
			fit(out, 200)
			fit(tt.in, 1<<19)
			if d := time.Since(start); d > slowdown*time.Second {
				t.Errorf("cleaning and fitting a megabyte of %s took %v", tt.name, d)
			}
		})
	}
	// The tab stops are where they were: a tab goes to the next multiple
	// of 8 of its line.
	if got := clean(strings.Repeat("a\t", 1000)); got != strings.Repeat("a       ", 1000) {
		t.Errorf("tabs after one cell are not stops of 8: %q", got[:40])
	}
	if got := clean(strings.Repeat("名\t", 1000)); got != strings.Repeat("名      ", 1000) {
		t.Errorf("tabs after a wide character are not stops of 8: %q", got[:40])
	}
	for _, tt := range []struct{ in, want string }{
		{"abcdefgh\tx", "abcdefgh        x"},
		{"abc\x1b[31m\tx", "abc     x"},
		{"\tx\n\ty", "        x\n        y"},
		{"ab\x00\tx", "ab\u2400     x"},
		{"e\u0301\tx", "e\u0301       x"},
	} {
		if got := clean(tt.in); got != tt.want {
			t.Errorf("clean(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
