package ads

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// sessionLifecycle owns Session's lifecycle plumbing: context, waitgroup,
// single-flight reconnect signalling, the FSM state with its epoch counter, and
// the retry policy. In session.go because only Session methods touch it.
type sessionLifecycle struct {
	ctxMu sync.RWMutex // protects ctx and shutdown against concurrent access during reconnect
	// The original ctx passed to NewSession. The active lifecycle ctx is re-derived
	// from it after every tearDownAndReset, so cancelling the original still shuts
	// the session down across any number of reconnects. Set once, never replaced.
	parentCtx context.Context
	ctx       context.Context
	shutdown  context.CancelFunc
	waitGroup sync.WaitGroup

	reconnectMu   sync.Mutex // protects reconnectDone
	reconnectDone chan struct{}

	// Makes one teardown+dial pair atomic against another, so two TCPs cannot
	// coexist and get us evicted (Beckhoff #49). Handshake redials only; the
	// reconnect loop would self-deadlock. INVARIANT: nothing on lifecycle.waitGroup
	// and no Client worker may take it -- tearDownAndReset waits both while it is held.
	dialMu sync.Mutex

	// unservedCooldown silences the reconnect loop entirely after N attempts where
	// the dial succeeded but the PLC answered nothing. That is a router holding our
	// IP in a state it will not serve, and since it keeps one TCP per host
	// (Beckhoff #85) redialing sustains it. Zero means the default.
	unservedCooldown time.Duration

	// reconnectAttempts counts dials made by the current reconnect loop. Exposed
	// for tests only.
	reconnectAttempts atomic.Int64

	// reconnectOwner is the single-flight gate: whoever flips it false -> true owns
	// the reconnect. Explicit ownership, not the FSM state -- an abandoned
	// Reconnecting state made every later Reconnect return "already in progress",
	// leaving the session there for ever with IsClosed() false.
	reconnectOwner atomic.Bool

	// connecting suppresses the automatic Reconnect a drop would spawn during
	// Connect: a rival tearing down under it leaked the socket, workers and
	// reconnect loop. A flag, not tighter ondrop bookkeeping -- five sites arm it,
	// two from a defer mid-Connect, and one gate is immune to all of them.
	connecting atomic.Bool

	closedCh   chan struct{}
	closedOnce sync.Once
	// closeErr is why the session ended, for Session.Err; set once, before closedCh.
	closeErr atomic.Pointer[error]
	// shutdownOnce guards the terminal teardown, which has two entry points. Gating
	// it on winning the FSM transition instead meant whichever lost did nothing: a
	// give-up left the socket, listener and workers up, and the later Close returned
	// nil without touching them.
	shutdownOnce sync.Once
	// spawnMu makes "is the session closed" and "register a goroutine" one decision.
	// A bare isClosed() before waitGroup.Add is a TOCTOU that panics the process with
	// "Add called concurrently with Wait". Reachable from user goroutines too, via
	// Subscribe -> ... -> tryOrphanDelete.
	spawnMu sync.Mutex // guards close(closedCh) so Close() and Reconnect-exhaustion can both fire safely

	// The FSM state plus the unified epoch counter, which is the source of truth for
	// closed and reconnecting. epoch bumps on every Connected entry and on cache
	// swaps that do not transition through Reloading.
	state sessionFSM

	// Genuine (re)entries into Connected, so the heartbeat watcher drops a silence
	// count describing subscriptions a reconnect has rebuilt. Not epoch, which also
	// fires on symbol swaps and would mask a real stall.
	connectedGen atomic.Uint64

	autoReconnect              bool
	maxReconnectAttempts       int
	backoffConfig              BackoffConfig
	strictReconnect            bool
	strictReconnectMaxAttempts int
	strictReconnectFailures    int

	// Flap detection: a connection that drops again inside flapWindow counts as a
	// flap, and flapCount feeds reconnectBackoff so the tiers govern cross-cycle
	// behaviour too. Without it a PLC that RSTs everything reports "attempts=1"
	// every few ms, since each Reconnect starts from zero.
	flapMu          sync.Mutex
	lastConnectedAt time.Time
	flapCount       int
}

const (
	// flapWindow marks a SEVERE flap: a connection that did not even last this
	// long counts double, because a PLC resetting us within five seconds is not
	// going to be helped by trying again soon.
	flapWindow = 5 * time.Second
	// How long a connection must survive to count as stabilised. Must meet the flap
	// threshold with no gap: a dead zone left a device resetting on a timer at a
	// constant first-tier cooldown for ever, ~500 sockets an hour.
	flapResetWindow = 60 * time.Second
)

