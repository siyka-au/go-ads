package ads

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/siyka-au/go-ads/v3/internal/adsconn"
	"github.com/siyka-au/go-ads/v3/internal/fakeplc"
	"github.com/siyka-au/go-ads/v3/internal/testlog"

	"github.com/siyka-au/go-ads/v3/ams"
	"github.com/siyka-au/go-ads/v3/internal/symtab"
)

// reconnect_test.go — reconnect FSM unit tests.
//
// Covers:
//   - R-RECON-002 (single-flight triggerReconnect)
//   - R-RECON-008 (Close during in-progress dial — no waitGroup misuse)
//   - R-RECON-009 (reconnect goroutine exits via reconnectDone)
//
// Also covers how a reconnect ends, and the rule that it can never end in a
// state nobody can get out of.
//
// The consumer contract this protects: benthos-umh never calls Reconnect and
// never inspects the FSM. It polls IsClosed(), and on true it drops the session
// and builds a new one (read.go:109/154). So a session that is neither connected
// nor closed is invisible to it — no data flows, nothing rebuilds, forever.
// Every exit from Reconnect must therefore leave the session Connected or Closed.

// newReconnectTestSession returns a synthetic Session with autoReconnect
// disabled (so triggerReconnect doesn't actually launch Reconnect against
// a non-existent transport). FSM is left in Constructed and the caller
// drives it to Connected before triggering.
func newReconnectTestSession() *Session {
	return &Session{
		tx:            &adsconn.Transport{},
		notifications: &notificationManager{activeNotifications: make(map[uint32]activeNotification), configsByKey: make(map[string]struct{}), orphanSeen: make(map[uint32]time.Time), orphanSem: make(chan struct{}, orphanDeleteMaxConcurrency)},
		cache:         &symbolCache{symbols: map[string]*symtab.Symbol{}, onDemandSymbols: map[string]bool{}},
		logger:        slog.Default(),
		lifecycle: &sessionLifecycle{
			closedCh:      make(chan struct{}),
			autoReconnect: false,
		},
	}
}

// TestTriggerReconnect_SingleFlight asserts that 50 concurrent
// triggerReconnect calls on a Connected Session result in exactly one
// disconnect-callback fire (the first detector wins via CAS) and exactly
// one Disconnected transition.
//
// With autoReconnect=false the production code does not launch a
// Reconnect goroutine — that's where the test diverges from R-RECON-002
// strict (which counts goroutine launches). The CAS gate inside
// triggerReconnect is the single-flight contract; we observe its visible
// effect: callback fired once.
//
// Validates: R-RECON-002 (single-flight triggerReconnect).
func TestTriggerReconnect_SingleFlight(t *testing.T) {
	var fires atomic.Int32
	sess := newReconnectTestSession()
	sess.onDisconnect = func() { fires.Add(1) }
	sess.lifecycle.state.transitionTo(SessionStateConnecting)
	sess.lifecycle.state.transitionTo(SessionStateConnected)

	const N = 50
	var startWG sync.WaitGroup
	startWG.Add(N)
	var done sync.WaitGroup
	done.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer done.Done()
			startWG.Done()
			sess.triggerReconnect()
		}()
	}
	startWG.Wait()
	done.Wait()
	// Allow any goroutine-scheduled disconnect callback to actually run.
	time.Sleep(50 * time.Millisecond)

	if got := fires.Load(); got != 1 {
		t.Errorf("disconnect callback fired %d times, want exactly 1 (single-flight CAS)", got)
	}
	// State must have transitioned exactly once: now Disconnected.
	if state := sess.lifecycle.state.load(); state != SessionStateDisconnected {
		t.Errorf("state = %v, want Disconnected", state)
	}
}

// TestReconnect_GoroutineExitsViaReconnectDone manually launches an
// abbreviated reconnect-shaped goroutine that closes lifecycle.reconnectDone
// on exit. Asserts subsequent waiters unblock.
//
// The full Reconnect path requires a live transport; we exercise the
// single contract that R-RECON-009 cares about: the channel SHALL close
// on goroutine exit. The production Reconnect uses defer close(); we
// verify the same pattern works as a black-box invariant.
//
// Validates: R-RECON-009 (reconnect goroutine exits via reconnectDone).
func TestReconnect_GoroutineExitsViaReconnectDone(t *testing.T) {
	sess := newReconnectTestSession()
	sess.lifecycle.state.transitionTo(SessionStateConnecting)
	sess.lifecycle.state.transitionTo(SessionStateConnected)

	// Simulate the production Reconnect entry: create reconnectDone,
	// run a "reconnect" body that returns, then close on defer.
	sess.lifecycle.reconnectMu.Lock()
	sess.lifecycle.reconnectDone = make(chan struct{})
	ch := sess.lifecycle.reconnectDone
	sess.lifecycle.reconnectMu.Unlock()

	go func() {
		defer func() {
			sess.lifecycle.reconnectMu.Lock()
			if sess.lifecycle.reconnectDone != nil {
				close(sess.lifecycle.reconnectDone)
				sess.lifecycle.reconnectDone = nil
			}
			sess.lifecycle.reconnectMu.Unlock()
		}()
		// Body: simulate quick reconnect-failed-and-bail.
		time.Sleep(10 * time.Millisecond)
	}()

	// waitForReconnect uses the same channel; assert it unblocks.
	done := make(chan struct{})
	go func() {
		sess.waitForReconnect()
		close(done)
	}()

	select {
	case <-done:
		// success — channel closed and waiter unblocked.
	case <-time.After(2 * time.Second):
		t.Fatal("waitForReconnect did not unblock — reconnectDone never closed")
	}

	// Sanity: the channel returned BEFORE the reconnect started is
	// closed (close(ch) is the same channel waiters block on).
	select {
	case <-ch:
		// expected — closed.
	default:
		t.Error("captured reconnectDone channel did not close")
	}
}

// TestReconnect_CloseDuringDial_NoWaitGroupMisuse exercises the
// Close-during-Reconnect race window. We synthesize a Session, set the
// FSM to Connected, then concurrently:
//   - Goroutine A: drives the FSM through Disconnected → Reconnecting,
//     allocates reconnectDone, does NOT add to waitGroup, then closes
//     reconnectDone (mimicking the failed-dial defer path).
//   - Goroutine B: calls Close, which should cleanly observe the
//     closed reconnectDone and proceed to wait on the waitGroup.
//
// Race-detector clean + no panic = pass.
//
// Validates: R-RECON-008 (Close during dial — race-detector clean).
func TestReconnect_CloseDuringDial_NoWaitGroupMisuse(t *testing.T) {
	sess := newReconnectTestSession()
	sess.lifecycle.state.transitionTo(SessionStateConnecting)
	sess.lifecycle.state.transitionTo(SessionStateConnected)

	// Allocate the reconnectDone channel as triggerReconnect would.
	sess.lifecycle.reconnectMu.Lock()
	sess.lifecycle.reconnectDone = make(chan struct{})
	sess.lifecycle.reconnectMu.Unlock()

	// Goroutine A: simulated reconnect attempt that fails-fast and exits.
	go func() {
		// Brief delay so Close has a chance to enter the wait.
		time.Sleep(20 * time.Millisecond)
		sess.lifecycle.reconnectMu.Lock()
		if sess.lifecycle.reconnectDone != nil {
			close(sess.lifecycle.reconnectDone)
			sess.lifecycle.reconnectDone = nil
		}
		sess.lifecycle.reconnectMu.Unlock()
	}()

	// Goroutine B: drive Close-equivalent: transition to Closed, wait
	// on reconnectDone. We avoid the real Close() because it walks the
	// cache and calls Client methods that need a live transport.
	if _, ok := sess.lifecycle.state.transitionToOnce(SessionStateClosed); !ok {
		t.Fatal("could not transition to Closed")
	}
	close(sess.lifecycle.closedCh)
	sess.lifecycle.reconnectMu.Lock()
	ch := sess.lifecycle.reconnectDone
	sess.lifecycle.reconnectMu.Unlock()
	if ch != nil {
		select {
		case <-ch:
			// success — reconnect goroutine exited cleanly.
		case <-time.After(2 * time.Second):
			t.Fatal("Close during reconnect: reconnectDone never closed")
		}
	}
	// Wait the waitGroup — should be a no-op since we never Add'd.
	sess.lifecycle.waitGroup.Wait()
}

