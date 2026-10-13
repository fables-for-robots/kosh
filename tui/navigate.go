package tui

import (
	"context"
	"errors"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

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
	// prompt. With literal set its second word is the name of a
	// namespace as the cluster has it: all is the namespace named all,
	// not all of them.
	commandMsg struct {
		text    string
		literal bool
	}
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
// namespace, where "all" and "-A" are all of them, as 0 is, unless
// literal is set. record puts it into the history when it goes to a
// resource. A namespace that is not a namespace's name (isNamespaceName)
// is said in the flash, and nothing is opened.
func (r *root) runCommand(text string, record, literal bool) tea.Cmd {
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
		// What follows the name, as one word: a space in it is no name.
		word := strings.Join(fields[1:], " ")
		switch {
		case !literal && (word == "all" || word == "-A"):
			// A namespace named all is chosen from the namespaces'
			// table, which sends it as literal.
		case isNamespaceName(word):
			ns = word
		default:
			text := "not a namespace: " + ansi.Truncate(cleanLine(word), namespaceShown, "…")
			return func() tea.Msg { return flashMsg{text: text, err: true} }
		}
	}
	return r.resolve(strings.Join(fields, " "), fields[0], ns, len(fields) > 1, record, false)
}

// namespaceShown is how many cells of a word that is not a namespace's
// name the flash shows.
const namespaceShown = 40

// isNamespaceName reports whether s is a namespace's name: a DNS-1123
// label, of lower-case letters, digits and "-", beginning and ending
// with a letter or a digit, of at most 63 characters. The UI sends no
// other namespace to the session, which puts it into the path of a
// request.
func isNamespaceName(s string) bool {
	if len(s) == 0 || len(s) > 63 {
		return false
	}
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case c == '-' && i > 0 && i < len(s)-1:
		default:
			return false
		}
	}
	return true
}

// resolve asks the session for the resource of name, in a command. start
// is the resolving of the session's first table, whose failure the body
// shows.
func (r *root) resolve(text, name, ns string, hasNS, record, start bool) tea.Cmd {
	sess := r.e.sess
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
		if msg.start {
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

// ref returns the reference of a row: of the table's resource, in the
// row's namespace, or for a container the pod's.
func (v *tableView) ref(r *trow) kube.ObjectRef {
	ref := kube.ObjectRef{Resource: v.q.Resource, Namespace: r.Namespace, Name: r.Name}
	if v.q.Resource.Is(kube.Containers.GVR.Group, kube.Containers.GVR.Resource) && v.q.Of != nil {
		ref.Namespace = v.q.Of.Namespace
	}
	return ref
}

// cursorRow returns the row under the cursor, or nil.
func (v *tableView) cursorRow() *trow {
	if len(v.shown) == 0 {
		return nil
	}
	return &v.data.rows[v.shown[v.cursor]]
}

// enter goes into the cursor's row: to the rows that belong to it, to
// the pods of a namespace, to the objects of a custom resource, or runs
// its binding of l (a container) or d (anything else).
func (v *tableView) enter() tea.Cmd {
	r := v.cursorRow()
	if r == nil || v.plain {
		return nil
	}
	ref := v.ref(r)
	res := v.q.Resource
	isAny := func(group string, names ...string) bool {
		return slices.ContainsFunc(names, func(n string) bool { return res.Is(group, n) })
	}
	switch {
	case isAny("apps", "deployments", "replicasets", "statefulsets", "daemonsets"),
		isAny("batch", "jobs"), isAny("", "services", "nodes"):
		return v.openOf("pods", ref)
	case isAny("batch", "cronjobs"):
		return v.openOf("jobs", ref)
	case isAny("", "pods"):
		return pushView(newTableView(v.e, kube.Query{Resource: kube.Containers, Of: &ref}, ""))
	case isAny("rbac.authorization.k8s.io", "roles", "clusterroles", "rolebindings", "clusterrolebindings"):
		return pushView(newTableView(v.e, kube.Query{Resource: kube.Rules, Of: &ref}, ""))
	case isAny("", "namespaces"):
		return func() tea.Msg { return commandMsg{text: "pods " + r.Name, literal: true} }
	case isAny("apiextensions.k8s.io", "customresourcedefinitions"):
		return v.openResource(r.Name, v.ns.current)
	case isAny(kube.Containers.GVR.Group, kube.Containers.GVR.Resource):
		return v.runBindingOn("l", ref)
	}
	return v.runBindingOn("d", ref)
}

// runBindingOn runs the binding of key with ref, when there is one.
func (v *tableView) runBindingOn(key string, ref kube.ObjectRef) tea.Cmd {
	if b, ok := v.bindingOf(key); ok {
		return b.Run(v.e, v.q, []kube.ObjectRef{ref})
	}
	return nil
}

// openOf resolves name and pushes the table of its rows of ref, in a
// command.
func (v *tableView) openOf(name string, ref kube.ObjectRef) tea.Cmd {
	e := v.e
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), apiTimeout)
		defer cancel()
		res, err := e.sess.Resolve(ctx, name)
		if err != nil {
			return flashMsg{text: err.Error(), err: true}
		}
		return pushViewMsg{newTableView(e, kube.Query{Resource: res, Of: &ref}, "")}
	}
}

