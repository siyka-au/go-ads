package ads

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/siyka-au/go-ads/v3/internal/fakeplc"

	"github.com/siyka-au/go-ads/v3/ams"
)

// TestRuntimeState_RefusesSymbolWorkOutsideRun: when the system service says the
// runtime is not in RUN, symbol and subscription calls must refuse and say why.
//
// Measured on TC3.1.4024 in CONFIG: every request to the runtime port 851 came back
// with AMS ErrorCode 6 (target port not found), while port 10000 answered
// ADSState=15. The library discarded the AMS error and parsed the response body
// anyway, so a subscribe failed with "0xF008: unknown error code" — an index group
// formatted as a return code. Asking the system service turns that into a fact.
func TestRuntimeState_RefusesSymbolWorkOutsideRun(t *testing.T) {
	srv := fakeplc.StartPLC(t)
	defer srv.Stop()
	srv.OnAddDeviceNotification(func(_ fakeplc.AddNotifRequest) fakeplc.AddNotifResponse {
		return fakeplc.AddNotifResponse{Handle: 0x1234}
	})

	sess, c := newWiredTestSession(t, srv, WithoutNotificationHeartbeat())
	c.SetNotificationHandler(sess.handleNotification)
	preSeedTypedSymbol(sess, "MAIN.cfg", 0xFC00)
	preSeedTypedSymbol(sess, "MAIN.cfg2", 0xFC01)
	preSeedTypedSymbol(sess, "MAIN.cfg3", 0xFC02)
	ch := make(chan *Update, 4)

	// No reading yet: the gate must permit, or every device that does not serve the
	// system service port would stop working.
	if _, err := sess.Subscribe(context.Background(), "MAIN.cfg", 0, 0,
		ams.TransModeServerOnChange, ch); err != nil {
		t.Fatalf("subscribe refused with no runtime-state reading: %v", err)
	}

	// Now the system service reports CONFIG.
	sess.recordRuntimeState(ams.StateConfig)
	_, err := sess.Subscribe(context.Background(), "MAIN.cfg2", 0, 0,
		ams.TransModeServerOnChange, ch)
	if err == nil {
		t.Error("subscribe succeeded although the runtime is in CONFIG: the runtime port does not exist in that state, so this " +
			"can only fail later and obscurely")
	}
	if !errors.Is(err, ErrRuntimeNotRunning) {
		t.Errorf("error = %v, want one wrapping ErrRuntimeNotRunning so callers can branch on it", err)
	}
	if err != nil && !strings.Contains(err.Error(), "15") {
		t.Errorf("error %q does not name the state; the operator needs to know it is CONFIG, not just that something failed", err)
	}
	if lerr := sess.LoadSymbols(context.Background()); !errors.Is(lerr, ErrRuntimeNotRunning) {
		t.Errorf("LoadSymbols error = %v, want ErrRuntimeNotRunning", lerr)
	}

	// Back to RUN: work is allowed again without rebuilding anything.
	sess.recordRuntimeState(ams.StateRun)
	if _, err := sess.Subscribe(context.Background(), "MAIN.cfg3", 0, 0,
		ams.TransModeServerOnChange, ch); err != nil {
		t.Errorf("subscribe still refused after the runtime returned to RUN: %v", err)
	}
}

