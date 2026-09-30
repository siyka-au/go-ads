//go:build integration

package integration

// Integration tests against the AdsGo_Testing PLC project's FB_EnumTest
// fixture (Main.fbEnumTest), a distinct fixture from FB_TypeTest: its 15
// named enum types plus one anonymous inline enum (eImplicit) are all
// selected by nSeed MOD 3 picking an ordinal (A/B/C), not by the
// UDINT_TO_x arithmetic FB_TypeTest uses -- so it gets its own file rather
// than folding into seed_test.go.
//
//	go test -tags integration -run TestEnum -v ./integration/...

import (
	"context"
	"fmt"
	"testing"
	"time"

	ads "github.com/siyka-au/go-ads/v3"
)

const enumFB = "Main.fbEnumTest."

// setEnumSeed stops auto-increment and writes nSeed, then confirms both the
// write (nSeed reads back) and a PLC cycle with it: FB_EnumTest has no UDINT
// sentinel field like FB_TypeTest's nUdintVar, but E_Default's own declared
// values are 0/1/2 -- exactly its ordinal -- so eDefault serves the same
// purpose (without this, a read can race the PLC's CASE eDefault OF logic and
// observe the previous seed's derived values with the new nSeed already
// applied).
func setEnumSeed(t *testing.T, sess *ads.Session, seed uint32) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := sess.WriteValue(ctx, enumFB+"bAutoMode", false); err != nil {
		t.Fatalf("write bAutoMode: %v", err)
	}
	if err := sess.WriteValue(ctx, enumFB+"nSeed", seed); err != nil {
		t.Fatalf("write nSeed: %v", err)
	}
	wantDefault := int16(seed % 3)
	for {
		got, err := sess.ReadValues(ctx, []string{enumFB + "nSeed", enumFB + "eDefault"})
		if err == nil && got[enumFB+"nSeed"] == seed && got[enumFB+"eDefault"] == wantDefault {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("seed %d not applied: last read %v, %v", seed, got, err)
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// restoreEnumState puts fbEnumTest's control variables back as they were.
func restoreEnumState(t *testing.T, sess *ads.Session) {
	t.Helper()
	names := []string{enumFB + "nSeed", enumFB + "bAutoMode", enumFB + "nAutoTickInterval"}
	saved, err := sess.ReadValues(context.Background(), names)
	if err != nil {
		t.Fatalf("read control state: %v", err)
	}
	t.Cleanup(func() {
		if _, err := sess.WriteValues(context.Background(), saved); err != nil {
			t.Errorf("restore control state: %v", err)
		}
	})
}

func enumSession(t *testing.T) *ads.Session {
	t.Helper()
	sess := openSeedSession(t, true)
	restoreEnumState(t, sess)
	return sess
}

// enumCase describes one FB_EnumTest enum field: its declared enum type name,
// the primitive BaseTypeName() resolves to, its byte size, the three declared
// member values in eOptionA/B/C order (index = eDefault's ordinal, nSeed%3),
// and how to cast a member's int64 value to the Go type ReadValue returns.
type enumCase struct {
	field, enumType, baseType string
	size                      uint32
	values                    [3]int64
	toGo                      func(v int64) any
}

func i8(v int64) any   { return int8(v) }
func u8(v int64) any   { return uint8(v) }
func i16(v int64) any  { return int16(v) }
func u16(v int64) any  { return uint16(v) }
func i32(v int64) any  { return int32(v) }
func u32(v int64) any  { return uint32(v) }
func i64f(v int64) any { return v }
func u64(v int64) any  { return uint64(v) }

var enumCases = []enumCase{
	{"eDefault", "E_Default", "INT", 2, [3]int64{0, 1, 2}, i16},
	{"eByte", "E_Byte", "USINT", 1, [3]int64{0, 1, 2}, u8},
	{"eWord", "E_Word", "UINT", 2, [3]int64{0, 1, 2}, u16},
	{"eDWord", "E_DWord", "UDINT", 4, [3]int64{0, 1, 2}, u32},
	{"eLWord", "E_LWord", "ULINT", 8, [3]int64{0, 1, 2}, u64},
	{"eSInt", "E_Sint", "SINT", 1, [3]int64{0, 1, 2}, i8},
	{"eUSInt", "E_USint", "USINT", 1, [3]int64{0, 1, 2}, u8},
	{"eInt", "E_Int", "INT", 2, [3]int64{0, 1, 2}, i16},
	{"eUInt", "E_UInt", "UINT", 2, [3]int64{0, 1, 2}, u16},
	{"eDInt", "E_Dint", "DINT", 4, [3]int64{0, 1, 2}, i32},
	{"eUDInt", "E_UDint", "UDINT", 4, [3]int64{0, 1, 2}, u32},
	{"eLInt", "E_Lint", "LINT", 8, [3]int64{0, 1, 2}, i64f},
	{"eULInt", "E_ULint", "ULINT", 8, [3]int64{0, 1, 2}, u64},
	{"eNegative", "E_Negative", "INT", 2, [3]int64{-1, 0, 1}, i16},
	{"eSparse", "E_Sparse", "INT", 2, [3]int64{-10, 0, 100}, i16},
	// Anonymous inline enum; TwinCAT-synthesized name, confirmed live.
	{"eImplicit", "Implicit_Enum__FB_EnumTest__eImplicit", "INT", 2, [3]int64{0, 1, 2}, i16},
}

// memberNames are the three declared member names, in eOptionA/B/C order,
// shared by every named type. eImplicit uses its own eImplicitOptionX names
// and has no *Write counterpart, so it's excluded from TestEnumWrite.
var memberNames = [3]string{"eOptionA", "eOptionB", "eOptionC"}

func TestEnumReadMatrix(t *testing.T) {
	sess := enumSession(t)
	ctx := context.Background()
	names := make([]string, len(enumCases))
	for i, c := range enumCases {
		names[i] = enumFB + c.field
	}
	for _, seed := range []uint32{0, 1, 2, 3, 4, 5} {
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			setEnumSeed(t, sess, seed)
			ordinal := seed % 3

			got, err := sess.ReadValues(ctx, names)
			if err != nil {
				t.Fatalf("ReadValues: %v", err)
			}
			for _, c := range enumCases {
				want := c.toGo(c.values[ordinal])
				if v := got[enumFB+c.field]; v != want {
					t.Errorf("%s = %#v, want %#v", c.field, v, want)
				}
			}
		})
	}
}

