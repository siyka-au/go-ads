//go:build integration

package integration

// Write tests against Main.fbWriteTest, which the PLC never modifies. Each
// value is written by one path and read back, and the structural struct write
// is read back member by member, so the write and read go through different
// code. Reads are verified independently against PLC-computed values in
// seed_integration_test.go.

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/siyka-au/go-ads/v3"

	"github.com/siyka-au/go-ads/v3/ams"

	"cloud.google.com/go/civil"
)

func date(y int, m time.Month, d int) civil.Date { return civil.Date{Year: y, Month: m, Day: d} }

func dateTime(y int, m time.Month, d, hh, mm, ss, ns int) civil.DateTime {
	return civil.DateTime{Date: date(y, m, d), Time: civil.Time{Hour: hh, Minute: mm, Second: ss, Nanosecond: ns}}
}

// printableASCII is every printable ASCII character once.
func printableASCII() string {
	var b strings.Builder
	for c := byte(' '); c <= '~'; c++ {
		b.WriteByte(c)
	}
	return b.String()
}

// writeEdges lists, per FB_WriteTest member, the values every write path must
// store and read back unchanged: each type's extremes, zero, negatives and the
// awkward cases (NaN, -0, subnormals, leap days, epoch rollovers, the longest
// string).
var writeEdges = map[string][]any{
	"bBoolVar":  {true, false},
	"nSintVar":  {int8(math.MinInt8), int8(-1), int8(0), int8(1), int8(math.MaxInt8)},
	"nUsintVar": {uint8(0), uint8(1), uint8(0xA5), uint8(math.MaxUint8)},
	"nByteVar":  {uint8(0), uint8(0x5A), uint8(math.MaxUint8)},
	"nIntVar":   {int16(math.MinInt16), int16(-1), int16(0), int16(1), int16(math.MaxInt16)},
	"nUintVar":  {uint16(0), uint16(1), uint16(0xA5A5), uint16(math.MaxUint16)},
	"nWordVar":  {uint16(0), uint16(0x5A5A), uint16(math.MaxUint16)},
	"nDintVar":  {int32(math.MinInt32), int32(-1), int32(0), int32(1), int32(math.MaxInt32)},
	"nUdintVar": {uint32(0), uint32(1), uint32(0xDEADBEEF), uint32(math.MaxUint32)},
	"nDwordVar": {uint32(0), uint32(0xA5A5A5A5), uint32(math.MaxUint32)},
	"nLintVar":  {int64(math.MinInt64), int64(-1), int64(0), int64(1<<53 + 1), int64(math.MaxInt64)},
	"nUlintVar": {uint64(0), uint64(1), uint64(1<<53 + 1), uint64(1 << 63), uint64(math.MaxUint64)},
	"nLwordVar": {uint64(0), uint64(0xCAFEBABEDEADBEEF), uint64(math.MaxUint64)},
	"fRealVar": {
		float32(0), float32(math.Copysign(0, -1)), float32(1.5), float32(-3.14),
		float32(math.MaxFloat32), float32(-math.MaxFloat32), float32(math.SmallestNonzeroFloat32),
		float32(1.1754944e-38), float32(math.Inf(1)), float32(math.Inf(-1)), float32(math.NaN()),
	},
	"fLrealVar": {
		0.0, math.Copysign(0, -1), 0.1, -2.718281828459045, math.MaxFloat64, -math.MaxFloat64,
		math.SmallestNonzeroFloat64, 2.2250738585072014e-308, math.Inf(1), math.Inf(-1), math.NaN(),
	},
	"tTimeVar": {
		time.Duration(0), time.Millisecond, 24 * time.Hour, 25*time.Hour + time.Minute + time.Second + time.Millisecond,
		time.Duration(math.MaxUint32) * time.Millisecond,
	},
	"tdTimeOfDayVar": {
		civil.Time{},
		civil.Time{Nanosecond: 1_000_000},
		civil.Time{Hour: 12},
		civil.Time{Hour: 23, Minute: 59, Second: 59, Nanosecond: 999_000_000},
	},
	"dDateVar": {date(1970, 1, 1), date(2000, 2, 29), date(2024, 2, 29), date(2038, 1, 19), date(2038, 1, 20), date(2106, 2, 7)},
	"dtDateTimeVar": {
		dateTime(1970, 1, 1, 0, 0, 0, 0), dateTime(2024, 2, 29, 23, 59, 59, 0),
		dateTime(2038, 1, 19, 3, 14, 7, 0), dateTime(2038, 1, 19, 3, 14, 8, 0), dateTime(2106, 2, 7, 6, 28, 15, 0),
	},
	"tLtimeVar": {time.Duration(0), time.Nanosecond, 999 * time.Nanosecond, 25 * time.Hour, time.Duration(math.MaxInt64)},
	"tdLTimeOfDayVar": {
		civil.Time{},
		civil.Time{Nanosecond: 1},
		civil.Time{Hour: 12, Minute: 34, Second: 56, Nanosecond: 789_012_345},
		civil.Time{Hour: 23, Minute: 59, Second: 59, Nanosecond: 999_999_999},
	},
	// LDATE's first whole day is 1677-09-22: the 21st's midnight is before
	// its earliest instant, 1677-09-21T00:12:43.145224192.
	"dLDateVar": {date(1677, 9, 22), date(1900, 1, 1), date(1969, 12, 31), date(1970, 1, 1), date(2024, 2, 29), date(2262, 4, 11)},
	"dtLDateTimeVar": {
		dateTime(1677, 9, 21, 0, 12, 43, 145_224_192), dateTime(1969, 12, 31, 23, 59, 59, 999_999_999),
		dateTime(1970, 1, 1, 0, 0, 0, 0), dateTime(2024, 2, 29, 12, 34, 56, 789_012_345), dateTime(2262, 4, 11, 23, 47, 16, 854_775_807),
	},
	"sStringVar": {"", "a", "S=1", printableASCII(), strings.Repeat("x", 255), strings.Repeat("é", 255), "Grüße ÄÖÜ ñ © ¿ ÿ"},
}