// openResource resolves name and pushes the table of that resource in
// the namespace ns, in a command.
func (v *tableView) openResource(name, ns string) tea.Cmd {
	e := v.e
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), apiTimeout)
		defer cancel()
		res, err := e.sess.Resolve(ctx, name)
		if err != nil {
			return flashMsg{text: err.Error(), err: true}
		}
		q := kube.Query{Resource: res}
		if res.Namespaced {
			q.Namespace = ns
		}
		return pushViewMsg{newTableView(e, q, "")}
	}
}

// owner pushes the table of the owner of the cursor's row, with the
// cursor on the owner, in a command. An object without an owner is said
// in the flash.
func (v *tableView) owner() tea.Cmd {
	r := v.cursorRow()
	if r == nil || !v.hasOwner() {
		return nil
	}
	e, ref := v.e, v.ref(r)
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), apiTimeout)
		defer cancel()
		owner, err := e.sess.Owner(ctx, ref)
		if errors.Is(err, kube.ErrNoOwner) {
			return flashMsg{text: ref.String() + " has no owner"}
		}
		if err != nil {
			return flashMsg{text: err.Error(), err: true}
		}
		q := kube.Query{Resource: owner.Resource}
		if owner.Resource.Namespaced {
			q.Namespace = owner.Namespace
		}
		return pushViewMsg{newTableView(e, q, owner.Name)}
	}
}

// hasOwner reports whether the rows are objects of the cluster, which
// may have an owner: not the containers, the rules or the names.
func (v *tableView) hasOwner() bool {
	return !v.plain && v.q.Resource.GVR.Group != kube.Containers.GVR.Group
}

// useShortcut switches the table to the namespace of shortcut i, and
// makes it the session's: it closes the watch and opens the one of the
// new namespace.
func (v *tableView) useShortcut(i int) tea.Cmd {
	if !v.hasShortcuts() || i >= len(v.ns.shortcuts) {
		return nil
	}
	ns := v.ns.shortcuts[i]
	if v.stop != nil {
		close(v.stop)
		v.stop = nil
	}
	if v.box != nil {
		v.box.close()
	}
	v.q.Namespace = ns
	v.watch, v.openErr = nil, ""
	v.data, v.read, v.hadRows = ttable{}, false, false
	v.prev, v.fresh = nil, map[rowKey]fresh{}
	v.marks, v.lastMark = map[rowKey]bool{}, nil
	v.cursor, v.cursorKey, v.top = 0, nil, 0
	v.rebuild()
	return tea.Batch(v.openWatch(), func() tea.Msg { return useNamespaceMsg{ns} })
}
