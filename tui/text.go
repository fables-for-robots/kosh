package tui

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/fables-for-robots/kosh/kube"
)

// textKind is what a text view shows of its object.
type textKind int

const (
	// textDescribe is what kubectl describe prints.
	textDescribe textKind = iota
	// textYAML is the object's YAML.
	textYAML
	// textDecode is the entries of a Secret, decoded.
	textDecode
)

// textAutoEvery is how often a YAML that reads itself again does.
const textAutoEvery = 2 * time.Second

// textSideStep is how many cells ← and → move a text sideways.
const textSideStep = 8

// textView shows a text about one object: its description, its YAML or
// a Secret's entries. Every line of it is cleaned when it is read.
type textView struct {
	id   int64
	e    env
	kind textKind
	ref  kube.ObjectRef

	lines []textLine
	width int // of the widest line, in cells

	// top is the first row drawn; left the first cell of a line drawn
	// while the lines are not wrapped.
	top, left int
	wrap      bool
	full      bool
	// w and h are the size the view was last drawn in, 0 before.
	w, h int

	// rows are the lines as they are drawn: one row a line, or the
	// pieces of a wrapped line, wrapped at rowsW (0: not wrapped).
	rows   []textRow
	rowsOK bool
	rowsW  int

	// loads counts the readings asked for; only the last one's message
	// is taken. cancel ends the one in flight.
	loads   int
	cancel  context.CancelFunc
	managed bool
	// auto: the YAML is read again every every. autoGen tells the ticks
	// of one turn of r from those of another, and autoStop ends the
	// wait of the turn that is on.
	auto     bool
	every    time.Duration
	autoGen  int
	autoStop chan struct{}
	closed   bool
	buried   bool // another view is over it: r rests (bury)

	// search is the text of the / prompt, re what it searches for, and
	// matches where it is found, in the order of the text. at is the
	// match the view is at, -1 before it is at one.
	search  string
	re      *regexp.Regexp
	matches []textMatch
	at      int
}

// textMatch is a match of a search: bytes [from, to) of a line.
type textMatch struct{ line, from, to int }

// textRow is the bytes [from, to) of a line, which is a row of their
// own when the line is wrapped, and the whole line otherwise.
type textRow struct{ line, from, to int }

// textLine is one line of the content, cleaned, with what drawing a
// part of it needs, so that a row costs what is on the screen and not
// what the line holds.
type textLine struct {
	text  string
	width int // in cells
	// marks are where the cells are in a long line: a mark every
	// textMarkEvery bytes or so, at the start of a grapheme.
	marks []textMark
	// spans are the colours of a line of YAML; nil for a plain line.
	spans []yamlSpan
}

// textMark says that the grapheme at byte b starts at cell c.
type textMark struct{ b, c int }

// textMarkEvery is how many bytes apart the marks of a line are.
const textMarkEvery = 1024

// textContent is the content of a text view, prepared for drawing in
// the command that read it.
type textContent struct {
	lines []textLine
	width int // of the widest line
	// matches are those of the search, which re was compiled from.
	search  string
	matches []textMatch
	// rows are the lines wrapped at wrapW, or one a line when it is 0.
	wrapW int
	rows  []textRow
}

// textLoadedMsg brings a reading of view id's object.
type textLoadedMsg struct {
	id      int64
	gen     int
	content *textContent
	err     error
	// tick: the reading was made because the YAML reads itself again,
	// in the turn autoGen.
	tick    bool
	autoGen int
}

// textTickMsg says that it is time for view id to read its YAML again.
type textTickMsg struct {
	id      int64
	autoGen int
}

// textNew returns a text view of ref that shows text, which it cleans.
// It does all that drawing the text needs first, and so is made in a
// command, not in Update.
func textNew(e env, kind textKind, ref kube.ObjectRef, text string) *textView {
	v := &textView{id: newID(), e: e, kind: kind, ref: ref, every: textAutoEvery, at: -1}
	c := textPrepare(text, kind, "", nil, 0)
	v.setContent(&c)
	return v
}

