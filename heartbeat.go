package ads

import (
	"context"
	"errors"
	"time"
)

// Heartbeat: proving the caller's subscriptions are alive without asking the PLC.
// A runtime restart kills subscriptions while leaving the connection, symbol
// version and ADS state unchanged, so one cyclic subscription of our own turns
// silence into proof. See notificationManager.heartbeatHandle.
//
// HeartbeatRecovery selects what happens when it goes silent. Recovery is not
// free -- a delete and an add per handle, 82 requests on a 41-symbol session --
// but Immediate stays the default as the behaviour that has been in the field.
type HeartbeatRecovery int

const (
	// HeartbeatRecoveryImmediate re-subscribes as soon as the silence window is
	// exceeded. The default.
	HeartbeatRecoveryImmediate HeartbeatRecovery = iota + 1
	// HeartbeatRecoveryConfirm requires two consecutive silent windows before
	// re-subscribing, trading time-to-notice for not firing the burst at a device
	// that was merely late.
	HeartbeatRecoveryConfirm
	// HeartbeatRecoveryObserve never re-subscribes. The silence is reported (log
	// plus the WithOnSymbolVersionChanged callback) and left to the consumer, for
	// a caller that would rather rebuild the session itself than have handles
	// churned underneath it.
	HeartbeatRecoveryObserve
)

func (h HeartbeatRecovery) String() string {
	switch h {
	case HeartbeatRecoveryImmediate:
		return "immediate"
	case HeartbeatRecoveryConfirm:
		return "confirm"
	case HeartbeatRecoveryObserve:
		return "observe"
	default:
		return "unset"
	}
}

const (
	defaultHeartbeatInterval = 2 * time.Second
	defaultHeartbeatMissed   = 5

	// heartbeatFailuresBeforeReconnect is how many silent windows may pass, with
	// no frame arriving on either socket, before the transport itself is treated
	// as the fault. Re-subscribing cannot fix a link that is gone, and repeated
	// attempts against one keep the socket busy enough that TCP's keepalive timer
	// never runs -- measured, the session then survives the whole outage and only
	// reconnects when the peer's RST arrives. Two is roughly 20-30s, against 107s
	// observed without this.
	heartbeatFailuresBeforeReconnect = 2
	// maxADSCycleTime is the longest cycle an ADS notification can carry: the
	// wire field is 32-bit 100ns ticks.
	maxADSCycleTime = 400 * time.Second

	// maxHeartbeatRecoveryBackoff caps the wait between recovery attempts once they
	// start failing. A PLC left in CONFIG is the normal reason, and it can sit there
	// for hours; the session still has to come back on its own within a sensible
	// time of the runtime serving again.
	maxHeartbeatRecoveryBackoff = 30 * time.Second

	// maxFailureBackoffShift bounds the doubling at base * 64, so the shift itself
	// can never run off into a nonsense window if failures keep counting up.
	maxFailureBackoffShift = 6
)

// heartbeatAllowedTicks reports the silent ticks tolerated before a recovery: the
// base window doubled per consecutive failure, capped in wall-clock terms. Pure
// and separate because both ways the arithmetic goes wrong are invisible -- the
// session just retries at the wrong rate.
//
// The cap is a duration in ticks, floored at base: at a long cycle it is fewer
// ticks than base and would run the backoff backwards, and once the cycle exceeds
// the budget the division truncates to zero and the window grows unbounded.
func heartbeatAllowedTicks(base, consecutiveFailures int, cycle time.Duration) int {
	if consecutiveFailures <= 0 || base <= 0 || cycle <= 0 {
		return base
	}
	shift := min(consecutiveFailures, maxFailureBackoffShift)
	capTicks := max(int(maxHeartbeatRecoveryBackoff/cycle), base)
	return min(base<<shift, capTicks)
}

