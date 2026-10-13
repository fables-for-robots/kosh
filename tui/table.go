package tui

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"k8s.io/apimachinery/pkg/labels"

	"github.com/fables-for-robots/kosh/keys"
	"github.com/fables-for-robots/kosh/kube"
	"github.com/fables-for-robots/kosh/render"
)

// How often a table reads its watch: when the watch says it changed, but
// not sooner than tableReadMin after the last reading, and at least
// every tableReadMax. A table takes them when it is made, into fields
// that tests may shorten.
var (
	tableReadMin = 500 * time.Millisecond
	tableReadMax = 2 * time.Second
)

// freshFor is how long a row that came or changed is coloured so.
const freshFor = 2 * time.Second

// The texts of a table without rows.
const (
	textNoRows  = "No resources found"
	textWaiting = "Waiting for the first list…"
)

// The marks of the sorted column in the header.
const (
	arrowAsc  = "↑"
	arrowDesc = "↓"
)

// columnGap is the space between two columns.
const columnGap = "  "

// rowKey is the object of a row.
type rowKey struct{ ns, name string }

// trow is a row as the table keeps it: cleaned, with its cells' widths.
type trow struct {
	render.Row
	key    rowKey
	widths []int
	joined string // the cells, to see a change
}

// ttable is a reading of a watch, cleaned.
type ttable struct {
	cols       []render.Column
	rows       []trow
	namespaced bool
	stale      bool
	err        string
}

// prepare cleans a reading of a watch, the names of its columns
// included, and measures its cells. A cell and a column's name are one
// line: a line feed in them becomes a space.
func prepare(t kube.Table) ttable {
	out := ttable{cols: make([]render.Column, len(t.Columns)), namespaced: t.Namespaced, stale: t.Stale}
	for i, c := range t.Columns {
		c.Name = cleanLine(c.Name)
		out.cols[i] = c
	}
	if t.Err != nil {
		out.err = cleanLine(t.Err.Error())
		return out
	}
	out.rows = make([]trow, len(t.Rows))
	for i, r := range t.Rows {
		cells := make([]string, len(t.Columns))
		widths := make([]int, len(t.Columns))
		for c := range cells {
			if c < len(r.Cells) {
				cells[c] = cleanLine(r.Cells[c])
				widths[c] = ansi.StringWidth(cells[c])
			}
		}
		r.Cells = cells
		r.Namespace = clean(r.Namespace)
		r.Name = clean(r.Name)
		out.rows[i] = trow{Row: r, key: rowKey{r.Namespace, r.Name}, widths: widths, joined: strings.Join(cells, "\x00")}
	}
	return out
}

// cleanLine cleans s and makes it one line.
func cleanLine(s string) string { return strings.ReplaceAll(clean(s), "\n", " ") }

// fresh says when a row came or changed.
type fresh struct {
	at      time.Time
	changed bool
}

// watchBox holds the watch of a table, which a command opens, so that a
// watch opened after its table was closed is closed too.
type watchBox struct {
	mu     sync.Mutex
	w      kube.Watch
	closed bool
}

// set keeps w, and reports false when the box is closed already: the
// caller then closes w.
func (b *watchBox) set(w kube.Watch) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return false
	}
	b.w = w
	return true
}

func (b *watchBox) close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	if b.w != nil {
		b.w.Close()
	}
}

// tableReadMsg is a reading of a table's watch, or why its watch could
// not be opened.
type tableReadMsg struct {
	id    int64
	gen   int
	watch kube.Watch
	table ttable
	err   string
}

// tableView is the table of a query.
type tableView struct {
	id       int64
	e        env
	q        kube.Query
	grant    keys.Grant
	selected string
	// open opens the watch; nil is the session's Watch of q.
	open func() (kube.Watch, error)
	// plain: a list that is not of the cluster's objects, as the names
	// are: no bindings, no owner, no namespaces.
	plain bool

	readMin, readMax time.Duration

	box     *watchBox
	watch   kube.Watch
	stop    chan struct{}
	gen     int
	closed  bool
	buried  bool // another view is over it: it does not read (bury)
	openErr string

	data    ttable
	read    bool
	hadRows bool
	prev    map[rowKey]string
	fresh   map[rowKey]fresh

	wide, faults bool
	filter       string
	match        func(trow) bool
	sortCol      string
	sortDesc     bool
	colAt        int

	vis       []int // the columns shown, as indexes of data.cols
	widths    []int // of the columns shown
	shown     []int // the rows shown, as indexes of data.rows
	cursor    int
	cursorKey *rowKey
	top       int
	page      int
	marks     map[rowKey]bool
	lastMark  *rowKey

	ns namespacesMsg
}

