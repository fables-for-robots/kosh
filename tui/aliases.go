package tui

import (
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/fables-for-robots/kosh/kube"
	"github.com/fables-for-robots/kosh/render"
)

// aliasesResource is what the list of names is a table of. It is no
// resource of the cluster.
var aliasesResource = kube.Resource{
	GVR:  schema.GroupVersionResource{Group: "kosh", Version: "v1", Resource: "aliases"},
	Kind: "Alias",
}

// namesView is the list of the cluster's resources: a table of its own
// rows, which enter goes to as a command would.
type namesView struct {
	*tableView
}

// openNames pushes the list of names.
func (r *root) openNames() tea.Cmd {
	return pushView(newNamesView(r.e))
}

func newNamesView(e env) *namesView {
	t := newTable(e, kube.Query{Resource: aliasesResource}, "")
	t.plain = true
	sess := e.sess
	t.open = func() (kube.Watch, error) {
		return staticWatch{t: namesTable(sess.Resources())}, nil
	}
	return &namesView{tableView: t}
}

// namesTable is the table of resources: the plural name, the short
// names, the kind, the group, whether namespaced. A row's Name is the
// command that goes to it: the plural name, with the group when another
// resource has the same plural name.
func namesTable(rr []kube.Resource) kube.Table {
	count := map[string]int{}
	for _, r := range rr {
		count[r.GVR.Resource]++
	}
	t := kube.Table{Columns: []render.Column{{Name: "NAME"}, {Name: "SHORTNAMES"}, {Name: "KIND"}, {Name: "GROUP"}, {Name: "NAMESPACED"}}}
	for _, r := range rr {
		name := r.GVR.Resource
		if count[name] > 1 && r.GVR.Group != "" {
			name += "." + r.GVR.Group
		}
		t.Rows = append(t.Rows, render.Row{
			Name:  name,
			Cells: []string{r.GVR.Resource, strings.Join(r.ShortNames, ","), r.Kind, r.GVR.Group, fmt.Sprint(r.Namespaced)},
		})
	}
	return t
}

// Update goes to the cursor's resource on enter; everything else is the
// table's.
func (n *namesView) Update(msg tea.Msg) (view, tea.Cmd) {
	if k, ok := msg.(tea.KeyPressMsg); ok && k.String() == "enter" {
		r := n.cursorRow()
		if r == nil {
			return n, nil
		}
		text := r.Name
		return n, func() tea.Msg { return commandMsg{text: text} }
	}
	_, cmd := n.tableView.Update(msg)
	return n, cmd
}

// Title is "Aliases[27]", with the filter.
func (n *namesView) Title() string {
	t := fmt.Sprintf("Aliases[%d]", len(n.shown))
	if n.filter != "" {
		t += " </" + n.filter + ">"
	}
	return t
}

func (n *namesView) Crumb() string { return "aliases" }

// staticWatch is a watch of a table that does not change.
type staticWatch struct{ t kube.Table }

func (w staticWatch) Table(time.Time) kube.Table { return w.t }
func (w staticWatch) Changed() <-chan struct{}   { return nil }
func (w staticWatch) Close()                     {}