// normalizeHeartbeatOptions resolves the interdependent options once, after the
// option loop and before any goroutine exists. WithNotificationSilenceTimeout is
// stated in time but enforced in ticks, so it needs a cycle a later option may
// change -- and resolving it lazily would be a data race against the watcher.
// Last-wins between the two ways of saying it.
func (sess *Session) normalizeHeartbeatOptions() {
	if sess.heartbeatSilence <= 0 {
		return
	}
	cycle := sess.heartbeatCycle()
	// Round up: a silence timeout is "do not conclude anything before this much
	// quiet", so truncating would conclude early.
	missed := int((sess.heartbeatSilence + cycle - 1) / cycle)
	if missed < 2 {
		// Same floor as WithNotificationHeartbeat, for the same reason: a single
		// late beat is not evidence of anything.
		missed = 2
	}
	sess.heartbeatMissed = missed
}

// heartbeatRecoveryMode reports the configured recovery mode, defaulting to
// Immediate for a session built without the option (including test fixtures that
// construct a Session literal).
func (sess *Session) heartbeatRecoveryMode() HeartbeatRecovery {
	if sess.heartbeatRecovery == 0 {
		return HeartbeatRecoveryImmediate
	}
	return sess.heartbeatRecovery
}

// heartbeatEnabled reports whether this session keeps a heartbeat.
func (sess *Session) heartbeatEnabled() bool {
	return !sess.heartbeatDisabled
}

func (sess *Session) heartbeatCycle() time.Duration {
	if sess.heartbeatInterval > 0 {
		return sess.heartbeatInterval
	}
	return defaultHeartbeatInterval
}

func (sess *Session) heartbeatAllowedMisses() int {
	if sess.heartbeatMissed >= 2 {
		return sess.heartbeatMissed
	}
	return defaultHeartbeatMissed
}

// establishHeartbeat registers the internal cyclic notification, if enabled and not
// already present. Failing is not fatal: the session works, it just loses the
// ability to notice its subscriptions dying quietly. The error is returned so a
// caller can tell a PLC that refused from one that did not answer; most callers
// have nothing to do with it.
func (sess *Session) establishHeartbeat(ctx context.Context) error {
	// Same window as recoverDeadSubscriptions: registering a beat on a session that
	// has already released its PLC resources strands it.
	if sess.isClosed() || !sess.heartbeatEnabled() || sess.notifications.heartbeatHandle.Load() != 0 {
		return nil
	}
	c := sess.client.Load()
	if c == nil {
		return nil
	}
	// Cyclic, one byte, on the symbol-version group: runtime-served (so it dies
	// with the runtime's notification table, which is the event being detected),
	// present regardless of the caller's program, and its payload is the version.
	handle, err := c.AddDeviceNotification(ctx, uint32(GroupSymbolVersion), 0, 1,
		TransModeServerCycle, 0, sess.heartbeatCycle())
	if err != nil {
		// First failure is worth a Warn; the rest are Debug, because the watcher
		// retries on a cadence and a device that refuses cyclic notifications
		// altogether would otherwise produce one Warn per interval forever.
		msg := "could not establish the notification heartbeat; retrying, and until it succeeds a silent subscription death will go unnoticed"
		if sess.notifications.heartbeatEstablishFailures.Add(1) == 1 {
			sess.logger.Warn(msg, "error", err)
		} else {
			sess.logger.Debug(msg, "error", err)
		}
		// Start the watcher anyway. It used to run only after a SUCCESSFUL establish,
		// so a session whose first attempt failed had no watchdog for its entire life
		// — no retry, and a later silent death went unnoticed with one Warn as the
		// only trace. The watcher re-attempts the beat itself (see heartbeatWatch).
		sess.startHeartbeatWatch()
		return err
	}
	sess.notifications.heartbeatEstablishFailures.Store(0)
	// A handle that is already one of the caller's would make every sample for
	// that subscription look like a beat and be swallowed. No real PLC issues
	// duplicates, but the consequence is bad enough to check for.
	sess.notifications.lock.Lock()
	_, collides := sess.notifications.activeNotifications[handle]
	sess.notifications.lock.Unlock()
	if collides {
		sess.logger.Warn("PLC returned a heartbeat handle that is already in use by a subscription; not using it",
			"handle", handle)
		if derr := c.DeleteDeviceNotification(ctx, handle); derr != nil {
			sess.logger.Debug("releasing the colliding heartbeat handle failed", "error", derr)
		}
		// Same reason as the establish-failure path above: returning without a
		// watcher leaves this session with no watchdog for its entire life, so
		// nothing retries the beat and a later silent subscription death goes
		// unnoticed. The collision itself is transient — the next attempt asks the
		// PLC for a fresh handle.
		sess.startHeartbeatWatch()
		return nil
	}
	// CompareAndSwap, not Store: two concurrent first subscribes both see no
	// heartbeat, both register one, and the second Store would orphan the first —
	// a cyclic registration the PLC keeps pushing that belongs to nothing. The
	// loser deletes what it just created.
	if !sess.notifications.heartbeatHandle.CompareAndSwap(0, handle) {
		sess.logger.Debug("another subscribe established the heartbeat first; releasing this one", "handle", handle)
		if derr := c.DeleteDeviceNotification(ctx, handle); derr != nil {
			sess.logger.Debug("releasing the redundant heartbeat handle failed", "handle", handle, "error", derr)
		}
		return nil
	}
	sess.notifications.heartbeatLastNs.Store(time.Now().UnixNano())
	sess.logger.Debug("notification heartbeat established", "handle", handle, "cycle", sess.heartbeatCycle())
	sess.startHeartbeatWatch()
	return nil
}

