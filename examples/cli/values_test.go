package main

import (
	"testing"
	"time"

	"cloud.google.com/go/civil"
)

func TestParseLike(t *testing.T) {
	tests := []struct {
		like any
		text string
		want any
	}{
		{false, "true", true},
		{int8(0), "-128", int8(-128)},
		{int16(0), "0x7fff", int16(32767)},
		{uint32(0), "4294967295", uint32(4294967295)},
		{float32(0), "1.5", float32(1.5)},
		{0.0, "-2.25", -2.25},
		{"", "hello world", "hello world"},
		{time.Duration(0), "25h1m1.001s", 25*time.Hour + time.Minute + time.Second + time.Millisecond},
		{civil.Time{}, "23:59:59.999", civil.Time{Hour: 23, Minute: 59, Second: 59, Nanosecond: 999_000_000}},
		{civil.Date{}, "2024-02-29", civil.Date{Year: 2024, Month: 2, Day: 29}},
		{civil.DateTime{}, "2024-06-15T13:30:00", civil.DateTime{Date: civil.Date{Year: 2024, Month: 6, Day: 15}, Time: civil.Time{Hour: 13, Minute: 30}}},
	}
	for _, tt := range tests {
		got, err := parseLike(tt.like, tt.text)
		if err != nil || got != tt.want {
			t.Errorf("parseLike(%T, %q) = %#v, %v; want %#v", tt.like, tt.text, got, err, tt.want)
		}
	}
	for _, bad := range []struct {
		like any
		text string
	}{
		{int8(0), "128"},
		{uint16(0), "-1"},
		{false, "yes please"},
		{civil.Date{}, "2024-02-30"},
		{map[string]any{}, "{}"},
	} {
		if _, err := parseLike(bad.like, bad.text); err == nil {
			t.Errorf("parseLike(%T, %q): expected an error", bad.like, bad.text)
		}
	}
}

func TestFormatValue(t *testing.T) {
	for v, want := range map[any]string{
		"S=7":            `"S=7"`,
		int16(-5):        "-5",
		90 * time.Minute: "1h30m0s",
		civil.Date{Year: 2024, Month: 2, Day: 29}: "2024-02-29",
	} {
		if got := formatValue(v); got != want {
			t.Errorf("formatValue(%#v) = %q, want %q", v, got, want)
		}
	}
}
