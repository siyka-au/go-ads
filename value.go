package ads

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
	"strings"
	"time"
	"unicode/utf16"

	"cloud.google.com/go/civil"
)

// Typed values
//
// ReadValue, ReadValues and Update.Value return a PLC value as the Go type
// below. Structs decode to map[string]any keyed by member name, arrays to []any
// in index order (nested for each extra dimension), and enums and aliases to
// their underlying type.
//
//	BOOL                       bool
//	SINT, INT, DINT, LINT      int8, int16, int32, int64
//	USINT/BYTE, UINT/WORD      uint8, uint16
//	UDINT/DWORD, ULINT/LWORD   uint32, uint64
//	REAL, LREAL                float32, float64
//	STRING, WSTRING            string
//	TIME, LTIME                time.Duration
//	TOD, LTOD                  civil.Time
//	DATE, LDATE                civil.Date
//	DT, LDT                    civil.DateTime
//
// DATE, DT and their long forms count from 1970-01-01 and are taken as UTC,
// TwinCAT's convention. DATE stores seconds and LDATE nanoseconds; TwinCAT's
// conversions keep both at midnight, and any time of day a raw write leaves in
// one is dropped. Use civil's In method to get a time.Time in another zone.

// scalarWidths is the encoded size of each fixed-width scalar type; STRING and
// WSTRING take the symbol's declared length.
var scalarWidths = map[string]uint32{
	"BOOL": 1, "SINT": 1, "USINT": 1, "BYTE": 1,
	"INT": 2, "INT16": 2, "UINT": 2, "UINT16": 2, "WORD": 2,
	"DINT": 4, "UDINT": 4, "DWORD": 4, "REAL": 4,
	"LINT": 8, "ULINT": 8, "LWORD": 8, "LREAL": 8,
	"TIME": 4, "TOD": 4, "TIME_OF_DAY": 4, "DATE": 4, "DT": 4, "DATE_AND_TIME": 4,
	"LTIME": 8, "LTOD": 8, "LTIME_OF_DAY": 8, "LDATE": 8, "LDT": 8, "LDATE_AND_TIME": 8,
}

// sizeErrorLabel keeps the type names the size errors have always reported.
var sizeErrorLabel = map[string]string{
	"USINT": "BYTE", "UINT": "WORD", "UINT16": "WORD", "UDINT": "DWORD",
	"INT16": "INT", "LWORD": "ULINT", "TIME_OF_DAY": "TOD", "DATE_AND_TIME": "DT",
	"LTIME_OF_DAY": "LTOD", "LDATE_AND_TIME": "LDT",
}

const (
	nsPerDay = int64(24 * time.Hour)
	msPerDay = nsPerDay / int64(time.Millisecond)
)

