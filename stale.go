package ads

import (
	"fmt"
	"time"

	"github.com/siyka-au/go-ads/v3/ams"
)

// handleStaleDetection runs the configured online-change strategy for a PLC code
// in the R-CACHE-009 set, reporting whether it handled the code. The user callback
// fires in its own goroutine (R-SES-007). Ignore surfaces the error unchanged,
// Close terminates asynchronously, AutoReload reloads and resubscribes.
func (sess *Session) handleStaleDetection(rc ams.ReturnCode) (stale bool, reason Reason) {
	stale, reason = detectStaleCache(rc)
	if !stale {
		return false, ""
	}
	sess.logger.Warn("stale-cache detection",
		"code", rc, "reason", reason, "strategy", sess.versionStrategy)
	switch sess.versionStrategy {
	case SymbolVersionIgnore:
		// Mark all active notification handles stale — next sample for each
		// handle will carry Update.Stale=true with this reason. The original
		// error surfaces to the calling op via the existing errors.As
		// intercept.
		sess.markAllHandlesStale(reason)
		if sess.versionCallback != nil {
			go sess.versionCallback(reason)
		}
	case SymbolVersionClose:
		if sess.versionCallback != nil {
			go sess.versionCallback(reason)
		}
		go sess.closeOnStaleDetection(reason)
	case SymbolVersionAutoReload:
		// CAS gates both the reload goroutine AND the callback so N
		// concurrent triggers fire one callback total (R-SES-011
		// "once per detection"). Without this, the callback was launched
		// unconditionally above and N triggers fired N callbacks.
		if sess.reloadInProgress.CompareAndSwap(false, true) {
			if sess.versionCallback != nil {
				go sess.versionCallback(reason)
			}
			go sess.autoReloadOnStaleDetection(reason)
		}
	}
	return true, reason
}

// markSymbolStale flags the next notification sample for handle h to be
// delivered with Update.Stale=true and Update.Reason=reason. One-shot —
// consumed on first delivery via consumeStaleFlag (R-NOT-017).
func (sess *Session) markSymbolStale(handle uint32, reason Reason) {
	sess.staleHandlesMu.Lock()
	defer sess.staleHandlesMu.Unlock()
	if sess.staleHandles == nil {
		sess.staleHandles = map[uint32]Reason{}
	}
	sess.staleHandles[handle] = reason
}

// consumeStaleFlag returns the pending stale reason for handle h and
// clears the entry. Returns ("", false) if no pending flag (R-NOT-017).
func (sess *Session) consumeStaleFlag(handle uint32) (Reason, bool) {
	sess.staleHandlesMu.Lock()
	defer sess.staleHandlesMu.Unlock()
	r, ok := sess.staleHandles[handle]
	if ok {
		delete(sess.staleHandles, handle)
	}
	return r, ok
}

// markAllHandlesStale flags every active handle's next sample with reason. Lock
// order: notifications.lock then staleHandlesMu, never cache.lock. Nil-guarded for
// bare Session{} literals in tests.
func (sess *Session) markAllHandlesStale(reason Reason) {
	if sess.notifications == nil {
		return
	}
	sess.notifications.lock.Lock()
	handles := make([]uint32, 0, len(sess.notifications.activeNotifications))
	for h := range sess.notifications.activeNotifications {
		handles = append(handles, h)
	}
	sess.notifications.lock.Unlock()
	for _, h := range handles {
		sess.markSymbolStale(h, reason)
	}
}

// closeOnStaleDetection terminates the session under SymbolVersionClose, in its
// own goroutine so the calling Read/Write does not block. Fires onDisconnect
// before Close so observers see the event even though we initiated it.
func (sess *Session) closeOnStaleDetection(reason Reason) {
	sess.logger.Info("Close strategy fired on stale-cache detection", "reason", reason)
	if sess.onDisconnect != nil && !sess.isClosed() {
		go sess.onDisconnect()
	}
	if err := sess.Close(); err != nil {
		sess.logger.Warn("Close error during stale-detection shutdown", "err", err)
	}
}

// tryRecordReloadAttempt prunes attempts outside the sliding window then
// records a new attempt and returns true. Returns false when the cap is
// exhausted within the window — caller MUST then degrade to Ignore
// (R-CACHE-013).
func (sess *Session) tryRecordReloadAttempt() bool {
	sess.reloadMu.Lock()
	defer sess.reloadMu.Unlock()
	now := time.Now()
	cutoff := now.Add(-sess.reloadWindow)
	pruned := sess.reloadAttempts[:0]
	for _, t := range sess.reloadAttempts {
		if t.After(cutoff) {
			pruned = append(pruned, t)
		}
	}
	sess.reloadAttempts = pruned
	if len(sess.reloadAttempts) >= sess.maxReloadAttempts {
		return false
	}
	sess.reloadAttempts = append(sess.reloadAttempts, now)
	return true
}

