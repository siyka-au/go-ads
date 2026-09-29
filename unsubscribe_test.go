package ads

import (
	"context"
	"log/slog"
	"sync/atomic"
	"testing"

	"github.com/siyka-au/go-ads/v3/internal/adsconn"
	"github.com/siyka-au/go-ads/v3/internal/testlog"

	"github.com/siyka-au/go-ads/v3/internal/fakeplc"

	"github.com/siyka-au/go-ads/v3/ams"
)

// bestEffortDeleteNotifications returns 0 for an empty input slice and never
// touches the network.
// Validates: R-NOT-015.
func TestBestEffortDeleteNotifications_Empty(t *testing.T) {
	conn := &Session{logger: slog.Default()}
	conn.client.Store(adsconn.New(adsconn.Config{Logger: conn.logger}))
	got := conn.bestEffortDeleteNotifications(context.Background(), nil)
	if got != 0 {
		t.Errorf("expected 0, got %d", got)
	}
	got = conn.bestEffortDeleteNotifications(context.Background(), []uint32{})
	if got != 0 {
		t.Errorf("expected 0, got %d", got)
	}
}

// TestDeleteNotification_AlreadyGoneStillCleansUpBookkeeping: when the PLC says
// the registration is already gone, the local bookkeeping must go with it.
//
// 0x714 NotifyHandleInvalid and 0x715 DeviceClientUnknown mean the PLC has no such
// registration — after a runtime restart or a dropped client identity, that is the
// normal answer. Returning early on it left the entry in activeNotifications
// forever (every retry gets the same code, so the caller can never delete it) and
// left the config on file, so the next reconnect re-subscribed a symbol the caller
// had explicitly deleted, creating a duplicate PLC registration. The batch sibling
// has always treated these codes as success-equivalent.
func TestDeleteNotification_AlreadyGoneStillCleansUpBookkeeping(t *testing.T) {
	srv := fakeplc.StartPLC(t)
	defer srv.Stop()
	var nextHandle atomic.Uint32
	nextHandle.Store(0xB00)
	srv.OnAddDeviceNotification(func(_ fakeplc.AddNotifRequest) fakeplc.AddNotifResponse {
		return fakeplc.AddNotifResponse{Handle: nextHandle.Add(1)}
	})
	// The PLC no longer knows this registration.
	srv.OnDeleteDeviceNotification(func(_ uint32) ams.ReturnCode {
		return ams.ReturnCodeDeviceNotifyHandleInvalid
	})

	sess, c := newWiredTestSession(t, srv, WithoutNotificationHeartbeat())
	c.SetNotificationHandler(sess.handleNotification)
	preSeedTypedSymbol(sess, "MAIN.gone", 0xF600)
	ch := make(chan *Update, 4)
	handle, err := sess.Subscribe(context.Background(), "MAIN.gone", 0, 0,
		ams.TransModeServerOnChange, ch)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	// The error is still reported — the caller asked for a delete and it did not
	// happen the way they asked — but the state must not be stranded.
	_ = sess.Unsubscribe(context.Background(), handle)

	sess.notifications.lock.Lock()
	_, stillActive := sess.notifications.activeNotifications[handle]
	stillConfigured := sess.notifications.hasConfig("MAIN.gone")
	sess.notifications.lock.Unlock()

	if stillActive {
		t.Errorf("handle %d still in activeNotifications after the PLC reported it already gone: "+
			"every retry gets the same code, so the caller can never remove it", handle)
	}
	if stillConfigured {
		t.Error("config for MAIN.gone still on file after a delete the PLC confirmed as already gone: " +
			"the next reconnect re-subscribes a symbol the caller deleted")
	}
}

