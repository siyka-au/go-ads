package ads

import (
	"context"
	"time"

	"github.com/siyka-au/go-ads/v3/internal/symtab"

	"github.com/siyka-au/go-ads/v3/ams"
)

// DeviceNotification (ADS cmd 8) packet decoder lives on *Client — see
// Client.deviceNotification. Session.handleNotification below is the
// cache-aware handler installed via Client.SetNotificationHandler from
// Session.Connect.

func (sess *Session) handleNotification(ctx context.Context, handle uint32, timestamp uint64, content []byte) {
	sess.dispatchSample(ctx, handle, timestamp, content, true)
}

// dispatchSample is the body of handleNotification. buffer controls whether an
// unknown handle may be parked in earlySamples: true on the live path, false
// when replayEarlySamples is re-dispatching a buffered sample, so a handle
// whose subscribe never committed cannot bounce between buffer and replay.
func (sess *Session) dispatchSample(ctx context.Context, handle uint32, timestamp uint64, content []byte, buffer bool) {
	// The heartbeat is ours, not the caller's: consume it before the
	// unknown-handle path can mistake it for a leaked subscription.
	if sess.consumeHeartbeat(handle, content) {
		return
	}
	// notifications.lock: handle lookup + symbol pointer/channel snapshot.
	sess.notifications.lock.Lock()
	entry, ok := sess.notifications.activeNotifications[handle]
	if !ok {
		sess.notifications.lock.Unlock()
		// Stale notifications are expected during:
		// - Close(): handles deleted from activeNotifications while listen() still drains
		// - Reconnect: Session.Reconnect clears activeNotifications before new subscriptions
		// - first-sample race — the PLC fires the first notification before our
		//   activeNotifications insert completes. The PLC-side handle exists the
		//   moment Add returns, so this window is unavoidable; it is wide enough
		//   to matter whenever a subscribe is still in flight.
		switch {
		case sess.isClosed() || sess.isReconnecting():
			sess.logger.Debug("received notification for deleted handle (expected during close/reconnect)", "handle", handle)
		case buffer && sess.subscribeRaceActive():
			// Our own subscribe is mid-flight, so this handle is almost
			// certainly one we are about to commit. Park the sample instead of
			// dropping it: a static symbol (constant string/bool) emits exactly
			// one sample, at subscribe time, and dropping it means the consumer
			// never sees that tag at all.
			sess.bufferEarlySample(ctx, handle, timestamp, content)
		default:
			// Genuine orphan: registered on the PLC, not in our map -- usually a
			// prior process with the same source NetID+port. Delete it, or the
			// router accumulates entries until it crashes (Beckhoff #268). One Warn
			// per interval with a count, the rest Debug: the PLC pushes one per
			// cycle per handle, interleaved with healthy samples after a reconnect.
			n := sess.notifications.orphanSamples.Add(1)
			if reportNow(&sess.notifications.orphanWarnNs, orphanReportInterval) {
				sess.logger.Warn("received notification for unknown handle", "handle", handle,
					"sinceLastReport", n,
					"detail", "a previous session's subscriptions are still registered on the PLC; deleting them")
				sess.notifications.orphanSamples.Store(0)
			} else {
				sess.logger.Debug("received notification for unknown handle", "handle", handle, "sinceLastReport", n)
			}
			sess.tryOrphanDelete(handle)
		}
		return
	}
	notification := entry.Ch
	fullName := entry.Sym.FullName
	sess.notifications.lock.Unlock()

	var notificationTime time.Time
	if timestamp == 0 {
		notificationTime = time.Now()
	} else {
		timeStamp := int64(timestamp)/windowsTick - secToUnixEpoch
		notificationTime = time.Unix(timeStamp, int64(timestamp)%windowsTick*100)
	}
	// cache.lock for parse() — symbol fields live in cache.symbols and parse
	// mutates Value/Valid. Lock ordering: cache after notifications release
	// (never both held).
	// Re-resolve via cache.symbols[FullName]: the symbol fetched from
	// activeNotifications may be stranded post-reload (loadSymbols swapped
	// the cache between subscribe and now), in which case parse with the
	// FRESH cache.datatypes against the OLD symbol's DataType key may
	// mismatch. If the symbol is gone from the live cache, log + skip.
	sess.cache.lock.Lock()
	live := sess.cache.symbols[symtab.Key(fullName)]
	if live == nil {
		sess.cache.lock.Unlock()
		sess.logger.Warn("notification target symbol no longer in cache; skipping parse",
			"handle", handle, "symbol", fullName)
		return
	}
	// R-CACHE-009 supplementary detection: 0-byte terminal sample =
	// symbol gone post-online-change. TwinCAT drops the old handle
	// silently after the symbol is deleted and emits one final 0-byte
	// sample on the now-dead handle. Intercept BEFORE the parse path so
	// the configured strategy fires (Ignore: log + callback; Close:
	// terminate; AutoReload: re-discover) and no spurious Update is
	// delivered for the dead handle.
	if len(content) == 0 && live.Length > 0 {
		dataType := live.DataType
		length := live.Length
		sess.cache.lock.Unlock()
		sess.logger.Debug("notification terminal 0-byte sample (symbol removed post-online-change)",
			"handle", handle, "symbol", fullName, "dataType", dataType, "expectedLength", length)
		sess.handleStaleDetection(ams.ReturnCodeDeviceSymbolNoFound)
		return
	}
	value, err := live.Decode(content, 0, sess.cache.datatypes)
	if err != nil {
		sess.cache.lock.Unlock()
		sess.logger.Error("error during parse of notification",
			"handle", handle, "symbol", fullName, "dataType", live.DataType, "error", err)
		return
	}
	value = symtab.CopyValue(value)
	sess.cache.lock.Unlock()

	sess.logger.Log(context.Background(), LevelTrace, "update received", "update", value)
	updateStruct := &Update{
		Variable:  fullName,
		Value:     value,
		TimeStamp: notificationTime,
	}
	// One-shot Stale flag (R-NOT-017): consume on first delivered sample.
	if reason, ok := sess.consumeStaleFlag(handle); ok {
		updateStruct.Stale = &StaleInfo{Reason: reason}
	}
	sess.deliverNotification(ctx, notification, updateStruct, handle, fullName)
}

// deliverNotification performs a non-blocking send on the caller-owned channel.
// Guards against the caller closing the channel: a select with default does NOT
// prevent panics on send-to-closed-channel — Go runtime always panics in that case.
// Recovers and logs an Error instead of crashing the listen goroutine.
//
// Caller must NOT close the update channel while subscriptions exist on this
// connection; see AddSymbolNotification(s) godoc for the ownership rule.
func (sess *Session) deliverNotification(ctx context.Context, ch chan<- *Update, update *Update, handle uint32, fullName string) {
	defer func() {
		if r := recover(); r != nil {
			sess.logger.Error("notification send panicked — caller closed the update channel?",
				"handle", handle,
				"symbol", fullName,
				"panic", r)
		}
	}()
	// Non-blocking send: deliver notification instantly or drop if channel full.
	// Caller controls backpressure by sizing the channel buffer appropriately
	// (e.g. make(chan *Update, 1024) for burst absorption).
	// This prevents goroutine accumulation and never blocks the receive pipeline.
	select {
	case <-ctx.Done():
	case ch <- update:
		sess.logger.Debug("Successfully delivered notification", "handle", handle)
	default:
		sess.logger.Warn("notification dropped (channel full, receiver too slow)",
			"handle", handle,
			"symbol", fullName)
	}
}
