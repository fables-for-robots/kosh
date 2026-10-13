package kube

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/duration"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"

	"github.com/fables-for-robots/kosh/render"
)

// tableLister reads the API server's table of a resource.
type tableLister interface {
	listTable(ctx context.Context, r Resource, namespace string) (*metav1.Table, error)
}

// tableAccept asks the API server for a list as a Table.
const tableAccept = "application/json;as=Table;v=v1;g=meta.k8s.io"

// restTables is the tableLister of a real API server.
type restTables struct {
	client rest.Interface
}

func newRESTTables(cfg *rest.Config) (*restTables, error) {
	cfg = rest.CopyConfig(cfg)
	cfg.APIPath = "/"
	cfg.GroupVersion = nil
	cfg.NegotiatedSerializer = scheme.Codecs.WithoutConversion()
	client, err := rest.UnversionedRESTClientFor(cfg)
	if err != nil {
		return nil, err
	}
	return &restTables{client: client}, nil
}

func (t *restTables) listTable(ctx context.Context, r Resource, namespace string) (*metav1.Table, error) {
	p, err := listPath(r, namespace)
	if err != nil {
		return nil, err
	}
	raw, err := t.client.Get().
		AbsPath(p).
		Param("includeObject", "Metadata").
		SetHeader("Accept", tableAccept).
		Do(ctx).Raw()
	if err != nil {
		return nil, err
	}
	var tab metav1.Table
	if err := json.Unmarshal(raw, &tab); err != nil {
		return nil, fmt.Errorf("the table of %s does not parse: %w", r.GVR.Resource, err)
	}
	if tab.Kind != "Table" {
		return nil, fmt.Errorf("the API server sent no table for %s, but %q", r.GVR.Resource, tab.Kind)
	}
	return &tab, nil
}

// listPath returns the path of the list of r, in namespace when r is
// namespaced and namespace is not empty. The client cleans the path it is
// given of "." and "..", so every string that goes into it is checked
// here, whatever was checked before: the GVR's strings as checkGVR says
// (an error that wraps ErrUnknownResource), the namespace as
// checkNamespace says (ErrInvalidNamespace). A name of these kinds is
// one path segment that needs no escaping.
func listPath(r Resource, namespace string) (string, error) {
	if err := checkGVR(r.GVR); err != nil {
		return "", err
	}
	if !r.Namespaced {
		namespace = ""
	}
	if err := checkNamespace(r, namespace); err != nil {
		return "", err
	}
	p := "/apis/" + r.GVR.Group + "/" + r.GVR.Version
	if r.GVR.Group == "" {
		p = "/api/" + r.GVR.Version
	}
	if namespace != "" {
		p += "/namespaces/" + namespace
	}
	return p + "/" + r.GVR.Resource, nil
}

// tableWatch is the table of a resource that is not typed: the API
// server's, read again every opts.TablePoll by a goroutine of its own.
type tableWatch struct {
	c         *Cluster
	res       Resource
	namespace string
	changed   chan struct{}
	cancel    context.CancelFunc
	done      chan struct{} // closed when the poll has ended
	closeOnce sync.Once

	mu        sync.Mutex
	stopAfter func() bool   // ends the Close that comes with the session's end
	last      *metav1.Table // of the last read that worked
	err       error         // of the last read, when none has worked
	stale     bool          // the last read failed after one that worked
}

func (s *session) watchTable(q Query) (Watch, error) {
	c := s.c
	if c.tables == nil {
		return nil, errors.New("kosh cannot read the API server's tables")
	}
	ctx, cancel := context.WithCancel(c.ctx)
	w := &tableWatch{
		c:         c,
		res:       q.Resource,
		namespace: q.Namespace,
		changed:   make(chan struct{}, 1),
		cancel:    cancel,
		done:      make(chan struct{}),
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		cancel()
		return nil, errClosed
	}
	c.wg.Add(1)
	c.mu.Unlock()
	go w.poll(ctx)

	w.mu.Lock()
	w.stopAfter = context.AfterFunc(s.ctx, w.Close)
	w.mu.Unlock()
	return w, nil
}