// TestRuntimeState_PollReportsTheState: the state has to be discovered by the
// session, not only by a caller who asks.
//
// It is a poll on purpose. There is nothing to subscribe to that survives the
// transition being watched: in CONFIG the runtime port that would carry a
// notification does not exist. One small request per heartbeat interval to a port
// that is up whenever the device is.
func TestRuntimeState_PollReportsTheState(t *testing.T) {
	srv := fakeplc.StartPLC(t)
	defer srv.Stop()
	srv.SetADSState(ams.StateConfig)

	// WithRuntimeStateWatch, not WithNotificationHeartbeat: the state poll used to
	// run at the heartbeat cycle, so this test tuned the heartbeat purely to make
	// the poll fast. The two are independent now (defaultStateWatchInterval), and
	// this test wants a fast POLL.
	sess, _ := newWiredTestSession(t, srv,
		WithNotificationHeartbeat(100*time.Millisecond, 3),
		WithRuntimeStateWatch(100*time.Millisecond))
	if state, known := sess.knownRuntimeState(); known {
		t.Fatalf("state already known before polling: %v", state)
	}
	sess.startRuntimeStateWatch()

	deadline := time.Now().Add(3 * time.Second)
	for {
		if state, known := sess.knownRuntimeState(); known {
			if state != ams.StateConfig {
				t.Errorf("polled state = %v, want CONFIG", state)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the runtime state was never polled: the session cannot tell CONFIG from a broken device")
		}
		time.Sleep(20 * time.Millisecond)
	}

	// And it must notice the way back.
	srv.SetADSState(ams.StateRun)
	deadline = time.Now().Add(3 * time.Second)
	for {
		if state, _ := sess.knownRuntimeState(); state == ams.StateRun {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the poll never saw the return to RUN, so the session would refuse subscriptions forever")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestRuntimeState_RefusesOnlyMeasuredStates: the gate must refuse only where a
// runtime port provably does not serve.
//
// An earlier version refused on anything that was not RUN, then on a list that
// included STOP and SHUTDOWN by inference. Only CONFIG and RECONFIG are measured
// (TC3.1.4024 in CONFIG reports 15 and answers AMS ErrorCode 6 for every request to
// a runtime port). STOP was seen only as a ~4s way-point during a CONFIG -> RUN
// switch, so whether a device can idle there while serving is unknown — and
// refusing every subscribe on such a device, with no PLC error to explain it, is
// worse than attempting the call.
func TestRuntimeState_RefusesOnlyMeasuredStates(t *testing.T) {
	refuse := []ams.State{ams.StateConfig, ams.StateReconfig}
	permit := []ams.State{ams.StateRun, ams.StateStop, ams.StateShutdown, ams.StateIdle, ams.StateStart, ams.StateInvalid, ams.State(99)}

	for _, state := range refuse {
		if !runtimeDefinitelyNotServing(state) {
			t.Errorf("state %d should be refused: it is measured to have no serving runtime port", uint16(state))
		}
	}
	for _, state := range permit {
		if runtimeDefinitelyNotServing(state) {
			t.Errorf("state %d is refused on inference rather than evidence; attempting the call and letting the PLC answer is "+
				"the safer default", uint16(state))
		}
	}
}

// TestRuntimeState_ReadingExpires: a state reading must not outlive its usefulness.
//
// The watch gives up after a run of failed polls, and before this a session that had
// seen CONFIG then kept that verdict forever — refusing every symbol and subscribe
// call for the rest of its life with nothing left to notice the runtime returning.
// Failing OPEN is deliberate: the worst case is the behaviour that predates the
// gate.
func TestRuntimeState_ReadingExpires(t *testing.T) {
	srv := fakeplc.StartPLC(t)
	defer srv.Stop()
	sess, _ := newWiredTestSession(t, srv, WithoutNotificationHeartbeat())

	sess.recordRuntimeState(ams.StateConfig)
	if _, known := sess.knownRuntimeState(); !known {
		t.Fatal("a fresh reading is not known")
	}
	if err := sess.requireRunningRuntime("probe"); err == nil {
		t.Fatal("a fresh CONFIG reading must refuse")
	}

	// Age it past the TTL, as an abandoned poll would.
	sess.runtimeStateNs.Store(time.Now().Add(-2 * runtimeStateTTL).UnixNano())
	if _, known := sess.knownRuntimeState(); known {
		t.Error("a stale reading is still reported as known: nothing refreshes it once the watch has given up, so the gates " +
			"would refuse for the life of the session")
	}
	if err := sess.requireRunningRuntime("probe"); err != nil {
		t.Errorf("a stale reading still refuses: %v", err)
	}
}

// TestRuntimeStateWatch_DefaultIsIndependentOfTheHeartbeat.
//
// The poll used to run at heartbeatCycle(), so WithNotificationHeartbeat(30s, ...)
// silently made the state poll 30s too — the gate reporting "the runtime is in
// CONFIG" went stale for half a minute because an unrelated knob moved. This is
// the only assertion on the default, since the poller's own test now pins an
// explicit interval.
func TestRuntimeStateWatch_DefaultIsIndependentOfTheHeartbeat(t *testing.T) {
	var sess Session
	if got := sess.stateWatchCycle(); got != defaultStateWatchInterval {
		t.Errorf("default state watch cycle = %v, want %v", got, defaultStateWatchInterval)
	}
	WithNotificationHeartbeat(30*time.Second, 3)(&sess)
	if got := sess.stateWatchCycle(); got != defaultStateWatchInterval {
		t.Errorf("state watch cycle = %v after a 30s heartbeat, want %v — the coupling is back",
			got, defaultStateWatchInterval)
	}
	WithRuntimeStateWatch(750 * time.Millisecond)(&sess)
	if got := sess.stateWatchCycle(); got != 750*time.Millisecond {
		t.Errorf("state watch cycle = %v after WithRuntimeStateWatch, want 750ms", got)
	}
}

// TestWithoutRuntimeStateWatch_StartsNoPollerAndKeepsTheOnce: the disabled check
// sits outside stateOnce.Do, so turning the watch off does not consume the Once —
// and with no reading the gates fall back to permitting, which is the behaviour
// that predates the watch.
func TestWithoutRuntimeStateWatch_StartsNoPollerAndKeepsTheOnce(t *testing.T) {
	srv := fakeplc.StartPLC(t)
	defer srv.Stop()
	srv.SetADSState(ams.StateConfig)

	sess, _ := newWiredTestSession(t, srv, WithoutRuntimeStateWatch())
	sess.startRuntimeStateWatch()

	time.Sleep(200 * time.Millisecond)
	if state, known := sess.knownRuntimeState(); known {
		t.Errorf("runtime state became known (%v) although the watch is disabled", state)
	}

	// The Once must still be unused: a session that had the watch disabled and
	// later enabled it would otherwise never get a poller.
	sess.stateWatchDisabled = false
	// An explicit fast interval: the default is 5s, so a poller started here would
	// not have ticked inside this test's deadline whether the Once was consumed or
	// not — which would make the assertion below vacuous.
	sess.stateWatchInterval = 100 * time.Millisecond
	sess.startRuntimeStateWatch()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, known := sess.knownRuntimeState(); known {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Error("no poller started after re-enabling the watch: stateOnce was consumed by the disabled path")
}
