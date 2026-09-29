package ads

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/siyka-au/go-ads/v3/internal/adsconn"

	"github.com/siyka-au/go-ads/v3/internal/symtab"

	"github.com/siyka-au/go-ads/v3/ams"
)

// notificationReleaseTimeout bounds the cleanup delete a batch issues for
// handles it declined to bind, when the caller's own context is already done.
// Short on purpose: the caller has stopped waiting, and the handles die with the
// connection anyway if this does not land.
const notificationReleaseTimeout = 2 * time.Second

// releaseCleanupCtx picks the context for releasing handles the library declined
// to bind: a usable caller ctx as-is, a done one replaced, since the handles exist
// on the PLC either way. The replacement does NOT inherit cancellation -- the
// batch may have aborted because the session is closing -- so WithoutCancel keeps
// the values and a timeout bounds it. A nil lifecycle ctx falls back to Background.
func (sess *Session) releaseCleanupCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx.Err() == nil {
		return ctx, func() {}
	}
	parent := context.Background()
	if sess.lifecycle != nil {
		if live := sess.currentLifecycleCtx(); live != nil {
			parent = context.WithoutCancel(live)
		}
	}
	return context.WithTimeout(parent, notificationReleaseTimeout)
}

// releaseUncommittedHandle gives back a handle acquired but never committed.
// Deliberately the raw Client call: the Session wrapper retires subscriptions the
// caller owns, and using it here cleared notificationChannel whenever
// activeNotifications was empty -- the state every sweep leaves behind. The
// release must also outlive a cancelled caller ctx, or a deadline expiring during
// the round-trip leaves the PLC streaming a subscription nobody owns.
func (sess *Session) releaseUncommittedHandle(ctx context.Context, handle uint32) error {
	releaseCtx, cancel := sess.releaseCleanupCtx(ctx)
	defer cancel()
	return sess.client.Load().DeleteDeviceNotification(releaseCtx, handle)
}

// takeNotificationHandles empties activeNotifications and returns every handle the
// PLC still holds, heartbeat included -- the point, since the heartbeat is not in
// that map and callers open-coding this forgot it, leaving the session with no beat.
// quiesceDispatch makes in-flight samples for the doomed handles read as race-window
// noise; the heartbeat clock restarts rather than zeroing, which would park the watcher.
func (sess *Session) takeNotificationHandles(quiesceDispatch bool) []uint32 {
	sess.notifications.lock.Lock()
	handles := make([]uint32, 0, len(sess.notifications.activeNotifications)+1)
	for h := range sess.notifications.activeNotifications {
		handles = append(handles, h)
	}
	if quiesceDispatch {
		sess.notifications.lastSubscribeNs.Store(time.Now().UnixNano())
	}
	sess.notifications.activeNotifications = make(map[uint32]activeNotification)
	sess.notifications.lock.Unlock()

	if hb := sess.notifications.heartbeatHandle.Swap(0); hb != 0 {
		handles = append(handles, hb)
	}
	sess.notifications.heartbeatLastNs.Store(time.Now().UnixNano())
	return handles
}

// releaseNotificationHandles best-effort deletes handles PLC-side and says how
// many the PLC confirmed. why names the caller in the log, since "requested=N
// deleted=M" is otherwise untraceable in a shutdown trace.
//
// Note when reading that log: bestEffortDelete counts 0x714/0x715 as
// success-equivalent, so deleted=N does not prove N registrations existed.
func (sess *Session) releaseNotificationHandles(ctx context.Context, handles []uint32, why string) int {
	if len(handles) == 0 {
		return 0
	}
	deleted := sess.bestEffortDeleteNotifications(ctx, handles)
	sess.logger.Info("released PLC notification handles", "why", why,
		"requested", len(handles), "deleted", deleted)
	return deleted
}

