package ads

import (
	"encoding/binary"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf16"

	"cloud.google.com/go/civil"
)

// decodeOK decodes data as a fresh symbol and fails the test on error.
func decodeOK(t *testing.T, sym *symbol, data []byte, datatypes map[string]SymbolUploadDataType) any {
	t.Helper()
	v, err := sym.decode(data, 0, datatypes)
	if err != nil {
		t.Fatalf("decode %s: %v", sym.DataType, err)
	}
	return v
}

// F-16: int(symbol.Length) wraps on 32-bit Go. Validate symbol.Length against
// data buffer size before arithmetic to prevent negative slice / huge alloc.
// Validates: R-PARSE-006.
func TestDecode_RejectsOversizedSymbolLength(t *testing.T) {
	sym := &symbol{Name: "x", DataType: "INT", Length: 0xFFFFFFFF}
	_, err := sym.decode(make([]byte, 16), 0, nil)
	if err == nil || !strings.Contains(err.Error(), "Length") {
		t.Fatalf("expected an error mentioning Length, got %v", err)
	}
}

// A change decoded straight after the previous sample must be stored and
// returned. The store used to skip values arriving within 50 ms of the last
// one, so a notification carried the value it was replacing.
func TestDecode_BackToBackChangesAreNotDropped(t *testing.T) {
	sym := &symbol{
		Name:     "x",
		DataType: "UDINT",
		Length:   4,
	}
	for _, want := range []uint32{1, 100002, 7} {
		got := decodeOK(t, sym, leu32(want), nil)
		if got != want {
			t.Errorf("decode %d returned %#v", want, got)
		}
		if sym.Value != want {
			t.Errorf("decode %d: cached Value %#v", want, sym.Value)
		}
	}
}

// ============================================================
// Type resolution: alias, ADST_ code, size inference
// ============================================================

// Validates: NO-SPEC.
func TestDecodeUnknownType(t *testing.T) {
	// Size 3 matches no standard width, so inferBaseType cannot resolve it.
	for _, dt := range []map[string]SymbolUploadDataType{nil, {}} {
		if _, err := (&symbol{DataType: "UNKNOWN_TYPE", Length: 3}).decode([]byte{0, 0, 0}, 0, dt); err == nil {
			t.Error("expected error for unknown data type")
		}
		if _, err := (&symbol{DataType: "UNKNOWN_TYPE", Length: 3}).encode(int8(1), dt); err == nil {
			t.Error("expected error writing an unknown data type")
		}
	}
}

// An array carries its element's ADST_ code and the whole array's Length. With
// no datatype table it has no Children, so decode falls through to BaseType
// resolution -- which used to read 40 bytes as one DINT and report "DINT Size
// Wrong", naming a type the caller never asked for.
func TestDecodeArrayWithoutDatatypeTable(t *testing.T) {
	sym := &symbol{
		Name:     "anCounters",
		DataType: "ARRAY [0..9] OF DINT",
		BaseType: ADSTInt32, // element type, not the aggregate
		Length:   40,        // 10 * 4
	}
	_, err := sym.decode(make([]byte, 40), 0, nil)
	if err == nil {
		t.Fatal("expected an error for an array with no datatype table")
	}
	if strings.Contains(err.Error(), "DINT Size Wrong") {
		t.Errorf("error still blames the element type: %v", err)
	}
	for _, want := range []string{"ARRAY [0..9] OF DINT", "LoadSymbols()"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q, got: %v", want, err)
		}
	}
}

// The width guard must not catch a plain scalar resolved through BaseType: a
// type-aliased DINT has Length 4 and decodes as a DINT.
func TestDecodeAliasScalarStillResolves(t *testing.T) {
	sym := &symbol{Name: "aliased", DataType: "E_SomeAlias", BaseType: ADSTInt32, Length: 4}
	if got := decodeOK(t, sym, le32(7), nil); got != int32(7) {
		t.Errorf("got %#v, want int32(7)", got)
	}
}

