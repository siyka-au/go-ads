//go:build integration

package ads

// String tests against Main.fbStringTest. Go writes sStringVar,
// sShortStringVar and wsWStringVar; the PLC reports what it sees in them each
// cycle -- its own LEN/WLEN, the raw bytes and code units (MEMCPY), and
// Beckhoff's own STRING<->WSTRING conversions -- and produces text of its own
// (literals, and every non-NUL byte converted by STRING_TO_WSTRING). So the
// encoding is checked against the PLC, not against this library.

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"unicode/utf16"
	"unicode/utf8"
)

const strFB = "Main.fbStringTest."

// allLatin1 is every non-NUL Latin-1 character, U+0001-U+00FF in order.
func allLatin1() string {
	b := make([]byte, 255)
	for i := range b {
		b[i] = byte(i + 1)
	}
	return latin1Decode(b)
}

// Beckhoff's STRING_TO_WSTRING of every byte gives the same character this
// library decodes that byte to: Latin-1, byte N is U+00N.
func TestSeedStringPLCMapping(t *testing.T) {
	sess := seedSession(t)
	got, err := sess.ReadValues(context.Background(), []string{strFB + "sAllBytes", strFB + "wsAllBytes"})
	if err != nil {
		t.Fatal(err)
	}
	ours := []rune(got[strFB+"sAllBytes"].(string))
	beckhoff := []rune(got[strFB+"wsAllBytes"].(string))
	if len(ours) != 255 || len(beckhoff) != 255 {
		t.Fatalf("got %d characters from sAllBytes and %d from wsAllBytes, want 255 each", len(ours), len(beckhoff))
	}
	for i := range 255 {
		if ours[i] != beckhoff[i] {
			t.Errorf("byte %#02x: go-ads decodes %U, STRING_TO_WSTRING gives %U", i+1, ours[i], beckhoff[i])
		}
	}
	if string(ours) != allLatin1() {
		t.Error("sAllBytes does not decode to U+0001-U+00FF in order")
	}
}

// Text typed into TwinCAT reads back as the text the PLC holds. The compiler
// keeps only the low byte of a STRING literal's character: the '€' (U+20AC)
// in sLatinLiteral is stored as 0xAC, which is '¬'.
func TestSeedStringLiterals(t *testing.T) {
	sess := seedSession(t)
	for name, want := range map[string]string{
		"sLatinLiteral": "Grüße ¬ ÄÖÜ ñ ©",
		"wsLiteral":     "Grüße € 日本語",
	} {
		v, err := sess.ReadValue(context.Background(), strFB+name)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		assertValue(t, name, v, want)
	}
}

func bytesAny(b []byte, n int) []any {
	out := make([]any, n)
	for i := range out {
		out[i] = uint8(0)
		if i < len(b) {
			out[i] = b[i]
		}
	}
	return out
}

func wordsAny(u []uint16, n int) []any {
	out := make([]any, n)
	for i := range out {
		out[i] = uint16(0)
		if i < len(u) {
			out[i] = u[i]
		}
	}
	return out
}

// prefixOf reads an array and returns its first n elements.
func prefixOf(sess *Session, name string, n int) func() (any, error) {
	return func() (any, error) {
		v, err := sess.ReadValue(context.Background(), name)
		if err != nil {
			return nil, err
		}
		return v.([]any)[:n], nil
	}
}

var latin1Cases = []string{"", "A", "Grüße ©", printableASCII(), allLatin1(), strings.Repeat("é", 255), "¡¿ Ñandú ÿ ß Æ Ø Þ µ ± ½ ×÷"}

// A string written from Go: the PLC stores it as Latin-1, counts the
// characters Go meant, and its own conversion to WSTRING recovers the text.
func TestSeedStringWrite(t *testing.T) {
	sess := seedSession(t)
	ctx := context.Background()
	for i, s := range latin1Cases {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			if err := sess.WriteValue(ctx, strFB+"sStringVar", s); err != nil {
				t.Fatalf("WriteValue %q: %v", s, err)
			}
			n := utf8.RuneCountInString(s)
			enc, _ := latin1Encode(s)
			// The characters, then the terminator; the rest of the buffer holds
			// whatever an earlier, longer write left.
			waitForValue(t, "stored bytes", bytesAny(enc, n+1), prefixOf(sess, strFB+"aStringBytes", n+1))
			waitForValue(t, "nStringLen", int16(n), func() (any, error) { return sess.ReadValue(ctx, strFB+"nStringLen") })
			waitForValue(t, "STRING_TO_WSTRING", s, func() (any, error) { return sess.ReadValue(ctx, strFB+"wsFromString") })
			back, err := sess.ReadValue(ctx, strFB+"sStringVar")
			if err != nil {
				t.Fatal(err)
			}
			assertValue(t, "sStringVar read back", back, s)
		})
	}
}