// consumeHeartbeat records a beat. Returns true when the sample was the heartbeat
// and must not reach the caller.
func (sess *Session) consumeHeartbeat(handle uint32, content []byte) bool {
	if handle == 0 || handle != sess.notifications.heartbeatHandle.Load() {
		return false
	}
	sess.notifications.heartbeatBeats.Add(1)
	sess.notifications.heartbeatLastNs.Store(time.Now().UnixNano())
	// The payload is the symbol version, so the beat carries online-change
	// detection for free — no extra request needed.
	if len(content) > 0 {
		// Record what we saw before acting on it, under the same lock that read the
		// old value. Without the write-back every following beat re-detects the same
		// change: under SymbolVersionIgnore that is a markAllHandlesStale and a fresh
		// versionCallback goroutine per interval, forever, against the documented
		// once-per-detection contract. AutoReload only escaped it because LoadSymbols
		// rewrites the field on its way through.
		sess.cache.lock.Lock()
		known := sess.cache.symbolVersion
		changed := known != 0 && content[0] != known
		if changed {
			sess.cache.symbolVersion = content[0]
		}
		sess.cache.lock.Unlock()
		if changed {
			sess.logger.Info("symbol version changed (seen on the heartbeat)", "old", known, "new", content[0])
			sess.handleStaleDetection(ReturnCodeDeviceSymbolVersionInvalid)
		}
	}
	return true
}

// startHeartbeatWatch runs the watcher once per session.
func (sess *Session) startHeartbeatWatch() {
	sess.heartbeatOnce.Do(func() {
		// Through the admission gate, not a bare Add: Close now waits this group, so
		// a raw Add is the same TOCTOU spawnMu exists to close. establishHeartbeat
		// runs on a USER goroutine and does a full PLC round-trip between its
		// isClosed() check and this call, which is plenty of room for Close to
		// complete its own Wait — and an Add landing after that is documented
		// WaitGroup misuse, i.e. a process-level panic.
		if !sess.trackGoroutineOn(&sess.heartbeatWG, sess.heartbeatWatch) {
			sess.logger.Debug("not starting the heartbeat watch: the session is closed")
		}
	})
}

