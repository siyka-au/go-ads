package ads

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strconv"
	"time"
	"unicode/utf16"
)

func (s *symbol) parse(data []byte, offset int, datatypes map[string]SymbolUploadDataType) (string, error) {
	start := offset
	// reject oversized s.Length before arithmetic. uint32 → int
	// conversion wraps on 32-bit Go; an attacker-controlled or buggy symbol
	// entry with Length = 0xFFFFFFFF produces a negative int that bypasses
	// the bounds guard and panics on slice. Compare in uint64 space to dodge
	// the wrap entirely.
	if uint64(s.Length) > uint64(len(data)) {
		return "", fmt.Errorf("parse %s: s.Length %d exceeds data buffer size %d", s.DataType, s.Length, len(data))
	}
	stop := start + int(s.Length)
	if start+int(s.Length) > len(data) {
		stop = len(data)
	}

	if len(s.Children) > 0 {
		for _, value := range s.Children {
			if _, err := value.parse(data[offset:stop], int(value.Offset), datatypes); err != nil {
				return "", fmt.Errorf("parsing child %q: %w", value.Name, err)
			}
		}
		s.Data = s.dataTree()
		s.updateValue(s.getJSON())
		return s.Value, nil
	}

	if start+int(s.Length) > len(data) {
		return "", fmt.Errorf("data too short for %s at offset %d: need %d bytes, got %d", s.DataType, start, s.Length, len(data)-start)
	}

	dt, err := s.scalarType(datatypes)
	if err != nil {
		return "", err
	}
	v, err := decodeScalar(dt, data[start:stop])
	if err != nil {
		return "", err
	}
	s.Data = v
	s.updateValue(formatScalar(v))
	return s.Value, nil
}

// updateValue stores a freshly decoded value. It always stores: the data came
// off the wire, so it is the current value. MinUpdateInterval is a read-cache
// TTL (see readFromSymbolRetry), not a rate limit on decoding -- applying it
// here dropped any change arriving within the interval of the previous one, so
// a notification returned the value it was replacing.
func (s *symbol) updateValue(newValue string) {
	s.LastUpdateTime = time.Now()
	s.Value = newValue
	s.Valid = true
	s.ValueParsed = true
}

var parseableTypes = []string{
	"BOOL",
	"BYTE",
	"USINT",
	"UINT",
	"UINT16",
	"WORD",
	"UDINT",
	"DWORD",
	"SINT",
	"INT",
	"INT16",
	"DINT",
	"REAL",
	"LREAL",
	"STRING",
	"WSTRING",
	"TIME",
	"TOD",
	"TIME_OF_DAY",
	"DATE",
	"DT",
	"DATE_AND_TIME",
	"LINT",
	"ULINT",
	"LWORD",
	"LTIME",
	"LTOD",
	"LTIME_OF_DAY",
	"LDATE",
	"LDT",
	"LDATE_AND_TIME",
}

// inferBaseType guesses a base type from a symbol's byte size, the last resort
// when neither the ADST_ code nor the datatype table resolves it.
//
// Only 1- and 2-byte widths, where no IEEE-754 form exists. At 4 and 8 the layout
// is ambiguous (DINT/REAL, LINT/LREAL) and reading a REAL as a DINT silently
// corrupts every parse, so those return "" and the caller points at LoadSymbols.
// baseType is threaded through for the chain; only size is inspected today.
func inferBaseType(size uint32, baseType ADSDataType) string {
	_ = baseType // reserved for future width+type tightening; see godoc above.
	switch size {
	case 1:
		return "SINT"
	case 2:
		return "INT"
	default:
		// 4 and 8 deliberately omitted — DINT/REAL and LINT/LREAL share
		// these widths and the byte layout cannot be disambiguated without
		// the datatype table.
		return ""
	}
}

