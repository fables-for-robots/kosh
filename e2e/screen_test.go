package e2e

// The screen model: a minimal terminal emulator for what kosh sends,
// which is what Bubble Tea's renderer sends. Bubble Tea repaints a frame
// cell by cell, moving the cursor about, so a word of the screen is
// seldom in the bytes as it is on the screen: the tests read the screen
// a terminal would show instead.

import (
	"bytes"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/ansi"
)

// screen is what a terminal of w by h shows of what was written to it.
// It knows what Bubble Tea's renderer sends: printing, with autowrap
// and insert mode; the cursor's moves, absolute, relative, by tab and
// by reverse index, saved and restored; erasing in the line and the
// display, of characters; inserting and deleting characters and lines;
// scrolling, within a region; repeating a character; the alternate
// screen. It draws no colour or attribute, and ignores what draws
// nothing: modes it does not know, queries, OSC, DCS and APC strings.
// It is safe to write to while it is read.
type screen struct {
	mu sync.Mutex
	p  *ansi.Parser

	w, h     int
	cells    [][]rune // 0 is the second cell of a wide character
	x, y     int
	wrapNext bool // the last cell was printed: the next character wraps first
	autowrap bool
	insert   bool
	top, bot int // the scroll region, rows inclusive
	saved    [2]int
	last     rune // for REP
	all      bytes.Buffer
	watched  []string          // texts to look for after every write
	seenAt   map[string]string // where each watched text was first seen
}

// newScreen returns a blank screen of w by h.
func newScreen(w, h int) *screen {
	s := &screen{p: ansi.NewParser(), autowrap: true}
	s.p.SetHandler(ansi.Handler{
		Print:     s.print,
		Execute:   s.execute,
		HandleCsi: s.csi,
		HandleEsc: s.esc,
	})
	s.resizeLocked(w, h)
	return s
}

// Write takes what the terminal is sent.
func (s *screen) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.all.Write(p)
	for _, b := range p {
		s.p.Advance(b)
	}
	s.look()
	return len(p), nil
}

// lines returns the screen's lines, each of its w cells.
func (s *screen) lines() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, s.h)
	for y, row := range s.cells {
		var b strings.Builder
		for _, r := range row {
			if r != 0 {
				b.WriteRune(r)
			}
		}
		out[y] = b.String()
	}
	return out
}

// String returns the screen's lines without the blanks at their end.
func (s *screen) String() string {
	lines := s.lines()
	for i := range lines {
		lines[i] = strings.TrimRight(lines[i], " ")
	}
	return strings.Join(lines, "\n")
}

// watch makes the screen look, after every write, for each of texts on
// the screen and in the bytes written so far without their escape
// sequences: a text that a frame shows, or sends broken up by cursor
// moves, is seen even when it is painted over before anybody looks.
func (s *screen) watch(texts ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.watched = append(s.watched, texts...)
	s.look()
}

// seen returns the watched texts that were seen, each followed by where
// it was seen first.
func (s *screen) seen() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, t := range s.watched {
		if where, ok := s.seenAt[t]; ok {
			out = append(out, t+" "+where)
		}
	}
	return out
}

// look records which watched texts are on the screen or in the bytes.
func (s *screen) look() {
	if len(s.watched) == 0 {
		return
	}
	if s.seenAt == nil {
		s.seenAt = map[string]string{}
	}
	var screen strings.Builder
	for _, row := range s.cells {
		for _, r := range row {
			if r != 0 {
				screen.WriteRune(r)
			}
		}
		screen.WriteByte('\n')
	}
	stripped := ansi.Strip(s.all.String())
	for _, t := range s.watched {
		if _, ok := s.seenAt[t]; ok {
			continue
		}
		switch {
		case strings.Contains(screen.String(), t):
			s.seenAt[t] = fmt.Sprintf("on the screen, after %d bytes:\n%s", s.all.Len(), screen.String())
		case strings.Contains(stripped, t):
			s.seenAt[t] = fmt.Sprintf("in the bytes, after %d bytes", s.all.Len())
		}
	}
}

// raw returns every byte written so far.
func (s *screen) raw() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.all.String()
}

