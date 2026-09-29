package ads

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/siyka-au/go-ads/v3/ams"
	"github.com/siyka-au/go-ads/v3/internal/fakeplc"
)

// TestConnect_VerifiesTheLinkAnswersEvenWithoutRouteRegistration: Connect must
// never report success against a PLC that accepts the socket and answers nothing.
//
// The liveness check used to live inside the route-registration branch, so a
// caller with a pre-registered route (no WithRoute, or WithSkipRouteRegistration)
// got no check at all. Measured against a TC/RTOS device in this state: Connect
// returned nil in 5.01s, IsClosed() false, FSM Connected — and every subsequent
// request timed out. That is the invisible-stuck state a consumer polling
// IsClosed() cannot detect.
func TestConnect_VerifiesTheLinkAnswersEvenWithoutRouteRegistration(t *testing.T) {
	srv := fakeplc.StartPLC(t)
	defer srv.Stop()

	// Accepts the connection; answers nothing.
	srv.DelayBefore(ams.CommandRead, uint32(ams.GroupSymbolVersion), time.Hour)

	sess, err := NewSession(context.Background(),
		Endpoint{Host: srv.Host, Port: srv.Port, Target: ams.Address{NetID: [6]byte{5, 1, 2, 3, 1, 1}, Port: 851}},
		WithRequestTimeout(300*time.Millisecond),
		WithTargetCheck(TargetCheckOff),
		WithAutoReconnect(false),
		// No WithRoute: the route is assumed to exist already, so registration —
		// and with it the old liveness check — is skipped.
		//
		// WithoutAmsPeerFallback matters here: without it this test silently bound
		// 0.0.0.0:48898, the host's real AMS port. On a machine running TwinCAT the
		// bind fails and the test still passes (it only asserts err != nil), so the
		// collision is invisible. It also made the fallback part of what this test
		// exercised, which belongs in the peer-route tests.
		WithoutAmsPeerFallback(),
	)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	t.Cleanup(func() { sess.Close() })

	err = sess.Connect(context.Background())
	if err == nil {
		t.Fatal("Connect reported success against a PLC that answered nothing; the session is deaf but looks healthy")
	}
	// The error has to name what was tried, or the operator cannot tell a deaf PLC
	// from a misconfigured one. Asserting only err != nil let the whole liveness
	// check — the second-opinion ReadState and the peer fallback — be deleted with
	// this test still green.
	for _, want := range []string{"GetSymbolVersion", "ReadState"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %s; it should say which services were tried", err, want)
		}
	}
	if state := sess.lifecycle.state.load(); state == SessionStateConnected {
		t.Error("session left Connected after a failed connect")
	}
	sess.peerMu.Lock()
	ln := sess.peerLn
	sess.peerMu.Unlock()
	if ln != nil {
		t.Error("a listener was bound although the fallback is disabled")
	}
}