// decodeScalar decodes b, the exact bytes of one value, as dataType.
func decodeScalar(dataType string, b []byte) (any, error) {
	if w, ok := scalarWidths[dataType]; ok && uint32(len(b)) != w {
		label := dataType
		if l, ok := sizeErrorLabel[dataType]; ok {
			label = l
		}
		return nil, fmt.Errorf("%s Size Wrong", label)
	}
	le := binary.LittleEndian
	switch dataType {
	case "BOOL":
		return b[0] != 0, nil
	case "SINT":
		return int8(b[0]), nil
	case "USINT", "BYTE":
		return b[0], nil
	case "INT", "INT16":
		return int16(le.Uint16(b)), nil
	case "UINT", "UINT16", "WORD":
		return le.Uint16(b), nil
	case "DINT":
		return int32(le.Uint32(b)), nil
	case "UDINT", "DWORD":
		return le.Uint32(b), nil
	case "LINT":
		return int64(le.Uint64(b)), nil
	case "ULINT", "LWORD":
		return le.Uint64(b), nil
	case "REAL":
		return math.Float32frombits(le.Uint32(b)), nil
	case "LREAL":
		return math.Float64frombits(le.Uint64(b)), nil
	case "STRING":
		if i := bytes.IndexByte(b, 0); i >= 0 {
			b = b[:i]
		}
		return string(b), nil
	case "WSTRING":
		n := len(b) &^ 1
		for i := 0; i+1 < len(b); i += 2 {
			if b[i] == 0 && b[i+1] == 0 {
				n = i
				break
			}
		}
		units := make([]uint16, n/2)
		for i := range units {
			units[i] = le.Uint16(b[i*2:])
		}
		return string(utf16.Decode(units)), nil
	case "TIME":
		return time.Duration(le.Uint32(b)) * time.Millisecond, nil
	case "LTIME":
		ns := le.Uint64(b)
		if ns > math.MaxInt64 {
			return nil, fmt.Errorf("LTIME %d ns exceeds time.Duration", ns)
		}
		return time.Duration(ns), nil
	case "TOD", "TIME_OF_DAY":
		ms := int64(le.Uint32(b))
		if ms >= msPerDay {
			return nil, fmt.Errorf("TOD %d ms is not within a day", ms)
		}
		return civilTimeOf(time.Duration(ms) * time.Millisecond), nil
	case "LTOD", "LTIME_OF_DAY":
		ns := le.Uint64(b)
		if ns >= uint64(nsPerDay) {
			return nil, fmt.Errorf("LTOD %d ns is not within a day", ns)
		}
		return civilTimeOf(time.Duration(ns)), nil
	case "DATE":
		return civil.DateOf(time.Unix(int64(le.Uint32(b)), 0).UTC()), nil
	case "LDATE":
		return civil.DateOf(time.Unix(0, int64(le.Uint64(b))).UTC()), nil
	case "DT", "DATE_AND_TIME":
		return civil.DateTimeOf(time.Unix(int64(le.Uint32(b)), 0).UTC()), nil
	case "LDT", "LDATE_AND_TIME":
		return civil.DateTimeOf(time.Unix(0, int64(le.Uint64(b))).UTC()), nil
	}
	return nil, fmt.Errorf("unknown format cannot parse: %s", dataType)
}

func civilTimeOf(d time.Duration) civil.Time {
	return civil.Time{
		Hour:       int(d / time.Hour),
		Minute:     int(d % time.Hour / time.Minute),
		Second:     int(d % time.Minute / time.Second),
		Nanosecond: int(d % time.Second),
	}
}

