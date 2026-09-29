package ads

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// reconnectBackoff returns the delay for the given reconnect attempt number (1-indexed)
// based on the configured BackoffConfig tiers.
func (sess *Session) reconnectBackoff(attempt int) time.Duration {
	cfg := sess.lifecycle.backoffConfig
	switch {
	case attempt <= cfg.InitialAttempts:
		return cfg.InitialInterval
	case attempt <= cfg.InitialAttempts+cfg.MidAttempts:
		return cfg.MidInterval
	case attempt <= cfg.InitialAttempts+cfg.MidAttempts+cfg.SlowAttempts:
		return cfg.SlowInterval
	default:
		return cfg.MaxInterval
	}
}

// logAttempt reports a failed reconnect attempt at Error, every time. Reporting
// only the first left a long outage looking healthy once a consumer's rolling
// error window passed it, with nothing but "reconnect backoff" at Info still
// printing -- which never says what failed.
func (sess *Session) logAttempt(msg string, args ...any) {
	sess.logger.Error(msg, args...)
}

// reconnectSleep sleeps for the appropriate backoff duration based on the attempt
// number. Returns early if Close() is called.
func (sess *Session) reconnectSleep(ctx context.Context, attempt int) error {
	delay := sess.reconnectBackoff(attempt)
	sess.logger.Info("reconnect backoff", "attempt", attempt, "delay", delay)
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		// The caller gave up mid-backoff. Give-up is terminal for the session —
		// see Reconnect's doc.
		return sess.giveUpReconnecting(fmt.Errorf("reconnect aborted during backoff: %w", ctx.Err()))
	case <-sess.lifecycle.closedCh:
		return fmt.Errorf("connection closed during reconnect")
	}
}

// unservedCooldownDuration is the configured quiet period, or the default.
func (sess *Session) unservedCooldownDuration() time.Duration {
	if d := sess.lifecycle.unservedCooldown; d > 0 {
		return d
	}
	return defaultUnservedCooldown
}

// isDeviceAnswer reports whether err carries an answer from the far side -- an ADS
// return code or a router rejection -- as opposed to silence, which says nothing.
// A rejection is the most direct evidence for an absent port (AMS ErrorCode 0x06).
// AMSError does not unwrap to ReturnCode, so both must be asked about separately.
func isDeviceAnswer(err error) bool {
	var rc ReturnCode
	var amsErr AMSError
	return errors.As(err, &rc) || errors.As(err, &amsErr)
}

// isUnservedError reports whether err means "the PLC accepted our connection and
// then said nothing", as opposed to a refused dial or a PLC-side verdict. A
// timeout with no ADS return code is the signature: the request went out and
// nothing came back.
func isUnservedError(err error) bool {
	if err == nil {
		return false
	}
	var rc ReturnCode
	if errors.As(err, &rc) {
		return false // the PLC answered, even if the answer was an error
	}
	// ErrTransportClosed means the link died under us, which is a drop rather
	// than a refusal to serve — only a plain deadline counts here.
	return errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, ErrTransportClosed)
}

// coolDownAfterUnserved holds the loop off with nothing open, so the router can
// settle. Returns an error only if the session is closed or the caller gave up
// while waiting.
func (sess *Session) coolDownAfterUnserved(ctx context.Context, attempts int, cause error) error {
	d := sess.unservedCooldownDuration()
	sess.logger.Error("PLC accepted the connection but answered nothing; backing off completely before trying again",
		"unservedAttempts", attempts, "cooldown", d, "error", cause,
		"hint", "a stale or duplicate route entry for this source NetID, or another client on this IP, can hold a TwinCAT router in this state")
	// Nothing open while we wait: the point is to stop competing for the
	// router's one-connection-per-IP slot.
	sess.tearDownAndReset()

	// Permit one route registration on the next attempt. Re-registering the
	// correct route is the measured recovery for a router that has stopped
	// answering — two TC3 devices mute, both restored by exactly this — so a
	// session that has concluded the PLC is silent should be allowed to try it.
	if !sess.route.shouldSkip() {
		sess.route.allowHealingRegistration()
	}

	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return sess.giveUpReconnecting(fmt.Errorf("reconnect aborted during unserved cooldown: %w", ctx.Err()))
	case <-sess.lifecycle.closedCh:
		return fmt.Errorf("connection closed during unserved cooldown")
	}
}