// TestConnect_FailedRouteActivationLeavesNothingRunning: a Connect that reports
// failure must not leave a live transport behind.
//
// Both route-stage error returns in Connect skipped tearDownAndReset, unlike every
// sibling path in the same function. The socket stayed open, the Client's workers
// (listen, transmit, recvWorkers) kept running, and the deferred re-arm restored
// onDrop — so when the PLC eventually closed that abandoned socket, the session
// silently began reconnecting a Connect the caller had been told failed. If it
// then succeeded, the caller holds a Session it believes is dead and never reads
// from, while a second Client publishes a second transmitWorker onto the same
// shared tx.sendChannel and frames go to whichever socket wins.
func TestConnect_FailedRouteActivationLeavesNothingRunning(t *testing.T) {
	srv := fakeplc.StartPLC(t)
	defer srv.Stop()
	router := fakeplc.NewRouter(t)

	// The router ACKs the registration, but the device never serves the route:
	// silence, which is what awaitRouteActive is there to catch.
	srv.OnRead(ams.GroupSymbolVersion, func(_, _, _ uint32) (ams.ReturnCode, []byte) {
		time.Sleep(400 * time.Millisecond)
		return ams.ReturnCodeNoErrors, []byte{3}
	})

	sess, err := NewSession(context.Background(),
		Endpoint{Host: srv.Host, Port: srv.Port, Target: ams.Address{NetID: [6]byte{5, 1, 2, 3, 1, 1}, Port: 851}},
		WithRequestTimeout(150*time.Millisecond),
		WithTargetCheck(TargetCheckOff),
		WithRoute("go-ads-test", "Administrator", "1"),
		WithHostIP("127.0.0.1"),
		WithRouteActivationTimeout(600*time.Millisecond),
		WithoutAmsPeerFallback(),
		WithBackoff(fastBackoff()),
	)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	sess.routerPort = router.Port
	t.Cleanup(func() { sess.Close() })

	if err := sess.Connect(context.Background()); err == nil {
		t.Fatal("Connect reported success although the route never activated")
	}

	// The Client that was published during the attempt must be finished with.
	if c := sess.client.Load(); c != nil && c.Err() == nil {
		t.Error("the Client published during the failed Connect is still live: its workers are running on an open socket")
	}

	// A retry is explicitly allowed from here (the rollback leaves Disconnected,
	// and Disconnected -> Connecting is a legal edge). It must not end up with two
	// Clients on the shared transport: the first one's transmitWorker would still
	// be competing for tx.sendChannel and writing to a socket that is gone.
	before := srv.Accepts()
	_ = sess.Connect(context.Background())
	if got := srv.Accepts() - before; got != 1 {
		t.Errorf("the retry produced %d new connections, want 1: the abandoned transport was never closed, "+
			"so the route stage redialed on top of it", got)
	}
}

