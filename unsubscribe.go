package ads

import (
	"context"
)

// DeleteDeviceNotification on Session wraps the raw Client RPC with
// notifications.lock cleanup: removes the entry from activeNotifications, drops
// the cached notificationConfig, and clears notificationChannel when the
// last subscription dies. Callers that want raw delete behavior use
// the Client method directly.
func (sess *Session) DeleteDeviceNotification(ctx context.Context, handle uint32) error {
	// Snapshot symbol name BEFORE PLC RPC so a concurrent Reconnect clearing
	// activeNotifications mid-flight doesn't strand notificationConfigs (which
	// would cause resubscribeNotifications to re-subscribe a deleted symbol).
	sess.notifications.lock.Lock()
	var symbolName string
	entry, ours := sess.notifications.activeNotifications[handle]
	if ours && entry.Sym != nil {
		symbolName = entry.Sym.FullName
	}
	sess.notifications.lock.Unlock()

	err := sess.client.Load().DeleteDeviceNotification(ctx, handle)
	// 0x714 NotifyHandleInvalid and 0x715 DeviceClientUnknown mean the PLC has no
	// such registration — after a runtime restart or a dropped client identity that
	// is the normal answer, and the batch path has always counted them as
	// success-equivalent. Returning early on them stranded the entry forever (every
	// retry gets the same code) and left the config on file, so the next reconnect
	// re-subscribed a symbol the caller had deleted. Report the error, but clean up.
	if err != nil && !isBestEffortDeleteSuccessErr(err) {
		return err
	}
	sess.notifications.lock.Lock()
	if symbolName != "" {
		sess.removeNotificationConfig(symbolName)
	}
	delete(sess.notifications.activeNotifications, handle)
	// The caller asked for this one to go, so a healthy session holds one fewer.
	sess.notifications.registered.Store(int64(len(sess.notifications.activeNotifications)))
	// Gated on the handle having actually been ours, not merely on the map being
	// empty. A raw-handle caller — or one of AddSymbolNotification's own refusal
	// paths releasing a handle it never committed — would otherwise clear the
	// channel whenever the map happened to be empty, which is exactly the state a
	// sweep leaves behind. resubscribeNotifications then returns early on the nil
	// channel while the reconnect reports success, and every subscription is
	// silently dropped.
	if ours && len(sess.notifications.activeNotifications) == 0 {
		sess.notifications.notificationChannel = nil
	}
	sess.notifications.lock.Unlock()
	// Name the symbol: a handle alone cannot be tied back to a tag when
	// reading a shutdown trace, and this layer is the only one that knows the
	// mapping. Empty when the handle was not ours (raw-handle caller).
	if err != nil {
		// Cleaned up, but say what the PLC actually answered rather than reporting
		// a clean delete.
		sess.logger.Info("notification bookkeeping cleared; the PLC had already dropped the registration",
			"handle", handle, "symbol", symbolName, "error", err)
		return err
	}
	// Per handle; the aggregate "notifications deleted" line below carries the
	// counts an operator actually needs.
	sess.logger.Debug("notification deleted", "handle", handle, "symbol", symbolName)
	return nil
}

// SumDeleteDeviceNotification wraps the raw RPC with activeNotifications cleanup,
// returning the per-handle codes. On a partial result the codes processed so far
// are still flushed and both the partial slice and the error are returned, so
// in-memory state matches what the PLC saw rather than leaving phantom entries for
// the next reconnect to re-clean.
func (sess *Session) SumDeleteDeviceNotification(ctx context.Context, handles []uint32) ([]ReturnCode, error) {
	return sess.sumDeleteDeviceNotification(ctx, handles, true)
}