// writeRejects lists values each member's type cannot hold. Every path must
// refuse them before anything is sent, leaving the PLC value as it was.
var writeRejects = map[string][]any{
	"bBoolVar":       {1, "true"},
	"nSintVar":       {128, -129, int16(200)},
	"nUsintVar":      {-1, 256},
	"nIntVar":        {math.MaxInt16 + 1, math.MinInt16 - 1},
	"nUintVar":       {-1, math.MaxUint16 + 1},
	"nDintVar":       {int64(math.MaxInt32) + 1, int64(math.MinInt32) - 1},
	"nUdintVar":      {-1, int64(math.MaxUint32) + 1},
	"nLintVar":       {uint64(math.MaxInt64) + 1},
	"nUlintVar":      {-1, int64(math.MinInt64)},
	"fRealVar":       {1.5, 1, "1.5"},
	"fLrealVar":      {float32(1.5), 1},
	"tTimeVar":       {-time.Millisecond, time.Microsecond, time.Duration(math.MaxUint32+1) * time.Millisecond, uint32(5)},
	"tdTimeOfDayVar": {civil.Time{Hour: 24}, civil.Time{Nanosecond: 1}, time.Hour},
	"dDateVar":       {date(1969, 12, 31), date(2106, 2, 8), date(2024, 2, 30), dateTime(2024, 1, 1, 0, 0, 0, 0)},
	"dtDateTimeVar": {
		dateTime(1969, 12, 31, 23, 59, 59, 0), dateTime(2106, 2, 7, 6, 28, 16, 0),
		dateTime(2024, 1, 1, 0, 0, 0, 1), date(2024, 1, 1),
	},
	"tLtimeVar":       {-time.Nanosecond},
	"tdLTimeOfDayVar": {civil.Time{Hour: 24}, civil.Time{Minute: 60}},
	"dLDateVar":       {date(1677, 9, 21), date(2262, 4, 12)},
	"dtLDateTimeVar":  {dateTime(1677, 9, 21, 0, 12, 43, 145_224_191), dateTime(2262, 4, 11, 23, 47, 16, 854_775_808)},
	"sStringVar":      {strings.Repeat("x", 256), strings.Repeat("é", 256), "a\x00b", "中", "✓", "€", 42},
}