// TestTearDownAndReset_ReleasesWaitersImmediately: a session-initiated teardown must
// fail in-flight requests with the reason, not let them sit out their timeout.
//
// readFrames returns on ctx.Done() without calling callOnDrop, so nothing else
// closed the Client's `dropped` channel on this path — the very signal `dropped`
// exists to provide.
func TestTearDownAndReset_ReleasesWaitersImmediately(t *testing.T) {
	srv := fakeplc.StartPLC(t)
	defer srv.Stop()
	// Answers nothing, so the request is still in flight when teardown happens.
	srv.DelayBefore(ams.CommandRead, uint32(ams.GroupSymbolVersion), time.Hour)

	sess := newDialableTestSession(t, srv.Host, srv.Port, 1)
	sess.requestTimeout = 30 * time.Second // far longer than this test may take
	if err := sess.dialAndStart(); err != nil {
		t.Fatalf("dialAndStart: %v", err)
	}
	c := sess.client.Load()

	errCh := make(chan error, 1)
	go func() {
		_, err := c.GetSymbolVersion(context.Background())
		errCh <- err
	}()
	time.Sleep(200 * time.Millisecond) // let the request reach the wire

	go sess.tearDownAndReset()

	select {
	case err := <-errCh:
		if !errors.Is(err, ErrTransportClosed) {
			t.Logf("in-flight request returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Error("an in-flight request was not released by the teardown: it will wait out its full request timeout instead of " +
			"learning the transport is gone")
	}
}

// TestConnect_ResetDuringLivenessProbeSpawnsNoRivalReconnect: a drop that lands
// while Connect is probing the link must not spawn a Reconnect that rebuilds the
// transport under Connect - and the consumer must still be told about the drop.
//
// Connect owns sess.tx / sess.client from the first publishWiredClient to the
// Connected transition, but ondrop is armed across most of that: the local
// handshake, the GetSymbolVersion/ReadState liveness probe, and the runtime-state
// read. (The route stage is not exposed - it disarms - and its two helpers re-arm
// from a defer in the MIDDLE of Connect, which is how the hole got there.) A RST
// in the armed window spawned a Reconnect that ran tearDownAndReset against the
// same tx while Connect was still building it: each cancelled the other's context
// and closed the other's socket, and after a Connect the caller was told had
// failed the session sat Connected on a second connection with IsClosed() false -
// a leaked socket, the Client's workers, the runtime watcher and an unbounded
// reconnect loop, per failed Connect. Unrecoverable in place, too: a retry gets
// "Connect already in progress".
//
// The other half of the invariant, and the reason the fix is not just "suppress
// the spawn": suppressing the rival goroutine must not suppress the drop SIGNAL.
// WithOnDisconnect is the only channel a consumer that neither polls IsClosed()
// nor inspects the FSM has, and the callback fires once - on the CAS's first
// detector - so a suppression placed above it loses it permanently rather than
// delaying it. Hence onDisconnect == 1 here.
func TestConnect_ResetDuringLivenessProbeSpawnsNoRivalReconnect(t *testing.T) {
	srv := fakeplc.StartPLC(t)
	defer srv.Stop()
	// A rival reconnect has to be able to SUCCEED, or "no second dial" and "a
	// second dial that failed" would look the same from the outside.
	srv.OnRead(ams.GroupSymbolVersion, func(_, _, _ uint32) (ams.ReturnCode, []byte) {
		return ams.ReturnCodeNoErrors, []byte{7}
	})
	// Close without answering the very GetSymbolVersion the liveness probe issues:
	// a real reset inside the armed window, not a timeout. Disarms afterwards, so
	// the reconnect this used to spawn gets a working link.
	srv.DropConnAfter(ams.CommandRead, 1)

	// "attempting reconnect" is logged before the rival's tearDownAndReset, so
	// gating on it parks the rival at the start of the damage instead of leaving
	// the interleaving to chance.
	gate := newGateOnLog("attempting reconnect", "reconnect successful")

	var disconnects atomic.Int64
	sess, err := NewSession(context.Background(),
		Endpoint{Host: srv.Host, Port: srv.Port, Target: ams.Address{NetID: [6]byte{5, 1, 2, 3, 1, 1}, Port: 851}},
		WithRequestTimeout(500*time.Millisecond),
		WithTargetCheck(TargetCheckOff),
		// No WithRoute: that is what routes Connect through the armed liveness
		// block rather than the disarmed route stage.
		WithoutAmsPeerFallback(),
		WithBackoff(fastBackoff()),
		WithOnDisconnect(func() { disconnects.Add(1) }),
		WithLogger(slog.New(gate)),
	)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	t.Cleanup(func() { sess.Close() })

	if cerr := sess.Connect(context.Background()); cerr == nil {
		t.Fatal("Connect reported success although the link was reset during the liveness probe")
	}

	// If a rival was spawned it is parked at the gate. Let it run to completion
	// before asserting, so what gets asserted on is the finished leak rather than
	// a half-built one.
	spawned := false
	select {
	case <-gate.w.reached:
		spawned = true
		close(gate.w.release)
		select {
		case <-gate.w.signalled:
		case <-time.After(10 * time.Second):
			t.Fatal("a rival reconnect was spawned but never finished; cannot assert on a half-built transport")
		}
	case <-time.After(1500 * time.Millisecond):
	}
	// Reported, not asserted: the invariants below are what must hold, and the
	// test should not encode the buggy mechanism.
	t.Logf("rival reconnect spawned: %v", spawned)

	if state := sess.lifecycle.state.load(); state == SessionStateConnected {
		t.Errorf("state = %v after a Connect that returned an error: a rival Reconnect finished the job underneath it", state)
	}
	if got := srv.Accepts(); got != 1 {
		t.Errorf("the server accepted %d connections, want 1: a second one was dialled while Connect still owned the transport", got)
	}
	if c := sess.client.Load(); c != nil && c.Err() == nil {
		t.Error("the Client published during the failed Connect is still live: its workers are running on an open socket")
	}

	// The drop signal survives the suppression. Polled, because the callback runs
	// in its own goroutine.
	deadline := time.Now().Add(2 * time.Second)
	for disconnects.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("WithOnDisconnect never fired for a drop during Connect: the consumer is told the Connect failed and nothing else, " +
				"so a suppression placed above the callback dispatch has silently taken away its only drop signal")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := disconnects.Load(); got != 1 {
		t.Errorf("onDisconnect fired %d times, want 1", got)
	}

	// And the session is still retryable, on one new connection - the contract
	// Connect's own doc states. Connect never cleared tx.disconnected, which only
	// ever worked because the suppressed Reconnect's dialAndStart did it: with the
	// spawn gone, the stale flag failed every request on the retry's perfectly
	// good socket.
	before := srv.Accepts()
	if rerr := sess.Connect(context.Background()); rerr != nil {
		t.Fatalf("retry after the failed Connect returned %v, want nil: the session is neither reconnectable nor retryable", rerr)
	}
	if got := srv.Accepts() - before; got != 1 {
		t.Errorf("the retry produced %d new connections, want 1", got)
	}
}

// TestConnect_ResetNamesTheRouteAsALikelyCause: the error a caller RECEIVES has to
// carry the diagnosis, not just the log line.
//
// Measured against 192.168.3.118 with its route table wiped: discovery resolved
// the target correctly, Connect then failed, and the caller got
// "transport dropped during connect: ... client transport closed" — nothing about
// routes. The useful sentence existed only in a log record at ERROR, so a consumer
// that surfaces err (which is what the Benthos plugin does) was back to guessing at
// exactly the failure this library exists to make legible.
func TestConnect_ResetNamesTheRouteAsALikelyCause(t *testing.T) {
	srv := fakeplc.StartPLC(t)
	defer srv.Stop()
	// Accept the TCP connection, then drop it on the first request — the wire
	// signature of a PLC with no route for our NetID.
	srv.DropConnAfter(ams.CommandRead, 1)

	sess := newDialableTestSession(t, srv.Host, srv.Port, 1)
	t.Cleanup(func() { _ = sess.Close() })
	sess.lifecycle.state.transitionTo(SessionStateDisconnected)

	err := sess.Connect(context.Background())
	if err == nil {
		t.Fatal("Connect succeeded against a stub that drops the first request")
	}
	msg := err.Error()
	for _, want := range []string{"route", "NetID"} {
		if !strings.Contains(msg, want) {
			t.Errorf("Connect error does not mention %q: %v\n"+
				"a caller that logs only this error cannot tell a missing route from a dead PLC", want, err)
		}
	}
}

// reconnect_storm_test.go — the P1b/P2b hardening from the 2026-08-27 field
// investigation: the publish window, the dialMu invariant, and the drop verdict.

// TestPublishWiredClient_PublishesBeforeStartingWorkers.
//
// The order used to be SetOnDrop -> startWorkers -> client.Store, on the
// reasoning that a concurrent reader must never see a half-built Client. But the
// workers ARE concurrent readers of sess.client: a drop landing between
// startWorkers and Store ran callOnDrop -> triggerReconnect -> tearDownAndReset,
// which loaded sess.client and therefore tore down the PREVIOUS Client —
// markDropped-ing and waiting the wrong one — while this Client's workers kept
// running with nobody waiting their WaitGroup and its transmitWorker sharing
// tx.sendChannel with the next dial's.
//
// Asserted structurally rather than by racing the window: by the time any worker
// exists, sess.client must already point at the Client those workers belong to.
func TestPublishWiredClient_PublishesBeforeStartingWorkers(t *testing.T) {
	srv := fakeplc.StartPLC(t)
	defer srv.Stop()

	sess := newDialableTestSession(t, srv.Host, srv.Port, 0)
	if err := sess.dialAndStart(); err != nil {
		t.Fatalf("dialAndStart: %v", err)
	}
	t.Cleanup(func() { sess.markClosed() })

	c := sess.client.Load()
	if c == nil {
		t.Fatal("dialAndStart returned with no Client published")
	}
	// The workers hold this Client's ctx and WaitGroup, so a teardown that loaded
	// anything else would wait the wrong ones.
	if got := sess.client.Load(); got != c {
		t.Errorf("sess.client changed after the dial: %p vs %p", got, c)
	}
	if c.Err() != nil {
		t.Error("the published Client has no live context: its workers cannot be stopped by a teardown")
	}
}
