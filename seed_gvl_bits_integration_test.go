//go:build integration

package ads

// Tests against FB_GvlTest, which writes the same seed-derived values as
// FB_TypeTest into the global list GVL_Test, and FB_BitPackingTest, whose
// ST_BitPacking holds 64 BIT members: bBitN = bit N of its ULINT nSeed.

import (
	"context"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/siyka-au/go-ads/v3/ams"
)

const (
	gvlFB  = "Main.fbGvlTest."
	gvl    = "GVL_Test."
	bitsFB = "Main.fbBitPackingTest."
)

// restoreControl puts an FB's nSeed and bAutoMode back as they were.
func restoreControl(t *testing.T, sess *Session, fb string) {
	t.Helper()
	names := []string{fb + "nSeed", fb + "bAutoMode", fb + "nAutoTickInterval"}
	saved, err := sess.ReadValues(context.Background(), names)
	if err != nil {
		t.Fatalf("read %s control state: %v", fb, err)
	}
	t.Cleanup(func() {
		if _, err := sess.WriteValues(context.Background(), saved); err != nil {
			t.Errorf("restore %s control state: %v", fb, err)
		}
	})
}

// waitForValue polls read until it returns want, failing after 5 s.
func waitForValue(t *testing.T, what string, want any, read func() (any, error)) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		got, err := read()
		if err == nil && sameValue(got, want) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: never became %#v; last %#v, %v", what, want, got, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func setGvlSeed(t *testing.T, sess *Session, seed uint32) {
	t.Helper()
	ctx := context.Background()
	if _, err := sess.WriteValues(ctx, map[string]any{gvlFB + "bAutoMode": false, gvlFB + "nSeed": seed}); err != nil {
		t.Fatalf("write GVL seed: %v", err)
	}
	waitForValue(t, "GVL_Test.nUdintVar", seed, func() (any, error) { return sess.ReadValue(ctx, gvl+"nUdintVar") })
}

// Every seed through the global list: scalars in one batch, the struct whole,
// the arrays whole.
func TestSeedGvlReadMatrix(t *testing.T) {
	sess := seedSession(t)
	restoreControl(t, sess, gvlFB)
	ctx := context.Background()
	fields := seedFields()
	names := make([]string, len(fields))
	for i, f := range fields {
		names[i] = gvl + f
	}
	for _, tc := range seedCases {
		t.Run(tc.name, func(t *testing.T) {
			setGvlSeed(t, sess, tc.seed)
			want := seedScalars(tc.seed)
			got, err := sess.ReadValues(ctx, names)
			if err != nil {
				t.Fatalf("ReadValues: %v", err)
			}
			for _, f := range fields {
				assertValue(t, gvl+f, got[gvl+f], want[f])
			}

			v, err := sess.ReadValue(ctx, gvl+"stStructVar")
			if err != nil {
				t.Fatalf("ReadValue stStructVar: %v", err)
			}
			st := v.(map[string]any)
			assertValue(t, "GVL_Test.stStructVar.nSeed", st["nSeed"], tc.seed)
			// FB_GvlTest does not fill stSubStructVar; the rest must match.
			for _, f := range fields {
				assertValue(t, "GVL_Test.stStructVar."+f, st[f], want[f])
			}

			for name, w := range gvlArrays(tc.seed) {
				v, err := sess.ReadValue(ctx, gvl+name)
				if err != nil {
					t.Errorf("ReadValue %s: %v", name, err)
					continue
				}
				assertValue(t, gvl+name, v, w)
			}
		})
	}
}

// Global variables by every path: each alone, each struct member, each 2-D
// element -- with the datatype table and resolved on demand.
func TestSeedGvlAccessPaths(t *testing.T) {
	const seed = 90_061_001
	for _, load := range []bool{true, false} {
		t.Run(fmt.Sprintf("loaded=%v", load), func(t *testing.T) {
			sess := openSeedSession(t, load)
			restoreControl(t, sess, gvlFB)
			setGvlSeed(t, sess, seed)
			ctx := context.Background()
			want := seedScalars(seed)
			for _, base := range []string{gvl, gvl + "stStructVar."} {
				for _, f := range seedFields() {
					v, err := sess.ReadValue(ctx, base+f)
					if err != nil {
						t.Errorf("ReadValue %s%s: %v", base, f, err)
						continue
					}
					assertValue(t, base+f, v, want[f])
				}
			}
			grid := gvlArrays(seed)["aIntArray2d"].([]any)
			for i := range 3 {
				for j := range 3 {
					name := fmt.Sprintf("%saIntArray2d[%d,%d]", gvl, i, j)
					v, err := sess.ReadValue(ctx, name)
					if err != nil {
						t.Errorf("ReadValue %s: %v", name, err)
						continue
					}
					assertValue(t, name, v, grid[i].([]any)[j])
				}
			}
		})
	}
}

// --- bit packing ---

