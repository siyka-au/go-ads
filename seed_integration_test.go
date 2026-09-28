//go:build integration

package ads

// Typed-value tests against the AdsGo_Testing PLC project
// (siyka/ads-go/plc/testing). Every output of Main.fbTypeTest is a
// deterministic function of nSeed, so each test writes a seed and compares the
// Go values ReadValue/ReadValues/Update.Data return with ones computed here.
//
// Opt in with ADS_SEED_PLC=1, plus:
//   ADS_PLC_IP       - PLC IP address, or 127.0.0.1 for a local runtime
//   ADS_TARGET_AMS   - AMS NetID of the runtime
//   ADS_TARGET_PORT  - AMS port of the PLC project (default 851)
//   ADS_LOCAL_MODE   - true to go through the local TwinCAT router
//
//	go test -tags integration -run TestSeed -v .

import (
	"context"
	"fmt"
	"math"
	"os"
	"reflect"
	"strconv"
	"testing"
	"time"

	"cloud.google.com/go/civil"
)

const seedFB = "Main.fbTypeTest."

func seedSession(t *testing.T) *Session {
	t.Helper()
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
	target, err := NewAMSAddress(netID, uint16(port))
	if err != nil {
		t.Fatal(err)
	}
	var opts []SessionOption
	if local, _ := strconv.ParseBool(os.Getenv("ADS_LOCAL_MODE")); local {
		opts = append(opts, WithLocalMode())
	}
	// NewSession's context bounds the session's lifetime, so it must outlive
	// this function; the timeout applies to connecting only.
	sess, err := NewSession(context.Background(), AMSEndpoint{IP: getEnvOrDefault("ADS_PLC_IP", "127.0.0.1"), Port: 48898, AMS: target}, opts...)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	if err := sess.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if err := sess.LoadSymbols(ctx); err != nil {
		t.Fatalf("LoadSymbols: %v", err)
	}
	restoreSeedState(t, sess)
	return sess
}

// restoreSeedState puts nSeed and bAutoMode back as they were when the test
// ends.
func restoreSeedState(t *testing.T, sess *Session) {
	t.Helper()
	ctx := context.Background()
	seed, err := sess.ReadFromSymbol(ctx, seedFB+"nSeed")
	if err != nil {
		t.Fatalf("read nSeed: %v", err)
	}
	auto, err := sess.ReadFromSymbol(ctx, seedFB+"bAutoMode")
	if err != nil {
		t.Fatalf("read bAutoMode: %v", err)
	}
	t.Cleanup(func() {
		_ = sess.WriteToSymbol(ctx, seedFB+"nSeed", seed)
		_ = sess.WriteToSymbol(ctx, seedFB+"bAutoMode", auto)
	})
}

// setSeed stops auto-increment and writes nSeed, then confirms both the write
// (nSeed reads back) and a PLC cycle with it (nUdintVar, derived from nSeed,
// matches).
func setSeed(t *testing.T, sess *Session, seed uint32) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := sess.WriteToSymbol(ctx, seedFB+"bAutoMode", "false"); err != nil {
		t.Fatalf("write bAutoMode: %v", err)
	}
	if err := sess.WriteToSymbol(ctx, seedFB+"nSeed", strconv.FormatUint(uint64(seed), 10)); err != nil {
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

func unixDate(sec int64) civil.Date         { return civil.DateOf(time.Unix(sec, 0).UTC()) }
func unixDateTime(sec int64) civil.DateTime { return civil.DateTimeOf(time.Unix(sec, 0).UTC()) }

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
		"tdTimeOfDayVar":  civilTimeOf(time.Duration(int64(s)%msPerDay) * time.Millisecond),
		"dDateVar":        unixDate(int64(s)), // UDINT_TO_DATE keeps only the day
		"dtDateTimeVar":   unixDateTime(int64(s)),
		"tLtimeVar":       time.Duration(s),
		"tdLTimeOfDayVar": civilTimeOf(time.Duration(s)),
		"dLDateVar":       civil.DateOf(time.Unix(0, int64(s)).UTC()),
		"dtLDateTimeVar":  civil.DateTimeOf(time.Unix(0, int64(s)).UTC()),
		"sStringVar":      "S=" + strconv.FormatUint(uint64(s), 10),
	}
}

// Seeds cover sign wrap of every narrow type, a TIME over 24 h, a TOD with
// seconds and milliseconds, and the top of UDINT.
var typedSeeds = []uint32{0, 1, 127, 128, 255, 256, 32767, 32768, 65535, 65536, 100001, 90_061_001, math.MaxInt32, math.MaxInt32 + 1, math.MaxUint32}