// AddSymbolNotification subscribes a single symbol. All notifications on one
// connection share the same channel, and a duplicate symbol is rejected; the
// stored channel is reused to re-subscribe after a reconnect. Prefer
// AddSymbolNotifications for more than one.
//
// The caller MUST NOT close updateReceiver while any notification is active -- a
// recover guards against it, but samples are silently dropped. Delete them or
// Close first.
func (sess *Session) AddSymbolNotification(ctx context.Context, symbolName string, maxDelay time.Duration, cycleTime time.Duration, transMode ams.TransMode, updateReceiver chan *Update) (uint32, error) {
	// Refuse outside RUN rather than produce a misleading failure: in CONFIG the
	// runtime port does not exist, so this cannot succeed, and the PLC's answer is
	// an AMS "port not found" rather than anything about symbols. Permits when no
	// state has been observed — see requireRunningRuntime.
	if err := sess.requireRunningRuntime("AddSymbolNotification"); err != nil {
		return 0, err
	}
	// Pre-check: channel match + duplicate-subscribe.
	sess.notifications.lock.Lock()
	if sess.notifications.notificationChannel != nil && sess.notifications.notificationChannel != updateReceiver {
		sess.notifications.lock.Unlock()
		return 0, fmt.Errorf("all symbol notifications on a connection must use the same updateReceiver channel")
	}
	if sess.notifications.hasLiveNotification(symbolName) {
		sess.notifications.lock.Unlock()
		return 0, fmt.Errorf("symbol %q already has an active notification; delete it before re-subscribing", symbolName)
	}
	sess.notifications.lock.Unlock()

	symbol, err := sess.getSymbol(ctx, symbolName)
	if err != nil {
		return 0, fmt.Errorf("notification for %q: %w", symbolName, err)
	}

	// Auto-fallback: InContext modes (5/6) require non-zero ContextMask on the symbol.
	// If ContextMask is 0 (single-task PLC, TC2, or variable not bound to a task),
	// downgrade to the regular mode (3/4) to avoid 0x070B errors or silent failures.
	actualMode := transMode
	if (transMode == ams.TransModeServerCycle2 || transMode == ams.TransModeServerOnChange2) && symbol.ContextMask == 0 {
		actualMode = adsconn.DowngradeTransMode(transMode)
		sess.logger.Warn("InContext mode not available for symbol (ContextMask=0), falling back",
			"symbol", symbolName,
			"requested", transMode.String(),
			"using", actualMode.String(),
			"flags", fmt.Sprintf("0x%04X", uint32(symbol.Flags)))
	}

	// Open the subscribe window BEFORE the RPC: the PLC-side handle exists as
	// soon as Add returns, so a first sample can arrive before the commit
	// below. endSubscribe replays anything buffered for the handle we commit
	// and closes the window. Deferred here so it runs after the
	// notifications.lock unlock defer registered further down (LIFO).
	subTok := sess.beginSubscribe()
	committed := make([]uint32, 0, 1)
	defer func() { sess.endSubscribe(ctx, subTok, committed) }()

	handle, err := sess.client.Load().AddDeviceNotification(ctx,
		uint32(ams.GroupSymbolValueByHandle),
		symbol.Handle,
		symbol.Length,
		actualMode,
		maxDelay,
		cycleTime)
	if err != nil {
		// Online-change detection: the request carried a cached handle, so a
		// stale-cache code means it is dead. On TC3 a runtime restart answers 0x710
		// without bumping the symbol version, making this the only signal there is.
		// Detection only -- subscribe is not idempotent, and a retry racing the
		// reload's own resubscribe would double-register the symbol.
		var rc ams.ReturnCode
		if errors.As(err, &rc) {
			sess.handleStaleDetection(rc)
		}
		return 0, err
	}
	// A subscription now exists, so it is worth protecting.
	_ = sess.establishHeartbeat(ctx)
	// Per-subscription, so it scales with the caller's symbol count. See the note
	// on "symbol resolved on-demand".
	sess.logger.Debug("notification created",
		"handle", handle,
		"symbol", symbolName,
		"mode", actualMode.String())
	// Subscribe time is when an unresolvable base type is actionable: the operator
	// can still load the datatype table before samples start arriving as
	// unconverted strings. Latched per symbol, so the read path stays quiet.
	sess.warnUnresolvedBaseType(symbolName)
	// Re-fetch *symbol from cache before commit. The pointer obtained
	// pre-roundtrip may be orphaned if loadSymbols swapped the cache during
	// the network call; using the stranded pointer would write notifications
	// into a symbol no longer reachable via ReadFromSymbol. Take cache.lock
	// FIRST and release before taking notifications.lock (lock-ordering rule).
	// Capture epoch under the lock; re-check it under notifications.lock
	// (atomic Load is lock-free) to close the residual race window where a
	// THIRD loadSymbols could run between cache.lock release and notifications.lock
	// acquire and re-strand `fresh`.
	sess.cache.lock.Lock()
	fresh := sess.cache.symbols[symtab.Key(symbolName)]
	cacheGen := sess.epoch()
	sess.cache.lock.Unlock()
	if fresh == nil {
		if delErr := sess.releaseUncommittedHandle(ctx, handle); delErr != nil {
			sess.logger.Warn("failed to release PLC handle after symbol vanished",
				"handle", handle, "symbol", symbolName, "error", delErr)
		}
		return 0, fmt.Errorf("symbol %q removed from cache during subscribe (likely online change or LoadSymbols)", symbolName)
	}
	// Re-check under notifications.lock to defend the channel/duplicate invariants
	// against concurrent callers that passed the pre-check while we were
	// doing the PLC roundtrip. On mismatch, release the just-acquired PLC handle.
	sess.notifications.lock.Lock()
	if sess.epoch() != cacheGen {
		sess.notifications.lock.Unlock()
		if delErr := sess.releaseUncommittedHandle(ctx, handle); delErr != nil {
			sess.logger.Warn("failed to release PLC handle after cache reload during subscribe",
				"handle", handle, "symbol", symbolName, "error", delErr)
		}
		return 0, fmt.Errorf("symbol %q stranded by concurrent cache reload during subscribe", symbolName)
	}
	if sess.notifications.notificationChannel != nil && sess.notifications.notificationChannel != updateReceiver {
		sess.notifications.lock.Unlock()
		if delErr := sess.releaseUncommittedHandle(ctx, handle); delErr != nil {
			sess.logger.Warn("failed to release PLC handle after channel-mismatch reject",
				"handle", handle, "symbol", symbolName, "error", delErr)
		}
		return 0, fmt.Errorf("all symbol notifications on a connection must use the same updateReceiver channel")
	}
	if sess.notifications.hasLiveNotification(symbolName) {
		sess.notifications.lock.Unlock()
		if delErr := sess.releaseUncommittedHandle(ctx, handle); delErr != nil {
			sess.logger.Warn("failed to release PLC handle after duplicate-subscribe reject",
				"handle", handle, "symbol", symbolName, "error", delErr)
		}
		return 0, fmt.Errorf("symbol %q already has an active notification; delete it before re-subscribing", symbolName)
	}
	defer sess.notifications.lock.Unlock()
	sess.notifications.activeNotifications[handle] = activeNotification{Sym: fresh, Ch: updateReceiver}
	// The PLC gave us this handle, so it is part of what a healthy session holds.
	sess.notifications.raiseRegistered()
	committed = append(committed, handle)

	// Save config for reconnect re-subscribe
	sess.notifications.addConfig(NotificationConfig{
		SymbolName:       symbolName,
		MaxDelay:         maxDelay,
		CycleTime:        cycleTime,
		TransmissionMode: transMode,
	})
	sess.notifications.notificationChannel = updateReceiver

	return handle, nil
}