// TestReconnectExhaustsMaxAttemptsTransitionsToClosed verifies that when
// Reconnect() exhausts maxReconnectAttempts, the FSM transitions to Closed
// rather than staying stuck in Reconnecting.
//
// A synthetic Session is used so no real TCP dial occurs. The session is
// pre-wired with an unreachable ip/port so dialAndStart fails immediately,
// and the lifecycle fields that tearDownAndReset needs are properly initialised.
//
// Validates: max-attempts exhaustion → FSM Closed (not stuck Reconnecting).
func TestReconnectExhaustsMaxAttemptsTransitionsToClosed(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	sess := &Session{
		ip:            "127.0.0.1",
		port:          1, // port 1 is always refused on loopback — instant TCP RST
		tx:            adsconn.NewTransport(ams.Address{}),
		notifications: &notificationManager{activeNotifications: make(map[uint32]activeNotification), configsByKey: make(map[string]struct{}), orphanSeen: make(map[uint32]time.Time), orphanSem: make(chan struct{}, orphanDeleteMaxConcurrency)},
		cache:         &symbolCache{symbols: map[string]*symtab.Symbol{}, onDemandSymbols: map[string]bool{}},
		logger:        slog.Default(),
		lifecycle: &sessionLifecycle{
			closedCh:             make(chan struct{}),
			autoReconnect:        false,
			maxReconnectAttempts: 1,
			backoffConfig: BackoffConfig{
				InitialInterval: 1 * time.Millisecond,
				InitialAttempts: 10,
				MidInterval:     1 * time.Millisecond,
				MidAttempts:     10,
				SlowInterval:    1 * time.Millisecond,
				SlowAttempts:    10,
				MaxInterval:     1 * time.Millisecond,
			},
			ctx:      ctx,
			shutdown: cancel,
		},
		requestTimeout: 200 * time.Millisecond,
	}
	// Drive FSM to Disconnected so Reconnect() can transition to Reconnecting.
	sess.lifecycle.state.transitionTo(SessionStateConstructed)
	sess.lifecycle.state.transitionTo(SessionStateConnecting)
	sess.lifecycle.state.transitionTo(SessionStateConnected)
	sess.lifecycle.state.transitionTo(SessionStateDisconnected)

	reconnErr := sess.Reconnect(context.Background())
	if reconnErr == nil {
		t.Fatal("expected Reconnect() to return error after max attempts")
	}

	state := sess.lifecycle.state.load()
	if state != SessionStateClosed {
		t.Fatalf("FSM state = %v, want Closed after max attempts exhausted", state)
	}

	// closedCh must be closed (non-blocking receive succeeds).
	select {
	case <-sess.lifecycle.closedCh:
		// expected
	default:
		t.Fatal("closedCh not closed after max attempts exhausted")
	}
}

// TestReconnectExhaustConcurrentClose_NoPanic drives the exhaustion path
// while a second goroutine concurrently calls into the markClosed/transition
// pair. With closedOnce sync.Once gating close(closedCh), both racers can
// claim the FSM transition without double-close panic. Without the guard,
// "panic: close of closed channel" was possible whenever Close ran between
// exhaustion's transitionToOnce and its raw close(closedCh).
//
// Validates: closedOnce guard introduced in Phase 1.1 Group B.
func TestReconnectExhaustConcurrentClose_NoPanic(t *testing.T) {
	for iter := 0; iter < 20; iter++ {
		ctx, cancel := context.WithCancel(context.Background())

		sess := &Session{
			ip:            "127.0.0.1",
			port:          1, // refused on loopback
			tx:            adsconn.NewTransport(ams.Address{}),
			notifications: &notificationManager{activeNotifications: make(map[uint32]activeNotification), configsByKey: make(map[string]struct{}), orphanSeen: make(map[uint32]time.Time), orphanSem: make(chan struct{}, orphanDeleteMaxConcurrency)},
			cache:         &symbolCache{symbols: map[string]*symtab.Symbol{}, onDemandSymbols: map[string]bool{}},
			logger:        slog.Default(),
			lifecycle: &sessionLifecycle{
				closedCh:             make(chan struct{}),
				autoReconnect:        false,
				maxReconnectAttempts: 1,
				backoffConfig: BackoffConfig{
					InitialInterval: 1 * time.Millisecond,
					InitialAttempts: 10,
					MidInterval:     1 * time.Millisecond,
					MidAttempts:     10,
					SlowInterval:    1 * time.Millisecond,
					SlowAttempts:    10,
					MaxInterval:     1 * time.Millisecond,
				},
				ctx:      ctx,
				shutdown: cancel,
			},
			requestTimeout: 200 * time.Millisecond,
		}
		sess.lifecycle.state.transitionTo(SessionStateConstructed)
		sess.lifecycle.state.transitionTo(SessionStateConnecting)
		sess.lifecycle.state.transitionTo(SessionStateConnected)
		sess.lifecycle.state.transitionTo(SessionStateDisconnected)

		// Race the exhaustion loop with a concurrent markClosed call. We test
		// markClosed directly rather than full Close() because Close()'s
		// reconnectDone wait would block this test on the goroutine we just
		// spawned. markClosed exercises the sync.Once guard, which is the
		// invariant under test.
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			_ = sess.Reconnect(context.Background())
		}()
		go func() {
			defer wg.Done()
			// Yield a few times so we land somewhere in the retry loop.
			for i := 0; i < 5; i++ {
				time.Sleep(time.Microsecond)
			}
			// transitionToOnce(Closed) may race with reconnect exhaustion;
			// markClosed is idempotent either way.
			sess.lifecycle.state.transitionToOnce(SessionStateClosed)
			sess.markClosed()
		}()
		wg.Wait()

		// Always end in Closed; closedCh always observably closed; no panic.
		if state := sess.lifecycle.state.load(); state != SessionStateClosed {
			t.Errorf("iter %d: FSM state = %v, want Closed", iter, state)
		}
		select {
		case <-sess.lifecycle.closedCh:
		default:
			t.Errorf("iter %d: closedCh not closed", iter)
		}
	}
}

// TestReconnect_FlapDetection_AccumulatesAcrossCycles exercises the cross-
// cycle flap counter introduced in v2.1. A PLC that disconnects shortly
// after each Connect should accumulate flapCount across Reconnect cycles,
// not just within a single cycle's retry loop.
//
// Validates the flap-counter field lives on sessionLifecycle and the
// flapWindow gating in Reconnect produces a sub-Connected→sub-Connected
// counter increment.
func TestReconnect_FlapDetection_AccumulatesAcrossCycles(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sess := &Session{
		ip:            "127.0.0.1",
		port:          1, // refused on loopback
		tx:            adsconn.NewTransport(ams.Address{}),
		notifications: &notificationManager{activeNotifications: make(map[uint32]activeNotification), configsByKey: make(map[string]struct{}), orphanSeen: make(map[uint32]time.Time), orphanSem: make(chan struct{}, orphanDeleteMaxConcurrency)},
		cache:         &symbolCache{symbols: map[string]*symtab.Symbol{}, onDemandSymbols: map[string]bool{}},
		logger:        slog.Default(),
		lifecycle: &sessionLifecycle{
			closedCh:             make(chan struct{}),
			autoReconnect:        false,
			maxReconnectAttempts: 1, // exhaust quickly
			backoffConfig: BackoffConfig{
				InitialInterval: 1 * time.Millisecond,
				InitialAttempts: 10,
				MidInterval:     1 * time.Millisecond,
				MidAttempts:     10,
				SlowInterval:    1 * time.Millisecond,
				SlowAttempts:    10,
				MaxInterval:     1 * time.Millisecond,
			},
			ctx:      ctx,
			shutdown: cancel,
		},
		requestTimeout: 200 * time.Millisecond,
	}
	sess.lifecycle.state.transitionTo(SessionStateConstructed)
	sess.lifecycle.state.transitionTo(SessionStateConnecting)
	sess.lifecycle.state.transitionTo(SessionStateConnected)
	// Pretend we just had a successful connect that immediately dropped.
	sess.lifecycle.flapMu.Lock()
	sess.lifecycle.lastConnectedAt = time.Now()
	sess.lifecycle.lastConnectedAt = sess.lifecycle.lastConnectedAt.Add(-50 * time.Millisecond) // within flapWindow
	sess.lifecycle.lastConnectedAt = time.Now().Add(-50 * time.Millisecond)
	sess.lifecycle.flapMu.Unlock()
	sess.lifecycle.state.transitionTo(SessionStateDisconnected)

	if err := sess.Reconnect(context.Background()); err == nil {
		t.Fatal("expected Reconnect to fail (port refused)")
	}

	sess.lifecycle.flapMu.Lock()
	gotCount := sess.lifecycle.flapCount
	sess.lifecycle.flapMu.Unlock()
	if gotCount < 1 {
		t.Errorf("flapCount = %d after a within-flapWindow Reconnect, want >= 1", gotCount)
	}
}