// autoReloadOnStaleDetection re-discovers and resubscribes under
// SymbolVersionAutoReload. Capped (R-CACHE-013): on exhaustion it warns, fires
// ReasonReloadCapExhausted and degrades to Ignore until the window slides out.
func (sess *Session) autoReloadOnStaleDetection(reason Reason) {
	defer sess.reloadInProgress.Store(false)
	if !sess.tryRecordReloadAttempt() {
		sess.logger.Warn("reload cap exhausted - degrading to Ignore",
			"reason", reason, "max", sess.maxReloadAttempts, "window", sess.reloadWindow)
		if sess.versionCallback != nil {
			go sess.versionCallback(ReasonReloadCapExhausted)
		}
		return
	}

	if sess.isClosed() {
		sess.logger.Debug("auto-reload skipped - session closed", "reason", reason)
		return
	}

	// Mark surviving handles Stale during reload window — any sample that
	// sneaks through the old handle pre-resubscribe carries
	// Reason=ReasonReloadInProgress so consumers can distinguish in-flight
	// from post-reload data.
	sess.markAllHandlesStale(ReasonReloadInProgress)

	sess.logger.Info("auto-reload starting", "reason", reason)
	// Bump epoch first so any in-flight retry helpers observing epoch
	// will see the change immediately (R-CACHE-003).
	sess.bumpEpoch()
	// Zero old handles so callers holding *symbol pointers force
	// on-demand re-resolution (R-CACHE-004).
	sess.cache.lock.Lock()
	zeroOldSymbolHandles(sess.cache.symbols)
	sess.cache.lock.Unlock()

	if err := sess.reloadSymbolsAndResubscribe(); err != nil {
		sess.logger.Error("auto-reload failed", "err", err)
		return
	}
	sess.logger.Info("auto-reload complete")
	if sess.onReconnect != nil {
		go sess.onReconnect()
	}
}

// reloadSymbolsAndResubscribe re-runs discovery and resubscribes after an online
// change, deleting the old handles first: left behind they hold table slots for
// ~10 min and repeated changes flood the router (Beckhoff #268).
func (sess *Session) reloadSymbolsAndResubscribe() error {
	// Transport is alive here, so quiesce dispatch: samples still arriving for the
	// handles being deleted are race-window noise, not orphans.
	oldHandles := sess.takeNotificationHandles(true)
	sess.releaseNotificationHandles(sess.currentLifecycleCtx(), oldHandles, "auto-reload before resubscribe")

	if err := sess.LoadSymbols(sess.currentLifecycleCtx()); err != nil {
		return fmt.Errorf("LoadSymbols: %w", err)
	}
	return sess.resubscribeNotifications()
}

// reloadSymbols re-establishes the symbol table after a reconnect, matching
// the discovery mode that was used before the connection dropped.
func (sess *Session) reloadSymbols() error {
	sess.cache.lock.Lock()
	fullyLoaded := sess.cache.symbolsFullyLoaded
	listLoaded := sess.cache.symbolListLoaded
	dtLoaded := sess.cache.datatypesLoaded
	hasOnDemand := len(sess.cache.onDemandSymbols) > 0
	sess.cache.lock.Unlock()

	switch {
	case fullyLoaded:
		// Full discovery was done — redo it
		return sess.loadSymbols(sess.currentLifecycleCtx())

	case listLoaded || dtLoaded:
		// Partial discovery — re-download what was loaded
		if listLoaded {
			if err := sess.LoadSymbolList(sess.currentLifecycleCtx(), SlowDiscoveryConfig{}); err != nil {
				return fmt.Errorf("reload symbol list: %w", err)
			}
		}
		if dtLoaded {
			if err := sess.LoadDataTypes(sess.currentLifecycleCtx(), SlowDiscoveryConfig{}); err != nil {
				return fmt.Errorf("reload datatypes: %w", err)
			}
		}

	case hasOnDemand:
		// Re-resolve only previously loaded symbols; missing ones are skipped unless
		// WithStrictReconnect. Snapshot the requested set before wiping
		// cache.symbols and leave onDemandSymbols alone, or a partial success on one
		// retry drops the failed names from the next one's set.
		sess.cache.lock.Lock()
		oldSymbols := make(map[string]bool, len(sess.cache.onDemandSymbols))
		for k, v := range sess.cache.onDemandSymbols {
			oldSymbols[k] = v
		}
		sess.cache.symbols = make(map[string]*symbol)
		sess.bumpEpoch()
		sess.cache.lock.Unlock()

		for name := range oldSymbols {
			if _, err := sess.getSymbol(sess.currentLifecycleCtx(), name); err != nil {
				if sess.lifecycle.strictReconnect {
					sess.lifecycle.strictReconnectFailures++
					if sess.lifecycle.strictReconnectMaxAttempts == 0 || sess.lifecycle.strictReconnectFailures > sess.lifecycle.strictReconnectMaxAttempts {
						return fmt.Errorf("re-resolve symbol %q (strict mode, %d failures): %w", name, sess.lifecycle.strictReconnectFailures, err)
					}
					return fmt.Errorf("re-resolve symbol %q (strict mode, attempt %d/%d): %w", name, sess.lifecycle.strictReconnectFailures, sess.lifecycle.strictReconnectMaxAttempts, err)
				}
				sess.logger.Warn("on-demand symbol unavailable after reconnect, skipping",
					"symbol", name, "error", err)
			}
		}

	default:
		// No symbols were loaded — read symbol version for future use
		version, err := sess.client.Load().GetSymbolVersion(sess.currentLifecycleCtx())
		if err != nil {
			sess.logger.Debug("could not read symbol version during reconnect", "error", err)
		} else {
			sess.cache.lock.Lock()
			sess.cache.symbolVersion = version
			sess.cache.lock.Unlock()
		}
	}

	return nil
}