// AddSymbolNotifications subscribes several symbols in one round-trip, returning
// results parallel to configs; a non-nil error means the batch never went at all.
// Partial outcomes are normal. Skipped != nil means the library did not commit it
// (match the ErrNotification* sentinels), otherwise Error is the PLC's verdict and
// NoErrors means Handle is valid. Do not close ch while notifications are active.
func (sess *Session) AddSymbolNotifications(ctx context.Context, configs []NotificationConfig, ch chan *Update) ([]ams.SumNotificationResult, error) {
	// Refuse outside RUN rather than produce a misleading failure: in CONFIG the
	// runtime port does not exist, so this cannot succeed, and the PLC's answer is
	// an AMS "port not found" rather than anything about symbols. Permits when no
	// state has been observed — see requireRunningRuntime.
	if err := sess.requireRunningRuntime("AddSymbolNotifications"); err != nil {
		return nil, err
	}
	if len(configs) == 0 {
		return nil, nil
	}

	results := make([]ams.SumNotificationResult, len(configs))

	// Snapshot already-subscribed symbol names so we can reject duplicates
	// pre-flight; the same check is repeated under the post-PLC lock to close
	// the TOCTOU window where a concurrent caller subscribed mid-roundtrip.
	sess.notifications.lock.Lock()
	if sess.notifications.notificationChannel != nil && sess.notifications.notificationChannel != ch {
		sess.notifications.lock.Unlock()
		return nil, fmt.Errorf("all symbol notifications on a connection must use the same updateReceiver channel")
	}
	// Snapshot the LIVE subscriptions so the dup-check inside the batch loop runs
	// lock-free against a per-call copy (cheaper than re-acquiring the manager lock
	// for each candidate). Live, not the configsByKey mirror: an entry on file with
	// no handle is a retry the caller may legitimately re-declare — see
	// hasLiveNotification.
	existing := sess.notifications.liveNotificationNames()
	sess.notifications.lock.Unlock()

	// Resolve symbols and build requests; track which result index maps to
	// which request slot so we can splice the sum response back.
	type symbolInfo struct {
		configIndex int // index into configs/results
		config      NotificationConfig
		symbol      *symtab.Symbol
	}
	var infos []symbolInfo
	var requests []ams.SumNotificationRequest
	batchSeen := make(map[string]struct{}, len(configs))

	for i, cfg := range configs {
		key := symtab.Key(cfg.SymbolName)
		if _, dup := existing[key]; dup {
			results[i].Skipped = fmt.Errorf("symbol %q: %w", cfg.SymbolName, ErrNotificationDuplicate)
			sess.logger.Warn("duplicate notification rejected (already subscribed)", "symbol", cfg.SymbolName)
			continue
		}
		if _, dup := batchSeen[key]; dup {
			results[i].Skipped = fmt.Errorf("symbol %q duplicated within batch: %w", cfg.SymbolName, ErrNotificationDuplicate)
			sess.logger.Warn("duplicate notification rejected (within batch)", "symbol", cfg.SymbolName)
			continue
		}
		batchSeen[key] = struct{}{}

		symbol, err := sess.getSymbol(ctx, cfg.SymbolName)
		if err != nil {
			results[i].Skipped = fmt.Errorf("resolve symbol %q: %w", cfg.SymbolName, err)
			sess.logger.Error("error getting symbol for batch notification", "error", err, "symbol", cfg.SymbolName)
			continue
		}
		infos = append(infos, symbolInfo{configIndex: i, config: cfg, symbol: symbol})

		actualMode := cfg.TransmissionMode
		if (actualMode == ams.TransModeServerCycle2 || actualMode == ams.TransModeServerOnChange2) && symbol.ContextMask == 0 {
			actualMode = adsconn.DowngradeTransMode(actualMode)
			sess.logger.Warn("InContext mode not available for symbol (ContextMask=0), falling back",
				"symbol", cfg.SymbolName,
				"requested", cfg.TransmissionMode.String(),
				"using", actualMode.String(),
				"flags", fmt.Sprintf("0x%04X", uint32(symbol.Flags)))
		}

		requests = append(requests, ams.SumNotificationRequest{
			Group:            uint32(ams.GroupSymbolValueByHandle),
			Offset:           symbol.Handle,
			Length:           symbol.Length,
			TransmissionMode: actualMode,
			MaxDelay:         cfg.MaxDelay,
			CycleTime:        cfg.CycleTime,
		})
	}

	if len(requests) == 0 {
		return results, nil
	}

	// Open the subscribe window BEFORE the batch RPC. This matters far more
	// here than in the single-symbol path: on a PLC without sum-command
	// support (TC2 answers 0x0701) SumAddDeviceNotification degrades to one
	// Add per symbol, so the earliest handles stream for the whole duration
	// of the remaining registrations. See AddSymbolNotification for the
	// defer-ordering rationale.
	subTok := sess.beginSubscribe()
	// Batch-scoped epoch. Compared per item, this refuses any commit once the
	// symbol cache has been reloaded since the batch began — which is what the
	// removed bulk re-check used to enforce. A per-item snapshot cannot: it only
	// sees a reload landing inside that one item's own commit.
	batchCacheEpoch := sess.epoch()
	committed := make([]uint32, 0, len(requests))
	committedIdx := make([]int, 0, len(requests))
	// Handles the PLC created but the library declined to bind. Released before
	// returning so a refusal cannot leak a subscription streaming to nobody.
	refused := make([]uint32, 0, len(requests))
	defer func() { sess.endSubscribe(ctx, subTok, committed) }()

	// Bind each handle the moment its own Add returns. On a PLC that rejects
	// the sum command this runs between the individual Adds, so symbol #1 is
	// recognisable while symbol #40 is still being registered — instead of the
	// whole batch becoming recognisable at the end. Called synchronously on
	// this goroutine, so results/committed need no extra guarding.
	onItem := func(i int, r ams.SumNotificationResult) {
		if i < 0 || i >= len(infos) {
			// Defensive: the index crosses a layer boundary. One bad index would
			// otherwise panic inside the RPC call.
			sess.logger.Error("notification batch callback got an out-of-range index",
				"index", i, "items", len(infos))
			return
		}
		info := infos[i]
		if r.Skipped != nil {
			// The Client layer already refused this one — e.g. the batch was
			// abandoned after the transport failed. Never treat it as bindable:
			// its Handle is zero and committing it would put a zero handle in
			// activeNotifications.
			results[info.configIndex] = r
			return
		}
		if r.Handle == 0 && r.Error == ams.ReturnCodeNoErrors {
			results[info.configIndex].Skipped = fmt.Errorf("symbol %q: PLC reported success without a handle", info.config.SymbolName)
			sess.logger.Error("notification batch: success with a zero handle",
				"symbol", info.config.SymbolName)
			return
		}
		if r.Error != ams.ReturnCodeNoErrors {
			results[info.configIndex] = r
			// Level by whether anyone has to act: a stale-detection code is the
			// expected answer after a runtime restart and heals itself, and logging
			// those at Error turned one restart into 22 ERROR lines in a second. The
			// code reaches the caller in results either way, so demoting costs nothing.
			level := slog.LevelError
			if stale, _ := detectStaleCache(r.Error); stale {
				level = slog.LevelWarn
			}
			sess.logger.Log(context.Background(), level, "error adding notification in batch",
				"symbol", info.config.SymbolName,
				"errorCode", uint32(r.Error))
			return
		}
		if skipErr := sess.commitNotification(info.config, r.Handle, ch, batchCacheEpoch); skipErr != nil {
			results[info.configIndex].Skipped = skipErr
			results[info.configIndex].Handle = r.Handle
			// The PLC created this registration before we refused it, so it is
			// streaming to nobody. Release it after the batch rather than here:
			// on a sum-unsupported PLC this callback runs between individual
			// Adds, and a Delete wedged in there would serialize into the
			// registration sequence.
			refused = append(refused, r.Handle)
			sess.logger.Warn("batch entry not committed; releasing the PLC handle",
				"symbol", info.config.SymbolName, "handle", r.Handle, "reason", skipErr)
			return
		}
		results[info.configIndex] = r
		committed = append(committed, r.Handle)
		committedIdx = append(committedIdx, info.configIndex)
		// Deliver anything the PLC already sent for this handle before the bind
		// landed. Must be outside commitNotification — replay takes cache.lock.
		sess.replayEarlySamples(ctx, []uint32{r.Handle})
		// Per handle in the batch: subscribing 40 symbols produced 40 of these.
		sess.logger.Debug("batch notification created",
			"handle", r.Handle,
			"symbol", info.config.SymbolName)
		// See the note at the single-subscribe site.
		sess.warnUnresolvedBaseType(info.config.SymbolName)
	}

	// settle is the batch tail: amend the sweep, then release every handle nothing
	// here ended up owning. Deferred because the bug it fixes was a return path that
	// skipped it -- the transport abort, which is exactly where handles are created
	// and not bound. Registered AFTER the endSubscribe defer so LIFO runs it first:
	// the amendment must decide what is stranded before the replay looks.
	settle := func() {
		// A sweep wipes activeNotifications and deletes those handles PLC-side, so
		// anything this batch committed beforehand now exists on neither side and
		// must not be reported as success. Asked of the map, not a generation
		// counter: "was MINE swept" is per entry, and a counter would also strand an
		// item that commits after the sweep and is owned by nobody else.
		if len(committedIdx) > 0 {
			var stranded []int
			sess.notifications.lock.Lock()
			for _, idx := range committedIdx {
				if _, bound := sess.notifications.activeNotifications[results[idx].Handle]; !bound {
					stranded = append(stranded, idx)
					// commitNotification registered this symbol; un-committing it has
					// to unregister it too. The cleanup delete below cannot: it only
					// drops a config whose handle is still in activeNotifications, and
					// the sweep emptied that map. Left in place, the retry this entry's
					// error documents is rejected as a duplicate forever.
					sess.removeNotificationConfig(configs[idx].SymbolName)
				}
			}
			sess.notifications.lock.Unlock()

			for _, idx := range stranded {
				results[idx] = ams.SumNotificationResult{
					Handle:  results[idx].Handle,
					Skipped: fmt.Errorf("symbol %q: %w", configs[idx].SymbolName, ErrNotificationStrandedByReload),
				}
			}
			if len(stranded) > 0 {
				sess.logger.Warn("notifications swept mid-batch; committed entries reported as stranded",
					"entries", len(stranded), "committed", len(committedIdx))
			}
		}

		// Release what we refused, plus anything the amendment just invalidated:
		// both are registrations on the PLC that nothing on this side owns.
		for _, idx := range committedIdx {
			if results[idx].Skipped != nil && results[idx].Handle != 0 {
				refused = append(refused, results[idx].Handle)
			}
		}
		if len(refused) > 0 {
			// A batch that aborted on a cancelled or expired ctx must still release
			// what the PLC created: reusing the dead ctx would fail the delete
			// before it was sent and leak a subscription streaming to nobody. The
			// session lifetime bounds the replacement, so a closing session does
			// not block here.
			releaseCtx, cancel := sess.releaseCleanupCtx(ctx)
			defer cancel()
			// Best-effort: 0x714 (already gone) counts as success, which covers the
			// stranded-by-reload case where the reload deleted them first.
			deleted := sess.bestEffortDeleteNotifications(releaseCtx, refused)
			sess.logger.Warn("released PLC notification handles the batch did not bind",
				"handles", len(refused), "deleted", deleted)
		}
	}
	defer settle()

	subResults, err := sess.client.Load().SumAddDeviceNotificationFunc(ctx, requests, onItem)
	if err != nil {
		// Transport-aborted batch: every entry that was about to be sent must
		// be marked Skipped so callers can distinguish "lib didn't try" from
		// "PLC rejected". Anything onItem already reported keeps its own, more
		// specific outcome — tested via Skipped rather than the Error/Handle
		// pair, which also matches entries onItem refused (those carry a zero
		// Error and a live Handle).
		for _, info := range infos {
			r := results[info.configIndex]
			reported := r.Skipped != nil || r.Handle != 0 || r.Error != ams.ReturnCodeNoErrors
			if reported {
				continue
			}
			results[info.configIndex].Skipped = fmt.Errorf("%w: %w", ErrNotificationTransportFailure, err)
		}
		return results, fmt.Errorf("batch add notification failed: %w", err)
	}

	// R-CACHE-009: fire online-change detection for first stale per-item code.
	// Once-per-batch semantics avoid callback amplification when multiple
	// items in the same response carry the same stale code (R-SES-011
	// "once per detection").
	for _, r := range subResults {
		if r.Error == ams.ReturnCodeNoErrors {
			continue
		}
		if stale, _ := detectStaleCache(r.Error); stale {
			sess.handleStaleDetection(r.Error)
			break
		}
	}

	// Everything else was already bound (or recorded as failed/skipped) by
	// onItem as its result arrived; nothing left to commit here.
	if len(committed) > 0 {
		_ = sess.establishHeartbeat(ctx)
	}
	return results, nil
}

