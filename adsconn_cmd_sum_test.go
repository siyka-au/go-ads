package ads

import (
	"testing"

	"github.com/siyka-au/go-ads/v3/ams"
)

// --- downgradeTransMode ---

// Validates: R-NOT-011.
func TestDowngradeTransMode(t *testing.T) {
	tests := []struct {
		input    ams.TransMode
		expected ams.TransMode
	}{
		{ams.TransModeServerOnChange2, ams.TransModeServerOnChange},
		{ams.TransModeServerCycle2, ams.TransModeServerCycle},
		{ams.TransModeServerOnChange, ams.TransModeServerOnChange},
		{ams.TransModeServerCycle, ams.TransModeServerCycle},
		{ams.TransModeClientCycle, ams.TransModeClientCycle},
		{ams.TransModeNoTransmission, ams.TransModeNoTransmission},
	}

	for _, tt := range tests {
		t.Run(tt.input.String(), func(t *testing.T) {
			result := downgradeTransMode(tt.input)
			if result != tt.expected {
				t.Errorf("downgradeTransMode(%v) = %v, want %v", tt.input, result, tt.expected)
			}
		})
	}
}
