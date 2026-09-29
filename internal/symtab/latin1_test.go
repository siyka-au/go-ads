package symtab

import (
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

// Every non-NUL byte decodes to the character with that code point and encodes
// back to it.
func TestLatin1EveryByteRoundTrips(t *testing.T) {
	for b := 1; b <= 0xFF; b++ {
		s := latin1Decode([]byte{byte(b)})
		if r, _ := utf8.DecodeRuneInString(s); r != rune(b) || utf8.RuneCountInString(s) != 1 {
			t.Errorf("%#02x decodes to %q, want U+%04X", b, s, b)
			continue
		}
		back, err := latin1Encode(s)
		if err != nil || !reflect.DeepEqual(back, []byte{byte(b)}) {
			t.Errorf("%#02x -> %q -> % x, %v", b, s, back, err)
		}
	}
}

func TestSTRINGEncodesLatin1(t *testing.T) {
	got, err := encodeScalar("STRING", "Grüße ©", 10)
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{'G', 'r', 0xFC, 0xDF, 'e', ' ', 0xA9, 0, 0, 0}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got % x, want % x", got, want)
	}
	back, err := decodeScalar("STRING", got)
	if err != nil || back != "Grüße ©" {
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

// Anything above U+00FF is refused -- including €, which Windows-1252 has but
// TwinCAT's STRING does not.
func TestSTRINGRefusesWhatLatin1CannotHold(t *testing.T) {
	for _, s := range []string{"€", "“quoted”", "中", "😀", "✓", "Ā", "abcĀ", string([]byte{0xFF, 0xFE})} {
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