// writeFields lists FB_WriteTest's scalar members in a stable order.
func writeFields() []string { return seedFields() }

// Each edge value written alone, both as a flat member and as a struct member
// by path, and read back.
func TestSeedWriteEdgesEachPath(t *testing.T) {
	sess := seedSession(t)
	ctx := context.Background()
	for _, base := range []string{writeFB, writeFB + "stStructVar.", writeFB + "stStructVar.stSubStructVar."} {
		for _, f := range writeFields() {
			for i, v := range writeEdges[f] {
				name := base + f
				t.Run(fmt.Sprintf("%s/%d", strings.TrimPrefix(name, writeFB), i), func(t *testing.T) {
					if err := sess.WriteValue(ctx, name, v); err != nil {
						t.Fatalf("WriteValue %#v: %v", v, err)
					}
					got, err := sess.ReadValue(ctx, name)
					if err != nil {
						t.Fatalf("ReadValue: %v", err)
					}
					assertValue(t, name, got, v)
				})
			}
		}
	}
}

// edgeSet returns the k-th edge value of every member, cycling each list, so a
// few sets cover every value in every member at once.
func edgeSet(k int) map[string]any {
	out := make(map[string]any, len(writeEdges))
	for f, vs := range writeEdges {
		out[f] = vs[k%len(vs)]
	}
	return out
}

func maxEdges() int {
	n := 0
	for _, vs := range writeEdges {
		n = max(n, len(vs))
	}
	return n
}

// All members written in one batch, read back in one batch.
func TestSeedWriteEdgesBatch(t *testing.T) {
	sess := seedSession(t)
	ctx := context.Background()
	for k := range maxEdges() {
		t.Run(fmt.Sprint(k), func(t *testing.T) {
			set := edgeSet(k)
			values := make(map[string]any, len(set))
			names := make([]string, 0, len(set))
			for f, v := range set {
				values[writeFB+f] = v
				names = append(names, writeFB+f)
			}
			if _, err := sess.WriteValues(ctx, values); err != nil {
				t.Fatalf("WriteValues: %v", err)
			}
			got, err := sess.ReadValues(ctx, names)
			if err != nil {
				t.Fatalf("ReadValues: %v", err)
			}
			for f, v := range set {
				assertValue(t, f, got[writeFB+f], v)
			}
		})
	}
}

// The whole struct written in one call, read back member by member: the
// struct encoder and the scalar decoder are independent code.
func TestSeedWriteEdgesStructWhole(t *testing.T) {
	sess := seedSession(t)
	ctx := context.Background()
	const st = writeFB + "stStructVar"
	for k := range maxEdges() {
		t.Run(fmt.Sprint(k), func(t *testing.T) {
			set := edgeSet(k)
			set["nSeed"] = uint32(k * 1_000_003)
			set["bAutoMode"] = k%2 == 1
			set["nAutoTickInterval"] = uint32(math.MaxUint32 - k)
			// The nested struct takes the next set, so it differs from its parent.
			set["stSubStructVar"] = edgeSet(k + 1)
			if err := sess.WriteValue(ctx, st, set); err != nil {
				t.Fatalf("WriteValue struct: %v", err)
			}
			for f, v := range set {
				got, err := sess.ReadValue(ctx, st+"."+f)
				if err != nil {
					t.Errorf("ReadValue %s: %v", f, err)
					continue
				}
				assertValue(t, f, got, v)
			}
			whole, err := sess.ReadValue(ctx, st)
			if err != nil {
				t.Fatalf("ReadValue struct: %v", err)
			}
			assertValue(t, "stStructVar", whole, set)
		})
	}
}