// newTableView returns the table of a query, with the cursor on the
// row of the name selected when that is not empty.
func newTableView(e env, q kube.Query, selected string) view {
	return newTable(e, q, selected)
}

func newTable(e env, q kube.Query, selected string) *tableView {
	v := &tableView{
		id:       newID(),
		e:        e,
		q:        q,
		grant:    e.sess.Grant(),
		selected: selected,
		readMin:  tableReadMin,
		readMax:  tableReadMax,
		page:     10,
		marks:    map[rowKey]bool{},
		fresh:    map[rowKey]fresh{},
	}
	if q.Resource.Is("", "events") || q.Resource.Is("events.k8s.io", "events") {
		v.sortCol = "LAST SEEN"
	}
	return v
}

// Init opens the watch and reads it.
func (v *tableView) Init() tea.Cmd { return v.openWatch() }

// openWatch opens the watch of the query in a command, and reads it.
func (v *tableView) openWatch() tea.Cmd {
	v.box = &watchBox{}
	v.stop = make(chan struct{})
	v.gen++
	box, id, gen, now := v.box, v.id, v.gen, v.e.now
	open := v.open
	if open == nil {
		sess, q := v.e.sess, v.q
		open = func() (kube.Watch, error) { return sess.Watch(q) }
	}
	return func() tea.Msg {
		w, err := open()
		if err != nil {
			return tableReadMsg{id: id, gen: gen, err: cleanLine(err.Error())}
		}
		if !box.set(w) {
			w.Close()
			return nil
		}
		return tableReadMsg{id: id, gen: gen, watch: w, table: prepare(w.Table(now()))}
	}
}

// readNow reads the watch again at once, and ends the wait for the next
// reading.
func (v *tableView) readNow() tea.Cmd {
	if v.watch == nil || v.closed {
		return nil
	}
	close(v.stop)
	v.stop = make(chan struct{})
	v.gen++
	w, id, gen, now := v.watch, v.id, v.gen, v.e.now
	return func() tea.Msg {
		return tableReadMsg{id: id, gen: gen, watch: w, table: prepare(w.Table(now()))}
	}
}

// wait waits for the next reading: until the watch changes, but not
// sooner than readMin after this one, or until readMax has passed.
func (v *tableView) wait() tea.Cmd {
	if v.buried {
		return nil
	}
	w, stop, id, gen, now := v.watch, v.stop, v.id, v.gen, v.e.now
	least, most := v.readMin, v.readMax
	return waitCmd(func() tea.Msg {
		start := time.Now()
		pause := time.NewTimer(least)
		defer pause.Stop()
		select {
		case <-stop:
			return nil
		case <-pause.C:
		}
		limit := time.NewTimer(most - time.Since(start))
		defer limit.Stop()
		select {
		case <-stop:
			return nil
		case <-w.Changed():
		case <-limit.C:
		}
		return tableReadMsg{id: id, gen: gen, watch: w, table: prepare(w.Table(now()))}
	})
}

// Close closes the watch.
func (v *tableView) Close() {
	if v.closed {
		return
	}
	v.closed = true
	if v.stop != nil {
		close(v.stop)
	}
	if v.box != nil {
		v.box.close()
	}
}

// Update takes the table's readings, the namespaces, and its keys.
func (v *tableView) Update(msg tea.Msg) (view, tea.Cmd) {
	switch msg := msg.(type) {
	case tableReadMsg:
		if msg.id != v.id || msg.gen != v.gen || v.closed {
			return v, nil
		}
		if msg.err != "" {
			v.openErr = msg.err
			return v, nil
		}
		v.watch = msg.watch
		v.apply(msg.table)
		return v, v.wait()
	case refreshMsg:
		v.buried = false
		return v, v.readNow()
	case buriedMsg:
		return v, v.bury()
	case namespacesMsg:
		v.ns = msg
		return v, nil
	case tea.KeyPressMsg:
		return v, v.key(msg)
	}
	return v, nil
}