// SymbolVersionStrategy selects online-change handling behavior (R-SES-011).
type SymbolVersionStrategy uint8

const (
	// SymbolVersionAutoReload is the default. On detection of an online
	// change, the Session re-runs symbol discovery and resubscribes
	// notifications. Bounded by WithMaxSymbolVersionReloadAttempts within
	// a sliding window (default 3 attempts / 60s).
	SymbolVersionAutoReload SymbolVersionStrategy = iota

	// SymbolVersionClose terminates the Session on detection. The
	// OnDisconnect callback fires and Session.Close() is invoked. The
	// caller decides reconnect timing.
	SymbolVersionClose

	// SymbolVersionIgnore surfaces the PLC error verbatim and flags the next sample
	// on surviving handles Stale, once. Removed symbols go SILENT instead -- no
	// terminal Update for a dead handle, so use WithOnSymbolVersionChanged to see
	// removals.
	SymbolVersionIgnore
)

// String returns the human-readable name of the strategy.
func (s SymbolVersionStrategy) String() string {
	switch s {
	case SymbolVersionAutoReload:
		return "AutoReload"
	case SymbolVersionClose:
		return "Close"
	case SymbolVersionIgnore:
		return "Ignore"
	default:
		return "Unknown"
	}
}

// Reason is the enumerated cause of a stale-cache detection or one-shot
// Update.Stale notification. Values are STABLE — safe for switch/case
// comparison by callers. New reasons may be added in future versions;
// callers SHOULD have a default branch.
type Reason string

const (
	ReasonSymbolVersionInvalid Reason = "symbol-version-invalid"
	ReasonSymbolNotFound       Reason = "symbol-not-found"
	ReasonInvalidOffset        Reason = "invalid-offset"
	ReasonSymbolNotActive      Reason = "symbol-not-active"
	ReasonNotifyHandleInvalid  Reason = "notify-handle-invalid"
	ReasonInvalidSize          Reason = "invalid-size"
	ReasonReloadCapExhausted   Reason = "reload-cap-exhausted"
	ReasonReloadInProgress     Reason = "reload-in-progress"
	// ReasonHeartbeatSilent is delivered when the internal heartbeat has gone
	// silent and the session is NOT re-subscribing because the caller chose
	// WithHeartbeatRecovery(HeartbeatRecoveryObserve). It is the only signal that
	// mode produces, and it means this session's subscriptions are dead until the
	// consumer rebuilds it.
	ReasonHeartbeatSilent Reason = "heartbeat-silent"
)

// detectStaleCache classifies a PLC return code against the R-CACHE-009
// detection set. Returns (true, reason) for codes that signal cache
// staleness from a PLC online change; (false, "") otherwise.
//
// Detection codes verified against Beckhoff InfoSys (TC2 Utilities).
func detectStaleCache(rc ams.ReturnCode) (stale bool, reason Reason) {
	switch rc {
	case ams.ReturnCodeDeviceSymbolVersionInvalid: // 0x711 — Beckhoff: "online change. Create a new handle."
		return true, ReasonSymbolVersionInvalid
	case ams.ReturnCodeDeviceSymbolNoFound: // 0x710
		return true, ReasonSymbolNotFound
	case ams.ReturnCodeDeviceInvalidOffset: // 0x703 — TC3 surfaces this on cached handle post-delete
		return true, ReasonInvalidOffset
	case ams.ReturnCodeDeviceSymbolNotActive: // 0x722 — Beckhoff: "Release the handle and try again."
		return true, ReasonSymbolNotActive
	case ams.ReturnCodeDeviceNotifyHandleInvalid: // 0x714
		return true, ReasonNotifyHandleInvalid
	case ams.ReturnCodeDeviceInvalidSize: // 0x705 — surfaces when cached symbol.Length disagrees with PLC's new size post-online-change (e.g. operator toggle INT↔LREAL)
		return true, ReasonInvalidSize
	}
	return false, ""
}