func TestEnumSymbolMetadata(t *testing.T) {
	sess := enumSession(t)
	ctx := context.Background()
	for _, c := range enumCases {
		t.Run(c.field, func(t *testing.T) {
			v, err := sess.Symbol(ctx, enumFB+c.field)
			if err != nil {
				t.Fatalf("Symbol: %v", err)
			}
			if v.DataType != c.enumType {
				t.Errorf("DataType = %q, want %q", v.DataType, c.enumType)
			}
			if base := v.BaseTypeName(); base != c.baseType {
				t.Errorf("BaseTypeName() = %q, want %q", base, c.baseType)
			}
			if v.Length != c.size {
				t.Errorf("Length = %d, want %d", v.Length, c.size)
			}
			if len(v.Constants) != 3 {
				t.Fatalf("Constants = %v, want 3 entries", v.Constants)
			}
			wantNames := memberNames
			if c.field == "eImplicit" {
				wantNames = [3]string{"eImplicitOptionA", "eImplicitOptionB", "eImplicitOptionC"}
			}
			for i, want := range wantNames {
				if v.Constants[i].Name != want || v.Constants[i].Value != c.values[i] {
					t.Errorf("Constants[%d] = %+v, want {%s %d}", i, v.Constants[i], want, c.values[i])
				}
			}
		})
	}
}

// TestEnumWrite uses each *Write field (owned by the test, untouched by the
// PLC) to write every declared member value and read it back, then attempts
// one undeclared-but-in-width value per type and confirms it's rejected.
func TestEnumWrite(t *testing.T) {
	sess := enumSession(t)
	ctx := context.Background()
	for _, c := range enumCases {
		if c.field == "eImplicit" {
			continue // no *Write counterpart
		}
		t.Run(c.field, func(t *testing.T) {
			field := enumFB + c.field + "Write"
			for i, v := range c.values {
				want := c.toGo(v)
				if err := sess.WriteValue(ctx, field, want); err != nil {
					t.Errorf("WriteValue(%s) = %v: %v", memberNames[i], want, err)
					continue
				}
				got, err := sess.ReadValue(ctx, field)
				if err != nil {
					t.Errorf("ReadValue after writing %s: %v", memberNames[i], err)
					continue
				}
				if got != want {
					t.Errorf("read back %#v after writing %s=%#v", got, memberNames[i], want)
				}
			}

			bad := c.toGo(undeclaredValue(c))
			if err := sess.WriteValue(ctx, field, bad); err == nil {
				t.Errorf("WriteValue(%v) succeeded, want rejection (not a declared member of %s)", bad, c.enumType)
			}
		})
	}
}

// undeclaredValue picks a value within the type's width but not among its
// three declared members.
func undeclaredValue(c enumCase) int64 {
	for _, v := range []int64{99, 50, 42} {
		if v != c.values[0] && v != c.values[1] && v != c.values[2] {
			return v
		}
	}
	panic("no undeclared value found")
}