// commitNotification binds a PLC handle to its symbol so dispatchSample
// recognises it, returning the reason for any refusal so the caller can surface
// it as Skipped and release the registration. Called once per handle as soon as
// it is known, so a batch served one Add at a time binds progressively. Caller
// must hold neither lock: this takes cache.lock and releases it before the other.
func (sess *Session) commitNotification(cfg NotificationConfig, handle uint32, ch chan *Update, batchCacheEpoch uint64) error {
	// Re-fetch the *symbol under cache.lock. The pointer resolved before the
	// PLC round-trip may have been stranded by a concurrent loadSymbols /
	// online-change reload that swapped cache.symbols.
	sess.cache.lock.Lock()
	fresh := sess.cache.symbols[symtab.Key(cfg.SymbolName)]
	sess.cache.lock.Unlock()

	sess.notifications.lock.Lock()
	defer sess.notifications.lock.Unlock()

	// Compare against the epoch the BATCH started with, not one snapshotted
	// moments ago: a reload that landed earlier in this batch must invalidate
	// every remaining item, not just the one unlucky enough to straddle it.
	// Unchanged epoch also means the `fresh` pointer read above is current.
	if sess.epoch() != batchCacheEpoch {
		return fmt.Errorf("symbol %q: %w", cfg.SymbolName, ErrNotificationStrandedByReload)
	}
	if fresh == nil {
		return fmt.Errorf("symbol %q: %w", cfg.SymbolName, ErrNotificationSymbolVanished)
	}
	if sess.notifications.notificationChannel != nil && sess.notifications.notificationChannel != ch {
		return fmt.Errorf("symbol %q: %w", cfg.SymbolName, ErrNotificationChannelMismatch)
	}
	// activeNotifications already contains anything committed earlier in this same
	// batch (the insert below precedes addConfig), so it doubles as the in-batch
	// duplicate guard — no separate pre/post snapshot needed.
	if sess.notifications.hasLiveNotification(cfg.SymbolName) {
		return fmt.Errorf("symbol %q subscribed concurrently during batch: %w", cfg.SymbolName, ErrNotificationDuplicate)
	}

	sess.notifications.activeNotifications[handle] = activeNotification{Sym: fresh, Ch: ch}
	// Same as the single-subscribe site: this is what a healthy session holds.
	// Missing it here made the gap check inert for every batch subscriber, which
	// is how the plugin subscribes -- caught on hardware, want=0 have=1.
	sess.notifications.raiseRegistered()
	// addConfig wraps in a fresh pendingNotification with resubscribeAttempts=0,
	// so a successful subscribe naturally resets any prior retry counter.
	sess.notifications.addConfig(cfg)
	sess.notifications.notificationChannel = ch
	return nil
}