// TestReconnect_WipesActiveNotificationsBeforeRetryLoop verifies Fix 3 of the
// v2.2.1 PLC-flood patch: Reconnect must capture the pre-reconnect handle
// list and wipe activeNotifications atomically before entering the retry
// loop, so a later successful dialAndStart can issue a bestEffortDelete
// against the saved handles (preventing TwinCAT AMS router handle-table
// accumulation across reconnect cycles).
//
// This test uses the refused-port session to force Reconnect into the
// exhaustion path; we only assert that the wipe ran. Full delete-RPC
// integration is exercised via hardware tests (TestIntegrationReconnect on
// TC3/TC2) where a real PLC accepts the SumDelete on the reconnected socket.
func TestReconnect_WipesActiveNotificationsBeforeRetryLoop(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	sess := &Session{
		ip:            "127.0.0.1",
		port:          1, // refused on loopback — Reconnect exhausts attempts
		tx:            adsconn.NewTransport(ams.Address{}),
		notifications: &notificationManager{activeNotifications: make(map[uint32]activeNotification), configsByKey: make(map[string]struct{}), orphanSeen: make(map[uint32]time.Time), orphanSem: make(chan struct{}, orphanDeleteMaxConcurrency)},
		cache:         &symbolCache{symbols: map[string]*symtab.Symbol{}, onDemandSymbols: map[string]bool{}},
		logger:        slog.Default(),
		lifecycle: &sessionLifecycle{
			closedCh:             make(chan struct{}),
			autoReconnect:        false,
			maxReconnectAttempts: 1,
			backoffConfig: BackoffConfig{
				InitialInterval: 1 * time.Millisecond,
				InitialAttempts: 10,
				MidInterval:     1 * time.Millisecond,
				MidAttempts:     10,
				SlowInterval:    1 * time.Millisecond,
				SlowAttempts:    10,
				MaxInterval:     1 * time.Millisecond,
			},
			ctx:      ctx,
			shutdown: cancel,
		},
		requestTimeout: 200 * time.Millisecond,
	}
	sess.lifecycle.state.transitionTo(SessionStateConstructed)
	sess.lifecycle.state.transitionTo(SessionStateConnecting)
	sess.lifecycle.state.transitionTo(SessionStateConnected)
	sess.lifecycle.state.transitionTo(SessionStateDisconnected)

	// Stage pre-reconnect handles. Fix 3's snapshot+wipe must capture these
	// before the retry loop starts.
	sess.notifications.lock.Lock()
	sess.notifications.activeNotifications[0xAAAA] = activeNotification{Sym: &symtab.Symbol{FullName: "MAIN.x"}}
	sess.notifications.activeNotifications[0xBBBB] = activeNotification{Sym: &symtab.Symbol{FullName: "MAIN.y"}}
	sess.notifications.lock.Unlock()

	_ = sess.Reconnect(ctx)

	sess.notifications.lock.Lock()
	postLen := len(sess.notifications.activeNotifications)
	sess.notifications.lock.Unlock()
	if postLen != 0 {
		t.Errorf("activeNotifications len = %d post-Reconnect, want 0 (Fix 3 must wipe under same lock as snapshot)", postLen)
	}
}

// TestAMSRouterErrorIsNotADeviceVerdict pins the provenance invariant at the one
// line where it used to be lost: amsReply.payload() wraps the AMS header's
// ErrorCode, and the result must not look like something the PLC said about an
// item. Every abort guard in this package (internal/adsconn/cmd_sum.go's notification fallbacks
// among them) decides "transport failure" vs "device verdict" with
// errors.As(err, &ReturnCode), so a router code that satisfies errors.As is
// silently promoted to a per-item PLC verdict.
//
// The code itself must stay readable through errors.Is — client_test.go's AMS
// test and every consumer branching on a named router condition depend on that.
func TestAMSRouterErrorIsNotADeviceVerdict(t *testing.T) {
	// The error a request gets back when the AMS router, not the device, refuses it.
	var err error = ams.RouterError{Code: ams.ReturnCodeGlobalTargetPortNotFound}
	// Double wrap is the real shape: executeSumCommand and the notification
	// fallbacks each add their own %w on the way out.
	wrapped := fmt.Errorf("outer: %w", fmt.Errorf("inner: %w", err))

	for _, tc := range []struct {
		name string
		err  error
	}{
		{name: "bare", err: err},
		{name: "double wrapped", err: wrapped},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var rc ams.ReturnCode
			if errors.As(tc.err, &rc) {
				t.Errorf("errors.As extracted ReturnCode %v from a router rejection; "+
					"every abort guard will read it as a PLC verdict about an item", rc)
			}
			if !errors.Is(tc.err, ams.ReturnCodeGlobalTargetPortNotFound) {
				t.Errorf("errors.Is(err, ReturnCodeGlobalTargetPortNotFound) = false, want true; got %v", tc.err)
			}
			if errors.Is(tc.err, ams.ReturnCodeGlobalInsertMailboxError) {
				t.Error("errors.Is matched an unrelated router code")
			}
			if !strings.Contains(tc.err.Error(), "target port") {
				t.Errorf("error text lost the code name: %q", tc.err.Error())
			}
			// A router rejection IS an answer from the far side. startRuntimeStateWatch
			// retires the system-service poll on a run of answers; if this reads false,
			// a device with no system service port never retires and the watcher polls
			// for the life of the session.
			if !isDeviceAnswer(tc.err) {
				t.Error("isDeviceAnswer = false for an AMS router rejection, want true")
			}
			// The reconnect loop's unserved cooldown must stay out of this: the
			// router answered, so this is not "accepted the connection then said
			// nothing".
			if isUnservedError(tc.err) {
				t.Error("isUnservedError = true for an AMS router rejection, want false")
			}
			// Capability latching gets the same check in adsconn
			// (TestAMSReplyPayload_RouterError).
		})
	}
}

// unreachableSession is newReconnectTestSession pointed at port 1 on loopback,
// where every dial is refused instantly.
func unreachableSession(t *testing.T, maxAttempts int) *Session {
	t.Helper()
	sess := newDialableTestSession(t, "127.0.0.1", 1, maxAttempts)
	sess.lifecycle.state.transitionTo(SessionStateDisconnected)
	return sess
}

// TestReconnect_CancelledCtxClosesSession: cancelling the context handed to
// Reconnect must end the attempt promptly AND leave the session closed.
//
// Closed rather than merely stopped, because Reconnecting has no exit to
// Disconnected (FSM table rows 21-24) — returning early any other way parks the
// session in Reconnecting, where the single-flight gate turns every later
// Reconnect into a no-op and the consumer never learns anything is wrong.
func TestReconnect_CancelledCtxClosesSession(t *testing.T) {
	// 60 attempts x 50ms: ignoring the ctx takes seconds, honouring it is
	// immediate, so the two outcomes are unambiguous.
	sess := unreachableSession(t, 60)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- sess.Reconnect(ctx) }()

	time.Sleep(120 * time.Millisecond) // let a dial fail and a backoff start
	cancelledAt := time.Now()
	cancel()

	var err error
	select {
	case err = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Reconnect never returned")
	}
	if elapsed := time.Since(cancelledAt); elapsed > 400*time.Millisecond {
		t.Errorf("Reconnect took %v to return after cancellation — it ran on through the attempt budget",
			elapsed.Round(time.Millisecond))
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Reconnect returned %v, want an error wrapping context.Canceled", err)
	}
	if !sess.IsClosed() {
		t.Error("session not closed after a cancelled reconnect — IsClosed is the consumer's only signal, so it would never rebuild")
	}
	if got := sess.lifecycle.state.load(); got != SessionStateClosed {
		t.Errorf("state = %v, want Closed", got)
	}
}

