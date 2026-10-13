package tui

import (
	"context"
	"fmt"
	"hash/fnv"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/fables-for-robots/kosh/kube"
)

// logRedrawEvery is the least time between two batches of lines that
// a log view takes: however fast a log is written, the view is drawn
// again at most 20 times a second. The lines that come in between are
// gathered and taken together.
var logRedrawEvery = 50 * time.Millisecond

// The last lines of a log.
const (
	logEnded = "— the log has ended —"
	logBroke = "— the log broke: %s —"
)

// logKind is what a line of a log view is.
type logKind int

const (
	// logText is a line of the log.
	logText logKind = iota
	// logNote is a line the view adds: the end of the log, an error.
	logNote
	// logMark is a rule across the view, with the time it was drawn.
	logMark
)

// logLine is a line of a log view, cleaned when it came.
type logLine struct {
	kind  logKind
	src   string // "pod/container"
	color int    // of src, in logPalette
	at    time.Time
	text  string
	err   bool // a note that is an error
}

// logPalette are the colours of the pods and containers of a log, as
// k9s's: teal, green, purple, lime, dodgerblue, yellow, fuchsia, aqua.
var logPalette = []lipgloss.Style{
	lipgloss.NewStyle().Foreground(lipgloss.Color("#008080")),
	lipgloss.NewStyle().Foreground(lipgloss.Color("#008000")),
	lipgloss.NewStyle().Foreground(lipgloss.Color("#800080")),
	lipgloss.NewStyle().Foreground(lipgloss.Color("#00FF00")),
	lipgloss.NewStyle().Foreground(lipgloss.Color("#1E90FF")),
	lipgloss.NewStyle().Foreground(lipgloss.Color("#FFFF00")),
	lipgloss.NewStyle().Foreground(lipgloss.Color("#FF00FF")),
	lipgloss.NewStyle().Foreground(lipgloss.Color("#00FFFF")),
}

// The styles of a log's own lines and of its times.
var (
	logStyleTime = lipgloss.NewStyle().Foreground(colorGray)
	logStyleMark = lipgloss.NewStyle().Foreground(colorOrange)
)

// logColorOf returns the colour of a pod and container, "pod/container",
// as an index of logPalette: the same for the same names.
func logColorOf(src string) int {
	h := fnv.New32a()
	h.Write([]byte(src))
	return int(h.Sum32() % uint32(len(logPalette)))
}

// logTimeFormat is how a line's time is shown.
const logTimeFormat = time.RFC3339

// logRange is where a log starts, as a key of the view chooses it.
type logRange struct {
	from  kube.LogFrom
	since time.Duration
	name  string // in the title
}

// logRanges are the ranges of the keys 0 to 6.
var logRanges = []logRange{
	{kube.LogTail, 0, "tail"},
	{kube.LogHead, 0, "head"},
	{kube.LogSince, time.Minute, "1m"},
	{kube.LogSince, 5 * time.Minute, "5m"},
	{kube.LogSince, 15 * time.Minute, "15m"},
	{kube.LogSince, 30 * time.Minute, "30m"},
	{kube.LogSince, time.Hour, "1h"},
}

// logPrepare cleans a batch of lines of the log. It is the one place
// where text of a log is cleaned: once, when it comes.
func logPrepare(batch []kube.LogLine) []logLine {
	out := make([]logLine, len(batch))
	for i, l := range batch {
		src := cleanLine(l.Pod) + "/" + cleanLine(l.Container)
		out[i] = logLine{src: src, color: logColorOf(src), at: l.Time, text: cleanLine(l.Text)}
	}
	return out
}

// logKeepLast keeps the last n of s at the start of its array, and
// clears the rest of it, so that what was dropped is let go.
func logKeepLast[T any](s []T, n int) []T {
	if len(s) <= n {
		return s
	}
	k := copy(s, s[len(s)-n:])
	clear(s[k:])
	return s[:k]
}

// logCopyMax is the most c copies: the lines from the end that fit in
// it.
const logCopyMax = 1 << 20

// logBox holds a log view's stream and the context it reads with, so
// that a stream opened after the view went on is closed too.
type logBox struct {
	ctx    context.Context
	cancel context.CancelFunc

	mu     sync.Mutex
	s      kube.LogStream
	closed bool
}