// encodeScalar encodes v as dataType. length is the symbol's declared size,
// used by STRING and WSTRING. v must be the Go type decodeScalar returns for
// dataType, except that integer types take any Go integer within range.
func encodeScalar(dataType string, v any, length uint32) ([]byte, error) {
	le := binary.LittleEndian
	switch dataType {
	case "BOOL":
		b, ok := v.(bool)
		if !ok {
			return nil, typeError(dataType, v, "bool")
		}
		if b {
			return []byte{1}, nil
		}
		return []byte{0}, nil
	case "SINT":
		n, err := intInRange(dataType, v, math.MinInt8, math.MaxInt8)
		return []byte{byte(n)}, err
	case "USINT", "BYTE":
		n, err := uintInRange(dataType, v, math.MaxUint8)
		return []byte{byte(n)}, err
	case "INT", "INT16":
		n, err := intInRange(dataType, v, math.MinInt16, math.MaxInt16)
		return le.AppendUint16(nil, uint16(n)), err
	case "UINT", "UINT16", "WORD":
		n, err := uintInRange(dataType, v, math.MaxUint16)
		return le.AppendUint16(nil, uint16(n)), err
	case "DINT":
		n, err := intInRange(dataType, v, math.MinInt32, math.MaxInt32)
		return le.AppendUint32(nil, uint32(n)), err
	case "UDINT", "DWORD":
		n, err := uintInRange(dataType, v, math.MaxUint32)
		return le.AppendUint32(nil, uint32(n)), err
	case "LINT":
		n, err := intInRange(dataType, v, math.MinInt64, math.MaxInt64)
		return le.AppendUint64(nil, uint64(n)), err
	case "ULINT", "LWORD":
		n, err := uintInRange(dataType, v, math.MaxUint64)
		return le.AppendUint64(nil, n), err
	case "REAL":
		f, ok := v.(float32)
		if !ok {
			return nil, typeError(dataType, v, "float32")
		}
		return le.AppendUint32(nil, math.Float32bits(f)), nil
	case "LREAL":
		f, ok := v.(float64)
		if !ok {
			return nil, typeError(dataType, v, "float64")
		}
		return le.AppendUint64(nil, math.Float64bits(f)), nil
	case "STRING":
		s, ok := v.(string)
		if !ok {
			return nil, typeError(dataType, v, "string")
		}
		// A NUL ends the string on the PLC, so the rest would be lost.
		if strings.IndexByte(s, 0) >= 0 {
			return nil, fmt.Errorf("STRING %q contains a NUL, which would end it on the PLC", s)
		}
		// The last byte is the terminator the PLC expects.
		if length < 1 || uint32(len(s)) > length-1 {
			return nil, fmt.Errorf("STRING of %d bytes does not fit %d bytes with its terminator", len(s), length)
		}
		buf := make([]byte, length)
		copy(buf, s)
		return buf, nil
	case "WSTRING":
		s, ok := v.(string)
		if !ok {
			return nil, typeError(dataType, v, "string")
		}
		if strings.IndexByte(s, 0) >= 0 {
			return nil, fmt.Errorf("WSTRING %q contains a NUL, which would end it on the PLC", s)
		}
		units := utf16.Encode([]rune(s))
		if length < 2 || uint32(len(units)) > (length-2)/2 {
			return nil, fmt.Errorf("WSTRING of %d UTF-16 units does not fit %d bytes with its terminator", len(units), length)
		}
		buf := make([]byte, length)
		for i, u := range units {
			le.PutUint16(buf[i*2:], u)
		}
		return buf, nil
	case "TIME":
		d, ok := v.(time.Duration)
		if !ok {
			return nil, typeError(dataType, v, "time.Duration")
		}
		if d < 0 || d%time.Millisecond != 0 || d/time.Millisecond > math.MaxUint32 {
			return nil, fmt.Errorf("TIME %v must be whole milliseconds from 0 to %d ms", d, uint32(math.MaxUint32))
		}
		return le.AppendUint32(nil, uint32(d/time.Millisecond)), nil
	case "LTIME":
		d, ok := v.(time.Duration)
		if !ok {
			return nil, typeError(dataType, v, "time.Duration")
		}
		if d < 0 {
			return nil, fmt.Errorf("LTIME %v is negative", d)
		}
		return le.AppendUint64(nil, uint64(d)), nil
	case "TOD", "TIME_OF_DAY", "LTOD", "LTIME_OF_DAY":
		t, ok := v.(civil.Time)
		if !ok {
			return nil, typeError(dataType, v, "civil.Time")
		}
		if !t.IsValid() {
			return nil, fmt.Errorf("%s %v is not a valid time of day", dataType, t)
		}
		d := time.Duration(t.Hour)*time.Hour + time.Duration(t.Minute)*time.Minute +
			time.Duration(t.Second)*time.Second + time.Duration(t.Nanosecond)
		if dataType == "LTOD" || dataType == "LTIME_OF_DAY" {
			return le.AppendUint64(nil, uint64(d)), nil
		}
		if d%time.Millisecond != 0 {
			return nil, fmt.Errorf("TOD %v must be whole milliseconds", t)
		}
		return le.AppendUint32(nil, uint32(d/time.Millisecond)), nil
	case "DATE", "LDATE":
		d, ok := v.(civil.Date)
		if !ok {
			return nil, typeError(dataType, v, "civil.Date")
		}
		if !d.IsValid() {
			return nil, fmt.Errorf("%s %v is not a valid date", dataType, d)
		}
		return encodeInstant(dataType, d.In(time.UTC))
	case "DT", "DATE_AND_TIME", "LDT", "LDATE_AND_TIME":
		dt, ok := v.(civil.DateTime)
		if !ok {
			return nil, typeError(dataType, v, "civil.DateTime")
		}
		if !dt.IsValid() {
			return nil, fmt.Errorf("%s %v is not a valid date and time", dataType, dt)
		}
		return encodeInstant(dataType, dt.In(time.UTC))
	}
	return nil, fmt.Errorf("datatype %q write is not implemented yet", dataType)
}

