package ads

import (
	"log/slog"
	"testing"
	"time"
)

// TestConnectedGeneration_OnlyAdvancesOnAConnectOrReconnect guards the property
// the heartbeat reset depends on: the generation must move ONLY when the session
// really re-entered Connected from a connect or reconnect attempt. Anything else
// resets the detector while the session sits Connected, which masks a real stall
// for as long as the other event keeps firing — the reason epoch() cannot be used
// here (bumpEpoch also fires on symbol-cache swaps).
func TestConnectedGeneration_OnlyAdvancesOnAConnectOrReconnect(t *testing.T) {
	tests := []struct {
		name string
		from SessionState
		want uint64
	}{
		{name: "a first connect advances it", from: SessionStateConnecting, want: 1},
		{name: "a reconnect advances it", from: SessionStateReconnecting, want: 1},
		// Reloading -> Connected is in the FSM table but no production path enters
		// Reloading today. If one ever does, an AutoReload cycle must still not
		// advance the generation, or every reload resets the heartbeat detector.
		{name: "a reload does not advance it", from: SessionStateReloading, want: 0},
		{name: "an idempotent re-announcement does not advance it", from: SessionStateConnected, want: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sess := &Session{
				lifecycle: &sessionLifecycle{closedCh: make(chan struct{})},
				logger:    slog.Default(),
			}
			sess.lifecycle.state.value.Store(uint32(tt.from))
			sess.enterConnected()
			if got := sess.lifecycle.state.load(); got != SessionStateConnected {
				t.Fatalf("state after enterConnected() from %v = %v, want Connected", tt.from, got)
			}
			if got := sess.connectedGen(); got != tt.want {
				t.Errorf("connectedGen() after entering Connected from %v = %d, want %d", tt.from, got, tt.want)
			}
		})
	}
}

// TestTrackGoroutine_RefusesAfterClose: registering a background goroutine and
// deciding whether the session is closed must be one atomic step.
//
// A bare isClosed() check before waitGroup.Add is a TOCTOU. Close can run to
// completion in the gap — its Wait returns with the counter at 0 — and the late Add
// is then exactly what sync.WaitGroup reports as "Add called concurrently with
// Wait", which panics the process rather than failing anything gracefully. The
// orphan-delete path reaches this from a USER goroutine (Subscribe ->
// endSubscribe -> replayEarlySamples -> dispatchSample -> tryOrphanDelete), so it
// is not confined to internal timing.
func TestTrackGoroutine_RefusesAfterClose(t *testing.T) {
	sess := &Session{
		logger:    slog.Default(),
		lifecycle: &sessionLifecycle{closedCh: make(chan struct{})},
	}

	ran := make(chan struct{}, 1)
	if !sess.trackGoroutine(func() { ran <- struct{}{} }) {
		t.Fatal("refused to start a goroutine on a live session")
	}
	select {
	case <-ran:
	case <-time.After(2 * time.Second):
		t.Fatal("tracked goroutine never ran")
	}
	sess.lifecycle.waitGroup.Wait()

	sess.markClosed()
	if sess.trackGoroutine(func() { t.Error("goroutine started on a closed session") }) {
		t.Error("trackGoroutine started a goroutine after the session was closed: the Add can land after Close's Wait has " +
			"already returned, which is WaitGroup misuse and panics the process")
	}
}

// TestNextFlapCount closes the dead zone that let a device reset us on a timer
// for ever at the cheapest backoff tier.
//
// Measured on 10.13.37.52, 2026-08-28: 21 consecutive resets at a metronomic ~35s.
// 35s sits between flapWindow (5s) and flapResetWindow (60s), where the old
// classifier neither incremented flapCount nor reset it — so it stayed frozen at 3
// and every cycle waited reconnectBackoff(3) = 1s, burning ~5 accepted sockets
// each time against a device with a small socket table.
func TestNextFlapCount(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name          string
		prev          int
		lastConnected time.Time
		servedNothing bool
		want          int
	}{
		{
			name: "a reset inside the severe window counts double",
			prev: 1, lastConnected: now.Add(-2 * time.Second), want: 3,
		},
		{
			name: "35s — the old dead zone — now counts as a flap",
			prev: 3, lastConnected: now.Add(-35 * time.Second), want: 4,
		},
		{
			name: "just under the reset window still counts",
			prev: 0, lastConnected: now.Add(-59 * time.Second), want: 1,
		},
		{
			name: "a connection that outlived the reset window clears the count",
			prev: 7, lastConnected: now.Add(-2 * time.Minute), want: 0,
		},
		{
			name: "no previous Connected and something was served: unchanged",
			prev: 2, lastConnected: time.Time{}, servedNothing: false, want: 2,
		},
		{
			name: "no previous Connected and nothing served: still a flap",
			prev: 2, lastConnected: time.Time{}, servedNothing: true, want: 3,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := nextFlapCount(tc.prev, tc.lastConnected, now, tc.servedNothing); got != tc.want {
				t.Errorf("nextFlapCount(prev=%d, servedNothing=%v) = %d, want %d",
					tc.prev, tc.servedNothing, got, tc.want)
			}
		})
	}
}

// TestNextFlapCount_MetronomeEscalates walks the measured scenario forward: a
// device that resets every 35s must reach the slow tiers instead of sitting on the
// first one. This is the regression that matters — the arithmetic above is only
// interesting because of what it does over many cycles.
func TestNextFlapCount_MetronomeEscalates(t *testing.T) {
	sess := &Session{lifecycle: &sessionLifecycle{backoffConfig: DefaultBackoffConfig()}}
	count := 0
	now := time.Now()
	var delays []time.Duration
	for cycle := 0; cycle < 12; cycle++ {
		// Each connection lives 35s, then resets, exactly as measured.
		now = now.Add(35 * time.Second)
		count = nextFlapCount(count, now.Add(-35*time.Second), now, false)
		sess.lifecycle.flapCount = count
		delays = append(delays, sess.reconnectBackoff(count))
	}
	first, last := delays[0], delays[len(delays)-1]
	if last <= first {
		t.Errorf("backoff did not escalate across 12 metronomic resets: first=%v last=%v (all=%v)",
			first, last, delays)
	}
	// The whole point: it must leave the cheapest tier well before cycle 12.
	if delays[len(delays)-1] < 5*time.Second {
		t.Errorf("after 12 flaps the cooldown is still %v; the loop is still burning sockets every cycle", last)
	}
}