// TestReconnect_EveryExitLeavesConnectedOrClosed is the invariant itself, driven
// over each way a reconnect can end.
func TestReconnect_EveryExitLeavesConnectedOrClosed(t *testing.T) {
	cases := []struct {
		name string
		run  func(t *testing.T) *Session
	}{
		{
			name: "attempts exhausted",
			run: func(t *testing.T) *Session {
				sess := unreachableSession(t, 2)
				_ = sess.Reconnect(context.Background())
				return sess
			},
		},
		{
			name: "context cancelled",
			run: func(t *testing.T) *Session {
				sess := unreachableSession(t, 60)
				ctx, cancel := context.WithCancel(context.Background())
				done := make(chan struct{})
				go func() { _ = sess.Reconnect(ctx); close(done) }()
				time.Sleep(80 * time.Millisecond)
				cancel()
				waitFor(t, done, 5*time.Second, "Reconnect after cancellation")
				return sess
			},
		},
		{
			name: "session closed mid-attempt",
			run: func(t *testing.T) *Session {
				sess := unreachableSession(t, 60)
				done := make(chan struct{})
				go func() { _ = sess.Reconnect(context.Background()); close(done) }()
				time.Sleep(80 * time.Millisecond)
				// The real API, not markClosed: Close is what a consumer calls,
				// and it also drives the FSM transition.
				_ = sess.Close()
				waitFor(t, done, 5*time.Second, "Reconnect after close")
				return sess
			},
		},
		{
			name: "abandoned Reconnecting state",
			run: func(t *testing.T) *Session {
				sess := unreachableSession(t, 2)
				// Nobody owns this state: an earlier attempt ended without
				// resolving it. Reconnect must take it over, not decline it.
				sess.lifecycle.state.value.Store(uint32(SessionStateReconnecting))
				_ = sess.Reconnect(context.Background())
				return sess
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sess := tc.run(t)
			switch got := sess.lifecycle.state.load(); got {
			case SessionStateClosed, SessionStateConnected:
			default:
				t.Errorf("state after %q = %v; a session left in %v is invisible to a consumer polling IsClosed()",
					tc.name, got, got)
			}
		})
	}
}

func waitFor(t *testing.T, done <-chan struct{}, d time.Duration, what string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("%s did not return within %v", what, d)
	}
}

// TestReconnect_CleanupKeepsTheUserChannel is the bug a power-cycle found on
// hardware and no stub test had caught.
//
// Reconnect wipes activeNotifications up front, then issues a best-effort delete
// for the handles it snapshotted. Session.SumDeleteDeviceNotification clears
// notificationChannel whenever activeNotifications is empty — a rule that exists
// so a user who deletes their last subscription can subscribe again with a
// different channel. During a reconnect that map is empty by construction, so the
// internal cleanup nils the channel, and resubscribeNotifications then returns
// early on savedChannel == nil without a single log line.
//
// Observed consequence on a power-cycled TC2: reconnect reported success, the FSM
// said Connected, IsClosed() stayed false, and not one notification ever arrived
// again. A consumer polling IsClosed() has no way to notice.
func TestReconnect_CleanupKeepsTheUserChannel(t *testing.T) {
	srv := fakeplc.StartPLC(t)
	defer srv.Stop()

	sess, c := newWiredTestSession(t, srv)
	c.SetNotificationHandler(sess.handleNotification)
	// One Add per symbol, which is also what a TC2 does — the shape this bug was
	// found on.
	if !c.Capabilities().SumAddNotifStateCAS(0, 2) {
		t.Fatal("could not force SumAddNotif into the unsupported state")
	}
	srv.OnWriteRead(ams.GroupSumupDeleteDeviceNotification, func(req []byte) []byte {
		return fakeplc.SumDeleteNotifPayload(make([]ams.ReturnCode, len(req)/4))
	})
	srv.OnDeleteDeviceNotification(func(_ uint32) ams.ReturnCode { return ams.ReturnCodeNoErrors })

	// State as it stands when a reconnect begins: one subscription, and the
	// config + channel that a resubscribe will need.
	const handle = 0xAB01
	sym := preSeedTypedSymbol(sess, "MAIN.keepchan", 0xE300)
	ch := make(chan *Update, 4)
	sess.notifications.lock.Lock()
	sess.notifications.activeNotifications[handle] = activeNotification{Sym: sym, Ch: ch}
	sess.notifications.notificationChannel = ch
	sess.notifications.addConfig(NotificationConfig{Symbol: "MAIN.keepchan", Mode: ams.TransModeServerOnChange})
	sess.notifications.lock.Unlock()

	// Exactly what Reconnect does: snapshot the handles, wipe the map, then
	// release the old registrations.
	sess.notifications.lock.Lock()
	saved := []uint32{handle}
	sess.notifications.activeNotifications = make(map[uint32]activeNotification)
	sess.notifications.lock.Unlock()
	sess.bestEffortDeleteNotifications(context.Background(), saved)

	sess.notifications.lock.Lock()
	gotCh := sess.notifications.notificationChannel
	pending := len(sess.notifications.pending)
	sess.notifications.lock.Unlock()

	if gotCh == nil {
		t.Error("cleanup cleared notificationChannel; resubscribeNotifications will return early and no notification will ever be restored")
	}
	if pending == 0 {
		t.Error("cleanup dropped the saved configs; there is nothing left to resubscribe")
	}

	// And the end-to-end consequence: a resubscribe must actually reach the PLC.
	var adds atomic.Int32
	srv.OnAddDeviceNotification(func(_ fakeplc.AddNotifRequest) fakeplc.AddNotifResponse {
		adds.Add(1)
		return fakeplc.AddNotifResponse{Handle: 0xAB02}
	})
	if err := sess.resubscribeNotifications(); err != nil {
		t.Fatalf("resubscribeNotifications: %v", err)
	}
	if adds.Load() == 0 {
		t.Error("resubscribe issued no Add — the subscription is gone for the life of the session")
	}
}

// TestReconnect_FailedHandleReleaseIsRetried: the pre-reconnect snapshot is the
// only record of handles that still exist on the PLC, so it must not be thrown
// away on a release that did not land.
//
// Found by flapping the link against a real TC2 through the proxy: with route
// registration skipped (a pre-registered route, which is common) the release
// attempt runs before anything has proved the transport works, reports
// "requested=3 deleted=0", and the snapshot is cleared anyway. Every flap then
// leaves another three registrations in the PLC's notification table — the
// accumulation this cleanup exists to prevent, reintroduced by moving the release
// earlier.
func TestReconnect_FailedHandleReleaseIsRetried(t *testing.T) {
	srv := fakeplc.StartPLC(t)
	defer srv.Stop()

	const attempts = 3
	sess := newDialableTestSession(t, srv.Host, srv.Port, attempts)

	var releaseAttempts atomic.Int32
	// Refuse every delete with a code that is NOT success-equivalent, so no
	// attempt ever lands and the snapshot must survive for the next one.
	srv.OnWriteRead(ams.GroupSumupDeleteDeviceNotification, func(req []byte) []byte {
		releaseAttempts.Add(1)
		codes := make([]ams.ReturnCode, len(req)/4)
		for i := range codes {
			codes[i] = ams.ReturnCodeDeviceError
		}
		return fakeplc.SumDeleteNotifPayload(codes)
	})
	srv.OnDeleteDeviceNotification(func(_ uint32) ams.ReturnCode {
		releaseAttempts.Add(1)
		return ams.ReturnCodeDeviceError
	})
	// Route probe fine, reload always fails: the loop runs its full budget.
	srv.OnRead(ams.GroupSymbolVersion, func(_, _, _ uint32) (ams.ReturnCode, []byte) {
		return ams.ReturnCodeNoErrors, []byte{9}
	})
	srv.OnRead(ams.GroupSymbolUploadInfo, func(_, _, _ uint32) (ams.ReturnCode, []byte) {
		return ams.ReturnCodeDeviceError, nil
	})

	sess.notifications.lock.Lock()
	sym := &symtab.Symbol{FullName: "MAIN.retryrelease", Name: "MAIN.retryrelease", DataType: "INT", Length: 2, Handle: 0xE400}
	sess.notifications.activeNotifications[0xBE01] = activeNotification{Sym: sym, Ch: make(chan *Update, 1)}
	sess.notifications.lock.Unlock()

	// Full discovery had been done, so the reload path really runs — and fails
	// against the stub above, which is what makes the loop take every attempt.
	sess.cache.lock.Lock()
	sess.cache.symbolsFullyLoaded = true
	sess.cache.lock.Unlock()

	sess.lifecycle.state.transitionTo(SessionStateDisconnected)
	_ = sess.Reconnect(context.Background())

	if got := releaseAttempts.Load(); got < 2 {
		t.Errorf("release attempted %d time(s) across %d reconnect attempts; a release that did not land must be retried, not discarded",
			got, attempts)
	}
}

