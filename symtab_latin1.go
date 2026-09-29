package ads

import "fmt"

// A TwinCAT STRING holds one byte per character in Latin-1 (ISO 8859-1): byte
// N is Unicode U+00N. That is what TwinCAT itself does -- STRING_TO_WSTRING
// maps 0x80-0x9F to U+0080-U+009F, and WSTRING_TO_STRING and the compiler keep
// only the low byte of a wider character, so a literal '€' is stored as 0xAC --
// not Windows-1252, which would put € and typographic quotes at 0x80-0x9F.
// Go strings are UTF-8, so STRINGs are converted both ways.

// latin1Decode converts Latin-1 bytes to a UTF-8 Go string.
func latin1Decode(b []byte) string {
	runes := make([]rune, len(b))
	for i, c := range b {
		runes[i] = rune(c)
	}
	return string(runes)
}

// latin1Encode converts a UTF-8 Go string to Latin-1 bytes, refusing a
// character above U+00FF rather than truncating it as TwinCAT would.
func latin1Encode(s string) ([]byte, error) {
	out := make([]byte, 0, len(s))
	for i, r := range s {
		if r > 0xFF {
			return nil, fmt.Errorf("character %q at byte %d of %q is not in Latin-1 (ISO 8859-1), the character set of a STRING", r, i, s)
		}
		out = append(out, byte(r))
	}
	return out, nil
}