// apply takes a reading.
func (v *tableView) apply(t ttable) {
	now := v.e.now()
	first := !v.read
	v.read = true
	prev := make(map[rowKey]string, len(t.rows))
	for _, r := range t.rows {
		prev[r.key] = r.joined
		if first {
			continue
		}
		if old, ok := v.prev[r.key]; !ok {
			v.fresh[r.key] = fresh{at: now}
		} else if old != r.joined {
			v.fresh[r.key] = fresh{at: now, changed: true}
		}
	}
	for k, f := range v.fresh {
		if _, ok := prev[k]; !ok || now.Sub(f.at) >= freshFor {
			delete(v.fresh, k)
		}
	}
	for k := range v.marks {
		if _, ok := prev[k]; !ok {
			delete(v.marks, k)
		}
	}
	if v.lastMark != nil && !v.marks[*v.lastMark] {
		v.lastMark = nil
	}
	v.prev = prev
	if len(t.rows) > 0 {
		v.hadRows = true
	}
	v.data = t
	v.rebuild()
}

// knownNamespace reports whether the rows are of one namespace that the
// query names.
func (v *tableView) knownNamespace() bool {
	if v.q.Of != nil {
		return v.q.Of.Namespace != ""
	}
	return v.q.Namespace != ""
}

// colIndex returns the index of the column named name, or -1.
func (v *tableView) colIndex(name string) int {
	for i, c := range v.data.cols {
		if c.Name == name {
			return i
		}
	}
	return -1
}

// sortedCol returns the index of the column the rows are sorted by, as
// the header marks it, or -1.
func (v *tableView) sortedCol() int {
	if v.sortCol != "" {
		return v.colIndex(v.sortCol)
	}
	if i := v.colIndex("NAMESPACE"); i >= 0 && v.isShown(i) {
		return i
	}
	return v.colIndex("NAME")
}

func (v *tableView) isShown(col int) bool {
	for _, c := range v.vis {
		if c == col {
			return true
		}
	}
	return false
}

// rebuild works out what is shown: the columns, the rows, their order,
// the widths, and where the cursor is.
func (v *tableView) rebuild() {
	v.vis = v.vis[:0]
	for i, c := range v.data.cols {
		if c.Wide && !v.wide {
			continue
		}
		if i == 0 && c.Name == "NAMESPACE" && v.data.namespaced && v.knownNamespace() {
			continue
		}
		v.vis = append(v.vis, i)
	}
	v.colAt = max(min(v.colAt, len(v.vis)-1), 0)

	v.shown = v.shown[:0]
	for i, r := range v.data.rows {
		if v.faults && r.Health != render.Error {
			continue
		}
		if v.match != nil && !v.match(r) {
			continue
		}
		v.shown = append(v.shown, i)
	}
	v.sortRows()

	v.widths = make([]int, len(v.vis))
	sorted := v.sortedCol()
	for j, c := range v.vis {
		v.widths[j] = ansi.StringWidth(v.data.cols[c].Name)
		if c == sorted {
			v.widths[j]++
		}
		for _, i := range v.shown {
			v.widths[j] = max(v.widths[j], v.data.rows[i].widths[c])
		}
	}

	// The cursor stays on its object, or at its place.
	at := -1
	if v.cursorKey != nil {
		at = v.indexOf(*v.cursorKey)
	}
	if at < 0 && v.selected != "" && len(v.shown) > 0 {
		for j, i := range v.shown {
			if v.data.rows[i].Name == v.selected {
				at = j
				break
			}
		}
		v.selected = ""
	}
	if at < 0 {
		at = v.cursor
	}
	v.setCursor(at)
}

// indexOf returns where the row of k is shown, or -1.
func (v *tableView) indexOf(k rowKey) int {
	for j, i := range v.shown {
		if v.data.rows[i].key == k {
			return j
		}
	}
	return -1
}

