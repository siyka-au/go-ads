package logging

import (
	"log/slog"
	"testing"
)

func TestHexAttr(t *testing.T) {
	attr := HexAttr("data", []byte{0xDE, 0xAD, 0xBE, 0xEF})
	if attr.Key != "data" {
		t.Errorf("key = %q, want %q", attr.Key, "data")
	}
	if s := attr.Value.String(); s != "deadbeef" {
		t.Errorf("hex string = %q, want deadbeef", s)
	}
}

func TestHexAttr_Empty(t *testing.T) {
	attr := HexAttr("empty", []byte{})
	if attr.Key != "empty" {
		t.Errorf("key = %q, want %q", attr.Key, "empty")
	}
}

func TestOr(t *testing.T) {
	if Or(nil) != slog.Default() {
		t.Error("Or(nil) should fall back to slog.Default()")
	}
	lg := slog.New(slog.DiscardHandler)
	if Or(lg) != lg {
		t.Error("Or(lg) should return lg")
	}
}