// nextFlapCount decides how a drop moves the flap counter; pure and separate
// because both ways it goes wrong are invisible. Shorter than flapWindow counts
// double, shorter than flapResetWindow is a flap, longer resets, and no previous
// Connected is a flap only if this attempt served nothing.
func nextFlapCount(prev int, lastConnected, now time.Time, servedNothing bool) int {
	if lastConnected.IsZero() {
		if servedNothing {
			return prev + 1
		}
		return prev
	}
	switch elapsed := now.Sub(lastConnected); {
	case elapsed < flapWindow:
		return prev + 2
	case elapsed < flapResetWindow:
		return prev + 1
	default:
		return 0
	}
}

// enterConnected announces Connected and advances connectedGen only when the
// session really came from a connect or reconnect, i.e. its subscriptions were
// just rebuilt. Every production transition goes through here.
func (sess *Session) enterConnected() {
	from, ok := sess.lifecycle.state.transitionTo(SessionStateConnected)
	if !ok {
		sess.logger.Warn("FSM invalid transition (ignoring)",
			"from", from, "to", SessionStateConnected)
		return
	}
	sess.logger.Log(context.Background(), LevelTrace, "FSM transition",
		"from", from, "to", SessionStateConnected)
	if from == SessionStateConnecting || from == SessionStateReconnecting {
		sess.lifecycle.connectedGen.Add(1)
	}
}

// connectedGen returns the connected-generation counter. Read lock-free, one Load
// per heartbeat tick; a stale read costs one extra tick of delay and never a
// skipped recovery.
func (sess *Session) connectedGen() uint64 {
	return sess.lifecycle.connectedGen.Load()
}

// trackGoroutine registers a background goroutine with the session's WaitGroup and
// starts it, refusing once closed. Callers holding resources for it (a semaphore
// slot, a throttle entry) must release them when this returns false.
func (sess *Session) trackGoroutine(fn func()) bool {
	return sess.trackGoroutineOn(&sess.lifecycle.waitGroup, fn)
}

// trackGoroutineOn is trackGoroutine against a specific WaitGroup. Choose by
// lifetime or reconnect deadlocks: tearDownAndReset waits lifecycle.waitGroup on
// every reconnect, so only self-finishing goroutines belong there. Session-lived
// ones (peer accept, heartbeat, state watch) need their own group, waited by Close.
func (sess *Session) trackGoroutineOn(wg *sync.WaitGroup, fn func()) bool {
	sess.lifecycle.spawnMu.Lock()
	defer sess.lifecycle.spawnMu.Unlock()
	// closedCh, not isClosed(): isClosed() reads the FSM, and closedCh is closed
	// first (and by paths like giveUpReconnecting that signal shutdown before the
	// state settles). The earliest signal is the one that has to gate the Add.
	select {
	case <-sess.lifecycle.closedCh:
		return false
	default:
	}
	if sess.isClosed() {
		return false
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		fn()
	}()
	return true
}

// admitBackgroundWork reports whether work touching PLC state may still start.
// Shares spawnMu with markClosed so "not closed" and "we have begun" are one
// decision: a bare isClosed() leaves a window where Close finishes its release and
// the work re-registers handles nobody will delete.
func (sess *Session) admitBackgroundWork() bool {
	sess.lifecycle.spawnMu.Lock()
	defer sess.lifecycle.spawnMu.Unlock()
	select {
	case <-sess.lifecycle.closedCh:
		return false
	default:
	}
	return !sess.isClosed()
}

// markClosed closes the closedCh signal channel exactly once. Safe for
// concurrent invocation from Close() and from Reconnect-exhaustion path.
func (sess *Session) markClosed() {
	// Under spawnMu so it pairs with trackGoroutine: once this returns, every
	// subsequent registration attempt sees the session closed and declines, so the
	// Wait that follows cannot race an Add.
	sess.lifecycle.spawnMu.Lock()
	defer sess.lifecycle.spawnMu.Unlock()
	sess.lifecycle.closedOnce.Do(func() {
		close(sess.lifecycle.closedCh)
	})
}

// reconnectAttemptsForTest exposes the dial counter to tests in this package.
func (l *sessionLifecycle) reconnectAttemptsForTest() int64 {
	return l.reconnectAttempts.Load()
}
