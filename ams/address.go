package ams

import (
	"fmt"
	"strconv"
	"strings"
)

// ParseNetID converts a dotted-notation NetID string (e.g. "192.168.1.1.1.1")
// NetID. Returns an error if the string is malformed (wrong
// number of parts or non-numeric values).
func ParseNetID(source string) (result NetID, err error) {
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

// NewAddress parses a dotted-notation NetID string plus a port into an
// Address. The typical TwinCAT 3 PLC runtime listens on port 851
// (PortR0PlcTc3).
func NewAddress(netID string, port Port) (Address, error) {
	id, err := ParseNetID(netID)
	if err != nil {
		return Address{}, err
	}
	return Address{NetID: id, Port: port}, nil
}

// NetID is a 6-byte AMS NetID, conventionally the host's IPv4 address plus ".1.1".
type NetID [6]byte

// String returns the NetID in dotted notation, e.g. "192.168.1.1.1.1".
func (n NetID) String() string {
	return fmt.Sprintf("%d.%d.%d.%d.%d.%d", n[0], n[1], n[2], n[3], n[4], n[5])
}

// IsZero reports whether n is the all-zero NetID, meaning "not set".
func (n NetID) IsZero() bool { return n == NetID{} }

// String returns "NetID:Port" in dotted notation, e.g. "1.2.3.4.1.1:851".
func (a Address) String() string {
	return fmt.Sprintf("%s:%d", a.NetID, a.Port)
}

// Equal reports whether a and other have the same NetID and Port.
func (a Address) Equal(other Address) bool {
	return a.NetID == other.NetID && a.Port == other.Port
}

// Address identifies an ADS endpoint by 6-byte NetID and 16-bit port.
//
// Wire layout is those 8 bytes contiguously, which encoding/binary reproduces from
// declaration order. DO NOT reorder or insert fields, or change either type's
// size, without updating Header and every binary.Read/Write site.
type Address struct {
	NetID NetID
	Port  Port
}