// removeNotificationConfig removes the first config matching symbolName.
// Must be called with notifications.lock held.
func (sess *Session) removeNotificationConfig(symbolName string) {
	key := symtab.Key(symbolName)
	if _, ok := sess.notifications.configsByKey[key]; !ok {
		return
	}
	delete(sess.notifications.configsByKey, key)
	for i, entry := range sess.notifications.pending {
		if strings.EqualFold(entry.Config.SymbolName, symbolName) {
			sess.notifications.pending = append(sess.notifications.pending[:i], sess.notifications.pending[i+1:]...)
			return
		}
	}
}

// Subscribe race window: the PLC-side notification handle exists from the
// moment AddDeviceNotification returns, but our activeNotifications insert
// happens afterwards — and on TC2, which answers 0x0701 to the sum command,
// AddSymbolNotifications degrades to one Add per symbol, so the last symbol
// of a 40-entry batch commits hundreds of milliseconds after the first
// symbol started streaming. Samples arriving in that window must be neither
// dropped nor mistaken for leaked handles.
const (
	// subscribeRaceWindow extends the guard past the last commit, covering
	// the gap between the insert and the in-flight counter reaching zero.
	subscribeRaceWindow = 100 * time.Millisecond
	// earlySampleMaxHandles bounds the buffer. Reaching it means far more
	// unknown handles are in flight than any real subscribe produces.
	earlySampleMaxHandles = 4096
	// subscribeRaceMaxOpen caps how long an in-flight subscribe may keep the
	// reaper suppressed, so a wedged one cannot disable it indefinitely.
	subscribeRaceMaxOpen = 30 * time.Second
	// earlySampleMaxBytes bounds the buffer in memory as well as in entries.
	// Generous for its purpose — one sample per symbol being subscribed — while
	// keeping a flood of large unknown-handle samples from growing without limit.
	earlySampleMaxBytes = 8 << 20
)