// textLines cuts text into lines and cleans each one.
func textLines(text string) []string {
	text = strings.TrimSuffix(text, "\n")
	if text == "" {
		return nil
	}
	lines := strings.Split(text, "\n")
	for i, l := range lines {
		lines[i] = clean(l)
	}
	return lines
}

// textPrepare makes the content of text for a view of kind: the lines
// cleaned and measured, their colours, the matches of re (whose text is
// search), and the rows wrapped at wrapW (0: not wrapped). It is what a
// command does with what it read, so that Update and View do not.
func textPrepare(text string, kind textKind, search string, re *regexp.Regexp, wrapW int) textContent {
	c := textContent{search: search, wrapW: wrapW}
	for _, l := range textLines(text) {
		tl := textMeasure(l)
		if kind == textYAML {
			tl.spans = yamlSpans(l)
		}
		c.lines = append(c.lines, tl)
		c.width = max(c.width, tl.width)
	}
	c.matches = textFind(c.lines, re)
	c.rows = textLayout(c.lines, wrapW)
	return c
}

// textNext returns the length in bytes and the width in cells of the
// grapheme at byte i of s.
func textNext(s string, i int) (n, w int) {
	if c := s[i]; c >= 0x20 && c < 0x7f && (i+1 == len(s) || s[i+1] < 0x80) {
		return 1, 1
	}
	g, w := ansi.FirstGraphemeCluster(s[i:], ansi.GraphemeWidth)
	if len(g) == 0 {
		return 1, 0
	}
	return len(g), w
}

// textMeasure measures a line and marks where its cells are.
func textMeasure(l string) textLine {
	tl := textLine{text: l}
	last := 0
	for i := 0; i < len(l); {
		if i-last >= textMarkEvery {
			tl.marks = append(tl.marks, textMark{i, tl.width})
			last = i
		}
		n, w := textNext(l, i)
		i += n
		tl.width += w
	}
	return tl
}

// byteAt returns where the first grapheme of the line that starts at
// cell c or after it begins, and the cell it starts at.
func (tl *textLine) byteAt(c int) (b, at int) {
	if c <= 0 {
		return 0, 0
	}
	if c >= tl.width {
		return len(tl.text), tl.width
	}
	k := sort.Search(len(tl.marks), func(k int) bool { return tl.marks[k].c > c })
	if k > 0 {
		b, at = tl.marks[k-1].b, tl.marks[k-1].c
	}
	for b < len(tl.text) && at < c {
		n, w := textNext(tl.text, b)
		b += n
		at += w
	}
	return b, at
}

// cellAt returns the cell the byte b of the line, the start of a
// grapheme, is drawn at.
func (tl *textLine) cellAt(b int) int {
	k := sort.Search(len(tl.marks), func(k int) bool { return tl.marks[k].b > b })
	i, c := 0, 0
	if k > 0 {
		i, c = tl.marks[k-1].b, tl.marks[k-1].c
	}
	for i < b && i < len(tl.text) {
		n, w := textNext(tl.text, i)
		i += n
		c += w
	}
	return c
}

// textFind returns the matches of re in the lines, those that are not
// empty, in the order of the text. A nil re has none.
func textFind(lines []textLine, re *regexp.Regexp) []textMatch {
	if re == nil {
		return nil
	}
	var out []textMatch
	for i, l := range lines {
		for _, m := range re.FindAllStringIndex(l.text, -1) {
			if m[1] > m[0] {
				out = append(out, textMatch{i, m[0], m[1]})
			}
		}
	}
	return out
}

// textLayout returns the rows of the lines wrapped at w cells, or one a
// line when w is 0. A wrapped row ends before the grapheme that does
// not fit.
func textLayout(lines []textLine, w int) []textRow {
	rows := make([]textRow, 0, len(lines))
	for i, l := range lines {
		if w <= 0 || l.width <= w {
			rows = append(rows, textRow{i, 0, len(l.text)})
			continue
		}
		from, cells := 0, 0
		for b := 0; b < len(l.text); {
			n, gw := textNext(l.text, b)
			if cells+gw > w && b > from {
				rows = append(rows, textRow{i, from, b})
				from, cells = b, 0
			}
			b += n
			cells += gw
		}
		rows = append(rows, textRow{i, from, len(l.text)})
	}
	return rows
}