// resize makes the screen w by h, as a terminal does when its window
// changes: what fits stays, the scroll region is the whole screen.
func (s *screen) resize(w, h int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resizeLocked(w, h)
}

func (s *screen) resizeLocked(w, h int) {
	cells := make([][]rune, h)
	for y := range cells {
		cells[y] = blankRow(w)
		if y < len(s.cells) {
			copy(cells[y], s.cells[y])
		}
	}
	s.cells, s.w, s.h = cells, w, h
	s.top, s.bot = 0, h-1
	s.x, s.y = min(s.x, w-1), min(s.y, h-1)
	s.wrapNext = false
}

func blankRow(w int) []rune {
	row := make([]rune, w)
	for i := range row {
		row[i] = ' '
	}
	return row
}

func (s *screen) print(r rune) {
	wd := ansi.StringWidth(string(r))
	if wd == 0 {
		return // a combining mark: the cell keeps its character
	}
	if s.wrapNext && s.autowrap {
		s.x = 0
		s.lineFeed()
	}
	s.wrapNext = false
	if s.x+wd > s.w {
		if !s.autowrap {
			s.x = s.w - wd
		} else {
			s.x = 0
			s.lineFeed()
		}
	}
	row := s.cells[s.y]
	if s.insert {
		copy(row[s.x+wd:], row[s.x:])
	}
	// A wide character of which one half is overwritten is gone whole.
	for x := s.x; x < s.x+wd; x++ {
		if row[x] == 0 && x > 0 {
			row[x-1] = ' '
		}
		if x+1 < s.w && row[x+1] == 0 && row[x] != 0 {
			row[x+1] = ' '
		}
	}
	row[s.x] = r
	if wd == 2 {
		row[s.x+1] = 0
	}
	s.last = r
	s.x += wd
	if s.x >= s.w {
		s.x = s.w - 1
		s.wrapNext = s.autowrap
	}
}

func (s *screen) execute(b byte) {
	switch b {
	case '\b':
		s.x = max(0, s.x-1)
		s.wrapNext = false
	case '\t':
		s.tab(1)
	case '\n', '\v', '\f':
		s.lineFeed()
	case '\r':
		s.x = 0
		s.wrapNext = false
	}
}

// lineFeed moves down a line, scrolling the region when the cursor is
// on its last line.
func (s *screen) lineFeed() {
	s.wrapNext = false
	switch {
	case s.y == s.bot:
		s.scrollUp(s.top, 1)
	case s.y < s.h-1:
		s.y++
	}
}

// scrollUp moves the lines from row to the region's end up by n.
func (s *screen) scrollUp(row, n int) {
	if row < s.top || row > s.bot {
		return
	}
	n = min(n, s.bot-row+1)
	copy(s.cells[row:s.bot+1], s.cells[row+n:s.bot+1])
	for y := s.bot - n + 1; y <= s.bot; y++ {
		s.cells[y] = blankRow(s.w)
	}
}

// scrollDown moves the lines from row to the region's end down by n.
func (s *screen) scrollDown(row, n int) {
	if row < s.top || row > s.bot {
		return
	}
	n = min(n, s.bot-row+1)
	copy(s.cells[row+n:s.bot+1], s.cells[row:s.bot+1-n])
	for y := row; y < row+n; y++ {
		s.cells[y] = blankRow(s.w)
	}
}

// tab moves to the n-th next tab stop, n negative to go back; there is
// one every 8 columns.
func (s *screen) tab(n int) {
	s.wrapNext = false
	for ; n > 0; n-- {
		s.x = min(s.w-1, (s.x/8+1)*8)
	}
	for ; n < 0; n++ {
		s.x = max(0, (s.x-1)/8*8)
	}
}

func (s *screen) erase(y, from, to int) {
	for x := max(0, from); x < min(s.w, to); x++ {
		s.cells[y][x] = ' '
	}
}

