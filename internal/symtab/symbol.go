package symtab

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/siyka-au/go-ads/v3/ams"
)

// symbolKey normalizes a symbol name for use as an internal map key.
// TwinCAT treats symbol names case-insensitively (IEC 61131-3).
// TC2 returns uppercase, TC3 preserves original casing — lowercasing
// ensures consistent lookups regardless of caller or PLC casing.
func Key(name string) string { return strings.ToLower(name) }

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

// subrangeBaseTypes are the integer types IEC 61131-3 allows a subrange
// restriction on. Not BOOL, REAL/LREAL, STRING, or the TIME/DATE family --
// TwinCAT does not emit a range suffix for those.
var subrangeBaseTypes = map[string]bool{
	"SINT": true, "USINT": true, "BYTE": true,
	"INT": true, "UINT": true, "WORD": true,
	"DINT": true, "UDINT": true, "DWORD": true,
	"LINT": true, "ULINT": true, "LWORD": true,
}

// parseIntRange extracts a subrange annotation like "INT (-10..10)" -- how
// TwinCAT reports an IEC 61131-3 subrange restriction (e.g. declared
// `nSubRange : INT(-10..10);`) in a symbol's type name. Mirrors
// normalizeStringDataType's STRING(n)/WSTRING(n) handling, but for integer
// subranges, which carry two bounds instead of a length, and a space before
// the parenthesis where STRING(n) has none.
//
// Returns ok=false, leaving dt to be handled as an ordinary type name, if dt
// doesn't have this shape, its base isn't a subrange-eligible integer type,
// or the bounds don't parse as low <= high.
func parseIntRange(dt string) (name string, low, high int64, ok bool) {
	open := strings.IndexByte(dt, '(')
	if open < 0 || dt[len(dt)-1] != ')' {
		return "", 0, 0, false
	}
	name = strings.TrimSpace(dt[:open])
	if !subrangeBaseTypes[name] {
		return "", 0, 0, false
	}
	lowStr, highStr, ok := strings.Cut(dt[open+1:len(dt)-1], "..")
	if !ok {
		return "", 0, 0, false
	}
	low, err := strconv.ParseInt(strings.TrimSpace(lowStr), 10, 64)
	if err != nil {
		return "", 0, 0, false
	}
	high, err = strconv.ParseInt(strings.TrimSpace(highStr), 10, 64)
	if err != nil || low > high {
		return "", 0, 0, false
	}
	return name, low, high, true
}

// resolveDataType normalizes a PLC-reported type name for storage on a Symbol
// or TypeInfo: STRING(n)/WSTRING(n) collapse to their bare name (see
// normalizeStringDataType), and a subrange like INT(-10..10) collapses to its
// base name with rangeMin/rangeMax set. A name is never both -- STRING and an
// integer subrange are mutually exclusive shapes -- so callers get exactly one
// of the two treatments.
func resolveDataType(dt string) (name string, rangeMin, rangeMax *int64) {
	if base, low, high, ok := parseIntRange(dt); ok {
		return base, &low, &high
	}
	return normalizeStringDataType(dt), nil, nil
}

// symbol is the internal cache record; external callers use SymbolView.
//
// Field guards: the metadata (FullName, DataType, RangeMin, RangeMax,
// Constants, Group, Offset, Length, BaseType, Flags, Parent, Children) is
// immutable after construction and needs no lock. Value, Valid, ValueParsed
// and LastUpdateTime are guarded by cache.lock, as is Handle -- zeroed on
// reload, and an observed zero simply fails the next PLC call and prompts a
// re-resolve. Parent/Children form a tree fixed at discovery.
type Symbol struct {
	FullName       string
	LastUpdateTime time.Time
	Name           string
	DataType       string
	Comment        string
	Handle         uint32
	Group          uint32
	Offset         uint32
	Length         uint32
	BaseType       ams.DataType // protocol ADST_ code (e.g., DataTypeReal32=4 for REAL)
	Flags          ams.SymbolFlag
	ContextMask    uint8 // PLC task context (bits 8-11 of Flags); 0 = no task binding

	// RangeMin/RangeMax are the declared bounds of an IEC 61131-3 subrange type
	// (e.g. INT(-10..10)), parsed from the type name by parseIntRange. Both nil
	// for a symbol with no subrange restriction; 0 is a legitimate bound, so
	// presence is signaled by non-nil rather than a separate flag.
	RangeMin, RangeMax *int64

	// Constants is an enum's declared members, parsed from its datatype-table
	// entry's EnumInfo block (see decodeExtendedDatatypeInfo). Nil for a
	// non-enum symbol, or an enum whose datatype table wasn't loaded.
	Constants []EnumConstant

	Value       any // decoded to its Go type; see value.go
	Valid       bool
	ValueParsed bool // true after first successful parse

	// BitMember marks a BIT member of a struct: Offset and Length count bits
	// from the start of the parent, not bytes.
	BitMember bool

	Parent   *Symbol
	Children map[string]*Symbol

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
	BaseTypeWarned bool
}