// Validates: NO-SPEC.
func TestAliasResolutionThroughDatatypeTable(t *testing.T) {
	datatypes := map[string]SymbolUploadDataType{"MyAlias": {DataType: "INT"}}
	sym := &symbol{DataType: "MyAlias", Length: 2}
	if got := decodeOK(t, sym, le16(42), datatypes); got != int16(42) {
		t.Errorf("decode: got %#v, want int16(42)", got)
	}
	b, err := sym.encode(int16(123), datatypes)
	if err != nil || !reflect.DeepEqual(b, le16(123)) {
		t.Errorf("encode: % x, %v", b, err)
	}
}

// Validates: R-SYM-004.
func TestDecodeWithBaseType(t *testing.T) {
	tests := []struct {
		name     string
		baseType ADSDataType
		length   uint32
		data     []byte
		want     any
	}{
		// Unknown DataType with an ADST_ code decodes as that primitive.
		{"REAL", ADSTReal32, 4, leF32(3.14), float32(3.14)},
		// An unsigned code must decode unsigned, not signed.
		{"UDINT", ADSTUint32, 4, leu32(3000000000), uint32(3000000000)},
		// No ADST_ code: a 2-byte symbol falls through to inference as INT.
		{"inferred INT", ADSTVoid, 2, le16(1337), int16(1337)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sym := &symbol{Name: "a", FullName: "GVL.a", DataType: "SomeAlias", Length: tt.length, BaseType: tt.baseType}
			if got := decodeOK(t, sym, tt.data, nil); got != tt.want {
				t.Errorf("got %#v (%T), want %#v (%T)", got, got, tt.want, tt.want)
			}
		})
	}
}

// F-18: the write path resolves types exactly as the read path does, including
// the 1-/2-byte inference fallback for a user type with no datatype table.
// Validates: NO-SPEC.
func TestEncodeFallsBackToInferredType(t *testing.T) {
	sym := &symbol{Name: "x", DataType: "MyEnum16", Length: 2}
	got, err := sym.encode(42, nil)
	if err != nil {
		t.Fatalf("expected inference fallback to succeed for a 2-byte type: %v", err)
	}
	if !reflect.DeepEqual(got, []byte{42, 0}) {
		t.Errorf("got % x, want 2a 00", got)
	}
}

// 4- and 8-byte writes must NOT silently infer DINT/LINT — would corrupt
// REAL/LREAL aliases. Caller must call LoadSymbols or use a known type.
// Validates: NO-SPEC (regression guard for REAL/LREAL ambiguity fix).
func TestEncodeRefuses4And8ByteInferenceWithoutDatatypes(t *testing.T) {
	for _, size := range []uint32{4, 8} {
		sym := &symbol{Name: "x", DataType: "MyUnknownType", Length: size}
		if _, err := sym.encode(42, nil); err == nil {
			t.Errorf("size=%d: expected error refusing inference (REAL/LREAL ambiguity), got nil", size)
		}
	}
}

// ============================================================
// Decode error paths
// ============================================================

// Validates: R-PARSE-006.
func TestDecodeDataTooShort(t *testing.T) {
	tests := []struct {
		dataType string
		length   uint32
		data     []byte
	}{
		{"BOOL", 1, []byte{}},
		{"INT", 2, []byte{0}},
		{"UINT", 2, []byte{0}},
		{"DINT", 4, []byte{0, 0}},
		{"UDINT", 4, []byte{0, 0}},
		{"REAL", 4, []byte{0, 0}},
		{"LREAL", 8, []byte{0, 0, 0, 0}},
		{"LINT", 8, []byte{0, 0, 0, 0}},
		{"ULINT", 8, []byte{0, 0, 0, 0}},
		{"TIME", 4, []byte{0, 0}},
		{"TOD", 4, []byte{0, 0}},
		{"DATE", 4, []byte{0, 0}},
		{"DT", 4, []byte{0, 0}},
		{"LTIME", 8, []byte{0, 0, 0, 0}},
		{"LDT", 8, []byte{0, 0, 0, 0}},
	}
	for _, tt := range tests {
		t.Run(tt.dataType, func(t *testing.T) {
			if _, err := (&symbol{DataType: tt.dataType, Length: tt.length}).decode(tt.data, 0, nil); err == nil {
				t.Errorf("expected error for %s with %d bytes", tt.dataType, len(tt.data))
			}
		})
	}
}

