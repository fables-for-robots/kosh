package kube

import (
	"context"
	"encoding/json"
	"errors"
	"math/rand/v2"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/duration"

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
	e.setTables(f)
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
		if got, err := listPath(tc.r, tc.namespace); err != nil || got != tc.want {
			t.Errorf("%v in %q: %s, %v; want %s", tc.r.GVR, tc.namespace, got, err, tc.want)
		}
	}
}

// agedRow is a row of the API server's table whose object is the partial
// metadata of namespace/name, created at created.
func agedRow(t *testing.T, namespace, name string, created time.Time, cells ...any) metav1.TableRow {
	t.Helper()
	raw, err := json.Marshal(&metav1.PartialObjectMetadata{
		TypeMeta:   metav1.TypeMeta{APIVersion: "meta.k8s.io/v1", Kind: "PartialObjectMetadata"},
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name, CreationTimestamp: metav1.NewTime(created)},
	})
	if err != nil {
		t.Fatal(err)
	}
	return metav1.TableRow{Cells: cells, Object: runtime.RawExtension{Raw: raw}}
}

// readTable returns the first table w shows with n rows.
func readTable(t *testing.T, w Watch, now time.Time, n int) Table {
	t.Helper()
	var tab Table
	eventually(t, "the first read", func() bool {
		tab = w.Table(now)
		return len(tab.Rows) == n
	})
	return tab
}

func TestGenericAgeSortsByCreation(t *testing.T) {
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	// The creation times humanise to the cells' texts, but are not what
	// the texts say exactly: the keys must come from the times.
	created := map[string]time.Time{
		"old":    now.Add(-(10*24 + 3) * time.Hour),
		"middle": now.Add(-(2*24*time.Hour + 40*time.Minute)),
		"young":  now.Add(-(5*time.Minute + 7*time.Second)),
	}
	texts := map[string]string{"old": "10d", "middle": "2d", "young": "5m"}
	for _, tc := range []struct{ name, typ string }{
		{"Age", "string"},
		{"Age", "date"},
		{"AGE", "string"},
		{"age", "date"},
	} {
		t.Run(tc.name+" "+tc.typ, func(t *testing.T) {
			e, f := genericEnv(t, Options{})
			f.set(&metav1.Table{
				ColumnDefinitions: []metav1.TableColumnDefinition{
					{Name: "Name", Type: "string"},
					{Name: tc.name, Type: tc.typ},
				},
				Rows: []metav1.TableRow{
					agedRow(t, "ns", "middle", created["middle"], "middle", texts["middle"]),
					agedRow(t, "ns", "old", created["old"], "old", texts["old"]),
					agedRow(t, "ns", "young", created["young"], "young", texts["young"]),
					// A server that ignores includeObject: the key comes from the text.
					tableRow(t, "", "", nil, "bare", "3h"),
				},
			}, nil)
			w := watchOf(t, e.session(t), Query{Resource: e.resource(t, "example.com", "widgets")})
			tab := readTable(t, w, now, 4)
			if want := (render.Column{Name: "AGE", Numeric: true}); tab.Columns[2] != want {
				t.Errorf("the Age column %+v, want %+v", tab.Columns[2], want)
			}
			byName := map[string]render.Row{}
			for _, r := range tab.Rows {
				byName[r.Cells[1]] = r
			}
			for name, ts := range created {
				r := byName[name]
				if r.Cells[2] != texts[name] {
					t.Errorf("%s: text %q, want %q as the server sent it", name, r.Cells[2], texts[name])
				}
				if want := now.Sub(ts).Seconds(); r.Order[2] != want {
					t.Errorf("%s: key %v, want %v, the seconds since its creation", name, r.Order[2], want)
				}
			}
			// As the typed renderers' AGE: the youngest has the smallest key.
			rows := append([]render.Row(nil), tab.Rows...)
			sort.SliceStable(rows, func(i, j int) bool { return rows[i].Order[2] < rows[j].Order[2] })
			var order []string
			for _, r := range rows {
				order = append(order, r.Cells[1])
			}
			if want := "young,bare,middle,old"; strings.Join(order, ",") != want {
				t.Errorf("sorted by the keys: %v, want %s", order, want)
			}
			if bare := byName["bare"]; bare.Cells[2] != "3h" || bare.Order[2] != 3*3600 {
				t.Errorf("a row without metadata: cell %q, key %v, want 3h and %v", bare.Cells[2], bare.Order[2], 3*3600)
			}
		})
	}
}