// setCursor puts the cursor on the shown row at, within the rows.
func (v *tableView) setCursor(at int) {
	v.cursor = max(min(at, len(v.shown)-1), 0)
	if len(v.shown) == 0 {
		v.cursorKey = nil
		return
	}
	k := v.data.rows[v.shown[v.cursor]].key
	v.cursorKey = &k
}

// sortRows sorts the rows shown: by the sort column, in its direction,
// and then by namespace and name. A Numeric column sorts by Row.Order.
func (v *tableView) sortRows() {
	col := -1
	if v.sortCol != "" {
		col = v.colIndex(v.sortCol)
	}
	numeric := col >= 0 && v.data.cols[col].Numeric
	rows := v.data.rows
	sort.SliceStable(v.shown, func(a, b int) bool {
		ra, rb := &rows[v.shown[a]], &rows[v.shown[b]]
		if col >= 0 {
			var c int
			if numeric {
				c = compareFloat(orderAt(ra, col), orderAt(rb, col))
			} else {
				c = strings.Compare(ra.Cells[col], rb.Cells[col])
			}
			if c != 0 {
				return (c < 0) != v.sortDesc
			}
		}
		if ra.Namespace != rb.Namespace {
			return ra.Namespace < rb.Namespace
		}
		return ra.Name < rb.Name
	})
}

func orderAt(r *trow, col int) float64 {
	if col < len(r.Order) {
		return r.Order[col]
	}
	return 0
}

