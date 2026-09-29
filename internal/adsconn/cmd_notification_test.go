package adsconn

import (
	"math"
	"testing"
	"time"
)

func TestDurationToADSTicks(t *testing.T) {
	tests := []struct {
		name    string
		d       time.Duration
		wantErr bool
		want    uint32
	}{
		{"zero", 0, false, 0},
		{"1ms", time.Millisecond, false, 10_000},
		{"max valid", time.Duration(math.MaxUint32) * 100 * time.Nanosecond, false, math.MaxUint32},
		{"negative", -time.Millisecond, true, 0},
		{"overflow", time.Duration(math.MaxUint32)*100*time.Nanosecond + 100*time.Nanosecond, true, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := durationToADSTicks(tt.d, "test")
			if (err != nil) != tt.wantErr {
				t.Fatalf("durationToADSTicks(%v) error = %v, wantErr %v", tt.d, err, tt.wantErr)
			}
			if !tt.wantErr && got != tt.want {
				t.Errorf("durationToADSTicks(%v) = %d, want %d", tt.d, got, tt.want)
			}
		})
	}
}
