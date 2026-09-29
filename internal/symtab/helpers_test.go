package symtab

import (
	"encoding/binary"
	"math"
	"reflect"
	"testing"
	"unicode/utf16"
)

// --- Assertion helpers ---

func requireNoError(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// --- Byte-encoding helpers for test data construction ---

func le16(v int16) []byte {
	b := make([]byte, 2)
	binary.LittleEndian.PutUint16(b, uint16(v))
	return b
}

func le32(v int32) []byte {
	b := make([]byte, 4)
	binary.LittleEndian.PutUint32(b, uint32(v))
	return b
}

func leu32(v uint32) []byte {
	b := make([]byte, 4)
	binary.LittleEndian.PutUint32(b, v)
	return b
}

func leF32(v float32) []byte {
	b := make([]byte, 4)
	binary.LittleEndian.PutUint32(b, math.Float32bits(v))
	return b
}

func leF64(v float64) []byte {
	b := make([]byte, 8)
	binary.LittleEndian.PutUint64(b, math.Float64bits(v))
	return b
}

// --- WSTRING / UTF-16LE helpers ---

func encodeUTF16LE(s string) []byte {
	encoded := utf16.Encode([]rune(s))
	buf := make([]byte, len(encoded)*2)
	for i, r := range encoded {
		binary.LittleEndian.PutUint16(buf[i*2:], r)
	}
	return buf
}

// --- symbol encode/decode round-trip helper ---

// testValueRoundTrip encodes value for a fresh symbol of dataType, decodes the
// bytes with another, and requires the identical Go value back.
func testValueRoundTrip(t *testing.T, dataType string, length uint32, value any) {
	t.Helper()
	sym := &Symbol{DataType: dataType, Length: length}
	data, err := sym.Encode(value, nil)
	if err != nil {
		t.Fatalf("encode(%s, %#v): %v", dataType, value, err)
	}
	got, err := (&Symbol{DataType: dataType, Length: length}).Decode(data, 0, nil)
	if err != nil {
		t.Fatalf("decode(%s): %v", dataType, err)
	}
	if !reflect.DeepEqual(got, value) {
		t.Errorf("round trip %s: wrote %#v (%T), got back %#v (%T)", dataType, value, value, got, got)
	}
}
