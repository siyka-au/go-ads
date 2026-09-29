package ads

import (
	"fmt"
	"strconv"
	"strings"
)

// ParseNetID converts a dotted-notation NetID string (e.g. "192.168.1.1.1.1")
// to a 6-byte array. Returns an error if the string is malformed (wrong
// number of parts or non-numeric values).
func ParseNetID(source string) (result [6]byte, err error) {
	parts := strings.Split(source, ".")
	if len(parts) != 6 {
		return result, fmt.Errorf("invalid NetID %q: expected 6 dot-separated parts, got %d", source, len(parts))
	}
	for i, a := range parts {
		value, e := strconv.ParseUint(a, 10, 8)
		if e != nil {
			return result, fmt.Errorf("invalid NetID %q: part %d (%q): %w", source, i, a, e)
		}
		result[i] = byte(value)
	}
	return
}

// NewAMSAddress parses a dotted-notation NetID string plus a uint16 port
// into an AMSAddress. The typical TwinCAT 3 PLC runtime listens on port
// 851 (PortR0PlcTc3).
func NewAMSAddress(netID string, port uint16) (AMSAddress, error) {
	id, err := ParseNetID(netID)
	if err != nil {
		return AMSAddress{}, err
	}
	return AMSAddress{NetID: id, Port: port}, nil
}

// NetIDString returns the AMS NetID in dotted notation
// (e.g. "192.168.1.1.1.1"). Use String for the full "NetID:Port" form.
func (a AMSAddress) NetIDString() string {
	return fmt.Sprintf("%d.%d.%d.%d.%d.%d", a.NetID[0], a.NetID[1], a.NetID[2], a.NetID[3], a.NetID[4], a.NetID[5])
}

// String returns "NetID:Port" in dotted notation, e.g. "1.2.3.4.1.1:851".
func (a AMSAddress) String() string {
	return fmt.Sprintf("%s:%d", a.NetIDString(), a.Port)
}

// Equal reports whether a and other have the same NetID and Port.
func (a AMSAddress) Equal(other AMSAddress) bool {
	return a.NetID == other.NetID && a.Port == other.Port
}

// AMSAddress identifies an ADS endpoint by 6-byte NetID and 16-bit port.
//
// Wire layout is those 8 bytes contiguously, which encoding/binary reproduces from
// declaration order. DO NOT reorder or insert fields, or change either type,
// without updating AMSHeader and every binary.Read/Write site.
type AMSAddress struct {
	NetID [6]byte
	Port  uint16
}