func (s *screen) esc(cmd ansi.Cmd) {
	if cmd.Intermediate() != 0 {
		return // a character set
	}
	switch cmd.Final() {
	case 'M': // reverse index
		s.wrapNext = false
		if s.y == s.top {
			s.scrollDown(s.top, 1)
		} else if s.y > 0 {
			s.y--
		}
	case 'D':
		s.lineFeed()
	case 'E':
		s.x = 0
		s.lineFeed()
	case '7':
		s.saved = [2]int{s.x, s.y}
	case '8':
		s.x, s.y, s.wrapNext = s.saved[0], s.saved[1], false
	case 'c':
		w, h := s.w, s.h
		s.cells = nil
		s.x, s.y, s.autowrap, s.insert = 0, 0, true, false
		s.resizeLocked(w, h)
	}
}

func (s *screen) csi(cmd ansi.Cmd, params ansi.Params) {
	// n is parameter i, at least 1; a missing one or 0 is def.
	n := func(i, def int) int {
		v, _, _ := params.Param(i, def)
		if v == 0 {
			return def
		}
		return v
	}
	if cmd.Intermediate() != 0 {
		return
	}
	if cmd.Prefix() == '?' {
		on := cmd.Final() == 'h'
		if cmd.Final() != 'h' && cmd.Final() != 'l' {
			return
		}
		params.ForEach(-1, func(_, mode int, _ bool) {
			switch mode {
			case 7:
				s.autowrap = on
			case 47, 1047, 1049: // the alternate screen, or back: blank either way
				if mode == 1049 && on {
					s.saved = [2]int{s.x, s.y}
				}
				for y := range s.cells {
					s.cells[y] = blankRow(s.w)
				}
				if mode == 1049 && !on {
					s.x, s.y = s.saved[0], s.saved[1]
				}
			}
		})
		return
	}
	if cmd.Prefix() != 0 {
		return
	}
	final := cmd.Final()
	if final != 'b' {
		s.wrapNext = false
	}
	switch final {
	case 'A':
		lim := 0
		if s.y >= s.top {
			lim = s.top
		}
		s.y = max(lim, s.y-n(0, 1))
	case 'B', 'e':
		lim := s.h - 1
		if s.y <= s.bot {
			lim = s.bot
		}
		s.y = min(lim, s.y+n(0, 1))
	case 'C', 'a':
		s.x = min(s.w-1, s.x+n(0, 1))
	case 'D':
		s.x = max(0, s.x-n(0, 1))
	case 'E':
		s.y, s.x = min(s.h-1, s.y+n(0, 1)), 0
	case 'F':
		s.y, s.x = max(0, s.y-n(0, 1)), 0
	case 'G', '`':
		s.x = min(s.w-1, n(0, 1)-1)
	case 'd':
		s.y = min(s.h-1, n(0, 1)-1)
	case 'H', 'f':
		s.y, s.x = min(s.h-1, n(0, 1)-1), min(s.w-1, n(1, 1)-1)
	case 'I':
		s.tab(n(0, 1))
	case 'Z':
		s.tab(-n(0, 1))
	case 'J':
		mode, _, _ := params.Param(0, 0)
		switch mode {
		case 0:
			s.erase(s.y, s.x, s.w)
			for y := s.y + 1; y < s.h; y++ {
				s.erase(y, 0, s.w)
			}
		case 1:
			for y := 0; y < s.y; y++ {
				s.erase(y, 0, s.w)
			}
			s.erase(s.y, 0, s.x+1)
		case 2, 3:
			for y := range s.h {
				s.erase(y, 0, s.w)
			}
		}
	case 'K':
		mode, _, _ := params.Param(0, 0)
		switch mode {
		case 0:
			s.erase(s.y, s.x, s.w)
		case 1:
			s.erase(s.y, 0, s.x+1)
		case 2:
			s.erase(s.y, 0, s.w)
		}
	case 'X':
		s.erase(s.y, s.x, s.x+n(0, 1))
	case '@':
		row, k := s.cells[s.y], min(n(0, 1), s.w-s.x)
		copy(row[s.x+k:], row[s.x:])
		s.erase(s.y, s.x, s.x+k)
	case 'P':
		row, k := s.cells[s.y], min(n(0, 1), s.w-s.x)
		copy(row[s.x:], row[s.x+k:])
		s.erase(s.y, s.w-k, s.w)
	case 'L':
		s.scrollDown(s.y, n(0, 1))
		s.x = 0
	case 'M':
		s.scrollUp(s.y, n(0, 1))
		s.x = 0
	case 'S':
		s.scrollUp(s.top, n(0, 1))
	case 'T':
		if len(params) <= 1 {
			s.scrollDown(s.top, n(0, 1))
		}
	case 'b':
		if s.last != 0 {
			for range n(0, 1) {
				s.print(s.last)
			}
		}
	case 'r':
		top, bot := n(0, 1)-1, n(1, s.h)-1
		if top < bot && bot < s.h {
			s.top, s.bot = top, bot
			s.x, s.y = 0, 0
		}
	case 'h', 'l':
		params.ForEach(-1, func(_, mode int, _ bool) {
			if mode == 4 {
				s.insert = final == 'h'
			}
		})
	case 's':
		if len(params) == 0 {
			s.saved = [2]int{s.x, s.y}
		}
	case 'u':
		s.x, s.y = s.saved[0], s.saved[1]
	}
}

