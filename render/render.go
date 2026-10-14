// Package render turns objects of the cluster into the rows of kosh's
// tables: one renderer for each kind that kosh knows by itself.
//
// A renderer is a pure function of the object and the time. It knows
// nothing of the terminal and nothing of the API server. The columns of
// a kind, their order and which of them are shown only wide follow k9s
// (internal/render at commit 8f29fba), less the columns that need GPUs,
// a vulnerability scan or a port-forward. The columns that need
// metrics-server are Metric: a Metered renderer fills them from a
// reading, and StripMetrics takes them away where there is none.
package render

import (
	"fmt"
	"sort"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// Health is what a row says of its object, and what colours it.
type Health int

const (
	// OK: nothing to say.
	OK Health = iota
	// Pending: on its way, and not there yet.
	Pending
	// Error: something is wrong with it. The VALID cell says what.
	Error
	// Completed: it ran and is done.
	Completed
)

func (h Health) String() string {
	switch h {
	case OK:
		return "ok"
	case Pending:
		return "pending"
	case Error:
		return "error"
	case Completed:
		return "completed"
	}
	return fmt.Sprintf("Health(%d)", int(h))
}

// Column is one column of a table.
type Column struct {
	// Name is the header, in capitals: "NAME", "READY".
	Name string
	// Wide: shown only when the table is wide.
	Wide bool
	// Right: the cells are aligned to the right.
	Right bool
	// Numeric: the rows are sorted in this column by Row.Order and not
	// by the text of the cell.
	Numeric bool
	// Metric: the cells come from metrics-server, through Meter. The
	// column is in the table only while the metrics API is served.
	Metric bool
}

// Row is one object in a table.
type Row struct {
	// Namespace and Name are the object's. Namespace is empty for an
	// object that has none.
	Namespace string
	Name      string
	// Cells has one text for each column.
	Cells []string
	// Order has one number for each column. It is read where the column
	// is Numeric: an age in seconds, a count, a quantity.
	Order []float64
	// Health colours the row.
	Health Health
	// Labels are the object's, for a filter by label.
	Labels map[string]string
}

// Renderer makes the rows of one kind.
type Renderer interface {
	// Columns returns the columns, the same at every call. A namespaced
	// kind has NAMESPACE first.
	Columns() []Column
	// Row returns the row of obj at the time now. It is an error when
	// obj is not of the renderer's kind. An object that lacks what a
	// cell needs gives that cell a blank value and no error and no
	// panic: objects are whatever the cluster holds.
	Row(obj runtime.Object, now time.Time) (Row, error)
}

type entry struct {
	renderer Renderer
	zero     func() runtime.Object
}

var (
	mu       sync.RWMutex
	registry = map[schema.GroupVersionResource]entry{}
)

// Register makes r the renderer of gvr. zero returns a new, empty object
// of the kind, which the tests of this package render to see that no
// renderer panics on an object that holds nothing. Every file of a kind
// calls Register from an init function. A second renderer for one gvr is
// a mistake in the program and panics.
func Register(gvr schema.GroupVersionResource, r Renderer, zero func() runtime.Object) {
	mu.Lock()
	defer mu.Unlock()
	if _, dup := registry[gvr]; dup {
		panic(fmt.Sprintf("render: two renderers for %s", gvr))
	}
	registry[gvr] = entry{renderer: r, zero: zero}
}

// For returns the renderer of gvr.
func For(gvr schema.GroupVersionResource) (Renderer, bool) {
	mu.RLock()
	defer mu.RUnlock()
	e, ok := registry[gvr]
	return e.renderer, ok
}

// Zero returns a new, empty object of the kind of gvr.
func Zero(gvr schema.GroupVersionResource) (runtime.Object, bool) {
	mu.RLock()
	defer mu.RUnlock()
	e, ok := registry[gvr]
	if !ok {
		return nil, false
	}
	return e.zero(), true
}

// Kinds returns every gvr that has a renderer, sorted by group and
// resource.
func Kinds() []schema.GroupVersionResource {
	mu.RLock()
	defer mu.RUnlock()
	kinds := make([]schema.GroupVersionResource, 0, len(registry))
	for gvr := range registry {
		kinds = append(kinds, gvr)
	}
	sort.Slice(kinds, func(i, j int) bool {
		if kinds[i].Group != kinds[j].Group {
			return kinds[i].Group < kinds[j].Group
		}
		return kinds[i].Resource < kinds[j].Resource
	})
	return kinds
}