// subscribeRaceActive reports whether an unknown handle is presumed one of ours
// mid-registration rather than leaked. The in-flight counter is authoritative but
// not unconditional -- a wedged subscribe would otherwise disable the orphan reaper
// for the session's life -- so the window also expires, generously enough for
// hundreds of symbols registered one at a time.
func (sess *Session) subscribeRaceActive() bool {
	mgr := sess.notifications
	now := time.Now().UnixNano()
	// Judged on the MOST RECENTLY opened subscribe still in flight: its handles are
	// what the PLC may be streaming now, so the window stays open even if an older
	// sibling wedged -- and a wedged one alone cannot hold it open, since nothing
	// younger vouches for it. A single timestamp answers neither question; only
	// per-subscribe starts do.
	if newest, open := mgr.newestOpenSubscribe(); open {
		return now-newest < subscribeRaceMaxOpen.Nanoseconds()
	}
	// Nothing in flight: the short tail covers the gap between the last commit
	// and the PLC's first sample for it.
	return now-mgr.lastSubscribeNs.Load() < subscribeRaceWindow.Nanoseconds()
}

// beginSubscribe marks a subscribe in flight and returns the token that closes it;
// every call pairs with endSubscribe, which is why callers defer it at once. The
// token tracks each subscribe individually: one shared counter and timestamp were
// not atomic across goroutines, and the resulting "in flight but no clock" state
// suppressed the orphan reaper permanently.
func (sess *Session) beginSubscribe() subscribeToken {
	mgr := sess.notifications
	now := time.Now().UnixNano()

	mgr.openMu.Lock()
	mgr.nextSubscribeToken++
	tok := subscribeToken(mgr.nextSubscribeToken)
	if mgr.openSubscribes == nil {
		mgr.openSubscribes = make(map[subscribeToken]int64)
	}
	mgr.openSubscribes[tok] = now
	mgr.subscribeInFlight.Store(int64(len(mgr.openSubscribes)))
	mgr.openMu.Unlock()

	mgr.lastSubscribeNs.Store(now)
	return tok
}

