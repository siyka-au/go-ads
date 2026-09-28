package ads

import (
	"encoding/binary"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/civil"
)

func u16(v uint16) []byte { return binary.LittleEndian.AppendUint16(nil, v) }
func u32(v uint32) []byte { return binary.LittleEndian.AppendUint32(nil, v) }
func u64(v uint64) []byte { return binary.LittleEndian.AppendUint64(nil, v) }

type decodeCase struct {
	name, dataType string
	data           []byte
	want           any
}

// decodeCases pairs PLC bytes with the Go value they decode to. The encode
// tests run them in reverse.
var decodeCases = []decodeCase{
	{"BOOL/true", "BOOL", []byte{1}, true},
	{"BOOL/nonzero", "BOOL", []byte{0x80}, true},
	{"BOOL/false", "BOOL", []byte{0}, false},
	{"SINT/min", "SINT", []byte{0x80}, int8(math.MinInt8)},
	{"USINT/max", "USINT", []byte{0xFF}, uint8(math.MaxUint8)},
	{"BYTE", "BYTE", []byte{161}, uint8(161)},
	{"INT/wrapped", "INT", u16(34465), int16(-31071)},
	{"INT16", "INT16", u16(0xFFFF), int16(-1)},
	{"UINT/max", "UINT", u16(math.MaxUint16), uint16(math.MaxUint16)},
	{"WORD", "WORD", u16(34465), uint16(34465)},
	{"DINT/min", "DINT", u32(0x80000000), int32(math.MinInt32)},
	{"UDINT/max", "UDINT", u32(math.MaxUint32), uint32(math.MaxUint32)},
	{"DWORD", "DWORD", u32(100001), uint32(100001)},
	{"LINT/min", "LINT", u64(1 << 63), int64(math.MinInt64)},
	{"ULINT/max", "ULINT", u64(math.MaxUint64), uint64(math.MaxUint64)},
	{"LWORD", "LWORD", u64(1 << 60), uint64(1 << 60)},
	{"REAL", "REAL", u32(math.Float32bits(100001)), float32(100001)},
	{"LREAL", "LREAL", u64(math.Float64bits(-0.25)), -0.25},
	{"STRING/terminated", "STRING", []byte("S=7\x00junk"), "S=7"},
	{"STRING/full", "STRING", []byte("abc"), "abc"},
	{"WSTRING", "WSTRING", []byte{'h', 0, 0xAC, 0x20, 0, 0, 'x', 0}, "h€"},

	{"TIME/zero", "TIME", u32(0), time.Duration(0)},
	{"TIME/over_24h", "TIME", u32(90061001), 25*time.Hour + time.Minute + time.Second + time.Millisecond},
	{"TIME/max", "TIME", u32(math.MaxUint32), time.Duration(math.MaxUint32) * time.Millisecond},
	{"LTIME/ns", "LTIME", u64(100001), 100001 * time.Nanosecond},
	{"LTIME/max", "LTIME", u64(math.MaxInt64), time.Duration(math.MaxInt64)},

	{"TOD/midnight", "TOD", u32(0), civil.Time{}},
	{"TOD/ms", "TOD", u32(100001), civil.Time{Minute: 1, Second: 40, Nanosecond: 1_000_000}},
	{"TIME_OF_DAY/last_ms", "TIME_OF_DAY", u32(86_399_999), civil.Time{Hour: 23, Minute: 59, Second: 59, Nanosecond: 999_000_000}},
	{"LTOD/ns", "LTOD", u64(100001), civil.Time{Nanosecond: 100001}},
	{"LTIME_OF_DAY/last_ns", "LTIME_OF_DAY", u64(uint64(nsPerDay - 1)), civil.Time{Hour: 23, Minute: 59, Second: 59, Nanosecond: 999_999_999}},

	{"DATE/epoch", "DATE", u32(0), civil.Date{Year: 1970, Month: 1, Day: 1}},
	{"DATE/midnight", "DATE", u32(86400), civil.Date{Year: 1970, Month: 1, Day: 2}},
	// A raw write can leave a time of day in a DATE; it is dropped.
	{"DATE/time_dropped", "DATE", u32(100001), civil.Date{Year: 1970, Month: 1, Day: 2}},
	{"DATE/max", "DATE", u32(math.MaxUint32), civil.Date{Year: 2106, Month: 2, Day: 7}},
	{"LDATE/epoch", "LDATE", u64(0), civil.Date{Year: 1970, Month: 1, Day: 1}},
	{"LDATE/time_dropped", "LDATE", u64(uint64(nsPerDay + 1)), civil.Date{Year: 1970, Month: 1, Day: 2}},
	{"LDATE/pre_epoch", "LDATE", u64(uint64(time.Date(1900, 6, 15, 0, 0, 0, 0, time.UTC).UnixNano())), civil.Date{Year: 1900, Month: 6, Day: 15}},
	{"LDATE/min", "LDATE", u64(1 << 63), civil.Date{Year: 1677, Month: 9, Day: 21}},

	{"DT", "DT", u32(100001), civil.DateTime{Date: civil.Date{Year: 1970, Month: 1, Day: 2}, Time: civil.Time{Hour: 3, Minute: 46, Second: 41}}},
	{"DATE_AND_TIME/max", "DATE_AND_TIME", u32(math.MaxUint32), civil.DateTime{Date: civil.Date{Year: 2106, Month: 2, Day: 7}, Time: civil.Time{Hour: 6, Minute: 28, Second: 15}}},
	{"LDT/ns", "LDT", u64(100001), civil.DateTime{Date: civil.Date{Year: 1970, Month: 1, Day: 1}, Time: civil.Time{Nanosecond: 100001}}},
	{"LDATE_AND_TIME/max", "LDATE_AND_TIME", u64(math.MaxInt64), civil.DateTime{Date: civil.Date{Year: 2262, Month: 4, Day: 11}, Time: civil.Time{Hour: 23, Minute: 47, Second: 16, Nanosecond: 854_775_807}}},
	{"LDT/min", "LDT", u64(1 << 63), civil.DateTime{Date: civil.Date{Year: 1677, Month: 9, Day: 21}, Time: civil.Time{Minute: 12, Second: 43, Nanosecond: 145_224_192}}},
}

