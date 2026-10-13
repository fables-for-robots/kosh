package kube

import (
	"context"
	"log/slog"
	"sync"
)

// auditRecord is one record of an auditLog, its attributes resolved.
type auditRecord struct {
	Level   slog.Level
	Message string
	Attrs   map[string]slog.Value
}

// attr returns the attribute key as a string, and whether it is there.
func (r auditRecord) attr(key string) (string, bool) {
	v, ok := r.Attrs[key]
	if !ok {
		return "", false
	}
	return v.String(), true
}

// auditLog is a slog.Handler that keeps every record.
type auditLog struct {
	mu      *sync.Mutex
	records *[]auditRecord
	attrs   []slog.Attr
}

func newAuditLog() *auditLog {
	return &auditLog{mu: &sync.Mutex{}, records: &[]auditRecord{}}
}

// logger returns a logger that writes to h.
func (h *auditLog) logger() *slog.Logger { return slog.New(h) }

func (h *auditLog) Enabled(context.Context, slog.Level) bool { return true }

func (h *auditLog) Handle(_ context.Context, r slog.Record) error {
	rec := auditRecord{Level: r.Level, Message: r.Message, Attrs: map[string]slog.Value{}}
	for _, a := range h.attrs {
		rec.Attrs[a.Key] = a.Value.Resolve()
	}
	r.Attrs(func(a slog.Attr) bool {
		rec.Attrs[a.Key] = a.Value.Resolve()
		return true
	})
	h.mu.Lock()
	defer h.mu.Unlock()
	*h.records = append(*h.records, rec)
	return nil
}

func (h *auditLog) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &auditLog{mu: h.mu, records: h.records, attrs: append(append([]slog.Attr(nil), h.attrs...), attrs...)}
}

func (h *auditLog) WithGroup(string) slog.Handler { return h }

// all returns every record so far.
func (h *auditLog) all() []auditRecord {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]auditRecord(nil), *h.records...)
}

// with returns the records whose message is msg.
func (h *auditLog) with(msg string) []auditRecord {
	var out []auditRecord
	for _, r := range h.all() {
		if r.Message == msg {
			out = append(out, r)
		}
	}
	return out
}

// reset forgets every record so far.
func (h *auditLog) reset() {
	h.mu.Lock()
	defer h.mu.Unlock()
	*h.records = nil
}