// STRING(5) holds five characters, however many bytes they are in UTF-8.
func TestSeedStringLengthLimit(t *testing.T) {
	sess := seedSession(t)
	ctx := context.Background()
	const short = strFB + "sShortStringVar"
	if err := sess.WriteValue(ctx, short, "Grüße"); err != nil {
		t.Fatalf("5 characters in STRING(5): %v", err)
	}
	waitForValue(t, "nShortStringLen", int16(5), func() (any, error) { return sess.ReadValue(ctx, strFB+"nShortStringLen") })
	for _, bad := range []string{"Grüßen", "abcdef"} {
		if err := sess.WriteValue(ctx, short, bad); err == nil {
			t.Errorf("%q in STRING(5): expected an error", bad)
		}
	}
	for _, bad := range []string{strings.Repeat("x", 256), strings.Repeat("é", 256)} {
		if err := sess.WriteValue(ctx, strFB+"sStringVar", bad); err == nil {
			t.Errorf("%d characters in STRING(255): expected an error", utf8.RuneCountInString(bad))
		}
	}
	v, _ := sess.ReadValue(ctx, short)
	assertValue(t, "sShortStringVar after refused writes", v, "Grüße")
}

// Characters above U+00FF are refused -- including €, which TwinCAT would
// truncate to '¬' -- and the PLC keeps its value.
func TestSeedStringRefusesOutsideLatin1(t *testing.T) {
	sess := seedSession(t)
	ctx := context.Background()
	if err := sess.WriteValue(ctx, strFB+"sStringVar", "before"); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"€", "“quoted”", "中", "😀", "✓", "Ā", "ok but 日本"} {
		if err := sess.WriteValue(ctx, strFB+"sStringVar", bad); err == nil {
			t.Errorf("%q: expected an error", bad)
		}
	}
	v, _ := sess.ReadValue(ctx, strFB+"sStringVar")
	assertValue(t, "sStringVar after refused writes", v, "before")
}

// A WSTRING written from Go: the PLC stores exactly its UTF-16 code units,
// including both halves of a surrogate pair, and counts them with WLEN.
func TestSeedWStringWrite(t *testing.T) {
	sess := seedSession(t)
	ctx := context.Background()
	cases := []string{"", "A", "Grüße €", "日本語", "a😀b", "😀", strings.Repeat("é", 255), strings.Repeat("😀", 127) + "x", allLatin1(), "“quoted” – dash … ‰ ™ Œ Ÿ"}
	for i, s := range cases {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			if err := sess.WriteValue(ctx, strFB+"wsWStringVar", s); err != nil {
				t.Fatalf("WriteValue %q: %v", s, err)
			}
			units := utf16.Encode([]rune(s))
			waitForValue(t, "stored code units", wordsAny(units, len(units)+1), prefixOf(sess, strFB+"aWStringWords", len(units)+1))
			waitForValue(t, "nWStringLen", int16(len(units)), func() (any, error) { return sess.ReadValue(ctx, strFB+"nWStringLen") })
			back, err := sess.ReadValue(ctx, strFB+"wsWStringVar")
			if err != nil {
				t.Fatal(err)
			}
			assertValue(t, "wsWStringVar read back", back, s)
		})
	}
}

// WSTRING(255) holds 255 UTF-16 code units: a character beyond the Basic
// Multilingual Plane takes two.
func TestSeedWStringLengthLimit(t *testing.T) {
	sess := seedSession(t)
	ctx := context.Background()
	for _, bad := range []string{strings.Repeat("é", 256), strings.Repeat("😀", 128)} {
		if err := sess.WriteValue(ctx, strFB+"wsWStringVar", bad); err == nil {
			t.Errorf("%d code units in WSTRING(255): expected an error", len(utf16.Encode([]rune(bad))))
		}
	}
}

// Beckhoff's WSTRING_TO_STRING agrees with this library on everything Latin-1
// holds. Beyond it, TwinCAT keeps the low byte ('€' U+20AC becomes '¬' 0xAC),
// where this library refuses; pinned here so a change in TwinCAT shows up.
func TestSeedWStringToStringOnPLC(t *testing.T) {
	sess := seedSession(t)
	ctx := context.Background()
	check := func(in, want string) {
		t.Helper()
		if err := sess.WriteValue(ctx, strFB+"wsWStringVar", in); err != nil {
			t.Fatal(err)
		}
		waitForValue(t, fmt.Sprintf("WSTRING_TO_STRING(%q)", in), want, func() (any, error) { return sess.ReadValue(ctx, strFB+"sFromWString") })
	}
	for _, s := range latin1Cases {
		check(s, s)
	}
	check("€ “ ”", "¬ \x1c \x1d")
}