// Validates: R-SYM-003 (partial).
func TestDecodeSizeWrong(t *testing.T) {
	// BOOL declared 2 bytes wide cannot decode.
	if _, err := (&symbol{DataType: "BOOL", Length: 2}).decode([]byte{1, 0}, 0, nil); err == nil {
		t.Error("expected error for BOOL with wrong size")
	}
}

// ============================================================
// Round trips through encode and decode
// ============================================================

// Validates: R-PARSE-007 (write/read symmetry).
func TestValueRoundTrip(t *testing.T) {
	tests := []struct {
		name     string
		dataType string
		length   uint32
		value    any
	}{
		{"BOOL/true", "BOOL", 1, true},
		{"BOOL/false", "BOOL", 1, false},
		{"BYTE/0", "BYTE", 1, uint8(0)},
		{"BYTE/255", "BYTE", 1, uint8(255)},
		{"USINT/42", "USINT", 1, uint8(42)},
		{"SINT/min", "SINT", 1, int8(math.MinInt8)},
		{"SINT/max", "SINT", 1, int8(math.MaxInt8)},
		{"UINT/max", "UINT", 2, uint16(math.MaxUint16)},
		{"WORD/1234", "WORD", 2, uint16(1234)},
		{"UINT16/5678", "UINT16", 2, uint16(5678)},
		{"INT/min", "INT", 2, int16(math.MinInt16)},
		{"INT16/-42", "INT16", 2, int16(-42)},
		{"UDINT/max", "UDINT", 4, uint32(math.MaxUint32)},
		{"DWORD/12345678", "DWORD", 4, uint32(12345678)},
		{"DINT/min", "DINT", 4, int32(math.MinInt32)},
		{"LINT/min", "LINT", 8, int64(math.MinInt64)},
		{"LINT/max", "LINT", 8, int64(math.MaxInt64)},
		{"ULINT/max", "ULINT", 8, uint64(math.MaxUint64)},
		{"LWORD/42", "LWORD", 8, uint64(42)},
		{"REAL/3.14", "REAL", 4, float32(3.14)},
		{"LREAL/pi", "LREAL", 8, math.Pi},
		{"LREAL/subnormal", "LREAL", 8, math.Float64frombits(1)},
		{"REAL/+Inf", "REAL", 4, float32(math.Inf(1))},
		{"LREAL/-Inf", "LREAL", 8, math.Inf(-1)},
		{"STRING/Hello", "STRING", 20, "Hello"},
		{"STRING/special", "STRING", 20, "a/b\\c\t\n"},
		{"STRING/exact", "STRING", 6, "Hello"}, // STRING(5): 5 chars + terminator
		{"STRING/empty", "STRING", 10, ""},
		{"TIME/zero", "TIME", 4, time.Duration(0)},
		{"TIME/with_ms", "TIME", 4, time.Hour + 2*time.Minute + 3*time.Second + 456*time.Millisecond},
		{"TIME/over_24h", "TIME", 4, 25*time.Hour + time.Minute + time.Second + time.Millisecond},
		{"LTIME/ns", "LTIME", 8, 1000*time.Hour + time.Nanosecond},
		{"TOD/midnight", "TOD", 4, civil.Time{}},
		{"TIME_OF_DAY/14:30", "TIME_OF_DAY", 4, civil.Time{Hour: 14, Minute: 30}},
		{"TOD/last_ms", "TOD", 4, civil.Time{Hour: 23, Minute: 59, Second: 59, Nanosecond: 999_000_000}},
		{"LTOD/last_ns", "LTOD", 8, civil.Time{Hour: 23, Minute: 59, Second: 59, Nanosecond: 999_999_999}},
		{"DATE/leap_year", "DATE", 4, civil.Date{Year: 2024, Month: 2, Day: 29}},
		{"DATE/epoch", "DATE", 4, civil.Date{Year: 1970, Month: 1, Day: 1}},
		{"LDATE/pre_epoch", "LDATE", 8, civil.Date{Year: 1900, Month: 6, Day: 15}},
		{"DT/epoch", "DT", 4, civil.DateTime{Date: civil.Date{Year: 1970, Month: 1, Day: 1}}},
		{"DT/Y2K38", "DT", 4, civil.DateTime{Date: civil.Date{Year: 2038, Month: 1, Day: 19}, Time: civil.Time{Hour: 3, Minute: 14, Second: 7}}},
		{"DATE_AND_TIME/full", "DATE_AND_TIME", 4, civil.DateTime{Date: civil.Date{Year: 2024, Month: 6, Day: 15}, Time: civil.Time{Hour: 23, Minute: 59, Second: 59}}},
		{"LDT/max", "LDT", 8, civil.DateTime{Date: civil.Date{Year: 2262, Month: 4, Day: 11}, Time: civil.Time{Hour: 23, Minute: 47, Second: 16, Nanosecond: 854_775_807}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			testValueRoundTrip(t, tt.dataType, tt.length, tt.value)
		})
	}
}

