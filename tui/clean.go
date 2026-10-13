package tui

import (
	"strings"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"
)

// tabWidth is the distance of the tab stops clean puts a tab's spaces up
// to.
const tabWidth = 8

// clean makes text from the cluster safe to draw: escape sequences
// are removed, every other control character but the line feed and
// the tab becomes a visible placeholder, a tab becomes spaces, and
// bytes that are not UTF-8 become U+FFFD. It is idempotent.
//
// An escape sequence is removed whole: a CSI sequence up to its final
// byte, an OSC, DCS, SOS, PM or APC string up to its BEL or ST (or the
// end of s), and any other ESC with its intermediate and final bytes.
// The 8-bit forms of the introducers (U+009B and the others) are read as
// the 7-bit ones. A C0 control character becomes its picture of the
// Control Pictures block (NUL is ␀, CR is ␍), DEL becomes ␡, and any
// other C1 control character U+FFFD.
func clean(s string) string {
	if isPlain(s) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	// col is the width of the line in b up to measured; what was written
	// after measured is measured at the next tab, so that every byte is
	// measured once.
	col, measured := 0, 0
	i := 0
	for i < len(s) {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && size <= 1:
			b.WriteRune(utf8.RuneError)
			i += size
		case r == 0x1b:
			i = skipEscape(s, i+1)
		case r == 0x9b:
			i = skipCSI(s, i+size)
		case r == 0x9d || r == 0x90 || r == 0x98 || r == 0x9e || r == 0x9f:
			i = skipString(s, i+size)
		case r == '\n':
			b.WriteByte('\n')
			col, measured = 0, b.Len()
			i += size
		case r == '\t':
			col += ansi.StringWidth(b.String()[measured:])
			n := tabWidth - col%tabWidth
			b.WriteString(strings.Repeat(" ", n))
			col += n
			measured = b.Len()
			i += size
		case r < 0x20:
			b.WriteRune(0x2400 + r)
			i += size
		case r == 0x7f:
			b.WriteRune(0x2421)
			i += size
		case r >= 0x80 && r <= 0x9f:
			b.WriteRune(utf8.RuneError)
			i += size
		default:
			b.WriteString(s[i : i+size])
			i += size
		}
	}
	return b.String()
}

// isPlain reports whether s is printable ASCII and so needs no
// cleaning.
func isPlain(s string) bool {
	for i := 0; i < len(s); i++ {
		if c := s[i]; c < 0x20 || c > 0x7e {
			return false
		}
	}
	return true
}

// skipEscape returns where the sequence that an ESC at i-1 began ends.
func skipEscape(s string, i int) int {
	if i >= len(s) {
		return i
	}
	switch s[i] {
	case '[':
		return skipCSI(s, i+1)
	case ']', 'P', 'X', '^', '_':
		return skipString(s, i+1)
	}
	// ESC, intermediate bytes, a final byte.
	for i < len(s) && s[i] >= 0x20 && s[i] <= 0x2f {
		i++
	}
	if i < len(s) && s[i] >= 0x30 && s[i] <= 0x7e {
		i++
	}
	return i
}

// skipCSI returns where a CSI sequence whose parameters start at i ends.
// A byte that cannot be in the sequence ends it and is read again as
// text.
func skipCSI(s string, i int) int {
	for i < len(s) && s[i] >= 0x30 && s[i] <= 0x3f {
		i++
	}
	for i < len(s) && s[i] >= 0x20 && s[i] <= 0x2f {
		i++
	}
	if i < len(s) && s[i] >= 0x40 && s[i] <= 0x7e {
		i++
	}
	return i
}

// skipString returns where a control string whose text starts at i
// ends: after its BEL or its ST (ESC \ or U+009C). Another ESC ends it
// and starts a sequence of its own.
func skipString(s string, i int) int {
	for i < len(s) {
		switch {
		case s[i] == 0x07:
			return i + 1
		case s[i] == 0x1b:
			if i+1 < len(s) && s[i+1] == '\\' {
				return i + 2
			}
			return i
		case strings.HasPrefix(s[i:], "\u009c"):
			return i + len("\u009c")
		}
		i++
	}
	return i
}

// fit cuts or pads s to exactly width cells, with "…" at the end of
// a cut line; it counts cells as the terminal does and keeps the
// styles in s intact. s is one line: a line feed and what follows it
// are left out. A width below one gives the empty string.
func fit(s string, width int) string {
	if width <= 0 {
		return ""
	}
	if nl := strings.IndexByte(s, '\n'); nl >= 0 {
		s = s[:nl]
	}
	w := ansi.StringWidth(s)
	if w > width {
		s = ansi.Truncate(s, width, "…")
		w = ansi.StringWidth(s)
	}
	if w < width {
		s += strings.Repeat(" ", width-w)
	}
	return s
}
