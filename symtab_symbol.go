package ads

import (
	"log/slog"
	"strings"
	"time"
)

// symbolKey normalizes a symbol name for use as an internal map key.
// TwinCAT treats symbol names case-insensitively (IEC 61131-3).
// TC2 returns uppercase, TC3 preserves original casing — lowercasing
// ensures consistent lookups regardless of caller or PLC casing.
func symbolKey(name string) string { return strings.ToLower(name) }

// normalizeStringDataType strips trailing array/length suffixes from STRING
// and WSTRING type names. The PLC reports types like "STRING(80)" or
// "WSTRING(255)"; the library treats all string variants of the same kind
// uniformly, so we collapse to the bare "STRING" or "WSTRING" prefix.
//
// Returns the input unchanged if it is neither a STRING nor a WSTRING.
func normalizeStringDataType(dt string) string {
	switch {
	case len(dt) >= 7 && dt[:7] == "WSTRING":
		return "WSTRING"
	case len(dt) >= 6 && dt[:6] == "STRING":
		return "STRING"
	default:
		return dt
	}
}

// symbol is the internal cache record; external callers use SymbolView.
//
// Field guards: the metadata (FullName, DataType, Group, Offset, Length,
// BaseType, Flags, Parent, Children) is immutable after construction and needs no
// lock. Value, Valid, ValueParsed and LastUpdateTime are guarded by cache.lock, as
// is Handle -- zeroed on reload, and an observed zero simply fails the next PLC
// call and prompts a re-resolve. Parent/Children form a tree fixed at discovery.
type symbol struct {
	FullName       string
	LastUpdateTime time.Time
	Name           string
	DataType       string
	Comment        string
	Handle         uint32
	Group          uint32
	Offset         uint32
	Length         uint32
	BaseType       ADSDataType // protocol ADST_ code (e.g., ADSTReal32=4 for REAL)
	Flags          SymbolFlag
	ContextMask    uint8 // PLC task context (bits 8-11 of Flags); 0 = no task binding

	Value       any // decoded to its Go type; see value.go
	Valid       bool
	ValueParsed bool // true after first successful parse

	// BitMember marks a BIT member of a struct: Offset and Length count bits
	// from the start of the parent, not bytes.
	BitMember bool

	Parent   *symbol
	Children map[string]*symbol

	// The session's logger, stamped on as the symbol enters the cache; nil falls
	// back to the package default. Carried here rather than threaded through
	// parse/writeToNode, whose ~70 call sites would otherwise bypass WithLogger --
	// unmutable, unroutable, and stripped of the host's structured fields.
	logger *slog.Logger

	// inferenceWarned latches the "inferring base type from size" warning, which
	// reports a STATIC property of the symbol — its width and the absence of a
	// datatype table — and so says nothing new on the second poll. It fired once
	// per symbol per poll: measured on TC2 with loadSymbols false, 44 of the 61
	// lines in a 25 s run. Latched per symbol, so the first one still tells the
	// operator to call LoadSymbols.
	inferenceWarned bool

	// baseTypeWarned latches the "base type unresolvable" warning from
	// SymbolView.BaseTypeName. Same reasoning as inferenceWarned: the condition is
	// a static property of the symbol, so repeating it per call says nothing — and
	// a consumer may call BaseTypeName once per sample.
	baseTypeWarned bool
}

// warnInferenceOnce logs the base-type inference warning the first time it
// applies to this symbol and stays quiet afterwards.
//
// Not atomic: symbols are parsed under cache.lock (see the F-39 note in
// improvements.md), so this field has the same protection every other field on
// symbol has. If that ever changes, this becomes a race — and a benign one, since
// the worst outcome is a duplicate warning.
func (s *symbol) warnInferenceOnce(msg string, args ...any) {
	if s.inferenceWarned {
		return
	}
	s.inferenceWarned = true
	s.log().Warn(msg, args...)
}

// log returns the logger records about this symbol belong on: the session's if
// the symbol came from one, the package default otherwise.
func (s *symbol) log() *slog.Logger {
	if s == nil || s.logger == nil {
		return getDefaultLogger()
	}
	return s.logger
}

// stampLogger attaches lg to a symbol and everything below it, so records
// produced while parsing or serialising a symbol reach the caller's logger.
//
// Called at the points where a session takes ownership of symbols, which is the
// only place that knows both the tree and the logger. Depth-bounded for the same
// reason collectSubtreeDepth is: a malformed PLC response can present a cycle,
// and a stack overflow is a worse outcome than an unstamped subtree.
func stampLogger(s *symbol, lg *slog.Logger, depth int) {
	if s == nil || lg == nil || depth >= collectSubtreeMaxDepth {
		return
	}
	s.logger = lg
	for _, child := range s.Children {
		stampLogger(child, lg, depth+1)
	}
}

// stampLoggerOnAll stamps a whole symbol map, for the bulk cache swaps.
func stampLoggerOnAll(symbols map[string]*symbol, lg *slog.Logger) {
	for _, s := range symbols {
		stampLogger(s, lg, 0)
	}
}

// invalidate drops the cached value so the next read goes to the PLC. Caller
// holds cache.lock.
func (s *symbol) invalidate() {
	s.Value = nil
	s.ValueParsed = false
}