// encodeInstant stores t as seconds (DATE, DT: uint32 from 1970) or
// nanoseconds (LDATE, LDT: int64 from 1970), refusing what the type cannot hold.
func encodeInstant(dataType string, t time.Time) ([]byte, error) {
	le := binary.LittleEndian
	if strings.HasPrefix(dataType, "L") {
		// time.Time spans far more than int64 nanoseconds do; UnixNano is
		// undefined outside 1677..2262, so check the range first.
		if t.Before(time.Unix(0, math.MinInt64)) || t.After(time.Unix(0, math.MaxInt64)) {
			return nil, fmt.Errorf("%s %v is outside 1677-09-21 to 2262-04-11", dataType, t)
		}
		return le.AppendUint64(nil, uint64(t.UnixNano())), nil
	}
	if t.Nanosecond() != 0 {
		return nil, fmt.Errorf("%s %v must be whole seconds", dataType, t)
	}
	sec := t.Unix()
	if sec < 0 || sec > math.MaxUint32 {
		return nil, fmt.Errorf("%s %v is outside 1970-01-01 to 2106-02-07", dataType, t)
	}
	return le.AppendUint32(nil, uint32(sec)), nil
}

func typeError(dataType string, v any, want string) error {
	return fmt.Errorf("%s takes %s, got %T", dataType, want, v)
}

// intInRange returns v, any Go integer, as int64 if it lies in [lo, hi].
func intInRange(dataType string, v any, lo, hi int64) (int64, error) {
	var n int64
	switch x := v.(type) {
	case int:
		n = int64(x)
	case int8:
		n = int64(x)
	case int16:
		n = int64(x)
	case int32:
		n = int64(x)
	case int64:
		n = x
	case uint, uint8, uint16, uint32, uint64:
		u, err := uintInRange(dataType, v, uint64(hi))
		return int64(u), err
	default:
		return 0, typeError(dataType, v, "an integer")
	}
	if n < lo || n > hi {
		return 0, fmt.Errorf("%s %d is outside %d to %d", dataType, n, lo, hi)
	}
	return n, nil
}

// uintInRange returns v, any Go integer, as uint64 if it lies in [0, hi].
func uintInRange(dataType string, v any, hi uint64) (uint64, error) {
	var n uint64
	switch x := v.(type) {
	case uint:
		n = uint64(x)
	case uint8:
		n = uint64(x)
	case uint16:
		n = uint64(x)
	case uint32:
		n = uint64(x)
	case uint64:
		n = x
	case int, int8, int16, int32, int64:
		i, err := intInRange(dataType, v, math.MinInt64, math.MaxInt64)
		if err != nil {
			return 0, err
		}
		if i < 0 {
			return 0, fmt.Errorf("%s %d is outside 0 to %d", dataType, i, hi)
		}
		n = uint64(i)
	default:
		return 0, typeError(dataType, v, "an integer")
	}
	if n > hi {
		return 0, fmt.Errorf("%s %d is outside 0 to %d", dataType, n, hi)
	}
	return n, nil
}

// encode serialises v, in the shape ReadValue returns for this symbol, into
// the symbol's bytes. A struct takes a map naming every member and nothing
// else; an array a slice of exactly its element count, nested per dimension.
func (s *symbol) encode(v any, datatypes map[string]SymbolUploadDataType) ([]byte, error) {
	if len(s.Children) == 0 {
		dt, err := s.scalarType(datatypes)
		if err != nil {
			return nil, err
		}
		b, err := encodeScalar(dt, v, s.Length)
		if err != nil {
			return nil, err
		}
		if uint32(len(b)) != s.Length {
			return nil, fmt.Errorf("%s encodes to %d bytes, symbol is %d", dt, len(b), s.Length)
		}
		return b, nil
	}
	buf := make([]byte, s.Length)
	put := func(c *symbol, cv any, label string) error {
		if c.BitMember {
			// A BIT member sets one bit of the parent; Offset counts bits.
			b, ok := cv.(bool)
			if !ok {
				return fmt.Errorf("%s: BIT takes bool, got %T", label, cv)
			}
			if c.Length != 1 || uint64(c.Offset)/8 >= uint64(len(buf)) {
				return fmt.Errorf("%s: bit %d (width %d) does not fit the %d-byte value", label, c.Offset, c.Length, len(buf))
			}
			WriteBit(buf, int(c.Offset), b)
			return nil
		}
		cb, err := c.encode(cv, datatypes)
		if err != nil {
			return fmt.Errorf("%s: %w", label, err)
		}
		if end := uint64(c.Offset) + uint64(len(cb)); end > uint64(len(buf)) {
			return fmt.Errorf("%s: ends at %d, past the %d-byte value", label, end, len(buf))
		}
		copy(buf[c.Offset:], cb)
		return nil
	}
	for _, c := range s.Children {
		if isArrayElement(c) {
			elems := sortedElements(s.Children)
			list, ok := v.([]any)
			if !ok {
				return nil, fmt.Errorf("%s is an array and takes []any, got %T", s.DataType, v)
			}
			if len(list) != len(elems) {
				return nil, fmt.Errorf("%s has %d elements, got %d", s.DataType, len(elems), len(list))
			}
			for i, e := range elems {
				if err := put(e, list[i], e.Name); err != nil {
					return nil, err
				}
			}
			return buf, nil
		}
		break
	}
	fields, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s is a struct and takes map[string]any, got %T", s.DataType, v)
	}
	for name := range fields {
		if _, ok := s.Children[name]; !ok {
			return nil, fmt.Errorf("%s has no member %q", s.DataType, name)
		}
	}
	for name, c := range s.Children {
		fv, ok := fields[name]
		if !ok {
			return nil, fmt.Errorf("%s: member %q missing; a struct write sets every member", s.DataType, name)
		}
		if err := put(c, fv, name); err != nil {
			return nil, err
		}
	}
	return buf, nil
}

