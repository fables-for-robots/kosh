package tui

import (
	"errors"
	"fmt"
	"strings"
	"sync/atomic"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/fables-for-robots/kosh/keys"
	"github.com/fables-for-robots/kosh/kube"
)

// The smallest window the frame is drawn in.
const (
	minWidth  = 80
	minHeight = 12
)

// headerHeight is the number of lines of the header.
const headerHeight = 6

// root is the model of a session: it owns the screen and hands the
// views their messages.
type root struct {
	e        env
	version  string
	grant    keys.Grant
	cluster  string
	w, h     int
	stack    []view
	dlg      dialog
	prompt   prompt
	noHeader bool
	flash    flashLine
	ns       namespaces
	hist     history
	// startErr is why the table of pods could not be opened at the
	// start; the body shows it while the stack is empty.
	startErr error
	// usageWatch is the cluster's use of its nodes, for the header's CPU
	// and MEM lines. usage is its last reading, and prevUsage the last ok
	// one before it, which the arrows compare with: a reading that is
	// not ok says n/a and leaves prevUsage as it was.
	usageWatch       kube.UsageWatch
	usage, prevUsage usageReading
}

// usageReading is a reading of the usage watch: ok is false while there
// is none.
type usageReading struct {
	u  kube.ClusterUsage
	ok bool
}

// usageMsg is what the usage watch says after it signalled.
type usageMsg struct {
	usageReading
}

func newRoot(e env, version string) *root {
	grant := e.sess.Grant()
	return &root{
		e:       e,
		version: version,
		grant:   grant,
		cluster: e.sess.ClusterVersion(),
		ns:      startNamespaces(grant),
	}
}

// Init resolves pods and opens their table in the starting namespace:
// the one of the key's Role, or default. It opens the cluster's usage
// for the header.
func (r *root) Init() tea.Cmd {
	r.usageWatch = r.e.sess.ClusterUsage()
	return tea.Batch(r.resolve("pods", "pods", "", false, true, true), r.waitUsage())
}

// waitUsage waits for the usage watch to signal, and reads it then. The
// session's end closes the watch, and the watch closes its Changed: the
// wait ends then with nothing, and nothing of the session is left
// waiting.
func (r *root) waitUsage() tea.Cmd {
	w := r.usageWatch
	return waitCmd(func() tea.Msg {
		if _, open := <-w.Changed(); !open {
			return nil
		}
		u, ok := w.Current()
		return usageMsg{usageReading{u: u, ok: ok}}
	})
}

// Update routes msg: a key press to the one part that has the keys, a
// message for the root to the root, and every other message to every
// view and the dialog.
func (r *root) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		r.w, r.h = msg.Width, msg.Height
		r.prompt.resize(r.w)
		cmd := r.broadcast(msg)
		if r.dlg != nil && !r.dialogFits(r.dlg) {
			// Closed, as Cancel would: nothing is done.
			r.dlg.Drop()
			r.dlg = nil
			cmd = tea.Batch(cmd, flashErr(errors.New(dialogTextTooSmall)))
		}
		return r, cmd
	case tea.KeyPressMsg:
		return r, r.key(msg)
	case tea.KeyReleaseMsg:
		return r, nil
	case tea.PasteMsg:
		return r, r.paste(msg)
	case pushViewMsg:
		return r, r.push(msg.v)
	case popViewMsg:
		return r, r.pop()
	case flashMsg:
		return r, r.flash.show(msg, r.e.now())
	case flashDoneMsg:
		r.flash.done(msg)
		return r, nil
	case openDialogMsg:
		if !r.dialogFits(msg.d) {
			msg.d.Drop()
			return r, flashErr(errors.New(dialogTextTooSmall))
		}
		r.dlg = msg.d
		return r, msg.d.Init()
	case closeDialogMsg:
		r.dlg = nil
		return r, nil
	case refreshMsg:
		return r, r.updateTop(msg)
	case commandMsg:
		return r, r.runCommand(msg.text, true, msg.literal)
	case resolvedMsg:
		return r, r.resolved(msg)
	case useNamespaceMsg:
		return r, r.useNamespace(msg.ns)
	case suggestionsMsg:
		r.prompt.suggest(msg.names)
		return r, nil
	case usageMsg:
		if r.usage.ok {
			r.prevUsage = r.usage
		}
		r.usage = msg.usageReading
		return r, r.waitUsage()
	}
	return r, r.broadcast(msg)
}

