package render

import (
	"testing"
	"time"
)

// Every renderer gives an object that holds nothing a row with one cell
// and one number for each of its columns, and does not panic.
func TestEveryRendererTakesAnEmptyObject(t *testing.T) {
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	for _, gvr := range Kinds() {
		t.Run(gvr.Resource, func(t *testing.T) {
			r, _ := For(gvr)
			obj, _ := Zero(gvr)
			cols := r.Columns()
			if len(cols) == 0 {
				t.Fatal("no columns")
			}
			row, err := r.Row(obj, now)
			if err != nil {
				t.Fatalf("Row of an empty object: %v", err)
			}
			if len(row.Cells) != len(cols) {
				t.Errorf("%d cells for %d columns: %q", len(row.Cells), len(cols), row.Cells)
			}
			if len(row.Order) != len(cols) {
				t.Errorf("%d numbers for %d columns", len(row.Order), len(cols))
			}
			seen := map[string]bool{}
			for _, c := range cols {
				if seen[c.Name] {
					t.Errorf("column %q twice", c.Name)
				}
				seen[c.Name] = true
			}
		})
	}
}

// A renderer refuses an object of another kind with an error.
func TestEveryRendererRefusesAnotherKind(t *testing.T) {
	kinds := Kinds()
	if len(kinds) < 2 {
		t.Skip("fewer than two renderers")
	}
	now := time.Now()
	for i, gvr := range kinds {
		other, _ := Zero(kinds[(i+1)%len(kinds)])
		r, _ := For(gvr)
		if _, err := r.Row(other, now); err == nil {
			t.Errorf("%s rendered a %T", gvr.Resource, other)
		}
	}
}