// giveUpReconnecting ends a reconnect for good: FSM to Closed, resources released,
// closedCh closed. Shared by attempt exhaustion and cancellation. Closing rather
// than parking in Reconnecting is the point -- a consumer's only liveness signal is
// IsClosed(), so "gave up but still alive" is unobservable.
func (sess *Session) giveUpReconnecting(cause error) error {
	sess.lifecycle.state.transitionToOnce(SessionStateClosed)
	// Idempotent via closedOnce, whichever of Close() and this ran first.
	sess.markClosed()
	// Full teardown: this path is reachable without the user ever calling Close, and
	// the socket and listener would leak for the life of the process. The blocking
	// half stays in Close -- this runs inside the Reconnect goroutine, so waiting on
	// reconnectDone here would deadlock.
	sess.shutdownTransport(true)
	return cause
}

// triggerReconnect prepares the connection state and launches the Reconnect
// goroutine. disconnected and reconnectDone are set BEFORE the launch, closing the
// window where a caller could see a "healthy" connection between the two.
func (sess *Session) triggerReconnect() {
	if sess.isClosed() {
		return
	}
	// CAS ensures only the first goroutine to detect disconnect fires the callback
	// and sets up reconnection. Subsequent callers (e.g. both listen() and transmitWorker()
	// detecting the same TCP failure) skip the callback to avoid double-firing.
	firstDetector := sess.tx.disconnected.CompareAndSwap(false, true)
	if firstDetector {
		sess.transitionState(SessionStateDisconnected)
	}
	// Fire disconnect callback in goroutine (must not block).
	// Callback must not call Session methods — connection may be closing.
	if firstDetector && sess.onDisconnect != nil && !sess.isClosed() {
		go sess.onDisconnect()
	}

	// Connect owns the transport end to end and schedules its own redial from
	// tx.disconnected. Placement is load-bearing: above the CAS the drop is lost,
	// above the callback WithOnDisconnect never fires, and creating reconnectDone
	// here would hang Close's wait on it.
	if sess.lifecycle.connecting.Load() {
		return
	}

	sess.lifecycle.reconnectMu.Lock()
	if sess.lifecycle.reconnectDone == nil {
		sess.lifecycle.reconnectDone = make(chan struct{})
	}
	sess.lifecycle.reconnectMu.Unlock()

	if sess.lifecycle.autoReconnect {
		// Background, not the lifecycle context: the auto path must not treat a
		// session-context replacement as "the caller gave up", which now closes
		// the session. Cancellation of an auto-reconnect is Close's job.
		go func() { _ = sess.Reconnect(context.Background()) }()
	} else {
		// No auto-reconnect: close reconnectDone immediately so sendRequest
		// waiters unblock with ErrDisconnected instead of hanging forever.
		sess.lifecycle.reconnectMu.Lock()
		if sess.lifecycle.reconnectDone != nil {
			close(sess.lifecycle.reconnectDone)
			sess.lifecycle.reconnectDone = nil
		}
		sess.lifecycle.reconnectMu.Unlock()
	}
}

// unservedAttemptsBeforeCooldown is how many consecutive dial-succeeds-then-
// silence attempts are tolerated before the loop goes quiet. Small on purpose: a
// PLC that accepts and does not answer will not change its mind within a few
// hundred milliseconds, and every extra dial makes a router livelock worse.
const unservedAttemptsBeforeCooldown = 3

// defaultUnservedCooldown is long enough for the router to finish whatever it is
// doing with our IP, and short enough that a session recovers on its own. The
// field evidence is "it works again after you stop trying for a bit"; this is that
// pause, made deliberate.
const defaultUnservedCooldown = 30 * time.Second

// preReconnectReleaseAttempts caps how many attempts re-try the delete of handles
// held before the drop. Retrying matters, since the link is usually still down on
// the first, but attempts are unbounded by default and an unreleasable handle must
// not cost a round trip for ever. The orphan reaper is the backstop.
const preReconnectReleaseAttempts = 3

