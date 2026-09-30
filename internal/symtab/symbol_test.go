package symtab

import "testing"

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