// TestDeleteNotification_ForeignHandleKeepsTheSubscriptionChannel: deleting a
// handle this session does not own must not disturb the ones it does.
//
// The clear of notificationChannel was gated on the map being empty rather than on
// the handle having actually been removed, so a delete for an unknown handle wiped
// the channel whenever the map happened to be empty — exactly the state a sweep
// leaves behind. resubscribeNotifications then returns early on a nil channel while
// the reconnect logs success, and every subscription is silently dropped. This is
// the single-symbol twin of the sum-path bug that hardware caught: a power cycle
// left notifications never resuming.
func TestDeleteNotification_ForeignHandleKeepsTheSubscriptionChannel(t *testing.T) {
	srv := fakeplc.StartPLC(t)
	defer srv.Stop()
	srv.OnDeleteDeviceNotification(func(_ uint32) ams.ReturnCode { return ams.ReturnCodeNoErrors })

	sess, c := newWiredTestSession(t, srv, WithoutNotificationHeartbeat())
	c.SetNotificationHandler(sess.handleNotification)

	// The state a sweep leaves: nothing bound yet, but the caller's intent and
	// channel are on file for the re-subscribe that follows.
	ch := make(chan *Update, 4)
	sess.notifications.lock.Lock()
	sess.notifications.notificationChannel = ch
	sess.notifications.addConfig(NotificationConfig{Symbol: "MAIN.keepme"})
	sess.notifications.lock.Unlock()

	if err := sess.Unsubscribe(context.Background(), 0xDEAD); err != nil {
		t.Fatalf("delete of a foreign handle: %v", err)
	}

	sess.notifications.lock.Lock()
	channel := sess.notifications.notificationChannel
	configs := len(sess.notifications.pending)
	sess.notifications.lock.Unlock()
	if channel == nil {
		t.Errorf("notificationChannel was cleared by deleting a handle this session never owned, with %d config(s) still on file: "+
			"resubscribeNotifications returns early on a nil channel while the reconnect reports success", configs)
	}
}

// TestDeleteDeviceNotification_ClearsState pins the cleanup contract:
// after a successful DeleteDeviceNotification, the activeNotifications entry
// is removed, the corresponding notificationConfigs entry is removed, and
// when the last subscription dies notificationChannel is reset to nil.
//
// Variant (handle_invalid): when the PLC returns ReturnCodeDeviceNotifyHandleInvalid
// (0x714), Session.DeleteDeviceNotification surfaces the error to the caller
// and does NOT clean up state — only the success path clears state. This
// preserves caller signal that something was wrong.
//
// Validates: R-NOT-008 (DeleteDeviceNotification clears state on success).
func TestDeleteDeviceNotification_ClearsState(t *testing.T) {
	t.Run("success_clears_state", func(t *testing.T) {
		srv := fakeplc.StartPLC(t)
		defer srv.Stop()

		const fakeHandle uint32 = 0x11110001
		srv.OnAddDeviceNotification(func(_ fakeplc.AddNotifRequest) fakeplc.AddNotifResponse {
			return fakeplc.AddNotifResponse{Handle: fakeHandle, Error: ams.ReturnCodeNoErrors}
		})
		srv.OnDeleteDeviceNotification(func(_ uint32) ams.ReturnCode {
			return ams.ReturnCodeNoErrors
		})

		sess, _ := newWiredTestSession(t, srv)
		preSeedSymbol(sess, "MAIN.x")

		ch := make(chan *Update, 1)
		h, err := sess.Subscribe(context.Background(), "MAIN.x", 0, 0, ams.TransModeServerOnChange, ch)
		if err != nil {
			t.Fatalf("Subscribe: %v", err)
		}
		if h != fakeHandle {
			t.Fatalf("handle = 0x%X, want 0x%X", h, fakeHandle)
		}
		// Sanity: state populated.
		sess.notifications.lock.Lock()
		_, hasNotif := sess.notifications.activeNotifications[h]
		nConfigs := len(sess.notifications.pending)
		hasChan := sess.notifications.notificationChannel != nil
		sess.notifications.lock.Unlock()
		if !hasNotif || nConfigs != 1 || !hasChan {
			t.Fatalf("post-add: hasNotif=%v configs=%d chanSet=%v", hasNotif, nConfigs, hasChan)
		}

		if err := sess.Unsubscribe(context.Background(), h); err != nil {
			t.Fatalf("DeleteDeviceNotification: %v", err)
		}

		sess.notifications.lock.Lock()
		_, stillThere := sess.notifications.activeNotifications[h]
		nConfigs = len(sess.notifications.pending)
		chanNil := sess.notifications.notificationChannel == nil
		sess.notifications.lock.Unlock()
		if stillThere {
			t.Errorf("activeNotifications still contains 0x%X after delete", h)
		}
		if nConfigs != 0 {
			t.Errorf("notificationConfigs len = %d, want 0", nConfigs)
		}
		if !chanNil {
			t.Errorf("notificationChannel not nil after last delete")
		}
	})

	t.Run("handle_invalid_surfaces_error", func(t *testing.T) {
		// Production behavior pinned (Session.DeleteDeviceNotification at
		// unsubscribe.go): when the underlying client RPC returns
		// a non-success code, the wrapper returns the error before running
		// activeNotifications cleanup. So state survives the call.
		// This is what production does today; if the contract changes to
		// treat 0x714 as success-equivalent (matching SumDeleteDeviceNotification),
		// this assertion will surface the divergence.
		srv := fakeplc.StartPLC(t)
		defer srv.Stop()

		const fakeHandle uint32 = 0x22220001
		srv.OnAddDeviceNotification(func(_ fakeplc.AddNotifRequest) fakeplc.AddNotifResponse {
			return fakeplc.AddNotifResponse{Handle: fakeHandle, Error: ams.ReturnCodeNoErrors}
		})
		srv.OnDeleteDeviceNotification(func(_ uint32) ams.ReturnCode {
			return ams.ReturnCodeDeviceNotifyHandleInvalid
		})

		sess, _ := newWiredTestSession(t, srv)
		preSeedSymbol(sess, "MAIN.x")

		ch := make(chan *Update, 1)
		h, err := sess.Subscribe(context.Background(), "MAIN.x", 0, 0, ams.TransModeServerOnChange, ch)
		if err != nil {
			t.Fatalf("Subscribe: %v", err)
		}
		err = sess.Unsubscribe(context.Background(), h)
		if err == nil {
			t.Errorf("DeleteDeviceNotification with 0x714: err = nil, want non-nil (Session wrapper surfaces RPC error)")
		}
	})
}