func TestDecodeScalar(t *testing.T) {
	for _, tt := range decodeCases {
		t.Run(tt.name, func(t *testing.T) {
			got, err := decodeScalar(tt.dataType, tt.data)
			if err != nil {
				t.Fatalf("decodeScalar: %v", err)
			}
			if got != tt.want {
				t.Errorf("got %#v (%T), want %#v (%T)", got, got, tt.want, tt.want)
			}
		})
	}
}

func TestDecodeScalarErrors(t *testing.T) {
	tests := []struct {
		name, dataType string
		data           []byte
		errContains    string
	}{
		{"INT/short", "INT", []byte{1}, "INT Size Wrong"},
		{"UDINT/label", "UDINT", []byte{1}, "DWORD Size Wrong"},
		{"LTIME/size", "LTIME", u32(1), "LTIME Size Wrong"},
		{"TOD/over_a_day", "TOD", u32(uint32(msPerDay)), "not within a day"},
		{"LTOD/over_a_day", "LTOD", u64(uint64(nsPerDay)), "not within a day"},
		{"LTIME/overflow", "LTIME", u64(math.MaxUint64), "exceeds time.Duration"},
		{"unknown", "POINTER TO INT", u64(0), "unknown format"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := decodeScalar(tt.dataType, tt.data)
			if err == nil || !strings.Contains(err.Error(), tt.errContains) {
				t.Errorf("err = %v, want it to contain %q", err, tt.errContains)
			}
		})
	}
}

// decode must leave the typed value in Value for aliases and enums too, resolved
// through the datatype table to the base type.
func TestDecodeStoresTypedValue(t *testing.T) {
	datatypes := map[string]SymbolUploadDataType{"E_Mode": {Name: "E_Mode", DataType: "INT"}}
	sym := &symbol{DataType: "E_Mode", Length: 2}
	if _, err := sym.decode(u16(3), 0, datatypes); err != nil {
		t.Fatal(err)
	}
	if sym.Value != int16(3) {
		t.Errorf("Value = %#v, want int16(3)", sym.Value)
	}
}

func TestDataTreeStruct(t *testing.T) {
	datatypes := map[string]SymbolUploadDataType{
		"ST_X": {
			Name: "ST_X", DataType: "ST_X", DatatypeEntry: datatypeEntry{Size: 12},
			Children: map[string]*SymbolUploadDataType{
				"bOn":   {Name: "bOn", DataType: "BOOL", DatatypeEntry: datatypeEntry{Offs: 0, Size: 1}},
				"tWait": {Name: "tWait", DataType: "TIME", DatatypeEntry: datatypeEntry{Offs: 4, Size: 4}},
				"dDay":  {Name: "dDay", DataType: "DATE", DatatypeEntry: datatypeEntry{Offs: 8, Size: 4}},
			},
		},
	}
	sym := addSymbol(symbolUploadSymbol{Name: "MAIN.st", DataType: "ST_X", SymbolEntry: symbolEntry{Size: 12}}, datatypes, nil)
	data := append(append([]byte{1, 0, 0, 0}, u32(90061001)...), u32(86400)...)
	if _, err := sym.decode(data, 0, datatypes); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"bOn":   true,
		"tWait": 25*time.Hour + time.Minute + time.Second + time.Millisecond,
		"dDay":  civil.Date{Year: 1970, Month: 1, Day: 2},
	}
	if !reflect.DeepEqual(sym.Value, want) {
		t.Errorf("Value = %#v, want %#v", sym.Value, want)
	}
}