// heartbeatWatch re-subscribes when the beats stop. Retrying matters as much as
// detecting: in CONFIG the re-subscribe cannot succeed, and the watcher keeps
// trying so the session recovers by itself once the runtime serves again.
//
// On a leash, though: uncapped it produced 1468 silence warnings in one run, 28%
// of the log. A closed transport was treated like a PLC in CONFIG, failures slowed
// nothing down, and each attempt re-queued every config.
func (sess *Session) heartbeatWatch() {
	// No Done here: trackGoroutineOn owns the Add and the Done.
	cycle := sess.heartbeatCycle()
	// lastBeats/quietTicks are the whole state of the detector: how many beats had
	// arrived at the previous tick, and how many ticks have passed with none.
	var lastBeats uint64
	quietTicks := 0
	// lastGen is the connected generation this detector state belongs to. A
	// reconnect deletes and re-adds every subscription and registers a fresh beat,
	// so a count carried across the gap is about handles that no longer exist.
	lastGen := sess.connectedGen()
	ticker := time.NewTicker(cycle)
	defer ticker.Stop()

	// consecutiveFailures backs the retry off and keeps the log to one line per
	// episode. Goroutine-local: this is the only writer.
	consecutiveFailures := 0
	// Frames seen when the last silent window opened, and how many windows have
	// passed without that number moving. A PLC that answers -- even to refuse --
	// moves it, which is the whole discriminator: anything arriving means the link
	// works and the subscriptions are worth retrying. Nothing arriving is the only
	// evidence about the transport, and it cannot be misread the way an error can.
	framesAtWindow := uint64(0)
	framelessWindows := 0
	// Ticks since the subscription gap was last acted on. Separate from quietTicks,
	// which a beat resets.
	gapTicks := 0
	// silentWindows counts consecutive silent windows, for
	// HeartbeatRecoveryConfirm. Reset whenever a beat arrives or a recovery runs,
	// so "2" always means two in a row rather than two ever.
	silentWindows := 0

	for {
		select {
		case <-sess.lifecycle.closedCh:
			return
		case <-ticker.C:
		}
		// A completed reconnect restarts the detector: otherwise a session that
		// dropped one tick short of the threshold fired recovery on its first
		// Connected tick, re-adding every handle the reconnect had just registered.
		// Strictly conservative -- it can only delay a recovery, and a frozen-but-
		// alive PLC does not reconnect, so its generation never moves.
		if gen := sess.connectedGen(); gen != lastGen {
			lastGen = gen
			// The beat counter is monotonic across reconnects, so re-read it rather
			// than leaving a pre-drop value that would read as a fresh beat.
			lastBeats = sess.notifications.heartbeatBeats.Load()
			quietTicks = 0
			// Also the backoff: the reconnect resubscribed everything, so how hard
			// recovery was on the previous transport says nothing about this one, and
			// an inflated window delays detection of a genuinely dead new set by up
			// to maxHeartbeatRecoveryBackoff. A PLC that flaps faster than the window
			// therefore regains the un-backed-off rate on each reconnect; that is
			// bounded by reconnectSleep, not by this counter.
			consecutiveFailures = 0
		}
		// Connected, not merely "not disconnected": dialAndStart clears the flag
		// before the route, reload and resubscribe steps, so a reconnect's tail
		// looks live and ticking through it churns handles it is still restoring.
		// No reset here -- every path back to Connected bumps the generation above,
		// and resetting per non-Connected tick is the masking bug from the other side.
		if sess.lifecycle.state.load() != SessionStateConnected {
			continue // a drop has its own recovery path; do not compete with it
		}
		// A transport that is gone cannot carry a re-subscribe, and getting it back
		// is the reconnect path's job, not ours. Without this the watcher spun at
		// the heartbeat interval for the life of the process on any session whose
		// client died while the FSM still said Connected.
		if c := sess.client.Load(); c == nil || (c.ctx != nil && c.ctx.Err() != nil) {
			continue
		}
		sess.notifications.lock.Lock()
		active := len(sess.notifications.activeNotifications)
		wanted := len(sess.notifications.pending)
		// Under the same lock as active, not re-read later: every writer updates
		// the map and this counter together while holding it, so reading them a
		// lock apart can pair a fresh registered with a stale active, and a reconnect
		// committing a batch in between then looks like a full gap.
		registered := int(sess.notifications.registered.Load())
		sess.notifications.lock.Unlock()
		if wanted == 0 && active == 0 {
			continue // the caller has asked for nothing; nothing to protect
		}
		// Subscriptions can die while the beat keeps arriving, and silence is the
		// only other trigger for recovery. Read the beat once and share it with the
		// silence check: the gap branch must not fire on a dead beat.
		beats := sess.notifications.heartbeatBeats.Load()
		beatArrived := beats != lastBeats

		gapTicks++
		if registered > active {
			// Count the gap regardless, act only on a tick that saw a beat: the
			// ticker runs at the beat's period, so gating the count on beatArrived
			// lets jitter reset it before it ever reaches allowed.
			if beatArrived {
				allowed := heartbeatAllowedTicks(sess.heartbeatAllowedMisses(), consecutiveFailures, cycle)
				if gapTicks >= allowed {
					gapTicks = 0
					sess.logger.Error("subscriptions are missing; recovering",
						"want", registered, "have", active,
						"detail", "the beat is arriving, so silence would never have revealed this")
					switch sess.recoverDeadSubscriptions() {
					case recoveryDone:
						consecutiveFailures = 0
					case recoveryDeferred:
						// Runtime not serving yet: wait, exactly as the silence path does.
					default:
						consecutiveFailures++
					}
					// This tick saw a beat, so record it exactly as the silence check
					// would have. Skipping it leaves lastBeats stale, and beatArrived
					// then stays true for ever -- including after the beat dies.
					lastBeats = beats
					quietTicks = 0
					continue
				}
			}
		} else {
			gapTicks = 0
		}
		// Silence measured in ticks of this ticker, not in wall-clock time: the
		// ticker is monotonic, so a clock step cannot make a healthy session look
		// dead (or a dead one look healthy). See notificationManager.heartbeatBeats.
		if beatArrived {
			lastBeats = beats
			quietTicks = 0
			// A beat is proof of life, so a previously-observed silent window no
			// longer counts toward Confirm's two-in-a-row.
			silentWindows = 0
			// And the failure counter goes with it. Under Observe nothing else ever
			// resets it — no recovery runs — so without this the first episode is the
			// only one that warns or fires the callback (both are gated on zero), and
			// the tolerated window doubles for every later episode. A beat means the
			// PLC is serving our heartbeat again, which is exactly the evidence that
			// the previous failures no longer describe the situation.
			consecutiveFailures = 0
			continue
		}
		quietTicks++
		// One decision for every path reaching a silent window: has any frame arrived
		// since the last one? Attempts that keep failing are evidence about the link,
		// not the subscriptions. Spawned, not inline: this runs on the heartbeat
		// watcher and Close waits heartbeatWG.
		escalate := func() bool {
			frames := uint64(0)
			if c := sess.client.Load(); c != nil {
				frames = c.framesSeen()
			}
			if frames != framesAtWindow {
				framesAtWindow = frames
				framelessWindows = 0
				return false
			}
			framelessWindows++
			if framelessWindows < heartbeatFailuresBeforeReconnect {
				return false
			}
			sess.logger.Error("no frame has arrived on either socket; treating the transport as dead and reconnecting",
				"windows", framelessWindows, "framesSeen", frames,
				"detail", "re-subscribing cannot fix a link that is gone, and retrying on it keeps TCP from noticing")
			go sess.triggerReconnect()
			return true
		}
		// Each consecutive failed recovery doubles the tolerated silence, capped, so
		// a PLC that stays in CONFIG for an hour costs a handful of attempts instead
		// of one per interval.
		allowed := heartbeatAllowedTicks(sess.heartbeatAllowedMisses(), consecutiveFailures, cycle)
		if quietTicks < allowed {
			continue
		}
		// No beat registered at all: re-attempt that first. It is the cheap
		// explanation for silence — one Add, versus tearing down and re-subscribing
		// everything — and it is the only way a session whose first attempt failed
		// ever gets a watchdog. Only if the beat is in place does continued silence
		// mean the subscriptions are dead.
		if sess.notifications.heartbeatHandle.Load() == 0 {
			quietTicks = 0
			// Counted as a failure so the interval grows: a device that refuses
			// cyclic notifications outright would otherwise get a round-trip every
			// `allowed` ticks forever. The log is throttled already; the request
			// rate was not.
			consecutiveFailures++
			ctx, cancel := context.WithTimeout(sess.currentLifecycleCtx(), cycle)
			_ = sess.establishHeartbeat(ctx)
			cancel()
			// Checked here too, not only after a recovery: recoverDeadSubscriptions
			// releases the heartbeat handle, so once one attempt has failed the loop
			// lives in this branch and never reaches the recovery path again.
			if escalate() {
				return
			}
			continue
		}
		quietTicks = 0

		// One Error per episode, because nothing is being delivered and a
		// notification session loses the samples in the window. Repeats go to Debug:
		// the operator needs to know the subscriptions died, not to be told again
		// every interval until they recover.
		msg := "no notification heartbeat within the allowed window; treating this session's subscriptions as dead and re-subscribing"
		args := []any{
			"cycle", cycle, "missedTicks", allowed,
			// Wall clock, so only ever informational — the decision above is made in
			// ticks. A stepped clock makes this number odd, not the outcome wrong.
			"silentForApprox", time.Duration(allowed) * cycle,
			"detail", "a runtime restart or CONFIG toggle stops delivery without dropping the connection, " +
				"changing the symbol version or reporting an error",
		}
		if consecutiveFailures == 0 {
			sess.logger.Error(msg, args...)
		} else {
			sess.logger.Debug(msg, append(args, "retry", consecutiveFailures)...)
		}

		switch mode := sess.heartbeatRecoveryMode(); mode {
		case HeartbeatRecoveryConfirm:
			// One silent window is not evidence enough for this caller: require a
			// second consecutive one before churning every handle. silentWindows is
			// reset by any beat arriving (quietTicks going back to zero above resets
			// the tick count; this counter is reset on recovery and on a beat below),
			// so two here means two in a row.
			silentWindows++
			if silentWindows < 2 {
				sess.logger.Info("heartbeat silent, waiting for a second window before re-subscribing",
					"mode", mode.String(), "window", 1)
				continue
			}
			silentWindows = 0
		case HeartbeatRecoveryObserve:
			// Report and do nothing. The consumer owns the decision; churning 41
			// handles under a caller that would rather rebuild the session is the
			// thing this mode exists to avoid.
			if consecutiveFailures == 0 {
				sess.logger.Error("heartbeat silent; not re-subscribing (WithHeartbeatRecovery(Observe))",
					"detail", "this session's subscriptions are dead until the consumer rebuilds it")
				// Spawned, never called inline: Close waits heartbeatWG, and this
				// runs ON the heartbeat watcher, so a callback that rebuilds the
				// session by calling Close would deadlock against the goroutine it
				// is running in. Rebuilding is precisely what this mode is for.
				// Matches every other versionCallback site.
				if cb := sess.versionCallback; cb != nil {
					go cb(ReasonHeartbeatSilent)
				}
			}
			consecutiveFailures++
			continue
		default:
			silentWindows = 0
		}

		switch sess.recoverDeadSubscriptions() {
		case recoveryDone:
			consecutiveFailures = 0
		case recoveryDeferred:
			// Nothing was attempted, so this says nothing about how hard recovery is.
			// Counting it doubled the wait every window while a runtime sat in CONFIG,
			// so by the time it returned to RUN the next attempt was minutes out —
			// measured on 192.168.3.118, which never recovered inside a 2 minute grace.
		default:
			consecutiveFailures++
			if escalate() {
				return
			}
		}
	}
}

