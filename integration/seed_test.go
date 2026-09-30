//go:build integration

package integration

// Integration tests against the AdsClient_DeterministicTester PLC project,
// part of the AdsClient_Tester TwinCAT solution
// (https://github.com/siyka-au/ads-client-tester). They assert Go values
// throughout.
//
//   - Main.fbTypeTest computes every output from nSeed each cycle, so reads are
//     checked against values the PLC itself derived, independent of this
//     library's encoder.
//   - Main.fbWriteTest is owned by the tests: the PLC never touches it. Once the
//     reads above prove decoding correct, write-then-read round trips there
//     test the encoder without circularity.
//   - Main.fbStructTest holds one struct under each pack_mode (0, 2, 4, 8).
//
// Opt in with ADS_SEED_PLC=1. Settings come from the environment or a .env
// file at the repo root (gitignored):
//
//	ADS_PLC_IP       PLC IP address, or 127.0.0.1 for a runtime on this host
//	ADS_TARGET_AMS   AMS NetID of the runtime
//	ADS_TARGET_PORT  AMS port of the PLC project (default 851)
//	ADS_LOCAL_MODE   true to go through the local TwinCAT router
//
//	go test -tags integration -run TestSeed -v .

import (
	"context"
	"fmt"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/siyka-au/go-ads/v3"

	"github.com/siyka-au/go-ads/v3/ams"

	"cloud.google.com/go/civil"
)

const (
	seedFB   = "Main.fbTypeTest."
	writeFB  = "Main.fbWriteTest."
	structFB = "Main.fbStructTest."
)

// loadSeedEnv sets KEY=VALUE pairs from .env that are not already set.
func loadSeedEnv(t *testing.T) {
	t.Helper()
	data, err := os.ReadFile(".env")
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if k, v, ok := strings.Cut(line, "="); ok {
			k, v = strings.TrimSpace(k), strings.Trim(strings.TrimSpace(v), `"'`)
			if _, set := os.LookupEnv(k); !set {
				t.Setenv(k, v)
			}
		}
	}
}

// seedSession connects to the seed PLC with the datatype table loaded, and
// restores Main.fbTypeTest's control variables when the test ends.
func seedSession(t *testing.T) *ads.Session {
	t.Helper()
	sess := openSeedSession(t, true)
	restoreSeedState(t, sess)
	return sess
}

func openSeedSession(t *testing.T, loadSymbols bool) *ads.Session {
	t.Helper()
	loadSeedEnv(t)
	if os.Getenv("ADS_SEED_PLC") == "" {
		t.Skip("ADS_SEED_PLC not set")
	}
	netID := os.Getenv("ADS_TARGET_AMS")
	if netID == "" {
		t.Fatal("ADS_TARGET_AMS must be set")
	}
	port, err := strconv.ParseUint(getEnvOrDefault("ADS_TARGET_PORT", "851"), 10, 16)
	if err != nil {
		t.Fatalf("ADS_TARGET_PORT: %v", err)
	}
	target, err := ams.NewAddress(netID, ams.Port(port))
	if err != nil {
		t.Fatal(err)
	}
	var opts []ads.Option
	if local, _ := strconv.ParseBool(os.Getenv("ADS_LOCAL_MODE")); local {
		opts = append(opts, ads.WithLocalMode())
	}
	// NewSession's context bounds the session's lifetime, so it must outlive
	// this function; the timeout applies to connecting only.
	sess, err := ads.NewSession(context.Background(), ads.Endpoint{Host: getEnvOrDefault("ADS_PLC_IP", "127.0.0.1"), Port: 48898, Target: target}, opts...)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := sess.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if loadSymbols {
		if err := sess.LoadSymbols(ctx); err != nil {
			t.Fatalf("LoadSymbols: %v", err)
		}
	}
	return sess
}

