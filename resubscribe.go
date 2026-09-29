package ads

import (
	"fmt"
)

// resubscribeNotifications restores the stored subscriptions after a reconnect,
// filtering out symbols that no longer exist. On error it rolls back partial
// PLC-side successes and restores the configs for the next attempt.
func (sess *Session) resubscribeNotifications() error {
	// One re-subscribe at a time, whichever path asked for it. See
	// notificationManager.resubscribeMu: the snapshot-then-clear at the top of this
	// function is what makes an overlap destructive.
	sess.notifications.resubscribeMu.Lock()
	defer sess.notifications.resubscribeMu.Unlock()
	return sess.resubscribeNotificationsLocked()
}

// resubscribeNotificationsLocked is resubscribeNotifications with resubscribeMu
// already held. Callers whose whole sequence must be atomic — snapshot the intent,
// try, restore on failure — hold the mutex across all of it and call this, rather
// than letting another path slip in between their snapshot and their restore.
func (sess *Session) resubscribeNotificationsLocked() error {
	sess.notifications.lock.Lock()
	savedPending := sess.notifications.pending
	savedChannel := sess.notifications.notificationChannel
	// Nothing to resubscribe, so nothing may be destroyed on the way out. Clearing
	// before this guard meant a no-op reporting success wiped the caller's declared
	// intent -- every symbol they never cancelled dropped from the set, with
	// "reconnect successful" logged over the top.
	if len(savedPending) == 0 || savedChannel == nil {
		sess.notifications.lock.Unlock()
		if len(savedPending) > 0 {
			// Not silent, and not a Warn: the caller cannot act on this, and the
			// intent is being KEPT. It matters only when reading back why a
			// resubscribe registered nothing.
			sess.logger.Debug("re-subscribe skipped: no channel is bound; keeping the declared subscriptions on file",
				"configs", len(savedPending))
		}
		return nil
	}
	// Clear via resetConfigs so the key-index mirror is wiped in lockstep — the
	// resubscribe below re-files every entry it commits, and a stale mirror would
	// leave the intent describing symbols this attempt has already replaced.
	sess.notifications.resetConfigs(nil)
	sess.notifications.lock.Unlock()
	validPending := sess.filterValidPending(savedPending)
	validConfigs := make([]NotificationConfig, len(validPending))
	for i, p := range validPending {
		validConfigs[i] = p.Config
	}
	if len(validConfigs) == 0 {
		// All symbols gone (e.g., PLC online change removed all subscribed vars).
		// Clear channel reference so a future AddSymbolNotification can use a new channel.
		sess.notifications.lock.Lock()
		sess.notifications.notificationChannel = nil
		// A healthy session now holds nothing, so the baseline has to say so.
		// Left where it was, the gap check would re-subscribe for ever against
		// symbols the PLC has told us it no longer has.
		sess.notifications.lowerRegisteredTo(0)
		sess.notifications.lock.Unlock()
		return nil
	}
	// What filterValidPending dropped is gone from the PLC, not missing because a
	// re-subscribe failed, so it comes off the baseline. Everything still on file --
	// including what this attempt re-queues -- stays counted, leaving a shortfall
	// visible as want > have.
	if dropped := len(savedPending) - len(validPending); dropped > 0 {
		sess.notifications.lock.Lock()
		sess.notifications.lowerRegisteredTo(len(validPending))
		sess.notifications.lock.Unlock()
		sess.logger.Info("re-subscribe: symbols are no longer on the PLC, lowering what a healthy session holds",
			"dropped", dropped, "remaining", len(validPending))
	}
	// Snapshot the handles first: on a partial success that then errors, the diff is
	// what rolls back the registrations this attempt created. Without it, repeated
	// retries accumulate orphaned notifications until the next disconnect.
	sess.notifications.lock.Lock()
	preHandles := make(map[uint32]struct{}, len(sess.notifications.activeNotifications))
	for h := range sess.notifications.activeNotifications {
		preHandles[h] = struct{}{}
	}
	sess.notifications.lock.Unlock()

	subResults, err := sess.AddSymbolNotifications(sess.currentLifecycleCtx(), validConfigs, savedChannel)

	// Skipped+Handle entries: the PLC accepted but we refused to commit, so the
	// handle is not in activeNotifications and leaks unless released here. Re-queue
	// the config for the next reconnect, dropping it after resubscribeMaxAttempts so
	// a persistently flapping symbol cannot churn for ever.
	var orphanHandles []uint32
	var retryEntries []pendingNotification
	var droppedConfigs []string
	for i, r := range subResults {
		if r.Skipped != nil && r.Handle != 0 {
			orphanHandles = append(orphanHandles, r.Handle)
		}
		if r.Skipped != nil && i < len(validPending) {
			entry := validPending[i]
			entry.resubscribeAttempts++
			if entry.resubscribeAttempts >= resubscribeMaxAttempts {
				droppedConfigs = append(droppedConfigs, entry.Config.SymbolName)
				continue
			}
			retryEntries = append(retryEntries, entry)
		}
	}
	if len(orphanHandles) > 0 {
		deleted := sess.bestEffortDeleteNotifications(sess.currentLifecycleCtx(), orphanHandles)
		sess.logger.Warn("resubscribe: released PLC handles for Skipped+Handle entries",
			"orphan_handles", len(orphanHandles),
			"deleted", deleted)
	}
	if len(retryEntries) > 0 {
		sess.notifications.lock.Lock()
		for _, p := range retryEntries {
			sess.notifications.addPending(p)
		}
		sess.notifications.lock.Unlock()
		sess.logger.Info("resubscribe: queued Skipped configs for next reconnect retry",
			"retry_count", len(retryEntries))
	}
	if len(droppedConfigs) > 0 {
		// Abandoned for good, so they stop counting toward healthy -- otherwise
		// the gap they leave is permanent and recovery retries them for the life
		// of the session, which is the churn resubscribeMaxAttempts exists to stop.
		sess.notifications.lock.Lock()
		sess.notifications.lowerRegisteredTo(len(validPending) - len(droppedConfigs))
		sess.notifications.lock.Unlock()
		sess.logger.Error("resubscribe: dropping configs after max retries",
			"dropped", droppedConfigs,
			"max_attempts", resubscribeMaxAttempts)
	}

	if err != nil {
		// Identify handles created during THIS attempt and best-effort delete.
		sess.notifications.lock.Lock()
		var newHandles []uint32
		for h := range sess.notifications.activeNotifications {
			if _, existed := preHandles[h]; !existed {
				newHandles = append(newHandles, h)
				// Drop client-side bookkeeping for the rollback handles.
				delete(sess.notifications.activeNotifications, h)
			}
		}
		// Restore configs so they can be retried by the next reconnect attempt.
		// resetConfigs rebuilds the key-index mirror to match savedPending.
		sess.notifications.resetConfigs(savedPending)
		sess.notifications.notificationChannel = savedChannel
		sess.notifications.lock.Unlock()
		if len(newHandles) > 0 {
			deleted := sess.bestEffortDeleteNotifications(sess.currentLifecycleCtx(), newHandles)
			sess.logger.Warn("resubscribe rollback: deleted partial-success handles",
				"new_handles", len(newHandles),
				"deleted", deleted)
		}
		return err
	}

	// Report what came back: "reconnect successful" says the socket is up, not that
	// data is flowing, and a session that returned short otherwise reads as healthy
	// everywhere. This is the only place the shortfall is visible.
	sess.notifications.lock.Lock()
	restored := len(sess.notifications.activeNotifications)
	sess.notifications.lock.Unlock()
	// Same message text the initial subscribe logs, with the counts as fields
	// rather than formatted into it: a bridge that dropped and came back reports
	// the event the same way whether it was a first connect or a recovery, and
	// the counts stay filterable instead of being baked into the string.
	if restored < len(validConfigs) {
		sess.logger.Error(fmt.Sprintf(
			"Registering notifications restored only %d/%d symbols; the missing ones deliver nothing until a later attempt restores them",
			restored, len(validConfigs)))
	} else {
		sess.logger.Info(fmt.Sprintf("Registering notifications succeeded for %d/%d symbols",
			restored, len(validConfigs)))
	}
	return nil
}