func TestGenericDurationCells(t *testing.T) {
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	e, f := genericEnv(t, Options{})
	long := strings.Repeat("9", 100000) + "s"
	f.set(&metav1.Table{
		ColumnDefinitions: []metav1.TableColumnDefinition{
			{Name: "Name", Type: "string"},
			{Name: "Last Seen", Type: "string"},
			{Name: "Size", Type: "string"},
			{Name: "Since", Type: "date"},
			{Name: "Odd", Type: "string"},
		},
		Rows: []metav1.TableRow{
			tableRow(t, "ns", "a", nil, "a", "2m30s", "3d", "120m", true),
			tableRow(t, "ns", "b", nil, "b", "<none>", "big", now.Add(-time.Hour).Format(time.RFC3339), []any{1.0, "x"}),
			tableRow(t, "ns", "c", nil, "c", "4d", "5m", nil, map[string]any{"k": nil}),
			tableRow(t, "ns", "d", nil, "d", "2y120d", "", "soon", long),
			// Fewer cells than columns.
			tableRow(t, "ns", "e", nil, "e"),
		},
	}, nil)
	w := watchOf(t, e.session(t), Query{Resource: e.resource(t, "example.com", "widgets")})
	tab := readTable(t, w, now, 5)
	numeric := map[string]bool{}
	for _, c := range tab.Columns {
		numeric[c.Name] = c.Numeric
	}
	// LAST SEEN holds durations and nothing else; SIZE holds text too.
	if want := map[string]bool{"NAMESPACE": false, "NAME": false, "LAST SEEN": true, "SIZE": false, "SINCE": true, "ODD": false}; !reflect.DeepEqual(numeric, want) {
		t.Errorf("numeric columns %v, want %v", numeric, want)
	}
	byName := map[string]render.Row{}
	for _, r := range tab.Rows {
		byName[r.Cells[1]] = r
	}
	day := 24 * time.Hour
	for _, tc := range []struct {
		row, text string
		col       int
		key       time.Duration
	}{
		{"a", "2m30s", 2, 2*time.Minute + 30*time.Second},
		{"b", "<none>", 2, 0},
		{"c", "4d", 2, 4 * day},
		{"d", "2y120d", 2, (2*365 + 120) * day},
		{"e", "", 2, 0},
		{"a", "3d", 3, 3 * day},
		{"b", "big", 3, 0},
		{"c", "5m", 3, 5 * time.Minute},
		{"a", "120m", 4, 120 * time.Minute},
		{"b", "60m", 4, time.Hour},
		{"c", "", 4, 0},
		{"d", "soon", 4, 0},
		{"a", "true", 5, 0},
		{"d", long, 5, 0},
	} {
		r := byName[tc.row]
		if r.Cells[tc.col] != tc.text || r.Order[tc.col] != tc.key.Seconds() {
			t.Errorf("%s, column %s: %.20q with key %v, want %.20q with key %v",
				tc.row, tab.Columns[tc.col].Name, r.Cells[tc.col], r.Order[tc.col], tc.text, tc.key.Seconds())
		}
	}
}

func TestHumanDuration(t *testing.T) {
	day := 24 * time.Hour
	year := 365 * day
	for _, tc := range []struct {
		s    string
		want time.Duration
	}{
		{"0s", 0},
		{"5s", 5 * time.Second},
		{"119s", 119 * time.Second},
		{"5m", 5 * time.Minute},
		{"2m30s", 2*time.Minute + 30*time.Second},
		{"120m", 120 * time.Minute},
		{"3h", 3 * time.Hour},
		{"3h20m", 3*time.Hour + 20*time.Minute},
		{"47h", 47 * time.Hour},
		{"4d", 4 * day},
		{"3d2h", 3*day + 2*time.Hour},
		{"400d", 400 * day},
		{"2y", 2 * year},
		{"2y120d", 2*year + 120*day},
		{"12y", 12 * year},
		{"292y", 292 * year},
		{"9223372036s", 9223372036 * time.Second},
	} {
		if got, ok := parseHumanDuration(tc.s); !ok || got != tc.want {
			t.Errorf("%q: %v, %v, want %v", tc.s, got, ok, tc.want)
		}
	}
	for _, s := range []string{
		"m5", "5", "5 m", "5mm", "", "<none>", "n/a", "1e3s", "-5m", "+5m",
		" 5m", "5m ", "5M", "s", "5x", "2s3m", "3h2s", "5m5m", "1d2h3m", "2m60s", "1.5h",
		"0x10s", "\x005s", "99999999999999999999s", "300y", "9223372037s", "00000000005s",
	} {
		if got, ok := parseHumanDuration(s); ok {
			t.Errorf("%q is a duration: %v", s, got)
		}
	}
	// What the humaniser prints parses to a value it prints the same.
	rnd := rand.New(rand.NewPCG(1, 2))
	for i := 0; i < 200; i++ {
		// Spread over the magnitudes: seconds to centuries.
		d := time.Duration(rnd.Int64N(int64(1) << (rnd.IntN(32) + 30)))
		for _, human := range []func(time.Duration) string{duration.HumanDuration, duration.ShortHumanDuration} {
			text := human(d)
			got, ok := parseHumanDuration(text)
			if !ok {
				t.Errorf("%v printed as %q does not parse", d, text)
				continue
			}
			if again := human(got); again != text {
				t.Errorf("%v printed as %q parses to %v, printed as %q", d, text, got, again)
			}
		}
	}
}