// key routes a key press: ctrl+c, the dialog, the prompt, a capturing
// view, the root's own keys, the top view.
func (r *root) key(k tea.KeyPressMsg) tea.Cmd {
	s := k.String()
	if s == "ctrl+c" {
		return tea.Quit
	}
	r.flash.key()
	if r.dlg != nil {
		var cmd tea.Cmd
		r.dlg, cmd = r.dlg.Update(k)
		return cmd
	}
	if r.prompt.open() {
		return r.promptKey(k)
	}
	top := r.top()
	if top != nil && top.Capturing() {
		return r.updateTop(k)
	}
	switch s {
	case ":":
		return r.openCommand()
	case "/":
		if _, ok := top.(filterable); ok {
			return r.openFilter()
		}
	case "?":
		if _, ok := top.(*helpView); ok {
			return r.pop()
		}
		return r.openHelp()
	case "ctrl+a":
		if _, ok := top.(*namesView); ok {
			return nil
		}
		return r.openNames()
	case "ctrl+e":
		r.noHeader = !r.noHeader
		return nil
	case "[":
		return r.historyBack()
	case "]":
		return r.historyForward()
	case "-":
		return r.historyLast()
	}
	return r.updateTop(k)
}

// paste goes where a key would.
func (r *root) paste(p tea.PasteMsg) tea.Cmd {
	if r.dlg != nil {
		var cmd tea.Cmd
		r.dlg, cmd = r.dlg.Update(p)
		return cmd
	}
	if r.prompt.open() {
		return r.promptPaste(p)
	}
	return r.updateTop(p)
}

// rootHints are the root's keys, for the help.
func rootHints() []hint {
	return []hint{
		{":", "Command"},
		{"/", "Filter"},
		{"?", "Help"},
		{"ctrl-a", "Aliases"},
		{"ctrl-e", "Toggle Header"},
		{"[", "History Back"},
		{"]", "History Forward"},
		{"-", "Last View"},
		{"esc", "Back"},
		{"ctrl-c", "Quit"},
	}
}

// View draws the screen.
func (r *root) View() tea.View {
	v := tea.NewView(r.render())
	v.AltScreen = true
	return v
}

func (r *root) render() string {
	if r.w <= 0 || r.h <= 0 {
		return ""
	}
	if r.w < minWidth || r.h < minHeight {
		return fit(fmt.Sprintf("kosh needs a terminal of at least %d by %d (this one is %dx%d)", minWidth, minHeight, r.w, r.h), r.w)
	}
	top := r.top()
	if top != nil && top.Fullscreen() {
		// The view has the whole window, less the last line while the
		// prompt is open.
		h := r.h
		if r.prompt.open() {
			h--
		}
		lines := block(top.View(r.w, h), r.w, h)
		if r.prompt.open() {
			lines = append(lines, r.prompt.line(r.w))
		}
		r.overlayDialog(lines)
		return strings.Join(lines, "\n")
	}
	bodyTop := 0
	if !r.noHeader {
		bodyTop += headerHeight
	}
	if r.prompt.open() {
		bodyTop++
	}
	// The lines of an error in the flash are taken from the body, which
	// keeps its frame and a line inside it.
	fl := r.flash.lines(r.w, min(flashMaxLines, r.h-bodyTop-1-3), r.e.now())
	bodyH := r.h - bodyTop - 1 - len(fl)
	// The body is drawn before the header, whose hints may depend on how
	// the view was drawn (the help's on whether a column is cut).
	body := r.body(bodyH)
	var lines []string
	if !r.noHeader {
		lines = append(lines, r.header()...)
	}
	if r.prompt.open() {
		lines = append(lines, r.prompt.line(r.w))
	}
	lines = append(lines, body...)
	lines = append(lines, r.crumbs())
	lines = append(lines, fl...)
	r.overlayDialog(lines)
	return strings.Join(lines, "\n")
}

// body draws the top view in its frame, in h lines.
func (r *root) body(h int) []string {
	w := r.w
	iw, ih := w-2, h-2
	var title string
	var inner []string
	if top := r.top(); top != nil {
		title = cleanLine(top.Title())
		inner = block(top.View(iw, ih), iw, ih)
	} else {
		var text []string
		if r.startErr != nil {
			for _, l := range wrapText(errorText(r.startErr.Error()), iw, ih) {
				text = append(text, styleError.Render(fit(l, iw)))
			}
		}
		inner = block(strings.Join(text, "\n"), iw, ih)
	}
	lines := make([]string, 0, h)
	lines = append(lines, topBorder(title, w))
	side := styleBorder.Render("│")
	for _, l := range inner {
		lines = append(lines, side+l+side)
	}
	lines = append(lines, styleBorder.Render("└"+strings.Repeat("─", iw)+"┘"))
	return lines
}