// bitPatterns covers every single bit, none, all, alternating and irregular
// patterns, and the two ends together.
func bitPatterns() []uint64 {
	p := []uint64{0, math.MaxUint64, 0x5555555555555555, 0xAAAAAAAAAAAAAAAA, 0x0123456789ABCDEF, 0x8000000000000001}
	for n := range 64 {
		p = append(p, 1<<n)
	}
	return p
}

// bitsOf is ST_BitPacking's value for pattern v: bBitN = bit N of v.
func bitsOf(v uint64) map[string]any {
	m := make(map[string]any, 64)
	for n := range 64 {
		m[fmt.Sprintf("bBit%d", n)] = v>>n&1 == 1
	}
	return m
}

// patternOf reassembles a pattern from an ST_BitPacking value.
func patternOf(t *testing.T, v any) uint64 {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok || len(m) != 64 {
		t.Fatalf("ST_BitPacking value is %T with %d members", v, len(m))
	}
	var out uint64
	for n := range 64 {
		if b, _ := m[fmt.Sprintf("bBit%d", n)].(bool); b {
			out |= 1 << n
		}
	}
	return out
}

func bitPaths(base string) []string {
	out := make([]string, 64)
	for n := range 64 {
		out[n] = fmt.Sprintf("%s.bBit%d", base, n)
	}
	return out
}

func setBitSeed(t *testing.T, sess *Session, seed uint64) {
	t.Helper()
	ctx := context.Background()
	if _, err := sess.WriteValues(ctx, map[string]any{bitsFB + "bAutoMode": false, bitsFB + "nSeed": seed}); err != nil {
		t.Fatalf("write bit seed: %v", err)
	}
	waitForValue(t, "stBitPacking", bitsOf(seed), func() (any, error) { return sess.ReadValue(ctx, bitsFB+"stBitPacking") })
}

func TestSeedBitPackingLayout(t *testing.T) {
	sess := seedSession(t)
	for _, target := range []string{"stBitPacking", "stBitPackingWrite"} {
		v, err := sess.Symbol(context.Background(), bitsFB+target)
		if err != nil {
			t.Fatalf("Symbol %s: %v", target, err)
		}
		if v.DataType != "ST_BitPacking" || v.Length != 8 {
			t.Errorf("%s: %s of %d bytes, want ST_BitPacking of 8", target, v.DataType, v.Length)
		}
		children := v.Children()
		if len(children) != 64 {
			t.Fatalf("%s: %d members, want 64", target, len(children))
		}
		for n := range 64 {
			c := children[fmt.Sprintf("bBit%d", n)]
			if c.DataType != "BIT" || !c.BitMember || c.Offset != uint32(n) || c.Length != 1 {
				t.Errorf("%s.bBit%d: %s, bit member %v, offset %d, length %d; want BIT at bit %d, 1 bit",
					target, n, c.DataType, c.BitMember, c.Offset, c.Length, n)
			}
		}
	}
}

// Every pattern the PLC sets, read as the whole struct and as all 64 bits in
// one batch; three patterns also bit by bit.
func TestSeedBitPackingRead(t *testing.T) {
	sess := seedSession(t)
	restoreControl(t, sess, bitsFB)
	ctx := context.Background()
	paths := bitPaths(bitsFB + "stBitPacking")
	for _, p := range bitPatterns() {
		t.Run(fmt.Sprintf("%#016x", p), func(t *testing.T) {
			setBitSeed(t, sess, p)
			want := bitsOf(p)
			whole, err := sess.ReadValue(ctx, bitsFB+"stBitPacking")
			if err != nil {
				t.Fatal(err)
			}
			assertValue(t, "stBitPacking", whole, want)

			got, err := sess.ReadValues(ctx, paths)
			if err != nil {
				t.Fatalf("ReadValues: %v", err)
			}
			for n, path := range paths {
				assertValue(t, path, got[path], want[fmt.Sprintf("bBit%d", n)])
			}

			if p == 0x5555555555555555 || p == 0xAAAAAAAAAAAAAAAA || p == 1<<63 {
				for n, path := range paths {
					v, err := sess.ReadValue(ctx, path)
					if err != nil {
						t.Errorf("ReadValue %s: %v", path, err)
						continue
					}
					assertValue(t, path, v, want[fmt.Sprintf("bBit%d", n)])
				}
			}
		})
	}
}

// Each bit of the Go-owned struct written on its own, set from all-zeros and
// cleared from all-ones, reading the whole struct back each time so every
// neighbouring bit is checked too.
func TestSeedBitPackingWriteEachBit(t *testing.T) {
	sess := seedSession(t)
	ctx := context.Background()
	const target = bitsFB + "stBitPackingWrite"
	check := func(label string, want uint64) {
		t.Helper()
		v, err := sess.ReadValue(ctx, target)
		if err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		if got := patternOf(t, v); got != want {
			t.Errorf("%s: struct holds %#016x, want %#016x", label, got, want)
		}
	}
	for _, base := range []uint64{0, math.MaxUint64} {
		if err := sess.WriteValue(ctx, target, bitsOf(base)); err != nil {
			t.Fatalf("write base %#x: %v", base, err)
		}
		check("base", base)
		for n := range 64 {
			path := fmt.Sprintf("%s.bBit%d", target, n)
			flipped := base ^ 1<<n
			if err := sess.WriteValue(ctx, path, flipped>>n&1 == 1); err != nil {
				t.Fatalf("WriteValue %s: %v", path, err)
			}
			check(fmt.Sprintf("bit %d flipped from %#x", n, base), flipped)
			if err := sess.WriteValue(ctx, path, base>>n&1 == 1); err != nil {
				t.Fatalf("WriteValue %s back: %v", path, err)
			}
			check(fmt.Sprintf("bit %d restored", n), base)
		}
	}
}

