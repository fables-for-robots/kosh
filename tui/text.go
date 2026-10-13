package tui

import (
	"context"
	"fmt"
	"regexp"
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

	lines []string // cleaned
	width int      // of the widest line, in cells

	// top is the first row drawn; left the first cell of a line drawn
	// while the lines are not wrapped.
	top, left int
	wrap      bool
	full      bool
	// w and h are the size the view was last drawn in, 0 before.
	w, h int

	// rows are the lines as they are drawn: one row a line, or the
	// pieces of a wrapped line.
	rows   []textRow
	rowsOK bool
	rowsW  int

	// loads counts the readings asked for; only the last one's message
	// is taken.
	loads   int
	managed bool
	// auto: the YAML is read again every every. autoGen tells the ticks
	// of one turn of r from those of another, and autoStop ends the
	// wait of the turn that is on.
	auto     bool
	every    time.Duration
	autoGen  int
	autoStop chan struct{}
	closed   bool

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

// textRow is the cells [from, to) of a line; to is -1 for the rest of
// it.
type textRow struct{ line, from, to int }

// textLoadedMsg brings a reading of view id's object.
type textLoadedMsg struct {
	id    int64
	gen   int
	lines []string
	err   error
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
func textNew(e env, kind textKind, ref kube.ObjectRef, text string) *textView {
	v := &textView{id: newID(), e: e, kind: kind, ref: ref, every: textAutoEvery, at: -1}
	v.setLines(textLines(text))
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

// setLines makes lines the content, and keeps the position in it.
func (v *textView) setLines(lines []string) {
	v.lines = lines
	v.width = 0
	for _, l := range lines {
		v.width = max(v.width, ansi.StringWidth(l))
	}
	v.rowsOK = false
	v.find()
	v.clamp()
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
		if msg.err != nil {
			return v, tea.Batch(cmd, flashErr(msg.err))
		}
		v.setLines(msg.lines)
		return v, cmd
	case textTickMsg:
		if msg.id != v.id || v.closed || !v.auto || msg.autoGen != v.autoGen {
			return v, nil
		}
		return v, v.load(true)
	case refreshMsg:
		return v, v.load(false)
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
	v.matches = nil
	if v.re == nil {
		v.at = -1
		return
	}
	for i, l := range v.lines {
		for _, m := range v.re.FindAllStringIndex(l, -1) {
			if m[1] > m[0] {
				v.matches = append(v.matches, textMatch{i, m[0], m[1]})
			}
		}
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
	l := v.lines[m.line]
	from := ansi.StringWidth(l[:m.from])
	to := ansi.StringWidth(l[:m.to])
	for j, r := range v.layout() {
		if r.line == m.line && (r.to < 0 || from < r.to || j == len(v.rows)-1 || v.rows[j+1].line != m.line) {
			v.top = j
			break
		}
	}
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
	wrapW := 0
	if v.wrap && v.w > 0 {
		wrapW = v.w
	}
	if v.rowsOK && v.rowsW == wrapW {
		return v.rows
	}
	v.rows = v.rows[:0]
	for i, l := range v.lines {
		if wrapW == 0 {
			v.rows = append(v.rows, textRow{i, 0, -1})
			continue
		}
		from := 0
		for _, piece := range strings.Split(ansi.Hardwrap(l, wrapW, true), "\n") {
			to := from + ansi.StringWidth(piece)
			v.rows = append(v.rows, textRow{i, from, to})
			from = to
		}
	}
	v.rowsOK, v.rowsW = true, wrapW
	return v.rows
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
// position: the row with the cell of the line that was at the top.
func (v *textView) relayout(change func()) {
	line, cell := -1, 0
	if rows := v.layout(); v.top < len(rows) {
		line, cell = rows[v.top].line, rows[v.top].from
	}
	change()
	v.rowsOK = false
	v.top = 0
	rows := v.layout()
	for i, r := range rows {
		if r.line == line && (r.to < 0 || cell < r.to || i == len(rows)-1 || rows[i+1].line != line) {
			v.top = i
			break
		}
	}
	v.clamp()
}

// copy puts the content on the user's clipboard.
func (v *textView) copy() tea.Cmd {
	return tea.Batch(
		tea.SetClipboard(strings.Join(v.lines, "\n")),
		flash(fmt.Sprintf("copied %d lines", len(v.lines))),
	)
}

// load reads the object again in a command. tick says that it is the
// YAML reading itself again.
func (v *textView) load(tick bool) tea.Cmd {
	if v.closed {
		return nil
	}
	v.loads++
	e, kind, ref, managed := v.e, v.kind, v.ref, v.managed
	msg := textLoadedMsg{id: v.id, gen: v.loads, tick: tick, autoGen: v.autoGen}
	return func() tea.Msg {
		text, err := textRead(e, kind, ref, managed)
		if err != nil {
			msg.err = err
		} else {
			msg.lines = textLines(text)
		}
		return msg
	}
}

// textRead reads what a text view of kind shows of ref, with a context
// of apiTimeout.
func textRead(e env, kind textKind, ref kube.ObjectRef, managed bool) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), apiTimeout)
	defer cancel()
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

// Close stops the reading again.
func (v *textView) Close() {
	v.closed = true
	v.auto = false
	if v.autoStop != nil {
		close(v.autoStop)
		v.autoStop = nil
	}
}

// View draws the rows from the position.
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
	return strings.Join(out, "\n")
}

// drawRow draws one row, w cells wide.
func (v *textView) drawRow(r textRow, w int) string {
	line := v.paint(r.line)
	switch {
	case r.to >= 0:
		line = ansi.Cut(line, r.from, r.to)
	case v.left > 0:
		line = ansi.TruncateLeft(line, v.left, "")
	}
	return fit(line, w)
}

// paint styles line i: YAML in its colours, and the matches of the
// search over them.
func (v *textView) paint(i int) string {
	l := v.lines[i]
	spans := []yamlSpan{{0, len(l), yamlPlain}}
	if v.kind == textYAML {
		spans = yamlSpans(l)
	}
	if v.re != nil {
		spans = textOverlay(spans, v.re.FindAllStringIndex(l, -1))
	}
	return yamlPaint(l, spans)
}

// textOverlay cuts spans where the matches begin and end, and makes the
// matched pieces matches.
func textOverlay(spans []yamlSpan, matches [][]int) []yamlSpan {
	if len(matches) == 0 {
		return spans
	}
	var out []yamlSpan
	for _, s := range spans {
		at := s.from
		for _, m := range matches {
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

func (v *textView) Capturing() bool  { return false }
func (v *textView) Fullscreen() bool { return v.full }
