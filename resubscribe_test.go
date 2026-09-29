package ads

import (
	"log/slog"
	"sync/atomic"
	"testing"

	"github.com/siyka-au/go-ads/v3/internal/fakeplc"

	"github.com/siyka-au/go-ads/v3/internal/symtab"

	"github.com/siyka-au/go-ads/v3/internal/testlog"

	"github.com/siyka-au/go-ads/v3/ams"
)

// TestResubscribeRetry_UpToMax exercises the resubscribeMaxAttempts cap path.
// On each call to resubscribeNotifications, configs that come back as Skipped
// have their counter incremented and are re-queued until counter >= max,
// at which point they are dropped with a WARN log.
//
// Drive Skipped: server returns SumAddDeviceNotification success with valid
// handles, but the test's onWriteRead handler removes the symbol from cache
// during the request. The post-roundtrip re-fetch finds nil → Skipped+Handle
// fires. Then we restore the symbol so the next iteration's filter keeps it.
//
// Counts WARN log emissions and confirms the config is dropped at the cap.
//
// Validates: R-NOT-013 (resubscribe retry-up-to-max with cap enforcement).
func TestResubscribeRetry_UpToMax(t *testing.T) {
	srv := fakeplc.StartPLC(t)
	defer srv.Stop()

	logHandler := &testlog.Handler{}
	logger := slog.New(logHandler)

	sess, _ := newWiredTestSession(t, srv)
	sess.logger = logger
	preSeedSymbol(sess, "MAIN.x")
	// Pre-populate config + channel so resubscribe has work to do.
	ch := make(chan *Update, 1)
	sess.notifications.lock.Lock()
	sess.notifications.pending = []pendingNotification{
		{Config: NotificationConfig{Symbol: "MAIN.x", Mode: ams.TransModeServerOnChange}},
	}
	sess.notifications.configsByKey[symtab.Key("MAIN.x")] = struct{}{}
	sess.notifications.notificationChannel = ch
	sess.notifications.lock.Unlock()

	var sumHandle atomic.Uint32
	sumHandle.Store(0xCC000001)

	// Sum-add request: respond with a fresh handle but FIRST swap the cache
	// to empty so the post-roundtrip re-fetch finds nil and Skipped fires.
	srv.OnWriteRead(ams.GroupSumupAddDeviceNotification, func(_ []byte) []byte {
		sess.cache.lock.Lock()
		sess.cache.symbols = map[string]*symtab.Symbol{}
		sess.cache.lock.Unlock()
		h := sumHandle.Add(1) - 1
		return fakeplc.SumAddNotifPayload([]fakeplc.SumNotifResponse{{Handle: h, Error: ams.ReturnCodeNoErrors}})
	})
	// bestEffortDelete after Skipped+Handle uses SumDeleteDeviceNotification.
	srv.OnWriteRead(ams.GroupSumupDeleteDeviceNotification, func(req []byte) []byte {
		// One handle per request (4 bytes). Always succeed.
		nItems := len(req) / 4
		codes := make([]ams.ReturnCode, nItems)
		for i := range codes {
			codes[i] = ams.ReturnCodeNoErrors
		}
		return fakeplc.SumDeleteNotifPayload(codes)
	})

	// Run resubscribeMaxAttempts (=3) iterations. Each attempt must be
	// preceded by re-seeding the symbol so filterValidNotificationConfigs
	// keeps it. After each, the config is re-queued with attempts++ until
	// the cap is reached.
	for i := 0; i < resubscribeMaxAttempts; i++ {
		preSeedSymbol(sess, "MAIN.x")
		// Re-pin the channel since resubscribe clears it when there are no
		// valid configs at the start.
		sess.notifications.lock.Lock()
		sess.notifications.notificationChannel = ch
		sess.notifications.lock.Unlock()

		if err := sess.resubscribeNotifications(); err != nil {
			t.Fatalf("iter %d: resubscribeNotifications: %v", i, err)
		}
	}

	// After resubscribeMaxAttempts iterations: the config must be DROPPED,
	// not requeued.
	sess.notifications.lock.Lock()
	leftover := len(sess.notifications.pending)
	sess.notifications.lock.Unlock()
	if leftover != 0 {
		t.Errorf("after %d retries: notificationConfigs still has %d entries, want 0 (dropped at cap)",
			resubscribeMaxAttempts, leftover)
	}

	// At least one WARN log about dropping configs after max retries.
	if logHandler.FindByMessage("dropping configs after max retries") == nil {
		t.Errorf("expected WARN log 'dropping configs after max retries' was not emitted")
	}
}

