// Package testlog provides a slog.Handler that records every log record, so
// tests can assert on what the library logged, at which level, with which
// attributes.
package testlog

import (
	"context"
	"log/slog"
	"slices"
	"strings"
	"sync"
)

// Record captures a single log entry for test assertions.
//
// Attrs is rendered with %v rather than kept as `any`: every assertion this
// suite makes on an attribute is "is it there, and does it say what I expect",
// and comparing strings keeps those assertions readable. Attributes used to be
// discarded entirely, which made a whole class of log content — the local port
// on a drop, the frame counts behind a drop verdict — untestable, so an empty
// field was indistinguishable from a correct one.
type Record struct {
	Level   slog.Level
	Message string
	Attrs   map[string]string
}

// Attr returns the value of the named attribute, or "" when the record does not
// carry it. The two cases are distinguished by HasAttr.
func (r *Record) Attr(key string) string { return r.Attrs[key] }

// HasAttr reports whether the record carries the named attribute at all, so a
// test can tell "absent" from "present but empty" — the difference between a log
// site that was never reached and one that logged a zero value.
func (r *Record) HasAttr(key string) bool {
	_, ok := r.Attrs[key]
	return ok
}

// Handler is a minimal slog.Handler that captures log records for testing.
//
// WithAttrs returns a derived handler that records into the same store, so
// attributes attached by the library through logger.With() (which is how the
// session tags records) survive into the assertions instead of being dropped.
type Handler struct {
	records []Record
	mu      sync.Mutex

	// parent, with and groups are set only on derived handlers. The zero value is
	// a usable root handler, which is how every test constructs one.
	parent *Handler
	with   []attrBatch
	groups []string
}

// attrBatch is the attributes from one WithAttrs call together with the group
// prefix that was open when it was made.
//
// The prefix has to travel with the batch rather than being read from the
// handler at Handle time: slog's contract is that a group qualifies only the
// attributes added after it, so logger.With("request_id", x).WithGroup("net")
// must still record "request_id", not "net.request_id".
type attrBatch struct {
	prefix []string
	attrs  []slog.Attr
}

// root returns the handler that owns the record store.
func (h *Handler) root() *Handler {
	for h.parent != nil {
		h = h.parent
	}
	return h
}

func (h *Handler) Enabled(_ context.Context, _ slog.Level) bool { return true }

// collectAttr records one attribute under its group-qualified key, flattening
// slog.Group values recursively.
//
// Groups are qualified rather than dropped because a handler that silently
// flattens them can make a test pass on a key the real handler would have
// written as "group.key" — the kind of green that means nothing. Nothing in the
// library groups attributes today; this keeps the helper honest if that changes.
func collectAttr(dst map[string]string, prefix []string, a slog.Attr) {
	// Resolve before the kind check. A slog.LogValuer — the session's secret type is
	// one — may return a group, and an unresolved value would land in the map as a
	// single opaque scalar instead of its children.
	a.Value = a.Value.Resolve()
	if a.Value.Kind() == slog.KindGroup {
		inner := a.Value.Group()
		if len(inner) == 0 {
			return // slog: a group with no attributes is ignored
		}
		next := prefix
		if a.Key != "" { // an empty group key inlines its attributes
			next = append(append([]string{}, prefix...), a.Key)
		}
		for _, sub := range inner {
			collectAttr(dst, next, sub)
		}
		return
	}
	key := a.Key
	if len(prefix) > 0 {
		key = strings.Join(prefix, ".") + "." + key
	}
	dst[key] = a.Value.String()
}

func (h *Handler) Handle(_ context.Context, r slog.Record) error {
	attrs := make(map[string]string, r.NumAttrs()+len(h.with))
	for _, batch := range h.with {
		for _, a := range batch.attrs {
			collectAttr(attrs, batch.prefix, a)
		}
	}
	r.Attrs(func(a slog.Attr) bool {
		collectAttr(attrs, h.groups, a)
		return true
	})
	root := h.root()
	root.mu.Lock()
	root.records = append(root.records, Record{Level: r.Level, Message: r.Message, Attrs: attrs})
	root.mu.Unlock()
	return nil
}

func (h *Handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	// h.groups is never mutated in place — WithGroup builds a fresh slice — so the
	// batch can share it. attrs is the caller's slice and is copied.
	merged := make([]attrBatch, 0, len(h.with)+1)
	merged = append(merged, h.with...)
	merged = append(merged, attrBatch{prefix: h.groups, attrs: slices.Clone(attrs)})
	return &Handler{parent: h.root(), with: merged, groups: h.groups}
}

func (h *Handler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h // slog: an empty group name must return the receiver
	}
	return &Handler{
		parent: h.root(),
		with:   h.with,
		groups: append(append([]string{}, h.groups...), name),
	}
}

func (h *Handler) FindByMessage(msg string) *Record {
	root := h.root()
	root.mu.Lock()
	defer root.mu.Unlock()
	for _, r := range root.records {
		if strings.Contains(r.Message, msg) {
			return &r
		}
	}
	return nil
}

// All returns a copy of every record captured so far. Named for the question,
// not the field it reads, because the field is already called records.
func (h *Handler) All() []Record {
	root := h.root()
	root.mu.Lock()
	defer root.mu.Unlock()
	return slices.Clone(root.records)
}

// RecordsByLevel returns the captured records logged at exactly level, for
// assertions about whether something was an error or merely a warning.
func (h *Handler) RecordsByLevel(level slog.Level) []Record {
	var out []Record
	for _, r := range h.All() {
		if r.Level == level {
			out = append(out, r)
		}
	}
	return out
}

// CountByMessage reports how many records contain msg. Separate from
// FindByMessage because "did this happen at all" and "did this happen once
// rather than every tick" are different questions.
func (h *Handler) CountByMessage(msg string) int {
	root := h.root()
	root.mu.Lock()
	defer root.mu.Unlock()
	n := 0
	for _, r := range root.records {
		if strings.Contains(r.Message, msg) {
			n++
		}
	}
	return n
}
