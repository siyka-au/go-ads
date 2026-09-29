package ads

import (
	"context"
	"errors"
	"time"
)

// Orphan-Delete frees handles left by a prior process sharing our source
// NetID+port; uncleaned they fill the PLC's handle table (Beckhoff #268).
// Throttled per handle, concurrency-bounded, and re-checked against
// activeNotifications before firing so we never delete our own subscription.
const (
	orphanDeleteThrottle       = 60 * time.Second
	orphanDeleteMaxConcurrency = 10
	orphanDeleteSeenMaxAge     = 5 * time.Minute
	orphanDeleteRPCTimeout     = 5 * time.Second
)

// isBestEffortDeleteSuccess reports whether the handle is gone, which is all
// best-effort cleanup wants: deleted, 0x714 (already gone), or 0x715 (client
// identity dropped, so our handles went with it). Beckhoff's AdsLib refuses
// 0x715; here it is routine on reconnect and counting it as failure is spam.
func isBestEffortDeleteSuccess(code ReturnCode) bool {
	return code == ReturnCodeNoErrors ||
		code == ReturnCodeDeviceNotifyHandleInvalid ||
		code == ReturnCodeDeviceClientUnknown
}

// isBestEffortDeleteSuccessErr is the error-wrapped variant of
// isBestEffortDeleteSuccess for call sites that receive a Go error rather
// than a bare ReturnCode (e.g., orphan-Delete's RPC return). Unwraps the
// error chain via errors.Is for ReturnCodeDeviceNotifyHandleInvalid and
// ReturnCodeDeviceClientUnknown — the two codes that mean "PLC has
// nothing of yours to free".
func isBestEffortDeleteSuccessErr(err error) bool {
	if err == nil {
		return true
	}
	return errors.Is(err, ReturnCodeDeviceNotifyHandleInvalid) ||
		errors.Is(err, ReturnCodeDeviceClientUnknown)
}

// orphanDeleteAbortReason re-checks, immediately before the RPC, whether the
// handle is ours after all: both windows open between scheduling and firing, and
// deleting a live subscription is the failure this area exists to prevent. Named
// so it can be tested directly rather than raced against inside a goroutine.
func (sess *Session) orphanDeleteAbortReason(handle uint32) (string, bool) {
	mgr := sess.notifications
	// A concurrent subscribe may have committed this very handle since the delete
	// was scheduled: handle IDs are not recycled (allocation is monotonic on both
	// TC2 and TC3), but a sample can arrive before its own commit lands.
	mgr.lock.Lock()
	_, present := mgr.activeNotifications[handle]
	mgr.lock.Unlock()
	if present {
		return "handle reappeared in activeNotifications", true
	}
	// Or a subscribe started after we scheduled this and simply has not
	// committed its handles yet, so absence is not proof of a leak.
	if mgr.subscribeInFlight.Load() > 0 {
		return "a subscribe is in flight", true
	}
	return "", false
}