// NaN compares unequal to itself, so it gets its own round trip.
// Validates: R-PARSE-007.
func TestValueRoundTripNaN(t *testing.T) {
	for _, tt := range []struct {
		dataType string
		length   uint32
		value    any
	}{
		{"REAL", 4, float32(math.NaN())},
		{"LREAL", 8, math.NaN()},
	} {
		data, err := (&symbol{DataType: tt.dataType, Length: tt.length}).encode(tt.value, nil)
		requireNoError(t, err)
		got := decodeOK(t, &symbol{DataType: tt.dataType, Length: tt.length}, data, nil)
		switch v := got.(type) {
		case float32:
			if !math.IsNaN(float64(v)) {
				t.Errorf("REAL: got %v, want NaN", v)
			}
		case float64:
			if !math.IsNaN(v) {
				t.Errorf("LREAL: got %v, want NaN", v)
			}
		default:
			t.Errorf("%s decoded to %T", tt.dataType, got)
		}
	}
}

// -0 keeps its sign bit through the round trip.
func TestValueRoundTripNegativeZero(t *testing.T) {
	data, err := (&symbol{DataType: "LREAL", Length: 8}).encode(math.Copysign(0, -1), nil)
	requireNoError(t, err)
	got := decodeOK(t, &symbol{DataType: "LREAL", Length: 8}, data, nil).(float64)
	if got != 0 || !math.Signbit(got) {
		t.Errorf("got %v (signbit %v), want -0", got, math.Signbit(got))
	}
}

// ============================================================
// Encode error paths
// ============================================================

// Validates: NO-SPEC.
func TestEncodeInvalidValues(t *testing.T) {
	tests := []struct {
		name     string
		dataType string
		length   uint32
		value    any
	}{
		{"BOOL/string", "BOOL", 1, "notabool"},
		{"BYTE/negative", "BYTE", 1, -1},
		{"BYTE/overflow", "BYTE", 1, 256},
		{"INT/overflow", "INT", 2, 40000},
		{"UINT/negative", "UINT", 2, -1},
		{"DINT/overflow", "DINT", 4, int64(3000000000)},
		{"REAL/string", "REAL", 4, "notafloat"},
		{"LREAL/float32", "LREAL", 8, float32(1)},
		{"LINT/string", "LINT", 8, "abc"},
		{"ULINT/negative", "ULINT", 8, -1},
		{"TIME/string", "TIME", 4, "not-a-time"},
		{"TOD/duration", "TOD", 4, time.Second},
		{"DATE/time", "DATE", 4, time.Now()},
		{"DT/date", "DT", 4, civil.Date{Year: 2000, Month: 1, Day: 1}},
		// A zero-length STRING has no room for its terminator (F-17).
		{"STRING/zero_length", "STRING", 0, "hello"},
		// A WSTRING needs 2 bytes for its terminator (F-17).
		{"WSTRING/length_0", "WSTRING", 0, "hi"},
		{"WSTRING/length_1", "WSTRING", 1, "hi"},
		// Too long for the declared size is refused, not truncated.
		{"STRING/overflow", "STRING", 4, "Hello"},
		{"WSTRING/overflow", "WSTRING", 6, "ABCDE"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := (&symbol{DataType: tt.dataType, Length: tt.length}).encode(tt.value, nil); err == nil {
				t.Errorf("expected error for %s with %#v", tt.dataType, tt.value)
			}
		})
	}
}

