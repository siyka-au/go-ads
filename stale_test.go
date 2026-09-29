package ads

import (
	"encoding/binary"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/siyka-au/go-ads/v3/ams"
	"github.com/siyka-au/go-ads/v3/internal/fakeplc"
	"github.com/siyka-au/go-ads/v3/internal/symtab"
)

// Validates: R-NOT-016.
func TestUpdate_StaleReasonFields(t *testing.T) {
	u := Update{Symbol: "x", Value: "1", Stale: &StaleInfo{Reason: ReasonSymbolVersionInvalid}}
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

// Validates: R-NOT-016 + R-NOT-017 — Ignore strategy flags next sample
// Stale=true, Reason=detected reason; flag is one-shot.
func TestNotification_StaleFlag_OneShotAfterDetection(t *testing.T) {
	sess := newTestConnection()
	defer sess.lifecycle.shutdown()

	ch := make(chan *Update, 4)
	sym := &symtab.Symbol{
		FullName: "MAIN.x", DataType: "INT", Length: 2,
	}
	const handle uint32 = 7
	sess.notifications.lock.Lock()
	sess.notifications.activeNotifications[handle] = activeNotification{Sym: sym, Ch: ch}
	sess.notifications.lock.Unlock()
	sess.cache.lock.Lock()
	sess.cache.symbols[symtab.Key(sym.FullName)] = sym
	sess.cache.lock.Unlock()

	// Mark stale (simulates prior R-CACHE-009 detection under Ignore).
	sess.markSymbolStale(handle, ReasonSymbolVersionInvalid)

	// First sample: must carry Stale=true.
	pkt := buildNotificationPacket(handle, 0, []byte{0x01, 0x00})
	if err := sess.drivePacket(sess.lifecycle.ctx, pkt); err != nil {
		t.Fatalf("drivePacket #1: %v", err)
	}
	select {
	case u := <-ch:
		if u.Stale == nil || u.Stale.Reason != ReasonSymbolVersionInvalid {
			t.Errorf("first sample: got Stale=%v, want non-nil with Reason=%q",
				u.Stale, ReasonSymbolVersionInvalid)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no first sample")
	}

	// Second sample: flag consumed, must NOT be Stale.
	pkt2 := buildNotificationPacket(handle, 0, []byte{0x02, 0x00})
	if err := sess.drivePacket(sess.lifecycle.ctx, pkt2); err != nil {
		t.Fatalf("drivePacket #2: %v", err)
	}
	select {
	case u := <-ch:
		if u.Stale != nil {
			t.Errorf("second sample: got Stale=%+v, want nil", u.Stale)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no second sample")
	}
}

// Validates: R-CACHE-012 + R-NOT-017 — Ignore strategy detection marks ALL
// active handles, not just the one that triggered detection.
func TestNotification_StaleFlag_IgnoreMarksAllHandles(t *testing.T) {
	sess := newTestConnection()
	defer sess.lifecycle.shutdown()
	sess.versionStrategy = SymbolVersionIgnore

	const h1, h2 uint32 = 11, 22
	sess.notifications.lock.Lock()
	sess.notifications.activeNotifications[h1] = activeNotification{Sym: &symtab.Symbol{FullName: "a"}}
	sess.notifications.activeNotifications[h2] = activeNotification{Sym: &symtab.Symbol{FullName: "b"}}
	sess.notifications.lock.Unlock()

	// Trigger detection through the strategy dispatcher.
	stale, reason := sess.handleStaleDetection(ams.ReturnCodeDeviceSymbolVersionInvalid)
	if !stale {
		t.Fatal("expected stale=true")
	}
	if reason != ReasonSymbolVersionInvalid {
		t.Fatalf("reason = %q, want %q", reason, ReasonSymbolVersionInvalid)
	}

	r1, ok1 := sess.consumeStaleFlag(h1)
	r2, ok2 := sess.consumeStaleFlag(h2)
	if !ok1 || r1 != ReasonSymbolVersionInvalid {
		t.Errorf("h1: ok=%v reason=%q", ok1, r1)
	}
	if !ok2 || r2 != ReasonSymbolVersionInvalid {
		t.Errorf("h2: ok=%v reason=%q", ok2, r2)
	}
}

// Validates: consumeStaleFlag idempotency.
func TestSession_ConsumeStaleFlag_IdempotentSecondCallEmpty(t *testing.T) {
	sess := &Session{}
	sess.markSymbolStale(99, ReasonSymbolNotFound)
	r, ok := sess.consumeStaleFlag(99)
	if !ok || r != ReasonSymbolNotFound {
		t.Errorf("first consume: ok=%v r=%q", ok, r)
	}
	r2, ok2 := sess.consumeStaleFlag(99)
	if ok2 || r2 != "" {
		t.Errorf("second consume: ok=%v r=%q, want (false, \"\")", ok2, r2)
	}
}

// TestSession_HandleStaleDetection_NoMatch validates that non-stale
// ReturnCodes are a no-op for the dispatcher.
//
// Validates: R-CACHE-009 dispatch (negative path).
func TestSession_HandleStaleDetection_NoMatch(t *testing.T) {
	sess := &Session{
		versionStrategy: SymbolVersionIgnore,
		logger:          slog.Default(),
	}
	stale, reason := sess.handleStaleDetection(ams.ReturnCodeNoErrors)
	if stale || reason != "" {
		t.Errorf("got (%v, %q), want (false, \"\")", stale, reason)
	}
}

// TestSession_HandleStaleDetection_Ignore_FiresCallback validates that the
// Ignore strategy fires the callback in a goroutine and returns the
// expected reason without altering Session state.
//
// Validates: R-CACHE-012 (Ignore) + R-NOT-016 (callback reason).
func TestSession_HandleStaleDetection_Ignore_FiresCallback(t *testing.T) {
	cbReason := make(chan Reason, 1)
	sess := &Session{
		versionStrategy: SymbolVersionIgnore,
		versionCallback: func(r Reason) { cbReason <- r },
		logger:          slog.Default(),
	}

	stale, reason := sess.handleStaleDetection(ams.ReturnCodeDeviceSymbolVersionInvalid)
	if !stale || reason != ReasonSymbolVersionInvalid {
		t.Errorf("got (%v, %q), want (true, %q)", stale, reason, ReasonSymbolVersionInvalid)
	}
	select {
	case r := <-cbReason:
		if r != ReasonSymbolVersionInvalid {
			t.Errorf("callback got %q, want %q", r, ReasonSymbolVersionInvalid)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("callback not invoked")
	}
}

// TestSession_HandleStaleDetection_NilCallbackOK validates R-SES-011: when
// no callback is configured, the dispatcher must still classify the code
// without panicking.
//
// Validates: R-SES-011 (nil-callback safety).
func TestSession_HandleStaleDetection_NilCallbackOK(t *testing.T) {
	sess := &Session{
		versionStrategy: SymbolVersionIgnore,
		versionCallback: nil,
		logger:          slog.Default(),
	}
	stale, _ := sess.handleStaleDetection(ams.ReturnCodeDeviceSymbolVersionInvalid)
	if !stale {
		t.Error("expected stale=true")
	}
}

// TestSession_HandleStaleDetection_Close exercises the SymbolVersionClose
// strategy end-to-end against a scriptable PLC stub: handleStaleDetection
// must spawn closeOnStaleDetection in a goroutine, which fires the
// onDisconnect callback and drives the FSM to Closed.
//
// Validates: R-CACHE-011 (Close strategy terminates session +
// surfaces lifecycle event to observers).
func TestSession_HandleStaleDetection_Close(t *testing.T) {
	srv := fakeplc.StartPLC(t)
	defer srv.Stop()

	disconnected := make(chan struct{})
	sess, _ := newWiredTestSession(t, srv,
		WithSymbolVersionStrategy(SymbolVersionClose),
		WithOnDisconnect(func() { close(disconnected) }),
	)

	// Trigger detection synthetically. Close strategy fires
	// closeOnStaleDetection in a goroutine, so the test must wait for the
	// disconnect signal rather than asserting immediately.
	stale, reason := sess.handleStaleDetection(ams.ReturnCodeDeviceSymbolVersionInvalid)
	if !stale || reason != ReasonSymbolVersionInvalid {
		t.Errorf("handleStaleDetection = (%v, %q), want (true, %q)",
			stale, reason, ReasonSymbolVersionInvalid)
	}

	select {
	case <-disconnected:
		// pass
	case <-time.After(3 * time.Second):
		t.Fatal("Close strategy did not fire onDisconnect within 3s")
	}

	// closeOnStaleDetection invokes sess.Close() in the same goroutine as
	// the onDisconnect callback. Close() runs synchronously, so once the
	// callback has fired and Close() returns, isClosed() must report true.
	// Allow a short grace window for the goroutine to finish Close().
	deadline := time.Now().Add(2 * time.Second)
	for !sess.isClosed() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !sess.isClosed() {
		t.Error("session not in Closed state after Close strategy")
	}
}

// TestSession_TryRecordReloadAttempt_CapEnforced validates that
// tryRecordReloadAttempt returns true until the per-window cap is reached
// and false thereafter. Pure unit — no PLC stub needed.
//
// Validates: R-CACHE-013 (sliding-window cap).
func TestSession_TryRecordReloadAttempt_CapEnforced(t *testing.T) {
	sess := &Session{
		maxReloadAttempts: 2,
		reloadWindow:      10 * time.Second,
	}
	if !sess.tryRecordReloadAttempt() {
		t.Error("attempt 1 must succeed")
	}
	if !sess.tryRecordReloadAttempt() {
		t.Error("attempt 2 must succeed")
	}
	if sess.tryRecordReloadAttempt() {
		t.Error("attempt 3 must be rejected (cap=2)")
	}
}

// TestSession_TryRecordReloadAttempt_SlidingWindow validates that entries
// older than reloadWindow are pruned, freeing capacity for new attempts.
//
// Validates: R-CACHE-013 (sliding window).
func TestSession_TryRecordReloadAttempt_SlidingWindow(t *testing.T) {
	sess := &Session{
		maxReloadAttempts: 2,
		reloadWindow:      50 * time.Millisecond,
	}
	if !sess.tryRecordReloadAttempt() {
		t.Fatal("attempt 1")
	}
	if !sess.tryRecordReloadAttempt() {
		t.Fatal("attempt 2")
	}
	// Wait for window to slide past.
	time.Sleep(80 * time.Millisecond)
	if !sess.tryRecordReloadAttempt() {
		t.Error("attempt after window slide must succeed")
	}
}

// TestSession_MarkAllHandlesStale validates the helper used by both the
// Ignore branch of handleStaleDetection and the AutoReload pre-reload
// window: every active notification handle gets a one-shot Stale flag
// with the supplied reason.
//
// Validates: R-NOT-017 (markAllHandlesStale fans the flag across handles).
func TestSession_MarkAllHandlesStale(t *testing.T) {
	sess := newTestConnection()
	defer sess.lifecycle.shutdown()
	sess.notifications.activeNotifications[42] = activeNotification{Sym: &symtab.Symbol{}}
	sess.notifications.activeNotifications[99] = activeNotification{Sym: &symtab.Symbol{}}

	sess.markAllHandlesStale(ReasonReloadInProgress)

	if r, ok := sess.consumeStaleFlag(42); !ok || r != ReasonReloadInProgress {
		t.Errorf("h=42: reason=%q ok=%v, want (%q, true)", r, ok, ReasonReloadInProgress)
	}
	if r, ok := sess.consumeStaleFlag(99); !ok || r != ReasonReloadInProgress {
		t.Errorf("h=99: reason=%q ok=%v, want (%q, true)", r, ok, ReasonReloadInProgress)
	}
	// Idempotency check: second consume returns ok=false.
	if _, ok := sess.consumeStaleFlag(42); ok {
		t.Error("flag should be one-shot — second consume should return ok=false")
	}
}

// TestSession_MarkAllHandlesStale_NilNotificationsSafe validates the
// nil-guard for unit-test bare Session{} construction.
func TestSession_MarkAllHandlesStale_NilNotificationsSafe(t *testing.T) {
	sess := &Session{logger: slog.Default()}
	// Must not panic even without notifications manager.
	sess.markAllHandlesStale(ReasonReloadInProgress)
}

// TestSession_AutoReload_BumpsEpoch validates that handleStaleDetection
// under SymbolVersionAutoReload triggers autoReloadOnStaleDetection which
// bumps the session epoch (R-CACHE-003) before attempting reload. The
// scriptable server doesn't implement the full reload sequence — but
// epoch is bumped BEFORE reload is attempted, so the test succeeds even
// when LoadSymbols() ultimately errors out.
//
// Validates: R-CACHE-010 (AutoReload bumps epoch + invalidates handles).
func TestSession_AutoReload_BumpsEpoch(t *testing.T) {
	srv := fakeplc.StartPLC(t)
	defer srv.Stop()

	sess, _ := newWiredTestSession(t, srv,
		WithSymbolVersionStrategy(SymbolVersionAutoReload),
		WithMaxSymbolVersionReloadAttempts(3),
		WithSymbolVersionReloadWindow(60*time.Second),
	)

	preEpoch := sess.epoch()

	sess.handleStaleDetection(ams.ReturnCodeDeviceSymbolVersionInvalid)

	deadline := time.After(5 * time.Second)
	for sess.epoch() == preEpoch {
		select {
		case <-deadline:
			t.Fatal("AutoReload did not bump epoch within 5s")
		default:
			time.Sleep(50 * time.Millisecond)
		}
	}
}

// TestSession_AutoReload_CapExhaustion_FiresCallback validates that once
// the sliding-window cap is exceeded, the callback fires with
// ReasonReloadCapExhausted and the strategy degrades to Ignore semantics
// (no further reload attempts within the window).
//
// Validates: R-CACHE-013 (cap exhaustion fires callback w/ specific reason).
func TestSession_AutoReload_CapExhaustion_FiresCallback(t *testing.T) {
	srv := fakeplc.StartPLC(t)
	defer srv.Stop()

	exhausted := make(chan Reason, 4)
	cb := func(reason Reason) {
		if reason == ReasonReloadCapExhausted {
			select {
			case exhausted <- reason:
			default:
			}
		}
	}

	sess, _ := newWiredTestSession(t, srv,
		WithSymbolVersionStrategy(SymbolVersionAutoReload),
		WithMaxSymbolVersionReloadAttempts(2),
		WithSymbolVersionReloadWindow(10*time.Second),
		WithOnSymbolVersionChanged(cb))

	for i := 0; i < 4; i++ {
		sess.handleStaleDetection(ams.ReturnCodeDeviceSymbolVersionInvalid)
		time.Sleep(50 * time.Millisecond)
	}

	select {
	case <-exhausted:
		// pass
	case <-time.After(3 * time.Second):
		t.Fatal("cap exhaustion did not fire callback w/ ReasonReloadCapExhausted within 3s")
	}
}

// TestSession_AutoReload_SingleFlight validates that when N stale events fire
// concurrently under SymbolVersionAutoReload, only one reload goroutine runs
// (R-CACHE-010). The single-flight guard (reloadInProgress CAS) must prevent
// duplicate concurrent bumpEpoch calls.
func TestSession_AutoReload_SingleFlight(t *testing.T) {
	srv := fakeplc.StartPLC(t)
	defer srv.Stop()

	sess, _ := newWiredTestSession(t, srv,
		WithSymbolVersionStrategy(SymbolVersionAutoReload),
		WithMaxSymbolVersionReloadAttempts(10),
		WithSymbolVersionReloadWindow(60*time.Second),
	)

	// Make the reload outlast the window in which the N detections arrive.
	// Without this the guard is only exercised if the goroutines happen to
	// overlap a reload that fails in microseconds against the stub — so the test
	// passed or failed on scheduling luck rather than on the guard, and it did
	// flake (~1 in 16). The delay makes the overlap a property of the test.
	srv.DelayBefore(ams.CommandRead, uint32(ams.GroupSymbolUploadInfo), 300*time.Millisecond)

	preEpoch := sess.epoch()

	const N = 5
	var wg sync.WaitGroup
	for range N {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sess.handleStaleDetection(ams.ReturnCodeDeviceSymbolVersionInvalid)
		}()
	}
	wg.Wait()

	// Give goroutines time to complete the reload path.
	deadline := time.After(3 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatal("epoch never bumped within 3s")
		default:
		}
		if sess.epoch() > preEpoch {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Allow the one in-flight goroutine to complete before sampling epoch.
	time.Sleep(200 * time.Millisecond)

	got := int(sess.epoch() - preEpoch)
	if got > 1 {
		t.Errorf("epoch bumped %d times, want 1 (single-flight guard failed)", got)
	}
}

// TestHandleStaleDetection_Ignore_FiresCallbackPerTrigger pins the contract
// that under SymbolVersionIgnore, N concurrent stale-cache detections fire N
// versionCallback invocations. The Ignore strategy intentionally does NOT
// gate the callback behind reloadInProgress CAS (which is AutoReload-only) —
// callers using Ignore typically install their own dedup logic and rely on
// every detection being observable.
//
// Companion to TestSession_AutoReload_SingleFlight, which pins the inverse
// contract for AutoReload (1 callback per N triggers).
func TestHandleStaleDetection_Ignore_FiresCallbackPerTrigger(t *testing.T) {
	sess := newTestConnection()
	defer sess.lifecycle.shutdown()
	sess.versionStrategy = SymbolVersionIgnore

	var callbacks atomic.Int32
	sess.versionCallback = func(_ Reason) {
		callbacks.Add(1)
	}

	const N = 25
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			sess.handleStaleDetection(ams.ReturnCodeDeviceSymbolVersionInvalid)
		}()
	}
	wg.Wait()

	// versionCallback is launched in a goroutine; give them all room to run.
	// Each callback is trivial (single atomic.Add), so a short bounded wait
	// is enough — but use a deadline so a regression that drops callbacks
	// fails reliably instead of timing out the whole test.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if callbacks.Load() == int32(N) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := callbacks.Load(); got != int32(N) {
		t.Errorf("Ignore strategy: %d callbacks for %d triggers, want %d (every detection must be observable)", got, N, N)
	}
}

// TestAutoReload_DeletesOldHandlesBeforeResubscribe verifies Fix 2 of the
// v2.2.1 PLC-flood patch: reloadSymbolsAndResubscribe must issue a
// SumDeleteDeviceNotification for every pre-reload handle BEFORE attempting
// to re-subscribe, so the PLC's notification handle table doesn't accumulate
// orphan entries across online-change cycles.
//
// We pre-populate activeNotifications with two synthetic handles, intercept
// the SumDelete RPC via the scriptable server, then drive
// reloadSymbolsAndResubscribe. LoadSymbols is expected to fail (no upload
// handler registered) — we only validate the pre-delete step ran and that
// activeNotifications was wiped under the same lock.
func TestAutoReload_DeletesOldHandlesBeforeResubscribe(t *testing.T) {
	srv := fakeplc.StartPLC(t)
	defer srv.Stop()

	var mu sync.Mutex
	var deletedHandles []uint32
	srv.OnWriteRead(ams.GroupSumupDeleteDeviceNotification, func(req []byte) []byte {
		n := len(req) / 4
		mu.Lock()
		for i := 0; i < n; i++ {
			deletedHandles = append(deletedHandles, binary.LittleEndian.Uint32(req[i*4:]))
		}
		mu.Unlock()
		codes := make([]ams.ReturnCode, n)
		for i := range codes {
			codes[i] = ams.ReturnCodeNoErrors
		}
		return fakeplc.SumDeleteNotifPayload(codes)
	})

	sess, _ := newWiredTestSession(t, srv)
	sess.notifications.lock.Lock()
	sess.notifications.activeNotifications[0xA1A1] = activeNotification{Sym: &symtab.Symbol{FullName: "MAIN.x"}, Ch: nil}
	sess.notifications.activeNotifications[0xB2B2] = activeNotification{Sym: &symtab.Symbol{FullName: "MAIN.y"}, Ch: nil}
	sess.notifications.lock.Unlock()

	// LoadSymbols is expected to fail (no upload-info handler registered).
	// The pre-delete step runs unconditionally before LoadSymbols, so we
	// don't care about the returned error.
	_ = sess.reloadSymbolsAndResubscribe()

	mu.Lock()
	got := append([]uint32{}, deletedHandles...)
	mu.Unlock()

	if len(got) != 2 {
		t.Fatalf("Delete RPC received %d handles, want 2: got=%v", len(got), got)
	}
	seen := map[uint32]bool{0xA1A1: false, 0xB2B2: false}
	for _, h := range got {
		if _, ok := seen[h]; ok {
			seen[h] = true
		}
	}
	for h, found := range seen {
		if !found {
			t.Errorf("handle 0x%X not in Delete RPC payload", h)
		}
	}

	sess.notifications.lock.Lock()
	postLen := len(sess.notifications.activeNotifications)
	sess.notifications.lock.Unlock()
	if postLen != 0 {
		t.Errorf("activeNotifications len = %d after reload, want 0 (must be wiped under lock with snapshot)", postLen)
	}
}

// TestAutoReload_NoOldHandles_SkipsDelete validates the empty-snapshot path:
// when reloadSymbolsAndResubscribe runs with no pre-existing handles, no
// SumDelete RPC must fire (avoids a useless wire round-trip on first reload).
func TestAutoReload_NoOldHandles_SkipsDelete(t *testing.T) {
	srv := fakeplc.StartPLC(t)
	defer srv.Stop()

	var deleteCalls atomic.Int32
	srv.OnWriteRead(ams.GroupSumupDeleteDeviceNotification, func(_ []byte) []byte {
		deleteCalls.Add(1)
		return fakeplc.SumDeleteNotifPayload([]ams.ReturnCode{ams.ReturnCodeNoErrors})
	})

	sess, _ := newWiredTestSession(t, srv)
	// activeNotifications is empty by construction.
	_ = sess.reloadSymbolsAndResubscribe()

	if got := deleteCalls.Load(); got != 0 {
		t.Errorf("SumDelete called %d times with empty active set, want 0", got)
	}
}