// setContent makes c the content and keeps the position in it. The
// matches and the rows are made again only when c's are for another
// search or another width.
func (v *textView) setContent(c *textContent) {
	v.lines, v.width = c.lines, c.width
	if c.search == v.search {
		v.matches = c.matches
		if v.at >= len(v.matches) {
			v.at = len(v.matches) - 1
		}
	} else {
		v.find()
	}
	v.rows, v.rowsW, v.rowsOK = c.rows, c.wrapW, true
	v.clamp()
}

// wrapWidth is the width the lines are wrapped at: 0 when they are not.
func (v *textView) wrapWidth() int {
	if v.wrap && v.w > 0 {
		return v.w
	}
	return 0
}

func (v *textView) Init() tea.Cmd { return nil }

// Update takes the view's keys, its readings and its ticks.
func (v *textView) Update(msg tea.Msg) (view, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		return v, v.key(msg)
	case textLoadedMsg:
		if msg.id != v.id || v.closed {
			return v, nil
		}
		var cmd tea.Cmd
		if msg.tick && v.auto && msg.autoGen == v.autoGen {
			cmd = v.tick()
		}
		if msg.gen != v.loads {
			// A later reading is on its way.
			return v, cmd
		}
		v.cancel = nil
		if msg.err != nil {
			return v, tea.Batch(cmd, flashErr(msg.err))
		}
		v.setContent(msg.content)
		return v, cmd
	case textTickMsg:
		if msg.id != v.id || v.closed || !v.auto || msg.autoGen != v.autoGen {
			return v, nil
		}
		return v, v.load(true)
	case refreshMsg:
		return v, v.load(v.surface())
	case buriedMsg:
		v.bury()
		return v, nil
	}
	return v, nil
}

// key handles a key press.
func (v *textView) key(k tea.KeyPressMsg) tea.Cmd {
	switch k.String() {
	case "up", "k":
		v.scroll(-1)
	case "down", "j":
		v.scroll(1)
	case "pgup", "ctrl+b":
		v.scroll(-v.page())
	case "pgdown", "ctrl+f", "space":
		v.scroll(v.page())
	case "g", "home":
		v.top = 0
	case "G", "end":
		v.top = len(v.layout())
		v.clamp()
	case "left":
		if !v.wrap {
			v.left = max(v.left-textSideStep, 0)
		}
	case "right":
		if !v.wrap {
			v.left += textSideStep
			v.clamp()
		}
	case "w":
		v.toggleWrap()
	case "f":
		v.full = !v.full
	case "c":
		return v.copy()
	case "n":
		v.next(1)
	case "N", "shift+n":
		v.next(-1)
	case "r":
		if v.kind == textYAML {
			return v.toggleAuto()
		}
	case "m":
		if v.kind == textYAML {
			v.managed = !v.managed
			return v.load(false)
		}
	case "esc":
		if v.search != "" {
			return v.SetFilter("")
		}
		return popView()
	case "q":
		return popView()
	case "ctrl+r":
		return v.load(false)
	}
	return nil
}

// SetFilter searches the content for text: a regular expression without
// regard to case, or the text as it stands when it is no regular
// expression. The view goes to the first match at or after the line at
// its top, or around the end to the first one. The empty text clears
// the search.
func (v *textView) SetFilter(text string) tea.Cmd {
	v.search = text
	v.re = nil
	if text != "" {
		re, err := regexp.Compile("(?i)" + text)
		if err != nil {
			re = regexp.MustCompile("(?i)" + regexp.QuoteMeta(text))
		}
		v.re = re
	}
	v.find()
	if len(v.matches) == 0 {
		return nil
	}
	line := 0
	if rows := v.layout(); v.top < len(rows) {
		line = rows[v.top].line
	}
	first := 0
	for i, m := range v.matches {
		if m.line >= line {
			first = i
			break
		}
	}
	v.goTo(first)
	return nil
}