// ============================================================
// STRING and WSTRING
// ============================================================

// Validates: R-PARSE-005.
func TestDecodeSTRING(t *testing.T) {
	tests := []struct {
		name   string
		length uint32
		data   []byte
		want   string
	}{
		// Buffer full with no terminator: the whole buffer.
		{"no_terminator", 5, []byte("Hello"), "Hello"},
		{"empty", 10, make([]byte, 10), ""},
		// Text after the terminator is not part of the value.
		{"trailing_garbage", 10, []byte("Hi\x00GARBAGE"), "Hi"},
		{"padded", 20, append([]byte("Hello\x00"), make([]byte, 14)...), "Hello"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := decodeOK(t, &symbol{DataType: "STRING", Length: tt.length}, tt.data, nil); got != tt.want {
				t.Errorf("got %#v, want %q", got, tt.want)
			}
		})
	}
}

// Validates: R-PARSE-003.
func TestEncodeSTRING_PadsWithZeros(t *testing.T) {
	data, err := (&symbol{DataType: "STRING", Length: 10}).encode("Hi", nil)
	requireNoError(t, err)
	if want := append([]byte("Hi"), make([]byte, 8)...); !reflect.DeepEqual(data, want) {
		t.Errorf("got % x, want % x", data, want)
	}
}

// Validates: R-PARSE-007 (WSTRING).
func TestDecodeWSTRING(t *testing.T) {
	pad := func(b []byte, n int) []byte { return append(b, make([]byte, n-len(b))...) }
	garbage := pad(encodeUTF16LE("Hi"), 20)
	garbage[6], garbage[7] = 0xFF, 0xFF // after the terminator at bytes 4-5
	tests := []struct {
		name   string
		length uint32
		data   []byte
		want   string
	}{
		{"ascii", 20, pad(encodeUTF16LE("Hello"), 20), "Hello"},
		{"unicode", 20, pad(encodeUTF16LE("日本語"), 20), "日本語"},
		{"after_terminator", 20, garbage, "Hi"},
		{"no_terminator", 10, encodeUTF16LE("ABCDE"), "ABCDE"},
		{"empty", 10, make([]byte, 10), ""},
		{"surrogate_pair", 10, pad(encodeUTF16LE("😀"), 10), "😀"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := decodeOK(t, &symbol{DataType: "WSTRING", Length: tt.length}, tt.data, nil); got != tt.want {
				t.Errorf("got %#v, want %q", got, tt.want)
			}
		})
	}
}

// Validates: R-PARSE-004.
func TestEncodeWSTRING(t *testing.T) {
	for _, text := range []string{"Hello", "日本語", "😀", "a", ""} {
		t.Run(text, func(t *testing.T) {
			data, err := (&symbol{DataType: "WSTRING", Length: 40}).encode(text, nil)
			requireNoError(t, err)
			want := append(encodeUTF16LE(text), make([]byte, 40-2*len(utf16.Encode([]rune(text))))...)
			if !reflect.DeepEqual(data, want) {
				t.Errorf("got % x, want % x", data, want)
			}
			testValueRoundTrip(t, "WSTRING", 40, text)
		})
	}
}

// A WSTRING whose surrogate pair would be split by the declared size is
// refused as too long; no unpaired surrogate can reach the PLC.
// Validates: R-PARSE-004.
func TestEncodeWSTRINGSurrogatePairDoesNotFit(t *testing.T) {
	value := "A\U0001D400" // 'A' plus a surrogate pair: 3 UTF-16 units
	if n := len(utf16.Encode([]rune(value))); n != 3 {
		t.Fatalf("expected 3 UTF-16 code units, got %d", n)
	}
	// Length 6 holds 2 units and the terminator: the pair would be split.
	if _, err := (&symbol{DataType: "WSTRING", Length: 6}).encode(value, nil); err == nil {
		t.Fatal("expected an error, got nil")
	}
	testValueRoundTrip(t, "WSTRING", 8, value)
}

