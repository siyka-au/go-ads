package ads

import (
	"testing"
)

// newGapManager builds a notificationManager holding n handles with the baseline
// raised to match, which is the state a first connect leaves behind.
func newGapManager(n int) *notificationManager {
	m := &notificationManager{activeNotifications: map[uint32]activeNotification{}}
	for i := 1; i <= n; i++ {
		m.activeNotifications[uint32(0x100+i)] = activeNotification{}
	}
	m.raiseRegistered()
	return m
}

// A re-subscribe that restores fewer handles than the session had must leave the
// shortfall visible. The baseline used to be stored outright on every commit, so
// the last partial commit redefined healthy as the smaller set: want == have, no
// gap, and nothing ever retried the symbols that did not come back.
func TestRegisteredBaseline_PartialResubscribeLeavesAGap(t *testing.T) {
	m := newGapManager(40)
	if want, have := m.subscriptionGap(); want != 40 || have != 40 {
		t.Fatalf("first connect baseline: want=%d have=%d, expected 40/40", want, have)
	}

	// A reconnect that gets only 12 of them back.
	m.lock.Lock()
	m.activeNotifications = map[uint32]activeNotification{}
	for i := 1; i <= 12; i++ {
		m.activeNotifications[uint32(0x200+i)] = activeNotification{}
		m.raiseRegistered() // as each commit lands
	}
	m.lock.Unlock()

	want, have := m.subscriptionGap()
	if want != 40 || have != 12 {
		t.Fatalf("want=%d have=%d, expected 40/12 — a partial re-subscribe redefined what healthy means, "+
			"so the 28 symbols that never came back leave no gap and nothing retries them", want, have)
	}
}

// The baseline must still rise when the caller genuinely subscribes more.
func TestRegisteredBaseline_RisesOnNewSubscriptions(t *testing.T) {
	m := newGapManager(2)
	m.lock.Lock()
	m.activeNotifications[0x999] = activeNotification{}
	m.raiseRegistered()
	m.lock.Unlock()
	if want, have := m.subscriptionGap(); want != 3 || have != 3 {
		t.Errorf("want=%d have=%d, expected 3/3: a new subscribe must raise the baseline", want, have)
	}
}

// Symbols the PLC no longer has must come off the baseline, or the gap check
// chases handles that can never come back for the life of the session.
func TestRegisteredBaseline_LowersWhenSymbolsAreGone(t *testing.T) {
	m := newGapManager(40)
	m.lock.Lock()
	m.lowerRegisteredTo(35) // filterValidPending dropped 5
	m.activeNotifications = map[uint32]activeNotification{}
	for i := 1; i <= 35; i++ {
		m.activeNotifications[uint32(0x300+i)] = activeNotification{}
		m.raiseRegistered()
	}
	m.lock.Unlock()
	if want, have := m.subscriptionGap(); want != 35 || have != 35 {
		t.Errorf("want=%d have=%d, expected 35/35: symbols gone from the PLC must not leave a permanent gap", want, have)
	}
	// And lowering never raises.
	m.lock.Lock()
	m.lowerRegisteredTo(99)
	m.lock.Unlock()
	if want, _ := m.subscriptionGap(); want != 35 {
		t.Errorf("lowerRegisteredTo raised the baseline to %d", want)
	}
}

// TestRestoreConfigs_KeepsWhatArrivedDuringTheAttempt: putting a snapshot back
// must not discard newer configs.
//
// Heartbeat recovery snapshots the caller's intent, tries to re-subscribe, and
// restores the snapshot on failure. With resetConfigs that restore was an
// overwrite, so a subscribe made while the attempt was in flight lost its config
// while its PLC handle stayed registered: never resubscribed after a reconnect, and
// subscribing that symbol again would duplicate the registration. Hardware showed
// the wider version of this race when power-cycling 192.168.3.70 with 40 symbols —
// heartbeat recovery and the reconnect loop both resubscribing, "bound
// notifications = 24, want 40".
func TestRestoreConfigs_KeepsWhatArrivedDuringTheAttempt(t *testing.T) {
	mgr := newTestNotificationManager()

	// What recovery snapshotted before it started.
	snapshot := []pendingNotification{
		{Config: NotificationConfig{Symbol: "MAIN.a"}},
		{Config: NotificationConfig{Symbol: "MAIN.b"}, resubscribeAttempts: 2},
	}
	// What the attempt left on file: its own configs cleared, plus one the user
	// subscribed while it was running.
	mgr.resetConfigs(nil)
	mgr.addConfig(NotificationConfig{Symbol: "MAIN.late"})

	mgr.restoreConfigs(snapshot)

	if !mgr.hasConfig("MAIN.late") {
		t.Error("the config that arrived during the attempt was discarded by the restore: its handle stays registered on the PLC " +
			"with nothing to resubscribe it")
	}
	for _, name := range []string{"MAIN.a", "MAIN.b"} {
		if !mgr.hasConfig(name) {
			t.Errorf("%s was not restored", name)
		}
	}
	if len(mgr.pending) != 3 {
		t.Errorf("pending = %d, want 3 (two restored plus the newer one)", len(mgr.pending))
	}
	// The retry counter has to survive, or a symbol that has already failed twice
	// gets a fresh budget on every recovery and never drops out.
	for _, entry := range mgr.pending {
		if entry.Config.Symbol == "MAIN.b" && entry.resubscribeAttempts != 2 {
			t.Errorf("MAIN.b restored with resubscribeAttempts = %d, want 2", entry.resubscribeAttempts)
		}
	}
	// And a restore must not duplicate what is already on file.
	mgr.restoreConfigs(snapshot)
	if len(mgr.pending) != 3 {
		t.Errorf("pending = %d after restoring the same snapshot twice, want 3", len(mgr.pending))
	}
}