// TestBestEffortDeleteNotifications_MixedSuccess pins the helper's
// counting + logging behavior with mixed PLC return codes. The helper
// relies on Session.SumDeleteDeviceNotification → Client.SumDeleteDeviceNotification
// for the actual PLC call; without a stub we cannot drive the mixed
// success/0x714/error response.
//
// We at least exercise the empty-handles path here (zero items returns 0).
//
// Validates: R-NOT-015 (partial — empty path; mixed-success is TODO).
func TestBestEffortDeleteNotifications_MixedSuccess(t *testing.T) {
	t.Run("empty_returns_zero", func(t *testing.T) {
		sess := newNotifTestSession()
		got := sess.bestEffortDeleteNotifications(context.Background(), nil)
		if got != 0 {
			t.Errorf("empty handles: got %d, want 0", got)
		}
	})

	t.Run("mixed_results", func(t *testing.T) {
		srv := fakeplc.StartPLC(t)
		defer srv.Stop()

		// Sum-delete returns [NoErrors, NotifyHandleInvalid, DeviceError].
		// Per bestEffortDeleteNotifications: NoErrors + NotifyHandleInvalid
		// count as success (handle gone PLC-side); DeviceError does NOT.
		srv.OnWriteRead(ams.GroupSumupDeleteDeviceNotification, func(_ []byte) []byte {
			return fakeplc.SumDeleteNotifPayload([]ams.ReturnCode{
				ams.ReturnCodeNoErrors,
				ams.ReturnCodeDeviceNotifyHandleInvalid,
				ams.ReturnCodeDeviceError,
			})
		})

		logHandler := &testlog.Handler{}
		sess, _ := newWiredTestSession(t, srv)
		sess.logger = slog.New(logHandler)

		got := sess.bestEffortDeleteNotifications(context.Background(), []uint32{1, 2, 3})
		if got != 2 {
			t.Errorf("deleted count = %d, want 2 (NoErrors + handle-invalid)", got)
		}
		// One handle did not clean up, so there must be a WARN log
		// reporting the partial cleanup.
		if logHandler.FindByMessage("some handles not cleaned up") == nil {
			t.Errorf("expected WARN 'some handles not cleaned up' (mixed-success path)")
		}
	})
}

func TestIsBestEffortDeleteSuccess(t *testing.T) {
	cases := []struct {
		name string
		code ams.ReturnCode
		want bool
	}{
		{"NoErrors", ams.ReturnCodeNoErrors, true},
		{"NotifyHandleInvalid", ams.ReturnCodeDeviceNotifyHandleInvalid, true},
		{"DeviceClientUnknown", ams.ReturnCodeDeviceClientUnknown, true},
		{"DeviceError", ams.ReturnCodeDeviceError, false},
		{"DeviceNotReady", ams.ReturnCodeDeviceNotReady, false},
		{"GlobalTargetNotFound", ams.ReturnCodeGlobalTargetNotFound, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := adsconn.IsBestEffortDeleteSuccess(tc.code); got != tc.want {
				t.Errorf("isBestEffortDeleteSuccess(%v) = %v, want %v", tc.code, got, tc.want)
			}
		})
	}
}