// sumDeleteDeviceNotification is SumDeleteDeviceNotification with control over the
// "last subscription died" bookkeeping. userTeardown=false is internal cleanup --
// reconnect and reload, which wipe activeNotifications before deleting, so the
// empty-map rule would fire every reconnect, clear notificationChannel and leave
// resubscribe returning early on a nil channel with the FSM reporting Connected.
func (sess *Session) sumDeleteDeviceNotification(ctx context.Context, handles []uint32, userTeardown bool) ([]ReturnCode, error) {
	codes, rpcErr := sess.client.Load().SumDeleteDeviceNotification(ctx, handles)
	if len(codes) == 0 {
		return codes, rpcErr
	}
	sess.notifications.lock.Lock()
	// Flush only handles for which we have a code. On partial-codes-plus-
	// error, len(codes) < len(handles) — leave the untried handles in
	// activeNotifications; the caller's retry / reconnect path picks them
	// up. min() guards against any future signature change where codes
	// might exceed handles.
	limit := len(codes)
	if len(handles) < limit {
		limit = len(handles)
	}
	deleted := 0
	for i := 0; i < limit; i++ {
		if !isBestEffortDeleteSuccess(codes[i]) {
			continue
		}
		h := handles[i]
		// Resolve the name before dropping the entry — this is the only place
		// it is still known, and a handle with no symbol is not traceable back
		// to a tag by the consumer (NotifyResult carries no handle).
		symbolName := ""
		if entry, ok := sess.notifications.activeNotifications[h]; ok && entry.Sym != nil {
			symbolName = entry.Sym.FullName
			sess.removeNotificationConfig(symbolName)
		}
		delete(sess.notifications.activeNotifications, h)
		// Only a caller teardown lowers the baseline. A release done by recovery
		// leaves it where it was, so the gap it opens is visible as want > have.
		if userTeardown {
			sess.notifications.registered.Store(int64(len(sess.notifications.activeNotifications)))
		}
		deleted++
		// Debug per handle: routine teardown of an N-symbol subscription is not
		// worth N Info lines. The summary below is the Info-worthy event.
		sess.logger.Debug("batch deleted notification handle",
			"handle", h, "symbol", symbolName, "errorCode", uint32(codes[i]))
	}
	// Only a user deleting their last subscription releases the channel, so a
	// later Add may bring a different one. Internal cleanup is mid-recovery: the
	// same channel is about to be reused by the resubscribe.
	if userTeardown && len(sess.notifications.activeNotifications) == 0 {
		sess.notifications.notificationChannel = nil
	}
	remaining := len(sess.notifications.activeNotifications)
	sess.notifications.lock.Unlock()
	if deleted > 0 {
		sess.logger.Info("notifications deleted",
			"deleted", deleted, "requested", len(handles), "remaining", remaining)
	}
	return codes, rpcErr
}

// bestEffortDeleteNotifications deletes handles for cleanup paths that cannot act
// on a failure, logging errors rather than returning them, and reports how many
// went. 0x714 and 0x715 count as gone. On *Session because it routes through the
// wrapper that keeps activeNotifications consistent with the PLC.
func (sess *Session) bestEffortDeleteNotifications(ctx context.Context, handles []uint32) int {
	if len(handles) == 0 {
		return 0
	}
	// userTeardown=false: this is recovery cleanup, not a user releasing their
	// last subscription, so it must not clear notificationChannel — the
	// resubscribe that follows needs it. See sumDeleteDeviceNotification.
	errors, err := sess.sumDeleteDeviceNotification(ctx, handles, false)
	// Count successes from any returned codes — sumDeleteNotificationFallback
	// returns partial codes alongside a non-nil error when it short-circuits
	// on transport failure, so handles cleaned up before the failure are not
	// "lost" from the operator's perspective.
	deleted := 0
	for _, code := range errors {
		if isBestEffortDeleteSuccess(code) {
			deleted++
		}
	}
	if err != nil {
		sess.logger.Warn("bestEffortDelete: SumDeleteDeviceNotification failed",
			"error", err,
			"partial_deleted", deleted,
			"handles", len(handles))
		return deleted
	}
	if deleted < len(handles) {
		sess.logger.Warn("bestEffortDelete: some handles not cleaned up",
			"deleted", deleted,
			"requested", len(handles))
	}
	return deleted
}