// feed writes the parts to a screen of w by h and returns it.
func feed(w, h int, parts ...string) *screen {
	s := newScreen(w, h)
	for _, p := range parts {
		_, _ = s.Write([]byte(p))
	}
	return s
}

// wantLines checks the first lines of s, each without the blanks at its
// end.
func wantLines(t *testing.T, s *screen, want ...string) {
	t.Helper()
	got := s.lines()
	for i, w := range want {
		g := ""
		if i < len(got) {
			g = strings.TrimRight(got[i], " ")
		}
		if g != w {
			t.Errorf("line %d is %q, want %q; the screen:\n%s", i, g, w, s)
		}
	}
}

func TestScreenPrintsAndMoves(t *testing.T) {
	s := feed(20, 5,
		"\x1b[?1049h\x1b[H\x1b[2J", // the alternate screen, cleared
		"hello world",
		"\x1b[1;7H\x1b[1;38;5;205mW\x1b[m", // a cell overwritten, with colour
		"\r\n", "second",
		"\x1b[3;1H", "third\x1b[1D\x1b[2C!", // back one, forward two
	)
	wantLines(t, s, "hello World", "second", "third !", "", "")
}

func TestScreenErases(t *testing.T) {
	s := feed(10, 4,
		"0123456789", "\x1b[2;1H", "abcdefghij", "\x1b[3;1H", "ABCDEFGHIJ", "\x1b[4;1H", "klmnopqrst",
		"\x1b[1;4H\x1b[K",  // erase to the end of the line
		"\x1b[2;4H\x1b[1K", // erase to the start of the line, the cursor's cell too
		"\x1b[3;3H\x1b[3X", // erase three characters
		"\x1b[4;5H\x1b[2K", // erase the line
	)
	wantLines(t, s, "012", "    efghij", "AB   FGHIJ", "")

	s = feed(4, 3, "aaaa", "bbbb", "cccc", "\x1b[2;3H\x1b[J")
	wantLines(t, s, "aaaa", "bb", "")
	s = feed(4, 3, "aaaa", "bbbb", "cccc", "\x1b[2;3H\x1b[1J")
	wantLines(t, s, "", "   b", "cccc")
}

// The way Bubble Tea repaints a line that changed in its middle: the
// probe saw "key (none)" become "key a" as \x1bM\x1b[9Da\x1b[K.
func TestScreenALineOverwrittenCellByCell(t *testing.T) {
	s := feed(20, 4,
		"size 100x30\r\nkey (none)\r\ncoloured line",
		"\x1b[2;1H\x1b[4C", "a\x1b[K",
	)
	wantLines(t, s, "size 100x30", "key a", "coloured line")

	s = feed(20, 4, "first\r\nkey (none)\r\nthird line 12", "\x1bM\x1b[9Da\x1b[K")
	wantLines(t, s, "first", "key a", "third line 12")
}

