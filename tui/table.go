package tui

import (
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/fables-for-robots/kosh/kube"
)

var (
	tableReadMin = 500 * time.Millisecond
	tableReadMax = 2 * time.Second
)

type tableView struct {
	e env
	q kube.Query
}

func newTableView(e env, q kube.Query, selected string) view { return &tableView{e: e, q: q} }

func (v *tableView) Init() tea.Cmd                      { return nil }
func (v *tableView) Update(msg tea.Msg) (view, tea.Cmd) { return v, nil }
func (v *tableView) View(w, h int) string               { return "" }
func (v *tableView) Title() string {
	r := v.q.Resource.GVR.Resource
	return strings.ToUpper(r[:1]) + r[1:] + "(" + v.q.Namespace + ")[0]"
}
func (v *tableView) Crumb() string      { return v.q.Resource.GVR.Resource }
func (v *tableView) Hints() []hint      { return nil }
func (v *tableView) Capturing() bool    { return false }
func (v *tableView) Fullscreen() bool   { return false }
func (v *tableView) Close()             {}
func (v *tableView) hasShortcuts() bool { return v.q.Resource.Namespaced && v.q.Of == nil }