// set keeps s, and reports false when the box is closed already: the
// caller then closes s.
func (b *logBox) set(s kube.LogStream) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return false
	}
	b.s = s
	return true
}

// close closes the stream and cancels its context.
func (b *logBox) close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	if b.s != nil {
		b.s.Close()
	}
	b.cancel()
}

// The messages of a log view.
type (
	// logReadMsg is the lines that came since the last one, and
	// whether the log has ended since.
	logReadMsg struct {
		id      int64
		gen     int
		lines   []logLine
		ended   bool
		err     string // why the log broke, cleaned
		skipped int
		at      time.Time // when the lines were taken
	}
	// logOpenedMsg is a stream opened again, or why it could not be.
	logOpenedMsg struct {
		id      int64
		gen     int
		stream  kube.LogStream
		skipped int
		err     string // cleaned
	}
)

// logView is the log of a pod, of one of its containers, or of the
// pods of a workload, followed as it is written.
type logView struct {
	id    int64
	e     env
	ref   kube.ObjectRef
	opts  kube.LogOptions
	every time.Duration

	// ctx lives as long as the view; each stream reads with a context
	// of its own below it.
	ctx    context.Context
	cancel context.CancelFunc
	box    *logBox
	stream kube.LogStream
	stop   chan struct{}
	gen    int
	closed bool
	// last is when the last batch was taken, by the wall clock.
	last    time.Time
	skipped int

	lines []logLine
	shown []int // the lines shown, as indexes of lines
	// first is the pod and container the first line came from; multi
	// is set once a line came from another one, and then each line shows
	// where it is from.
	first string
	multi bool

	auto, wrap, times, full bool
	// filter is the text of /, and re what it matches; nil shows every
	// line.
	filter string
	re     *regexp.Regexp

	pos  logPos // the first row shown while auto is off
	w, h int    // of the last drawing
}

// logPos is a row of the lines shown: the line, as an index of shown,
// and the row in it when it is wrapped.
type logPos struct{ line, row int }

// before reports whether p comes before q.
func (p logPos) before(q logPos) bool {
	return p.line < q.line || p.line == q.line && p.row < q.row
}

// logNewView returns the view of the log of ref, without a stream.
func logNewView(e env, ref kube.ObjectRef, opts kube.LogOptions) *logView {
	ctx, cancel := context.WithCancel(context.Background())
	return &logView{
		id:     newID(),
		e:      e,
		ref:    ref,
		opts:   opts,
		every:  logRedrawEvery,
		ctx:    ctx,
		cancel: cancel,
		stop:   make(chan struct{}),
		auto:   true,
	}
}

// newBox makes the box of the next stream.
func (v *logView) newBox() *logBox {
	ctx, cancel := context.WithCancel(v.ctx)
	v.box = &logBox{ctx: ctx, cancel: cancel}
	return v.box
}

// logOpen returns a command that opens the log of ref and pushes its
// view. When the log cannot be opened within apiTimeout no view is
// pushed, and the flash says why.
func logOpen(e env, ref kube.ObjectRef, opts kube.LogOptions) tea.Cmd {
	return logOpenWithin(e, ref, opts, apiTimeout)
}

// logOpenWithin is logOpen with the time Logs has to return a stream.
// When it has not returned by then, its context is cancelled. The
// context of a stream that was opened lives as long as the view.
func logOpenWithin(e env, ref kube.ObjectRef, opts kube.LogOptions, within time.Duration) tea.Cmd {
	return func() tea.Msg {
		v := logNewView(e, ref, opts)
		box := v.newBox()
		timer := time.AfterFunc(within, box.cancel)
		s, err := e.sess.Logs(box.ctx, ref, opts)
		if !timer.Stop() {
			if err == nil {
				s.Close()
			}
			v.Close()
			return flashMsg{text: fmt.Sprintf("the log of %s did not open in %v", ref, within), err: true}
		}
		if err != nil {
			v.Close()
			return flashMsg{text: err.Error(), err: true}
		}
		box.set(s)
		v.stream = s
		v.skipped = s.Skipped()
		return pushViewMsg{v}
	}
}

// Init starts reading the stream.
func (v *logView) Init() tea.Cmd { return v.read() }

