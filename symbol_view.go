package ads

import (
	"log/slog"
	"slices"

	"github.com/siyka-au/go-ads/v3/internal/symtab"

	"github.com/siyka-au/go-ads/v3/ams"
)

// connLogger returns a session's logger, or the package default for a nil or
// detached session. Free functions that receive a *Session use this so their
// records reach the caller's handler like everything else.
func connLogger(conn *Session) *slog.Logger {
	if conn == nil || conn.logger == nil {
		return slog.Default()
	}
	return conn.logger
}

// log returns the logger records about this view belong on.
func (v SymbolView) log() *slog.Logger { return connLogger(v.conn) }

// SymbolView is a read-only snapshot of a symbol's metadata and cached value,
// captured under cache.lock at creation. It does not track later updates -- call
// Symbol again or subscribe for fresh data.
//
// The snapshot model buys lock-free field access and internal consistency (Parsed
// and Value captured together), at the cost of going stale after a concurrent
// reload. IsValid() is false for the zero value. Children() and ChildrenWalk()
// release the lock before calling the iterator, so any Session method is safe
// inside a walk.
type SymbolView struct {
	Name        string
	FullName    string
	DataType    string
	Comment     string
	Handle      uint32
	Group       uint32
	Offset      uint32
	Length      uint32
	BaseType    ams.DataType // protocol ADST_ code; see DataType.IECName
	Flags       ams.SymbolFlag
	ContextMask uint8 // PLC task context (bits 8-11 of Flags); 0 = no task binding
	Parsed      bool  // true if Value has been decoded at least once at snapshot time
	IsRoot      bool  // true if this symbol has no parent (top-level program/global var)
	BitMember   bool  // a BIT member of a struct: Offset and Length count bits
	Value       any   // the cached value as its Go type (see internal/symtab/value.go); a copy

	// RangeMin/RangeMax are the declared bounds of an IEC 61131-3 subrange type
	// (e.g. INT(-10..10)). Both nil for a symbol with no subrange restriction;
	// 0 is a legitimate bound, so presence is signaled by non-nil.
	RangeMin, RangeMax *int64

	conn *Session
}

// IsValid reports whether the view is backed by a live connection. Zero-value
// SymbolView returns false; views obtained from Symbol/Symbols return
// true (until the connection is closed).
func (v SymbolView) IsValid() bool { return v.conn != nil && v.FullName != "" }

// BaseTypeName returns the IEC 61131-3 primitive underlying this symbol, for
// consumers that need the storage layout without tracking user-defined names.
// Resolved from the protocol ADST_ code, else the datatype table, else inferred
// from size -- inference is limited to 1- and 2-byte widths, since 4 and 8 are
// REAL/LREAL ambiguous.
//
// Returns "" when none resolve -- no symbol has "" legitimately, so callers branch
// on it -- and warns once per symbol naming the remedy. Never fetches the table
// itself: a getter with no ctx and no error has no business doing I/O.
func (v SymbolView) BaseTypeName() string {
	// parse() switches on the declared name first, so it wins: TC3 stamps DT
	// with the ADST_ code for its storage type and still parses a timestamp.
	if slices.Contains(symtab.ParseableTypes, v.DataType) {
		return v.DataType
	}
	if name := v.BaseType.IECName(); name != "" {
		return name
	}
	if v.conn != nil {
		v.conn.cache.lock.Lock()
		dts := v.conn.cache.datatypes
		v.conn.cache.lock.Unlock()
		if dts != nil {
			if dt, ok := dts[v.DataType]; ok {
				if name := dt.DataType; name != "" {
					return name
				}
			}
		}
	}
	if inferred := symtab.InferBaseType(v.Length, v.BaseType); inferred != "" {
		return inferred
	}
	// Unresolvable, and until now silently so: a 4- or 8-byte user-defined type
	// (typically a DINT-backed enum) with no datatype table loaded returned "" with
	// nothing logged and nothing in the return to distinguish it from a symbol that
	// genuinely has no base type. Downstream that shipped as a quoted string and was
	// never repaired.
	//
	// Deliberately does NOT fetch the datatype table: a caller running with symbol
	// loading off has declined that upload, and this getter has no context to do I/O
	// with anyway. Say what is missing and what fixes it; the caller decides.
	v.warnUnresolvedBaseType()
	return ""
}

// warnUnresolvedBaseType reports an unresolvable base type once per symbol.
func (v SymbolView) warnUnresolvedBaseType() {
	if v.conn == nil {
		return
	}
	v.conn.warnUnresolvedBaseType(v.FullName)
}