func TestDataTree2DArray(t *testing.T) {
	const typeName = "ARRAY [0..2,0..2] OF INT"
	datatypes := map[string]SymbolUploadDataType{
		typeName: {
			Name: typeName, DataType: "INT", DatatypeEntry: datatypeEntry{Size: 18, ArrayDim: 2},
			Children: makeArrayChildren([]datatypeArrayInfo{{0, 3}, {0, 3}}, "INT", 18, nil),
		},
	}
	sym := addSymbol(symbolUploadSymbol{Name: "MAIN.a", DataType: typeName, SymbolEntry: symbolEntry{Size: 18}}, datatypes, nil)
	var data []byte
	for i := range 9 {
		data = append(data, u16(uint16(32766+i))...) // crosses 32767 → -32768
	}
	if _, err := sym.decode(data, 0, datatypes); err != nil {
		t.Fatal(err)
	}
	want := []any{
		[]any{int16(32766), int16(32767), int16(-32768)},
		[]any{int16(-32767), int16(-32766), int16(-32765)},
		[]any{int16(-32764), int16(-32763), int16(-32762)},
	}
	if !reflect.DeepEqual(sym.Value, want) {
		t.Errorf("Value = %#v, want %#v", sym.Value, want)
	}
}

// Every value TestDecodeScalar decodes must encode back to the same bytes.
func TestEncodeScalarRoundTrip(t *testing.T) {
	// Cases whose decode discards information, so the value cannot reproduce
	// the bytes: a non-1 true, text after a terminator or no terminator at all,
	// and a time of day in a DATE/LDATE (for LDATE/min, truncating to midnight
	// also lands before the earliest LDATE).
	lossy := map[string]bool{
		"BOOL/nonzero": true, "STRING/terminated": true, "STRING/full": true, "WSTRING": true,
		"DATE/time_dropped": true, "DATE/max": true, "LDATE/time_dropped": true, "LDATE/min": true,
	}
	for _, tt := range decodeCases {
		t.Run(tt.name, func(t *testing.T) {
			if lossy[tt.name] {
				t.Skip("decoding is lossy here by design")
			}
			got, err := encodeScalar(tt.dataType, tt.want, uint32(len(tt.data)))
			if err != nil {
				t.Fatalf("encodeScalar(%#v): %v", tt.want, err)
			}
			if string(got) != string(tt.data) {
				t.Errorf("encodeScalar(%#v) = % x, want % x", tt.want, got, tt.data)
			}
		})
	}
}

func TestEncodeScalarAcceptsAnyIntegerInRange(t *testing.T) {
	for _, v := range []any{int(-5), int8(-5), int16(-5), int32(-5), int64(-5)} {
		b, err := encodeScalar("INT", v, 2)
		if err != nil || !reflect.DeepEqual(b, u16(0xFFFB)) {
			t.Errorf("INT from %T: % x, %v", v, b, err)
		}
	}
	for _, v := range []any{uint(7), uint8(7), uint16(7), uint32(7), uint64(7), int(7)} {
		b, err := encodeScalar("UDINT", v, 4)
		if err != nil || !reflect.DeepEqual(b, u32(7)) {
			t.Errorf("UDINT from %T: % x, %v", v, b, err)
		}
	}
}