// Reconnect re-establishes the transport, reloads symbols and re-subscribes.
// Cancelling ctx CLOSES the session, as exhausting the attempt limit does -- not a
// pause: Reconnecting has no exit to Disconnected, so a session left there would
// never retry and IsClosed() could not see it.
func (sess *Session) Reconnect(ctx context.Context) error {
	// closeReconnectDone closes the reconnectDone channel if still open and
	// nils it. Mutex + nil-check is safe against concurrent callers — only
	// the first observer of a non-nil channel closes it.
	closeReconnectDone := func() {
		sess.lifecycle.reconnectMu.Lock()
		if sess.lifecycle.reconnectDone != nil {
			close(sess.lifecycle.reconnectDone)
			sess.lifecycle.reconnectDone = nil
		}
		sess.lifecycle.reconnectMu.Unlock()
	}

	if sess.isClosed() {
		// triggerReconnect may have created reconnectDone before Close ran.
		// Close it so Session.Close()'s reconnectDone wait unblocks instead
		// of hanging forever.
		closeReconnectDone()
		return fmt.Errorf("connection closed")
	}
	// Single-flight on explicit ownership, not on the FSM state: see
	// sessionLifecycle.reconnectOwner for why the state cannot serve as the gate.
	if !sess.lifecycle.reconnectOwner.CompareAndSwap(false, true) {
		sess.logger.Info("reconnect already in progress, skipping")
		return nil
	}
	// One defer, in this order on purpose. A drop landing between "transport live"
	// and this release spawns a Reconnect that loses the CAS above, so the FSM
	// acknowledges it and nothing retries -- the session then sits Disconnected with
	// IsClosed() false for ever. Whoever releases ownership must adopt it, and
	// reconnectDone closes first or Close's wait hangs on an orphan trigger.
	defer func() {
		closeReconnectDone()
		sess.lifecycle.reconnectOwner.Store(false)
		if sess.lifecycle.autoReconnect && !sess.isClosed() && sess.tx.disconnected.Load() {
			sess.logger.Info("adopting a drop that arrived while the previous reconnect was finishing")
			go func() { _ = sess.Reconnect(context.Background()) }()
		}
	}()

	// transitionToOnce reports ok=false both for an illegal transition and for
	// "already in that state". Only the first is a refusal: we hold the ownership
	// flag, so an existing Reconnecting state has no live owner and is ours to
	// take over.
	if from, ok := sess.lifecycle.state.transitionToOnce(SessionStateReconnecting); !ok && from != SessionStateReconnecting {
		sess.logger.Info("reconnect not permitted from the current state, skipping", "state", from)
		// The deferred hand-off closes reconnectDone on this path too. Close()
		// waits on that channel with no timeout and no closedCh alternative, so
		// leaving an orphan open here is a hang whether or not we are closed.
		return nil
	} else if !ok {
		sess.logger.Warn("taking over a reconnect state left behind by an earlier attempt")
	}

	// Create a channel that waiters (sendRequest) can block on.
	// triggerReconnect() may have already created it — only create if nil.
	sess.lifecycle.reconnectMu.Lock()
	if sess.lifecycle.reconnectDone == nil {
		sess.lifecycle.reconnectDone = make(chan struct{})
	}
	sess.lifecycle.reconnectMu.Unlock()

	// A Connected -> drop inside flapWindow means the last cycle never stabilised,
	// so back off before dialing and throttle cross-cycle storms too. A drop on a
	// connection that never carried a frame is always a flap, however long it lasted.
	neverServed := false
	if c := sess.client.Load(); c != nil {
		neverServed = !c.wasEstablished()
	}

	sess.lifecycle.flapMu.Lock()
	lastConn := sess.lifecycle.lastConnectedAt
	sess.lifecycle.flapCount = nextFlapCount(sess.lifecycle.flapCount, lastConn, time.Now(), neverServed)
	flapCount := sess.lifecycle.flapCount
	sess.lifecycle.flapMu.Unlock()

	if flapCount > 0 {
		delay := sess.reconnectBackoff(flapCount)
		sess.logger.Error("connection flapping, applying cross-cycle cooldown before reconnect",
			"flapCount", flapCount, "delay", delay,
			"lastConnectedAgo", time.Since(lastConn),
			"lastDropServedNothing", neverServed,
			"detail", "each reconnect costs the PLC an accepted socket; backing off protects its socket table as much as ours")
		timer := time.NewTimer(delay)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return sess.giveUpReconnecting(fmt.Errorf("reconnect aborted during flap cooldown: %w", ctx.Err()))
		case <-sess.lifecycle.closedCh:
			timer.Stop()
			return fmt.Errorf("connection closed during flap cooldown")
		}
	}

	// Error, not Info: from here until "reconnect successful" nothing is read and
	// every notification sample in the gap is lost.
	sess.logger.Error("session disconnected, reconnecting; no data is read until it succeeds",
		"flapCount", flapCount)
	sess.tx.disconnected.Store(true)
	// State is already Reconnecting (transitionToOnce above).

	// Snapshot the handles before clearing: the PLC may still hold them. Load
	// bearing on a silent loss where it never saw a FIN -- those handles stream
	// alongside the new ones and uncleaned fill the router's table (Beckhoff #268).
	// The heartbeat rides along, which lets establishHeartbeat register a fresh one.
	savedHandles := sess.takeNotificationHandles(false)

	sess.tearDownAndReset()

	var lastErr error
	attempts := 0
	releaseTries := 0
	unserved := 0

	// retryAfter handles every post-dial failure the same way: record, tear down,
	// then back off -- or go quiet when the PLC has accepted and said nothing
	// repeatedly. One helper, because four copies is how the unserved case ended up
	// handled in one and missed in the rest.
	retryAfter := func(err error, stage string) error {
		lastErr = err
		// Capture the verdict before the teardown: whether the socket this attempt
		// used ever carried a frame is what separates "the PLC is refusing to serve
		// us" from "the link died mid-work", and only the first should go quiet.
		servedNothing := false
		if c := sess.client.Load(); c != nil {
			servedNothing = !c.wasEstablished()
		}
		sess.logAttempt("reconnect step failed, retrying",
			"stage", stage, "error", err, "attempt", attempts,
			"servedNothing", servedNothing)
		sess.resetForRetry()
		// isUnservedError alone misses the field shape: the PLC accepts the TCP and
		// RSTs it within ~40ms having served nothing -- the same "said nothing", with
		// a reset instead of silence. It stays narrow (excluding ErrTransportClosed,
		// so a mid-stream drop is not misread), so the verdict is added here.
		if isUnservedError(err) || servedNothing {
			unserved++
			if unserved >= unservedAttemptsBeforeCooldown {
				unserved = 0
				return sess.coolDownAfterUnserved(ctx, unservedAttemptsBeforeCooldown, err)
			}
		} else {
			unserved = 0
		}
		return sess.reconnectSleep(ctx, attempts)
	}

	for {
		if sess.isClosed() {
			return fmt.Errorf("connection closed during reconnect")
		}
		attempts++
		if err := ctx.Err(); err != nil {
			sess.logger.Info("reconnect abandoned: caller context done", "error", err, "attempts", attempts-1)
			return sess.giveUpReconnecting(fmt.Errorf("reconnect aborted: %w", err))
		}

		if sess.lifecycle.maxReconnectAttempts > 0 && attempts > sess.lifecycle.maxReconnectAttempts {
			sess.logger.Error("max reconnect attempts exhausted, closing session",
				"maxAttempts", sess.lifecycle.maxReconnectAttempts, "error", lastErr)
			// lastErr can be nil: a path that retries without recording an error
			// (waiting for a runtime that is not running) leaves it unset, and
			// wrapping nil with %w prints "%!w(<nil>)" — which is what this said
			// before.
			giveUp := fmt.Errorf("reconnect failed after %d attempts", sess.lifecycle.maxReconnectAttempts)
			if lastErr != nil {
				giveUp = fmt.Errorf("reconnect failed after %d attempts: %w", sess.lifecycle.maxReconnectAttempts, lastErr)
			}
			return sess.giveUpReconnecting(giveUp)
		}

		// Dial TCP, configure keepalive, clear disconnected flag, start goroutines.
		// dialAndStart re-checks closed.Load() before waitGroup.Add(2).
		sess.lifecycle.reconnectAttempts.Add(1)
		if err := sess.dialAndStart(); err != nil {
			lastErr = err
			sess.logAttempt("reconnect dial/start failed, retrying",
				"error", err, "ip", sess.ip, "port", sess.port, "attempt", attempts)
			if err := sess.reconnectSleep(ctx, attempts); err != nil {
				return err
			}
			continue
		}

		// Re-perform local-mode handshake if needed
		if sess.isLocal {
			if err := sess.localHandshake(); err != nil {
				if rerr := retryAfter(err, "local handshake"); rerr != nil {
					return rerr
				}
				continue
			}
		}

		// Smart route registration: probe first, register only if needed.
		if err := sess.ensureRoute(); err != nil {
			if rerr := retryAfter(err, "route"); rerr != nil {
				return rerr
			}
			continue
		}

		// Release here, not after the reload: first point where the transport is up
		// AND routed, which is all a Delete needs. Forget the snapshot only once
		// every handle is accounted for, or a release that did not land loses the
		// record of what the PLC still holds.
		if len(savedHandles) > 0 {
			releaseTries++
			deleted := sess.bestEffortDeleteNotifications(sess.currentLifecycleCtx(), savedHandles)
			sess.logger.Info("reconnect: cleaned up pre-reconnect notification handles",
				"requested", len(savedHandles), "deleted", deleted)
			switch {
			case deleted >= len(savedHandles):
				savedHandles = nil
			case releaseTries >= preReconnectReleaseAttempts:
				// Bounded on purpose: reconnect attempts are unbounded by default,
				// and a PLC that keeps refusing leaves the orphan reaper as the
				// backstop — it deletes these if they ever stream again.
				sess.logger.Warn("reconnect: giving up on releasing pre-reconnect notification handles",
					"unreleased", len(savedHandles)-deleted, "attempts", releaseTries)
				savedHandles = nil
			default:
				sess.logger.Warn("reconnect: keeping unreleased notification handles for the next attempt",
					"unreleased", len(savedHandles)-deleted, "attempt", releaseTries)
			}
		}

		// Re-load symbols based on discovery mode
		if err := sess.reloadSymbols(); err != nil {
			if rerr := retryAfter(err, "symbol reload"); rerr != nil {
				return rerr
			}
			continue
		}

		// Re-subscribe notifications using stored configs.
		if err := sess.resubscribeNotifications(); err != nil {
			if errors.Is(err, ErrRuntimeNotRunning) {
				// Transport fine, route served, runtime not running. Counting this as an
				// attempt spends the budget with no network involved and eventually
				// closes a session whose only problem is a PLC in CONFIG. Report,
				// sleep, retry without consuming an attempt.
				sess.logger.Info("reconnect: transport restored but the PLC runtime is not running; waiting for it",
					"error", err)
				// resetForRetry, exactly as retryAfter does: the loop dials a fresh
				// connection every iteration and only a teardown stops the previous
				// Client's workers. Skipping it redialed on top of a live Client —
				// caught by -race as a write/read conflict on tx.connection.
				sess.resetForRetry()
				if serr := sess.reconnectSleep(ctx, attempts); serr != nil {
					return serr
				}
				// Give the attempt back. `continue` alone returns to the attempts++ at
				// the loop head, so the budget burned anyway — which is the whole
				// defect: a PLC in CONFIG walked the session through every attempt and
				// closed it, just more slowly.
				attempts--
				continue
			}
			if rerr := retryAfter(err, "notification resubscribe"); rerr != nil {
				return rerr
			}
			continue
		}

		// No disconnected.Store(false) here: dialAndStart already cleared it, so this
		// only ever erased a drop landing in the tail. tx.disconnected is the sole
		// record of one -- the FSM has no Reconnecting->Disconnected edge -- and
		// erasing it left the session Connected on a dead socket.
		sess.lifecycle.strictReconnectFailures = 0 // reset on success
		// epoch bumps inside the transition helper when target == Connected;
		// enterConnected additionally advances the connected generation, which is
		// what tells the heartbeat watcher to drop the pre-drop silence count.
		sess.enterConnected()
		sess.lifecycle.flapMu.Lock()
		sess.lifecycle.lastConnectedAt = time.Now()
		sess.lifecycle.flapMu.Unlock()
		sess.logger.Info("reconnect successful", "attempts", attempts, "flapCount", flapCount)

		// Fire reconnect callback in goroutine (must not block).
		// Callback must not call Session methods — connection may be closing.
		if sess.onReconnect != nil && !sess.isClosed() {
			go sess.onReconnect()
		}
		return nil
	}
}

// filterValidPending returns only pending entries whose symbols still exist
// in the current symbol table. Logs a warning for dropped subscriptions.
func (sess *Session) filterValidPending(entries []pendingNotification) []pendingNotification {
	sess.cache.lock.Lock()
	defer sess.cache.lock.Unlock()

	valid := make([]pendingNotification, 0, len(entries))
	for _, entry := range entries {
		name := entry.Config.SymbolName
		if _, exists := sess.cache.symbols[symbolKey(name)]; exists {
			valid = append(valid, entry)
		} else if _, onDemand := sess.cache.onDemandSymbols[symbolKey(name)]; onDemand {
			valid = append(valid, entry)
		} else {
			sess.logger.Warn("notification symbol gone after reconnect, dropping subscription",
				"symbol", name)
		}
	}
	return valid
}

// resetForRetry tears down goroutines, closes the TCP connection, and resets
// channels/state so the next retry iteration starts clean.
func (sess *Session) resetForRetry() {
	sess.tx.disconnected.Store(true)
	sess.tearDownAndReset()
	// Allow route re-registration on next attempt (PLC may have rebooted)
}
