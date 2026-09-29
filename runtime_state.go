package ads

import (
	"context"
	"fmt"
	"time"
)

// defaultStateWatchInterval is how often the runtime-state poller runs. Fixed, not
// derived from the heartbeat cycle: that coupling let WithNotificationHeartbeat
// silently stale the CONFIG gate by half a minute.
const defaultStateWatchInterval = 5 * time.Second

// stateWatchCycle is the runtime-state poll interval: the configured one, or
// defaultStateWatchInterval.
func (sess *Session) stateWatchCycle() time.Duration {
	if sess.stateWatchInterval > 0 {
		return sess.stateWatchInterval
	}
	return defaultStateWatchInterval
}

// RuntimeState reads the device's ADS state from the system service port. This is
// the SYSTEM's state, not the runtime port's: ADSStateConfig means no runtime port
// is serving. Answers while the runtime is unavailable, which is the point.
func (sess *Session) RuntimeState(ctx context.Context) (ADSState, error) {
	c := sess.client.Load()
	if c == nil {
		return ADSStateInvalid, ErrTransportClosed
	}
	state, err := c.ReadStateOnPort(ctx, PortSystemService)
	if err != nil {
		return ADSStateInvalid, err
	}
	sess.recordRuntimeState(state.ADSState)
	return state.ADSState, nil
}

// runtimeStateQuietly is RuntimeState with the transport-fault logging suppressed,
// for the probe at connect: a device without a system service port answers every one
// of these with an AMS error, and readStateOn reports that at Error in steady state.
func (sess *Session) runtimeStateQuietly(ctx context.Context) (ADSState, error) {
	c := sess.client.Load()
	if c == nil {
		return ADSStateInvalid, ErrTransportClosed
	}
	c.beginHandshake()
	defer c.endHandshake()
	state, err := c.ReadStateOnPort(ctx, PortSystemService)
	if err != nil {
		return ADSStateInvalid, err
	}
	sess.recordRuntimeState(state.ADSState)
	return state.ADSState, nil
}

func (sess *Session) recordRuntimeState(state ADSState) {
	previous := ADSState(sess.runtimeState.Swap(uint32(state)))
	sess.runtimeStateNs.Store(time.Now().UnixNano())
	if previous == state || previous == ADSStateInvalid {
		return
	}
	sess.logger.Info("PLC runtime state changed", "from", previous, "to", state)
	// No nudge into the heartbeat watcher. One was written and removed: with
	// deferrals no longer counted as failures the interval never inflates while the
	// runtime is away, so no test could tell the nudge from its absence.
}

// runtimeStateTTL is how long a reading is trusted; beyond it the gates permit
// again. Without it a session that saw CONFIG and lost the system service refused
// everything for life. Failing open costs one attempt the PLC answers, failing
// closed costs the session.
const runtimeStateTTL = 30 * time.Second

// knownRuntimeState returns the last observed state and whether one was observed
// recently enough to act on.
func (sess *Session) knownRuntimeState() (ADSState, bool) {
	state := ADSState(sess.runtimeState.Load())
	if state == ADSStateInvalid {
		return state, false
	}
	// Wall clock, deliberately: a clock step here can only make a fresh reading look
	// stale, which permits — the safe direction. (Contrast the heartbeat detector,
	// where a step in either direction was harmful, so that one counts ticks.)
	if at := sess.runtimeStateNs.Load(); at != 0 && time.Since(time.Unix(0, at)) > runtimeStateTTL {
		return ADSStateInvalid, false
	}
	return state, true
}

// requireRunningRuntime refuses an operation that cannot work outside RUN. With
// no reading at all it permits rather than inventing a reason to fail. A
// whitelist of provably-not-serving states, not "anything but RUN": refusing on
// unfamiliar states would break a working device with no PLC error to explain it.
func runtimeDefinitelyNotServing(state ADSState) bool {
	switch state {
	case ADSStateConfig, ADSStateReconfig:
		// The measured cases. TC3.1.4024 in CONFIG reports 15 and answers every
		// request to a runtime port with AMS ErrorCode 6 (target port not found).
		return true
	default:
		// Everything else is permitted, including STOP and SHUTDOWN. STOP was only
		// ever observed as a ~4s way-point during CONFIG -> RUN; whether a device can
		// idle there while serving is unknown, and refusing on inference is a worse
		// failure than letting the PLC answer.
		return false
	}
}

