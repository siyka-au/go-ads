package ads

import "fmt"

// A TwinCAT STRING holds one byte per character in Windows-1252, the Windows
// code page for Western European text. It matches Unicode for 0x00-0x7F and
// 0xA0-0xFF (Latin-1) and differs only in 0x80-0x9F, where Windows-1252 has
// the euro sign, typographic quotes and a few letters instead of controls.
// Go strings are UTF-8, so STRINGs are converted both ways.

// cp1252High maps bytes 0x80-0x9F to Unicode. The five bytes Windows-1252
// leaves undefined (0x81, 0x8D, 0x8F, 0x90, 0x9D) map to the C1 control of the
// same value, as Windows' own conversion does, so every byte round-trips.
var cp1252High = [32]rune{
	0x20AC, 0x0081, 0x201A, 0x0192, 0x201E, 0x2026, 0x2020, 0x2021, // 80-87
	0x02C6, 0x2030, 0x0160, 0x2039, 0x0152, 0x008D, 0x017D, 0x008F, // 88-8F
	0x0090, 0x2018, 0x2019, 0x201C, 0x201D, 0x2022, 0x2013, 0x2014, // 90-97
	0x02DC, 0x2122, 0x0161, 0x203A, 0x0153, 0x009D, 0x017E, 0x0178, // 98-9F
}

// cp1252Byte is the reverse of cp1252High.
var cp1252Byte = func() map[rune]byte {
	m := make(map[rune]byte, len(cp1252High))
	for i, r := range cp1252High {
		m[r] = byte(0x80 + i)
	}
	return m
}()

// cp1252Decode converts Windows-1252 bytes to a UTF-8 Go string.
func cp1252Decode(b []byte) string {
	runes := make([]rune, len(b))
	for i, c := range b {
		if c >= 0x80 && c <= 0x9F {
			runes[i] = cp1252High[c-0x80]
		} else {
			runes[i] = rune(c)
		}
	}
	return string(runes)
}

// cp1252Encode converts a UTF-8 Go string to Windows-1252 bytes, refusing a
// character the code page cannot hold rather than substituting one.
func cp1252Encode(s string) ([]byte, error) {
	out := make([]byte, 0, len(s))
	for i, r := range s {
		switch b, ok := cp1252Byte[r]; {
		case ok:
			out = append(out, b)
		case r < 0x80 || (r >= 0xA0 && r <= 0xFF):
			out = append(out, byte(r))
		default:
			return nil, fmt.Errorf("character %q at byte %d of %q is not in Windows-1252, the character set of a STRING", r, i, s)
		}
	}
	return out, nil
}