func compareFloat(a, b float64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// sortBy sorts by the column named name, when the table has it: up at
// first, and the other way when it is sorted by it already.
func (v *tableView) sortBy(name string) {
	if v.colIndex(name) < 0 {
		return
	}
	if v.sortCol == name {
		v.sortDesc = !v.sortDesc
	} else {
		v.sortCol, v.sortDesc = name, false
	}
	v.rebuild()
}

// SetFilter keeps the rows that match text: a regular expression over
// the cells, without regard to case, or the text as it stands when it is
// no regular expression; "!" in front keeps those that do not match;
// "-l selector" keeps those whose labels match the label selector. A
// selector that does not parse keeps nothing, and the flash says so.
func (v *tableView) SetFilter(text string) tea.Cmd {
	v.filter = text
	match, err := compileFilter(text)
	v.match = match
	v.rebuild()
	if err != nil {
		return flashErr(err)
	}
	return nil
}

// compileFilter returns what keeps the rows a filter keeps: nil for
// every row.
func compileFilter(text string) (func(trow) bool, error) {
	if text == "" {
		return nil, nil
	}
	if rest, ok := strings.CutPrefix(text, "-l "); ok {
		sel, err := labels.Parse(strings.TrimSpace(rest))
		if err != nil {
			return func(trow) bool { return false }, fmt.Errorf("%q is not a label selector: %w", text, err)
		}
		return func(r trow) bool { return sel.Matches(labels.Set(r.Labels)) }, nil
	}
	invert := false
	if rest, ok := strings.CutPrefix(text, "!"); ok {
		invert, text = true, rest
	}
	re, err := regexp.Compile("(?i)" + text)
	if err != nil {
		re = regexp.MustCompile("(?i)" + regexp.QuoteMeta(text))
	}
	return func(r trow) bool {
		for _, c := range r.Cells {
			if re.MatchString(c) {
				return !invert
			}
		}
		return invert
	}, nil
}

// Title is "Pods(default)[12]": the resource, what the rows are of, how
// many are shown, the filter, and whether the rows may be out of date.
func (v *tableView) Title() string {
	of := v.q.Namespace
	if v.q.Of != nil {
		of = v.q.Of.String()
	} else if of == "" || !v.q.Resource.Namespaced {
		of = "all"
	}
	t := fmt.Sprintf("%s(%s)[%d]", capital(v.q.Resource.GVR.Resource), of, len(v.shown))
	if v.filter != "" {
		t += " </" + v.filter + ">"
	}
	if v.data.stale {
		t += " (stale)"
	}
	return t
}

// capital returns s with its first letter a capital.
func capital(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// Crumb is the resource's plural name.
func (v *tableView) Crumb() string { return v.q.Resource.GVR.Resource }

func (v *tableView) Capturing() bool  { return false }
func (v *tableView) Fullscreen() bool { return false }

// hasShortcuts reports whether the namespace shortcuts apply: for a
// namespaced resource of no other object.
func (v *tableView) hasShortcuts() bool {
	return !v.plain && v.q.Resource.Namespaced && v.q.Of == nil
}

// View draws the header and the rows that fit, or why there are none,
// in exactly h lines of w cells.
func (v *tableView) View(w, h int) string {
	return strings.Join(block(v.draw(w, h), w, h), "\n")
}

// draw draws what View shows, in at most h lines.
func (v *tableView) draw(w, h int) string {
	if w <= 0 || h <= 0 {
		return ""
	}
	if v.openErr != "" {
		return styleError.Render(fit(v.openErr, w))
	}
	if v.data.err != "" {
		return styleError.Render(fit(v.data.err, w))
	}
	var lines []string
	if len(v.vis) > 0 {
		lines = append(lines, v.headerLine(w))
	}
	rows := h - len(lines)
	v.page = max(rows, 1)
	if len(v.shown) == 0 {
		text := textNoRows
		if !v.read || (v.data.stale && !v.hadRows) {
			text = textWaiting
		}
		lines = append(lines, styleText.Render(fit(text, w)))
		if len(lines) > h {
			// One line: the text says more than the header.
			lines = lines[1:]
		}
		return strings.Join(lines, "\n")
	}
	if v.cursor < v.top {
		v.top = v.cursor
	}
	if v.cursor >= v.top+rows {
		v.top = v.cursor - rows + 1
	}
	v.top = max(min(v.top, len(v.shown)-rows), 0)
	now := v.e.now()
	for j := v.top; j < len(v.shown) && j < v.top+rows; j++ {
		r := &v.data.rows[v.shown[j]]
		lines = append(lines, v.rowStyle(r, j, now).Render(fit(v.rowLine(r), w)))
	}
	return strings.Join(lines, "\n")
}

// headerLine draws the names of the columns, the sorted one with its
// arrow and the one under the column cursor marked.
func (v *tableView) headerLine(w int) string {
	sorted := v.sortedCol()
	var b strings.Builder
	for j, c := range v.vis {
		if j > 0 {
			b.WriteString(columnGap)
		}
		col := v.data.cols[c]
		label := styleColumn.Render(col.Name)
		if j == v.colAt {
			label = styleColumnAt.Render(col.Name)
		}
		lw := ansi.StringWidth(col.Name)
		if c == sorted {
			arrow := arrowAsc
			if v.sortDesc {
				arrow = arrowDesc
			}
			label += styleSorter.Render(arrow)
			lw++
		}
		pad := strings.Repeat(" ", max(v.widths[j]-lw, 0))
		if col.Right {
			b.WriteString(pad + label)
		} else {
			b.WriteString(label + pad)
		}
	}
	return fit(b.String(), w)
}

// rowLine is a row's cells in their columns.
func (v *tableView) rowLine(r *trow) string {
	var b strings.Builder
	for j, c := range v.vis {
		if j > 0 {
			b.WriteString(columnGap)
		}
		pad := strings.Repeat(" ", max(v.widths[j]-r.widths[c], 0))
		if v.data.cols[c].Right {
			b.WriteString(pad + r.Cells[c])
		} else {
			b.WriteString(r.Cells[c] + pad)
		}
	}
	return b.String()
}

// rowStyle is the style of a row, the first that applies: the cursor, a
// mark, new or changed in the last freshFor, its health.
func (v *tableView) rowStyle(r *trow, j int, now time.Time) lipgloss.Style {
	switch {
	case j == v.cursor:
		return styleCursor
	case v.marks[r.key]:
		return styleMark
	}
	if f, ok := v.fresh[r.key]; ok && now.Sub(f.at) < freshFor {
		if f.changed {
			return styleRowChanged
		}
		return styleRowNew
	}
	return styleOfHealth(r.Health)
}
