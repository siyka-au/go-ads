package ads

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/siyka-au/go-ads/v3/ams"
	"github.com/siyka-au/go-ads/v3/internal/fakeplc"
	"github.com/siyka-au/go-ads/v3/router"
)

// TestAddRoute_ReadsTheSourceNetIDUnderItsLock: Session.AddRoute is exported and
// callable from any goroutine, and it read sess.source with no lock while
// localHandshake writes that field under tx.connMu from the reconnect goroutine.
//
// The read that matters is the one INSIDE the spawned goroutine: the code's own
// comment says that goroutine outlives the AddRoute call when the caller's context
// is cancelled, so the racing read is on a detached goroutine with no join. A torn
// NetID there registers a route for a spliced identity — the PLC answers nothing,
// the session burns its whole unserved/cooldown budget, and a junk entry is left in
// the device's route table, which is the failure shape that took two TC3 devices
// mute (see route.go on duplicate entries for one NetID).
//
// The assertion is the race detector's; this test earns its place in the -race job.
// It is deterministic in the accesses it makes, not in whether the detector sees
// them, so the value assertion below stands in when it does not: every registration
// must carry one of the two whole NetIDs, never a mixture of both.
func TestAddRoute_ReadsTheSourceNetIDUnderItsLock(t *testing.T) {
	router := fakeplc.NewRouter(t)

	sess := newDialableTestSession(t, "127.0.0.1", 1, 1)
	sess.routerPort = router.Port
	sess.route = &routeManager{name: "go-ads-test", username: "Administrator", password: secret("1")}
	// No callbackIP: that is what makes AddRoute derive the host IP from the NetID,
	// which is the second bare read.
	sess.callbackIP = ""

	first := [6]byte{10, 0, 0, 1, 1, 1}
	second := [6]byte{192, 168, 3, 52, 1, 1}
	sess.tx.SetSource(ams.Address{NetID: first, Port: 10500})

	// The writer, doing exactly what localHandshake does: replace the whole address
	// under tx.connMu. Every byte differs between the two NetIDs, so a torn read is
	// visible in the value and not only to the detector.
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		for i := 0; i < 500; i++ {
			next := first
			if i%2 == 0 {
				next = second
			}
			sess.tx.SetSource(ams.Address{NetID: next, Port: 10500})
		}
	}()

	for i := 0; i < 40; i++ {
		if err := sess.AddRoute(context.Background(), sess.route.name, sess.route.username, string(sess.route.password)); err != nil {
			t.Fatalf("AddRoute against the stub router: %v", err)
		}
	}
	<-writerDone

	if got := router.Registrations(); got < 40 {
		t.Errorf("registrations = %d, want at least 40: the test did not exercise the read it is about", got)
	}
	for _, netID := range router.RegisteredNetIDs() {
		if netID != first && netID != second {
			t.Errorf("a registration carried NetID %v, which is neither of the two written: the read was torn", netID)
		}
	}
}