// TestResubscribeNotifications_RollbackOnError verifies that when
// SubscribeAll returns an outer error mid-resubscribe, the rollback
// path restores notificationConfigs and notificationChannel from the
// pre-call snapshot. Without rollback, the configs would be left empty
// after a failed retry and subsequent reconnects would have nothing to
// resubscribe (user notification subscriptions silently dropped).
//
// Drives the error by registering a SumAddDeviceNotification handler that
// returns a too-short response so executeSumCommand's length validation
// fails — surfaces as outer err to SubscribeAll.
//
// Validates: resubscribeNotifications save/restore via resetConfigs.
func TestResubscribeNotifications_RollbackOnError(t *testing.T) {
	srv := fakeplc.StartPLC(t)
	defer srv.Stop()

	// Truncated response: claims n=1 item but returns 0 bytes of item data.
	// executeSumCommand asserts len(resp) >= n*itemReadSize (n*8 for Add).
	srv.OnWriteRead(ams.GroupSumupAddDeviceNotification, func(_ []byte) []byte {
		return []byte{} // too short — outer parse will fail
	})

	sess, _ := newWiredTestSession(t, srv)
	preSeedSymbol(sess, "MAIN.x")
	ch := make(chan *Update, 1)
	saved := []pendingNotification{
		{Config: NotificationConfig{Symbol: "MAIN.x", Mode: ams.TransModeServerOnChange, MaxDelay: 0, CycleTime: 0}},
	}

	sess.notifications.lock.Lock()
	sess.notifications.resetConfigs(saved)
	sess.notifications.notificationChannel = ch
	sess.notifications.lock.Unlock()

	// resubscribeNotifications runs the SubscribeAll path. With the
	// truncated-response handler installed, the call errors out and rollback
	// must restore both fields.
	err := sess.resubscribeNotifications()

	// Rollback restored — savedConfigs is back in place.
	sess.notifications.lock.Lock()
	got := sess.notifications.pending
	gotChannel := sess.notifications.notificationChannel
	sess.notifications.lock.Unlock()

	if len(got) != 1 || got[0].Config.Symbol != "MAIN.x" {
		t.Errorf("pending after rollback = %+v, want 1 entry for MAIN.x", got)
	}
	if gotChannel != ch {
		t.Errorf("notificationChannel after rollback = %v, want %v (saved channel)", gotChannel, ch)
	}
	if !sess.notifications.hasConfig("MAIN.x") {
		t.Errorf("configsByKey mirror not rebuilt by resetConfigs on rollback")
	}
	// SubscribeAll may return err or nil depending on whether the
	// SumAddNotifState CAS landed on unsupported (triggering fallback). Either
	// is acceptable for the rollback contract — we care about the restoration.
	_ = err
}

// TestResubscribe_NilChannelKeepsTheDeclaredIntent: a re-subscribe that has no
// channel to deliver to is a no-op, and a no-op must not destroy the caller's
// declared subscriptions.
//
// resubscribeNotificationsLocked cleared pending BEFORE its "nothing to do" guard
// and then returned nil, so M symbols the caller never cancelled were dropped from
// the resubscribe set silently, with a "reconnect successful" logged over the top.
// The state is reachable: a resubscribe re-queues entries the PLC refused with a
// retryable reason (they have no handle), and the caller then deletes its remaining
// LIVE subscriptions by handle — the last delete nils notificationChannel while
// those re-queued entries are still on file.
func TestResubscribe_NilChannelKeepsTheDeclaredIntent(t *testing.T) {
	srv := fakeplc.StartPLC(t)
	defer srv.Stop()
	sess, _ := newWiredTestSession(t, srv)

	// Declared intent with no channel bound and nothing live — exactly the state a
	// re-queued entry plus a full user teardown leaves behind.
	sess.notifications.lock.Lock()
	sess.notifications.addPending(pendingNotification{Config: NotificationConfig{Symbol: "MAIN.stranded"}})
	sess.notifications.notificationChannel = nil
	sess.notifications.lock.Unlock()

	if err := sess.resubscribeNotifications(); err != nil {
		t.Fatalf("resubscribe with no bound channel = %v, want nil: a no-op must still report success", err)
	}

	sess.notifications.lock.Lock()
	pending := len(sess.notifications.pending)
	mirrored := sess.notifications.hasConfig("MAIN.stranded")
	sess.notifications.lock.Unlock()
	if pending != 1 {
		t.Errorf("pending = %d after a resubscribe with no bound channel, want 1: the caller's declared intent must survive a no-op resubscribe", pending)
	}
	if !mirrored {
		t.Error("configsByKey no longer mirrors pending: the two must move in lockstep")
	}
}
