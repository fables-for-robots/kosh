package kube

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/fables-for-robots/kosh/render"
)

var widgetsGVR = schema.GroupVersionResource{Group: "example.com", Version: "v1", Resource: "widgets"}

// fakeTables is a tableLister that returns what a test sets.
type fakeTables struct {
	mu         sync.Mutex
	table      *metav1.Table
	err        error
	calls      int
	namespaces []string
}

func (f *fakeTables) listTable(_ context.Context, _ Resource, namespace string) (*metav1.Table, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.namespaces = append(f.namespaces, namespace)
	if f.err != nil {
		return nil, f.err
	}
	return f.table.DeepCopy(), nil
}

func (f *fakeTables) set(t *metav1.Table, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.table, f.err = t, err
}

func (f *fakeTables) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// tableRow is a row of the API server's table whose object is the
// partial metadata of namespace/name, or none when name is empty.
func tableRow(t *testing.T, namespace, name string, labels map[string]string, cells ...any) metav1.TableRow {
	t.Helper()
	row := metav1.TableRow{Cells: cells}
	if name != "" {
		raw, err := json.Marshal(&metav1.PartialObjectMetadata{
			TypeMeta:   metav1.TypeMeta{APIVersion: "meta.k8s.io/v1", Kind: "PartialObjectMetadata"},
			ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name, Labels: labels},
		})
		if err != nil {
			t.Fatal(err)
		}
		row.Object = runtime.RawExtension{Raw: raw}
	}
	return row
}

func widgetColumns() []metav1.TableColumnDefinition {
	return []metav1.TableColumnDefinition{
		{Name: "Name", Type: "string"},
		{Name: "Count", Type: "integer"},
		{Name: "Weight", Type: "number"},
		{Name: "Age", Type: "date"},
		{Name: "Extra", Type: "string", Priority: 1},
	}
}

// genericEnv is a Cluster whose tables come from a fakeTables.
func genericEnv(t *testing.T, opts Options) (*env, *fakeTables) {
	t.Helper()
	e := newEnv(t, envConfig{opts: opts})
	f := &fakeTables{table: &metav1.Table{ColumnDefinitions: widgetColumns()}}
	e.c.tables = f
	return e, f
}

func TestGenericTable(t *testing.T) {
	e, f := genericEnv(t, Options{})
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	created := now.Add(-5 * time.Minute).Format(time.RFC3339)
	f.set(&metav1.Table{
		ColumnDefinitions: widgetColumns(),
		Rows: []metav1.TableRow{
			tableRow(t, "ns", "small", map[string]string{"size": "s"}, "small", int64(3), 1.5, created, "x"),
			// JSON gives numbers as float64, and a date may be anything.
			tableRow(t, "other", "big", nil, "big", float64(12), float64(2), "<unknown>", nil),
			tableRow(t, "", "", nil, "orphan", int64(0), 0.0, created, "y"),
		},
	}, nil)
	widgets := e.resource(t, "example.com", "widgets")
	w := watchOf(t, e.session(t), Query{Resource: widgets, Namespace: "ns"})

	var tab Table
	eventually(t, "the first read", func() bool {
		tab = w.Table(now)
		return len(tab.Rows) == 3
	})
	wantColumns := []render.Column{
		{Name: "NAMESPACE"},
		{Name: "NAME"},
		{Name: "COUNT", Numeric: true},
		{Name: "WEIGHT", Numeric: true},
		{Name: "AGE", Numeric: true},
		{Name: "EXTRA", Wide: true},
	}
	if !reflect.DeepEqual(tab.Columns, wantColumns) {
		t.Errorf("columns\n got %+v\nwant %+v", tab.Columns, wantColumns)
	}
	if !tab.Namespaced || tab.Stale || tab.Err != nil {
		t.Errorf("namespaced %v, stale %v, err %v", tab.Namespaced, tab.Stale, tab.Err)
	}
	byName := map[string]render.Row{}
	for _, r := range tab.Rows {
		byName[r.Cells[1]] = r
	}
	small := byName["small"]
	if small.Name != "small" || small.Namespace != "ns" || small.Labels["size"] != "s" {
		t.Errorf("small: name %q, namespace %q, labels %v", small.Name, small.Namespace, small.Labels)
	}
	if want := []string{"ns", "small", "3", "1.5", "5m", "x"}; !reflect.DeepEqual(small.Cells, want) {
		t.Errorf("small: cells %q, want %q", small.Cells, want)
	}
	if want := []float64{0, 0, 3, 1.5, 300, 0}; !reflect.DeepEqual(small.Order, want) {
		t.Errorf("small: order %v, want %v", small.Order, want)
	}
	big := byName["big"]
	if want := []string{"other", "big", "12", "2", "<unknown>", ""}; !reflect.DeepEqual(big.Cells, want) {
		t.Errorf("big: cells %q, want %q", big.Cells, want)
	}
	if big.Order[2] != 12 {
		t.Errorf("big: order %v", big.Order)
	}
	orphan, ok := byName["orphan"]
	if !ok || orphan.Name != "" {
		t.Errorf("a row without an object: %+v", orphan)
	}
	f.mu.Lock()
	if f.namespaces[0] != "ns" {
		t.Errorf("the table was read for %q, want ns", f.namespaces[0])
	}
	f.mu.Unlock()
}

