package render

import (
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// testNow is the time the tests of this package render at.
var testNow = time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)

// ago returns the time d before testNow, as the timestamp of an object.
func ago(d time.Duration) metav1.Time {
	return metav1.NewTime(testNow.Add(-d))
}

// rowOf renders obj with the renderer of gvr at testNow. It returns the
// row, and its cells by the name of their column.
func rowOf(t *testing.T, gvr schema.GroupVersionResource, obj runtime.Object) (Row, map[string]string) {
	t.Helper()
	r, ok := For(gvr)
	if !ok {
		t.Fatalf("no renderer for %s", gvr.Resource)
	}
	row, err := r.Row(obj, testNow)
	if err != nil {
		t.Fatalf("Row: %v", err)
	}
	cols := r.Columns()
	if len(row.Cells) != len(cols) || len(row.Order) != len(cols) {
		t.Fatalf("%d cells and %d numbers for %d columns", len(row.Cells), len(row.Order), len(cols))
	}
	cells := make(map[string]string, len(cols))
	for i, c := range cols {
		cells[c.Name] = row.Cells[i]
	}
	return row, cells
}

// orderOf returns the number the row is sorted by in the column.
func orderOf(t *testing.T, gvr schema.GroupVersionResource, row Row, column string) float64 {
	t.Helper()
	r, _ := For(gvr)
	for i, c := range r.Columns() {
		if c.Name == column {
			if !c.Numeric {
				t.Fatalf("column %s is not numeric", column)
			}
			return row.Order[i]
		}
	}
	t.Fatalf("no column %s", column)
	return 0
}

// wantCells fails the test for every column of want whose cell is
// another.
func wantCells(t *testing.T, got, want map[string]string) {
	t.Helper()
	for name, w := range want {
		g, ok := got[name]
		if !ok {
			t.Errorf("no column %s", name)
			continue
		}
		if g != w {
			t.Errorf("%s = %q, want %q", name, g, w)
		}
	}
}

// wantColumns fails the test when the columns of the renderer of gvr are
// not the ones named, in that order. A name with a star at its end is a
// column that is shown only wide, as the spec's appendix writes them:
//
//	wantColumns(t, Deployments, "NAMESPACE", "NAME", "READY", "UP-TO-DATE", "AVAILABLE", "LABELS*", "VALID*", "AGE")
func wantColumns(t *testing.T, gvr schema.GroupVersionResource, want ...string) {
	t.Helper()
	r, ok := For(gvr)
	if !ok {
		t.Fatalf("no renderer for %s", gvr.Resource)
	}
	wantColumnList(t, r.Columns(), want...)
}

// wantColumnList is wantColumns for columns that belong to no renderer:
// those of the containers and of the rules.
func wantColumnList(t *testing.T, cols []Column, want ...string) {
	t.Helper()
	var got []string
	for _, c := range cols {
		name := c.Name
		if c.Wide {
			name += "*"
		}
		got = append(got, name)
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("columns:\n got %s\nwant %s", strings.Join(got, " | "), strings.Join(want, " | "))
	}
}
