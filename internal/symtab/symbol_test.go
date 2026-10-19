package symtab

import (
	"encoding/hex"
	"strings"
	"testing"
)

// Raw 0xF009 (GroupSymbolInfoByNameEx) replies captured from a CX on
// TwinCAT 2.11.2234. For struct members and array elements NameLength
// counts the NUL terminator.
const (
	// .stMachineStatus.stMotor1.fSpeed: nameLen=33 for a 32-char name.
	tc211StructMemberReply = "4700000040400000c30f0000080000000500000002000000" +
		"2100050000002e53544d414348494e455354415455532e53544d4f544f52312e4653504545440000" +
		"4c5245414c0000"
	// .anCounters[1]: nameLen=15 for a 14-char name; the byte after it is '.', not NUL.
	tc211ArrayElementReply = "7100000040400000d7120000040000000300000001000000" +
		"0f0004003d002e414e434f554e544552535b315d002e44494e5400" +
		"205772697465206563686f202d20504c4320696e6372656d656e7473207768656e20616e79" +
		"207772697465207661726961626c65206368616e6765732000"
)

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("bad fixture: %v", err)
	}
	return b
}

func TestCString(t *testing.T) {
	cases := []struct {
		in   []byte
		want string
	}{
		{[]byte("MAIN.x"), "MAIN.x"},
		{[]byte("MAIN.x\x00"), "MAIN.x"},
		{[]byte("MAIN.x\x00junk"), "MAIN.x"},
		{[]byte{0}, ""},
		{nil, ""},
	}
	for _, c := range cases {
		if got := cString(c.in); got != c.want {
			t.Errorf("cString(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestParseSymbolInfo_TC211NulCountedInName(t *testing.T) {
	cases := []struct {
		reply, wantName, wantType string
	}{
		{tc211StructMemberReply, ".STMACHINESTATUS.STMOTOR1.FSPEED", "LREAL"},
		{tc211ArrayElementReply, ".ANCOUNTERS[1]", "DINT"},
	}
	for _, c := range cases {
		t.Run(c.wantName, func(t *testing.T) {
			info, err := ParseSymbolInfo(mustHex(t, c.reply))
			if err != nil {
				t.Fatalf("ParseSymbolInfo: %v", err)
			}
			if info.Name != c.wantName {
				t.Errorf("Name = %q, want %q", info.Name, c.wantName)
			}
			if info.DataType != c.wantType {
				t.Errorf("DataType = %q, want %q", info.DataType, c.wantType)
			}
			if strings.ContainsRune(info.Comment, 0) {
				t.Errorf("Comment %q contains NUL", info.Comment)
			}
		})
	}
}

func TestParseIntRange(t *testing.T) {
	tests := []struct {
		dt        string
		name      string
		low, high int64
		ok        bool
	}{
		{dt: "INT (-10..10)", name: "INT", low: -10, high: 10, ok: true},
		{dt: "BYTE (0..255)", name: "BYTE", low: 0, high: 255, ok: true},
		{dt: "UDINT (0..4294967295)", name: "UDINT", low: 0, high: 4294967295, ok: true},
		{dt: "DINT (-2147483648..2147483647)", name: "DINT", low: -2147483648, high: 2147483647, ok: true},

		// No suffix at all.
		{dt: "INT", ok: false},

		// STRING(n)/WSTRING(n) carry a length, not a range, and their base type
		// isn't in subrangeBaseTypes -- must not be mistaken for a subrange.
		{dt: "STRING(80)", ok: false},
		{dt: "WSTRING(255)", ok: false},

		// Not an integer type -- IEC 61131-3 doesn't allow subranges on these.
		{dt: "REAL (0..10)", ok: false},
		{dt: "BOOL (0..1)", ok: false},

		// Malformed bounds.
		{dt: "INT (10..)", ok: false},
		{dt: "INT (abc..10)", ok: false},
		{dt: "INT (10..-10)", ok: false}, // low > high
		{dt: "INT (-10.10)", ok: false},  // missing ".."
		{dt: "INT ()", ok: false},
		{dt: "INT (-10..10", ok: false}, // unterminated
	}
	for _, tt := range tests {
		t.Run(tt.dt, func(t *testing.T) {
			name, low, high, ok := parseIntRange(tt.dt)
			if ok != tt.ok {
				t.Fatalf("parseIntRange(%q) ok = %v, want %v", tt.dt, ok, tt.ok)
			}
			if !ok {
				return
			}
			if name != tt.name || low != tt.low || high != tt.high {
				t.Errorf("parseIntRange(%q) = (%q, %d, %d), want (%q, %d, %d)",
					tt.dt, name, low, high, tt.name, tt.low, tt.high)
			}
		})
	}
}

func TestResolveDataType(t *testing.T) {
	t.Run("subrange", func(t *testing.T) {
		name, low, high := resolveDataType("INT (-10..10)")
		if name != "INT" || low == nil || high == nil || *low != -10 || *high != 10 {
			t.Errorf("resolveDataType(%q) = (%q, %v, %v), want (\"INT\", -10, 10)", "INT (-10..10)", name, low, high)
		}
	})
	t.Run("string length, not a range", func(t *testing.T) {
		name, low, high := resolveDataType("STRING(80)")
		if name != "STRING" || low != nil || high != nil {
			t.Errorf("resolveDataType(%q) = (%q, %v, %v), want (\"STRING\", nil, nil)", "STRING(80)", name, low, high)
		}
	})
	t.Run("plain type, unaffected", func(t *testing.T) {
		name, low, high := resolveDataType("INT")
		if name != "INT" || low != nil || high != nil {
			t.Errorf("resolveDataType(%q) = (%q, %v, %v), want (\"INT\", nil, nil)", "INT", name, low, high)
		}
	})
}
