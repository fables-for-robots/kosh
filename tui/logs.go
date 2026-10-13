package tui

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"

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
)

// logLine is a line of a log view, cleaned when it came.
type logLine struct {
	kind logKind
	src  string // "pod/container"
	at   time.Time
	text string
	err  bool // a note that is an error
}

// logPrepare cleans a batch of lines of the log. It is the one place
// where text of a log is cleaned: once, when it comes.
func logPrepare(batch []kube.LogLine) []logLine {
	out := make([]logLine, len(batch))
	for i, l := range batch {
		out[i] = logLine{
			src:  cleanLine(l.Pod) + "/" + cleanLine(l.Container),
			at:   l.Time,
			text: cleanLine(l.Text),
		}
	}
	return out
}

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

	auto bool
	top  int // the first line shown, as an index of shown, while auto is off
	w, h int // of the last drawing
}

// newLogView returns the view of the log of ref, without a stream.
func newLogView(e env, ref kube.ObjectRef, opts kube.LogOptions) *logView {
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
// view. When the log cannot be opened no view is pushed, and the flash
// says why.
func logOpen(e env, ref kube.ObjectRef, opts kube.LogOptions) tea.Cmd {
	return func() tea.Msg {
		v := newLogView(e, ref, opts)
		box := v.newBox()
		s, err := e.sess.Logs(box.ctx, ref, opts)
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
			if n := len(got) - kube.LogBufferLines; n > 0 {
				got = got[n:]
			}
		}
		msg := func(ended bool) tea.Msg {
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
	case tea.KeyPressMsg:
		return v, v.key(msg)
	}
	return v, nil
}

// add puts lines at the end of the buffer, and drops the oldest beyond
// kube.LogBufferLines.
func (v *logView) add(lines []logLine) {
	for _, l := range lines {
		v.lines = append(v.lines, l)
		v.shown = append(v.shown, len(v.lines)-1)
	}
	n := len(v.lines) - kube.LogBufferLines
	if n <= 0 {
		return
	}
	v.lines = v.lines[n:]
	d := 0
	for d < len(v.shown) && v.shown[d] < n {
		d++
	}
	v.shown = v.shown[d:]
	for i := range v.shown {
		v.shown[i] -= n
	}
	v.top = max(v.top-d, 0)
}

// key handles a key press.
func (v *logView) key(k tea.KeyPressMsg) tea.Cmd {
	page := max(v.h, 1)
	switch k.String() {
	case "s":
		v.auto = !v.auto
		if !v.auto {
			v.top = v.endTop()
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
		v.auto, v.top = false, 0
	case "G", "end":
		v.top = v.endTop()
	case "esc", "q":
		return popView()
	}
	return nil
}

// scroll moves the view by d lines. Up turns autoscroll off.
func (v *logView) scroll(d int) {
	if v.auto {
		if d >= 0 {
			return
		}
		v.auto = false
		v.top = v.endTop()
	}
	v.top = max(min(v.top+d, v.endTop()), 0)
}

// endTop is the first line shown when the end of the log is.
func (v *logView) endTop() int {
	if len(v.shown) == 0 {
		return 0
	}
	if v.h <= 0 {
		return len(v.shown) - 1
	}
	rows := 0
	for i := len(v.shown) - 1; i >= 0; i-- {
		rows += v.rowCount(i)
		if rows > v.h {
			return min(i+1, len(v.shown)-1)
		}
	}
	return 0
}

// rowCount is how many rows the shown line i takes.
func (v *logView) rowCount(i int) int { return 1 }

// rows draws the shown line i in rows of w cells.
func (v *logView) rows(i, w int) []string {
	l := &v.lines[v.shown[i]]
	switch {
	case l.kind == logNote && l.err:
		return []string{styleError.Render(fit(l.text, w))}
	case l.kind == logNote:
		return []string{styleDim.Render(fit(l.text, w))}
	}
	return []string{fit(l.text, w)}
}

// View draws the end of the log while autoscroll is on, and the lines
// from the top otherwise.
func (v *logView) View(w, h int) string {
	if w <= 0 || h <= 0 {
		return ""
	}
	v.w, v.h = w, h
	var rows []string
	if v.auto {
		for i := len(v.shown) - 1; i >= 0 && len(rows) < h; i-- {
			r := v.rows(i, w)
			for j := len(r) - 1; j >= 0 && len(rows) < h; j-- {
				rows = append(rows, r[j])
			}
		}
		for a, b := 0, len(rows)-1; a < b; a, b = a+1, b-1 {
			rows[a], rows[b] = rows[b], rows[a]
		}
	} else {
		v.top = max(min(v.top, v.endTop()), 0)
		for i := v.top; i < len(v.shown) && len(rows) < h; i++ {
			rows = append(rows, v.rows(i, w)...)
		}
		if len(rows) > h {
			rows = rows[:h]
		}
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
	b.WriteString(" tail")
	if v.skipped > 0 {
		fmt.Fprintf(&b, " (%d containers left out)", v.skipped)
	}
	return b.String()
}

// Crumb is "logs".
func (v *logView) Crumb() string { return "logs" }

// Hints are the keys of the view.
func (v *logView) Hints() []hint {
	return []hint{{"s", "Autoscroll"}}
}

func (v *logView) Capturing() bool  { return false }
func (v *logView) Fullscreen() bool { return false }