func (sess *Session) tryOrphanDelete(handle uint32) {
	// Lifecycle guards: never fire during shutdown or active reconnect.
	if sess.isClosed() || sess.isReconnecting() {
		sess.logger.Debug("orphan delete skipped: session closing or reconnecting", "handle", handle)
		return
	}

	// Throttle map + bounded sem may be uninitialised on Session{} struct
	// literals used in some tests. Skip silently — these tests don't
	// exercise the orphan path.
	mgr := sess.notifications
	if mgr.orphanSeen == nil || mgr.orphanSem == nil {
		return
	}

	// Throttle check + GC old entries under orphanMu. Don't write the
	// throttle entry yet — only commit it after we've successfully
	// acquired a sem slot, otherwise a sem-full drop would 60s-lock the
	// handle even though no RPC fired.
	mgr.orphanMu.Lock()
	now := time.Now()
	cutoff := now.Add(-orphanDeleteSeenMaxAge)
	for h, t := range mgr.orphanSeen {
		if t.Before(cutoff) {
			delete(mgr.orphanSeen, h)
		}
	}
	if last, seen := mgr.orphanSeen[handle]; seen && now.Sub(last) < orphanDeleteThrottle {
		mgr.orphanMu.Unlock()
		sess.logger.Debug("orphan delete throttled (recent attempt for same handle)",
			"handle", handle,
			"last_attempt_ago", now.Sub(last))
		return
	}
	mgr.orphanMu.Unlock()

	// Bounded concurrency: non-blocking acquire. If sem full, drop this
	// attempt without writing throttle entry; the next orphan sample
	// retries immediately rather than waiting out a 60s throttle window.
	select {
	case mgr.orphanSem <- struct{}{}:
	default:
		sess.logger.Debug("orphan delete skipped: max concurrent deletes in flight",
			"handle", handle,
			"max_concurrent", orphanDeleteMaxConcurrency)
		return
	}

	// Record the attempt now, before scheduling: a high-rate orphan stream would
	// otherwise slip several deletes through the gap before the goroutine runs.
	// The goroutine clears it again if it aborts, so an attempt that sends no RPC
	// does not lock the handle out for the throttle window.
	mgr.orphanMu.Lock()
	mgr.orphanSeen[handle] = now
	mgr.orphanMu.Unlock()

	// Track the goroutine so Close waits for in-flight orphan deletes. Via
	// trackGoroutine, because the isClosed() check far above is a TOCTOU: Close can
	// finish in the gap and its Wait return before this Add lands, which panics the
	// process. Refusal means releasing what was reserved for the goroutine.
	started := sess.trackGoroutine(func() {
		defer func() { <-mgr.orphanSem }()
		defer func() {
			if r := recover(); r != nil {
				sess.logger.Error("orphan delete goroutine panic recovered",
					"handle", handle, "panic", r)
			}
		}()

		if reason, abort := sess.orphanDeleteAbortReason(handle); abort {
			// No RPC went out, so do not hold the throttle: a genuinely leaked
			// handle would otherwise go unreaped for the whole window because it
			// happened to fire while someone was subscribing.
			mgr.orphanMu.Lock()
			delete(mgr.orphanSeen, handle)
			mgr.orphanMu.Unlock()
			sess.logger.Debug("orphan delete aborted", "handle", handle, "reason", reason)
			return
		}

		c := sess.client.Load()
		if c == nil {
			sess.logger.Debug("orphan delete aborted: client not initialised", "handle", handle)
			return
		}

		// Snapshot lifecycle.ctx under ctxMu.RLock — Reconnect replaces it
		// via tearDownAndReset under ctxMu.Lock, so a raw read races with
		// the swap. RLock + snapshot matches the pattern at session.go's
		// Close/triggerReconnect call sites.
		sess.lifecycle.ctxMu.RLock()
		parentCtx := sess.lifecycle.ctx
		sess.lifecycle.ctxMu.RUnlock()
		if err := parentCtx.Err(); err != nil {
			sess.logger.Debug("orphan delete aborted: lifecycle context done", "handle", handle, "error", err)
			return
		}

		ctx, cancel := context.WithTimeout(parentCtx, orphanDeleteRPCTimeout)
		defer cancel()
		if err := c.DeleteDeviceNotification(ctx, handle); err != nil {
			// 0x714 (already reaped) and 0x715 (client identity dropped, so the handle
			// went with it) are expected and stay at Debug, or they flood under a
			// high-rate orphan stream. Everything else is a real failure at Warn.
			if isBestEffortDeleteSuccessErr(err) {
				sess.logger.Debug("orphan delete RPC: handle already gone PLC-side (expected)",
					"handle", handle, "error", err)
				return
			}
			sess.logger.Warn("orphan delete RPC failed",
				"handle", handle, "error", err)
			return
		}
		// State the observation, not a guess at the cause. The old wording
		// claimed the handle "was leaked by a prior session/process" — during
		// the v2.2.0 subscribe-race regression it was saying that about
		// subscriptions this very session had created milliseconds earlier,
		// which sent the diagnosis in the wrong direction for months.
		// Rate-limited with a count: a reconnect leaves one orphan per handle, so
		// this arrives 40-odd at a time and one line per handle says nothing extra.
		d := sess.notifications.orphanDeletes.Add(1)
		if reportNow(&sess.notifications.orphanDeleteNs, orphanReportInterval) {
			sess.logger.Info("deleted a PLC notification handle this session does not own",
				"handle", handle, "deletedSinceLastReport", d,
				"hint", "usually a subscription left behind by an earlier process sharing this source NetID and port")
			sess.notifications.orphanDeletes.Store(0)
		} else {
			sess.logger.Debug("deleted a PLC notification handle this session does not own",
				"handle", handle, "deletedSinceLastReport", d)
		}
	})
	if !started {
		// Release what was reserved for a goroutine that will not run: the semaphore
		// slot, and the throttle entry — a genuinely leaked handle should not be
		// locked out for the whole window because it fired as the session closed.
		<-mgr.orphanSem
		mgr.orphanMu.Lock()
		delete(mgr.orphanSeen, handle)
		mgr.orphanMu.Unlock()
		sess.logger.Debug("orphan delete not started: the session is closed", "handle", handle)
	}
}