func TestSeedReadValues(t *testing.T) {
	sess := seedSession(t)
	names := make([]string, 0, len(seedScalars(0)))
	for field := range seedScalars(0) {
		names = append(names, seedFB+field)
	}
	for _, seed := range typedSeeds {
		t.Run(fmt.Sprint(seed), func(t *testing.T) {
			setSeed(t, sess, seed)
			got, err := sess.ReadValues(context.Background(), names)
			if err != nil {
				t.Fatalf("ReadValues: %v", err)
			}
			for field, want := range seedScalars(seed) {
				if v := got[seedFB+field]; v != want {
					t.Errorf("%s = %#v (%T), want %#v (%T)", field, v, v, want, want)
				}
			}
		})
	}
}

// ReadValue and ReadValues must agree, one symbol at a time.
func TestSeedReadValue(t *testing.T) {
	sess := seedSession(t)
	const seed = 90_061_001
	setSeed(t, sess, seed)
	for field, want := range seedScalars(seed) {
		v, err := sess.ReadValue(context.Background(), seedFB+field)
		if err != nil {
			t.Errorf("%s: %v", field, err)
			continue
		}
		if v != want {
			t.Errorf("%s = %#v (%T), want %#v (%T)", field, v, v, want, want)
		}
	}
}

func TestSeedStruct(t *testing.T) {
	sess := seedSession(t)
	const seed = 100001
	setSeed(t, sess, seed)
	v, err := sess.ReadValue(context.Background(), seedFB+"stStructVar")
	if err != nil {
		t.Fatal(err)
	}
	got, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("stStructVar is %T, want map[string]any", v)
	}
	want := seedScalars(seed)
	want["nSeed"] = uint32(seed)
	for field, w := range want {
		if got[field] != w {
			t.Errorf("stStructVar.%s = %#v (%T), want %#v (%T)", field, got[field], got[field], w, w)
		}
	}
}

func TestSeedArrays(t *testing.T) {
	sess := seedSession(t)
	const seed = 32760 // INT elements cross 32767 → -32768
	setSeed(t, sess, seed)
	got, err := sess.ReadValues(context.Background(), []string{seedFB + "aIntArray", seedFB + "aDintArray", seedFB + "aIntArray2d"})
	if err != nil {
		t.Fatal(err)
	}
	wantInt, wantDint := make([]any, 10), make([]any, 10)
	for i := range uint32(10) {
		wantInt[i] = int16(seed + i)
		wantDint[i] = int32(seed + i)
	}
	want2d := make([]any, 3)
	for i := range uint32(3) {
		row := make([]any, 3)
		for j := range uint32(3) {
			row[j] = int16(seed + i*3 + j)
		}
		want2d[i] = row
	}
	for name, want := range map[string][]any{"aIntArray": wantInt, "aDintArray": wantDint, "aIntArray2d": want2d} {
		if !reflect.DeepEqual(got[seedFB+name], want) {
			t.Errorf("%s = %#v, want %#v", name, got[seedFB+name], want)
		}
	}
}

// A change written by another session straight after subscribing must arrive
// as its own value. It used to arrive carrying the value it replaced.
func TestSeedNotificationTyped(t *testing.T) {
	sess := seedSession(t)
	writer := seedSession(t)
	setSeed(t, writer, 1)

	ch := make(chan *Update, 64)
	var configs []NotificationConfig
	for field := range seedScalars(0) {
		if field == "sStringVar" {
			continue // the PLC rewrites it every cycle, so it notifies every cycle
		}
		configs = append(configs, NotificationConfig{SymbolName: seedFB + field, CycleTime: 10 * time.Millisecond, TransmissionMode: TransModeServerOnChange})
	}
	if _, err := sess.AddSymbolNotifications(context.Background(), configs, ch); err != nil {
		t.Fatal(err)
	}
	const seed = 100002
	if err := writer.WriteToSymbol(context.Background(), seedFB+"nSeed", strconv.Itoa(seed)); err != nil {
		t.Fatal(err)
	}

	want := seedScalars(seed)
	pending := len(configs)
	seen := map[string]bool{}
	deadline := time.After(5 * time.Second)
	for pending > 0 {
		select {
		case u := <-ch:
			field := u.Variable[len(seedFB):]
			if u.Data == want[field] && !seen[field] {
				seen[field] = true
				pending--
			}
		case <-deadline:
			for _, c := range configs {
				if field := c.SymbolName[len(seedFB):]; !seen[field] {
					t.Errorf("%s: never received %#v", field, want[field])
				}
			}
			return
		}
	}
}