// read waits for the next lines of the stream, in a command: for the
// first batch that comes, and then for every batch that comes until
// every has passed since the last lines were taken. It holds no more
// than the buffer would keep.
func (v *logView) read() tea.Cmd {
	if v.stream == nil || v.closed {
		return nil
	}
	s, stop, id, gen, last, every := v.stream, v.stop, v.id, v.gen, v.last, v.every
	return waitCmd(func() tea.Msg {
		lines := s.Lines()
		var got []logLine
		take := func(b []kube.LogLine) {
			got = append(got, logPrepare(b)...)
			if len(got) > 2*kube.LogBufferLines {
				got = logKeepLast(got, kube.LogBufferLines)
			}
		}
		msg := func(ended bool) tea.Msg {
			got = logKeepLast(got, kube.LogBufferLines)
			m := logReadMsg{id: id, gen: gen, lines: got, ended: ended, skipped: s.Skipped(), at: time.Now()}
			if ended && s.Err() != nil {
				m.err = cleanLine(s.Err().Error())
			}
			return m
		}
		select {
		case <-stop:
			return nil
		case b, ok := <-lines:
			if !ok {
				return msg(true)
			}
			take(b)
		}
		due := time.NewTimer(time.Until(last.Add(every)))
		defer due.Stop()
		for {
			select {
			case <-stop:
				return nil
			case b, ok := <-lines:
				if !ok {
					return msg(true)
				}
				take(b)
			case <-due.C:
				return msg(false)
			}
		}
	})
}

// Close closes the stream and cancels the view's context.
func (v *logView) Close() {
	if v.closed {
		return
	}
	v.closed = true
	close(v.stop)
	if v.box != nil {
		v.box.close()
	}
	v.cancel()
}

// Update takes the view's lines and its keys.
func (v *logView) Update(msg tea.Msg) (view, tea.Cmd) {
	switch msg := msg.(type) {
	case logReadMsg:
		if msg.id != v.id || msg.gen != v.gen || v.closed {
			return v, nil
		}
		v.last = msg.at
		v.skipped = msg.skipped
		v.add(msg.lines)
		if msg.ended {
			if msg.err != "" {
				v.add([]logLine{{kind: logNote, text: fmt.Sprintf(logBroke, msg.err), err: true}})
			} else {
				v.add([]logLine{{kind: logNote, text: logEnded}})
			}
			return v, nil
		}
		return v, v.read()
	case logOpenedMsg:
		if msg.id != v.id || msg.gen != v.gen || v.closed {
			return v, nil
		}
		if msg.err != "" {
			v.add([]logLine{{kind: logNote, text: msg.err, err: true}})
			return v, nil
		}
		v.stream, v.skipped = msg.stream, msg.skipped
		return v, v.read()
	case tea.KeyPressMsg:
		return v, v.key(msg)
	}
	return v, nil
}

// add puts lines at the end of the buffer, and drops the oldest beyond
// kube.LogBufferLines first, so that the buffer never holds more.
func (v *logView) add(lines []logLine) {
	if n := len(lines) - kube.LogBufferLines; n > 0 {
		lines = lines[n:]
	}
	v.drop(len(v.lines) + len(lines) - kube.LogBufferLines)
	for _, l := range lines {
		v.lines = append(v.lines, l)
		if v.passes(&l) {
			v.shown = append(v.shown, len(v.lines)-1)
		}
		if l.kind == logText {
			if v.first == "" {
				v.first = l.src
			} else if l.src != v.first {
				v.multi = true
			}
		}
	}
}

// drop lets go of the n oldest lines of the buffer.
func (v *logView) drop(n int) {
	if n <= 0 {
		return
	}
	n = min(n, len(v.lines))
	v.lines = logKeepLast(v.lines, len(v.lines)-n)
	d := 0
	for d < len(v.shown) && v.shown[d] < n {
		d++
	}
	v.shown = logKeepLast(v.shown, len(v.shown)-d)
	for i := range v.shown {
		v.shown[i] -= n
	}
	if v.pos.line -= d; v.pos.line < 0 {
		v.pos = logPos{}
	}
}