func TestGenericTableBeforeTheFirstRead(t *testing.T) {
	e, f := genericEnv(t, Options{})
	f.mu.Lock() // the first read waits
	w := watchOf(t, e.session(t), Query{Resource: e.resource(t, "example.com", "widgets")})
	tab := w.Table(time.Now())
	f.mu.Unlock()
	if !tab.Stale || len(tab.Rows) != 0 || tab.Err != nil {
		t.Errorf("before the first read: stale %v, %d rows, err %v", tab.Stale, len(tab.Rows), tab.Err)
	}
	waitChanged(t, w)
	if tab := w.Table(time.Now()); tab.Stale || len(tab.Columns) == 0 {
		t.Errorf("after the first read: stale %v, columns %v", tab.Stale, tab.Columns)
	}
}

func TestGenericTablePolls(t *testing.T) {
	e, f := genericEnv(t, Options{TablePoll: 10 * time.Millisecond})
	f.set(&metav1.Table{ColumnDefinitions: widgetColumns(), Rows: []metav1.TableRow{
		tableRow(t, "ns", "a", nil, "a", int64(1), 1.0, "", ""),
	}}, nil)
	w := watchOf(t, e.session(t), Query{Resource: e.resource(t, "example.com", "widgets")})
	waitChanged(t, w)
	rowsWhen(t, w, "ns/a")

	// Reads that find what was there: no signal.
	n := f.callCount()
	eventually(t, "more reads", func() bool { return f.callCount() > n+3 })
	select {
	case <-w.Changed():
		t.Fatal("a signal although nothing changed")
	default:
	}

	f.set(&metav1.Table{ColumnDefinitions: widgetColumns(), Rows: []metav1.TableRow{
		tableRow(t, "ns", "a", nil, "a", int64(1), 1.0, "", ""),
		tableRow(t, "ns", "b", nil, "b", int64(2), 1.0, "", ""),
	}}, nil)
	waitChanged(t, w)
	rowsWhen(t, w, "ns/a", "ns/b")
}

func TestGenericTableErrors(t *testing.T) {
	e, f := genericEnv(t, Options{TablePoll: 10 * time.Millisecond})
	refused := errors.New("refused")
	f.set(nil, refused)
	w := watchOf(t, e.session(t), Query{Resource: e.resource(t, "example.com", "widgets")})
	waitChanged(t, w)
	tab := w.Table(time.Now())
	if !errors.Is(tab.Err, refused) || len(tab.Rows) != 0 || len(tab.Columns) != 0 {
		t.Fatalf("a first read that fails: err %v, %d rows, %d columns", tab.Err, len(tab.Rows), len(tab.Columns))
	}

	f.set(&metav1.Table{ColumnDefinitions: widgetColumns(), Rows: []metav1.TableRow{
		tableRow(t, "ns", "a", nil, "a", int64(1), 1.0, "", ""),
	}}, nil)
	tab = rowsWhen(t, w, "ns/a")
	if tab.Err != nil || tab.Stale {
		t.Errorf("after a good read: err %v, stale %v", tab.Err, tab.Stale)
	}

	drain(w.Changed())
	f.set(nil, refused)
	waitChanged(t, w)
	tab = w.Table(time.Now())
	if tab.Err != nil || !tab.Stale || len(tab.Rows) != 1 {
		t.Errorf("a later read that fails: err %v, stale %v, %d rows", tab.Err, tab.Stale, len(tab.Rows))
	}
}

func TestGenericTableStopsWithClose(t *testing.T) {
	for _, how := range []string{"Close", "the session's end", "the Cluster's Close"} {
		t.Run(how, func(t *testing.T) {
			e, f := genericEnv(t, Options{TablePoll: time.Millisecond})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			w, err := e.c.Session(ctx, fullGrant).Watch(Query{Resource: e.resource(t, "example.com", "widgets")})
			if err != nil {
				t.Fatal(err)
			}
			eventually(t, "reads", func() bool { return f.callCount() > 2 })
			switch how {
			case "Close":
				w.Close()
				w.Close()
			case "the session's end":
				cancel()
				eventually(t, "the poll ends", func() bool {
					n := f.callCount()
					time.Sleep(20 * time.Millisecond)
					return f.callCount() == n
				})
			case "the Cluster's Close":
				e.c.Close()
			}
			n := f.callCount()
			time.Sleep(20 * time.Millisecond)
			if got := f.callCount(); got != n {
				t.Errorf("%d reads after %s", got-n, how)
			}
			w.Close()
		})
	}
}

func TestListPath(t *testing.T) {
	pods := Resource{GVR: render.Pods, Namespaced: true}
	widgets := Resource{GVR: widgetsGVR, Namespaced: true}
	nodes := Resource{GVR: render.Nodes}
	for _, tc := range []struct {
		r         Resource
		namespace string
		want      string
	}{
		{pods, "", "/api/v1/pods"},
		{pods, "ns", "/api/v1/namespaces/ns/pods"},
		{widgets, "", "/apis/example.com/v1/widgets"},
		{widgets, "ns", "/apis/example.com/v1/namespaces/ns/widgets"},
		{nodes, "ns", "/api/v1/nodes"},
	} {
		if got := listPath(tc.r, tc.namespace); got != tc.want {
			t.Errorf("%v in %q: %s, want %s", tc.r.GVR, tc.namespace, got, tc.want)
		}
	}
}
