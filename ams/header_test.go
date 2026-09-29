package ams

import (
	"bytes"
	"testing"
)

func TestAMSHeaderSize_Constant(t *testing.T) {
	if HeaderSize != 32 {
		t.Errorf("HeaderSize = %d, want 32", HeaderSize)
	}
}

func TestEncodeAMSHeader_LengthIs32(t *testing.T) {
	h := Header{
		Target:    Address{NetID: [6]byte{5, 154, 236, 19, 1, 1}, Port: 851},
		Source:    Address{NetID: [6]byte{192, 168, 3, 52, 1, 1}, Port: 40000},
		Command:   CommandRead,
		State:     4,
		Length:    12,
		ErrorCode: 0,
		InvokeID:  42,
	}
	got := EncodeHeader(h)
	if len(got) != HeaderSize {
		t.Errorf("EncodeHeader returned %d bytes, want %d", len(got), HeaderSize)
	}
}

func TestEncodeAMSHeader_ParseRoundTrip(t *testing.T) {
	original := Header{
		Target:    Address{NetID: [6]byte{5, 154, 236, 19, 1, 1}, Port: 851},
		Source:    Address{NetID: [6]byte{192, 168, 3, 52, 1, 1}, Port: 40000},
		Command:   CommandDeviceNotification,
		State:     5,
		Length:    256,
		ErrorCode: 0,
		InvokeID:  0xDEADBEEF,
	}
	encoded := EncodeHeader(original)
	parsed, err := ParseHeader(encoded)
	if err != nil {
		t.Fatalf("ParseHeader: %v", err)
	}
	if parsed != original {
		t.Errorf("round-trip mismatch:\n got  %+v\n want %+v", parsed, original)
	}
}

func TestParseAMSHeader_RejectsShort(t *testing.T) {
	cases := [][]byte{
		nil,
		{},
		make([]byte, 31), // one byte short
	}
	for i, b := range cases {
		t.Run("", func(t *testing.T) {
			if _, err := ParseHeader(b); err == nil {
				t.Errorf("case %d (len=%d): expected error, got nil", i, len(b))
			}
		})
	}
}

func TestParseAMSHeader_IgnoresTrailingBytes(t *testing.T) {
	h := Header{
		Target:   Address{NetID: [6]byte{1, 2, 3, 4, 1, 1}, Port: 851},
		Source:   Address{NetID: [6]byte{5, 6, 7, 8, 1, 1}, Port: 40000},
		Command:  CommandRead,
		State:    4,
		Length:   8,
		InvokeID: 1,
	}
	header := EncodeHeader(h)
	withPayload := make([]byte, 0, len(header)+6)
	withPayload = append(withPayload, header...)
	withPayload = append(withPayload, 0xAA, 0xBB, 0xCC, 0xDD, 0xEE, 0xFF)
	got, err := ParseHeader(withPayload)
	if err != nil {
		t.Fatalf("ParseHeader with trailing payload: %v", err)
	}
	if got != h {
		t.Errorf("header mismatch:\n got  %+v\n want %+v", got, h)
	}
}

func TestEncodeAMSHeader_WireFormat(t *testing.T) {
	// Verify wire-level field layout against Beckhoff spec:
	// offset 0-7  target, 8-15 source, 16-17 command, 18-19 state,
	// offset 20-23 length, 24-27 errorcode, 28-31 invokeID.
	h := Header{
		Target:    Address{NetID: [6]byte{1, 2, 3, 4, 5, 6}, Port: 0x0851},
		Source:    Address{NetID: [6]byte{0xA, 0xB, 0xC, 0xD, 0xE, 0xF}, Port: 0x9C40},
		Command:   0x0009,
		State:     0x0004,
		Length:    0x00000020,
		ErrorCode: 0x00000000,
		InvokeID:  0x00000001,
	}
	b := EncodeHeader(h)

	// Target NetID at 0..5
	if !bytes.Equal(b[0:6], []byte{1, 2, 3, 4, 5, 6}) {
		t.Errorf("target netid bytes wrong: %x", b[0:6])
	}
	// Target Port at 6..7 (little-endian 0x0851)
	if b[6] != 0x51 || b[7] != 0x08 {
		t.Errorf("target port bytes wrong: %x %x", b[6], b[7])
	}
	// Source NetID at 8..13
	if !bytes.Equal(b[8:14], []byte{0xA, 0xB, 0xC, 0xD, 0xE, 0xF}) {
		t.Errorf("source netid bytes wrong: %x", b[8:14])
	}
	// Command at 16..17 (little-endian 0x0009)
	if b[16] != 0x09 || b[17] != 0x00 {
		t.Errorf("command bytes wrong: %x %x", b[16], b[17])
	}
	// InvokeID at 28..31 (little-endian 0x00000001)
	if b[28] != 0x01 || b[29] != 0x00 || b[30] != 0x00 || b[31] != 0x00 {
		t.Errorf("invokeID bytes wrong: %x %x %x %x", b[28], b[29], b[30], b[31])
	}
}

func TestParseAMSHeader_RejectsOversizedLength(t *testing.T) {
	h := Header{
		Target:   Address{NetID: [6]byte{1, 2, 3, 4, 1, 1}, Port: 851},
		Source:   Address{NetID: [6]byte{5, 6, 7, 8, 1, 1}, Port: 40000},
		Command:  CommandRead,
		State:    4,
		Length:   MaxPayloadSize + 1, // one byte over the cap
		InvokeID: 1,
	}
	bad := EncodeHeader(h)
	if _, err := ParseHeader(bad); err == nil {
		t.Errorf("ParseHeader should reject Length=%d > MaxPayloadSize=%d", h.Length, MaxPayloadSize)
	}
}

func TestParseAMSHeader_AcceptsExactCapLength(t *testing.T) {
	h := Header{
		Target:   Address{NetID: [6]byte{1, 2, 3, 4, 1, 1}, Port: 851},
		Source:   Address{NetID: [6]byte{5, 6, 7, 8, 1, 1}, Port: 40000},
		Command:  CommandRead,
		State:    4,
		Length:   MaxPayloadSize, // exact cap is allowed
		InvokeID: 1,
	}
	ok := EncodeHeader(h)
	if _, err := ParseHeader(ok); err != nil {
		t.Errorf("ParseHeader rejected exact-cap Length=%d: %v", h.Length, err)
	}
}