// passes reports whether l is shown under the filter. The lines the
// view adds itself are always shown.
func (v *logView) passes(l *logLine) bool {
	return l.kind != logText || v.re == nil || v.re.MatchString(l.text)
}

// SetFilter shows only the lines whose text matches text: a regular
// expression without regard to case, or the text as it stands when it
// is none. The empty text shows every line.
func (v *logView) SetFilter(text string) tea.Cmd {
	v.filter, v.re = text, nil
	if text != "" {
		re, err := regexp.Compile("(?i)" + text)
		if err != nil {
			re = regexp.MustCompile("(?i)" + regexp.QuoteMeta(text))
		}
		v.re = re
	}
	v.shown = v.shown[:0]
	for i := range v.lines {
		if v.passes(&v.lines[i]) {
			v.shown = append(v.shown, i)
		}
	}
	v.pos = v.clampPos(v.pos)
	return nil
}

// canAll reports whether a toggles all containers: in the log of a pod
// that names no container.
func (v *logView) canAll() bool {
	return v.ref.Resource.Is("", "pods") && v.opts.Container == ""
}

// reopen closes the stream, empties the buffer and opens the stream
// again with the view's options, in a command. When it cannot be
// opened, the view says why in a line.
func (v *logView) reopen() tea.Cmd {
	close(v.stop)
	v.stop = make(chan struct{})
	if v.box != nil {
		v.box.close()
	}
	v.gen++
	v.stream, v.skipped, v.last = nil, 0, time.Time{}
	v.first, v.multi = "", false
	v.clear()
	box := v.newBox()
	sess, ref, opts, id, gen := v.e.sess, v.ref, v.opts, v.id, v.gen
	return func() tea.Msg {
		s, err := sess.Logs(box.ctx, ref, opts)
		if err != nil {
			return logOpenedMsg{id: id, gen: gen, err: cleanLine(err.Error())}
		}
		if !box.set(s) {
			s.Close()
			return nil
		}
		return logOpenedMsg{id: id, gen: gen, stream: s, skipped: s.Skipped()}
	}
}

// from makes the log start where range i says, and opens it again.
func (v *logView) from(i int) tea.Cmd {
	r := logRanges[i]
	v.opts.From, v.opts.Since = r.from, r.since
	if r.from == kube.LogHead {
		v.auto = false
	}
	return v.reopen()
}

// rangeName is the name of where the log starts.
func (v *logView) rangeName() string {
	for _, r := range logRanges {
		if r.from == v.opts.From && (r.from != kube.LogSince || r.since == v.opts.Since) {
			return r.name
		}
	}
	return v.opts.Since.String()
}

// key handles a key press.
func (v *logView) key(k tea.KeyPressMsg) tea.Cmd {
	page := max(v.h, 1)
	switch k.String() {
	case "s":
		v.auto = !v.auto
		if !v.auto {
			v.pos = v.endPos()
		}
	case "up", "k":
		v.scroll(-1)
	case "down", "j":
		v.scroll(1)
	case "pgup", "ctrl+b":
		v.scroll(-page)
	case "pgdown", "ctrl+f":
		v.scroll(page)
	case "g", "home":
		v.auto, v.pos = false, logPos{}
	case "G", "end":
		v.pos = v.endPos()
	case "w":
		v.wrap = !v.wrap
	case "t":
		v.times = !v.times
	case "f":
		v.full = !v.full
	case "m":
		v.add([]logLine{{kind: logMark, at: v.e.now()}})
	case "C":
		v.clear()
	case "c":
		return v.copy()
	case "0", "1", "2", "3", "4", "5", "6":
		return v.from(int(k.String()[0] - '0'))
	case "a":
		if v.canAll() {
			v.opts.AllContainers = !v.opts.AllContainers
			return v.reopen()
		}
	case "esc":
		if v.filter != "" {
			return v.SetFilter("")
		}
		return popView()
	case "q":
		return popView()
	}
	return nil
}

// clear empties the buffer.
func (v *logView) clear() {
	v.lines, v.shown, v.pos = nil, nil, logPos{}
}