// find finds the matches of the search in the content. The view stays
// where it is, at the match of the same number when there still is one.
func (v *textView) find() {
	v.matches = textFind(v.lines, v.re)
	if v.re == nil {
		v.at = -1
	}
	if v.at >= len(v.matches) {
		v.at = len(v.matches) - 1
	}
}

// next goes to the match by of the one the view is at, around the ends.
func (v *textView) next(by int) {
	n := len(v.matches)
	if n == 0 {
		return
	}
	at := v.at
	if at < 0 && by < 0 {
		at = 0
	}
	v.goTo(((at+by)%n + n) % n)
}

// goTo makes match i the one the view is at, and puts it at the top:
// the row it starts in, and when the lines are not wrapped, sideways to
// where it is seen whole.
func (v *textView) goTo(i int) {
	v.at = i
	m := v.matches[i]
	v.top = v.rowOf(m.line, m.from)
	l := &v.lines[m.line]
	from, to := l.cellAt(m.from), l.cellAt(m.to)
	// The last cell of a line cut at the right is its "…".
	if !v.wrap && v.w > 0 && (from < v.left || to > v.left+v.w-1) {
		v.left = max(to-(v.w-1), 0)
		if from < v.left {
			v.left = from
		}
	}
	v.clamp()
}

// page is how many rows a page is: the height last drawn, or 10 before.
func (v *textView) page() int {
	if v.h > 0 {
		return v.h
	}
	return 10
}

// scroll moves the position by n rows, within the content.
func (v *textView) scroll(n int) {
	v.top += n
	v.clamp()
}

// clamp brings the position back into the content: the last page is
// full when there is more than a page, and nothing is scrolled
// sideways beyond the widest line.
func (v *textView) clamp() {
	rows := len(v.layout())
	last := rows - 1
	if v.h > 0 {
		last = rows - v.h
	}
	v.top = max(min(v.top, last), 0)
	if v.wrap {
		v.left = 0
	} else if v.w > 0 {
		v.left = max(min(v.left, v.width-v.w), 0)
	} else {
		v.left = max(min(v.left, v.width-1), 0)
	}
}

// layout returns the rows of the content for the width last drawn.
func (v *textView) layout() []textRow {
	if wrapW := v.wrapWidth(); !v.rowsOK || v.rowsW != wrapW {
		v.rows = textLayout(v.lines, wrapW)
		v.rowsOK, v.rowsW = true, wrapW
	}
	return v.rows
}

// rowOf returns the row that holds byte b of line i.
func (v *textView) rowOf(i, b int) int {
	rows := v.layout()
	k := sort.Search(len(rows), func(k int) bool {
		r := rows[k]
		return r.line > i || r.line == i && r.to > b
	})
	if k == len(rows) || rows[k].line != i {
		// b is the end of the line: its last row.
		k--
	}
	return max(k, 0)
}

// toggleWrap wraps the lines or stops wrapping them, and keeps the line
// at the top.
func (v *textView) toggleWrap() {
	v.relayout(func() {
		v.wrap = !v.wrap
		v.left = 0
	})
}

// relayout makes the change, which changes the rows, and keeps the
// position: the row with the byte of the line that was at the top.
func (v *textView) relayout(change func()) {
	line, b := -1, 0
	if rows := v.layout(); v.top < len(rows) {
		line, b = rows[v.top].line, rows[v.top].from
	}
	change()
	v.rowsOK = false
	v.top = 0
	if line >= 0 {
		v.top = v.rowOf(line, b)
	}
	v.clamp()
}

// copy puts the content on the user's clipboard.
func (v *textView) copy() tea.Cmd {
	texts := make([]string, len(v.lines))
	for i, l := range v.lines {
		texts[i] = l.text
	}
	return tea.Batch(
		tea.SetClipboard(strings.Join(texts, "\n")),
		flash(fmt.Sprintf("copied %d lines", len(v.lines))),
	)
}

