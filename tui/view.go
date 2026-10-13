package tui

import (
	"sort"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/fables-for-robots/kosh/keys"
	"github.com/fables-for-robots/kosh/kube"
)

// This file is the frame the parts of the UI are hung in. The root
// model owns the screen: the header, the prompt, the crumbs and the
// flash line, a stack of views of which the top one fills the body, and
// at most one dialog over it. Views and dialogs ask the root for things
// by returning a command that yields one of the messages below.

// view is one screen of the body: a table, a text, a log, the editor.
type view interface {
	// Init returns what the view needs done when it is first shown.
	Init() tea.Cmd
	// Update handles a message. The root gives a view every message it
	// does not consume itself, the top view only.
	Update(msg tea.Msg) (view, tea.Cmd)
	// View draws the view into width by height cells: exactly height
	// lines, none wider than width.
	View(width, height int) string
	// Title is the text in the frame's top line: "Pods(default)[12]".
	Title() string
	// Crumb is the view's name in the crumbs: "pods", "logs".
	Crumb() string
	// Hints are the keys of the view, for the header and the help, in
	// the order they are shown.
	Hints() []hint
	// Capturing is true while the view reads text of its own, as a
	// search does. The root then gives it every key, and handles none
	// of its own but ctrl+c.
	Capturing() bool
	// Fullscreen is true while the view wants the whole screen: the
	// root then draws the view alone, without the header, the frame,
	// the crumbs and the flash line.
	Fullscreen() bool
	// Close releases what the view holds: a watch, a log stream. The
	// root calls it once, when the view leaves the stack.
	Close()
}

// filterable is a view that / filters or searches in. The root reads
// the text in its prompt and gives it to the view as it is typed, and
// the empty text when esc ends the prompt; enter ends the prompt and
// leaves the view with the text it has.
type filterable interface {
	SetFilter(text string) tea.Cmd
}

// hint is one key and what it does: {"ctrl-d", "Delete"}.
type hint struct {
	Key  string
	Text string
}

// dialog is a box over the body that has every key until it closes
// itself with closeDialogMsg.
type dialog interface {
	Init() tea.Cmd
	Update(msg tea.Msg) (dialog, tea.Cmd)
	// View draws the box, at most width cells wide. The root centres
	// it.
	View(width int) string
}

// The messages a view or a dialog sends the root.
type (
	// pushViewMsg puts a view on top of the stack and shows it.
	pushViewMsg struct{ v view }
	// popViewMsg takes the top view off the stack. The last view stays.
	popViewMsg struct{}
	// flashMsg shows a line in the flash, in the colour of an error
	// when err is set, until the next one or for 5 seconds.
	flashMsg struct {
		text string
		err  bool
	}
	// openDialogMsg shows a dialog.
	openDialogMsg struct{ d dialog }
	// closeDialogMsg closes the dialog that is shown.
	closeDialogMsg struct{}
	// refreshMsg asks the top view to read its data again now.
	refreshMsg struct{}
)

func pushView(v view) tea.Cmd   { return func() tea.Msg { return pushViewMsg{v} } }
func popView() tea.Cmd          { return func() tea.Msg { return popViewMsg{} } }
func flash(text string) tea.Cmd { return func() tea.Msg { return flashMsg{text: text} } }
func flashErr(err error) tea.Cmd {
	return func() tea.Msg { return flashMsg{text: err.Error(), err: true} }
}
func openDialog(d dialog) tea.Cmd { return func() tea.Msg { return openDialogMsg{d} } }
func closeDialog() tea.Cmd        { return func() tea.Msg { return closeDialogMsg{} } }
func refresh() tea.Cmd            { return func() tea.Msg { return refreshMsg{} } }

// env is what a view is made with.
type env struct {
	sess kube.Session
	now  func() time.Time
}

// apiTimeout bounds every call a view makes to the session. No call to
// the session is made in Update or View: each runs in a command, with a
// context of this timeout, and comes back as a message.
const apiTimeout = 30 * time.Second

// binding is one key of a table, beyond moving about in it. The parts
// of the UI add theirs with bind, each from an init function in a file
// of its own; the table view asks bindingsFor which of them a table of
// a resource has.
type binding struct {
	// Key is the key as tea.KeyPressMsg.String() names it: "d", "L",
	// "ctrl+d", "enter".
	Key string
	// Hint is how the header and the help show it: {"ctrl-d", "Delete"}.
	Hint hint
	// Order places the hint among the others: lower first.
	Order int
	// Applies reports whether a table of res has the key for a session
	// with grant. A key that changes the cluster does not apply to a
	// read-only grant.
	Applies func(res kube.Resource, grant keys.Grant) bool
	// Run is called when the key is pressed, with the rows it is for:
	// the marked rows, or the row under the cursor when none is marked.
	// It is not called for a table without rows.
	Run func(e env, q kube.Query, refs []kube.ObjectRef) tea.Cmd
}

var bindings []binding

// bind adds keys to the tables.
func bind(b ...binding) {
	bindings = append(bindings, b...)
}

// bindingsFor returns the keys a table of res has for grant, by Order
// and then by Key.
func bindingsFor(res kube.Resource, grant keys.Grant) []binding {
	var out []binding
	for _, b := range bindings {
		if b.Applies == nil || b.Applies(res, grant) {
			out = append(out, b)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Order != out[j].Order {
			return out[i].Order < out[j].Order
		}
		return out[i].Key < out[j].Key
	})
	return out
}