// copy gives the clipboard the lines shown, as they are drawn but
// whole and without styles, and says how many: the last of them that
// fit in logCopyMax.
func (v *logView) copy() tea.Cmd {
	var out []string
	size := 0
	for i := len(v.shown) - 1; i >= 0; i-- {
		s, _ := v.text(&v.lines[v.shown[i]], v.w)
		n := len(s)
		if len(out) > 0 {
			n++
		}
		if size+n > logCopyMax {
			break
		}
		size += n
		out = append(out, s)
	}
	slices.Reverse(out)
	text := fmt.Sprintf("copied %d lines", len(out))
	if len(out) < len(v.shown) {
		text = fmt.Sprintf("copied the last %d of %d lines", len(out), len(v.shown))
	}
	return tea.Batch(tea.SetClipboard(strings.Join(out, "\n")), flash(text))
}

// scroll moves the view by d rows. Up turns autoscroll off.
func (v *logView) scroll(d int) {
	if v.auto {
		if d >= 0 {
			return
		}
		v.auto = false
		v.pos = v.endPos()
	}
	end := v.endPos()
	p := v.clampPos(v.pos)
	for ; d < 0; d++ {
		if p.row > 0 {
			p.row--
		} else if p.line > 0 {
			p.line--
			p.row = v.rowCount(p.line) - 1
		} else {
			break
		}
	}
	for ; d > 0 && p.before(end); d-- {
		if p.row+1 < v.rowCount(p.line) {
			p.row++
		} else {
			p.line, p.row = p.line+1, 0
		}
	}
	v.pos = p
}

// endPos is the first row shown at the end of the log: the last rows
// fill the view, the first of them perhaps in the middle of a line.
// Autoscroll shows it, and the view can go no further down.
func (v *logView) endPos() logPos {
	if len(v.shown) == 0 {
		return logPos{}
	}
	if v.h <= 0 {
		return logPos{line: len(v.shown) - 1}
	}
	rows := 0
	for i := len(v.shown) - 1; i >= 0; i-- {
		c := v.rowCount(i)
		if rows+c >= v.h {
			return logPos{i, rows + c - v.h}
		}
		rows += c
	}
	return logPos{}
}

// clampPos returns p within the lines shown and not beyond the end.
func (v *logView) clampPos(p logPos) logPos {
	if len(v.shown) == 0 || p.line < 0 {
		return logPos{}
	}
	if p.line >= len(v.shown) {
		return v.endPos()
	}
	p.row = max(min(p.row, v.rowCount(p.line)-1), 0)
	if end := v.endPos(); end.before(p) {
		return end
	}
	return p
}

// logSpan is a part of a line that is drawn in a style: the bytes
// from to to.
type logSpan struct {
	from, to int
	style    lipgloss.Style
}

// text returns the line as it is drawn w cells wide, without styles,
// and the parts of it that have a style, in their order.
func (v *logView) text(l *logLine, w int) (string, []logSpan) {
	switch l.kind {
	case logMark:
		s := logRule(l.at, w)
		return s, []logSpan{{0, len(s), logStyleMark}}
	case logNote:
		style := styleDim
		if l.err {
			style = styleError
		}
		return l.text, []logSpan{{0, len(l.text), style}}
	}
	var b strings.Builder
	var spans []logSpan
	if v.multi {
		spans = append(spans, logSpan{0, len(l.src), logPalette[l.color]})
		b.WriteString(l.src + " ")
	}
	if v.times && !l.at.IsZero() {
		ts := l.at.UTC().Format(logTimeFormat)
		spans = append(spans, logSpan{b.Len(), b.Len() + len(ts), logStyleTime})
		b.WriteString(ts + " ")
	}
	off := b.Len()
	b.WriteString(l.text)
	if v.re != nil {
		for _, m := range v.re.FindAllStringIndex(l.text, -1) {
			if m[0] < m[1] {
				spans = append(spans, logSpan{off + m[0], off + m[1], styleMatch})
			}
		}
	}
	return b.String(), spans
}

// logRule is the line of a mark made at t, w cells wide.
func logRule(t time.Time, w int) string {
	label := "── " + t.UTC().Format(logTimeFormat) + " "
	return label + strings.Repeat("─", max(w-ansi.StringWidth(label), 2))
}