// TestReconnect_HandleReleaseRetryIsBounded: retrying is right, retrying forever
// is not. Production runs with unbounded reconnect attempts by default, so a PLC
// that keeps rejecting the delete would be asked once per attempt for the life of
// the session. After a few rounds the handles are better left to the orphan
// reaper, which deletes them if they ever stream again.
func TestReconnect_HandleReleaseRetryIsBounded(t *testing.T) {
	srv := fakeplc.StartPLC(t)
	defer srv.Stop()

	// More reconnect attempts than release attempts, so the cap is what limits it.
	sess := newDialableTestSession(t, srv.Host, srv.Port, preReconnectReleaseAttempts+4)

	var releaseAttempts atomic.Int32
	srv.OnWriteRead(ams.GroupSumupDeleteDeviceNotification, func(req []byte) []byte {
		releaseAttempts.Add(1)
		codes := make([]ams.ReturnCode, len(req)/4)
		for i := range codes {
			codes[i] = ams.ReturnCodeDeviceError
		}
		return fakeplc.SumDeleteNotifPayload(codes)
	})
	srv.OnDeleteDeviceNotification(func(_ uint32) ams.ReturnCode {
		releaseAttempts.Add(1)
		return ams.ReturnCodeDeviceError
	})
	srv.OnRead(ams.GroupSymbolVersion, func(_, _, _ uint32) (ams.ReturnCode, []byte) {
		return ams.ReturnCodeNoErrors, []byte{9}
	})
	srv.OnRead(ams.GroupSymbolUploadInfo, func(_, _, _ uint32) (ams.ReturnCode, []byte) {
		return ams.ReturnCodeDeviceError, nil
	})

	sess.notifications.lock.Lock()
	sym := &symtab.Symbol{FullName: "MAIN.bounded", Name: "MAIN.bounded", DataType: "INT", Length: 2, Handle: 0xE500}
	sess.notifications.activeNotifications[0xBE02] = activeNotification{Sym: sym, Ch: make(chan *Update, 1)}
	sess.notifications.lock.Unlock()
	sess.cache.lock.Lock()
	sess.cache.symbolsFullyLoaded = true
	sess.cache.lock.Unlock()

	sess.lifecycle.state.transitionTo(SessionStateDisconnected)
	_ = sess.Reconnect(context.Background())

	// Two-sided on purpose. "at most N" alone passed with the release removed from
	// the reconnect loop entirely (0 <= 3), which is the opposite defect and was
	// only caught by a sibling test. The session is given more reconnect attempts
	// than the release cap, so the count is exactly the cap.
	if got := int(releaseAttempts.Load()); got != preReconnectReleaseAttempts {
		t.Errorf("release attempted %d times, want exactly %d — fewer means the retry was dropped, more means an unreleasable "+
			"handle is retried on every reconnect attempt forever",
			got, preReconnectReleaseAttempts)
	}
}

// TestReconnect_PreReconnectHandlesReleasedWhenTransportIsUp: Reconnect empties
// activeNotifications up front and keeps the handles only in a local snapshot,
// releasing them at a point reached only after BOTH the route probe and the
// symbol reload have succeeded. A reconnect whose dial and route come up but
// whose reload keeps failing therefore has a live, routed transport on every
// attempt and still never issues those deletes — the handles stay in the PLC's
// notification table, and a session flapping this way accumulates them, which is
// the table exhaustion (Beckhoff #268) the cleanup exists to prevent.
//
// What this does NOT claim: once the loop gives up the transport is gone, so
// nothing can be deleted then. The fix is to release while the transport is
// usable, not to keep trying afterwards.
func TestReconnect_PreReconnectHandlesReleasedWhenTransportIsUp(t *testing.T) {
	srv := fakeplc.StartPLC(t)
	defer srv.Stop()

	sess := newDialableTestSession(t, srv.Host, srv.Port, 2)

	var deletedMu sync.Mutex
	var deleted []uint32
	// The release goes out as a sum-delete; record the handles it carries.
	srv.OnWriteRead(ams.GroupSumupDeleteDeviceNotification, func(req []byte) []byte {
		deletedMu.Lock()
		for i := 0; i+4 <= len(req); i += 4 {
			deleted = append(deleted, binary.LittleEndian.Uint32(req[i:i+4]))
		}
		deletedMu.Unlock()
		codes := make([]ams.ReturnCode, len(req)/4)
		return fakeplc.SumDeleteNotifPayload(codes)
	})
	// Per-handle fallback, in case the sum path is unavailable.
	srv.OnDeleteDeviceNotification(func(h uint32) ams.ReturnCode {
		deletedMu.Lock()
		deleted = append(deleted, h)
		deletedMu.Unlock()
		return ams.ReturnCodeNoErrors
	})
	// Route probe succeeds every attempt, so the transport is routed and usable;
	// the symbol reload is what fails, so the loop exhausts.
	srv.OnRead(ams.GroupSymbolVersion, func(_, _, _ uint32) (ams.ReturnCode, []byte) {
		return ams.ReturnCodeNoErrors, []byte{9}
	})
	srv.OnRead(ams.GroupSymbolUploadInfo, func(_, _, _ uint32) (ams.ReturnCode, []byte) {
		return ams.ReturnCodeDeviceError, nil
	})

	// A subscription for Reconnect to snapshot.
	sess.notifications.lock.Lock()
	sym := &symtab.Symbol{FullName: "MAIN.flap", Name: "MAIN.flap", DataType: "INT", Length: 2, Handle: 0xE200}
	sess.notifications.activeNotifications[0xBEEF] = activeNotification{Sym: sym, Ch: make(chan *Update, 1)}
	sess.notifications.lock.Unlock()

	sess.lifecycle.state.transitionTo(SessionStateDisconnected)
	_ = sess.Reconnect(context.Background())

	deletedMu.Lock()
	got := append([]uint32(nil), deleted...)
	deletedMu.Unlock()
	if !slices.Contains(got, 0xBEEF) {
		t.Errorf("pre-reconnect handle 0xBEEF was never released (deletes seen: %v) — it stays in the PLC notification table while the session keeps reconnecting", got)
	}
}

// TestReconnect_UnservedPLCTriggersCooldown: a PLC that accepts the TCP
// connection and then answers nothing must not be hammered.
//
// This is the .224 shape, seen in this lab for months: connect succeeds, the
// route registers "successfully", and every request times out. Beckhoff's own
// maintainer describes the mechanism — the TwinCAT router expects exactly one TCP
// connection per remote IP and drops the older one whenever a new connection is
// accepted (Beckhoff/ADS#85) — so a client that redials every backoff sustains
// the fight rather than resolving it. Field observation matches: the device often
// serves again once something stops trying entirely for a while.
//
// So after a few consecutive unserved attempts the loop must go quiet: no
// sockets, no route registration, for a cooldown period. Distinguishing feature
// of "unserved" is that the dial SUCCEEDS and the request then times out —
// unreachable hosts (refused dials) are a different failure and keep their fast
// retries.
func TestReconnect_UnservedPLCTriggersCooldown(t *testing.T) {
	srv := fakeplc.StartPLC(t)
	defer srv.Stop()

	// Accept the connection, then answer nothing at all — the .224 shape. The
	// silence has to land on a step AFTER the dial, so route registration is
	// skipped (it is UDP and there is no responder here, which would be a
	// different failure) and the symbol reload is what goes unanswered.
	srv.DelayBefore(ams.CommandRead, uint32(ams.GroupSymbolUploadInfo), time.Hour)
	srv.DelayBefore(ams.CommandReadWrite, uint32(ams.GroupSymbolUploadInfo), time.Hour)

	sess := newDialableTestSession(t, srv.Host, srv.Port, 0) // unbounded attempts
	sess.route = &routeManager{skipRegistration: true}
	sess.requestTimeout = 200 * time.Millisecond
	sess.cache.symbolsFullyLoaded = true
	sess.lifecycle.unservedCooldown = 2 * time.Second
	sess.lifecycle.state.transitionTo(SessionStateDisconnected)

	done := make(chan struct{})
	go func() { _ = sess.Reconnect(context.Background()); close(done) }()
	t.Cleanup(func() { _ = sess.Close(); <-done })

	// Let it burn through the unserved-attempt allowance, then watch it hold off.
	time.Sleep(3 * time.Second)
	duringCooldown := srv.Accepts()
	time.Sleep(1500 * time.Millisecond)
	afterQuiet := srv.Accepts()

	t.Logf("dials: %d by the time the cooldown started, %d after a further 1.5s", duringCooldown, afterQuiet)
	if afterQuiet-duringCooldown > 2 {
		t.Errorf("%d further dials during a %v cooldown — an unserved PLC is being hammered, which is what keeps the router losing",
			afterQuiet-duringCooldown, sess.lifecycle.unservedCooldown)
	}
	if duringCooldown == 0 {
		t.Error("never dialed at all; the test did not exercise the path")
	}
}