func TestScreenWrapsAndRepeats(t *testing.T) {
	// A full line leaves the cursor on its last cell until the next
	// character, which goes to the next line.
	s := feed(5, 3, "abcde", "f")
	wantLines(t, s, "abcde", "f")
	// Without autowrap the last cell is overwritten.
	s = feed(5, 3, "\x1b[?7l", "abcdefg")
	wantLines(t, s, "abcdg")
	// REP repeats the last character; a wide one takes two cells.
	s = feed(10, 2, "x\x1b[3b", "↑", "界z")
	wantLines(t, s, "xxxx↑界z")
	if got := []rune(s.lines()[0]); len(got) != 9 {
		t.Errorf("the line has %d runes, want 9: %q", len(got), string(got))
	}
}

func TestScreenScrolls(t *testing.T) {
	// A line feed on the last line scrolls the screen up.
	s := feed(5, 3, "a\r\nb\r\nc\r\nd")
	wantLines(t, s, "b", "c", "d")
	// Within a scroll region only the region moves.
	s = feed(5, 4, "top\r\n1\r\n2\r\nbot", "\x1b[2;3r", "\x1b[3;1H\n", "x")
	wantLines(t, s, "top", "2", "x", "bot")
	// Scroll up and down, insert and delete lines.
	s = feed(5, 4, "a\r\nb\r\nc\r\nd", "\x1b[S")
	wantLines(t, s, "b", "c", "d", "")
	s = feed(5, 4, "a\r\nb\r\nc\r\nd", "\x1b[T")
	wantLines(t, s, "", "a", "b", "c")
	s = feed(5, 4, "a\r\nb\r\nc\r\nd", "\x1b[2;1H\x1b[L")
	wantLines(t, s, "a", "", "b", "c")
	s = feed(5, 4, "a\r\nb\r\nc\r\nd", "\x1b[2;1H\x1b[M")
	wantLines(t, s, "a", "c", "d", "")
	// Reverse index on the top line scrolls down.
	s = feed(5, 3, "a\r\nb\r\nc", "\x1b[H\x1bM")
	wantLines(t, s, "", "a", "b")
}

func TestScreenInsertsAndDeletesCharacters(t *testing.T) {
	s := feed(8, 2, "abcdef", "\x1b[1;3H\x1b[2@")
	wantLines(t, s, "ab  cdef")
	s = feed(8, 2, "abcdef", "\x1b[1;3H\x1b[2P")
	wantLines(t, s, "abef")
	s = feed(8, 2, "abcdef", "\x1b[1;3H\x1b[4hXY\x1b[4l")
	wantLines(t, s, "abXYcdef")
}

func TestScreenTabsAndCursorSaves(t *testing.T) {
	s := feed(30, 2, "a\tb", "\x1b[2I", "c", "\x1b[2Z", "d")
	wantLines(t, s, "a       b       d       c")
	s = feed(10, 3, "ab", "\x1b7", "\x1b[3;5Hxy", "\x1b8", "Z", "\x1b[s\x1b[2;2HQ\x1b[uW")
	wantLines(t, s, "abZW", " Q", "    xy")
}

func TestScreenIgnoresWhatDrawsNothing(t *testing.T) {
	s := feed(30, 2,
		"\x1b]0;a title\x07", "\x1b]52;c;?\x1b\\", // OSC, with both terminators
		"\x1b[?25l\x1b[?2004h\x1b[>4;1m\x1b[=1;1u\x1b[?u\x1b[c", // modes, keyboard, queries
		"\x1bP+q544e\x1b\\", "\x1b(B", // DCS, a character set
		"seen",
	)
	wantLines(t, s, "seen")
}

// A narrow character written over either half of a wide one takes the
// whole wide character away: its other half becomes a blank.
func TestScreenOverwritesHalfOfAWideCharacter(t *testing.T) {
	s := feed(6, 1, "a界b", "\x1b[1;2Hx") // over the first half
	wantLines(t, s, "ax b")
	s = feed(6, 1, "a界b", "\x1b[1;3Hx") // over the second half
	wantLines(t, s, "a xb")
	if got := []rune(s.lines()[0]); len(got) != 6 {
		t.Errorf("the line has %d cells, want 6: %q", len(got), string(got))
	}
}