// restoreSeedState puts fbTypeTest's control variables back as they were.
func restoreSeedState(t *testing.T, sess *ads.Session) {
	t.Helper()
	names := []string{seedFB + "nSeed", seedFB + "bAutoMode", seedFB + "nAutoTickInterval"}
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

// setSeed stops auto-increment and writes nSeed, then confirms both the write
// (nSeed reads back) and a PLC cycle with it (nUdintVar, derived from nSeed).
func setSeed(t *testing.T, sess *ads.Session, seed uint32) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := sess.WriteValue(ctx, seedFB+"bAutoMode", false); err != nil {
		t.Fatalf("write bAutoMode: %v", err)
	}
	if err := sess.WriteValue(ctx, seedFB+"nSeed", seed); err != nil {
		t.Fatalf("write nSeed: %v", err)
	}
	for {
		got, err := sess.ReadValues(ctx, []string{seedFB + "nSeed", seedFB + "nUdintVar"})
		if err == nil && got[seedFB+"nSeed"] == seed && got[seedFB+"nUdintVar"] == seed {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("seed %d not applied: last read %v, %v", seed, got, err)
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func civilTimeOfDuration(d time.Duration) civil.Time {
	return civil.Time{
		Hour: int(d / time.Hour), Minute: int(d % time.Hour / time.Minute),
		Second: int(d % time.Minute / time.Second), Nanosecond: int(d % time.Second),
	}
}

// seedScalars is what FB_TypeTest computes from nSeed, as Go values.
func seedScalars(s uint32) map[string]any {
	return map[string]any{
		"bBoolVar":        s%2 == 0,
		"nSintVar":        int8(s),
		"nUsintVar":       uint8(s),
		"nByteVar":        uint8(s),
		"nIntVar":         int16(s),
		"nUintVar":        uint16(s),
		"nWordVar":        uint16(s),
		"nDintVar":        int32(s),
		"nUdintVar":       s,
		"nDwordVar":       s,
		"nLintVar":        int64(s),
		"nUlintVar":       uint64(s),
		"nLwordVar":       uint64(s),
		"fRealVar":        float32(s),
		"fLrealVar":       float64(s),
		"tTimeVar":        time.Duration(s) * time.Millisecond,
		"tdTimeOfDayVar":  civilTimeOfDuration(time.Duration(int64(s)%int64(24*time.Hour/time.Millisecond)) * time.Millisecond),
		"dDateVar":        civil.DateOf(time.Unix(int64(s), 0).UTC()), // UDINT_TO_DATE keeps the day
		"dtDateTimeVar":   civil.DateTimeOf(time.Unix(int64(s), 0).UTC()),
		"tLtimeVar":       time.Duration(s),
		"tdLTimeOfDayVar": civilTimeOfDuration(time.Duration(s)),
		"dLDateVar":       civil.DateOf(time.Unix(0, int64(s)).UTC()), // ULINT_TO_LDATE keeps the day
		"dtLDateTimeVar":  civil.DateTimeOf(time.Unix(0, int64(s)).UTC()),
		"sStringVar":      "S=" + strconv.FormatUint(uint64(s), 10),
	}
}

func seedFields() []string {
	fields := make([]string, 0, 24)
	for f := range seedScalars(0) {
		fields = append(fields, f)
	}
	sort.Strings(fields)
	return fields
}

// subRangeValue is what FB_TypeTest.nSubRange computes from nSeed. It is a
// top-level-only field: unlike every other seedScalars entry, it is not
// mirrored into ST_TypeTestStruct/ST_TypeTestSubStruct, nor into GVL_Test
// (which reuses seedScalars/seedFields for its own, unrelated variables) --
// so it is tested separately rather than folded into seedScalars.
func subRangeValue(s uint32) int16 { return int16(int32(s%21) - 10) }

// sameValue compares Go values as the library returns them, treating NaNs of
// the same type as equal and telling -0 from +0.
func sameValue(a, b any) bool {
	switch x := a.(type) {
	case float32:
		y, ok := b.(float32)
		if !ok {
			return false
		}
		if x != x || y != y {
			return x != x && y != y
		}
		return x == y && math.Signbit(float64(x)) == math.Signbit(float64(y))
	case float64:
		y, ok := b.(float64)
		if !ok {
			return false
		}
		if x != x || y != y {
			return x != x && y != y
		}
		return x == y && math.Signbit(x) == math.Signbit(y)
	case map[string]any:
		y, ok := b.(map[string]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for k, v := range x {
			if w, ok := y[k]; !ok || !sameValue(v, w) {
				return false
			}
		}
		return true
	case []any:
		y, ok := b.([]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for i := range x {
			if !sameValue(x[i], y[i]) {
				return false
			}
		}
		return true
	}
	return a == b
}

func assertValue(t *testing.T, label string, got, want any) {
	t.Helper()
	if !sameValue(got, want) {
		t.Errorf("%s = %#v (%T), want %#v (%T)", label, got, got, want, want)
	}
}

func ptrTo[T any](v T) *T { return &v }

// sameRange compares two possibly-nil subrange bounds: equal if both nil, or
// both non-nil with the same value.
func sameRange(a, b *int64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func fmtRangeBound(v *int64) string {
	if v == nil {
		return "<nil>"
	}
	return strconv.FormatInt(*v, 10)
}

// Seeds cover the wrap of every narrow type, float32's last exact integer and
// the first it rounds, TOD's last millisecond and its rollover, a TIME over
// 24 h, and the edges of DINT and UDINT.
var seedCases = []struct {
	name string
	seed uint32
}{
	{"zero", 0},
	{"one", 1},
	{"sint_max", 127},
	{"sint_wrap", 128},
	{"byte_max", 255},
	{"byte_wrap", 256},
	{"int_max", 32_767},
	{"int_wrap", 32_768},
	{"uint_max", 65_535},
	{"uint_wrap", 65_536},
	{"real_exact_max", 16_777_215},
	{"real_rounds", 16_777_217},
	{"tod_last_ms", 86_399_999},
	{"tod_rolls_over", 86_400_000},
	{"time_over_24h", 90_061_001},
	{"dint_max", math.MaxInt32},
	{"dint_wrap", math.MaxInt32 + 1},
	{"udint_max", math.MaxUint32},
}

func expectedArrays(s uint32) map[string]any {
	a, d := make([]any, 10), make([]any, 10)
	for i := range uint32(10) {
		a[i] = int16(s + i)
		d[i] = int32(s + i)
	}
	a2 := make([]any, 3)
	for i := range uint32(3) {
		row := make([]any, 3)
		for j := range uint32(3) {
			row[j] = int16(s + i*3 + j)
		}
		a2[i] = row
	}
	// fbTypeTest's aDintArray is ARRAY[-9..9]: element i = DINT(seed) + i, so
	// the slice starts at index -9.
	neg := make([]any, 19)
	for k := range 19 {
		neg[k] = int32(s) + int32(k-9)
	}
	// aStructVar is ten ST_TypeTestSubStructs, each holding the scalars.
	structs := make([]any, 10)
	for k := range structs {
		structs[k] = seedScalars(s)
	}
	return map[string]any{"aIntArray": a, "aDintArray": neg, "aIntArray2d": a2, "aStructVar": structs}
}

// gvlArrays is FB_GvlTest's arrays: aDintArray there is ARRAY[0..9].
func gvlArrays(s uint32) map[string]any {
	out := expectedArrays(s)
	d := make([]any, 10)
	for i := range uint32(10) {
		d[i] = int32(s + i)
	}
	out["aDintArray"] = d
	delete(out, "aStructVar")
	return out
}

// Every seed, read three ways: all scalars in one batch, the struct whole, and
// the arrays whole.
func TestSeedReadMatrix(t *testing.T) {
	sess := seedSession(t)
	ctx := context.Background()
	fields := seedFields()
	names := make([]string, len(fields), len(fields)+1)
	for i, f := range fields {
		names[i] = seedFB + f
	}
	names = append(names, seedFB+"nSubRange")
	for _, tc := range seedCases {
		t.Run(tc.name, func(t *testing.T) {
			setSeed(t, sess, tc.seed)
			want := seedScalars(tc.seed)

			got, err := sess.ReadValues(ctx, names)
			if err != nil {
				t.Fatalf("ReadValues: %v", err)
			}
			for _, f := range fields {
				assertValue(t, f, got[seedFB+f], want[f])
			}
			assertValue(t, "nSubRange", got[seedFB+"nSubRange"], subRangeValue(tc.seed))

			v, err := sess.ReadValue(ctx, seedFB+"stStructVar")
			if err != nil {
				t.Fatalf("ReadValue stStructVar: %v", err)
			}
			st, ok := v.(map[string]any)
			if !ok {
				t.Fatalf("stStructVar is %T, want map[string]any", v)
			}
			assertValue(t, "stStructVar.nSeed", st["nSeed"], tc.seed)
			for _, f := range fields {
				assertValue(t, "stStructVar."+f, st[f], want[f])
			}
			assertValue(t, "stStructVar.stSubStructVar", st["stSubStructVar"], want)
			if len(st) != len(fields)+4 { // + nSeed, bAutoMode, nAutoTickInterval, stSubStructVar
				t.Errorf("stStructVar has %d members, want %d", len(st), len(fields)+4)
			}

			for name, w := range expectedArrays(tc.seed) {
				v, err := sess.ReadValue(ctx, seedFB+name)
				if err != nil {
					t.Errorf("ReadValue %s: %v", name, err)
					continue
				}
				assertValue(t, name, v, w)
			}
		})
	}
}

// One seed through every way of naming a value: each scalar alone, each struct
// member by path, each array element by index -- with the datatype table and
// without it (symbols resolved on demand by the PLC).
func TestSeedReadAccessPaths(t *testing.T) {
	const seed = 100_001
	for _, mode := range []struct {
		name string
		load bool
	}{{"loaded", true}, {"on_demand", false}} {
		t.Run(mode.name, func(t *testing.T) {
			sess := openSeedSession(t, mode.load)
			restoreSeedState(t, sess)
			setSeed(t, sess, seed)
			ctx := context.Background()
			want := seedScalars(seed)

			bases := []string{seedFB, seedFB + "stStructVar.", seedFB + "stStructVar.stSubStructVar.", seedFB + "aStructVar[0].", seedFB + "aStructVar[9]."}
			for _, base := range bases {
				for _, f := range seedFields() {
					v, err := sess.ReadValue(ctx, base+f)
					if err != nil {
						t.Errorf("ReadValue %s%s: %v", base, f, err)
						continue
					}
					assertValue(t, base+f, v, want[f])
				}
			}

			arrays := expectedArrays(seed)
			for i := range 10 {
				name := fmt.Sprintf("%saIntArray[%d]", seedFB, i)
				v, err := sess.ReadValue(ctx, name)
				if err != nil {
					t.Errorf("ReadValue %s: %v", name, err)
					continue
				}
				assertValue(t, name, v, arrays["aIntArray"].([]any)[i])
			}
			for i := range 3 {
				for j := range 3 {
					name := fmt.Sprintf("%saIntArray2d[%d,%d]", seedFB, i, j) // IEC syntax
					v, err := sess.ReadValue(ctx, name)
					if err != nil {
						t.Errorf("ReadValue %s: %v", name, err)
						continue
					}
					assertValue(t, name, v, arrays["aIntArray2d"].([]any)[i].([]any)[j])
				}
			}
			for i := -9; i <= 9; i++ {
				name := fmt.Sprintf("%saDintArray[%d]", seedFB, i)
				v, err := sess.ReadValue(ctx, name)
				if err != nil {
					t.Errorf("ReadValue %s: %v", name, err)
					continue
				}
				assertValue(t, name, v, arrays["aDintArray"].([]any)[i+9])
			}
			// An array element that is itself a struct, read whole: a struct, so
			// only with the datatype table (TestSeedCompositeNeedsDatatypeTable
			// covers the on-demand error).
			if mode.load {
				v, err := sess.ReadValue(ctx, seedFB+"aStructVar[4]")
				if err != nil {
					t.Errorf("ReadValue aStructVar[4]: %v", err)
				} else {
					assertValue(t, "aStructVar[4]", v, want)
				}
			}
		})
	}
}

// Without the datatype table a struct or array cannot be decoded; the error
// must say so rather than misread it.
func TestSeedCompositeNeedsDatatypeTable(t *testing.T) {
	sess := openSeedSession(t, false)
	for _, name := range []string{"stStructVar", "aIntArray", "aIntArray2d", "aDintArray", "aStructVar", "aStructVar[4]", "stStructVar.stSubStructVar"} {
		_, err := sess.ReadValue(context.Background(), seedFB+name)
		if err == nil || !strings.Contains(err.Error(), "LoadSymbols") {
			t.Errorf("%s without the table: err = %v, want one pointing at LoadSymbols", name, err)
		}
	}
}

// The symbol metadata a consumer sees: TwinCAT's type names and sizes.
func TestSeedSymbolMetadata(t *testing.T) {
	sess := seedSession(t)
	tests := []struct {
		name, dataType     string
		length             uint32
		rangeMin, rangeMax *int64 // both nil except for nSubRange
	}{
		{"bBoolVar", "BOOL", 1, nil, nil},
		{"nSintVar", "SINT", 1, nil, nil},
		{"nUsintVar", "USINT", 1, nil, nil},
		{"nByteVar", "BYTE", 1, nil, nil},
		{"nIntVar", "INT", 2, nil, nil},
		{"nUintVar", "UINT", 2, nil, nil},
		{"nWordVar", "WORD", 2, nil, nil},
		{"nDintVar", "DINT", 4, nil, nil},
		{"nUdintVar", "UDINT", 4, nil, nil},
		{"nDwordVar", "DWORD", 4, nil, nil},
		{"nLintVar", "LINT", 8, nil, nil},
		{"nUlintVar", "ULINT", 8, nil, nil},
		{"nLwordVar", "LWORD", 8, nil, nil},
		{"fRealVar", "REAL", 4, nil, nil},
		{"fLrealVar", "LREAL", 8, nil, nil},
		// TwinCAT reports the subrange declaration "INT(-10..10)" as the type
		// name "INT (-10..10)" (a space before the parenthesis, unlike
		// STRING(n)); resolveDataType strips it to "INT" and recovers the bound.
		{"nSubRange", "INT", 2, ptrTo(int64(-10)), ptrTo(int64(10))},
		{"tTimeVar", "TIME", 4, nil, nil},
		{"tdTimeOfDayVar", "TIME_OF_DAY", 4, nil, nil},
		{"dDateVar", "DATE", 4, nil, nil},
		{"dtDateTimeVar", "DATE_AND_TIME", 4, nil, nil},
		{"tLtimeVar", "LTIME", 8, nil, nil},
		{"tdLTimeOfDayVar", "LTIME_OF_DAY", 8, nil, nil},
		{"dLDateVar", "LDATE", 8, nil, nil},
		{"dtLDateTimeVar", "LDATE_AND_TIME", 8, nil, nil},
		{"sStringVar", "STRING", 256, nil, nil},
		{"aIntArray", "ARRAY [0..9] OF INT", 20, nil, nil},
		{"aDintArray", "ARRAY [-9..9] OF DINT", 76, nil, nil},
		{"aStructVar", "ARRAY [0..9] OF ST_TypeTestSubStruct", 0, nil, nil},
		{"aIntArray2d", "ARRAY [0..2,0..2] OF INT", 18, nil, nil},
		{"stStructVar", "ST_TypeTestStruct", 0, nil, nil}, // size logged, not fixed here
	}
	for _, tt := range tests {
		v, err := sess.Symbol(context.Background(), seedFB+tt.name)
		if err != nil {
			t.Errorf("Symbol %s: %v", tt.name, err)
			continue
		}
		if v.DataType != tt.dataType {
			t.Errorf("%s: DataType %q, want %q", tt.name, v.DataType, tt.dataType)
		}
		if tt.length != 0 && v.Length != tt.length {
			t.Errorf("%s: Length %d, want %d", tt.name, v.Length, tt.length)
		}
		if !sameRange(v.RangeMin, tt.rangeMin) || !sameRange(v.RangeMax, tt.rangeMax) {
			t.Errorf("%s: range = %s..%s, want %s..%s",
				tt.name, fmtRangeBound(v.RangeMin), fmtRangeBound(v.RangeMax),
				fmtRangeBound(tt.rangeMin), fmtRangeBound(tt.rangeMax))
		}
		if tt.length == 0 {
			t.Logf("%s: Length %d, %d members", tt.name, v.Length, len(v.Children()))
		}
	}
}

// ReadValue always reads from the PLC. It used to serve a value decoded less
// than 50 ms earlier from cache, so a struct read straight after a change the
// PLC made (here, derived from a new seed) returned the old value.
func TestSeedReadValueIsNeverStale(t *testing.T) {
	sess := seedSession(t)
	ctx := context.Background()
	for _, seed := range []uint32{11, 12, 13} {
		setSeed(t, sess, seed)
		v, err := sess.ReadValue(ctx, seedFB+"stStructVar")
		if err != nil {
			t.Fatal(err)
		}
		assertValue(t, fmt.Sprintf("stStructVar.nSeed after seed %d", seed), v.(map[string]any)["nSeed"], seed)
		a, err := sess.ReadValue(ctx, seedFB+"aIntArray")
		if err != nil {
			t.Fatal(err)
		}
		assertValue(t, fmt.Sprintf("aIntArray[0] after seed %d", seed), a.([]any)[0], int16(seed))
	}
}
