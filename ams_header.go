package ads

import (
	"bytes"
	"encoding/binary"
	"fmt"
)

type amsTCPHeader struct {
	Unknown1 uint8
	System   uint8
	Length   uint32
}

// AMSHeader is the 32-byte ADS frame header (per Beckhoff AMS/ADS spec).
// Wire layout (little-endian):
//
//	offset 0  Target NetID(6) + Port(2)       = AMSAddress
//	offset 8  Source NetID(6) + Port(2)       = AMSAddress
//	offset 16 CommandID (2)
//	offset 18 State flags (2)
//	offset 20 Data length (4) — payload bytes following this header
//	offset 24 ErrorCode (4)
//	offset 28 InvokeID (4)
//
// Exported so router and proxy implementations can parse, mutate and re-encode
// frames without touching the Client RPC plumbing.
type AMSHeader struct {
	Target    AMSAddress
	Source    AMSAddress
	Command   CommandID
	State     uint16
	Length    uint32
	ErrorCode uint32
	InvokeID  uint32
}

// AMSHeaderSize is the on-wire size of an AMSHeader in bytes.
const AMSHeaderSize = 32

// MaxAMSPayloadSize is a sanity bound on the Length field of a parsed
// AMSHeader. The Beckhoff ADS protocol does not define a hard ceiling but
// real-world frames stay under ~16MB (SumRead with thousands of items is
// the typical upper bound). 32MB is a defensive cap that rejects corrupt
// or hostile frames before they cause downstream slice-bounds panics in
// the documented "b[AMSHeaderSize : AMSHeaderSize+h.Length]" pattern.
const MaxAMSPayloadSize = 32 * 1024 * 1024 // 32 MiB

// ParseAMSHeader decodes a 32-byte AMS header from the front of b. The
// caller is responsible for stripping any preceding 6-byte AMS-over-TCP
// length prefix. Returns an error if b is shorter than AMSHeaderSize or
// if the decoded payload Length field exceeds MaxAMSPayloadSize (the
// latter catches corrupt/hostile frames that would otherwise panic the
// caller's "b[AMSHeaderSize : AMSHeaderSize+h.Length]" extraction).
// The payload (Length bytes) follows directly after the header in b;
// callers extract it as b[AMSHeaderSize : AMSHeaderSize+int(h.Length)].
func ParseAMSHeader(b []byte) (AMSHeader, error) {
	var h AMSHeader
	if len(b) < AMSHeaderSize {
		return h, fmt.Errorf("ams: header too short: %d bytes, need %d", len(b), AMSHeaderSize)
	}
	if err := binary.Read(bytes.NewReader(b[:AMSHeaderSize]), binary.LittleEndian, &h); err != nil {
		return h, fmt.Errorf("ams: parse header: %w", err)
	}
	if h.Length > MaxAMSPayloadSize {
		return h, fmt.Errorf("ams: declared payload length %d exceeds MaxAMSPayloadSize %d (corrupt or hostile frame)", h.Length, MaxAMSPayloadSize)
	}
	return h, nil
}

// EncodeAMSHeader serializes an AMSHeader to its 32-byte wire form.
// No length prefix is added; callers wrap with the 6-byte AMS-over-TCP
// header (1 unknown + 1 system + 4 length) when sending over TCP.
func EncodeAMSHeader(h AMSHeader) []byte {
	buf := make([]byte, 0, AMSHeaderSize)
	w := bytes.NewBuffer(buf)
	_ = binary.Write(w, binary.LittleEndian, h)
	return w.Bytes()
}
