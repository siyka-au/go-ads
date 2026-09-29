// Package logging holds the slog helpers shared by go-ads packages.
package logging

import (
	"encoding/hex"
	"log/slog"
)

// LevelTrace is a custom slog level for trace-level logging,
// below slog.LevelDebug (-4). Matches zerolog's Trace level semantics.
const LevelTrace = slog.Level(-8)

// HexAttr returns a slog.Attr that formats a byte slice as a hex string.
// Equivalent to zerolog's .Hex("key", data) method.
func HexAttr(key string, data []byte) slog.Attr {
	return slog.String(key, hex.EncodeToString(data))
}

// Or returns lg, or slog.Default() when lg is nil. Library code logs through
// it wherever a logger is optional, so a zero-value struct never panics.
func Or(lg *slog.Logger) *slog.Logger {
	if lg != nil {
		return lg
	}
	return slog.Default()
}