// load reads the object again in a command, and prepares what it read
// there for the search and the width of now. tick says that it is the
// YAML reading itself again. A reading still in flight is cancelled: its
// message would not be taken.
func (v *textView) load(tick bool) tea.Cmd {
	if v.closed {
		return nil
	}
	if v.cancel != nil {
		v.cancel()
	}
	ctx, cancel := context.WithTimeout(context.Background(), apiTimeout)
	v.cancel = cancel
	v.loads++
	e, kind, ref, managed := v.e, v.kind, v.ref, v.managed
	search, re, wrapW := v.search, v.re, v.wrapWidth()
	msg := textLoadedMsg{id: v.id, gen: v.loads, tick: tick, autoGen: v.autoGen}
	return func() tea.Msg {
		defer cancel()
		text, err := textRead(ctx, e, kind, ref, managed)
		if err != nil {
			msg.err = err
		} else {
			c := textPrepare(text, kind, search, re, wrapW)
			msg.content = &c
		}
		return msg
	}
}

// textRead reads what a text view of kind shows of ref, within ctx.
func textRead(ctx context.Context, e env, kind textKind, ref kube.ObjectRef, managed bool) (string, error) {
	switch kind {
	case textYAML:
		return e.sess.YAML(ctx, ref, managed)
	case textDecode:
		entries, err := e.sess.SecretData(ctx, ref)
		if err != nil {
			return "", err
		}
		return textEntries(entries), nil
	default:
		return e.sess.Describe(ctx, ref)
	}
}

// textEntries draws a Secret's entries as "key: value", in the order
// they come; a value of several lines is indented under its key.
func textEntries(entries []kube.SecretEntry) string {
	var b strings.Builder
	for _, en := range entries {
		val := strings.TrimSuffix(en.Value, "\n")
		if !strings.Contains(val, "\n") {
			b.WriteString(en.Key + ": " + val + "\n")
			continue
		}
		b.WriteString(en.Key + ":\n")
		for _, l := range strings.Split(val, "\n") {
			b.WriteString("  " + l + "\n")
		}
	}
	return b.String()
}

// toggleAuto turns reading the YAML again on or off.
func (v *textView) toggleAuto() tea.Cmd {
	v.auto = !v.auto
	v.autoGen++
	if v.autoStop != nil {
		close(v.autoStop)
		v.autoStop = nil
	}
	if !v.auto {
		return nil
	}
	v.autoStop = make(chan struct{})
	return v.tick()
}

// tick waits every, then says that it is time to read again.
func (v *textView) tick() tea.Cmd {
	if v.buried {
		return nil
	}
	id, gen, every, stop := v.id, v.autoGen, v.every, v.autoStop
	return waitCmd(func() tea.Msg {
		t := time.NewTimer(every)
		defer t.Stop()
		select {
		case <-stop:
			return nil
		case <-t.C:
			return textTickMsg{id: id, autoGen: gen}
		}
	})
}

// Close stops the reading again, and cancels a reading in flight.
func (v *textView) Close() {
	v.closed = true
	v.auto = false
	if v.cancel != nil {
		v.cancel()
		v.cancel = nil
	}
	if v.autoStop != nil {
		close(v.autoStop)
		v.autoStop = nil
	}
}

// View draws the rows from the position: exactly h lines of w cells.
func (v *textView) View(w, h int) string {
	if w <= 0 || h <= 0 {
		return ""
	}
	if w != v.w {
		v.relayout(func() { v.w = w })
	}
	v.h = h
	v.clamp()
	rows := v.layout()
	out := make([]string, 0, h)
	for i := v.top; i < len(rows) && len(out) < h; i++ {
		out = append(out, v.drawRow(rows[i], w))
	}
	for len(out) < h {
		out = append(out, fit("", w))
	}
	return strings.Join(out, "\n")
}

// drawRow draws one row, w cells wide. It paints only the part of the
// line that is seen: the row of a wrapped line, or the cells from left
// and one more, which fit turns into "…" when the line goes on.
func (v *textView) drawRow(r textRow, w int) string {
	if v.wrapWidth() > 0 {
		return fit(v.paint(r.line, r.from, r.to), w)
	}
	l := &v.lines[r.line]
	from, at := l.byteAt(v.left)
	to, _ := l.byteAt(v.left + w + 1)
	return fit(strings.Repeat(" ", max(at-v.left, 0))+v.paint(r.line, from, to), w)
}

