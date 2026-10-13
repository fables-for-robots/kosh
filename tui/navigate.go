package tui

import (
	"context"
	"errors"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/fables-for-robots/kosh/kube"
)

// maxRecent is how many namespaces besides all and default have a
// shortcut: 2 to 9.
const maxRecent = 8

// namespaces are the session's current namespace and the namespaces of
// its shortcuts.
type namespaces struct {
	// current is the namespace a command without one goes to. Empty is
	// all of them.
	current string
	// recent are the namespaces other than all and default that the
	// session used, the last first.
	recent []string
}

// use makes ns the current namespace and moves it to the front of the
// others.
func (n *namespaces) use(ns string) {
	n.current = ns
	if ns == "" || ns == "default" {
		return
	}
	n.recent = slices.DeleteFunc(n.recent, func(s string) bool { return s == ns })
	n.recent = append([]string{ns}, n.recent...)
	if len(n.recent) > maxRecent {
		n.recent = n.recent[:maxRecent]
	}
}

// shortcuts returns the namespace of each shortcut, 0 first: all,
// default, then the others.
func (n namespaces) shortcuts() []string {
	return append([]string{"", "default"}, n.recent...)
}

// msg tells the views the namespaces.
func (n namespaces) msg() namespacesMsg {
	return namespacesMsg{current: n.current, shortcuts: n.shortcuts()}
}

// The messages of getting around.
type (
	// namespacesMsg tells every view the session's current namespace
	// and the namespace of each shortcut, 0 first. The root sends it to
	// a view when the view is pushed, and to all of them when the
	// namespaces change.
	namespacesMsg struct {
		current   string
		shortcuts []string
	}
	// useNamespaceMsg asks the root to make ns the current namespace.
	useNamespaceMsg struct{ ns string }
	// commandMsg asks the root to run text as a command typed at the
	// prompt.
	commandMsg struct{ text string }
	// resolvedMsg is the outcome of resolving the name of a command.
	resolvedMsg struct {
		text   string // the command, to be put into the history
		name   string
		ns     string
		hasNS  bool
		record bool
		start  bool
		res    kube.Resource
		err    error
	}
)

// useNamespace makes ns the current namespace and tells the views.
func (r *root) useNamespace(ns string) tea.Cmd {
	r.ns.use(ns)
	return r.broadcast(r.ns.msg())
}

// runCommand runs a command of the prompt: a name and optionally a
// namespace. record puts it into the history when it goes to a
// resource.
func (r *root) runCommand(text string, record bool) tea.Cmd {
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return nil
	}
	switch fields[0] {
	case "q", "q!", "qa", "Q":
		return tea.Quit
	case "alias", "a":
		return r.openNames()
	case "h", "help", "?":
		return r.openHelp()
	}
	ns := ""
	if len(fields) > 1 {
		ns = fields[1]
	}
	return r.resolve(strings.Join(fields, " "), fields[0], ns, len(fields) > 1, record)
}

// resolve asks the session for the resource of name, in a command.
func (r *root) resolve(text, name, ns string, hasNS, record bool) tea.Cmd {
	sess := r.e.sess
	start := len(r.stack) == 0
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), apiTimeout)
		defer cancel()
		res, err := sess.Resolve(ctx, name)
		return resolvedMsg{text: text, name: name, ns: ns, hasNS: hasNS, record: record, start: start, res: res, err: err}
	}
}

// resolved replaces the stack by the table of the resource a command
// named, or says why there is none.
func (r *root) resolved(msg resolvedMsg) tea.Cmd {
	if msg.err != nil {
		if msg.start && len(r.stack) == 0 {
			r.startErr = msg.err
		}
		if errors.Is(msg.err, kube.ErrUnknownResource) {
			return flashErr(errors.New("no such resource: " + msg.name))
		}
		return flashErr(msg.err)
	}
	var cmds []tea.Cmd
	if msg.hasNS {
		cmds = append(cmds, r.useNamespace(msg.ns))
	}
	q := kube.Query{Resource: msg.res}
	if msg.res.Namespaced {
		q.Namespace = r.ns.current
	}
	r.startErr = nil
	if msg.record {
		r.hist.add(msg.text)
	}
	return tea.Batch(append(cmds, r.replace(newTableView(r.e, q, "")))...)
}