// ============================================================
// Bit symbols
// ============================================================

// Bit symbols in TwinCAT have DataType="BOOL" — the BitValue flag affects
// addressing (offset encodes bit position) but NOT decoding. Handle-based
// reads return data matching the declared DataType, and the flag must not
// interfere with other types (TC2 sets it on UDINT, LREAL, etc.).

// Validates: R-PARSE-007.
func TestBitValueFlagDoesNotAffectDecoding(t *testing.T) {
	tests := []struct {
		dataType string
		length   uint32
		data     []byte
		want     any
	}{
		{"BOOL", 1, []byte{0x01}, true},
		{"BOOL", 1, []byte{0x00}, false},
		{"UDINT", 4, leu32(12345), uint32(12345)},
		{"LREAL", 8, leF64(3.14), 3.14},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("%s/%v", tt.dataType, tt.want), func(t *testing.T) {
			sym := &symbol{DataType: tt.dataType, Length: tt.length, Flags: SymbolFlagBitValue}
			if got := decodeOK(t, sym, tt.data, nil); got != tt.want {
				t.Errorf("got %#v, want %#v", got, tt.want)
			}
		})
	}
}

// Validates: R-PARSE-007.
func TestEncodeBitSymbol(t *testing.T) {
	for v, want := range map[bool]byte{true: 0x01, false: 0x00} {
		data, err := (&symbol{DataType: "BOOL", Length: 1, Flags: SymbolFlagBitValue}).encode(v, nil)
		if err != nil || len(data) != 1 || data[0] != want {
			t.Errorf("%v: got % x, %v; want %02x", v, data, err, want)
		}
	}
}

// Validates: NO-SPEC.
func TestReadBit_AllPositions(t *testing.T) {
	// 0xA5 = 10100101
	data := []byte{0xA5}
	expected := []bool{true, false, true, false, false, true, false, true}
	for i, want := range expected {
		if got := ReadBit(data, i); got != want {
			t.Errorf("bit %d: got %v, want %v", i, got, want)
		}
	}
	if ReadBit(data, 8) || ReadBit(data, -1) {
		t.Error("out-of-range bits must read false")
	}
}

// Validates: NO-SPEC.
func TestWriteBit(t *testing.T) {
	data := []byte{0x00}
	WriteBit(data, 3, true)
	if data[0] != 0x08 {
		t.Errorf("set: got 0x%02X, want 0x08", data[0])
	}
	data = []byte{0xFF}
	WriteBit(data, 3, false)
	if data[0] != 0xF7 {
		t.Errorf("clear: got 0x%02X, want 0xF7", data[0])
	}
	// Setting a set bit and clearing a clear one change nothing.
	data = []byte{0xA5}
	WriteBit(data, 2, true)
	WriteBit(data, 1, false)
	if data[0] != 0xA5 {
		t.Errorf("got 0x%02X, want 0xA5 (unchanged)", data[0])
	}
}

// ============================================================
// Structs
// ============================================================