// A watched text is seen when it is on the screen, or in the bytes
// written with their escape sequences taken out, even when it is painted
// over before anybody looks.
func TestScreenWatches(t *testing.T) {
	s := newScreen(20, 3)
	s.watch("secret", "c2VjcmV0")
	_, _ = s.Write([]byte("nothing here"))
	if seen := s.seen(); len(seen) != 0 {
		t.Fatalf("seen %q in %q", seen, s.raw())
	}
	// Written cell by cell with cursor moves between, then erased.
	_, _ = s.Write([]byte("\x1b[2;1Hsec\x1b[1C\x1b[1Dret"))
	_, _ = s.Write([]byte("\x1b[2;1H\x1b[2K"))
	if strings.Contains(s.String(), "secret") {
		t.Fatalf("the screen still shows it:\n%s", s)
	}
	if seen := s.seen(); len(seen) != 1 || !strings.HasPrefix(seen[0], "secret ") {
		t.Errorf("seen %q, want secret", seen)
	}
	// Painted on the screen out of order: only the screen has it.
	s = newScreen(20, 3)
	s.watch("c2VjcmV0")
	_, _ = s.Write([]byte("\x1b[1;5HcmV0\x1b[1;1Hc2Vj"))
	if seen := s.seen(); len(seen) != 1 || !strings.Contains(seen[0], "on the screen") {
		t.Errorf("seen %q, want c2VjcmV0 on the screen", seen)
	}
}

func TestScreenResizes(t *testing.T) {
	s := feed(10, 3, "abcdefghij\r\nsecond")
	s.resize(4, 5)
	if got := s.lines(); len(got) != 5 || len([]rune(got[0])) != 4 {
		t.Fatalf("after a resize to 4 by 5, %d lines of %q", len(got), got)
	}
	wantLines(t, s, "abcd", "seco", "", "", "")
	_, _ = s.Write([]byte("\x1b[5;4HZ"))
	wantLines(t, s, "abcd", "seco", "", "", "   Z")
}

// Sequences may arrive cut anywhere, a character of UTF-8 too.
func TestScreenTakesSequencesInPieces(t *testing.T) {
	stream := "\x1b[2;3Hmid\x1b[1;1H→ok\x1b[38;2;1;2;3mc\x1b[m"
	s := newScreen(10, 3)
	for i := 0; i < len(stream); i++ {
		_, _ = s.Write([]byte{stream[i]})
	}
	wantLines(t, s, "→okc", "  mid")
}

// frames is a model that draws a frame like kosh's: a header, a table in
// a border, and a dialog over it. Each key press draws the next step.
type frames struct {
	w, h, step int
}

func (m frames) Init() tea.Cmd { return nil }

func (m frames) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
	case tea.KeyPressMsg:
		m.step++
	}
	return m, nil
}

var (
	frameKey    = lipgloss.NewStyle().Foreground(lipgloss.Color("#ff5faf")).Bold(true)
	frameBorder = lipgloss.NewStyle().Foreground(lipgloss.Color("#00afff"))
	frameRow    = lipgloss.NewStyle().Background(lipgloss.Color("#303030"))
)

// rows are the table's rows at step: from step 2 on, the first is gone
// and the others moved up, which Bubble Tea draws by scrolling.
func (m frames) rows() []string {
	var rows []string
	for i := range 40 {
		status := "Running"
		if m.step >= 1 && i == 3 {
			status = "Pending"
		}
		rows = append(rows, fmt.Sprintf("web-%02d  1/1  %-8s %3d", i, status, i*7))
	}
	if m.step >= 2 {
		rows = rows[1:]
	}
	return rows
}

