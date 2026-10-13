package render

import "testing"

// The finished program has a renderer for each of the 26 kinds, and
// for no other.
func TestEveryKindHasARenderer(t *testing.T) {
	got := map[string]bool{}
	for _, gvr := range Kinds() {
		got[gvr.String()] = true
	}
	for _, gvr := range All {
		if !got[gvr.String()] {
			t.Errorf("no renderer for %s", gvr)
		}
		delete(got, gvr.String())
	}
	for gvr := range got {
		t.Errorf("a renderer for %s, which is not in All", gvr)
	}
}
