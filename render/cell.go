package render

import (
	"sort"
	"strconv"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/duration"
)

// The values of a cell that has nothing to show, as k9s prints them.
const (
	// MissingValue: the object does not have it.
	MissingValue = "<none>"
	// NAValue: it does not apply to this object.
	NAValue = "n/a"
	// UnknownValue: it should be there and is not known.
	UnknownValue = "<unknown>"
)

// B builds a row cell by cell, in the order of the columns, so that
// Cells and Order stay of one length. Every renderer makes its rows
// with it.
type B struct {
	row Row
}

// NewRow starts the row of an object: its namespace, name and labels.
func NewRow(meta metav1.Object) *B {
	return &B{row: Row{
		Namespace: meta.GetNamespace(),
		Name:      meta.GetName(),
		Labels:    meta.GetLabels(),
	}}
}

// Text adds a cell that is sorted by its text.
func (b *B) Text(s string) *B {
	return b.Num(s, 0)
}

// Num adds a cell of a Numeric column: its text, and what it is sorted
// by.
func (b *B) Num(s string, order float64) *B {
	b.row.Cells = append(b.row.Cells, s)
	b.row.Order = append(b.row.Order, order)
	return b
}

// Int adds a cell that shows n and is sorted by it.
func (b *B) Int(n int64) *B {
	return b.Num(strconv.FormatInt(n, 10), float64(n))
}

// Age adds a cell that shows how long ago t was, as kubectl does ("5d",
// "3h2m"), sorted by the seconds since. A zero t shows UnknownValue.
func (b *B) Age(t time.Time, now time.Time) *B {
	if t.IsZero() {
		return b.Num(UnknownValue, 0)
	}
	d := now.Sub(t)
	return b.Num(duration.HumanDuration(d), d.Seconds())
}

// Map adds a cell that shows a map as "a=1,b=2", sorted by key, or
// MissingValue when the map is empty. It is the LABELS and the SELECTOR
// cell.
func (b *B) Map(m map[string]string) *B {
	return b.Text(MapText(m))
}

// Valid adds the VALID cell: empty when reason is, and reason otherwise,
// which also makes the row an Error.
func (b *B) Valid(reason string) *B {
	if reason != "" {
		b.row.Health = Error
	}
	return b.Text(reason)
}

// Health sets the row's health. Valid with a reason overrides it when it
// is called later.
func (b *B) Health(h Health) *B {
	b.row.Health = h
	return b
}

// Row returns the row.
func (b *B) Row() Row {
	return b.row
}

// MapText returns a map as "a=1,b=2", sorted by key, or MissingValue
// when the map is empty.
func MapText(m map[string]string) string {
	if len(m) == 0 {
		return MissingValue
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var sb strings.Builder
	for i, k := range keys {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(k)
		sb.WriteByte('=')
		sb.WriteString(m[k])
	}
	return sb.String()
}

// Join returns the values joined by commas, or MissingValue when there
// are none.
func Join(values []string) string {
	if len(values) == 0 {
		return MissingValue
	}
	return strings.Join(values, ",")
}

// Or returns s, or MissingValue when s is empty.
func Or(s string) string {
	if s == "" {
		return MissingValue
	}
	return s
}
