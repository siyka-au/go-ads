package ads

import (
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

// Every non-NUL byte decodes to one character that encodes back to it.
func TestCP1252EveryByteRoundTrips(t *testing.T) {
	for b := 1; b <= 0xFF; b++ {
		s := cp1252Decode([]byte{byte(b)})
		if utf8.RuneCountInString(s) != 1 {
			t.Errorf("%#02x decodes to %q, want one character", b, s)
			continue
		}
		back, err := cp1252Encode(s)
		if err != nil || !reflect.DeepEqual(back, []byte{byte(b)}) {
			t.Errorf("%#02x -> %q -> % x, %v", b, s, back, err)
		}
	}
}

func TestCP1252KnownMappings(t *testing.T) {
	for b, want := range map[byte]rune{
		0x41: 'A', 0x80: '€', 0x8A: 'Š', 0x93: '“', 0x99: '™', 0x9F: 'Ÿ',
		0xA0: ' ', 0xA9: '©', 0xC4: 'Ä', 0xDF: 'ß', 0xE9: 'é', 0xFF: 'ÿ',
		0x81: '\u0081', 0x9D: '\u009D', // undefined in Windows-1252: the C1 control, as Windows maps it
	} {
		if got := []rune(cp1252Decode([]byte{b})); len(got) != 1 || got[0] != want {
			t.Errorf("%#02x decodes to %q, want %q", b, string(got), want)
		}
	}
}

func TestSTRINGEncodesWindows1252(t *testing.T) {
	got, err := encodeScalar("STRING", "Grüße €", 10)
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{'G', 'r', 0xFC, 0xDF, 'e', ' ', 0x80, 0, 0, 0}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got % x, want % x", got, want)
	}
	back, err := decodeScalar("STRING", got)
	if err != nil || back != "Grüße €" {
		t.Errorf("decoded back to %q, %v", back, err)
	}
}

// A STRING(n) holds n characters, whatever their UTF-8 length in Go.
func TestSTRINGLengthCountsCharacters(t *testing.T) {
	// STRING(5): 5 characters and the terminator.
	if _, err := encodeScalar("STRING", "Grüße", 6); err != nil {
		t.Errorf("5 characters (7 UTF-8 bytes) in STRING(5): %v", err)
	}
	if _, err := encodeScalar("STRING", "Grüßen", 6); err == nil {
		t.Error("6 characters in STRING(5): expected an error")
	}
	if _, err := encodeScalar("STRING", strings.Repeat("é", 255), 256); err != nil {
		t.Errorf("255 é in STRING(255): %v", err)
	}
}

func TestSTRINGRefusesWhatWindows1252CannotHold(t *testing.T) {
	for _, s := range []string{"中", "😀", "✓", "Ā", "abcĀ", string([]byte{0xFF, 0xFE})} {
		if _, err := encodeScalar("STRING", s, 256); err == nil {
			t.Errorf("STRING %q: expected an error", s)
		}
	}
}

func TestWSTRINGRefusesInvalidUTF8(t *testing.T) {
	if _, err := encodeScalar("WSTRING", string([]byte{'a', 0xFF}), 20); err == nil {
		t.Error("expected an error")
	}
}

// Characters beyond the Basic Multilingual Plane are two UTF-16 code units.
func TestWSTRINGSurrogatePairs(t *testing.T) {
	got, err := encodeScalar("WSTRING", "a😀", 10)
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{'a', 0, 0x3D, 0xD8, 0x00, 0xDE, 0, 0, 0, 0}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got % x, want % x", got, want)
	}
	if back, _ := decodeScalar("WSTRING", got); back != "a😀" {
		t.Errorf("decoded back to %q", back)
	}
}