// Every pattern written whole and as 64 bits in one batch, read back both ways.
func TestSeedBitPackingWritePatterns(t *testing.T) {
	sess := seedSession(t)
	ctx := context.Background()
	const target = bitsFB + "stBitPackingWrite"
	paths := bitPaths(target)
	for _, p := range bitPatterns() {
		t.Run(fmt.Sprintf("%#016x", p), func(t *testing.T) {
			if err := sess.WriteValue(ctx, target, bitsOf(p)); err != nil {
				t.Fatalf("WriteValue whole: %v", err)
			}
			got, err := sess.ReadValues(ctx, paths)
			if err != nil {
				t.Fatalf("ReadValues: %v", err)
			}
			for n, path := range paths {
				assertValue(t, path, got[path], p>>n&1 == 1)
			}

			inverse := ^p
			values := make(map[string]any, 64)
			for n, path := range paths {
				values[path] = inverse>>n&1 == 1
			}
			if _, err := sess.WriteValues(ctx, values); err != nil {
				t.Fatalf("WriteValues bits: %v", err)
			}
			whole, err := sess.ReadValue(ctx, target)
			if err != nil {
				t.Fatal(err)
			}
			if got := patternOf(t, whole); got != inverse {
				t.Errorf("after the batch: %#016x, want %#016x", got, inverse)
			}
		})
	}
}

// A BIT takes a bool and nothing else; a refused write changes nothing.
func TestSeedBitPackingRejects(t *testing.T) {
	sess := seedSession(t)
	ctx := context.Background()
	const target = bitsFB + "stBitPackingWrite"
	const pattern = 0x0123456789ABCDEF
	if err := sess.WriteValue(ctx, target, bitsOf(pattern)); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []any{1, "true", uint8(1)} {
		if err := sess.WriteValue(ctx, target+".bBit5", bad); err == nil {
			t.Errorf("WriteValue bBit5 = %#v: expected an error", bad)
		}
		whole := bitsOf(pattern)
		whole["bBit5"] = bad
		if err := sess.WriteValue(ctx, target, whole); err == nil {
			t.Errorf("WriteValue struct with bBit5 = %#v: expected an error", bad)
		}
	}
	missing := bitsOf(pattern)
	delete(missing, "bBit63")
	if err := sess.WriteValue(ctx, target, missing); err == nil {
		t.Error("struct write missing bBit63: expected an error")
	}
	v, err := sess.ReadValue(ctx, target)
	if err != nil {
		t.Fatal(err)
	}
	if got := patternOf(t, v); got != pattern {
		t.Errorf("after refused writes: %#016x, want %#016x", got, uint64(pattern))
	}
}

// With the seed counting every cycle, each notification of the struct must
// reassemble to a count greater than the one before.
func TestSeedBitPackingNotify(t *testing.T) {
	sess := seedSession(t)
	restoreControl(t, sess, bitsFB)
	ctx := context.Background()
	const start = 1<<32 - 50 // crosses from the low word into the high one
	setBitSeed(t, sess, start)
	ch := subscribe(t, sess, []string{bitsFB + "stBitPacking"}, ams.TransModeServerOnChange, 10*time.Millisecond)
	if _, err := sess.WriteValues(ctx, map[string]any{bitsFB + "nAutoTickInterval": uint32(0), bitsFB + "bAutoMode": true}); err != nil {
		t.Fatal(err)
	}
	var seen []uint64
	deadline := time.After(1500 * time.Millisecond)
collect:
	for {
		select {
		case u := <-ch:
			seen = append(seen, patternOf(t, u.Value))
		case <-deadline:
			break collect
		}
	}
	if err := sess.WriteValue(ctx, bitsFB+"bAutoMode", false); err != nil {
		t.Fatal(err)
	}
	if len(seen) < 50 {
		t.Fatalf("%d updates in 1.5 s, want many more", len(seen))
	}
	for i := 1; i < len(seen); i++ {
		if seen[i] <= seen[i-1] {
			t.Errorf("update %d went from %#x to %#x", i, seen[i-1], seen[i])
			break
		}
	}
	if seen[len(seen)-1] < 1<<32 {
		t.Errorf("never crossed into the high word: last %#x", seen[len(seen)-1])
	}
	t.Logf("%d updates, %#x..%#x", len(seen), seen[0], seen[len(seen)-1])
}
