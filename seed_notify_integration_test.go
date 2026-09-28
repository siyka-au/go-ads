//go:build integration

package ads

// Notification tests against Main.fbTypeTest. Values arrive in Update.Value as
// the same Go types ReadValue returns, and are checked against what the PLC
// computes from the seed.

import (
	"context"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"
)

func subscribe(t *testing.T, sess *Session, names []string, mode TransMode, cycle time.Duration) chan *Update {
	t.Helper()
	ch := make(chan *Update, 4096)
	configs := make([]NotificationConfig, len(names))
	for i, n := range names {
		configs[i] = NotificationConfig{SymbolName: n, CycleTime: cycle, TransmissionMode: mode}
	}
	results, err := sess.AddSymbolNotifications(context.Background(), configs, ch)
	if err != nil {
		t.Fatalf("AddSymbolNotifications: %v", err)
	}
	for i, r := range results {
		if r.Skipped != nil || r.Error != ReturnCodeNoErrors {
			t.Fatalf("subscribe %s: skipped=%v code=%v", names[i], r.Skipped, r.Error)
		}
	}
	return ch
}

// awaitValues reads updates until every name has delivered want[name], or the
// timeout passes; it reports the last value seen for any that never matched.
func awaitValues(t *testing.T, ch chan *Update, want map[string]any, timeout time.Duration) {
	t.Helper()
	last := map[string]any{}
	pending := len(want)
	matched := map[string]bool{}
	deadline := time.After(timeout)
	for pending > 0 {
		select {
		case u := <-ch:
			w, ok := want[u.Variable]
			if !ok || matched[u.Variable] {
				continue
			}
			last[u.Variable] = u.Value
			if sameValue(u.Value, w) {
				matched[u.Variable] = true
				pending--
			}
		case <-deadline:
			for name, w := range want {
				if !matched[name] {
					t.Errorf("%s: never delivered %#v (%T); last %#v (%T)", name, w, w, last[name], last[name])
				}
			}
			return
		}
	}
}

// Every scalar type: the value on subscribe, then a change made by another
// session straight afterwards, for seed pairs that flip signs and wrap types.
func TestSeedNotifyEveryType(t *testing.T) {
	writer := seedSession(t)
	fields := seedFields()
	names := make([]string, len(fields))
	for i, f := range fields {
		names[i] = seedFB + f
	}
	for _, pair := range [][2]uint32{{1, 0}, {127, 128}, {32_767, 32_768}, {math.MaxInt32, math.MaxInt32 + 1}, {0, math.MaxUint32}, {86_399_999, 90_061_001}} {
		t.Run(fmt.Sprintf("%d_to_%d", pair[0], pair[1]), func(t *testing.T) {
			setSeed(t, writer, pair[0])
			sess := openSeedSession(t, true)
			ch := subscribe(t, sess, names, TransModeServerOnChange, 10*time.Millisecond)

			want := map[string]any{}
			for f, v := range seedScalars(pair[0]) {
				want[seedFB+f] = v
			}
			awaitValues(t, ch, want, 5*time.Second)

			if err := writer.WriteValue(context.Background(), seedFB+"nSeed", pair[1]); err != nil {
				t.Fatal(err)
			}
			// On change only: a member whose value is the same for both seeds
			// (a DATE within one day, float32 rounding to one value) stays quiet.
			before := seedScalars(pair[0])
			want = map[string]any{}
			for f, v := range seedScalars(pair[1]) {
				if !sameValue(v, before[f]) {
					want[seedFB+f] = v
				}
			}
			awaitValues(t, ch, want, 5*time.Second)
			// And, over the next 200 ms, nothing arrives for the unchanged ones.
			// sStringVar is exempt: the PLC rewrites it every cycle, so it
			// notifies every cycle whether or not the text changed.
			quiet := time.After(200 * time.Millisecond)
		drain:
			for {
				select {
				case u := <-ch:
					f := strings.TrimPrefix(u.Variable, seedFB)
					if f != "sStringVar" && sameValue(seedScalars(pair[1])[f], before[f]) {
						t.Errorf("%s: notified although its value did not change (%#v)", f, u.Value)
					}
				case <-quiet:
					break drain
				}
			}
		})
	}
}