// poll reads the table now and every opts.TablePoll until ctx is done.
func (w *tableWatch) poll(ctx context.Context) {
	defer w.c.wg.Done()
	defer close(w.done)
	t := time.NewTicker(w.c.opts.TablePoll)
	defer t.Stop()
	for {
		w.read(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// read reads the table once and signals when what the watch shows
// changed.
func (w *tableWatch) read(ctx context.Context) {
	tab, err := w.c.tables.listTable(ctx, w.res, w.namespace)
	if ctx.Err() != nil {
		return
	}
	w.mu.Lock()
	var changed bool
	switch {
	case err != nil && w.last == nil:
		changed = w.err == nil || w.err.Error() != err.Error()
		w.err = err
	case err != nil:
		changed = !w.stale
		w.stale = true
	default:
		changed = w.last == nil || w.stale || !sameTable(w.last, tab)
		w.last, w.err, w.stale = tab, nil, false
	}
	w.mu.Unlock()
	if changed {
		select {
		case w.changed <- struct{}{}:
		default:
		}
	}
}

func sameTable(a, b *metav1.Table) bool {
	return reflect.DeepEqual(a.ColumnDefinitions, b.ColumnDefinitions) && reflect.DeepEqual(a.Rows, b.Rows)
}

func (w *tableWatch) Table(now time.Time) Table {
	w.mu.Lock()
	last, err, stale := w.last, w.err, w.stale
	w.mu.Unlock()
	switch {
	case last != nil:
		t := convertTable(last, w.res.Namespaced, now)
		t.Stale = stale
		return t
	case err != nil:
		return Table{Namespaced: w.res.Namespaced, Err: err}
	default:
		return Table{Namespaced: w.res.Namespaced, Stale: true}
	}
}

func (w *tableWatch) Changed() <-chan struct{} { return w.changed }

func (w *tableWatch) Close() {
	w.closeOnce.Do(func() {
		w.mu.Lock()
		stop := w.stopAfter
		w.mu.Unlock()
		if stop != nil {
			stop()
		}
		w.cancel()
		<-w.done
	})
}

// convertTable turns the API server's table into kosh's: names in
// capitals, a column of priority above 0 Wide, integers, numbers and
// dates Numeric, a date shown as its age, and for a namespaced resource
// a NAMESPACE column in front.
func convertTable(in *metav1.Table, namespaced bool, now time.Time) Table {
	t := Table{Namespaced: namespaced}
	if namespaced {
		t.Columns = append(t.Columns, render.Column{Name: "NAMESPACE"})
	}
	for _, d := range in.ColumnDefinitions {
		t.Columns = append(t.Columns, render.Column{
			Name:    strings.ToUpper(d.Name),
			Wide:    d.Priority > 0,
			Numeric: d.Type == "integer" || d.Type == "number" || d.Type == "date",
		})
	}
	for _, r := range in.Rows {
		var m metav1.PartialObjectMetadata
		if len(r.Object.Raw) > 0 {
			_ = json.Unmarshal(r.Object.Raw, &m)
		}
		row := render.Row{Namespace: m.Namespace, Name: m.Name, Labels: m.Labels}
		if namespaced {
			row.Cells = append(row.Cells, m.Namespace)
			row.Order = append(row.Order, 0)
		}
		for i, d := range in.ColumnDefinitions {
			var v any
			if i < len(r.Cells) {
				v = r.Cells[i]
			}
			text, order := cell(d.Type, v, now)
			row.Cells = append(row.Cells, text)
			row.Order = append(row.Order, order)
		}
		t.Rows = append(t.Rows, row)
	}
	return t
}

// cell returns the text of a cell of a column of type typ, and what it
// is sorted by.
func cell(typ string, v any, now time.Time) (string, float64) {
	switch typ {
	case "date":
		if s, ok := v.(string); ok {
			if ts, err := time.Parse(time.RFC3339, s); err == nil {
				d := now.Sub(ts)
				return duration.HumanDuration(d), d.Seconds()
			}
		}
	case "integer", "number":
		if f, ok := number(v); ok {
			return strconv.FormatFloat(f, 'f', -1, 64), f
		}
	}
	switch v := v.(type) {
	case nil:
		return "", 0
	case string:
		return v, 0
	default:
		return fmt.Sprint(v), 0
	}
}

func number(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int32:
		return float64(n), true
	case int64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	}
	return 0, false
}
