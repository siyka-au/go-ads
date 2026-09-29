package ams

import "testing"

func TestTransModeText_RoundTrip(t *testing.T) {
	for _, tt := range transModeText {
		b, err := tt.mode.MarshalText()
		if err != nil || string(b) != tt.text {
			t.Errorf("MarshalText(%v) = %q, %v; want %q", tt.mode, b, err, tt.text)
		}
		var got TransMode
		if err := got.UnmarshalText(b); err != nil || got != tt.mode {
			t.Errorf("UnmarshalText(%q) = %v, %v; want %v", b, got, err, tt.mode)
		}
	}
}

func TestTransModeText_CaseInsensitive(t *testing.T) {
	var got TransMode
	if err := got.UnmarshalText([]byte("SERVERONCHANGE2")); err != nil || got != TransModeServerOnChange2 {
		t.Errorf("got %v, %v; want ServerOnChange2", got, err)
	}
}

func TestTransModeText_Unknown(t *testing.T) {
	var got TransMode
	if err := got.UnmarshalText([]byte("sometimes")); err == nil {
		t.Error("expected an error for an unknown mode")
	}
	if _, err := TransMode(99).MarshalText(); err == nil {
		t.Error("expected an error marshalling an unknown mode")
	}
}

func TestDeviceInfoVersion(t *testing.T) {
	if got := (DeviceInfo{Major: 3, Minor: 1, Build: 4024}).Version(); got != "3.1.4024" {
		t.Errorf("Version() = %q", got)
	}
}

func TestStateString(t *testing.T) {
	if StateRun.String() != "Run" || StateConfig.String() != "Config" {
		t.Errorf("got %q, %q", StateRun, StateConfig)
	}
	if State(77).String() != "State(77)" {
		t.Errorf("unknown state = %q", State(77))
	}
}
