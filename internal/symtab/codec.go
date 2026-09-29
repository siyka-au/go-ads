package symtab

import (
	"fmt"
	"time"

	"github.com/siyka-au/go-ads/v3/ams"
)

// decode decodes the symbol's value from data at offset into s.Value (see
// value.go for the Go types) and returns it. A struct or array decodes each
// child in place and assembles their values.
func (s *Symbol) Decode(data []byte, offset int, datatypes map[string]TypeInfo) (any, error) {
	start := offset
	// reject oversized s.Length before arithmetic. uint32 → int
	// conversion wraps on 32-bit Go; an attacker-controlled or buggy symbol
	// entry with Length = 0xFFFFFFFF produces a negative int that bypasses
	// the bounds guard and panics on slice. Compare in uint64 space to dodge
	// the wrap entirely.
	if uint64(s.Length) > uint64(len(data)) {
		return nil, fmt.Errorf("decode %s: s.Length %d exceeds data buffer size %d", s.DataType, s.Length, len(data))
	}
	stop := start + int(s.Length)
	if start+int(s.Length) > len(data) {
		stop = len(data)
	}

	if len(s.Children) > 0 {
		for _, child := range s.Children {
			var err error
			if child.BitMember {
				err = child.decodeBit(data[offset:stop])
			} else {
				_, err = child.Decode(data[offset:stop], int(child.Offset), datatypes)
			}
			if err != nil {
				return nil, fmt.Errorf("decoding child %q: %w", child.Name, err)
			}
		}
		s.store(s.valueTree())
		return s.Value, nil
	}

	if start+int(s.Length) > len(data) {
		return nil, fmt.Errorf("data too short for %s at offset %d: need %d bytes, got %d", s.DataType, start, s.Length, len(data)-start)
	}

	dt, err := s.scalarType(datatypes)
	if err != nil {
		return nil, err
	}
	v, err := decodeScalar(dt, data[start:stop])
	if err != nil {
		return nil, err
	}
	s.store(v)
	return s.Value, nil
}

// decodeBit decodes a BIT member from its parent's bytes: Offset counts bits
// from the start of parent.
func (s *Symbol) decodeBit(parent []byte) error {
	if s.Length != 1 {
		return fmt.Errorf("%s is %d bits wide; only single BIT members are supported", s.Name, s.Length)
	}
	if uint64(s.Offset)/8 >= uint64(len(parent)) {
		return fmt.Errorf("bit %d of %s is past the %d-byte parent", s.Offset, s.Name, len(parent))
	}
	s.store(ReadBit(parent, int(s.Offset)))
	return nil
}

// store records a freshly decoded value. It always stores: the data came off
// the wire, so it is the current value. A rate limit here once dropped any
// change arriving within 50 ms of the previous one, so a notification returned
// the value it was replacing.
func (s *Symbol) store(v any) {
	s.LastUpdateTime = time.Now()
	s.Value = v
	s.Valid = true
	s.ValueParsed = true
}

var ParseableTypes = []string{
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
func InferBaseType(size uint32, baseType ams.DataType) string {
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