// warnInferenceOnce logs the base-type inference warning the first time it
// applies to this symbol and stays quiet afterwards.
//
// Not atomic: symbols are parsed under cache.lock (see the F-39 note in
// improvements.md), so this field has the same protection every other field on
// symbol has. If that ever changes, this becomes a race — and a benign one, since
// the worst outcome is a duplicate warning.
func (s *Symbol) warnInferenceOnce(msg string, args ...any) {
	if s.inferenceWarned {
		return
	}
	s.inferenceWarned = true
	s.log().Warn(msg, args...)
}

// log returns the logger records about this symbol belong on: the session's if
// the symbol came from one, the package default otherwise.
func (s *Symbol) log() *slog.Logger {
	if s == nil || s.logger == nil {
		return slog.Default()
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
func StampLogger(s *Symbol, lg *slog.Logger, depth int) {
	if s == nil || lg == nil || depth >= maxTreeDepth {
		return
	}
	s.logger = lg
	for _, child := range s.Children {
		StampLogger(child, lg, depth+1)
	}
}

// stampLoggerOnAll stamps a whole symbol map, for the bulk cache swaps.
func StampLoggerOnAll(symbols map[string]*Symbol, lg *slog.Logger) {
	for _, s := range symbols {
		StampLogger(s, lg, 0)
	}
}

// invalidate drops the cached value so the next read goes to the PLC. Caller
// holds cache.lock.
func (s *Symbol) Invalidate() {
	s.Value = nil
	s.ValueParsed = false
}

// ParseSymbolInfo decodes a GroupSymbolInfoByNameEx response: a symbolEntry
// header followed by the NUL-terminated name, type and comment.
func ParseSymbolInfo(resp []byte) (ams.SymbolInfo, error) {
	buff := bytes.NewBuffer(resp)
	entry := SymbolEntry{}
	if err := binary.Read(buff, binary.LittleEndian, &entry); err != nil {
		return ams.SymbolInfo{}, fmt.Errorf("parse symbol entry: %w", err)
	}
	name := make([]byte, entry.NameLength)
	if err := binary.Read(buff, binary.LittleEndian, name); err != nil {
		return ams.SymbolInfo{}, fmt.Errorf("read symbol name: %w", err)
	}
	buff.Next(1) // null terminator
	dt := make([]byte, entry.TypeLength)
	if err := binary.Read(buff, binary.LittleEndian, dt); err != nil {
		return ams.SymbolInfo{}, fmt.Errorf("read symbol type: %w", err)
	}
	buff.Next(1) // null terminator
	comment := make([]byte, entry.CommentLength)
	if err := binary.Read(buff, binary.LittleEndian, comment); err != nil {
		return ams.SymbolInfo{}, fmt.Errorf("read symbol comment: %w", err)
	}
	return ams.SymbolInfo{
		Name:     string(name), // PLC-returned casing (authoritative)
		DataType: string(dt),
		Comment:  string(comment),
		Group:    ams.Group(entry.IGroup),
		Offset:   entry.IOffs,
		Length:   entry.Size,
		BaseType: ams.DataType(entry.DataType),
		Flags:    ams.SymbolFlag(entry.Flags),
	}, nil
}

// FromInfo builds a symbol from a single-name lookup. It has no children:
// struct and array members need the full symbol and data type tables.
func FromInfo(info ams.SymbolInfo) *Symbol {
	dataType, rangeMin, rangeMax := resolveDataType(info.DataType)
	return &Symbol{
		FullName:       info.Name,
		Name:           info.Name,
		DataType:       dataType,
		RangeMin:       rangeMin,
		RangeMax:       rangeMax,
		Comment:        info.Comment,
		Group:          uint32(info.Group),
		Offset:         info.Offset,
		Length:         info.Length,
		BaseType:       info.BaseType,
		Flags:          info.Flags,
		ContextMask:    info.Flags.ContextMask(),
		LastUpdateTime: time.Now(),
	}
}

// maxTreeDepth caps recursion over a symbol tree, as a defense against a
// malformed or self-referential PLC response.
const maxTreeDepth = 256