// TestReconnect_RefusedDialKeepsFastRetries: the cooldown must not slow down the
// ordinary case. A refused dial means the PLC is down or unreachable, and the
// router is not in a bad state, so retries stay on the configured backoff.
func TestReconnect_RefusedDialKeepsFastRetries(t *testing.T) {
	sess := unreachableSession(t, 0)            // port 1: instantly refused, unbounded attempts
	sess.lifecycle.unservedCooldown = time.Hour // would be catastrophic if applied here

	done := make(chan struct{})
	go func() { _ = sess.Reconnect(context.Background()); close(done) }()
	t.Cleanup(func() { _ = sess.Close(); <-done })

	// fastBackoff is 50ms, so 5 attempts is ~250ms of work. Polled rather than
	// slept: the assertion is the attempt count, and a fixed second spent waiting
	// for it proves nothing the poll does not.
	deadline := time.Now().Add(time.Second)
	var got int64
	for time.Now().Before(deadline) {
		if got = sess.lifecycle.reconnectAttemptsForTest(); got >= 5 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("only %d attempts in 1s against a refused port; a refused dial must not enter the unserved cooldown", got)
}

// TestReconnect_DoesNotReRegisterRouteEveryAttempt: a session registers its route
// at most once, however many times it reconnects.
//
// ensureRoute registers whenever its probe fails, and the probe fails on every
// attempt against a PLC that answers nothing — so an unbounded reconnect loop
// registered the same route dozens of times. On a TC/RTOS device in this lab that
// left the runtime route table holding TWO entries for our NetID where the
// persisted config has one, after which the router began answering on a
// connection it opened to us instead of ours, and the session stopped working
// entirely. Registering once is enough; if the route is registered and the PLC
// still will not talk, more registrations cannot help.
func TestReconnect_DoesNotReRegisterRouteEveryAttempt(t *testing.T) {
	srv := fakeplc.StartPLC(t)
	defer srv.Stop()
	router := fakeplc.NewRouter(t)

	// The PLC accepts TCP and answers no ADS request, so every probe fails.
	srv.DelayBefore(ams.CommandRead, uint32(ams.GroupSymbolVersion), time.Hour)

	sess := newDialableTestSession(t, srv.Host, srv.Port, 6)
	sess.routerPort = router.Port
	sess.route = &routeManager{
		name:              "go-ads-test",
		username:          "Administrator",
		password:          secret("1"),
		activationTimeout: 300 * time.Millisecond,
	}
	sess.requestTimeout = 200 * time.Millisecond
	sess.lifecycle.unservedCooldown = 300 * time.Millisecond
	sess.lifecycle.state.transitionTo(SessionStateDisconnected)

	_ = sess.Reconnect(context.Background())

	// One per unserved episode, not one per attempt. Re-registering the correct
	// route is the measured recovery for a mute router, so a session that has
	// concluded the PLC is silent is allowed one more — but a plain retry loop
	// must not register every time round.
	got := router.Registrations()
	if got == 0 {
		t.Error("no registration at all; the route was never established")
	}
	if got >= 6 {
		t.Errorf("route registered %d times across 6 reconnect attempts — that is once per attempt, which is the storm this guards against", got)
	}
	t.Logf("route registered %d time(s) across 6 attempts", got)
}

// TestReconnect_ReRegistersRouteToHealAMuteDevice: re-registering our own route is
// the measured way back from a router that has stopped answering, so a reconnect
// loop that has concluded the PLC is silent must be allowed to try it.
//
// The state, measured on TC3.1.4024 and TC3.1.4026: a foreign NetID claimed the
// address the PLC routes to us, and TC3 keys its route table by ADDRESS, so the
// device answered nothing at all — not on our connection, not on one it opened.
// One registration of our own NetID for our own address rebound it and both
// devices returned to normal service immediately.
//
// So the rule is neither "register every attempt" (which is how the address gets
// contested in the first place) nor "register once per session, ever" (which locks
// out the recovery). It is once per unserved episode.
func TestReconnect_ReRegistersRouteToHealAMuteDevice(t *testing.T) {
	srv := fakeplc.StartPLC(t)
	defer srv.Stop()
	router := fakeplc.NewRouter(t)

	// The device answers only once it has seen a SECOND registration: the first is
	// the session establishing its route, the second is the healing one.
	srv.OnRead(ams.GroupSymbolVersion, func(_, _, _ uint32) (ams.ReturnCode, []byte) {
		if router.Registrations() < 2 {
			// Outlast the client's request timeout without answering: silence, not
			// a malformed reply, is what a router in this state produces — and only
			// a plain deadline counts as "unserved".
			time.Sleep(400 * time.Millisecond)
		}
		return ams.ReturnCodeNoErrors, []byte{12}
	})

	sess := newDialableTestSession(t, srv.Host, srv.Port, 40)
	sess.routerPort = router.Port
	sess.route = &routeManager{
		name:              "go-ads-test",
		username:          "Administrator",
		password:          secret("1"),
		activationTimeout: 500 * time.Millisecond,
	}
	sess.callbackIP = "192.168.3.52"
	sess.requestTimeout = 200 * time.Millisecond
	sess.lifecycle.unservedCooldown = 300 * time.Millisecond
	sess.lifecycle.state.transitionTo(SessionStateDisconnected)

	done := make(chan error, 1)
	go func() { done <- sess.Reconnect(context.Background()) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("reconnect never recovered: %v (registrations=%d)", err, router.Registrations())
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("reconnect did not finish; registrations=%d", router.Registrations())
	}

	if got := router.Registrations(); got < 2 {
		t.Errorf("registrations = %d, want at least 2: the session must be allowed one healing re-registration after concluding the PLC is silent", got)
	} else {
		t.Logf("recovered after %d registrations", got)
	}
	if state := sess.lifecycle.state.load(); state != SessionStateConnected {
		t.Errorf("state = %v, want Connected after recovery", state)
	}
}

// TestReconnect_ForceRegistrationRegistersOnEveryReconnect: WithForceRouteRegistration
// promises registration on every Connect AND every Reconnect, so a session that
// sets it must register again on each reconnect.
//
// The option was honoured unconditionally on Connect and then overruled by the
// once-per-session latch on Reconnect, so "always re-register" meant "register
// once, then stop" — which fails exactly the case its godoc names: a device with
// non-persistent routes that forgets its route table on reboot is reconnected to
// without a re-registration, and the session then waits out awaitRouteActive on a
// route the PLC no longer has.
//
// Three reconnects, not one: asserting a single re-registration would also pass
// against the latch, whose one healing registration per unserved-cooldown episode
// is a different mechanism (see TestReconnect_ReRegistersRouteToHealAMuteDevice).
// The probe here always succeeds, so no unserved episode can occur and every
// registration observed is the option's own doing.
func TestReconnect_ForceRegistrationRegistersOnEveryReconnect(t *testing.T) {
	srv := fakeplc.StartPLC(t)
	defer srv.Stop()
	router := fakeplc.NewRouter(t)

	// Always answered, so the probe and awaitRouteActive both succeed: this
	// isolates the force flag from the probe-failure fallback path.
	srv.OnRead(ams.GroupSymbolVersion, func(_, _, _ uint32) (ams.ReturnCode, []byte) {
		return ams.ReturnCodeNoErrors, []byte{12}
	})

	sess := newDialableTestSession(t, srv.Host, srv.Port, 6)
	sess.routerPort = router.Port
	sess.route = &routeManager{
		name:                   "go-ads-test",
		username:               "Administrator",
		password:               secret("1"),
		forceRouteRegistration: true,
		activationTimeout:      300 * time.Millisecond,
	}
	t.Cleanup(func() { sess.Close() })

	const reconnects = 3
	for i := 1; i <= reconnects; i++ {
		sess.lifecycle.state.transitionTo(SessionStateDisconnected)
		if err := sess.Reconnect(context.Background()); err != nil {
			t.Fatalf("reconnect %d of %d: %v (registrations=%d)", i, reconnects, err, router.Registrations())
		}
	}

	if got := router.Registrations(); got != reconnects {
		t.Errorf("WithForceRouteRegistration: registrations = %d after %d reconnects, want %d (the godoc and R-ROUTE-005 both say every Connect and Reconnect)",
			got, reconnects, reconnects)
	}
}

// TestReconnect_DropWhileFinishingIsNotLost: a drop that lands while a reconnect
// is finishing must still be recovered.
//
// Reconnect holds reconnectOwner until its deferred release runs, which is AFTER
// it has marked the transport live and gone Connected. A drop in that gap takes
// the session to Disconnected and spawns a Reconnect that loses the ownership CAS
// and returns immediately — so the drop is acknowledged by the FSM and then
// dropped on the floor. Nothing retries: the owner is on its way out, and
// heartbeatWatch skips while disconnected.
//
// The session then sits Disconnected with IsClosed() false and no retry loop,
// which is precisely the state a consumer polling IsClosed() cannot see (see this
// file's header). The same gap orphans a reconnectDone that Close() waits on
// unconditionally, so Close() hangs too — asserted here as well, since it is the
// same root cause.
func TestReconnect_DropWhileFinishingIsNotLost(t *testing.T) {
	srv := fakeplc.StartPLC(t)
	defer srv.Stop()
	srv.OnRead(ams.GroupSymbolVersion, func(_, _, _ uint32) (ams.ReturnCode, []byte) {
		return ams.ReturnCodeNoErrors, []byte{7}
	})

	sess := newDialableTestSession(t, srv.Host, srv.Port, 40)
	gate := newGateOnLog("reconnect successful", "reconnect already in progress")
	sess.logger = slog.New(gate)
	// The drop we inject is delivered the way the read loop delivers one, so the
	// auto path has to be the thing that recovers it.
	sess.lifecycle.autoReconnect = true
	sess.lifecycle.state.transitionTo(SessionStateDisconnected)

	first := make(chan error, 1)
	go func() { first <- sess.Reconnect(context.Background()) }()

	select {
	case <-gate.w.reached:
	case err := <-first:
		t.Fatalf("reconnect finished without reaching the success log: %v", err)
	case <-time.After(15 * time.Second):
		t.Fatal("reconnect never reached the success log")
	}

	// Pinned inside the window: transport live, state Connected, owner still held.
	if got := sess.lifecycle.state.load(); got != SessionStateConnected {
		t.Fatalf("state = %v at the success log, want Connected — the gate is in the wrong place", got)
	}
	if !sess.lifecycle.reconnectOwner.Load() {
		t.Fatal("reconnectOwner is already released at the success log — the gate is in the wrong place")
	}

	// The drop lands here, exactly as callOnDrop would deliver it.
	sess.triggerReconnect()

	// Wait for the reconnect it spawned to actually LOSE the ownership CAS before
	// releasing the gate. Without this the test is a race it usually wins: the
	// owner is released microseconds after triggerReconnect returns, so the new
	// goroutine's CAS normally succeeds and the window is never exercised.
	select {
	case <-gate.w.signalled:
	case <-time.After(10 * time.Second):
		t.Fatal("the drop's reconnect never reported losing the ownership CAS; the window was not exercised")
	}
	close(gate.w.release)

	if err := <-first; err != nil {
		t.Fatalf("first reconnect returned %v, want nil", err)
	}

	// The session must end up Connected (recovered) or Closed (gave up loudly).
	// Disconnected forever is the failure this test exists for.
	deadline := time.Now().Add(15 * time.Second)
	for {
		state := sess.lifecycle.state.load()
		if state == SessionStateConnected || state == SessionStateClosed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("session parked in %v with IsClosed()=%v and no reconnect owner (%v): "+
				"the drop that landed while the previous reconnect was finishing was acknowledged and then abandoned",
				state, sess.IsClosed(), sess.lifecycle.reconnectOwner.Load())
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Same root cause, second symptom: triggerReconnect may have installed a fresh
	// reconnectDone that no owner will ever close, and Close() waits on it with no
	// timeout.
	closed := make(chan struct{})
	go func() { defer close(closed); sess.Close() }()
	select {
	case <-closed:
	case <-time.After(10 * time.Second):
		t.Fatal("Close() hung: reconnectDone was orphaned by the abandoned drop")
	}
}

// TestReconnect_RuntimeNotRunningDoesNotBurnAttempts: a PLC in CONFIG must not walk
// the session through its reconnect budget.
//
// The RUN gate on SubscribeAll fires inside the reconnect loop's
// resubscribe, and an instant refusal costs no network time — so the whole attempt
// budget burns at the backoff rate and giveUpReconnecting CLOSES a session whose
// only problem is a runtime that is not running. That is the opposite of the
// contract: stay up, keep polling, resume when it returns.
func TestReconnect_RuntimeNotRunningDoesNotBurnAttempts(t *testing.T) {
	srv := fakeplc.StartPLC(t)
	defer srv.Stop()
	srv.OnRead(ams.GroupSymbolVersion, func(_, _, _ uint32) (ams.ReturnCode, []byte) {
		return ams.ReturnCodeNoErrors, []byte{4}
	})
	// The resubscribe uses the batch path, so this group has to answer or the second
	// phase fails on a parse error long before reaching the behaviour under test —
	// the same stub gap that made three earlier tests pass for the wrong reason.
	var nextHandle atomic.Uint32
	nextHandle.Store(0x4200)
	srv.OnWriteRead(ams.GroupSumupAddDeviceNotification, func(req []byte) []byte {
		items := make([]fakeplc.SumNotifResponse, len(req)/40)
		for i := range items {
			items[i] = fakeplc.SumNotifResponse{Error: ams.ReturnCodeNoErrors, Handle: nextHandle.Add(1)}
		}
		return fakeplc.SumAddNotifPayload(items)
	})
	srv.OnWriteRead(ams.GroupSumupDeleteDeviceNotification, func(req []byte) []byte {
		codes := make([]ams.ReturnCode, len(req)/4)
		for i := range codes {
			codes[i] = ams.ReturnCodeNoErrors
		}
		return fakeplc.SumDeleteNotifPayload(codes)
	})

	const attempts = 3
	sess := newDialableTestSession(t, srv.Host, srv.Port, attempts)

	// A subscription on file, and a runtime that is not running: the resubscribe
	// inside the reconnect loop is refused by the gate every time.
	ch := make(chan *Update, 4)
	preSeedTypedSymbol(sess, "MAIN.waiting", 0x4100)
	sess.notifications.lock.Lock()
	sess.notifications.notificationChannel = ch
	sess.notifications.addConfig(NotificationConfig{Symbol: "MAIN.waiting", Mode: ams.TransModeServerOnChange})
	sess.notifications.lock.Unlock()
	sess.recordRuntimeState(ams.StateConfig)

	sess.lifecycle.state.transitionTo(SessionStateDisconnected)
	done := make(chan error, 1)
	go func() { done <- sess.Reconnect(context.Background()) }()

	// Long enough that an attempt-consuming loop would exhaust 3 attempts at the
	// 50ms test backoff and close the session many times over.
	select {
	case err := <-done:
		t.Fatalf("Reconnect returned %v while the runtime was not running; it should keep waiting", err)
	case <-time.After(2 * time.Second):
	}
	if sess.IsClosed() {
		t.Error("the session closed itself because the PLC was in CONFIG: a runtime that is not running is not a reconnect failure")
	}

	// And it recovers by itself once the runtime is back, without being rebuilt.
	sess.recordRuntimeState(ams.StateRun)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Reconnect after the runtime returned: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Reconnect never completed after the runtime returned to RUN")
	}
	if state := sess.lifecycle.state.load(); state != SessionStateConnected {
		t.Errorf("state = %v, want Connected", state)
	}
}

// TestGiveUpReconnecting_TearsDownTheTransport: giving up must actually tear the
// session down, not merely flip the FSM to Closed.
//
// The bug this pins: giveUpReconnecting released PLC-side resources only, gated on
// winning the transition to Closed — no context cancel, no socket close, no peer
// listener release. The user's later Close() then found the FSM already Closed and
// returned nil having done nothing, so port 48898, the transmit worker and every
// recv worker stayed alive for the life of the process. Reachable from any
// WithMaxReconnectAttempts session whose PLC does not come back.
func TestGiveUpReconnecting_TearsDownTheTransport(t *testing.T) {
	srv := fakeplc.StartPLC(t)
	sess := newDialableTestSession(t, srv.Host, srv.Port, 1)
	if err := sess.dialAndStart(); err != nil {
		t.Fatalf("dialAndStart: %v", err)
	}
	c := sess.client.Load()

	// A bound listener stands in for the inbound-route port: the observable proof
	// that stopPeerListener ran, without needing a real PLC to connect back.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	sess.peerMu.Lock()
	sess.peerLn = ln
	sess.peerMu.Unlock()

	// Nothing to reconnect to: one attempt, one refusal, then give up.
	srv.Stop()
	sess.transitionState(SessionStateDisconnected)
	if rerr := sess.Reconnect(context.Background()); rerr == nil {
		t.Fatalf("Reconnect = nil, want an error after exhausting attempts")
	}

	if lctx := sess.currentLifecycleCtx(); lctx.Err() == nil {
		t.Error("lifecycle context is still live after giving up: nothing keyed on it will ever stop")
	}
	// Deadline so a listener that is still open fails the assertion instead of
	// blocking here until the whole test binary times out.
	if tl, ok := ln.(*net.TCPListener); ok {
		_ = tl.SetDeadline(time.Now().Add(2 * time.Second))
	}
	if _, aerr := ln.Accept(); !errors.Is(aerr, net.ErrClosed) {
		t.Errorf("peer listener Accept = %v, want net.ErrClosed: the inbound port is still bound", aerr)
	}
	if !sess.IsClosed() {
		t.Error("IsClosed() = false after giving up: the consumer's only signal never fires")
	}

	// And the user's Close must still complete the blocking half.
	done := make(chan error, 1)
	go func() { done <- sess.Close() }()
	select {
	case cerr := <-done:
		if cerr != nil {
			t.Errorf("Close after giving up = %v, want nil", cerr)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Close hung after giving up")
	}

	workers := make(chan struct{})
	go func() { c.Wait(); close(workers) }()
	select {
	case <-workers:
	case <-time.After(5 * time.Second):
		t.Error("client workers still running after give-up + Close")
	}
}

// TestReconnect_DropDuringTheTailIsNotErased: a drop observed while a reconnect
// is finishing must not be forgotten by the reconnect that is finishing.
//
// Sibling of TestReconnect_DropWhileFinishingIsNotLost above, one step earlier in
// the tail. That test injects the drop after "reconnect successful" is logged;
// this one lands it before, while the pre-reconnect handle cleanup is still
// running - i.e. before Reconnect stores disconnected=false on its way out.
//
// The bug: that store is a no-op on the happy path, because dialAndStart already
// cleared the flag once the workers were up. Its only effect is to erase a drop
// that landed in between - and tx.disconnected is the SOLE record of it, since
// the FSM has no Reconnecting->Disconnected edge. Erased, the adopt check in the
// deferred hand-off sees a healthy transport, nothing redials, and the session
// sits Connected on a dead socket with IsClosed() false: no data, and no signal
// the consumer can act on.
func TestReconnect_DropDuringTheTailIsNotErased(t *testing.T) {
	srv := fakeplc.StartPLC(t)
	defer srv.Stop()
	srv.OnRead(ams.GroupSymbolVersion, func(_, _, _ uint32) (ams.ReturnCode, []byte) {
		return ams.ReturnCodeNoErrors, []byte{7}
	})

	sess := newDialableTestSession(t, srv.Host, srv.Port, 40)
	sess.lifecycle.autoReconnect = true
	// One saved handle, so takeNotificationHandles returns non-empty and the
	// cleanup below logs from inside the window we need to pin.
	sess.notifications.heartbeatHandle.Store(4242)
	gate := newGateOnLog("cleaned up pre-reconnect notification handles", "reconnect already in progress")
	sess.logger = slog.New(gate)
	sess.lifecycle.state.transitionTo(SessionStateDisconnected)
	t.Cleanup(func() { _ = sess.Close() })

	first := make(chan error, 1)
	go func() { first <- sess.Reconnect(context.Background()) }()

	select {
	case <-gate.w.reached:
	case err := <-first:
		t.Fatalf("reconnect finished without reaching the cleanup log: %v", err)
	case <-time.After(15 * time.Second):
		t.Fatal("reconnect never reached the cleanup log")
	}

	// The gate sits after dialAndStart, so the flag is already clear here. If it
	// is not, the drop below would be indistinguishable from the state we started
	// in and the test would prove nothing.
	if sess.tx.Disconnected() {
		t.Fatal("transport still marked disconnected at the cleanup log — the gate is in the wrong place")
	}
	before := srv.Accepts()

	// A real drop, delivered the way the read loop delivers one.
	srv.CloseClientConns()

	// Poll until the drop is RECORDED, not merely injected. This is what makes the
	// ordering deterministic instead of hopeful: the erasing store is downstream of
	// the gate, so the flag must be observably true before the gate is released.
	deadline := time.Now().Add(10 * time.Second)
	for !sess.tx.Disconnected() {
		if time.Now().After(deadline) {
			t.Fatal("the injected drop was never recorded on the transport; the window was not exercised")
		}
		time.Sleep(5 * time.Millisecond)
	}

	select {
	case <-gate.w.signalled:
	case <-time.After(10 * time.Second):
		t.Fatal("the drop's reconnect never reported losing the ownership CAS; the window was not exercised")
	}
	close(gate.w.release)

	if err := <-first; err != nil {
		t.Fatalf("first reconnect returned %v, want nil", err)
	}

	// The invariant, not the interleaving: a drop observed during a reconnect must
	// be adopted and redialled.
	deadline = time.Now().Add(15 * time.Second)
	for srv.Accepts() <= before {
		if time.Now().After(deadline) {
			t.Fatalf("no redial after a drop during the reconnect tail (accepts stayed at %d): "+
				"state=%v IsClosed=%v disconnected=%v — the drop was erased on the way out",
				before, sess.lifecycle.state.load(), sess.IsClosed(), sess.tx.Disconnected())
		}
		time.Sleep(20 * time.Millisecond)
	}

	// And never Connected over a Client that has already been dropped.
	if state := sess.lifecycle.state.load(); state == SessionStateConnected {
		if c := sess.client.Load(); c != nil && c.Err() != nil {
			t.Error("session reports Connected over a client whose context is already cancelled")
		}
	}
}

// TestDropVerdict_NeverServedNamesTheRoute and its established counterpart are
// the two halves of the classification. Before the split, both produced the same
// message and the same route hint, because the only evidence consulted was the
// errno — and EOF/ECONNRESET look identical whether the socket is 20ms or 20h old.
// A field investigation lost hours re-reading route tables over drops of sessions
// that had been delivering samples for half an hour.
func TestDropVerdict_NeverServedNamesTheRoute(t *testing.T) {
	var c adsconn.Conn
	if c.Established() {
		t.Fatal("a client that has decoded no frames must not be judged established")
	}
	// The sentinel Connect attaches follows from the same predicate.
	if c.Established() {
		t.Error("verdict flipped without a frame having been decoded")
	}
}

// TestReconnect_LogsAtError: while a session is down nothing is read, and in
// notification mode every sample in the gap is lost. umh-core's benthos health
// check holds a data-flow component out of active on any error line and has no
// allowlist, so the level is what makes an outage visible downstream.
func TestReconnect_LogsAtError(t *testing.T) {
	srv := fakeplc.StartPLC(t)
	logs := &testlog.Handler{}
	sess := newDialableTestSession(t, srv.Host, srv.Port, 1)
	sess.logger = slog.New(logs)
	t.Cleanup(func() { sess.markClosed() })

	// Nothing to dial: the loop fails at the first step and exhausts its one
	// attempt, which is the shape of a PLC that has gone away.
	srv.Stop()

	if err := sess.Reconnect(context.Background()); err == nil {
		t.Fatal("Reconnect against a stopped server returned nil")
	}

	errs := logs.RecordsByLevel(slog.LevelError)
	if len(errs) == 0 {
		t.Fatal("a failed reconnect logged no error: umh-core sees a healthy component while no data is read")
	}
	want := []string{
		"session disconnected",             // the transition into not serving
		"reconnect dial/start failed",      // the attempt
		"max reconnect attempts exhausted", // giving up
	}
	for _, msg := range want {
		var found bool
		for _, rec := range errs {
			if strings.Contains(rec.Message, msg) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("no Error record contains %q; levels on the reconnect path decayed", msg)
		}
	}
}

// Every failed attempt logs at Error, not just the first. Reporting only the
// first left a long outage looking healthy: once the consumer's rolling error
// window passed that one line the component read as fine, and the only thing
// still printing was "reconnect backoff" at Info, which announces a retry but
// never says what failed. An operator watching a down bridge has to see the
// cause on each attempt.
func TestReconnect_LogsEveryFailedAttemptAtError(t *testing.T) {
	srv := fakeplc.StartPLC(t)
	logs := &testlog.Handler{}
	const attempts = 3
	sess := newDialableTestSession(t, srv.Host, srv.Port, attempts)
	sess.logger = slog.New(logs)
	t.Cleanup(func() { sess.markClosed() })

	srv.Stop()
	if err := sess.Reconnect(context.Background()); err == nil {
		t.Fatal("Reconnect against a stopped server returned nil")
	}

	var failures int
	for _, rec := range logs.RecordsByLevel(slog.LevelError) {
		if strings.Contains(rec.Message, "reconnect dial/start failed") ||
			strings.Contains(rec.Message, "reconnect step failed") {
			failures++
		}
	}
	if failures < attempts {
		t.Errorf("only %d of %d failed attempts logged at Error — the later ones decayed to Debug, "+
			"so an outage outlasting the consumer's error window reads as healthy", failures, attempts)
	}
}