// Validates: R-PARSE-001.
func TestDecodeNestedStructThreeLevels(t *testing.T) {
	// ST_Inner { value: INT }
	// ST_Middle { inner: ST_Inner, count: BYTE }
	// ST_Outer { middle: ST_Middle, flag: BOOL }
	innerChild := &symbol{Name: "value", FullName: "o.middle.inner.value", DataType: "INT", Length: 2, Offset: 0}
	inner := &symbol{
		Name: "inner", FullName: "o.middle.inner", DataType: "ST_Inner", Length: 2, Offset: 0,
		Children: map[string]*symbol{"value": innerChild},
	}
	innerChild.Parent = inner

	countChild := &symbol{Name: "count", FullName: "o.middle.count", DataType: "BYTE", Length: 1, Offset: 2}
	middle := &symbol{
		Name: "middle", FullName: "o.middle", DataType: "ST_Middle", Length: 4, Offset: 0,
		Children: map[string]*symbol{"inner": inner, "count": countChild},
	}
	inner.Parent = middle
	countChild.Parent = middle

	flagChild := &symbol{Name: "flag", FullName: "o.flag", DataType: "BOOL", Length: 1, Offset: 4}
	outer := &symbol{
		Name: "o", FullName: "o", DataType: "ST_Outer", Length: 5,
		Children: map[string]*symbol{"middle": middle, "flag": flagChild},
	}
	middle.Parent = outer
	flagChild.Parent = outer

	// Data: INT=1234, BYTE=42, padding(1), BOOL=1
	data := make([]byte, 5)
	binary.LittleEndian.PutUint16(data[0:2], 1234)
	data[2] = 42
	data[4] = 1

	got := decodeOK(t, outer, data, nil)
	want := map[string]any{
		"middle": map[string]any{
			"inner": map[string]any{"value": int16(1234)},
			"count": uint8(42),
		},
		"flag": true,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %#v, want %#v", got, want)
	}
	// Each member also holds its own value in the cache.
	if innerChild.Value != int16(1234) || countChild.Value != uint8(42) || flagChild.Value != true {
		t.Errorf("members cached %#v, %#v, %#v", innerChild.Value, countChild.Value, flagChild.Value)
	}

	// And the nested value encodes back to the same bytes.
	back, err := outer.encode(want, nil)
	requireNoError(t, err)
	if !reflect.DeepEqual(back, data) {
		t.Errorf("encode: got % x, want % x", back, data)
	}
}

// Padding between members is written as zeros.
// Validates: R-PARSE-001.
func TestEncodeStructWithPadding(t *testing.T) {
	parent := &symbol{
		Name: "test", FullName: "test", DataType: "ST_Test", Length: 8,
		Children: map[string]*symbol{
			"field1": {Name: "field1", FullName: "test.field1", DataType: "INT", Length: 2, Offset: 0},
			"field2": {Name: "field2", FullName: "test.field2", DataType: "DINT", Length: 4, Offset: 4},
		},
	}
	data, err := parent.encode(map[string]any{"field1": int16(42), "field2": int32(100)}, nil)
	requireNoError(t, err)
	if want := []byte{42, 0, 0, 0, 100, 0, 0, 0}; !reflect.DeepEqual(data, want) {
		t.Errorf("got % x, want % x", data, want)
	}
}

// A struct write must name every member: the string path used to zero the
// ones a JSON object left out, silently changing PLC state.
// Validates: R-PARSE-001.
func TestEncodeStructRequiresEveryMember(t *testing.T) {
	parent := &symbol{
		Name: "s", FullName: "s", DataType: "ST_S", Length: 2,
		Children: map[string]*symbol{
			"x": {Name: "x", FullName: "s.x", DataType: "BYTE", Length: 1, Offset: 0},
			"y": {Name: "y", FullName: "s.y", DataType: "BYTE", Length: 1, Offset: 1},
		},
	}
	for name, v := range map[string]any{
		"missing member": map[string]any{"x": uint8(7)},
		"not a map":      "not json",
		"member type":    map[string]any{"x": uint8(7), "y": "7"},
	} {
		if _, err := parent.encode(v, nil); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

// F-xx: a member whose Offset + Length passes the end must error, not panic --
// including when the sum overflows uint32.
// Validates: R-WRITE-OVERFLOW-001.
func TestEncodeChildOffsetOverflow(t *testing.T) {
	parent := &symbol{
		Name: "Parent", FullName: "Parent", DataType: "SomeStruct", Length: 10,
		Children: map[string]*symbol{
			"Field": {Name: "Field", FullName: "Parent.Field", DataType: "DINT", Offset: math.MaxUint32 - 1, Length: 4},
		},
	}
	if _, err := parent.encode(map[string]any{"Field": int32(42)}, nil); err == nil {
		t.Fatal("expected error for child Offset+Length overflow, got nil")
	}
}