func TestEncodeScalarErrors(t *testing.T) {
	tests := []struct {
		name, dataType string
		v              any
		length         uint32
		errContains    string
	}{
		{"SINT/overflow", "SINT", 128, 1, "outside"},
		{"USINT/negative", "USINT", -1, 1, "outside"},
		{"UINT/overflow", "UINT", uint32(70000), 2, "outside"},
		{"LINT/from_huge_uint", "LINT", uint64(math.MaxUint64), 8, "outside"},
		{"INT/float", "INT", 1.5, 2, "takes an integer"},
		{"REAL/float64", "REAL", 1.5, 4, "takes float32"},
		{"BOOL/int", "BOOL", 1, 1, "takes bool"},
		{"STRING/too_long", "STRING", "abcd", 4, "does not fit"},
		{"TIME/sub_ms", "TIME", time.Microsecond, 4, "whole milliseconds"},
		{"TIME/negative", "TIME", -time.Second, 4, "whole milliseconds from 0"},
		{"TIME/string", "TIME", "00:00:01", 4, "takes time.Duration"},
		{"TOD/invalid", "TOD", civil.Time{Hour: 24}, 4, "not a valid time"},
		{"TOD/sub_ms", "TOD", civil.Time{Nanosecond: 1}, 4, "whole milliseconds"},
		{"DATE/pre_epoch", "DATE", civil.Date{Year: 1969, Month: 12, Day: 31}, 4, "outside 1970"},
		{"DATE/invalid", "DATE", civil.Date{Year: 2024, Month: 2, Day: 30}, 4, "not a valid date"},
		{"DT/fraction", "DT", civil.DateTime{Date: civil.Date{Year: 2000, Month: 1, Day: 1}, Time: civil.Time{Nanosecond: 5}}, 4, "whole seconds"},
		{"LDT/past_2262", "LDT", civil.DateTime{Date: civil.Date{Year: 2263, Month: 1, Day: 1}}, 8, "outside 1677"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := encodeScalar(tt.dataType, tt.v, tt.length)
			if err == nil || !strings.Contains(err.Error(), tt.errContains) {
				t.Errorf("err = %v, want it to contain %q", err, tt.errContains)
			}
		})
	}
}

func testStructSymbol(t *testing.T) (*symbol, map[string]SymbolUploadDataType) {
	t.Helper()
	datatypes := map[string]SymbolUploadDataType{
		"ST_X": {
			Name: "ST_X", DataType: "ST_X", DatatypeEntry: datatypeEntry{Size: 12},
			Children: map[string]*SymbolUploadDataType{
				"bOn":   {Name: "bOn", DataType: "BOOL", DatatypeEntry: datatypeEntry{Offs: 0, Size: 1}},
				"tWait": {Name: "tWait", DataType: "TIME", DatatypeEntry: datatypeEntry{Offs: 4, Size: 4}},
				"dDay":  {Name: "dDay", DataType: "DATE", DatatypeEntry: datatypeEntry{Offs: 8, Size: 4}},
			},
		},
	}
	return addSymbol(symbolUploadSymbol{Name: "MAIN.st", DataType: "ST_X", SymbolEntry: symbolEntry{Size: 12}}, datatypes, nil), datatypes
}

func TestEncodeStructRoundTrip(t *testing.T) {
	sym, datatypes := testStructSymbol(t)
	want := map[string]any{"bOn": true, "tWait": 90 * time.Minute, "dDay": civil.Date{Year: 2026, Month: 9, Day: 28}}
	b, err := sym.encode(want, datatypes)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sym.decode(b, 0, datatypes); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(sym.Value, want) {
		t.Errorf("round trip = %#v, want %#v", sym.Value, want)
	}
}

func TestEncodeStructRejectsIncompleteOrUnknown(t *testing.T) {
	sym, datatypes := testStructSymbol(t)
	for name, v := range map[string]any{
		"missing member": map[string]any{"bOn": true, "tWait": time.Second},
		"unknown member": map[string]any{"bOn": true, "tWait": time.Second, "dDay": civil.Date{Year: 2000, Month: 1, Day: 1}, "extra": 1},
		"not a map":      []any{true},
		"member type":    map[string]any{"bOn": 1, "tWait": time.Second, "dDay": civil.Date{Year: 2000, Month: 1, Day: 1}},
	} {
		if _, err := sym.encode(v, datatypes); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestEncode2DArrayRoundTrip(t *testing.T) {
	const typeName = "ARRAY [0..1,0..2] OF DINT"
	datatypes := map[string]SymbolUploadDataType{
		typeName: {
			Name: typeName, DataType: "DINT", DatatypeEntry: datatypeEntry{Size: 24, ArrayDim: 2},
			Children: makeArrayChildren([]datatypeArrayInfo{{0, 2}, {0, 3}}, "DINT", 24, nil),
		},
	}
	sym := addSymbol(symbolUploadSymbol{Name: "MAIN.a", DataType: typeName, SymbolEntry: symbolEntry{Size: 24}}, datatypes, nil)
	want := []any{[]any{int32(1), int32(-2), int32(3)}, []any{int32(math.MinInt32), int32(0), int32(math.MaxInt32)}}
	b, err := sym.encode(want, datatypes)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sym.decode(b, 0, datatypes); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(sym.Value, want) {
		t.Errorf("round trip = %#v, want %#v", sym.Value, want)
	}
	for name, v := range map[string]any{
		"short row":   []any{[]any{int32(1)}, []any{int32(1), int32(2), int32(3)}},
		"extra row":   []any{want[0], want[1], want[1]},
		"not a slice": map[string]any{},
	} {
		if _, err := sym.encode(v, datatypes); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}