// endSubscribe replays samples buffered for committed handles, then discards what
// remains once nothing is in flight -- a leftover belongs to a handle that never
// committed, and if it really is leaked PLC-side its next sample takes the orphan
// path. MUST run after notifications.lock is released, since the replay takes
// cache.lock; deferring it before the unlock defer gives that ordering for free.
func (sess *Session) endSubscribe(ctx context.Context, tok subscribeToken, committed []uint32) {
	mgr := sess.notifications
	mgr.lastSubscribeNs.Store(time.Now().UnixNano())
	sess.replayEarlySamples(ctx, committed)

	// Closing this subscribe and deciding whether anything may be discarded is one
	// critical section. Split apart, a subscribe beginning between them would see
	// the window still open, buffer a sample, and have this call throw it away.
	// Both locks are leaves and are always taken in this order, openMu first.
	mgr.openMu.Lock()
	delete(mgr.openSubscribes, tok)
	remaining := len(mgr.openSubscribes)
	mgr.subscribeInFlight.Store(int64(remaining))

	mgr.earlyMu.Lock()
	dropped := 0
	if remaining == 0 {
		dropped = len(mgr.earlySamples)
		if dropped > 0 {
			mgr.earlySamples = nil
			mgr.earlyBytes = 0
		}
	}
	mgr.earlyMu.Unlock()
	mgr.openMu.Unlock()

	if dropped > 0 {
		sess.logger.Debug("discarded buffered samples for handles that never committed", "count", dropped)
	}
}