// paint styles bytes [from, to) of line i: YAML in its colours, and the
// matches of the search that are in them over those.
func (v *textView) paint(i, from, to int) string {
	l := &v.lines[i]
	var spans []yamlSpan
	if l.spans == nil {
		spans = []yamlSpan{{0, to - from, yamlPlain}}
	} else {
		for _, s := range l.spans {
			if f, t := max(s.from, from), min(s.to, to); f < t {
				spans = append(spans, yamlSpan{f - from, t - from, s.part})
			}
		}
	}
	k := sort.Search(len(v.matches), func(k int) bool {
		m := v.matches[k]
		return m.line > i || m.line == i && m.to > from
	})
	var ms [][]int
	for ; k < len(v.matches) && v.matches[k].line == i && v.matches[k].from < to; k++ {
		m := v.matches[k]
		ms = append(ms, []int{max(m.from, from) - from, min(m.to, to) - from})
	}
	return yamlPaint(l.text[from:to], textOverlay(spans, ms))
}

// textOverlay cuts spans where the matches begin and end, and makes the
// matched pieces matches. The matches are in order and do not overlap.
func textOverlay(spans []yamlSpan, matches [][]int) []yamlSpan {
	if len(matches) == 0 {
		return spans
	}
	var out []yamlSpan
	k := 0
	for _, s := range spans {
		at := s.from
		for k < len(matches) && matches[k][1] <= s.from {
			k++
		}
		for _, m := range matches[k:] {
			if m[0] >= s.to {
				break
			}
			from, to := max(m[0], at), min(m[1], s.to)
			if from >= to {
				continue
			}
			if from > at {
				out = append(out, yamlSpan{at, from, s.part})
			}
			out = append(out, yamlSpan{from, to, yamlMatch})
			at = to
		}
		if at < s.to {
			out = append(out, yamlSpan{at, s.to, s.part})
		}
	}
	return out
}

// Title is "Describe(pods/default/web-1)", with the search and where
// the view is in its matches, " </err> 3/17", and " (auto)" while the
// YAML reads itself again.
func (v *textView) Title() string {
	t := map[textKind]string{textDescribe: "Describe", textYAML: "YAML", textDecode: "Decode"}[v.kind] + "(" + v.ref.String() + ")"
	if v.search != "" {
		t += fmt.Sprintf(" </%s> %d/%d", v.search, v.at+1, len(v.matches))
	}
	if v.auto {
		t += " (auto)"
	}
	return t
}

// Crumb is "describe", "yaml" or "decode".
func (v *textView) Crumb() string {
	return map[textKind]string{textDescribe: "describe", textYAML: "yaml", textDecode: "decode"}[v.kind]
}

// Hints are the keys of the view that apply to its kind.
func (v *textView) Hints() []hint {
	hs := []hint{
		{"c", "Copy"},
		{"f", "Toggle FullScreen"},
		{"w", "Toggle Wrap"},
		{"/", "Search"},
		{"n", "Next Match"},
		{"shift-n", "Prev Match"},
	}
	if v.kind == textYAML {
		hs = append(hs, hint{"r", "Toggle Auto-Refresh"}, hint{"m", "Toggle ManagedFields"})
	}
	return hs
}

// helpHints are every key of the view, for the help: the movement keys,
// the hints, reading again, and the ways back.
func (v *textView) helpHints() []hint {
	hs := []hint{
		{"j/down", "Down"},
		{"k/up", "Up"},
		{"ctrl-f/pgdown/space", "Page Down"},
		{"ctrl-b/pgup", "Page Up"},
		{"g/home", "Top"},
		{"shift-g/end", "Bottom"},
		{"left/right", "Sideways"},
	}
	hs = append(hs, v.Hints()...)
	return append(hs,
		hint{"ctrl-r", "Read Again"},
		hint{"esc", "Clear Search / Back"},
		hint{"q", "Back"},
	)
}

func (v *textView) Capturing() bool  { return false }
func (v *textView) Fullscreen() bool { return v.full }