// warnUnresolvedBaseType reports once per symbol that the base type needs the
// datatype table. Called from BaseTypeName and from the subscribe paths, where it
// is actionable before samples arrive; one latch serves both. The latch lives on
// the cached *symbol, since a SymbolView is a per-call copy and would latch
// nothing. Logged after the lock is released -- the handler is user-supplied.
func (sess *Session) warnUnresolvedBaseType(symbolName string) {
	sess.cache.lock.Lock()
	sym, ok := sess.cache.symbols[symtab.Key(symbolName)]
	if !ok || sym.BaseTypeWarned {
		sess.cache.lock.Unlock()
		return
	}
	// Re-derive resolvability under the lock: the datatype table may have been
	// loaded since whatever prompted this call.
	dataType, baseType, length := sym.DataType, sym.BaseType, sym.Length
	_, inTable := sess.cache.datatypes[dataType]
	sess.cache.lock.Unlock()

	if baseType.IECName() != "" || inTable || symtab.InferBaseType(length, baseType) != "" ||
		slices.Contains(symtab.ParseableTypes, dataType) {
		return
	}

	sess.cache.lock.Lock()
	if sym.BaseTypeWarned {
		sess.cache.lock.Unlock()
		return
	}
	sym.BaseTypeWarned = true
	sess.cache.lock.Unlock()

	sess.logger.Warn("cannot resolve the base type of a user-defined type; no datatype table is loaded",
		"symbol", symbolName,
		"dataType", dataType,
		"size", length,
		"detail", "4- and 8-byte widths are ambiguous (DINT/REAL, LINT/LREAL share them), so they cannot be inferred from size",
		"hint", "call LoadSymbols (or LoadDataTypes) to load the datatype table; without it this symbol's value is delivered as an unconverted string")
}

// Children returns SymbolViews for struct/array members captured at view
// creation time. Returns nil for scalars and for symbols whose subtree has
// not been populated by full discovery. The map is freshly allocated per
// call; callers can mutate it without affecting library state.
//
// Each child is a freshly-snapshotted SymbolView (lookups walk the cache
// under cache.lock once per call, then release). Caller code in a walk loop
// is free to call any Session method - no lock held.
func (v SymbolView) Children() map[string]SymbolView {
	if v.conn == nil {
		return nil
	}
	v.conn.cache.lock.Lock()
	s := v.conn.cache.symbols[symtab.Key(v.FullName)]
	if s == nil || len(s.Children) == 0 {
		v.conn.cache.lock.Unlock()
		return nil
	}
	out := make(map[string]SymbolView, len(s.Children))
	for k, c := range s.Children {
		if c == nil {
			continue
		}
		out[k] = viewOf(c, v.conn)
	}
	v.conn.cache.lock.Unlock()
	return out
}

// ChildrenWalk visits every symbol in the subtree rooted at this view in
// depth-first order. Snapshots the entire subtree under cache.lock once,
// releases the lock, then invokes fn for each entry. fn is free to call
// any Session method (Value field reads are lock-free, Symbol etc.
// take their own locks - no deadlock risk).
//
// Walk terminates early if fn returns false.
func (v SymbolView) ChildrenWalk(fn func(SymbolView) bool) {
	if v.conn == nil || fn == nil {
		return
	}
	v.conn.cache.lock.Lock()
	root := v.conn.cache.symbols[symtab.Key(v.FullName)]
	if root == nil {
		v.conn.cache.lock.Unlock()
		return
	}
	// Snapshot the subtree under cache.lock so the slice is internally
	// consistent, then release the lock BEFORE invoking the caller's fn —
	// fn may call other Session methods that take their own locks, and the
	// lock-ordering rule forbids holding cache.lock across user callbacks.
	var snapshots []SymbolView
	collectSubtree(root, v.conn, &snapshots)
	v.conn.cache.lock.Unlock()
	for _, view := range snapshots {
		if !fn(view) {
			return
		}
	}
}

// collectSubtreeMaxDepth caps recursion depth as defense against malformed
// data forming a Children cycle. Real PLC struct nesting is at most a few
// dozen levels.
const collectSubtreeMaxDepth = 256

func collectSubtree(s *symtab.Symbol, conn *Session, out *[]SymbolView) {
	collectSubtreeDepth(s, conn, out, 0)
}

func collectSubtreeDepth(s *symtab.Symbol, conn *Session, out *[]SymbolView, depth int) {
	if depth >= collectSubtreeMaxDepth {
		connLogger(conn).Warn("collectSubtree hit depth cap; possible Children cycle or malformed symbol tree",
			"symbol", s.FullName,
			"max_depth", collectSubtreeMaxDepth)
		return
	}
	for _, c := range s.Children {
		if c == nil {
			continue
		}
		*out = append(*out, viewOf(c, conn))
		collectSubtreeDepth(c, conn, out, depth+1)
	}
}

// viewOf builds a SymbolView for s. Caller must hold cache.lock so the
// snapshot of metadata + value is internally consistent. O(1).
func viewOf(s *symtab.Symbol, conn *Session) SymbolView {
	return SymbolView{
		Name:        s.Name,
		FullName:    s.FullName,
		DataType:    s.DataType,
		Comment:     s.Comment,
		Handle:      s.Handle,
		Group:       s.Group,
		Offset:      s.Offset,
		Length:      s.Length,
		BaseType:    s.BaseType,
		Flags:       s.Flags,
		ContextMask: s.ContextMask,
		Parsed:      s.Valid,
		IsRoot:      s.Parent == nil,
		BitMember:   s.BitMember,
		Value:       symtab.CopyValue(s.Value),
		RangeMin:    s.RangeMin,
		RangeMax:    s.RangeMax,
		conn:        conn,
	}
}

// cachedView returns the view of a symbol already in the cache, with no I/O.
func (sess *Session) cachedView(name string) (SymbolView, bool) {
	sess.cache.lock.Lock()
	defer sess.cache.lock.Unlock()
	s := sess.cache.symbols[symtab.Key(name)]
	if s == nil {
		return SymbolView{}, false
	}
	return viewOf(s, sess), true
}