// recoverDeadSubscriptions releases what the PLC may still hold and re-subscribes
// from the stored configs, then re-establishes the heartbeat.
//
// Reports the outcome so the watcher can tell the three cases apart: recovered,
// failed (back off), and deferred because the runtime is not serving (do not back
// off — nothing was attempted, and the state poll will say when to try again).
type recoveryOutcome int

const (
	recoveryFailed recoveryOutcome = iota
	recoveryDone
	recoveryDeferred
)

func (sess *Session) recoverDeadSubscriptions() recoveryOutcome {
	// Atomic with markClosed, not a bare check, which heartbeatWatch's own is: Close
	// releases the PLC resources BEFORE cancelling the context, so a recovery
	// entering that window still has a live transport and registers handles after
	// the release meant to be terminal -- nobody will ever delete them.
	if !sess.admitBackgroundWork() {
		sess.logger.Debug("skipping subscription recovery: the session is closed")
		return recoveryFailed
	}
	// Before touching anything: a runtime that is not serving cannot accept a
	// re-subscribe, so releasing the handles and attempting one only to fail is pure
	// churn — and the attempt used to be counted as a failure, which doubled the
	// backoff every window. On hardware that meant the session was minutes away from
	// its next try by the time the runtime came back.
	if state, known := sess.knownRuntimeState(); known && runtimeDefinitelyNotServing(state) {
		sess.logger.Info("re-subscribe deferred: the PLC runtime is not in RUN",
			"state", uint16(state))
		return recoveryDeferred
	}

	ctx := sess.currentLifecycleCtx()

	// Held across the whole sequence below — snapshot the intent, try, restore on
	// failure — so no other re-subscribe can run between the snapshot and the
	// restore. Serialising only the attempt would still let a reconnect's
	// re-subscribe read the intent this one has already cleared.
	sess.notifications.resubscribeMu.Lock()
	defer sess.notifications.resubscribeMu.Unlock()

	// The transport is alive here — only the PLC's notification table died — so
	// quiesce dispatch. Takes the old heartbeat with it, so a stale handle cannot
	// be mistaken for a beat once a new one is registered, and so
	// establishHeartbeat below is not a no-op.
	//
	// bestEffortDeleteNotifications runs with userTeardown=false, so
	// notificationChannel survives for the resubscribe that follows.
	stale := sess.takeNotificationHandles(true)
	sess.releaseNotificationHandles(ctx, stale, "dead subscriptions before re-subscribing")

	// Snapshot the caller's intent before trying. resubscribeNotifications treats
	// a failed attempt as one of a bounded number of retries and drops the config
	// after three — which is right for a reconnect, and wrong here: while the PLC
	// is in CONFIG every attempt fails, and burning the budget threw away
	// subscriptions the caller never cancelled. Measured on hardware: three
	// refusals and the session was silent for good.
	sess.notifications.lock.Lock()
	intent := make([]pendingNotification, len(sess.notifications.pending))
	copy(intent, sess.notifications.pending)
	channel := sess.notifications.notificationChannel
	sess.notifications.lock.Unlock()

	restoreIntent := func() {
		sess.notifications.lock.Lock()
		// Merge, not overwrite: a subscribe that landed while this attempt was in
		// flight must not be erased by a snapshot taken before it existed.
		sess.notifications.restoreConfigs(intent)
		if sess.notifications.notificationChannel == nil {
			sess.notifications.notificationChannel = channel
		}
		sess.notifications.lock.Unlock()
	}

	if err := sess.resubscribeNotificationsLocked(); err != nil {
		restoreIntent()
		if errors.Is(err, ErrRuntimeNotRunning) {
			// The gate refused mid-flight: the runtime stopped serving between the
			// check above and the attempt. Deferred, not failed.
			sess.logger.Info("re-subscribe deferred: the PLC runtime stopped serving", "error", err)
			return recoveryDeferred
		}
		sess.logger.Error("re-subscribe after a heartbeat timeout failed; keeping the subscriptions on file and retrying in the next window",
			"error", err, "configs", len(intent))
		return recoveryFailed
	}
	// A "successful" resubscribe that bound nothing is the CONFIG case: the PLC
	// refused every item. Same treatment — keep the intent, try again later.
	sess.notifications.lock.Lock()
	bound := len(sess.notifications.activeNotifications)
	sess.notifications.lock.Unlock()
	if bound == 0 && len(intent) > 0 {
		restoreIntent()
		sess.logger.Error("re-subscribe bound nothing (PLC not serving yet); keeping the subscriptions on file and retrying in the next window",
			"configs", len(intent))
		return recoveryFailed
	}
	sess.notifications.heartbeatLastNs.Store(time.Now().UnixNano())
	_ = sess.establishHeartbeat(ctx)
	sess.notifications.lock.Lock()
	sess.notifications.raiseRegistered()
	sess.notifications.lock.Unlock()
	sess.logger.Info("subscriptions re-established after the heartbeat stopped")
	return recoveryDone
}
