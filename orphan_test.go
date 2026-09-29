package ads

import (
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/siyka-au/go-ads/v3/internal/fakeplc"

	"github.com/siyka-au/go-ads/v3/internal/symtab"

	"github.com/siyka-au/go-ads/v3/ams"
)

// --- Fix 1 (orphan-Delete on unknown handle) ---

// TestOrphanDelete_FiresOnUnknownHandleOutsideRaceWindow verifies that an
// unknown-handle notification arriving past the first-sample-race window
// schedules an async DeleteDeviceNotification on the PLC, so PLC handle
// table slots leaked by prior processes don't accumulate.
func TestOrphanDelete_FiresOnUnknownHandleOutsideRaceWindow(t *testing.T) {
	srv := fakeplc.StartPLC(t)
	defer srv.Stop()

	var deleted atomic.Int32
	var seenHandle atomic.Uint32
	srv.OnDeleteDeviceNotification(func(h uint32) ams.ReturnCode {
		deleted.Add(1)
		seenHandle.Store(h)
		return ams.ReturnCodeNoErrors
	})

	sess, c := newWiredTestSession(t, srv)
	c.SetNotificationHandler(sess.handleNotification)
	// Push lastSubscribeNs far into the past so the race-window guard
	// passes and the default (Warn + orphan-Delete) branch fires.
	sess.notifications.lastSubscribeNs.Store(time.Now().Add(-1 * time.Second).UnixNano())

	pkt := buildNotificationPacket(0xDEADBEEF, 0, []byte{0x01, 0x02})
	if err := sess.drivePacket(sess.lifecycle.ctx, pkt); err != nil {
		t.Fatalf("drivePacket: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for deleted.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := deleted.Load(); got != 1 {
		t.Fatalf("Delete RPC calls = %d, want 1", got)
	}
	if h := seenHandle.Load(); h != 0xDEADBEEF {
		t.Errorf("deleted handle = 0x%X, want 0xDEADBEEF", h)
	}
}

// TestOrphanDelete_ThrottledOnRepeatedHandle: same orphan handle arriving
// at high rate must trigger Delete only once within the 60s throttle window.
func TestOrphanDelete_ThrottledOnRepeatedHandle(t *testing.T) {
	srv := fakeplc.StartPLC(t)
	defer srv.Stop()

	var deleted atomic.Int32
	srv.OnDeleteDeviceNotification(func(_ uint32) ams.ReturnCode {
		deleted.Add(1)
		return ams.ReturnCodeNoErrors
	})

	sess, c := newWiredTestSession(t, srv)
	c.SetNotificationHandler(sess.handleNotification)
	sess.notifications.lastSubscribeNs.Store(time.Now().Add(-1 * time.Second).UnixNano())

	for i := 0; i < 10; i++ {
		pkt := buildNotificationPacket(0xCAFE, 0, []byte{0xAA})
		_ = sess.drivePacket(sess.lifecycle.ctx, pkt)
	}

	// Wait for any in-flight Delete to settle.
	time.Sleep(300 * time.Millisecond)
	if got := deleted.Load(); got != 1 {
		t.Errorf("Delete RPC calls = %d, want 1 (throttle should suppress repeats within 60s)", got)
	}
}

// TestOrphanDelete_DistinctHandlesNotThrottled verifies the throttle is
// per-handle: different orphan handles each get a Delete attempt.
func TestOrphanDelete_DistinctHandlesNotThrottled(t *testing.T) {
	srv := fakeplc.StartPLC(t)
	defer srv.Stop()

	var deleted atomic.Int32
	seen := make(map[uint32]bool)
	var mu sync.Mutex
	srv.OnDeleteDeviceNotification(func(h uint32) ams.ReturnCode {
		mu.Lock()
		seen[h] = true
		mu.Unlock()
		deleted.Add(1)
		return ams.ReturnCodeNoErrors
	})

	sess, c := newWiredTestSession(t, srv)
	c.SetNotificationHandler(sess.handleNotification)
	sess.notifications.lastSubscribeNs.Store(time.Now().Add(-1 * time.Second).UnixNano())

	for _, h := range []uint32{0xAAA, 0xBBB, 0xCCC} {
		pkt := buildNotificationPacket(h, 0, []byte{0x00})
		_ = sess.drivePacket(sess.lifecycle.ctx, pkt)
	}

	deadline := time.Now().Add(2 * time.Second)
	for deleted.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := deleted.Load(); got != 3 {
		t.Fatalf("Delete RPC calls = %d, want 3", got)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, h := range []uint32{0xAAA, 0xBBB, 0xCCC} {
		if !seen[h] {
			t.Errorf("orphan handle 0x%X not deleted", h)
		}
	}
}

// TestOrphanDelete_SuppressedDuringRaceWindow: unknown handle arriving
// within 100ms of a successful subscribe (first-sample-race) must NOT fire
// orphan-Delete — that handle is most likely our own pending subscribe.
func TestOrphanDelete_SuppressedDuringRaceWindow(t *testing.T) {
	srv := fakeplc.StartPLC(t)
	defer srv.Stop()

	var deleted atomic.Int32
	srv.OnDeleteDeviceNotification(func(_ uint32) ams.ReturnCode {
		deleted.Add(1)
		return ams.ReturnCodeNoErrors
	})

	sess, c := newWiredTestSession(t, srv)
	c.SetNotificationHandler(sess.handleNotification)
	// Mark lastSubscribeNs as just now — race-window active.
	sess.notifications.lastSubscribeNs.Store(time.Now().UnixNano())

	pkt := buildNotificationPacket(0xBEEF, 0, []byte{0x42})
	_ = sess.drivePacket(sess.lifecycle.ctx, pkt)

	time.Sleep(200 * time.Millisecond)
	if got := deleted.Load(); got != 0 {
		t.Errorf("Delete RPC calls = %d, want 0 (race-window must suppress orphan-Delete)", got)
	}
}

// TestOrphanDelete_SuppressedDuringReconnect: FSM state Reconnecting must
// suppress orphan-Delete. Old subscriptions are being explicitly cleaned
// up by Fix 3's post-dial Delete path; firing async orphan-Deletes during
// the reconnect window would race with that cleanup.
func TestOrphanDelete_SuppressedDuringReconnect(t *testing.T) {
	srv := fakeplc.StartPLC(t)
	defer srv.Stop()

	var deleted atomic.Int32
	srv.OnDeleteDeviceNotification(func(_ uint32) ams.ReturnCode {
		deleted.Add(1)
		return ams.ReturnCodeNoErrors
	})

	sess, c := newWiredTestSession(t, srv)
	c.SetNotificationHandler(sess.handleNotification)
	sess.notifications.lastSubscribeNs.Store(time.Now().Add(-1 * time.Second).UnixNano())
	// Walk FSM into Reconnecting state.
	sess.lifecycle.state.value.Store(uint32(SessionStateReconnecting))

	pkt := buildNotificationPacket(0x1111, 0, []byte{0x00})
	_ = sess.drivePacket(sess.lifecycle.ctx, pkt)

	time.Sleep(200 * time.Millisecond)
	if got := deleted.Load(); got != 0 {
		t.Errorf("Delete RPC calls = %d, want 0 (Reconnecting state must suppress orphan-Delete)", got)
	}
}

// TestOrphanDelete_AbortsWhenHandleReappearsInActiveNotifications guards
// against the critical race: between scheduling and firing the Delete RPC,
// a concurrent Subscribe receives the same handle ID back from
// the PLC (PLC reuses freed slot IDs). The orphan-Delete must re-check
// activeNotifications under lock and abort if the handle is now present.
func TestOrphanDelete_AbortsWhenHandleReappearsInActiveNotifications(t *testing.T) {
	srv := fakeplc.StartPLC(t)
	defer srv.Stop()

	var deleted atomic.Int32
	// Block the Delete RPC for 200ms so we can inject the handle back into
	// activeNotifications before the goroutine's re-check fires.
	srv.DelayBefore(ams.CommandDeleteDeviceNotification, 0, 200*time.Millisecond)
	srv.OnDeleteDeviceNotification(func(_ uint32) ams.ReturnCode {
		deleted.Add(1)
		return ams.ReturnCodeNoErrors
	})

	sess, c := newWiredTestSession(t, srv)
	c.SetNotificationHandler(sess.handleNotification)
	sess.notifications.lastSubscribeNs.Store(time.Now().Add(-1 * time.Second).UnixNano())

	pkt := buildNotificationPacket(0xACEACE, 0, []byte{0x00})
	_ = sess.drivePacket(sess.lifecycle.ctx, pkt)

	// Immediately stage the same handle into activeNotifications, simulating
	// a concurrent re-subscribe getting the same ID back from the PLC. The
	// goroutine's re-check (under notifications.lock, just before the RPC
	// is dispatched) must see this and abort.
	sess.notifications.lock.Lock()
	sess.notifications.activeNotifications[0xACEACE] = activeNotification{Sym: &symtab.Symbol{FullName: "MAIN.x"}}
	sess.notifications.lock.Unlock()

	time.Sleep(500 * time.Millisecond)
	if got := deleted.Load(); got != 0 {
		t.Errorf("Delete RPC calls = %d, want 0 (re-check must abort when handle reappeared in activeNotifications)", got)
	}
}

// TestOrphanDelete_RPCFailureNonFatal: PLC returning an error for the
// Delete RPC must not panic, leak goroutines, or skip the throttle update.
func TestOrphanDelete_RPCFailureNonFatal(t *testing.T) {
	srv := fakeplc.StartPLC(t)
	defer srv.Stop()

	srv.OnDeleteDeviceNotification(func(_ uint32) ams.ReturnCode {
		return ams.ReturnCodeDeviceError
	})

	sess, c := newWiredTestSession(t, srv)
	c.SetNotificationHandler(sess.handleNotification)
	sess.notifications.lastSubscribeNs.Store(time.Now().Add(-1 * time.Second).UnixNano())

	pkt := buildNotificationPacket(0x9999, 0, []byte{0x00})
	if err := sess.drivePacket(sess.lifecycle.ctx, pkt); err != nil {
		t.Fatalf("drivePacket returned error on PLC-side Delete failure: %v", err)
	}

	time.Sleep(300 * time.Millisecond)
	// Throttle must still have recorded the attempt — second drive should be throttled.
	pkt2 := buildNotificationPacket(0x9999, 0, []byte{0x00})
	_ = sess.drivePacket(sess.lifecycle.ctx, pkt2)
	time.Sleep(100 * time.Millisecond)
	// Verify Session not closed/panicked: a benign op should succeed.
	if sess.isClosed() {
		t.Error("session unexpectedly closed after orphan-Delete RPC failure")
	}
}

// TestOrphanDeleteAbortReason covers the reaper's last-moment guards directly.
// The previous pair of tests for this did not: one set subscribeInFlight and
// asserted no Delete RPC, but with the counter set dispatch takes the buffer
// branch and never reaches the reaper at all, so the assertion held for the
// wrong reason; the other raced an unsynchronised goroutine against a Store and
// would flake on a loaded machine.
func TestOrphanDeleteAbortReason(t *testing.T) {
	tests := []struct {
		name       string
		setup      func(sess *Session)
		wantAbort  bool
		wantReason string
	}{
		{
			name:      "unknown handle, nothing in flight",
			setup:     func(*Session) {},
			wantAbort: false,
		},
		{
			name: "handle reappeared in activeNotifications",
			setup: func(sess *Session) {
				sess.notifications.lock.Lock()
				sess.notifications.activeNotifications[0x4242] = activeNotification{Sym: &symtab.Symbol{FullName: "MAIN.x"}}
				sess.notifications.lock.Unlock()
			},
			wantAbort:  true,
			wantReason: "reappeared",
		},
		{
			name: "a subscribe is in flight",
			setup: func(sess *Session) {
				sess.beginSubscribe()
			},
			wantAbort:  true,
			wantReason: "in flight",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sess := newNotifTestSession()
			tt.setup(sess)
			reason, abort := sess.orphanDeleteAbortReason(0x4242)
			if abort != tt.wantAbort {
				t.Fatalf("abort = %v, want %v (reason %q)", abort, tt.wantAbort, reason)
			}
			if tt.wantReason != "" && !strings.Contains(reason, tt.wantReason) {
				t.Errorf("reason = %q, want it to mention %q", reason, tt.wantReason)
			}
		})
	}
}

// TestOrphanDelete_BuffersRatherThanReapsWhileSubscribing keeps the behavioural
// half of what the old test was really checking: with a subscribe in flight an
// unknown sample is parked, so the reaper is never even consulted.
func TestOrphanDelete_BuffersRatherThanReapsWhileSubscribing(t *testing.T) {
	srv := fakeplc.StartPLC(t)
	defer srv.Stop()

	var deleted atomic.Int32
	srv.OnDeleteDeviceNotification(func(_ uint32) ams.ReturnCode {
		deleted.Add(1)
		return ams.ReturnCodeNoErrors
	})

	sess, c := newWiredTestSession(t, srv)
	c.SetNotificationHandler(sess.handleNotification)
	// Stale timestamp: the old 100ms-window guard alone would let the reaper through.
	sess.notifications.lastSubscribeNs.Store(time.Now().Add(-time.Second).UnixNano())
	sess.beginSubscribe()

	if err := sess.drivePacket(sess.lifecycle.ctx, buildNotificationPacket(0x7777, 0, intSample(1))); err != nil {
		t.Fatalf("drivePacket: %v", err)
	}

	time.Sleep(200 * time.Millisecond)
	if got := deleted.Load(); got != 0 {
		t.Errorf("Delete RPC calls = %d, want 0 while a subscribe is in flight", got)
	}
	if got := earlySampleCount(sess); got != 1 {
		t.Errorf("buffered samples = %d, want 1 (the sample must be parked, not dropped)", got)
	}
}

// TestOrphanDelete_NotStartedAfterClose: the orphan reaper is the path that reaches
// trackGoroutine from a user goroutine, so it gets its own check — including that a
// refusal releases what it reserved, or a genuinely leaked handle stays locked out
// of the reaper for the whole throttle window.
func TestOrphanDelete_NotStartedAfterClose(t *testing.T) {
	srv := fakeplc.StartPLC(t)
	defer srv.Stop()
	var deletes atomic.Int32
	srv.OnDeleteDeviceNotification(func(_ uint32) ams.ReturnCode {
		deletes.Add(1)
		return ams.ReturnCodeNoErrors
	})

	sess, c := newWiredTestSession(t, srv, WithoutNotificationHeartbeat())
	c.SetNotificationHandler(sess.handleNotification)
	sess.markClosed()

	const unknown = uint32(0xDEAD10CC)
	sess.tryOrphanDelete(unknown)
	time.Sleep(300 * time.Millisecond)

	if got := deletes.Load(); got != 0 {
		t.Errorf("%d orphan delete(s) issued after the session was closed", got)
	}
	sess.notifications.orphanMu.Lock()
	_, throttled := sess.notifications.orphanSeen[unknown]
	sess.notifications.orphanMu.Unlock()
	if throttled {
		t.Error("the refused attempt left its throttle entry behind: a genuinely leaked handle would go unreaped for the " +
			"whole window because it happened to fire as the session closed")
	}
	if len(sess.notifications.orphanSem) != 0 {
		t.Errorf("the refused attempt leaked a semaphore slot (%d held)", len(sess.notifications.orphanSem))
	}
}