// copyData returns v with its maps and slices copied, so a caller holding a
// struct or array value cannot alter the cache's. Scalars are values already.
func copyData(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = copyData(e)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = copyData(e)
		}
		return out
	}
	return v
}

// isScalarType reports whether dataType is a primitive decodeScalar handles.
func isScalarType(dataType string) bool {
	_, fixed := scalarWidths[dataType]
	return fixed || dataType == "STRING" || dataType == "WSTRING"
}

// scalarType resolves the primitive a leaf symbol decodes as: its own type, the
// base of an alias or enum from the datatype table, the ADST_ code the PLC sent,
// or as a last resort a type inferred from its size.
func (s *symbol) scalarType(datatypes map[string]SymbolUploadDataType) (string, error) {
	if isScalarType(s.DataType) {
		return s.DataType, nil
	}
	if dt, ok := datatypes[s.DataType]; ok && isScalarType(dt.DataType) {
		return dt.DataType, nil
	}
	// The ADST_ code is authoritative when present: the PLC sends the base type
	// (e.g. ADSTReal32 for a REAL-based alias).
	if resolved := adsTypeToString(s.BaseType); resolved != "" {
		// An array reports its element's ADST_ code with the whole array's
		// Length, so resolving on BaseType alone would hand the scalar case
		// 40 bytes to read a 4-byte DINT. That reports "DINT Size Wrong" --
		// a type the caller never asked for, and no hint that the datatype
		// table is what is missing. Children (and with them per-element
		// parsing) are only linked when that table resolves the type, so
		// name the real problem.
		if w := adsTypeWidth(s.BaseType); w > 0 && w != s.Length {
			return "", fmt.Errorf("cannot parse %s: %d bytes, but its base type %s is %d bytes — "+
				"this looks like an array or struct, which needs the datatype table; call LoadSymbols()",
				s.DataType, s.Length, resolved, w)
		}
		return resolved, nil
	}
	// Infer from the size when neither the ADST_ code nor the table resolves it.
	// Only 1- and 2-byte widths — see inferBaseType for why 4/8 are refused.
	if inferred := inferBaseType(s.Length, s.BaseType); inferred != "" {
		s.warnInferenceOnce("inferring base type from size (no datatype table loaded; LoadSymbols() recommended for user-defined types)",
			"symbol", s.DataType, "size", s.Length, "baseType", s.BaseType, "inferred", inferred)
		return inferred, nil
	}
	// A struct, or an alias the table would resolve, looks the same from here as
	// a type this library cannot decode; without the table, name the fix.
	if len(datatypes) == 0 {
		return "", fmt.Errorf("unknown format cannot parse: %s is not a primitive type and the datatype table is not loaded; call LoadSymbols()", s.DataType)
	}
	return "", fmt.Errorf("unknown format cannot parse: %s", s.DataType)
}
