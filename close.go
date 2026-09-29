package ads

import (
	"encoding/binary"
	"time"
)

// releasePLCResources frees notifications and, while the transport is alive,
// symbol handles. Notification cleanup runs even disconnected: the PLC keys
// subscriptions by source NetID and would deliver them to the next session using
// it. Stranded symbol handles are harmless and get reaped on route timeout.
func (sess *Session) releasePLCResources(wasDisconnected bool) {
	// Terminal, so no need to quiesce dispatch. Takes the heartbeat with it, which
	// is why Close no longer releases that separately.
	handles := sess.takeNotificationHandles(false)
	if len(handles) > 0 {
		deleted := sess.bestEffortDeleteNotifications(sess.currentLifecycleCtx(), handles)
		sess.logger.Info("releasePLCResources: best-effort notification cleanup",
			"requested", len(handles), "deleted", deleted,
			"wasDisconnected", wasDisconnected)
	}

	if wasDisconnected {
		sess.logger.Info("already disconnected, skipping handle cleanup")
		return
	}
	// Collect symbol handles under lock, then release without holding the lock.
	sess.cache.lock.Lock()
	symHandles := make([]uint32, 0, len(sess.cache.symbols))
	for _, symbol := range sess.cache.symbols {
		if symbol.Handle != 0 {
			symHandles = append(symHandles, symbol.Handle)
		}
	}
	sess.cache.lock.Unlock()

	// Release handles individually — ADS has no batch release command.
	// Re-check disconnected each iteration so a mid-loop PLC failure
	// doesn't force every remaining Write to time out.
	for i, h := range symHandles {
		if sess.isDisconnected() {
			sess.logger.Info("releasePLCResources: disconnected during handle release, stopping cleanup",
				"released", i,
				"remaining", len(symHandles)-i)
			break
		}
		handleBytes := make([]byte, 4)
		binary.LittleEndian.PutUint32(handleBytes, h)
		if err := sess.client.Load().Write(sess.currentLifecycleCtx(), uint32(GroupSymbolReleaseHandle), 0, handleBytes); err != nil {
			sess.logger.Warn("failed to release symbol handle", "error", err, "handle", h)
		} else {
			// Per handle. The notification-delete path was demoted in 6fc9b14; this is
			// the read-handle path, which was missed then.
			sess.logger.Debug("handle deleted", "handle", h)
		}
	}
}

// shutdownTransport is the non-blocking half of teardown: release PLC resources,
// stop the listener, cancel, close the sockets. This is what makes the workers
// return on their own, so a caller that cannot wait for them leaves nothing
// behind. Runs once per session, whichever path arrives first.
func (sess *Session) shutdownTransport(wasDisconnected bool) {
	sess.lifecycle.shutdownOnce.Do(func() {
		// Stop accepting inbound PLC connections before tearing the transport down,
		// so the accept loop cannot hand one to a Client that is going away.
		sess.stopPeerListener()
		// releasePLCResources collects the heartbeat along with the caller's handles
		// (see takeNotificationHandles), so it does not need releasing separately.
		sess.releasePLCResources(wasDisconnected)
		// Capture cancel under RLock then release before invoking — see
		// tearDownAndReset for the symmetric pattern. Holding RLock across the
		// cancel() blocks tearDownAndReset's ctxMu.Lock replacement.
		sess.lifecycle.ctxMu.RLock()
		cancel := sess.lifecycle.shutdown
		sess.lifecycle.ctxMu.RUnlock()
		if cancel != nil {
			cancel()
		}
		// Close the TCP connection to unblock listen(), which may be stuck in ReadFull.
		sess.tx.connMu.Lock()
		if sess.tx.connection != nil {
			_ = sess.tx.connection.Close()
		}
		sess.tx.connMu.Unlock()
		if c := sess.client.Load(); c != nil {
			c.markDropped() // same reason as in tearDownAndReset
			// Adopted inbound connections have their own readers; closing the sockets
			// is what lets those readers return.
			c.closePeerConns()
		}
	})
}

// closeReconnectGrace bounds how long Close waits for an in-flight reconnect
// attempt to notice it should stop. Generous relative to a dial: the point is a
// ceiling, not a deadline.
const closeReconnectGrace = 10 * time.Second

// Close releases PLC-side notifications and symbol handles, cancels the session
// context, closes the socket and waits for the workers. Idempotent: the teardown
// runs once and a repeat call only re-waits finished workers. Implements
// io.Closer.
func (sess *Session) Close() error {
	// Capture transport-disconnected state BEFORE the FSM transitions into
	// Closed. The cleanup branch below uses this to decide whether to attempt
	// network ops; once state is Closed, isDisconnected() returns false even
	// if the transport was already gone.
	wasDisconnected := sess.isDisconnected()
	// NOT gated on winning this transition: giveUpReconnecting may already have
	// moved the FSM to Closed, and returning early there left the socket, listener
	// and workers up. Only Close can wait for those workers, since
	// giveUpReconnecting runs inside the goroutine it waits for.
	sess.lifecycle.state.transitionToOnce(SessionStateClosed)
	sess.markClosed()
	sess.logger.Info("Close called, shutting down")
	sess.shutdownTransport(wasDisconnected)
	// Wait out any in-progress reconnect BEFORE the goroutine waitGroup: its retry
	// loop may Add after Close, and Wait first races that into "WaitGroup misuse".
	// closedCh tells it to stop, reconnectDone closes when it returns.
	sess.lifecycle.reconnectMu.Lock()
	ch := sess.lifecycle.reconnectDone
	sess.lifecycle.reconnectMu.Unlock()
	if ch != nil {
		// Bounded: a bare receive made Close's latency the reconnect loop's worst
		// case. Proceeding on the timeout is safe -- the waits below are what
		// actually establish that no goroutine of ours is running.
		select {
		case <-ch:
		case <-time.After(closeReconnectGrace):
			sess.logger.Warn("Close proceeded without waiting out the in-flight reconnect",
				"grace", closeReconnectGrace,
				"detail", "an attempt was mid-dial when Close ran; its own context is cancelled and it will exit")
		}
	}
	// The heartbeat watcher's exit was the one Close never observed, so it could
	// return mid-recovery. Waited after the cancel and socket close above, so
	// anything in flight aborts rather than holding this up.
	sess.heartbeatWG.Wait()
	sess.stateWG.Wait()
	sess.logger.Info("Waiting for workers to close")
	if c := sess.client.Load(); c != nil {
		// Repeated from shutdownTransport on purpose: a reconnect in flight when the
		// teardown ran may have swapped in a different *Client, and waiting on one
		// whose sockets are still open never returns.
		c.markDropped()
		c.closePeerConns()
		c.waitGroup.Wait()
	}
	sess.lifecycle.waitGroup.Wait()
	sess.logger.Info("Close DONE")
	return nil
}