// A router that answers nothing must be reported as retryable, not read as a
// missing route. Measured at ~8s of silence on a TC3 4024 after the client
// restarted; the old code spent that window registering a route that existed.
func TestAwaitRouterAwake_DeafRouterIsRetryable(t *testing.T) {
	deaf := fakeplc.NewRouter(t) // no identity: never answers identify
	defer func(d time.Duration) { routerDeafGrace = d }(routerDeafGrace)
	routerDeafGrace = 2 * time.Second

	sess := &Session{
		ip:         deaf.Host,
		routerPort: deaf.Port,
		logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	start := time.Now()
	err := sess.awaitRouterAwake(context.Background())
	if !errors.Is(err, ErrRouterUnresponsive) {
		t.Fatalf("got %v, want ErrRouterUnresponsive — a deaf router reads as a missing route again", err)
	}
	if waited := time.Since(start); waited < routerDeafGrace {
		t.Errorf("gave up after %v, before the %v grace", waited, routerDeafGrace)
	}
}

// And it must not wait when the router is answering, which is every other call.
func TestAwaitRouterAwake_ReturnsWhileTheRouterAnswers(t *testing.T) {
	awake := fakeplc.NewRouter(t)
	awake.SetIdentity(fakeplc.Identity{NetID: [6]byte{5, 1, 2, 3, 1, 1}, Port: 10000})
	defer func(d time.Duration) { routerDeafGrace = d }(routerDeafGrace)
	routerDeafGrace = 5 * time.Second

	sess := &Session{
		ip:         awake.Host,
		routerPort: awake.Port,
		logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	start := time.Now()
	if err := sess.awaitRouterAwake(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if waited := time.Since(start); waited > routerAwakePoll {
		t.Errorf("waited %v on a router that was answering", waited)
	}
}

func TestRouteManager_ShouldSkip(t *testing.T) {
	tests := []struct {
		name             string
		routeName        string
		skipRegistration bool
		want             bool
	}{
		{"empty name → skip", "", false, true},
		{"name set, no skip → register", "myroute", false, false},
		{"name set + explicit skip → skip", "myroute", true, true},
		{"empty name + explicit skip → skip", "", true, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := &routeManager{name: tc.routeName, skipRegistration: tc.skipRegistration}
			if got := r.shouldSkip(); got != tc.want {
				t.Errorf("shouldSkip() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestRedialDuringHandshake_FlagsDisconnectedAcrossTheGap.
//
// awaitRouteActive's redial used to leave tx.disconnected false, because only
// resetForRetry set it — so the flag that gates user RPCs said "connected" while
// there was no socket and no workers at all. Survivable while the gap was a single
// dial; with a deliberate backoff before the dial it is up to seconds.
//
// Note this is tx.disconnected, not the public IsDisconnected(): that one is
// FSM-based and was never wrong here, because a session in an activation window is
// Connecting or Reconnecting rather than Connected.
func TestRedialDuringHandshake_FlagsDisconnectedAcrossTheGap(t *testing.T) {
	srv := fakeplc.StartPLC(t)
	defer srv.Stop()

	sess := newDialableTestSession(t, srv.Host, srv.Port, 0)
	if err := sess.dialAndStart(); err != nil {
		t.Fatalf("dialAndStart: %v", err)
	}
	t.Cleanup(func() { sess.markClosed() })

	if sess.tx.Disconnected() {
		t.Fatal("tx.disconnected set immediately after a successful dial")
	}

	// Observe the flag from inside the gap. dialAndStart clears it again once the
	// new workers are up, so the only place it can be seen is during the teardown.
	var sawDisconnected bool
	var mu sync.Mutex
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 2000; i++ {
			if sess.tx.Disconnected() {
				mu.Lock()
				sawDisconnected = true
				mu.Unlock()
				return
			}
			time.Sleep(200 * time.Microsecond)
		}
	}()

	if err := sess.redialDuringHandshake(); err != nil {
		t.Fatalf("redialDuringHandshake: %v", err)
	}
	<-done

	mu.Lock()
	seen := sawDisconnected
	mu.Unlock()
	if !seen {
		t.Error("tx.disconnected was never true across a redial: the RPC gate says connected while there is no socket")
	}
	if sess.tx.Disconnected() {
		t.Error("still flagged disconnected after the redial completed")
	}
}

// TestRedialDuringHandshake_LeavesOndropDisarmed: publishWiredClient arms ondrop
// on every new Client, so a caller deliberately holding the transport during a
// handshake would get a window in which a PLC RST spawns exactly the rival
// Reconnect that the disarm exists to prevent. Both callers need this, which is
// why it lives in the helper rather than being repeated at each of them.
func TestRedialDuringHandshake_LeavesOndropDisarmed(t *testing.T) {
	srv := fakeplc.StartPLC(t)
	defer srv.Stop()

	sess := newDialableTestSession(t, srv.Host, srv.Port, 0)
	if err := sess.dialAndStart(); err != nil {
		t.Fatalf("dialAndStart: %v", err)
	}
	t.Cleanup(func() { sess.markClosed() })

	if err := sess.redialDuringHandshake(); err != nil {
		t.Fatalf("redialDuringHandshake: %v", err)
	}
	c := sess.client.Load()
	if c == nil {
		t.Fatal("no Client after the redial")
	}
	armed := c.OnDropArmed()
	if armed {
		t.Error("ondrop is armed on the new Client: a drop mid-handshake would spawn a rival Reconnect")
	}
	if !c.Handshaking() {
		t.Error("the new Client is not in a handshake region: expected transport faults would log at ERROR")
	}
}

// TestDialMu_SerialisesConcurrentRedials: two redials must not interleave, or two
// TCP connections to the same AMS router briefly coexist — the documented way to
// get evicted, since the router serves one TCP per host and closes the older
// (Beckhoff/ADS#49).
//
// Run this under -race: the failure it guards against is a torn teardown, not a
// wrong number.
func TestDialMu_SerialisesConcurrentRedials(t *testing.T) {
	srv := fakeplc.StartPLC(t)
	defer srv.Stop()

	sess := newDialableTestSession(t, srv.Host, srv.Port, 0)
	if err := sess.dialAndStart(); err != nil {
		t.Fatalf("dialAndStart: %v", err)
	}
	t.Cleanup(func() { sess.markClosed() })

	const goroutines = 4
	var wg sync.WaitGroup
	errs := make(chan error, goroutines)
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := sess.redialDuringHandshake(); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent redial failed: %v", err)
	}

	// One survivor, and it is the one sess.client points at.
	c := sess.client.Load()
	if c == nil {
		t.Fatal("no Client after concurrent redials")
	}
	if c.Err() != nil {
		t.Error("the surviving Client's context is already cancelled")
	}
}

// TestAwaitRouteActive_ConfigFallbackStillRunsWhileRedialling is the documented
// degradation, asserted rather than left to be discovered.
//
// The CONFIG second opinion (ReadState on the system service port, which stays up
// when the runtime port does not) runs before the redial decision on every
// iteration, so capping the redials does not remove it — it reduces how many
// chances a device gets to answer it. A device that both drops the socket AND sits
// in CONFIG is therefore likelier to be reported as "route not served" than as
// ErrRuntimeNotRunning. That trade is deliberate: the honest failure is bounded,
// the storm was not.
//
// What must remain true is that a device answering the system service gets the
// runtime-not-running verdict rather than the route one.
func TestAwaitRouteActive_ConfigFallbackStillRunsWhileRedialling(t *testing.T) {
	srv := fakeplc.StartPLC(t)
	defer srv.Stop()
	srv.SetADSState(ams.StateConfig)

	sess := activationTestSession(t, srv, 3*time.Second)

	// The runtime port answers nothing (the probe reads the symbol version there),
	// while the system service still reports CONFIG — the real shape of a PLC that
	// is up but not running.
	srv.OnRead(ams.GroupSymbolVersion, func(_, _, _ uint32) (ams.ReturnCode, []byte) {
		return ams.ReturnCodeDeviceError, nil
	})

	_, err := sess.awaitRouteActive(sess.currentLifecycleCtx)
	if err == nil {
		t.Fatal("awaitRouteActive reported success for a PLC whose runtime is not running")
	}
	if !errors.Is(err, ErrRuntimeNotRunning) {
		t.Errorf("error = %v, want ErrRuntimeNotRunning: the route was being served, the runtime was not", err)
	}
}

// TestNestedRedial_DoesNotDeadlock.
//
// dialMu is not reentrant, and the reconnect path reaches awaitRouteActive through
// ensureRoute: Reconnect tears down before its retry loop and dials inside it,
// while awaitRouteActive redials within that span. Holding dialMu across a
// reconnect iteration would therefore self-deadlock on the inner redial, which is
// why the lock's scope is the pair inside redialDuringHandshake and nothing wider.
//
// The failure mode is a hang, not a wrong answer, so this test is written around a
// timeout: without one it would report as a suite timeout in some other test.
func TestNestedRedial_DoesNotDeadlock(t *testing.T) {
	srv := fakeplc.StartPLC(t)
	defer srv.Stop()

	sess := activationTestSession(t, srv, time.Second)
	srv.OnRead(ams.GroupSymbolVersion, func(_, _, _ uint32) (ams.ReturnCode, []byte) {
		return ams.ReturnCodeNoErrors, []byte{4}
	})

	done := make(chan error, 1)
	go func() {
		// ensureRoute -> awaitRouteActive -> redialDuringHandshake is the nesting
		// that matters; ensureRoute with no force and no probe failures probes and
		// returns, so drive awaitRouteActive directly after a redial to put two
		// acquisitions of dialMu on the same goroutine's stack in sequence.
		if err := sess.redialDuringHandshake(); err != nil {
			done <- err
			return
		}
		_, err := sess.awaitRouteActive(sess.currentLifecycleCtx)
		done <- err
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("nested redial path failed: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("nested redial path deadlocked: dialMu is held across a call that acquires it again")
	}
}

// route_activation_test.go — awaitRouteActive's context handling.
//
// The bug these pin: awaitRouteActive redials via tearDownAndReset, which
// cancels lifecycle.ctx and installs a fresh one. A caller that passed
// lifecycle.ctx *by value* therefore had the loop cancel its own context on the
// first redial — every later probe born already dead, and the failure reported
// as caller cancellation. That is why the parameter is a func() context.Context
// re-read per attempt rather than a captured ctx.

// TestCurrentLifecycleCtx_TracksReplacement: the helper must see the context
// tearDownAndReset installs, not the one it replaced.
func TestCurrentLifecycleCtx_TracksReplacement(t *testing.T) {
	parent, cancelParent := context.WithCancel(context.Background())
	defer cancelParent()
	sessCtx, cancel := context.WithCancel(parent)
	sess := &Session{
		lifecycle: &sessionLifecycle{
			closedCh:  make(chan struct{}),
			parentCtx: parent,
			ctx:       sessCtx,
			shutdown:  cancel,
		},
		logger: slog.Default(),
	}

	before := sess.currentLifecycleCtx()
	if before.Err() != nil {
		t.Fatalf("lifecycle ctx already done: %v", before.Err())
	}

	// Replace it the way a redial does.
	sess.lifecycle.ctxMu.Lock()
	sess.lifecycle.shutdown()
	sess.lifecycle.ctx, sess.lifecycle.shutdown = context.WithCancel(parent)
	sess.lifecycle.ctxMu.Unlock()
	defer sess.lifecycle.shutdown()

	if before.Err() == nil {
		t.Error("the replaced context should be cancelled — a captured one is exactly the bug")
	}
	after := sess.currentLifecycleCtx()
	if after.Err() != nil {
		t.Errorf("currentLifecycleCtx returned a dead context: %v", after.Err())
	}
	if after == before {
		t.Error("currentLifecycleCtx returned the stale context")
	}
}

// TestAwaitRouteActive_SurvivesCtxReplacement is the regression proper: it runs
// the real awaitRouteActive against a stub PLC whose first probe fails, and
// replaces lifecycle.ctx mid-loop exactly as the redial's tearDownAndReset
// does. With the ctx supplied per attempt the second probe succeeds; with a
// captured ctx (the bug) it is born cancelled and the call fails.
func TestAwaitRouteActive_SurvivesCtxReplacement(t *testing.T) {
	srv := fakeplc.StartPLC(t)
	defer srv.Stop()

	sess, _ := newWiredTestSession(t, srv)
	sess.route = &routeManager{name: "go-ads-test", activationTimeout: 4 * time.Second}

	var probes atomic.Int32
	srv.OnRead(ams.GroupSymbolVersion, func(_, _, _ uint32) (ams.ReturnCode, []byte) {
		n := probes.Add(1)
		if n == 1 {
			// Fail the first probe, then replace lifecycle.ctx the way a redial
			// would. A captured ctx is dead from here on.
			sess.lifecycle.ctxMu.Lock()
			sess.lifecycle.shutdown()
			sess.lifecycle.ctx, sess.lifecycle.shutdown = context.WithCancel(sess.lifecycle.parentCtx)
			sess.lifecycle.ctxMu.Unlock()
			return ams.ReturnCodeDeviceError, nil
		}
		return ams.ReturnCodeNoErrors, []byte{7}
	})

	version, err := sess.awaitRouteActive(sess.currentLifecycleCtx)
	if err != nil {
		t.Fatalf("awaitRouteActive after a mid-loop ctx replacement: %v", err)
	}
	if version != 7 {
		t.Errorf("symbol version = %d, want 7 (the winning probe's value must be returned so Connect need not re-read it)", version)
	}
	if got := probes.Load(); got < 2 {
		t.Errorf("probe attempts = %d, want >= 2 (the loop must retry after the first failure)", got)
	}
}

// TestAwaitRouteActive_RestoresClientState: the ondrop handler and the
// handshaking flag must both be put back however the call ends, or a later
// transport fault is either ignored or logged at the wrong level for the rest
// of the session.
func TestAwaitRouteActive_RestoresClientState(t *testing.T) {
	srv := fakeplc.StartPLC(t)
	defer srv.Stop()

	sess, c := newWiredTestSession(t, srv)
	sess.route = &routeManager{name: "go-ads-test", activationTimeout: time.Second}
	srv.OnRead(ams.GroupSymbolVersion, func(_, _, _ uint32) (ams.ReturnCode, []byte) {
		return ams.ReturnCodeNoErrors, []byte{3}
	})

	if _, err := sess.awaitRouteActive(sess.currentLifecycleCtx); err != nil {
		t.Fatalf("awaitRouteActive: %v", err)
	}
	if c.Handshaking() {
		t.Error("handshaking count non-zero after return — later real faults would log at Debug")
	}
	restored := c.OnDropArmed()
	if !restored {
		t.Error("ondrop not restored after return — a later drop would not trigger reconnect")
	}
}

// TestRouteActivationBudget covers the derived per-probe timeout, including the
// clamps that keep a shortened total coherent.
func TestRouteActivationBudget(t *testing.T) {
	tests := []struct {
		name       string
		configured time.Duration
		wantTotal  time.Duration
		wantProbe  time.Duration
	}{
		{name: "default", configured: 0, wantTotal: defaultRouteActivationTimeout, wantProbe: 2 * time.Second},
		{name: "short total clamps probe to floor", configured: time.Second, wantTotal: time.Second, wantProbe: minRouteActivationProbe},
		{name: "long total clamps probe to ceiling", configured: time.Minute, wantTotal: time.Minute, wantProbe: maxRouteActivationProbe},
		{name: "mid total divides by four", configured: 4 * time.Second, wantTotal: 4 * time.Second, wantProbe: time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sess := &Session{route: &routeManager{activationTimeout: tt.configured}}
			total, probe := sess.routeActivationBudget()
			if total != tt.wantTotal {
				t.Errorf("total = %v, want %v", total, tt.wantTotal)
			}
			if probe != tt.wantProbe {
				t.Errorf("probe = %v, want %v", probe, tt.wantProbe)
			}
			if probe > total {
				t.Errorf("probe %v exceeds total %v — one attempt would overrun the budget", probe, total)
			}
		})
	}
}

// --- redial storm bound (P1a) ---

// TestRedialBackoff pins the arithmetic. A loop that waits the wrong amount is
// invisible from outside — it just retries at the wrong rate — which is why this
// is a pure function with its own test rather than arithmetic inline in the loop.
func TestRedialBackoff(t *testing.T) {
	tests := []struct {
		name string
		n    int
		want time.Duration
	}{
		{name: "first redial waits the base", n: 0, want: 250 * time.Millisecond},
		{name: "second doubles", n: 1, want: 500 * time.Millisecond},
		{name: "third doubles again", n: 2, want: time.Second},
		{name: "fourth would be 2s, which is also the cap", n: 3, want: 2 * time.Second},
		{name: "beyond the cap stays at the cap", n: 9, want: 2 * time.Second},
		{name: "negative is treated as the first redial", n: -1, want: 250 * time.Millisecond},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := redialBackoff(tc.n); got != tc.want {
				t.Errorf("redialBackoff(%d) = %v, want %v", tc.n, got, tc.want)
			}
		})
	}
}

// activationTestSession builds a session that has really dialed srv through
// dialAndStart, so its Client came from publishWiredClient and shares
// lifecycle.ctx.
//
// newWiredTestSession cannot be used for anything that redials: its Client comes
// from Dial with a context of its own, so tearDownAndReset's wait for that
// Client's workers never returns (the helper says as much). That is also why no
// test reached awaitRouteActive's redial branch before this file.
func activationTestSession(t *testing.T, srv *fakeplc.PLC, budget time.Duration) *Session {
	t.Helper()
	sess := newDialableTestSession(t, srv.Host, srv.Port, 0)
	sess.route = &routeManager{name: "go-ads-test", activationTimeout: budget}
	if err := sess.dialAndStart(); err != nil {
		t.Fatalf("dialAndStart: %v", err)
	}
	t.Cleanup(func() { sess.markClosed() })
	return sess
}

// TestAwaitRouteActive_CapsTheRedialStorm is the regression this branch exists
// for.
//
// Before the cap, awaitRouteActive redialled on EVERY 250ms poll for the whole
// activation budget: measured in the field as 76 ephemeral ports in 11s, and ~40
// sockets per 10s window. Under the Beckhoff one-TCP-per-host rule each redial
// evicted its own predecessor, so the storm was self-defeating as well as
// expensive — it is what turned one drop into an unrecoverable deadloop.
//
// The stub drops the connection on every symbol-version probe, which is what a
// device does for a route it is not serving yet, so the loop takes the retryable
// path every time. What is asserted is the number of TCP connections the window
// costs.
func TestAwaitRouteActive_CapsTheRedialStorm(t *testing.T) {
	srv := fakeplc.StartPLC(t)
	defer srv.Stop()

	// 3s: long enough that the old behaviour would redial ~12 times (3s / 250ms).
	sess := activationTestSession(t, srv, 3*time.Second)

	// Sticky, unlike dropConnAfter, which disarms itself after one firing: the
	// probe has to keep failing at transport level or the loop never takes the
	// branch under test.
	srv.DropConnAlways(ams.CommandRead)

	if _, err := sess.awaitRouteActive(sess.currentLifecycleCtx); err == nil {
		t.Fatal("awaitRouteActive returned nil for a route the PLC never served")
	}

	// Total accepts, not a delta around the call: the stub's counter is incremented
	// by its accept goroutine, so it can lag the client-side dial that the fixture
	// already made, and a baseline read can miss it. The budget is therefore stated
	// as "the one connection this session started with, plus the cap".
	const want = maxRouteActivationRedials + 1
	if dials := srv.Accepts(); dials > want {
		t.Errorf("the session used %d TCP connections in total, want <= %d (1 initial + %d redials) — this is the redial storm",
			dials, want, maxRouteActivationRedials)
	}
}

// TestAwaitRouteActive_DoesNotSpinAfterTheBudget: with the redial budget spent
// and the transport gone, every further probe fails instantly. Continuing to poll
// would be a tight spin for the rest of the budget — so capping the redials
// WITHOUT this early exit makes the loop worse, not better.
//
// The budget is deliberately long and the assertion is on elapsed time: the loop
// must give up when there is nothing left to probe on, rather than sitting out the
// full window.
func TestAwaitRouteActive_DoesNotSpinAfterTheBudget(t *testing.T) {
	srv := fakeplc.StartPLC(t)
	defer srv.Stop()

	const budget = 10 * time.Second
	sess := activationTestSession(t, srv, budget)
	srv.DropConnAlways(ams.CommandRead)

	start := time.Now()
	if _, err := sess.awaitRouteActive(sess.currentLifecycleCtx); err == nil {
		t.Fatal("awaitRouteActive returned nil for a route the PLC never served")
	}
	elapsed := time.Since(start)

	// The bounded path costs the three backoffs (250+500+1000ms) plus a few probe
	// round-trips against a stub that answers by hanging up. Anything near the
	// budget means it kept polling a dead transport.
	if elapsed > budget/2 {
		t.Errorf("awaitRouteActive took %v of a %v budget; it is still polling after the redial budget was spent",
			elapsed, budget)
	}
}

// TestAwaitRouteActive_CloseDuringTheWaitOpensNoSocket.
//
// The wait before a redial has to watch three things, and Close is the one a
// two-arm select misses: on the Connect path the context passed in is Connect's
// OWN caller context, which Close does not cancel. Without closedCh in that
// select, Close returns, this loop then wakes, and dialAndStart opens a real TCP
// connection to the PLC — it dials before it re-checks isClosed — leaving a stray
// socket and a stray ephemeral port in the one code path whose whole purpose is to
// stop burning ephemeral ports.
func TestAwaitRouteActive_CloseDuringTheWaitOpensNoSocket(t *testing.T) {
	srv := fakeplc.StartPLC(t)
	defer srv.Stop()

	sess := activationTestSession(t, srv, 5*time.Second)
	srv.DropConnAlways(ams.CommandRead)

	// A context of its own that is never cancelled: this is Connect's caller ctx,
	// the case where closedCh is the only signal that ever arrives.
	callerCtx, cancelCaller := context.WithCancel(context.Background())
	defer cancelCaller()

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = sess.awaitRouteActive(func() context.Context { return callerCtx })
	}()

	// Let it fail a probe and enter the backoff wait, then close underneath it.
	time.Sleep(150 * time.Millisecond)
	sess.markClosed()
	afterClose := srv.Accepts()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("awaitRouteActive did not return after the session was closed — the wait is not watching closedCh")
	}

	if extra := srv.Accepts() - afterClose; extra > 0 {
		t.Errorf("%d TCP connection(s) opened after the session was closed", extra)
	}
}

// TestAwaitRouteActive_ProbesAgainAfterARedial: the reordered loop is
// close -> wait -> dial -> probe, and the probe at the end of that sequence is the
// point of the whole exercise. A reorder that left the window able to expire
// between the dial and the probe would turn every recoverable activation into a
// timeout, and would do it silently — the socket count would look right.
//
// The stub refuses at transport level until the route "comes live", then answers.
func TestAwaitRouteActive_ProbesAgainAfterARedial(t *testing.T) {
	srv := fakeplc.StartPLC(t)
	defer srv.Stop()

	sess := activationTestSession(t, srv, 5*time.Second)
	srv.OnRead(ams.GroupSymbolVersion, func(_, _, _ uint32) (ams.ReturnCode, []byte) {
		return ams.ReturnCodeNoErrors, []byte{9}
	})
	srv.DropConnAlways(ams.CommandRead)

	// Let the loop take the retryable path at least once, then start serving.
	go func() {
		time.Sleep(300 * time.Millisecond)
		srv.StopDroppingConn(ams.CommandRead)
	}()

	version, err := sess.awaitRouteActive(sess.currentLifecycleCtx)
	if err != nil {
		t.Fatalf("awaitRouteActive did not recover once the route was served: %v", err)
	}
	if version != 9 {
		t.Errorf("symbol version = %d, want 9 — the winning probe's value must be returned", version)
	}
	if srv.DroppingAlways(ams.CommandRead) {
		t.Fatal("the stub was still refusing; this test proved nothing")
	}
}

// router_port_test.go — Endpoint.RouterPort.
//
// A PLC behind NAT is reached on forwarded ports, and NAT maps one external
// port per internal port: the forwarded UDP port is a different number than the
// forwarded TCP port and cannot be derived from it. So the router port has to be
// independently settable, and the UDP calls (route registration, identify) have
// to use it rather than the protocol constant.

// TestNewSession_RouterPortDefaultsToProtocolPort: leaving it unset keeps the
// TwinCAT default, so nothing changes for a directly-reachable PLC.
func TestNewSession_RouterPortDefaultsToProtocolPort(t *testing.T) {
	sess, err := NewSession(context.Background(), Endpoint{
		Host:   "127.0.0.1",
		Target: ams.Address{NetID: [6]byte{5, 1, 2, 3, 1, 1}, Port: 851},
	}, WithTargetCheck(TargetCheckOff))
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	t.Cleanup(func() { sess.Close() })
	if got := sess.effectiveRouterPort(); got != router.DefaultPort {
		t.Errorf("router port = %d, want the protocol default %d", got, router.DefaultPort)
	}
	if sess.port != 48898 {
		t.Errorf("TCP port = %d, want the protocol default 48898", sess.port)
	}
}

// TestNewSession_RouterPortIndependentOfTCPPort is the NAT shape: two unrelated
// forwarded numbers, neither derived from the other.
func TestNewSession_RouterPortIndependentOfTCPPort(t *testing.T) {
	sess, err := NewSession(context.Background(), Endpoint{
		Host:       "127.0.0.1",
		Port:       5534, // external TCP -> 48898 on the PLC
		RouterPort: 6499, // external UDP -> 48899 on the PLC
		Target:     ams.Address{NetID: [6]byte{5, 1, 2, 3, 1, 1}, Port: 851},
	}, WithTargetCheck(TargetCheckOff))
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	t.Cleanup(func() { sess.Close() })
	if sess.port != 5534 {
		t.Errorf("TCP port = %d, want 5534", sess.port)
	}
	if got := sess.effectiveRouterPort(); got != 6499 {
		t.Errorf("router port = %d, want 6499", got)
	}
}

// TestSessionUsesRouterPortForIdentify proves the plumbing end to end for the
// UDP half: a responder on an arbitrary port is found only if the session
// actually probes RouterPort rather than the constant.
func TestSessionUsesRouterPortForIdentify(t *testing.T) {
	r := fakeplc.NewRouter(t)
	if r.Port == router.DefaultPort {
		// On the default port the test would pass even if the session ignored
		// RouterPort entirely.
		t.Fatal("the fake router landed on the protocol default port")
	}
	r.SetIdentity(fakeplc.Identity{
		NetID: [6]byte{5, 9, 8, 7, 1, 1}, Port: 10000,
		Tags: []fakeplc.Tag{fakeplc.NameTag("NAT-PLC"), fakeplc.VersionTag(3, 1, 4024)},
	})

	// No target AMS at all, so NewSession must discover it — over RouterPort.
	sess, err := NewSession(context.Background(), Endpoint{
		Host:       r.Host,
		Port:       5534, // nothing listens here; discovery is UDP-only
		RouterPort: r.Port,
	})
	if err != nil {
		t.Fatalf("NewSession with discovery over a forwarded router port: %v", err)
	}
	t.Cleanup(func() { sess.Close() })

	if got := sess.target.NetID.String(); got != "5.9.8.7.1.1" {
		t.Errorf("discovered NetID = %s, want 5.9.8.7.1.1", got)
	}
	if sess.target.Port != 851 {
		t.Errorf("discovered runtime port = %d, want 851 for the reported TwinCAT 3", sess.target.Port)
	}
}

// TestIsProbeRetryable_TransportLevel verifies the predicate identifies
// transport-level probe errors (RST/EOF/timeout/closed) as retryable, so
// ensureRouteOnConnect's redial-retry path engages on transient PLC slot
// conflicts instead of spuriously firing AddRoute.
func TestIsProbeRetryable_TransportLevel(t *testing.T) {
	connResetOp := &net.OpError{
		Op:  "read",
		Net: "tcp",
		Err: &net.OpError{Err: syscall.ECONNRESET},
	}
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil → not retryable", nil, false},
		{"ErrTransportClosed", ErrTransportClosed, true},
		{"wrapped ErrTransportClosed", fmt.Errorf("send: %w", ErrTransportClosed), true},
		{"bare io.EOF", io.EOF, true},
		{"wrapped io.EOF", fmt.Errorf("recv: %w", io.EOF), true},
		{"context.DeadlineExceeded → not retryable (caller-ctx)", context.DeadlineExceeded, false},
		{"wrapped DeadlineExceeded → not retryable", fmt.Errorf("probe: %w", context.DeadlineExceeded), false},
		{"bare ECONNRESET", syscall.ECONNRESET, true},
		{"wrapped net.OpError with ECONNRESET", fmt.Errorf("listen: %w", connResetOp), true},
		{"unrelated error → not retryable", errors.New("ads: ReturnCodeRouterNotInitialized"), false},
		{"context.Canceled → not retryable", context.Canceled, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isProbeRetryable(tc.err); got != tc.want {
				t.Errorf("isProbeRetryable(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
