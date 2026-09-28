//go:build integration

package ads

// Struct layout tests against Main.fbStructTest: the same four members
// (BOOL, DWORD, BOOL, LWORD) under pack_mode 0, 2, 4 and 8, so each member sits
// at a different offset in each struct. Decoding and encoding must follow the
// offsets the PLC reports rather than assume a packing.

import (
	"context"
	"fmt"
	"math"
	"testing"
	"time"
)

var packStructs = []struct {
	name    string
	size    uint32
	offsets map[string]uint32
}{
	{"stPackNone", 14, map[string]uint32{"bBoolMember1": 0, "nDwordMember": 1, "bBoolMember2": 5, "nLwordMember": 6}},
	{"stPackTwo", 16, map[string]uint32{"bBoolMember1": 0, "nDwordMember": 2, "bBoolMember2": 6, "nLwordMember": 8}},
	{"stPackFour", 20, map[string]uint32{"bBoolMember1": 0, "nDwordMember": 4, "bBoolMember2": 8, "nLwordMember": 12}},
	{"stPackEight", 24, map[string]uint32{"bBoolMember1": 0, "nDwordMember": 4, "bBoolMember2": 8, "nLwordMember": 16}},
}

// packExpected is what FB_StructTest computes from its nSeed.
func packExpected(seed uint32) map[string]any {
	return map[string]any{
		"bBoolMember1": seed%2 == 0,
		"nDwordMember": seed,
		"bBoolMember2": seed%3 == 0,
		"nLwordMember": uint64(seed) * 3,
	}
}

// setStructSeed writes FB_StructTest.nSeed, waits for the PLC to apply it, and
// restores the original when the test ends.
func setStructSeed(t *testing.T, sess *Session, seed uint32) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := sess.WriteValue(ctx, structFB+"nSeed", seed); err != nil {
		t.Fatalf("write nSeed: %v", err)
	}
	for {
		v, err := sess.ReadValue(ctx, structFB+"stPackEight.nDwordMember")
		if err == nil && v == seed {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("struct seed %d not applied: %v, %v", seed, v, err)
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func restoreStructSeed(t *testing.T, sess *Session) {
	t.Helper()
	saved, err := sess.ReadValue(context.Background(), structFB+"nSeed")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sess.WriteValue(context.Background(), structFB+"nSeed", saved) })
}

// The size and member offsets the PLC reports for each pack mode.
func TestSeedStructPackingLayout(t *testing.T) {
	sess := seedSession(t)
	for _, p := range packStructs {
		for _, target := range []string{p.name, p.name + "Write"} {
			v, err := sess.GetSymbol(context.Background(), structFB+target)
			if err != nil {
				t.Errorf("GetSymbol %s: %v", target, err)
				continue
			}
			if v.Length != p.size {
				t.Errorf("%s: Length %d, want %d", target, v.Length, p.size)
			}
			children := v.Children()
			if len(children) != len(p.offsets) {
				t.Errorf("%s: %d members, want %d", target, len(children), len(p.offsets))
			}
			for member, off := range p.offsets {
				c, ok := children[member]
				if !ok {
					t.Errorf("%s: member %s missing", target, member)
					continue
				}
				if c.Offset != off {
					t.Errorf("%s.%s: offset %d, want %d", target, member, c.Offset, off)
				}
			}
		}
	}
}

// Values the PLC computes, read as whole structs (alone and in one batch) and
// member by member.
func TestSeedStructPackingRead(t *testing.T) {
	sess := seedSession(t)
	restoreStructSeed(t, sess)
	ctx := context.Background()
	names := make([]string, len(packStructs))
	for i, p := range packStructs {
		names[i] = structFB + p.name
	}
	for _, seed := range []uint32{0, 1, 2, 3, 6, 100, 0x55555555, math.MaxUint32} {
		t.Run(fmt.Sprint(seed), func(t *testing.T) {
			setStructSeed(t, sess, seed)
			want := packExpected(seed)
			batch, err := sess.ReadValues(ctx, names)
			if err != nil {
				t.Fatalf("ReadValues: %v", err)
			}
			for _, p := range packStructs {
				assertValue(t, p.name+" (batch)", batch[structFB+p.name], want)
				whole, err := sess.ReadValue(ctx, structFB+p.name)
				if err != nil {
					t.Errorf("ReadValue %s: %v", p.name, err)
				} else {
					assertValue(t, p.name, whole, want)
				}
				for member, w := range want {
					got, err := sess.ReadValue(ctx, structFB+p.name+"."+member)
					if err != nil {
						t.Errorf("ReadValue %s.%s: %v", p.name, member, err)
						continue
					}
					assertValue(t, p.name+"."+member, got, w)
				}
			}
		})
	}
}

// Whole structs written to the Go-owned targets, read back member by member
// and whole.
func TestSeedStructPackingWrite(t *testing.T) {
	sess := seedSession(t)
	ctx := context.Background()
	sets := []map[string]any{
		{"bBoolMember1": true, "nDwordMember": uint32(math.MaxUint32), "bBoolMember2": true, "nLwordMember": uint64(math.MaxUint64)},
		{"bBoolMember1": false, "nDwordMember": uint32(0), "bBoolMember2": false, "nLwordMember": uint64(0)},
		{"bBoolMember1": true, "nDwordMember": uint32(0x01020304), "bBoolMember2": false, "nLwordMember": uint64(0x0102030405060708)},
		{"bBoolMember1": false, "nDwordMember": uint32(0xA5A5A5A5), "bBoolMember2": true, "nLwordMember": uint64(0x5A5A5A5A5A5A5A5A)},
	}
	for i, set := range sets {
		for _, p := range packStructs {
			target := structFB + p.name + "Write"
			t.Run(fmt.Sprintf("%s/%d", p.name, i), func(t *testing.T) {
				if err := sess.WriteValue(ctx, target, set); err != nil {
					t.Fatalf("WriteValue: %v", err)
				}
				for member, w := range set {
					got, err := sess.ReadValue(ctx, target+"."+member)
					if err != nil {
						t.Errorf("ReadValue %s: %v", member, err)
						continue
					}
					assertValue(t, member, got, w)
				}
				whole, err := sess.ReadValue(ctx, target)
				if err != nil {
					t.Fatal(err)
				}
				assertValue(t, "whole", whole, set)
			})
		}
	}
}
