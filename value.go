package ads

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"

	"cloud.google.com/go/civil"
)

// Typed values
//
// ReadValue, ReadValues and Update.Data return a PLC value as the Go type
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

// formatScalar renders a decoded scalar as the string API returns it. Dates and
// times keep every digit the PLC stored: TIME as h:mm:ss with hours unbounded,
// TOD as hh:mm:ss, both with trailing zeros of the fraction trimmed.
func formatScalar(v any) string {
	switch v := v.(type) {
	case bool:
		return strconv.FormatBool(v)
	case int8:
		return strconv.FormatInt(int64(v), 10)
	case int16:
		return strconv.FormatInt(int64(v), 10)
	case int32:
		return strconv.FormatInt(int64(v), 10)
	case int64:
		return strconv.FormatInt(v, 10)
	case uint8:
		return strconv.FormatUint(uint64(v), 10)
	case uint16:
		return strconv.FormatUint(uint64(v), 10)
	case uint32:
		return strconv.FormatUint(uint64(v), 10)
	case uint64:
		return strconv.FormatUint(v, 10)
	case float32:
		return strconv.FormatFloat(float64(v), 'f', -1, 32)
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case string:
		return v
	case time.Duration:
		return formatClock(v)
	case civil.Time:
		return formatClock(time.Duration(v.Hour)*time.Hour + time.Duration(v.Minute)*time.Minute +
			time.Duration(v.Second)*time.Second + time.Duration(v.Nanosecond))
	case civil.Date:
		return v.String()
	case civil.DateTime:
		return v.Date.String() + " " + formatClock(time.Duration(v.Time.Hour)*time.Hour+
			time.Duration(v.Time.Minute)*time.Minute+time.Duration(v.Time.Second)*time.Second+
			time.Duration(v.Time.Nanosecond))
	}
	return fmt.Sprint(v)
}

// formatClock renders d as hh:mm:ss[.fffffffff], hours unbounded and the
// fraction's trailing zeros trimmed.
func formatClock(d time.Duration) string {
	neg := d < 0
	if neg {
		d = -d
	}
	h, rem := d/time.Hour, d%time.Hour
	m, rem := rem/time.Minute, rem%time.Minute
	s, ns := rem/time.Second, rem%time.Second
	out := fmt.Sprintf("%02d:%02d:%02d", h, m, s)
	if ns != 0 {
		out += strings.TrimRight(fmt.Sprintf(".%09d", ns), "0")
	}
	if neg {
		out = "-" + out
	}
	return out
}

// parseClock parses the string form formatClock produces, [-]h:mm[:ss[.f]]
// with hours unbounded, or a Go duration such as "25h1m1.001s".
func parseClock(s string) (time.Duration, error) {
	if !strings.Contains(s, ":") {
		return time.ParseDuration(s)
	}
	neg := strings.HasPrefix(s, "-")
	parts := strings.Split(strings.TrimPrefix(s, "-"), ":")
	if len(parts) < 2 || len(parts) > 3 {
		return 0, fmt.Errorf("expected h:mm or h:mm:ss[.fffffffff], got %q", s)
	}
	h, err := strconv.ParseUint(parts[0], 10, 32)
	if err != nil {
		return 0, fmt.Errorf("hours in %q: %w", s, err)
	}
	m, err := strconv.ParseUint(parts[1], 10, 8)
	if err != nil || m > 59 {
		return 0, fmt.Errorf("minutes in %q must be 0-59", s)
	}
	d := time.Duration(h)*time.Hour + time.Duration(m)*time.Minute
	if len(parts) == 3 {
		sec, frac, _ := strings.Cut(parts[2], ".")
		sv, err := strconv.ParseUint(sec, 10, 8)
		if err != nil || sv > 59 {
			return 0, fmt.Errorf("seconds in %q must be 0-59", s)
		}
		d += time.Duration(sv) * time.Second
		if frac != "" {
			if len(frac) > 9 {
				return 0, fmt.Errorf("fraction in %q is finer than a nanosecond", s)
			}
			ns, err := strconv.ParseUint(frac+strings.Repeat("0", 9-len(frac)), 10, 32)
			if err != nil {
				return 0, fmt.Errorf("fraction in %q: %w", s, err)
			}
			d += time.Duration(ns)
		}
	}
	if neg {
		d = -d
	}
	return d, nil
}

// parseTimeOfDay parses a clock string that must fall within one day.
func parseTimeOfDay(s string) (time.Duration, error) {
	d, err := parseClock(s)
	if err != nil {
		return 0, err
	}
	if d < 0 || d >= 24*time.Hour {
		return 0, fmt.Errorf("time of day %q is not within a day", s)
	}
	return d, nil
}

// parseDateTime parses "2006-01-02 15:04:05[.fffffffff]", with a space or a
// "T" between date and time, as UTC.
func parseDateTime(s string) (time.Time, error) {
	return time.Parse("2006-01-02 15:04:05.999999999", strings.Replace(s, "T", " ", 1))
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
	return "", fmt.Errorf("unknown format cannot parse: %s", s.DataType)
}