// A value the type cannot hold is refused by every path, and the PLC keeps
// what it had. In a batch, the other members are still written.
func TestSeedWriteRejects(t *testing.T) {
	sess := seedSession(t)
	ctx := context.Background()
	for _, f := range writeFields() {
		for i, bad := range writeRejects[f] {
			t.Run(fmt.Sprintf("%s/%d", f, i), func(t *testing.T) {
				before := writeEdges[f][0]
				for _, name := range []string{writeFB + f, writeFB + "stStructVar." + f} {
					if err := sess.WriteValue(ctx, name, before); err != nil {
						t.Fatalf("seed %s: %v", name, err)
					}
					if err := sess.WriteValue(ctx, name, bad); err == nil {
						t.Errorf("WriteValue %s = %#v (%T): expected an error", name, bad, bad)
					}
					if got, err := sess.ReadValue(ctx, name); err != nil || !sameValue(got, before) {
						t.Errorf("%s after a refused write = %#v, %v; want %#v", name, got, err, before)
					}
				}

				// In a batch, the refused member is reported and the other written.
				other := writeFB + "nDintVar"
				if f == "nDintVar" {
					other = writeFB + "nLintVar"
				}
				otherValue := any(int32(-424242))
				if f == "nDintVar" {
					otherValue = int64(-424242)
				}
				_, err := sess.WriteValues(ctx, map[string]any{writeFB + f: bad, other: otherValue})
				var batchErr *ads.BatchError
				if !errors.As(err, &batchErr) || len(batchErr.Items) != 1 || batchErr.Items[0].Symbol != writeFB+f {
					t.Errorf("WriteValues with a bad %s: err = %v, want a BatchError naming it alone", f, err)
				}
				if got, _ := sess.ReadValue(ctx, other); got != otherValue {
					t.Errorf("the valid member of the batch was not written: %#v", got)
				}
				if got, _ := sess.ReadValue(ctx, writeFB+f); !sameValue(got, before) {
					t.Errorf("%s after a refused batch write = %#v, want %#v", f, got, before)
				}
			})
		}
	}
}

func intArray(vals ...int) []any {
	out := make([]any, len(vals))
	for i, v := range vals {
		out[i] = int16(v)
	}
	return out
}

func dintArray(vals ...int64) []any {
	out := make([]any, len(vals))
	for i, v := range vals {
		out[i] = int32(v)
	}
	return out
}

func TestSeedWriteArrays(t *testing.T) {
	sess := seedSession(t)
	ctx := context.Background()
	cases := []struct {
		name  string
		value []any
	}{
		{"aIntArray", intArray(math.MinInt16, -1, 0, 1, math.MaxInt16, 0x5A5A, -0x5A5A, 2, 3, 4)},
		{"aIntArray", intArray(0, 0, 0, 0, 0, 0, 0, 0, 0, 0)},
		{"aDintArray", dintArray(math.MinInt32, -1, 0, 1, math.MaxInt32, 7, 8, 9, 10, 11)},
		{"aIntArray2d", []any{intArray(math.MinInt16, 0, math.MaxInt16), intArray(-1, 1, -2), intArray(3, -3, 0)}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := sess.WriteValue(ctx, writeFB+c.name, c.value); err != nil {
				t.Fatalf("WriteValue: %v", err)
			}
			got, err := sess.ReadValue(ctx, writeFB+c.name)
			if err != nil {
				t.Fatal(err)
			}
			assertValue(t, c.name, got, c.value)
		})
	}

	// One element written by index shows in the whole array, the rest intact.
	base := intArray(10, 11, 12, 13, 14, 15, 16, 17, 18, 19)
	if err := sess.WriteValue(ctx, writeFB+"aIntArray", base); err != nil {
		t.Fatal(err)
	}
	if err := sess.WriteValue(ctx, writeFB+"aIntArray[3]", int16(-3)); err != nil {
		t.Fatalf("WriteValue aIntArray[3]: %v", err)
	}
	base[3] = int16(-3)
	got, _ := sess.ReadValue(ctx, writeFB+"aIntArray")
	assertValue(t, "aIntArray after [3]", got, base)

	grid := []any{intArray(1, 2, 3), intArray(4, 5, 6), intArray(7, 8, 9)}
	if err := sess.WriteValue(ctx, writeFB+"aIntArray2d", grid); err != nil {
		t.Fatal(err)
	}
	if err := sess.WriteValue(ctx, writeFB+"aIntArray2d[2,1]", int16(-8)); err != nil {
		t.Fatalf("WriteValue aIntArray2d[2,1]: %v", err)
	}
	grid[2].([]any)[1] = int16(-8)
	got, _ = sess.ReadValue(ctx, writeFB+"aIntArray2d")
	assertValue(t, "aIntArray2d after [2,1]", got, grid)

	// Wrong shapes are refused and leave the array as it was.
	for _, bad := range []any{
		intArray(1, 2, 3), // too short
		intArray(1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11), // too long
		dintArray(1, 2, 3, 4, 5, 6, 7, 8, 9, 40000), // an element out of INT's range
		[]int16{1, 2, 3, 4, 5, 6, 7, 8, 9, 10},      // not []any
	} {
		if err := sess.WriteValue(ctx, writeFB+"aIntArray", bad); err == nil {
			t.Errorf("WriteValue aIntArray = %#v: expected an error", bad)
		}
	}
	got, _ = sess.ReadValue(ctx, writeFB+"aIntArray")
	assertValue(t, "aIntArray after refused writes", got, base)
}