func (m frames) lines() []string {
	if m.w < 30 || m.h < 12 {
		return []string{"too small"}
	}
	lines := []string{
		frameKey.Render("Key:") + "     alice@laptop   " + frameKey.Render("<d>") + " Describe",
		frameKey.Render("Cluster:") + " v1.33.1        " + frameKey.Render("<y>") + " YAML",
		"",
	}
	inner := m.w - 2
	title := fmt.Sprintf(" Pods(default)[%d] ", len(m.rows()))
	left := (inner - len(title)) / 2
	lines = append(lines, frameBorder.Render("┌"+strings.Repeat("─", left)+title+strings.Repeat("─", inner-left-len(title))+"┐"))
	body := m.h - len(lines) - 2
	for i := range body {
		cell := ""
		if i < len(m.rows()) {
			cell = m.rows()[i]
		}
		cell += strings.Repeat(" ", inner-len(cell))
		if i == 0 {
			cell = frameRow.Render(cell)
		}
		if m.step >= 3 && i == body/2 {
			box := "╭ Kill pods/default/web-01? [ Cancel ] Kill ╮"
			at := (inner - len([]rune(box))) / 2
			plain := []rune(ansi.Strip(cell))
			cell = string(plain[:at]) + box + string(plain[at+len([]rune(box)):])
		}
		lines = append(lines, frameBorder.Render("│")+cell+frameBorder.Render("│"))
	}
	lines = append(lines, frameBorder.Render("└"+strings.Repeat("─", inner)+"┘"), "<pods>")
	return lines
}

func (m frames) View() tea.View {
	v := tea.NewView(strings.Join(m.lines(), "\n"))
	v.AltScreen = true
	return v
}

// onlcr is what the emulated terminal of an SSH session does to what the
// program writes: a line feed becomes a carriage return and a line feed.
type onlcr struct{ to io.Writer }

func (p onlcr) Write(b []byte) (int, error) {
	if _, err := p.to.Write(bytes.ReplaceAll(b, []byte("\n"), []byte("\r\n"))); err != nil {
		return 0, err
	}
	return len(b), nil
}

// The screen shows each frame Bubble Tea's renderer draws, as kosh's
// SSH server runs it: TERM from the client, colours from the
// environment, input and output on the session, a terminal that turns
// a line feed into a carriage return and a line feed. The frames change
// a cell, scroll the table, draw a dialog over it, and follow two
// resizes.
func TestScreenFollowsBubbleTea(t *testing.T) {
	env := []string{"TERM=xterm-256color"}
	scr := newScreen(100, 30)
	in, keys := io.Pipe()
	defer func() { _ = keys.Close() }()
	p := tea.NewProgram(frames{},
		tea.WithInput(in), tea.WithOutput(onlcr{scr}),
		tea.WithEnvironment(env), tea.WithColorProfile(colorprofile.Env(env)),
		tea.WithWindowSize(100, 30), tea.WithoutSignalHandler(),
	)
	done := make(chan error, 1)
	go func() {
		_, err := p.Run()
		done <- err
	}()

	m := frames{w: 100, h: 30}
	showsFrame := func(what string) {
		t.Helper()
		var want []string
		for _, l := range m.lines() {
			want = append(want, strings.TrimRight(ansi.Strip(l), " "))
		}
		for len(want) < m.h {
			want = append(want, "")
		}
		deadline := time.Now().Add(5 * time.Second)
		for {
			got := scr.lines()
			for i := range got {
				got[i] = strings.TrimRight(got[i], " ")
			}
			if strings.Join(got, "\n") == strings.Join(want, "\n") {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s: the screen is not the frame after 5 seconds.\nscreen:\n%s\nframe:\n%s\nbytes: %q",
					what, strings.Join(got, "\n"), strings.Join(want, "\n"), scr.raw())
			}
			time.Sleep(5 * time.Millisecond)
		}
	}

	showsFrame("the first frame")
	for step, what := range []string{"a cell changed", "the table scrolled", "a dialog drawn over it"} {
		m.step = step + 1
		p.Send(tea.KeyPressMsg{Code: 'n', Text: "n"})
		showsFrame(what)
	}
	for _, size := range [][2]int{{80, 24}, {120, 40}} {
		m.w, m.h = size[0], size[1]
		scr.resize(m.w, m.h)
		p.Send(tea.WindowSizeMsg{Width: m.w, Height: m.h})
		showsFrame(fmt.Sprintf("resized to %dx%d", m.w, m.h))
	}

	p.Quit()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