// logPaint draws the bytes from a to b of s, with the styles of spans.
func logPaint(s string, spans []logSpan, a, b int) string {
	var out strings.Builder
	at := a
	for _, sp := range spans {
		from, to := max(sp.from, at), min(sp.to, b)
		if from >= to {
			continue
		}
		out.WriteString(s[at:from])
		out.WriteString(sp.style.Render(s[from:to]))
		at = to
	}
	out.WriteString(s[at:b])
	return out.String()
}

// logBreaks returns where the rows of s begin when it is wrapped at w
// cells: 0 first.
func logBreaks(s string, w int) []int {
	starts := []int{0}
	col := 0
	for i := 0; i < len(s); {
		g, gw := ansi.FirstGraphemeCluster(s[i:], ansi.GraphemeWidth)
		if len(g) == 0 {
			break
		}
		if col > 0 && col+gw > w {
			starts = append(starts, i)
			col = 0
		}
		col += gw
		i += len(g)
	}
	return starts
}

// rowCount is how many rows the shown line i takes.
func (v *logView) rowCount(i int) int {
	if !v.wrap || v.w <= 0 {
		return 1
	}
	s, _ := v.text(&v.lines[v.shown[i]], v.w)
	return len(logBreaks(s, v.w))
}

// rows draws the shown line i in rows of w cells: one, cut at the
// width, or as many as it takes when wrapped.
func (v *logView) rows(i, w int) []string {
	s, spans := v.text(&v.lines[v.shown[i]], w)
	if !v.wrap {
		return []string{fit(logPaint(s, spans, 0, len(s)), w)}
	}
	starts := logBreaks(s, w)
	out := make([]string, len(starts))
	for j, a := range starts {
		b := len(s)
		if j+1 < len(starts) {
			b = starts[j+1]
		}
		out[j] = fit(logPaint(s, spans, a, b), w)
	}
	return out
}

// View draws the end of the log while autoscroll is on, and the rows
// from where the view stands otherwise: exactly h rows of w cells.
func (v *logView) View(w, h int) string {
	if w <= 0 || h <= 0 {
		return ""
	}
	v.w, v.h = w, h
	var pos logPos
	if v.auto {
		pos = v.endPos()
	} else {
		v.pos = v.clampPos(v.pos)
		pos = v.pos
	}
	rows := make([]string, 0, h)
	for i := pos.line; i < len(v.shown) && len(rows) < h; i++ {
		r := v.rows(i, w)
		if i == pos.line {
			r = r[min(pos.row, len(r)-1):]
		}
		rows = append(rows, r...)
	}
	if len(rows) > h {
		rows = rows[:h]
	}
	for len(rows) < h {
		rows = append(rows, fit("", w))
	}
	return strings.Join(rows, "\n")
}

// Title is "Logs(pods/default/web-1) [autoscroll:on] tail", with the
// notes that apply.
func (v *logView) Title() string {
	var b strings.Builder
	b.WriteString("Logs(" + v.ref.String())
	if v.opts.Container != "" {
		b.WriteString(":" + v.opts.Container)
	}
	b.WriteString(")")
	if v.opts.Previous {
		b.WriteString(" previous")
	}
	if v.auto {
		b.WriteString(" [autoscroll:on]")
	} else {
		b.WriteString(" [autoscroll:off]")
	}
	b.WriteString(" " + v.rangeName())
	if v.filter != "" {
		b.WriteString(" </" + v.filter + ">")
	}
	if v.skipped > 0 {
		fmt.Fprintf(&b, " (%d containers left out)", v.skipped)
	}
	return b.String()
}

// Crumb is "logs".
func (v *logView) Crumb() string { return "logs" }

// Hints are the keys of the view.
func (v *logView) Hints() []hint {
	hs := []hint{
		{"s", "Autoscroll"},
		{"w", "Wrap"},
		{"t", "Times"},
		{"f", "Full Screen"},
	}
	for i, r := range logRanges {
		hs = append(hs, hint{fmt.Sprint(i), capital(r.name)})
	}
	hs = append(hs, hint{"/", "Filter"}, hint{"m", "Mark"}, hint{"shift-c", "Clear"})
	if v.canAll() {
		hs = append(hs, hint{"a", "All Containers"})
	}
	return append(hs, hint{"c", "Copy"})
}

func (v *logView) Capturing() bool { return false }

// Fullscreen is true after f.
func (v *logView) Fullscreen() bool { return v.full }