// Whole structs and arrays by notification: each seed written must arrive as
// one consistent snapshot, every member matching that seed.
func TestSeedNotifyComposite(t *testing.T) {
	sess := seedSession(t)
	setSeed(t, sess, 0)
	ch := subscribe(t, sess, []string{seedFB + "stStructVar", seedFB + "aIntArray2d"}, TransModeServerOnChange, 10*time.Millisecond)

	for seed := uint32(1); seed <= 10; seed++ {
		if err := sess.WriteValue(context.Background(), seedFB+"nSeed", seed); err != nil {
			t.Fatal(err)
		}
		wantStruct := seedScalars(seed)
		wantStruct["nSeed"] = seed
		gotStruct, gotArray := false, false
		deadline := time.After(5 * time.Second)
		for !gotStruct || !gotArray {
			select {
			case u := <-ch:
				switch u.Variable {
				case seedFB + "stStructVar":
					m, ok := u.Value.(map[string]any)
					if !ok {
						t.Fatalf("struct update is %T", u.Value)
					}
					if m["nSeed"] != seed {
						continue // an earlier seed
					}
					for f, w := range wantStruct {
						assertValue(t, fmt.Sprintf("seed %d: stStructVar.%s", seed, f), m[f], w)
					}
					gotStruct = true
				case seedFB + "aIntArray2d":
					if sameValue(u.Value, expectedArrays(seed)["aIntArray2d"]) {
						gotArray = true
					}
				}
			case <-deadline:
				t.Fatalf("seed %d: struct delivered=%v, array delivered=%v", seed, gotStruct, gotArray)
			}
		}
	}
}

// With bAutoMode and a tick interval of 0 the PLC increments nSeed every cycle.
// On-change notifications must deliver the count strictly increasing: never
// repeated, reordered or stale.
func TestSeedNotifyAutoIncrement(t *testing.T) {
	sess := seedSession(t)
	setSeed(t, sess, 0)
	ch := subscribe(t, sess, []string{seedFB + "nUdintVar", seedFB + "tTimeVar"}, TransModeServerOnChange, 10*time.Millisecond)
	ctx := context.Background()
	if _, err := sess.WriteValues(ctx, map[string]any{seedFB + "nAutoTickInterval": uint32(0), seedFB + "bAutoMode": true}); err != nil {
		t.Fatal(err)
	}

	var counts, times []uint32
	deadline := time.After(2 * time.Second)
collect:
	for {
		select {
		case u := <-ch:
			switch u.Variable {
			case seedFB + "nUdintVar":
				counts = append(counts, u.Value.(uint32))
			case seedFB + "tTimeVar":
				times = append(times, uint32(u.Value.(time.Duration)/time.Millisecond))
			}
		case <-deadline:
			break collect
		}
	}
	if err := sess.WriteValue(ctx, seedFB+"bAutoMode", false); err != nil {
		t.Fatal(err)
	}
	for name, seq := range map[string][]uint32{"nUdintVar": counts, "tTimeVar": times} {
		if len(seq) < 50 {
			t.Errorf("%s: %d updates in 2 s at a 10 ms cycle, want many more", name, len(seq))
			continue
		}
		maxGap := uint32(0)
		for i := 1; i < len(seq); i++ {
			if seq[i] <= seq[i-1] {
				t.Errorf("%s: update %d went from %d to %d", name, i, seq[i-1], seq[i])
				break
			}
			maxGap = max(maxGap, seq[i]-seq[i-1])
		}
		t.Logf("%s: %d updates, %d..%d, largest step %d", name, len(seq), seq[0], seq[len(seq)-1], maxGap)
	}
}

// ServerCycle sends every cycle whether or not the value changed.
func TestSeedNotifyServerCycle(t *testing.T) {
	sess := seedSession(t)
	setSeed(t, sess, 4242)
	ch := subscribe(t, sess, []string{seedFB + "nUdintVar"}, TransModeServerCycle, 100*time.Millisecond)
	n := 0
	deadline := time.After(1050 * time.Millisecond)
collect:
	for {
		select {
		case u := <-ch:
			if u.Value != uint32(4242) {
				t.Errorf("update %d = %#v, want uint32(4242)", n, u.Value)
			}
			n++
		case <-deadline:
			break collect
		}
	}
	if n < 8 || n > 13 {
		t.Errorf("%d updates in 1.05 s at a 100 ms cycle, want about 11", n)
	}
}

// After DeleteDeviceNotification no more updates arrive for that handle.
func TestSeedNotifyUnsubscribe(t *testing.T) {
	sess := seedSession(t)
	setSeed(t, sess, 1)
	ch := make(chan *Update, 64)
	h, err := sess.AddSymbolNotification(context.Background(), seedFB+"nUdintVar", 0, 10*time.Millisecond, TransModeServerOnChange, ch)
	if err != nil {
		t.Fatal(err)
	}
	awaitValues(t, ch, map[string]any{seedFB + "nUdintVar": uint32(1)}, 5*time.Second)
	if err := sess.DeleteDeviceNotification(context.Background(), h); err != nil {
		t.Fatalf("DeleteDeviceNotification: %v", err)
	}
	setSeed(t, sess, 2)
	select {
	case u := <-ch:
		if !strings.HasSuffix(u.Variable, "nUdintVar") || u.Value == uint32(2) {
			t.Errorf("update after unsubscribe: %s = %#v", u.Variable, u.Value)
		}
	case <-time.After(500 * time.Millisecond):
	}
}
