package ads

import (
	"testing"

	"github.com/siyka-au/go-ads/v3/ams"
)

// Validates: R-NOT-016.
func TestUpdate_StaleReasonFields(t *testing.T) {
	u := Update{Variable: "x", Value: "1", Stale: &StaleInfo{Reason: ReasonSymbolVersionInvalid}}
	if u.Stale == nil {
		t.Error("Stale field missing")
	}
	if u.Stale.Reason != ReasonSymbolVersionInvalid {
		t.Errorf("Stale.Reason field missing or wrong: %q", u.Stale.Reason)
	}
}

// Validates: R-SES-011.
func TestSymbolVersionStrategy_String(t *testing.T) {
	tests := []struct {
		s    SymbolVersionStrategy
		want string
	}{
		{SymbolVersionAutoReload, "AutoReload"},
		{SymbolVersionClose, "Close"},
		{SymbolVersionIgnore, "Ignore"},
	}
	for _, tt := range tests {
		if got := tt.s.String(); got != tt.want {
			t.Errorf("strategy %d → %q, want %q", tt.s, got, tt.want)
		}
	}
}

// Validates: R-NOT-016 reason enumeration.
func TestStaleReasonConstants(t *testing.T) {
	cases := map[Reason]string{
		ReasonSymbolVersionInvalid: "symbol-version-invalid",
		ReasonSymbolNotFound:       "symbol-not-found",
		ReasonInvalidOffset:        "invalid-offset",
		ReasonSymbolNotActive:      "symbol-not-active",
		ReasonNotifyHandleInvalid:  "notify-handle-invalid",
		ReasonInvalidSize:          "invalid-size",
		ReasonReloadCapExhausted:   "reload-cap-exhausted",
		ReasonReloadInProgress:     "reload-in-progress",
	}
	for got, want := range cases {
		if string(got) != want {
			t.Errorf("constant value drift: got %q, want %q", string(got), want)
		}
	}
}

// Validates: R-CACHE-009 detection set + R-NOT-016 reason mapping.
func TestDetectStaleCache(t *testing.T) {
	tests := []struct {
		rc        ams.ReturnCode
		wantStale bool
		wantReas  Reason
	}{
		// 5 codes in detection set.
		{ams.ReturnCodeDeviceSymbolVersionInvalid, true, ReasonSymbolVersionInvalid}, // 0x711
		{ams.ReturnCodeDeviceSymbolNoFound, true, ReasonSymbolNotFound},              // 0x710
		{ams.ReturnCodeDeviceInvalidOffset, true, ReasonInvalidOffset},               // 0x703
		{ams.ReturnCodeDeviceSymbolNotActive, true, ReasonSymbolNotActive},           // 0x722
		{ams.ReturnCodeDeviceNotifyHandleInvalid, true, ReasonNotifyHandleInvalid},   // 0x714
		{ams.ReturnCodeDeviceInvalidSize, true, ReasonInvalidSize},                   // 0x705
		// Negative cases — must NOT trigger.
		{ams.ReturnCodeNoErrors, false, ""},
		{ams.ReturnCodeDeviceTimeout, false, ""},
		{ams.ReturnCodeDeviceWarning, false, ""}, // 0x720 — explicitly NOT in set (Beckhoff: signal warning)
	}
	for _, tt := range tests {
		stale, reason := detectStaleCache(tt.rc)
		if stale != tt.wantStale || reason != tt.wantReas {
			t.Errorf("detectStaleCache(0x%X) = (%v, %q), want (%v, %q)",
				uint32(tt.rc), stale, reason, tt.wantStale, tt.wantReas)
		}
	}
}