// bufferEarlySample parks the most recent sample for an uncommitted handle.
func (sess *Session) bufferEarlySample(ctx context.Context, handle uint32, timestamp uint64, content []byte) {
	mgr := sess.notifications
	// Copy: content is freshly allocated per sample today, but the handler
	// signature is exported and this buffer outlives the dispatch call.
	buf := make([]byte, len(content))
	copy(buf, content)

	mgr.earlyMu.Lock()
	if mgr.earlySamples == nil {
		mgr.earlySamples = make(map[uint32]earlySample)
	}
	prev, known := mgr.earlySamples[handle]
	// Replacing an entry frees its bytes, so only the delta counts.
	delta := len(buf)
	if known {
		delta -= len(prev.content)
	}
	full := (!known && len(mgr.earlySamples) >= earlySampleMaxHandles) ||
		mgr.earlyBytes+delta > earlySampleMaxBytes
	if !full {
		mgr.earlySamples[handle] = earlySample{timestamp: timestamp, content: buf}
		mgr.earlyBytes += delta
	}
	held := mgr.earlyBytes
	mgr.earlyMu.Unlock()

	if full {
		sess.logger.Warn("early notification sample dropped (buffer full)",
			"handle", handle, "max_handles", earlySampleMaxHandles,
			"max_bytes", earlySampleMaxBytes, "held_bytes", held)
		return
	}
	sess.logger.Debug("buffered early notification sample (handle registration still in flight)", "handle", handle)

	// The commit can land between the map miss that sent us here and the insert
	// above. If it did, the replay that would have collected this sample has
	// already run, and no later one will look for a handle that is already
	// bound — so the sample would sit parked until some unrelated subscribe
	// discarded it. For a static symbol that is the only sample it will ever
	// send. Whoever loses that race cleans up after itself: replayEarlySamples
	// takes the sample only if the handle is bound by now, so this is a no-op
	// in the common case.
	sess.replayEarlySamples(ctx, []uint32{handle})
}

// replayEarlySamples dispatches buffered samples for handles that are bound in
// activeNotifications, removing them from the buffer as it goes.
//
// A handle that is NOT bound is left parked on purpose: its commit may still be
// coming, and dispatching it would take the unknown-handle path and schedule an
// orphan delete for a handle we may be about to own.
func (sess *Session) replayEarlySamples(ctx context.Context, handles []uint32) {
	if len(handles) == 0 {
		return
	}
	mgr := sess.notifications
	type replay struct {
		handle uint32
		sample earlySample
	}
	var pending []replay

	// notifications.lock first and released before earlyMu — the two are never
	// held together, in either order, anywhere.
	bound := make(map[uint32]struct{}, len(handles))
	mgr.lock.Lock()
	for _, h := range handles {
		if _, ok := mgr.activeNotifications[h]; ok {
			bound[h] = struct{}{}
		}
	}
	mgr.lock.Unlock()

	mgr.earlyMu.Lock()
	for _, h := range handles {
		if _, ok := bound[h]; !ok {
			continue
		}
		if s, ok := mgr.earlySamples[h]; ok {
			delete(mgr.earlySamples, h)
			mgr.earlyBytes -= len(s.content)
			pending = append(pending, replay{handle: h, sample: s})
		}
	}
	mgr.earlyMu.Unlock()

	for _, p := range pending {
		sess.logger.Debug("replaying buffered early notification sample", "handle", p.handle)
		sess.dispatchSample(ctx, p.handle, p.sample.timestamp, p.sample.content, false)
	}
}