// A struct write must name every member and only its members.
func TestSeedWriteStructRejects(t *testing.T) {
	sess := seedSession(t)
	ctx := context.Background()
	const st = writeFB + "stStructVar"
	full := edgeSet(0)
	full["nSeed"], full["bAutoMode"], full["nAutoTickInterval"] = uint32(1), false, uint32(2)
	full["stSubStructVar"] = edgeSet(1)
	if err := sess.WriteValue(ctx, st, full); err != nil {
		t.Fatal(err)
	}
	missing := make(map[string]any, len(full))
	for k, v := range full {
		missing[k] = v
	}
	delete(missing, "sStringVar")
	extra := make(map[string]any, len(full)+1)
	for k, v := range full {
		extra[k] = v
	}
	extra["nNoSuchMember"] = int16(1)
	wrongType := make(map[string]any, len(full))
	for k, v := range full {
		wrongType[k] = v
	}
	wrongType["nIntVar"] = "one"
	for name, v := range map[string]any{"missing member": missing, "unknown member": extra, "member type": wrongType, "not a map": []any{1}} {
		if err := sess.WriteValue(ctx, st, v); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
	got, err := sess.ReadValue(ctx, st)
	if err != nil {
		t.Fatal(err)
	}
	assertValue(t, "stStructVar after refused writes", got, full)
}

// Reading or writing a symbol that does not exist fails with the PLC's
// "symbol not found", on its own and inside a batch.
func TestSeedMissingSymbol(t *testing.T) {
	sess := seedSession(t)
	ctx := context.Background()
	const missing = writeFB + "nNoSuchVar"
	if _, err := sess.ReadValue(ctx, missing); !errors.Is(err, ams.ReturnCodeDeviceSymbolNoFound) {
		t.Errorf("ReadValue missing: err = %v, want symbol not found", err)
	}
	if err := sess.WriteValue(ctx, missing, int16(1)); !errors.Is(err, ams.ReturnCodeDeviceSymbolNoFound) {
		t.Errorf("WriteValue missing: err = %v, want symbol not found", err)
	}
	got, err := sess.ReadValues(ctx, []string{missing, writeFB + "bBoolVar"})
	var batchErr *ads.BatchError
	if !errors.As(err, &batchErr) || len(batchErr.Items) != 1 || batchErr.Items[0].Symbol != missing {
		t.Errorf("ReadValues: err = %v, want a BatchError naming only the missing symbol", err)
	}
	if _, ok := got[writeFB+"bBoolVar"]; !ok {
		t.Errorf("ReadValues dropped the valid symbol: %#v", got)
	}
}