func (s *symbol) writeToNode(value string, datatypes map[string]SymbolUploadDataType) (data []byte, err error) {
	if len(s.Children) > 0 {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal([]byte(value), &fields); err != nil {
			return nil, fmt.Errorf("struct write requires JSON input: %w", err)
		}
		buf := make([]byte, s.Length)
		for name, child := range s.Children {
			raw, ok := fields[name]
			if !ok {
				continue
			}
			// Convert JSON value to string for writeToNode:
			// - JSON strings ("hello") → unquoted (hello)
			// - numbers/bools (42, true) → raw text (42, true)
			var childValue string
			if len(raw) > 0 && raw[0] == '"' {
				if err := json.Unmarshal(raw, &childValue); err != nil {
					return nil, fmt.Errorf("field %q: invalid JSON string: %w", name, err)
				}
			} else {
				childValue = string(raw)
			}
			childBytes, err := child.writeToNode(childValue, datatypes)
			if err != nil {
				return nil, fmt.Errorf("field %q: %w", name, err)
			}
			end64 := uint64(child.Offset) + uint64(child.Length)
			if end64 > uint64(len(buf)) {
				return nil, fmt.Errorf("field %q: offset+length %d exceeds struct size %d", name, end64, len(buf))
			}
			end := uint32(end64)
			copy(buf[child.Offset:end], childBytes)
		}
		return buf, nil
	}

	buf := new(bytes.Buffer)
	dt := s.DataType

	if !slices.Contains(parseableTypes, dt) {
		// Try datatype-table lookup first. If that fails or yields a non-parseable
		// type, fall back to size-based inference — mirrors the read path
		// at parse() so unknown types can round-trip via writeToSymbol.
		if datatypes != nil {
			if dtEntry, ok := datatypes[dt]; ok {
				if slices.Contains(parseableTypes, dtEntry.DataType) {
					dt = dtEntry.DataType
				}
			}
		}
		// If still not parseable after table lookup, infer from byte size.
		// Only 1/2 byte widths are inferred (signed-int) — see inferBaseType
		// doc for why 4/8 are refused (REAL/LREAL ambiguity).
		if !slices.Contains(parseableTypes, dt) {
			if inferred := inferBaseType(s.Length, s.BaseType); inferred != "" {
				s.warnInferenceOnce("inferring base type from size for write (no datatype table loaded; LoadSymbols() recommended)",
					"symbol", s.DataType,
					"size", s.Length,
					"baseType", s.BaseType,
					"inferred", inferred)
				dt = inferred
			} else {
				return nil, fmt.Errorf("data type %q not parseable and size %d not inferable (only 1/2 byte sizes auto-inferred to avoid REAL/LREAL ambiguity); call LoadSymbols() first or use a known type", s.DataType, s.Length)
			}
		}
	}
	switch dt {
	case "BOOL":
		v, e := strconv.ParseBool(value)
		if e != nil {
			return nil, e
		}

		if v {
			buf.WriteByte(1)
		} else {
			buf.WriteByte(0)
		}
	case "BYTE", "USINT": // Unsigned Short INT 0 to 255
		v, e := strconv.ParseUint(value, 10, 8)
		if e != nil {
			return nil, e
		}
		buf.WriteByte(uint8(v))
	case "UINT", "WORD", "UINT16":
		v, e := strconv.ParseUint(value, 10, 16)
		if e != nil {
			return nil, e
		}

		v16 := uint16(v)
		if err := binary.Write(buf, binary.LittleEndian, &v16); err != nil {
			return nil, fmt.Errorf("binary.Write UINT failed: %w", err)
		}
	case "UDINT", "DWORD":
		v, e := strconv.ParseUint(value, 10, 32)
		if e != nil {
			return nil, e
		}

		v32 := uint32(v)
		if err := binary.Write(buf, binary.LittleEndian, &v32); err != nil {
			return nil, fmt.Errorf("binary.Write UDINT failed: %w", err)
		}

	case "SINT": // Short INT -128 to 127
		v, e := strconv.ParseInt(value, 10, 8)
		if e != nil {
			return nil, e
		}
		buf.WriteByte(byte(int8(v)))
	case "INT", "INT16":
		v, e := strconv.ParseInt(value, 10, 16)
		if e != nil {
			return nil, e
		}

		v16 := int16(v)
		if err := binary.Write(buf, binary.LittleEndian, &v16); err != nil {
			return nil, fmt.Errorf("binary.Write INT failed: %w", err)
		}
	case "DINT":
		v, e := strconv.ParseInt(value, 10, 32)
		if e != nil {
			return nil, e
		}

		v32 := int32(v)
		if err := binary.Write(buf, binary.LittleEndian, &v32); err != nil {
			return nil, fmt.Errorf("binary.Write DINT failed: %w", err)
		}

	case "REAL":
		v, e := strconv.ParseFloat(value, 32)
		if e != nil {
			return nil, e
		}

		v32 := math.Float32bits(float32(v))
		if err := binary.Write(buf, binary.LittleEndian, &v32); err != nil {
			return nil, fmt.Errorf("binary.Write REAL failed: %w", err)
		}
	case "LREAL":
		v, e := strconv.ParseFloat(value, 64)
		if e != nil {
			return nil, e
		}

		v64 := math.Float64bits(v)
		if err := binary.Write(buf, binary.LittleEndian, &v64); err != nil {
			return nil, fmt.Errorf("binary.Write LREAL failed: %w", err)
		}
	case "LINT":
		v, e := strconv.ParseInt(value, 10, 64)
		if e != nil {
			return nil, e
		}
		if err := binary.Write(buf, binary.LittleEndian, &v); err != nil {
			return nil, fmt.Errorf("binary.Write LINT failed: %w", err)
		}
	case "ULINT", "LWORD":
		v, e := strconv.ParseUint(value, 10, 64)
		if e != nil {
			return nil, e
		}
		if err := binary.Write(buf, binary.LittleEndian, &v); err != nil {
			return nil, fmt.Errorf("binary.Write ULINT failed: %w", err)
		}
	case "TIME":
		d, e := parseClock(value)
		if e != nil {
			return nil, fmt.Errorf("TIME: %w", e)
		}
		if d < 0 || d/time.Millisecond > math.MaxUint32 {
			return nil, fmt.Errorf("TIME %s is outside 0 to %d ms", value, uint32(math.MaxUint32))
		}
		buf.Write(binary.LittleEndian.AppendUint32(nil, uint32(d/time.Millisecond)))
	case "LTIME":
		d, e := parseClock(value)
		if e != nil {
			return nil, fmt.Errorf("LTIME: %w", e)
		}
		if d < 0 {
			return nil, fmt.Errorf("LTIME %s is negative", value)
		}
		buf.Write(binary.LittleEndian.AppendUint64(nil, uint64(d)))
	case "TOD", "TIME_OF_DAY":
		d, e := parseTimeOfDay(value)
		if e != nil {
			return nil, fmt.Errorf("TOD: %w", e)
		}
		buf.Write(binary.LittleEndian.AppendUint32(nil, uint32(d/time.Millisecond)))
	case "LTOD", "LTIME_OF_DAY":
		d, e := parseTimeOfDay(value)
		if e != nil {
			return nil, fmt.Errorf("LTOD: %w", e)
		}
		buf.Write(binary.LittleEndian.AppendUint64(nil, uint64(d)))
	case "LDATE":
		t, e := time.Parse("2006-01-02", value)
		if e != nil {
			return nil, fmt.Errorf("LDATE: expected format 2006-01-02: %w", e)
		}
		buf.Write(binary.LittleEndian.AppendUint64(nil, uint64(t.UnixNano())))
	case "LDT", "LDATE_AND_TIME":
		t, e := parseDateTime(value)
		if e != nil {
			return nil, fmt.Errorf("LDT: expected format 2006-01-02 15:04:05.999999999: %w", e)
		}
		buf.Write(binary.LittleEndian.AppendUint64(nil, uint64(t.UnixNano())))
	case "DATE":
		t, e := time.Parse("2006-01-02", value)
		if e != nil {
			return nil, fmt.Errorf("DATE: expected format 2006-01-02: %w", e)
		}
		// Inverse of parse(): parse uses time.Unix(0, i*second) then Format in local TZ
		target := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
		secs := uint32(target.Unix())
		if err := binary.Write(buf, binary.LittleEndian, &secs); err != nil {
			return nil, fmt.Errorf("binary.Write DATE failed: %w", err)
		}
	case "DT", "DATE_AND_TIME":
		t, e := parseDateTime(value)
		if e != nil {
			return nil, fmt.Errorf("DT: expected format 2006-01-02 15:04:05: %w", e)
		}
		target := time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), 0, time.UTC)
		secs := uint32(target.Unix())
		if err := binary.Write(buf, binary.LittleEndian, &secs); err != nil {
			return nil, fmt.Errorf("binary.Write DT failed: %w", err)
		}
	case "STRING":
		// refuse to write to a STRING declared with Length=0. The PLC
		// expects at least 1 byte for the null terminator; a 0-byte payload
		// is silently truncating and indistinguishable from caller error.
		if s.Length < 1 {
			return nil, fmt.Errorf("STRING write requires s.Length >= 1, got %d", s.Length)
		}
		newBuf := make([]byte, s.Length)
		// Reserve last byte for null terminator — PLC expects null-terminated strings.
		maxLen := int(s.Length) - 1
		src := []byte(value)
		if len(src) > maxLen {
			src = src[:maxLen]
		}
		copy(newBuf, src)
		buf.Write(newBuf)
	case "WSTRING":
		// WSTRING needs at least 2 bytes for the UTF-16 null terminator.
		if s.Length < 2 {
			return nil, fmt.Errorf("WSTRING write requires s.Length >= 2, got %d", s.Length)
		}
		encoded := utf16.Encode([]rune(value))
		newBuf := make([]byte, s.Length)
		maxChars := (int(s.Length) - 2) / 2 // reserve 2 bytes for null terminator
		if len(encoded) > maxChars {
			encoded = encoded[:maxChars]
			// If truncation lands inside a UTF-16 surrogate pair (high
			// surrogate at the end with no following low surrogate), drop
			// the high surrogate to avoid emitting an invalid sequence.
			if len(encoded) > 0 {
				last := encoded[len(encoded)-1]
				if last >= 0xD800 && last <= 0xDBFF {
					encoded = encoded[:len(encoded)-1]
				}
			}
		}
		for i, r := range encoded {
			binary.LittleEndian.PutUint16(newBuf[i*2:], r)
		}
		buf.Write(newBuf)
	default:
		return nil, fmt.Errorf("datatype %q write is not implemented yet", s.DataType)
	}
	return buf.Bytes(), nil
}

// ReadBit extracts a single bit from a byte slice.
// bitIndex 0 is the least significant bit of the first byte.
// Returns false if bitIndex is out of range.
func ReadBit(data []byte, bitIndex int) bool {
	byteIdx := bitIndex / 8
	if bitIndex < 0 || byteIdx >= len(data) {
		return false
	}
	bitIdx := bitIndex % 8
	return data[byteIdx]&(1<<uint(bitIdx)) != 0
}

// WriteBit sets or clears a single bit in a byte slice.
// bitIndex 0 is the least significant bit of the first byte.
// Does nothing if bitIndex is out of range.
func WriteBit(data []byte, bitIndex int, value bool) {
	byteIdx := bitIndex / 8
	if bitIndex < 0 || byteIdx >= len(data) {
		return
	}
	bitIdx := bitIndex % 8
	if value {
		data[byteIdx] |= 1 << uint(bitIdx)
	} else {
		data[byteIdx] &^= 1 << uint(bitIdx)
	}
}