// topBorder is the frame's top line, with the title in its middle.
func topBorder(title string, w int) string {
	iw := w - 2
	if title == "" {
		return styleBorder.Render("┌" + strings.Repeat("─", iw) + "┐")
	}
	text := " " + ansi.Truncate(title, max(iw-4, 1), "…") + " "
	tw := ansi.StringWidth(text)
	left := (iw - tw) / 2
	right := iw - tw - left
	return styleBorder.Render("┌"+strings.Repeat("─", left)) + titleStyled(text) + styleBorder.Render(strings.Repeat("─", right)+"┐")
}

// titleStyled colours a title of the form "Name(of)[count] rest" in its
// parts; another title is drawn in the title style.
func titleStyled(t string) string {
	i := strings.Index(t, "(")
	j := strings.LastIndex(t, ")[")
	if i < 0 || j < i {
		return styleTitle.Render(t)
	}
	k := strings.Index(t[j:], "]")
	if k < 0 {
		return styleTitle.Render(t)
	}
	k += j
	return styleTitle.Render(t[:i+1]) + styleTitleOf.Render(t[i+1:j]) + styleTitle.Render(")[") +
		styleTitleCount.Render(t[j+2:k]) + styleTitle.Render("]") + styleTitleExtra.Render(t[k+1:])
}

// block makes s exactly h lines of exactly w cells.
func block(s string, w, h int) []string {
	if h <= 0 {
		return nil
	}
	lines := strings.SplitN(s, "\n", h+1)
	if len(lines) > h {
		lines = lines[:h]
	}
	out := make([]string, h)
	for i := range out {
		if i < len(lines) {
			out[i] = fit(lines[i], w)
		} else {
			out[i] = fit("", w)
		}
	}
	return out
}

// dialogBoxWidth is the width a dialog is drawn in at most.
func (r *root) dialogBoxWidth() int { return max(r.w-4, 1) }

// dialogFits reports whether d can be shown on the screen: the screen is
// not too small for the frame, and has the lines for d's fields and
// buttons.
func (r *root) dialogFits(d dialog) bool {
	return r.w >= minWidth && r.h >= minHeight && d.Fits(r.dialogBoxWidth(), r.h)
}

// overlayDialog draws the dialog, when there is one, centred over the
// whole screen, whose lines are lines: no taller than they are.
func (r *root) overlayDialog(lines []string) {
	h := len(lines)
	if r.dlg == nil || h <= 0 {
		return
	}
	boxW := r.dialogBoxWidth()
	box := strings.Split(r.dlg.View(boxW, h), "\n")
	if len(box) > h {
		box = box[len(box)-h:]
	}
	bw := 0
	for i, l := range box {
		box[i] = ansi.Truncate(l, boxW, "")
		bw = max(bw, ansi.StringWidth(box[i]))
	}
	x := (r.w - bw) / 2
	y := (h - len(box)) / 2
	for i, l := range box {
		at := y + i
		if at < 0 || at >= len(lines) {
			continue
		}
		base := lines[at]
		left := fit(ansi.Truncate(base, x, ""), x)
		right := ansi.TruncateLeft(base, x+bw, "")
		lines[at] = fit(left+ansi.ResetStyle+fit(l, bw)+ansi.ResetStyle+right, r.w)
	}
}

// waiter is a command that waits for something outside the program: a
// watch's change, a log's lines, a time. Every such command is made
// with waitCmd, so that the tests' harness knows to leave it running.
type waiter struct{ fn func() tea.Msg }

func (w waiter) run() tea.Msg { return w.fn() }

// waitCmd returns a command that waits, as fn does.
//
// The rule: every command that waits for something outside the program,
// or that re-arms itself, is made with waitCmd. That includes a timer
// and a refresh: do not return tea.Tick or tea.Every, but a waitCmd that
// sleeps or selects on a timer. The tests' harness leaves a waitCmd
// running and takes its message when a test awaits it; any other
// command it runs to its end at once, and a test whose commands do not
// come to an end fails. A waitCmd is not put into a tea.Sequence.
func waitCmd(fn func() tea.Msg) tea.Cmd { return waiter{fn}.run }

// lastID numbers the views, which put their numbers into their
// messages.
var lastID atomic.Int64

// newID returns a number no other view has.
func newID() int64 { return lastID.Add(1) }