func (sess *Session) requireRunningRuntime(what string) error {
	if state, known := sess.knownRuntimeState(); known && runtimeDefinitelyNotServing(state) {
		return fmt.Errorf("%s: %w (ADS state %d); the runtime port is not serving, so this cannot succeed until it returns to RUN",
			what, ErrRuntimeNotRunning, uint16(state))
	}
	return nil
}

// startRuntimeStateWatch polls the system service for the runtime state at the
// heartbeat interval. Polling is the only option: in CONFIG the runtime port that
// would carry a notification does not exist. Gives up after a run of failures, so
// a device without a system service port costs nothing and the gates permit.
func (sess *Session) startRuntimeStateWatch() {
	// Checked OUTSIDE stateOnce.Do: consuming the Once here would leave a session
	// that had the watch disabled unable to ever start one. With it off, Close's
	// wait is a no-op and the gates fall back to permitting.
	if sess.stateWatchDisabled {
		sess.logger.Debug("runtime state watch disabled by option")
		return
	}
	sess.stateOnce.Do(func() {
		started := sess.trackGoroutineOn(&sess.stateWG, func() {
			interval := sess.stateWatchCycle()
			ticker := time.NewTicker(interval)
			defer ticker.Stop()
			failures := 0
			const giveUpAfter = 5
			for {
				select {
				case <-sess.lifecycle.closedCh:
					return
				case <-ticker.C:
				}
				// Connected, not merely "not disconnected": dialAndStart clears the flag
				// before the route, reload and resubscribe steps run. Polling through
				// that fires requests at a router not yet serving us, and counts each
				// timeout as evidence the device has no system service.
				if sess.lifecycle.state.load() != SessionStateConnected {
					continue
				}
				c := sess.client.Load()
				if c == nil || (c.ctx != nil && c.ctx.Err() != nil) {
					continue
				}
				// requestTimeout, not the tick period: a device answering slower than
				// one interval would otherwise be declared unable to answer at all.
				pollTimeout := sess.requestTimeout
				if pollTimeout < interval {
					pollTimeout = interval
				}
				ctx, cancel := context.WithTimeout(sess.currentLifecycleCtx(), pollTimeout)
				// Quietly: on a device with no system service every poll fails, and
				// readStateOn logs a transport fault at Error in steady state, which
				// is exactly the log-based health signal transportFaultLevel exists
				// to protect.
				c.beginHandshake()
				state, err := c.ReadStateOnPort(ctx, PortSystemService)
				c.endHandshake()
				cancel()
				if err != nil {
					// Only an answer is evidence. A timeout is what a busy device or a
					// router mid-activation produces, so counting those towards "no
					// system service" retired the feature on healthy hardware.
					if !isDeviceAnswer(err) {
						sess.logger.Debug("runtime-state poll did not get an answer; not counting it against the device",
							"error", err)
						continue
					}
					failures++
					sess.logger.Debug("the system service refused the runtime-state read",
						"error", err, "attempt", failures)
					if failures >= giveUpAfter {
						// Clear the last reading on the way out, or it stands forever
						// with nothing left to refresh it and every gated call keeps
						// refusing. knownRuntimeState's TTL would eventually do this
						// too; doing it here makes the hand-off immediate.
						sess.runtimeState.Store(uint32(ADSStateInvalid))
						sess.logger.Info("this device does not answer on the system service port; runtime-state reporting is off for this session, and symbol calls will be attempted as before",
							"port", uint32(PortSystemService), "attempts", failures)
						return
					}
					continue
				}
				failures = 0
				sess.recordRuntimeState(state.ADSState)
			}
		})
		if !started {
			sess.logger.Debug("not starting the runtime-state watch: the session is closed")
		}
	})
}
