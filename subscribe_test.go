package ads

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/siyka-au/go-ads/v3/ams"
	"github.com/siyka-au/go-ads/v3/internal/fakeplc"
	"github.com/siyka-au/go-ads/v3/internal/symtab"
	"github.com/siyka-au/go-ads/v3/internal/testlog"
)

// subscribe_test.go — Session.Subscribe(s) unit tests, including the
// subscribe-race regressions.
//
// Regression guarded here (shipped in v2.2.0, bisected on TC2 hardware
// 2026-08-20): the PLC-side notification handle exists the moment
// AddDeviceNotification returns, so the PLC can emit the first sample before
// activeNotifications carries the handle. Two failures followed from that:
//
//  1. The sample was dropped. Harmless for a cyclic tag (later samples land
//     on the normal path) but total loss for a static symbol, which emits
//     exactly once at subscribe.
//  2. The orphan reaper treated the handle as leaked and deleted the
//     subscription off the PLC, so the tag never streamed again. On TC2 —
//     which answers 0x0701 to the sum command, forcing one Add per symbol —
//     a 40-symbol batch takes far longer than the 100 ms race window, and
//     30 of 40 tags were reaped.

// TestSubscribeAll_ResultsCarryMetadataAndErr: a consumer labels its values from
// the result — no follow-up lookup per symbol — and checks one Err instead of
// pairing Skipped with a ReturnCode.
func TestSubscribeAll_ResultsCarryMetadataAndErr(t *testing.T) {
	srv := servingPLC(t)
	sess, c := newWiredTestSession(t, srv)
	c.SetNotificationHandler(sess.handleNotification)
	preSeedTypedSymbol(sess, "MAIN.a", 0x3001)

	ch := make(chan *Update, 8)
	results, err := sess.SubscribeAll(context.Background(), []NotificationConfig{
		{Symbol: "MAIN.a", Mode: ams.TransModeServerOnChange},
		{Symbol: "MAIN.a", Mode: ams.TransModeServerOnChange}, // duplicate within the batch
	}, ch)
	if err != nil {
		t.Fatalf("SubscribeAll: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("got %d results, want 2", len(results))
	}
	if results[0].Err != nil || results[0].Handle == 0 {
		t.Errorf("first entry = %+v, want a live handle", results[0])
	}
	if results[0].Symbol.DataType != "INT" || results[0].Symbol.Length != 2 {
		t.Errorf("first entry's metadata = %q/%d, want INT/2", results[0].Symbol.DataType, results[0].Symbol.Length)
	}
	if !errors.Is(results[1].Err, ErrNotificationDuplicate) || results[1].Handle != 0 {
		t.Errorf("duplicate entry = %+v, want ErrNotificationDuplicate and no handle", results[1])
	}
}

// TestUpdate_CarriesTheSubscribedSpelling: TC2 upper-cases names; the Update
// must come back under the name the caller subscribed with.
func TestUpdate_CarriesTheSubscribedSpelling(t *testing.T) {
	srv := servingPLC(t)
	sess, c := newWiredTestSession(t, srv)
	c.SetNotificationHandler(sess.handleNotification)
	preSeedTypedSymbol(sess, "MAIN.COUNTER", 0x3001) // the PLC's casing

	ch := make(chan *Update, 8)
	results, err := sess.SubscribeAll(context.Background(), []NotificationConfig{
		{Symbol: "Main.counter", Mode: ams.TransModeServerOnChange},
	}, ch)
	if err != nil || results[0].Err != nil {
		t.Fatalf("SubscribeAll: %v / %v", err, results[0].Err)
	}
	sess.handleNotification(context.Background(), results[0].Handle, 0, intSample(7))
	select {
	case u := <-ch:
		if u.Symbol != "Main.counter" {
			t.Errorf("Update.Symbol = %q, want the subscribed spelling Main.counter", u.Symbol)
		}
	case <-time.After(time.Second):
		t.Fatal("no update delivered")
	}
}

func intSample(v uint16) []byte {
	b := make([]byte, 2)
	binary.LittleEndian.PutUint16(b, v)
	return b
}

// ageOpenSubscribe backdates an open subscribe's start time, standing in for an
// RPC that has been in flight for a long time without waiting for one.
func ageOpenSubscribe(sess *Session, tok subscribeToken, start time.Time) {
	sess.notifications.openMu.Lock()
	defer sess.notifications.openMu.Unlock()
	sess.notifications.openSubscribes[tok] = start.UnixNano()
}

// earlySampleCount reports how many handles currently hold a buffered sample.
func earlySampleCount(sess *Session) int {
	sess.notifications.earlyMu.Lock()
	defer sess.notifications.earlyMu.Unlock()
	return len(sess.notifications.earlySamples)
}

// TestSubscribeRace_EarlySampleReplayedAfterCommit drives the exact ordering
// that loses a static symbol: the PLC emits the first (and, for a constant,
// only) sample while AddDeviceNotification is still in flight. The sample
// must be buffered and replayed once the handle is committed, not dropped.
func TestSubscribeRace_EarlySampleReplayedAfterCommit(t *testing.T) {
	srv := fakeplc.StartPLC(t)
	defer srv.Stop()

	var deleted atomic.Int32
	srv.OnDeleteDeviceNotification(func(_ uint32) ams.ReturnCode {
		deleted.Add(1)
		return ams.ReturnCodeNoErrors
	})

	sess, c := newWiredTestSession(t, srv)
	c.SetNotificationHandler(sess.handleNotification)
	preSeedTypedSymbol(sess, "MAIN.sMachineName", 0xC0DE)

	const plcHandle = 0x1234
	var addSeen atomic.Int32
	srv.OnAddDeviceNotification(func(_ fakeplc.AddNotifRequest) fakeplc.AddNotifResponse {
		// Only the caller's subscription (the first Add) gets plcHandle and the
		// early sample; anything else — the session's own cyclic heartbeat — must
		// get a DIFFERENT handle, as a real PLC would. A stub handing the same
		// handle to two subscriptions makes the heartbeat swallow the caller's
		// samples, which no device can actually do.
		if addSeen.Add(1) > 1 {
			return fakeplc.AddNotifResponse{Handle: plcHandle + uint32(addSeen.Load())}
		}
		// Fire the sample BEFORE the Add response goes back on the wire, so
		// the commit into activeNotifications cannot have happened yet.
		if err := sess.drivePacket(sess.lifecycle.ctx, buildNotificationPacket(plcHandle, 0, intSample(4242))); err != nil {
			t.Errorf("drivePacket from Add handler: %v", err)
		}
		return fakeplc.AddNotifResponse{Handle: plcHandle}
	})

	ch := make(chan *Update, 4)
	handle, err := sess.Subscribe(context.Background(), "MAIN.sMachineName", 0, 0, ams.TransModeServerOnChange, ch)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if handle != plcHandle {
		t.Fatalf("handle = 0x%X, want 0x%X", handle, plcHandle)
	}

	select {
	case u := <-ch:
		if u.Value != int16(4242) {
			t.Errorf("Update.Value = %#v, want int16(4242)", u.Value)
		}
		if u.Symbol != "MAIN.sMachineName" {
			t.Errorf("Update.Symbol = %q, want %q", u.Symbol, "MAIN.sMachineName")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("early sample never delivered: buffered sample was not replayed after commit")
	}

	if got := deleted.Load(); got != 0 {
		t.Errorf("Delete RPC calls = %d, want 0 (our own in-flight handle must never be reaped)", got)
	}
	if got := earlySampleCount(sess); got != 0 {
		t.Errorf("buffered samples after commit = %d, want 0", got)
	}
}

// TestSubscribeRace_BatchOnSumUnsupportedPLC reproduces the TC2 shape: the sum
// command is unsupported, so SubscribeAll degrades to one Add per
// symbol with enough latency that the batch outlasts subscribeRaceWindow. The
// first symbol streams while the last is still registering. Every early sample
// must survive and no handle may be reaped.
func TestSubscribeRace_BatchOnSumUnsupportedPLC(t *testing.T) {
	srv := fakeplc.StartPLC(t)
	defer srv.Stop()

	var deleted atomic.Int32
	var deletedHandles []uint32
	var delMu sync.Mutex
	srv.OnDeleteDeviceNotification(func(h uint32) ams.ReturnCode {
		delMu.Lock()
		deletedHandles = append(deletedHandles, h)
		delMu.Unlock()
		deleted.Add(1)
		return ams.ReturnCodeNoErrors
	})
	// 40ms per Add over 5 symbols puts the batch well past the 100ms window.
	srv.DelayBefore(ams.CommandAddDeviceNotification, 0, 40*time.Millisecond)

	sess, c := newWiredTestSession(t, srv)
	c.SetNotificationHandler(sess.handleNotification)
	// Mark the sum command unsupported, as TC2 does by answering 0x0701 to
	// group 0xF085. SumAddDeviceNotification then degrades to one Add per
	// symbol — the condition under which the batch outlasts the race window.
	if !c.Capabilities().SumAddNotifStateCAS(0, 2) {
		t.Fatal("could not force SumAddNotif into the unsupported state")
	}

	const symbolCount = 5
	names := make([]string, symbolCount)
	configs := make([]NotificationConfig, symbolCount)
	for i := range names {
		names[i] = "MAIN.tag" + string(rune('A'+i))
		preSeedTypedSymbol(sess, names[i], uint32(0xC000+i))
		configs[i] = NotificationConfig{Symbol: names[i], Mode: ams.TransModeServerOnChange}
	}

	var nextHandle atomic.Uint32
	nextHandle.Store(0x100)
	srv.OnAddDeviceNotification(func(_ fakeplc.AddNotifRequest) fakeplc.AddNotifResponse {
		h := nextHandle.Add(1)
		// Every symbol emits its one sample immediately, before its own Add
		// response is even sent — the worst case of the race.
		if err := sess.drivePacket(sess.lifecycle.ctx, buildNotificationPacket(h, 0, intSample(uint16(h)))); err != nil {
			t.Errorf("drivePacket from Add handler: %v", err)
		}
		return fakeplc.AddNotifResponse{Handle: h}
	})

	ch := make(chan *Update, 2*symbolCount)
	results, err := sess.subscribeAll(context.Background(), configs, ch)
	if err != nil {
		t.Fatalf("SubscribeAll: %v", err)
	}
	for i, r := range results {
		if r.Skipped != nil {
			t.Fatalf("results[%d] (%s) Skipped = %v", i, names[i], r.Skipped)
		}
		if r.Error != ams.ReturnCodeNoErrors {
			t.Fatalf("results[%d] (%s) Error = 0x%X", i, names[i], uint32(r.Error))
		}
	}

	got := make(map[string]any, symbolCount)
	deadline := time.After(3 * time.Second)
	for len(got) < symbolCount {
		select {
		case u := <-ch:
			got[u.Symbol] = u.Value
		case <-deadline:
			t.Fatalf("delivered %d/%d symbols, want all; missing early samples were dropped: got=%v", len(got), symbolCount, got)
		}
	}
	for _, name := range names {
		if _, ok := got[name]; !ok {
			t.Errorf("no Update for %s", name)
		}
	}

	// Give any scheduled reaper goroutine time to fire before asserting.
	time.Sleep(200 * time.Millisecond)
	if n := deleted.Load(); n != 0 {
		delMu.Lock()
		handles := append([]uint32(nil), deletedHandles...)
		delMu.Unlock()
		t.Errorf("Delete RPC calls = %d, want 0 (reaper deleted live subscriptions): handles=%v", n, handles)
	}
}

// TestSubscribeRace_BatchBindsEachHandleBeforeNextAdd pins the per-item bind:
// on a PLC that rejects the sum command, each handle must be in
// activeNotifications before the next AddDeviceNotification is issued. Binding
// the whole batch at the end instead leaves the early handles unrecognisable
// for the rest of the batch, which is what the PLC is streaming into.
func TestSubscribeRace_BatchBindsEachHandleBeforeNextAdd(t *testing.T) {
	srv := fakeplc.StartPLC(t)
	defer srv.Stop()

	sess, c := newWiredTestSession(t, srv)
	c.SetNotificationHandler(sess.handleNotification)
	if !c.Capabilities().SumAddNotifStateCAS(0, 2) {
		t.Fatal("could not force SumAddNotif into the unsupported state")
	}

	const symbolCount = 4
	names := make([]string, symbolCount)
	configs := make([]NotificationConfig, symbolCount)
	for i := range names {
		names[i] = "MAIN.bind" + string(rune('A'+i))
		preSeedTypedSymbol(sess, names[i], uint32(0xD000+i))
		configs[i] = NotificationConfig{Symbol: names[i], Mode: ams.TransModeServerOnChange}
	}

	var mu sync.Mutex
	var issued []uint32 // handles already returned by a previous Add
	var unboundAtNextAdd []uint32
	var nextHandle atomic.Uint32
	nextHandle.Store(0x200)

	srv.OnAddDeviceNotification(func(_ fakeplc.AddNotifRequest) fakeplc.AddNotifResponse {
		// Every handle handed out earlier in this batch must be bound by now.
		mu.Lock()
		prior := append([]uint32(nil), issued...)
		mu.Unlock()
		sess.notifications.lock.Lock()
		for _, h := range prior {
			if _, ok := sess.notifications.activeNotifications[h]; !ok {
				unboundAtNextAdd = append(unboundAtNextAdd, h)
			}
		}
		sess.notifications.lock.Unlock()

		h := nextHandle.Add(1)
		mu.Lock()
		issued = append(issued, h)
		mu.Unlock()
		return fakeplc.AddNotifResponse{Handle: h}
	})

	ch := make(chan *Update, 4*symbolCount)
	results, err := sess.subscribeAll(context.Background(), configs, ch)
	if err != nil {
		t.Fatalf("SubscribeAll: %v", err)
	}
	for i, r := range results {
		if r.Skipped != nil || r.Error != ams.ReturnCodeNoErrors {
			t.Fatalf("results[%d] (%s): Skipped=%v Error=0x%X", i, names[i], r.Skipped, uint32(r.Error))
		}
	}
	if len(unboundAtNextAdd) != 0 {
		t.Errorf("handles still unbound when the next Add was issued: %v", unboundAtNextAdd)
	}

	sess.notifications.lock.Lock()
	active := len(sess.notifications.activeNotifications)
	sess.notifications.lock.Unlock()
	if active != symbolCount {
		t.Errorf("activeNotifications = %d, want %d", active, symbolCount)
	}
}

// TestSubscribeRace_ReloadMidBatchStrandsWholeBatch: a symbol-cache reload
// landing partway through a batch invalidates the entries committed before it
// (the reload snapshots activeNotifications and deletes those handles PLC-side)
// as well as everything after it. Both halves must come back Skipped with
// ErrNotificationStrandedByReload — reporting the first half as success hands
// the caller subscriptions that exist on neither side.
//
// The epoch is bumped from inside the Add handler, which is exactly where a
// real reload lands: between two individual Adds of a sum-unsupported batch.
func TestSubscribeRace_ReloadMidBatchStrandsWholeBatch(t *testing.T) {
	srv := fakeplc.StartPLC(t)
	defer srv.Stop()

	sess, c := newWiredTestSession(t, srv)
	c.SetNotificationHandler(sess.handleNotification)
	if !c.Capabilities().SumAddNotifStateCAS(0, 2) {
		t.Fatal("could not force SumAddNotif into the unsupported state")
	}

	const symbolCount = 4
	names := make([]string, symbolCount)
	configs := make([]NotificationConfig, symbolCount)
	for i := range names {
		names[i] = "MAIN.reload" + string(rune('A'+i))
		preSeedTypedSymbol(sess, names[i], uint32(0xE000+i))
		configs[i] = NotificationConfig{Symbol: names[i], Mode: ams.TransModeServerOnChange}
	}

	var adds atomic.Int32
	var nextHandle atomic.Uint32
	nextHandle.Store(0x300)
	// Written on the stub's goroutine, read on the test's: guarded, because a
	// data race here would be reported as a failure of the code under test.
	var reapedMu sync.Mutex
	var reapedByReload []uint32
	srv.OnAddDeviceNotification(func(_ fakeplc.AddNotifRequest) fakeplc.AddNotifResponse {
		// After the second Add, do what autoReloadOnStaleDetection actually does
		// to shared state, in its real order: bump the epoch FIRST, then
		// snapshot activeNotifications and swap the map. The amendment's
		// correctness depends on exactly that ordering, so the test has to
		// reproduce it rather than just move the counter.
		if adds.Add(1) == 2 {
			sess.bumpEpoch()
			sess.notifications.lock.Lock()
			for h := range sess.notifications.activeNotifications {
				reapedMu.Lock()
				reapedByReload = append(reapedByReload, h)
				reapedMu.Unlock()
			}
			sess.notifications.activeNotifications = make(map[uint32]activeNotification)
			sess.notifications.lock.Unlock()
		}
		return fakeplc.AddNotifResponse{Handle: nextHandle.Add(1)}
	})

	ch := make(chan *Update, symbolCount)
	results, err := sess.subscribeAll(context.Background(), configs, ch)
	if err != nil {
		t.Fatalf("SubscribeAll: %v", err)
	}

	for i, r := range results {
		if r.Skipped == nil {
			t.Errorf("results[%d] (%s) reported success, but the reload invalidated it", i, names[i])
			continue
		}
		if !errors.Is(r.Skipped, ErrNotificationStrandedByReload) {
			t.Errorf("results[%d] (%s) Skipped = %v, want ErrNotificationStrandedByReload", i, names[i], r.Skipped)
		}
		// The PLC created the registration before the library refused it, so the
		// handle has to reach the caller for cleanup.
		if r.Handle == 0 {
			t.Errorf("results[%d] (%s) Handle = 0; the caller cannot release what it is not told about", i, names[i])
		}
	}
	// The entries the reload swept are precisely the ones reported as stranded:
	// that equality is the invariant the amendment relies on.
	reapedMu.Lock()
	sweptCount := len(reapedByReload)
	reapedMu.Unlock()
	if sweptCount == 0 {
		t.Error("the simulated reload swept nothing — the test did not reproduce the interleaving it claims to")
	}
	sess.notifications.lock.Lock()
	surviving := len(sess.notifications.activeNotifications)
	sess.notifications.lock.Unlock()
	if surviving != 0 {
		t.Errorf("activeNotifications = %d, want 0 (nothing may be committed after the reload)", surviving)
	}
}

// TestSubscribeRace_PlainSymbolReloadDoesNotStrandBatch: an epoch bump is not by
// itself evidence that anything was stranded. A caller running LoadSymbols /
// RefreshSymbols in parallel advances the epoch but never touches
// activeNotifications, so entries this batch bound are alive on both sides. The
// amendment used to key on the epoch and tore them down anyway: reported
// Skipped, then deleted PLC-side — a working subscription destroyed by an
// unrelated cache refresh. It must key on an actual notification sweep.
func TestSubscribeRace_PlainSymbolReloadDoesNotStrandBatch(t *testing.T) {
	srv := fakeplc.StartPLC(t)
	defer srv.Stop()

	sess, c := newWiredTestSession(t, srv)
	c.SetNotificationHandler(sess.handleNotification)
	if !c.Capabilities().SumAddNotifStateCAS(0, 2) {
		t.Fatal("could not force SumAddNotif into the unsupported state")
	}

	const symbolCount = 3
	names := make([]string, symbolCount)
	configs := make([]NotificationConfig, symbolCount)
	for i := range names {
		names[i] = "MAIN.plain" + string(rune('A'+i))
		preSeedTypedSymbol(sess, names[i], uint32(0xC100+i))
		configs[i] = NotificationConfig{Symbol: names[i], Mode: ams.TransModeServerOnChange}
	}

	var deletedMu sync.Mutex
	var deleted []uint32
	srv.OnDeleteDeviceNotification(func(h uint32) ams.ReturnCode {
		deletedMu.Lock()
		deleted = append(deleted, h)
		deletedMu.Unlock()
		return ams.ReturnCodeNoErrors
	})

	var nextHandle atomic.Uint32
	nextHandle.Store(0x800)
	var adds atomic.Int32
	srv.OnAddDeviceNotification(func(_ fakeplc.AddNotifRequest) fakeplc.AddNotifResponse {
		// A concurrent LoadSymbols lands after the first commit: epoch moves,
		// activeNotifications is untouched.
		if adds.Add(1) == 2 {
			sess.bumpEpoch()
		}
		return fakeplc.AddNotifResponse{Handle: nextHandle.Add(1)}
	})

	ch := make(chan *Update, symbolCount)
	results, err := sess.subscribeAll(context.Background(), configs, ch)
	if err != nil {
		t.Fatalf("SubscribeAll: %v", err)
	}

	// Item 0 was committed before the bump and nothing swept it: it stays a
	// success. (Items after the bump are legitimately refused — their symbol
	// handles came from the pre-reload cache.)
	if results[0].Skipped != nil {
		t.Errorf("results[0] (%s) Skipped = %v; a plain symbol reload strands nothing", names[0], results[0].Skipped)
	}
	if results[0].Handle == 0 {
		t.Errorf("results[0] (%s) has no handle despite committing", names[0])
	}

	sess.notifications.lock.Lock()
	_, stillBound := sess.notifications.activeNotifications[results[0].Handle]
	sess.notifications.lock.Unlock()
	if !stillBound {
		t.Errorf("handle %d is no longer bound; the batch unwound a live subscription", results[0].Handle)
	}
	deletedMu.Lock()
	releasedFirst := slices.Contains(deleted, results[0].Handle)
	deletedMu.Unlock()
	if releasedFirst {
		t.Errorf("handle %d was released PLC-side; the caller still holds it", results[0].Handle)
	}
}

// TestSubscribeRace_PostSweepCommitIsNotStranded: "a sweep happened" is not the
// same question as "was MY entry swept". A batch item that commits AFTER the
// sweep goes into the fresh activeNotifications map — nothing swept it, and its
// handle is in no snapshot anyone will delete. Reporting it stranded is a double
// failure: the caller is told a working subscription is gone, and the library
// then deletes the handle out from under itself.
//
// The reconnect path is where this bites. Auto-reload bumps the epoch before it
// sweeps, so commitNotification refuses late commits; Reconnect does not, so the
// late commit succeeds and only the sweep-vs-batch comparison can catch it.
func TestSubscribeRace_PostSweepCommitIsNotStranded(t *testing.T) {
	srv := fakeplc.StartPLC(t)
	defer srv.Stop()

	sess, c := newWiredTestSession(t, srv)
	c.SetNotificationHandler(sess.handleNotification)
	if !c.Capabilities().SumAddNotifStateCAS(0, 2) {
		t.Fatal("could not force SumAddNotif into the unsupported state")
	}
	if !c.Capabilities().SumDeleteNotifStateCAS(0, 2) {
		t.Fatal("could not force SumDeleteNotif into the unsupported state")
	}

	const symbolCount = 2
	names := make([]string, symbolCount)
	configs := make([]NotificationConfig, symbolCount)
	for i := range names {
		names[i] = "MAIN.post" + string(rune('A'+i))
		preSeedTypedSymbol(sess, names[i], uint32(0xD200+i))
		configs[i] = NotificationConfig{Symbol: names[i], Mode: ams.TransModeServerOnChange}
	}

	var deletedMu sync.Mutex
	var deleted []uint32
	srv.OnDeleteDeviceNotification(func(h uint32) ams.ReturnCode {
		deletedMu.Lock()
		deleted = append(deleted, h)
		deletedMu.Unlock()
		return ams.ReturnCodeNoErrors
	})

	var adds atomic.Int32
	var nextHandle atomic.Uint32
	nextHandle.Store(0x900)
	srv.OnAddDeviceNotification(func(_ fakeplc.AddNotifRequest) fakeplc.AddNotifResponse {
		// Between item 0's commit and item 1's, wipe the map the way Reconnect
		// does — and, like Reconnect, WITHOUT bumping the epoch, so item 1's
		// commit legitimately lands in the new map.
		if adds.Add(1) == 2 {
			sess.notifications.lock.Lock()
			sess.notifications.activeNotifications = make(map[uint32]activeNotification)
			sess.notifications.lock.Unlock()
		}
		return fakeplc.AddNotifResponse{Handle: nextHandle.Add(1)}
	})

	ch := make(chan *Update, symbolCount)
	results, err := sess.subscribeAll(context.Background(), configs, ch)
	if err != nil {
		t.Fatalf("SubscribeAll: %v", err)
	}

	// Item 0 was in the snapshot the sweep took: correctly stranded.
	if results[0].Skipped == nil {
		t.Errorf("results[0] (%s) reported success, but the sweep removed it", names[0])
	}

	// Item 1 committed after the sweep. It is bound, live, and owned by us.
	if results[1].Skipped != nil {
		t.Errorf("results[1] (%s) Skipped = %v; it committed after the sweep, so nothing stranded it",
			names[1], results[1].Skipped)
	}
	if results[1].Handle == 0 {
		t.Fatalf("results[1] (%s) has no handle despite committing", names[1])
	}
	sess.notifications.lock.Lock()
	_, stillBound := sess.notifications.activeNotifications[results[1].Handle]
	sess.notifications.lock.Unlock()
	if !stillBound {
		t.Errorf("handle %d is no longer bound; the batch tore down a live subscription", results[1].Handle)
	}
	deletedMu.Lock()
	releasedLive := slices.Contains(deleted, results[1].Handle)
	deletedMu.Unlock()
	if releasedLive {
		t.Errorf("handle %d was deleted PLC-side while the caller still owns it", results[1].Handle)
	}
}

// TestSubscribeRace_StrandedSymbolCanBeResubscribed: ErrNotificationStrandedByReload
// is documented as retryable, so a retry has to actually work. commitNotification
// registers the symbol in configsByKey before the sweep takes it away, and the
// cleanup delete cannot undo that — Session.SumDeleteDeviceNotification only
// drops a config for a handle still present in activeNotifications, and the sweep
// emptied that map. So the entry the caller was told to retry was rejected
// ErrNotificationDuplicate forever, rescued only if an unrelated reconnect
// happened to reset the config table.
func TestSubscribeRace_StrandedSymbolCanBeResubscribed(t *testing.T) {
	srv := fakeplc.StartPLC(t)
	defer srv.Stop()

	sess, c := newWiredTestSession(t, srv)
	c.SetNotificationHandler(sess.handleNotification)
	if !c.Capabilities().SumAddNotifStateCAS(0, 2) {
		t.Fatal("could not force SumAddNotif into the unsupported state")
	}
	if !c.Capabilities().SumDeleteNotifStateCAS(0, 2) {
		t.Fatal("could not force SumDeleteNotif into the unsupported state")
	}
	srv.OnDeleteDeviceNotification(func(_ uint32) ams.ReturnCode { return ams.ReturnCodeNoErrors })

	const symbolCount = 2
	names := make([]string, symbolCount)
	configs := make([]NotificationConfig, symbolCount)
	for i := range names {
		names[i] = "MAIN.retry" + string(rune('A'+i))
		preSeedTypedSymbol(sess, names[i], uint32(0xD400+i))
		configs[i] = NotificationConfig{Symbol: names[i], Mode: ams.TransModeServerOnChange}
	}

	var adds atomic.Int32
	var nextHandle atomic.Uint32
	nextHandle.Store(0xA00)
	srv.OnAddDeviceNotification(func(_ fakeplc.AddNotifRequest) fakeplc.AddNotifResponse {
		// Sweep after item 0 is bound, so item 0 comes back stranded.
		if adds.Add(1) == 2 {
			sess.notifications.lock.Lock()
			sess.notifications.activeNotifications = make(map[uint32]activeNotification)
			sess.notifications.lock.Unlock()
		}
		return fakeplc.AddNotifResponse{Handle: nextHandle.Add(1)}
	})

	ch := make(chan *Update, symbolCount)
	results, err := sess.subscribeAll(context.Background(), configs, ch)
	if err != nil {
		t.Fatalf("SubscribeAll: %v", err)
	}
	if !errors.Is(results[0].Skipped, ErrNotificationStrandedByReload) {
		t.Fatalf("results[0] Skipped = %v, want ErrNotificationStrandedByReload — the test did not reproduce the stranding it needs", results[0].Skipped)
	}

	// The documented response to a stranded entry: subscribe it again.
	if _, err := sess.Subscribe(context.Background(), names[0], 0, 0, ams.TransModeServerOnChange, ch); err != nil {
		if errors.Is(err, ErrNotificationDuplicate) || strings.Contains(err.Error(), "already") {
			t.Errorf("retry of a stranded symbol rejected as a duplicate: %v", err)
		} else {
			t.Errorf("retry of a stranded symbol failed: %v", err)
		}
	}
}

// TestSubscribeRace_AbortedBatchStillAmendsAndReleases: a batch that gives up
// part-way is exactly the case where the PLC has created handles nobody owns and
// where a reload may have landed on the ones already bound. The abort path used
// to return straight out, skipping both the reload amendment and the release —
// so the caller was told a stranded entry had succeeded, and the handle stayed
// registered, streaming into a channel nothing reads.
//
// The batch is aborted with an expiring context, which keeps the connection
// alive so the cleanup delete is observable. That is also why the release runs on
// a fresh context: on the caller's expired one it would fail before being sent.
func TestSubscribeRace_AbortedBatchStillAmendsAndReleases(t *testing.T) {
	srv := fakeplc.StartPLC(t)
	defer srv.Stop()

	sess, c := newWiredTestSession(t, srv)
	c.SetNotificationHandler(sess.handleNotification)
	if !c.Capabilities().SumAddNotifStateCAS(0, 2) {
		t.Fatal("could not force SumAddNotif into the unsupported state")
	}
	// Sum-delete goes down the same fallback road, one Delete per handle, so the
	// stub sees them individually.
	if !c.Capabilities().SumDeleteNotifStateCAS(0, 2) {
		t.Fatal("could not force SumDeleteNotif into the unsupported state")
	}

	const symbolCount = 3
	names := make([]string, symbolCount)
	configs := make([]NotificationConfig, symbolCount)
	for i := range names {
		names[i] = "MAIN.abort" + string(rune('A'+i))
		preSeedTypedSymbol(sess, names[i], uint32(0xB000+i))
		configs[i] = NotificationConfig{Symbol: names[i], Mode: ams.TransModeServerOnChange}
	}

	var deletedMu sync.Mutex
	var deleted []uint32
	srv.OnDeleteDeviceNotification(func(h uint32) ams.ReturnCode {
		deletedMu.Lock()
		deleted = append(deleted, h)
		deletedMu.Unlock()
		return ams.ReturnCodeNoErrors
	})

	var adds atomic.Int32
	var nextHandle atomic.Uint32
	nextHandle.Store(0x700)
	srv.OnAddDeviceNotification(func(_ fakeplc.AddNotifRequest) fakeplc.AddNotifResponse {
		if adds.Add(1) == 2 {
			// Item 0 is bound by now, so the reload lands on a committed entry and
			// the amendment — not commitNotification's own epoch check — is what has
			// to strand it. Then outlast the caller's deadline so the batch aborts
			// here, on a live connection.
			sess.bumpEpoch()
			sess.notifications.lock.Lock()
			sess.notifications.activeNotifications = make(map[uint32]activeNotification)
			sess.notifications.lock.Unlock()
			time.Sleep(600 * time.Millisecond)
		}
		return fakeplc.AddNotifResponse{Handle: nextHandle.Add(1)}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	ch := make(chan *Update, symbolCount)
	results, err := sess.subscribeAll(ctx, configs, ch)
	if err == nil {
		t.Fatal("batch reported success despite the context expiring mid-batch")
	}
	if len(results) != symbolCount {
		t.Fatalf("results len = %d, want %d", len(results), symbolCount)
	}

	// The amendment must have run on the abort path: item 0 was bound before the
	// reload, so it cannot be reported as a success.
	if results[0].Skipped == nil {
		t.Errorf("results[0] (%s) reported success, but a reload invalidated it before the abort", names[0])
	} else if !errors.Is(results[0].Skipped, ErrNotificationStrandedByReload) {
		t.Errorf("results[0] Skipped = %v, want ErrNotificationStrandedByReload", results[0].Skipped)
	}
	if results[0].Handle == 0 {
		t.Error("results[0] Handle = 0; the caller cannot release what it is not told about")
	}

	// The release must have run too, on a context the caller's expiry did not kill.
	deadline := time.Now().Add(2 * time.Second)
	for {
		deletedMu.Lock()
		n := len(deleted)
		deletedMu.Unlock()
		if n > 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	deletedMu.Lock()
	got := append([]uint32(nil), deleted...)
	deletedMu.Unlock()
	if len(got) == 0 {
		t.Error("no handle was released after the abort — the PLC keeps streaming a subscription nothing owns")
	}
	if results[0].Handle != 0 && !slices.Contains(got, results[0].Handle) {
		t.Errorf("stranded handle %d was not released; deletes seen: %v", results[0].Handle, got)
	}

	sess.notifications.lock.Lock()
	bound := len(sess.notifications.activeNotifications)
	sess.notifications.lock.Unlock()
	if bound != 0 {
		t.Errorf("activeNotifications = %d, want 0 after an aborted batch whose commits were stranded", bound)
	}
}

// TestOrphanReaperArmedAfterAbortedBatch: an Add whose reply is lost to a
// transport failure may have created a registration on the PLC whose handle
// number this side never learned. Nothing can delete a number nobody has, so the
// only cleanup is the orphan reaper firing when that handle next streams — which
// makes "the reaper is armed once an aborted batch returns" a load-bearing claim
// rather than a detail. It would not hold if the batch left its subscribe window
// open: while the window is open an unknown handle is presumed to be ours and the
// sample is buffered instead.
func TestOrphanReaperArmedAfterAbortedBatch(t *testing.T) {
	srv := fakeplc.StartPLC(t)
	defer srv.Stop()

	sess, c := newWiredTestSession(t, srv)
	c.SetNotificationHandler(sess.handleNotification)
	if !c.Capabilities().SumAddNotifStateCAS(0, 2) {
		t.Fatal("could not force SumAddNotif into the unsupported state")
	}

	var deletedMu sync.Mutex
	var deleted []uint32
	srv.OnDeleteDeviceNotification(func(h uint32) ams.ReturnCode {
		deletedMu.Lock()
		deleted = append(deleted, h)
		deletedMu.Unlock()
		return ams.ReturnCodeNoErrors
	})

	const symbolCount = 3
	configs := make([]NotificationConfig, symbolCount)
	for i := range configs {
		name := fmt.Sprintf("MAIN.reaped%d", i)
		preSeedTypedSymbol(sess, name, uint32(0xD600+i))
		configs[i] = NotificationConfig{Symbol: name, Mode: ams.TransModeServerOnChange}
	}

	var adds atomic.Int32
	var nextHandle atomic.Uint32
	nextHandle.Store(0xC00)
	srv.OnAddDeviceNotification(func(_ fakeplc.AddNotifRequest) fakeplc.AddNotifResponse {
		if adds.Add(1) == 2 {
			// Answer late enough that the caller's deadline expires first: the PLC
			// created a registration whose reply this side never used, which is the
			// shape that leaves an unknown handle behind.
			time.Sleep(600 * time.Millisecond)
		}
		return fakeplc.AddNotifResponse{Handle: nextHandle.Add(1)}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	ch := make(chan *Update, symbolCount)
	if _, err := sess.subscribeAll(ctx, configs, ch); err == nil {
		t.Fatal("batch unexpectedly succeeded; the abort this test needs did not happen")
	}

	// The window must be shut, or the sample below is buffered as "probably ours".
	if n := sess.notifications.subscribeInFlight.Load(); n != 0 {
		t.Fatalf("subscribeInFlight = %d after the batch returned; the reaper is still disabled", n)
	}
	// Past the trailing race window too.
	time.Sleep(subscribeRaceWindow + 50*time.Millisecond)

	// The PLC streams the handle nobody recorded.
	const unknown = 0xC0FF
	if err := sess.drivePacket(sess.currentLifecycleCtx(), buildNotificationPacket(unknown, 0, intSample(1))); err != nil {
		t.Fatalf("drivePacket: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		deletedMu.Lock()
		seen := slices.Contains(deleted, unknown)
		deletedMu.Unlock()
		if seen || time.Now().After(deadline) {
			if !seen {
				t.Errorf("handle 0x%X was never reaped; an Add whose reply was lost would stay registered on the PLC", unknown)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestSubscribeRace_ConnectionDropsMidBatch is the realistic trigger: not an
// engineer doing an online change, but the link failing while a sum-unsupported
// PLC is being subscribed one Add at a time. The stub answers two Adds and then
// drops the TCP connection outright.
//
// What must hold afterwards: every config has a verdict, nothing claims success
// without a handle, and the reported successes match what is actually bound —
// a result set that lies about the transport being alive is the failure mode
// this whole branch exists to eliminate.
func TestSubscribeRace_ConnectionDropsMidBatch(t *testing.T) {
	srv := fakeplc.StartPLC(t)
	defer srv.Stop()

	sess, c := newWiredTestSession(t, srv)
	c.SetNotificationHandler(sess.handleNotification)
	if !c.Capabilities().SumAddNotifStateCAS(0, 2) {
		t.Fatal("could not force SumAddNotif into the unsupported state")
	}
	// Don't let the drop spawn a reconnect that races the assertions.
	c.SetOnDrop(nil)

	const symbolCount = 5
	names := make([]string, symbolCount)
	configs := make([]NotificationConfig, symbolCount)
	for i := range names {
		names[i] = "MAIN.drop" + string(rune('A'+i))
		preSeedTypedSymbol(sess, names[i], uint32(0xF000+i))
		configs[i] = NotificationConfig{Symbol: names[i], Mode: ams.TransModeServerOnChange}
	}

	var nextHandle atomic.Uint32
	nextHandle.Store(0x400)
	srv.OnAddDeviceNotification(func(_ fakeplc.AddNotifRequest) fakeplc.AddNotifResponse {
		return fakeplc.AddNotifResponse{Handle: nextHandle.Add(1)}
	})
	// Answer two Adds, then vanish mid-request on the third.
	srv.DropConnAfter(ams.CommandAddDeviceNotification, 3)

	ch := make(chan *Update, symbolCount)
	results, err := sess.subscribeAll(context.Background(), configs, ch)
	// An error is acceptable here and so is nil — what matters is the results.
	t.Logf("SubscribeAll returned err=%v", err)

	if len(results) != symbolCount {
		t.Fatalf("results len = %d, want %d", len(results), symbolCount)
	}
	successes := 0
	for i, r := range results {
		switch {
		case r.Skipped != nil:
			// fine: refused, with the handle surfaced if the PLC made one
		case r.Error != ams.ReturnCodeNoErrors:
			// fine: PLC-side rejection
		case r.Handle != 0:
			successes++
		default:
			t.Errorf("results[%d] (%s) is a zero value: no verdict was recorded for it", i, names[i])
		}
	}

	sess.notifications.lock.Lock()
	bound := len(sess.notifications.activeNotifications)
	sess.notifications.lock.Unlock()
	if successes != bound {
		t.Errorf("reported %d successes but %d handles are bound — the result set does not match reality", successes, bound)
	}
	t.Logf("after mid-batch link loss: %d reported success, %d bound", successes, bound)

	// The subscribe window must have closed despite the failure, or the orphan
	// reaper stays disabled for the life of the session.
	if n := sess.notifications.subscribeInFlight.Load(); n != 0 {
		t.Errorf("subscribeInFlight = %d, want 0 after the batch returned", n)
	}
}

// TestSubscribeRace_ConnectionDropsMidBatchAtScale is the same failure at field
// size. It pins the cost, because the cost IS the bug: a dead transport is not
// marked dead (only Client.Close sets tx.disconnected — listen's drop path just
// fires ondrop), so every remaining Add waits out its own request timeout on a
// corpse. 40 symbols dropping early means the batch holds subscribeInFlight for
// minutes, and the orphan reaper is disabled for every second of it.
func TestSubscribeRace_ConnectionDropsMidBatchAtScale(t *testing.T) {
	srv := fakeplc.StartPLC(t)
	defer srv.Stop()

	sess, c := newWiredTestSession(t, srv, WithRequestTimeout(300*time.Millisecond))
	c.SetNotificationHandler(sess.handleNotification)
	if !c.Capabilities().SumAddNotifStateCAS(0, 2) {
		t.Fatal("could not force SumAddNotif into the unsupported state")
	}
	c.SetOnDrop(nil)

	const symbolCount = 40
	configs := make([]NotificationConfig, symbolCount)
	for i := range configs {
		name := fmt.Sprintf("MAIN.scale%02d", i)
		preSeedTypedSymbol(sess, name, uint32(0xA000+i))
		configs[i] = NotificationConfig{Symbol: name, Mode: ams.TransModeServerOnChange}
	}

	var nextHandle atomic.Uint32
	nextHandle.Store(0x500)
	var adds atomic.Int32
	srv.OnAddDeviceNotification(func(_ fakeplc.AddNotifRequest) fakeplc.AddNotifResponse {
		adds.Add(1)
		return fakeplc.AddNotifResponse{Handle: nextHandle.Add(1)}
	})
	// Die after the third Add: 37 requests still to go.
	srv.DropConnAfter(ams.CommandAddDeviceNotification, 3)

	ch := make(chan *Update, symbolCount)
	start := time.Now()
	results, err := sess.subscribeAll(context.Background(), configs, ch)
	elapsed := time.Since(start)
	t.Logf("40-symbol batch with the link dying at Add 3: took %v, err=%v (answered Adds=%d)",
		elapsed.Round(time.Millisecond), err, adds.Load())

	if len(results) != symbolCount {
		t.Fatalf("results len = %d, want %d", len(results), symbolCount)
	}
	successes, transportFailures, other := 0, 0, 0
	for _, r := range results {
		switch {
		case r.Skipped != nil && errors.Is(r.Skipped, ErrNotificationTransportFailure):
			transportFailures++
		case r.Skipped != nil:
			other++
		case r.Handle != 0 && r.Error == ams.ReturnCodeNoErrors:
			successes++
		default:
			other++
		}
	}
	t.Logf("verdicts: %d success, %d transport-failure, %d other", successes, transportFailures, other)

	// The batch must give up once the transport is confirmed dead rather than
	// waiting out a timeout per remaining symbol. With a 300ms timeout, honest
	// abort finishes in well under a second; the pre-fix behaviour is ~37 x 300ms.
	if elapsed > 5*time.Second {
		t.Errorf("batch took %v after the link died — it is timing out per symbol instead of aborting", elapsed)
	}
	// A link failure must not be reported as a PLC-side rejection.
	if transportFailures == 0 {
		t.Errorf("no entry reported ErrNotificationTransportFailure; %d 'other' verdicts hide a link failure as something else", other)
	}
	if n := sess.notifications.subscribeInFlight.Load(); n != 0 {
		t.Errorf("subscribeInFlight = %d, want 0", n)
	}
}

// TestSubscribeFallback_AMSRouterErrorAbortsBatch is the deliberate sibling of
// the test above, one failure mode over: instead of the socket dying, the AMS
// router keeps answering and refuses every request. That is what a TwinCAT
// system dropping into CONFIG does — port 851 stops existing, the connection
// stays up, and each reply carries AMS ErrorCode 0x06.
//
// The router's refusal must not be recorded as a per-item PLC verdict. Skipped
// == nil means "Error carries the PLC-side return code" (internal/adsconn/cmd_sum.go's
// SumNotificationResult contract), so mislabelling here tells the consumer the
// runtime individually rejected 37 named symbols it never saw — destroying the
// ErrNotificationTransportFailure retry signal that is the documented way to
// know the batch is worth retrying.
func TestSubscribeFallback_AMSRouterErrorAbortsBatch(t *testing.T) {
	srv := fakeplc.StartPLC(t)
	defer srv.Stop()

	sess, c := newWiredTestSession(t, srv, WithRequestTimeout(300*time.Millisecond))
	c.SetNotificationHandler(sess.handleNotification)
	if !c.Capabilities().SumAddNotifStateCAS(0, 2) {
		t.Fatal("could not force SumAddNotif into the unsupported state")
	}
	c.SetOnDrop(nil)

	const symbolCount = 40
	configs := make([]NotificationConfig, symbolCount)
	for i := range configs {
		name := fmt.Sprintf("MAIN.router%02d", i)
		preSeedTypedSymbol(sess, name, uint32(0xB000+i))
		configs[i] = NotificationConfig{Symbol: name, Mode: ams.TransModeServerOnChange}
	}

	var nextHandle atomic.Uint32
	nextHandle.Store(0x500)
	var adds atomic.Int32
	srv.OnAddDeviceNotification(func(_ fakeplc.AddNotifRequest) fakeplc.AddNotifResponse {
		adds.Add(1)
		return fakeplc.AddNotifResponse{Handle: nextHandle.Add(1)}
	})
	// Items 0-2 get handles; from item 3 on, the router refuses. Sticky, as
	// CONFIG mode is.
	srv.AMSErrorAfter(ams.CommandAddDeviceNotification, 4, ams.ReturnCodeGlobalTargetPortNotFound)

	ch := make(chan *Update, symbolCount)
	results, err := sess.subscribeAll(context.Background(), configs, ch)
	t.Logf("40-symbol batch with the router refusing from Add 4: err=%v (answered Adds=%d)", err, adds.Load())

	if err == nil {
		t.Error("SubscribeAll returned nil error after the router refused 37 of 40 items")
	}
	if len(results) != symbolCount {
		t.Fatalf("results len = %d, want %d", len(results), symbolCount)
	}
	successes, transportFailures, routerCodeAsVerdict, other := 0, 0, 0, 0
	for _, r := range results {
		switch {
		case r.Skipped != nil && errors.Is(r.Skipped, ErrNotificationTransportFailure):
			transportFailures++
		case r.Skipped == nil && r.Error == ams.ReturnCodeGlobalTargetPortNotFound:
			routerCodeAsVerdict++
		case r.Skipped == nil && r.Handle != 0 && r.Error == ams.ReturnCodeNoErrors:
			successes++
		default:
			other++
		}
	}
	t.Logf("verdicts: %d success, %d transport-failure, %d router-code-as-device-verdict, %d other",
		successes, transportFailures, routerCodeAsVerdict, other)

	// The load-bearing assertion: a router code must never be presented as a
	// per-item PLC verdict.
	if routerCodeAsVerdict != 0 {
		t.Errorf("%d items report the router's 0x06 as a PLC verdict (Skipped == nil), want 0", routerCodeAsVerdict)
	}
	if successes != 3 {
		t.Errorf("successes = %d, want 3", successes)
	}
	if transportFailures != symbolCount-3 {
		t.Errorf("transport failures = %d, want %d", transportFailures, symbolCount-3)
	}
	if other != 0 {
		t.Errorf("%d items have a verdict that is none of the three expected shapes", other)
	}
	// The batch must stop issuing requests at the first refusal instead of
	// sending all 40 against a router that has already said no.
	if got := adds.Load(); got != 4 {
		t.Errorf("stub answered %d Adds, want 4 — the batch carried on past the first router refusal", got)
	}
	if n := sess.notifications.subscribeInFlight.Load(); n != 0 {
		t.Errorf("subscribeInFlight = %d, want 0", n)
	}
}

// TestSubscribeRace_UncommittedSamplesDiscarded: when the subscribe that
// opened the window commits nothing (PLC rejected the item), the parked
// sample must be discarded rather than retained. A genuinely leaked handle
// keeps firing and its next sample takes the orphan path normally.
func TestSubscribeRace_UncommittedSamplesDiscarded(t *testing.T) {
	srv := fakeplc.StartPLC(t)
	defer srv.Stop()

	sess, c := newWiredTestSession(t, srv)
	c.SetNotificationHandler(sess.handleNotification)
	preSeedTypedSymbol(sess, "MAIN.rejected", 0xC0DE)

	srv.OnAddDeviceNotification(func(_ fakeplc.AddNotifRequest) fakeplc.AddNotifResponse {
		if err := sess.drivePacket(sess.lifecycle.ctx, buildNotificationPacket(0x555, 0, intSample(7))); err != nil {
			t.Errorf("drivePacket from Add handler: %v", err)
		}
		return fakeplc.AddNotifResponse{Error: ams.ReturnCodeDeviceInvalidParam}
	})

	ch := make(chan *Update, 1)
	if _, err := sess.Subscribe(context.Background(), "MAIN.rejected", 0, 0, ams.TransModeServerOnChange, ch); err == nil {
		t.Fatal("Subscribe: err = nil, want PLC rejection")
	}

	if got := earlySampleCount(sess); got != 0 {
		t.Errorf("buffered samples after failed subscribe = %d, want 0", got)
	}
	select {
	case u := <-ch:
		t.Errorf("unexpected Update %+v for a handle that was never committed", u)
	default:
	}
}

// TestReplayEarlySamples_LeavesUnboundHandleParked: a handle still absent from
// activeNotifications must stay in the buffer. Its commit may yet arrive, and
// dispatching it would take the unknown-handle path and schedule an orphan
// delete against a handle we may be about to own. endSubscribe's discard is
// what clears it, not replay.
func TestReplayEarlySamples_LeavesUnboundHandleParked(t *testing.T) {
	sess := newNotifTestSession()
	sess.lifecycle.ctx, sess.lifecycle.shutdown = context.WithCancel(context.Background())
	defer sess.lifecycle.shutdown()
	// Keep the reaper out: it needs a client, which this session has none of.
	sess.beginSubscribe()

	sess.bufferEarlySample(context.Background(), 0x42, 0, intSample(9))
	if got := earlySampleCount(sess); got != 1 {
		t.Fatalf("buffered samples = %d, want 1", got)
	}

	sess.replayEarlySamples(context.Background(), []uint32{0x42})

	if got := earlySampleCount(sess); got != 1 {
		t.Errorf("buffered samples after replaying an unbound handle = %d, want 1 (must stay parked)", got)
	}
}

// TestBufferEarlySample_SelfHealsWhenCommitLandedFirst is the regression for the
// lost static-symbol sample. dispatchSample releases notifications.lock before
// it buffers, so the commit and its replay can both complete in that gap —
// leaving the sample parked after the only replay that would have collected it.
// Buffering therefore re-checks, and delivers the sample itself if the handle is
// bound by then. Constructed deterministically: the handle is already bound when
// bufferEarlySample runs, which is exactly the state that interleaving produces.
func TestBufferEarlySample_SelfHealsWhenCommitLandedFirst(t *testing.T) {
	sess := newNotifTestSession()
	sess.lifecycle.ctx, sess.lifecycle.shutdown = context.WithCancel(context.Background())
	defer sess.lifecycle.shutdown()
	sess.beginSubscribe()

	const handle = 0x99
	sym := preSeedTypedSymbol(sess, "MAIN.sStaticName", 0xC0DE)
	ch := make(chan *Update, 2)
	sess.notifications.lock.Lock()
	sess.notifications.activeNotifications[handle] = activeNotification{Sym: sym, Ch: ch}
	sess.notifications.lock.Unlock()

	sess.bufferEarlySample(context.Background(), handle, 0, intSample(1234))

	select {
	case u := <-ch:
		if u.Value != int16(1234) {
			t.Errorf("Update.Value = %#v, want int16(1234)", u.Value)
		}
		if u.Symbol != "MAIN.sStaticName" {
			t.Errorf("Update.Symbol = %q, want %q", u.Symbol, "MAIN.sStaticName")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("sample never delivered: buffering did not notice the handle was already bound")
	}
	if got := earlySampleCount(sess); got != 0 {
		t.Errorf("buffered samples = %d, want 0 (the sample must be consumed, not left parked)", got)
	}
}

// TestEndSubscribe_DiscardIsAtomicWithDecrement: a subscribe that begins while
// another is finishing must keep its buffered sample. The decrement and the
// discard have to be one critical section, or the newcomer's sample is wiped by
// the outgoing call.
func TestEndSubscribe_DiscardIsAtomicWithDecrement(t *testing.T) {
	sess := newNotifTestSession()
	ctx := context.Background()

	first := sess.beginSubscribe()
	second := sess.beginSubscribe()
	sess.bufferEarlySample(ctx, 0x1, 0, intSample(1))

	// First subscribe finishes: still one in flight, so nothing may be dropped.
	sess.endSubscribe(ctx, first, nil)
	if got := earlySampleCount(sess); got != 1 {
		t.Fatalf("buffered samples = %d, want 1 while a subscribe is still in flight", got)
	}

	// Last one finishes with nothing committed: now the parked sample goes.
	sess.endSubscribe(ctx, second, nil)
	if got := earlySampleCount(sess); got != 0 {
		t.Errorf("buffered samples = %d, want 0 after the last subscribe finished", got)
	}
	if n := sess.notifications.subscribeInFlight.Load(); n != 0 {
		t.Errorf("subscribeInFlight = %d, want 0", n)
	}
}

// TestEndSubscribe_DiscardIsAtomicWithDecrement_Concurrent is the atomicity half
// the test above cannot reach: single-threaded, it only shows the count is
// honoured. An outgoing endSubscribe runs on one goroutine while a newcomer opens
// its own window and parks a sample on another; closing the subscribe and
// deciding what may be discarded have to be one critical section, or the outgoing
// call sees nothing in flight after the newcomer has already buffered and wipes
// its sample.
//
// The ordering is forced rather than raced. An earlier version released both
// goroutines together and asserted the same thing — but the outgoing side only
// had to reach a mutex while the newcomer had to open a window AND allocate,
// copy and insert a sample, so the outgoing side won essentially always, and when
// it wins the buffer is still empty and every assertion holds for free. It passed
// with the critical section split three different ways (measured, 15k rounds).
// Sequencing it means one round is enough to kill a split.
func TestEndSubscribe_DiscardIsAtomicWithDecrement_Concurrent(t *testing.T) {
	ctx := context.Background()

	sess := newNotifTestSession()
	outgoing := sess.beginSubscribe()

	buffered := make(chan struct{})
	var newcomer subscribeToken
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		// Only start closing once the newcomer's sample is parked — the state in
		// which discarding it would be a bug.
		<-buffered
		sess.endSubscribe(ctx, outgoing, nil)
	}()
	go func() {
		defer wg.Done()
		newcomer = sess.beginSubscribe()
		sess.bufferEarlySample(ctx, 0x2, 0, intSample(7))
		close(buffered)
	}()
	wg.Wait()

	// The newcomer's window is still open, so its sample must still be there.
	if got := earlySampleCount(sess); got != 1 {
		t.Fatalf("buffered samples = %d, want 1 — the outgoing endSubscribe discarded a sample belonging to a window that was still open", got)
	}
	sess.endSubscribe(ctx, newcomer, nil)
	if got := earlySampleCount(sess); got != 0 {
		t.Errorf("buffered samples = %d, want 0 once nothing is in flight", got)
	}
	if n := sess.notifications.subscribeInFlight.Load(); n != 0 {
		t.Errorf("subscribeInFlight = %d, want 0", n)
	}
}

// TestBufferEarlySample_BoundEnforced: the buffer is capped so a flood of
// unknown handles during a subscribe cannot grow it without limit.
func TestBufferEarlySample_BoundEnforced(t *testing.T) {
	sess := newNotifTestSession()
	sess.beginSubscribe()

	for i := 0; i < earlySampleMaxHandles+16; i++ {
		sess.bufferEarlySample(context.Background(), uint32(i), 0, intSample(uint16(i)))
	}
	if got := earlySampleCount(sess); got != earlySampleMaxHandles {
		t.Errorf("buffered samples = %d, want %d (cap)", got, earlySampleMaxHandles)
	}

	// A handle already in the buffer is still updatable at the cap.
	sess.bufferEarlySample(context.Background(), 0, 0, intSample(0xBEEF))
	sess.notifications.earlyMu.Lock()
	s, ok := sess.notifications.earlySamples[0]
	sess.notifications.earlyMu.Unlock()
	if !ok {
		t.Fatal("handle 0 missing from buffer")
	}
	if v := binary.LittleEndian.Uint16(s.content); v != 0xBEEF {
		t.Errorf("buffered content = 0x%X, want 0xBEEF (latest sample wins)", v)
	}
}

// TestBufferEarlySample_CopiesContent: the buffer outlives the dispatch call,
// so it must not alias the caller's slice.
func TestBufferEarlySample_CopiesContent(t *testing.T) {
	sess := newNotifTestSession()

	content := intSample(1)
	sess.bufferEarlySample(context.Background(), 0x11, 0, content)
	binary.LittleEndian.PutUint16(content, 0xFFFF)

	sess.notifications.earlyMu.Lock()
	s := sess.notifications.earlySamples[0x11]
	sess.notifications.earlyMu.Unlock()
	if v := binary.LittleEndian.Uint16(s.content); v != 1 {
		t.Errorf("buffered content = %d, want 1 (buffer aliases caller slice)", v)
	}
}

// TestSubscribeRaceActive covers the two independent triggers and the negative
// case. Cases go through beginSubscribe rather than poking the counter, so a case
// that claims a subscribe is open has one that really is: an out-of-band counter
// raise used to reach a fail-open branch that returned true unconditionally, and
// the table pinned that as if it were the spec.
func TestSubscribeRaceActive(t *testing.T) {
	tests := []struct {
		name      string
		openAt    *time.Duration // when an open subscribe began, nil = none open
		lastSubAt time.Duration  // relative to now, used only when nothing is open
		want      bool
	}{
		{name: "in flight, recently opened", openAt: durPtr(-time.Second), want: true},
		{name: "in flight, opened just inside the cap", openAt: durPtr(-subscribeRaceMaxOpen + 5*time.Second), want: true},
		{name: "in flight, wedged past the cap", openAt: durPtr(-subscribeRaceMaxOpen - time.Second), want: false},
		{name: "idle, recent subscribe", lastSubAt: 0, want: true},
		{name: "idle, stale subscribe", lastSubAt: -time.Second, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sess := newNotifTestSession()
			if tt.openAt != nil {
				tok := sess.beginSubscribe()
				ageOpenSubscribe(sess, tok, time.Now().Add(*tt.openAt))
			}
			sess.notifications.lastSubscribeNs.Store(time.Now().Add(tt.lastSubAt).UnixNano())
			if got := sess.subscribeRaceActive(); got != tt.want {
				t.Errorf("subscribeRaceActive() = %v, want %v", got, tt.want)
			}
		})
	}
}

func durPtr(d time.Duration) *time.Duration { return &d }

// TestSubscribeRaceActive_WedgedSubscribeExpires pins both directions of the cap,
// which is why per-subscribe start times are tracked instead of one timestamp.
//
// A wedged subscribe alone must NOT hold the window open: measured from
// lastSubscribeNs — refreshed by both begin and end — the deadline was pushed
// forward forever by ordinary traffic and the reaper stayed off for the life of
// the session, which is what fills the PLC handle table.
//
// A young subscribe running alongside a wedged one must hold it OPEN: judged on
// the oldest open subscribe instead, the window closed while a freshly started
// subscribe was still registering, and its first sample took the unknown-handle
// path — not buffered, logged as a leak, an orphan delete scheduled. For a static
// symbol that is the only sample the tag will ever emit.
func TestSubscribeRaceActive_WedgedSubscribeExpires(t *testing.T) {
	sess := newNotifTestSession()

	wedged := sess.beginSubscribe()
	if !sess.subscribeRaceActive() {
		t.Fatal("subscribeRaceActive() = false with a subscribe just opened")
	}

	// Age it past the cap.
	ageOpenSubscribe(sess, wedged, time.Now().Add(-subscribeRaceMaxOpen-time.Second))
	if sess.subscribeRaceActive() {
		t.Error("subscribeRaceActive() = true for a subscribe wedged past subscribeRaceMaxOpen — the reaper is disabled indefinitely")
	}

	// A short subscribe running alongside it keeps the window open on its own
	// merit, then closes — and must not have extended the wedged one's life.
	healthy := sess.beginSubscribe()
	if !sess.subscribeRaceActive() {
		t.Error("window closed while a freshly opened subscribe was in flight")
	}
	sess.endSubscribe(context.Background(), healthy, nil)
	if sess.subscribeRaceActive() {
		t.Error("a later subscribe pushed the wedged one's deadline out again")
	}

	// Once the wedged call finally returns, nothing is tracked and a fresh
	// subscribe starts from its own clock.
	sess.endSubscribe(context.Background(), wedged, nil)
	if _, open := sess.notifications.newestOpenSubscribe(); open {
		t.Error("a subscribe is still tracked after every one closed")
	}
	sess.beginSubscribe()
	if !sess.subscribeRaceActive() {
		t.Error("a fresh subscribe inherited the wedged one's expired clock")
	}
}

// TestEndSubscribe_NestedSubscribesKeepWindowOpen: two concurrent subscribes
// must not have the first one to finish tear down the shared buffer while the
// second is still registering handles.
func TestEndSubscribe_NestedSubscribesKeepWindowOpen(t *testing.T) {
	sess := newNotifTestSession()
	ctx := context.Background()

	first := sess.beginSubscribe()
	second := sess.beginSubscribe()
	sess.bufferEarlySample(context.Background(), 0xABC, 0, intSample(3))

	sess.endSubscribe(ctx, first, nil)
	if got := earlySampleCount(sess); got != 1 {
		t.Errorf("buffered samples with one subscribe still in flight = %d, want 1", got)
	}
	if !sess.subscribeRaceActive() {
		t.Error("subscribeRaceActive() = false while a subscribe is still in flight")
	}

	sess.endSubscribe(ctx, second, nil)
	if got := earlySampleCount(sess); got != 0 {
		t.Errorf("buffered samples after last subscribe finished = %d, want 0", got)
	}
	if n := sess.notifications.subscribeInFlight.Load(); n != 0 {
		t.Errorf("subscribeInFlight = %d, want 0", n)
	}
}

// TestBufferEarlySample_ByteBudgetEnforced: the handle cap alone does not bound
// memory. A sample is network-sized — struct and array symbols run to tens of KB
// — so the buffer also has to stop counting bytes, and replacing an entry must
// release the bytes it held rather than leaking them.
func TestBufferEarlySample_ByteBudgetEnforced(t *testing.T) {
	sess := newNotifTestSession()
	sess.beginSubscribe()

	big := make([]byte, 1<<20) // 1 MiB per sample
	for i := 0; i < (earlySampleMaxBytes/len(big))+4; i++ {
		sess.bufferEarlySample(context.Background(), uint32(i), 0, big)
	}

	sess.notifications.earlyMu.Lock()
	held := sess.notifications.earlyBytes
	entries := len(sess.notifications.earlySamples)
	sess.notifications.earlyMu.Unlock()

	if held > earlySampleMaxBytes {
		t.Errorf("held %d bytes, over the %d budget", held, earlySampleMaxBytes)
	}
	if entries == 0 {
		t.Error("budget rejected everything; it should admit samples up to the cap")
	}

	// Replacing an entry must not double-count: same handle, same size, so the
	// total has to stay put.
	before := held
	sess.bufferEarlySample(context.Background(), 0, 0, big)
	sess.notifications.earlyMu.Lock()
	after := sess.notifications.earlyBytes
	sess.notifications.earlyMu.Unlock()
	if after != before {
		t.Errorf("byte count moved from %d to %d when replacing an entry of equal size", before, after)
	}
}

// TestReplayEarlySamples_ReleasesBytes: taking a sample out of the buffer has to
// give its bytes back, or a long-lived session leaks the budget one replay at a
// time until nothing can be buffered.
func TestReplayEarlySamples_ReleasesBytes(t *testing.T) {
	sess := newNotifTestSession()
	sess.beginSubscribe()

	const handle = 0x555
	sym := preSeedTypedSymbol(sess, "MAIN.x", 0xC0DE)
	ch := make(chan *Update, 1)
	sess.notifications.lock.Lock()
	sess.notifications.activeNotifications[handle] = activeNotification{Sym: sym, Ch: ch}
	sess.notifications.lock.Unlock()

	// bufferEarlySample self-heals when the handle is already bound, so the
	// sample is consumed and its bytes must be released with it.
	sess.bufferEarlySample(context.Background(), handle, 0, intSample(7))

	sess.notifications.earlyMu.Lock()
	held := sess.notifications.earlyBytes
	sess.notifications.earlyMu.Unlock()
	if held != 0 {
		t.Errorf("earlyBytes = %d after the sample was consumed, want 0", held)
	}
}

// TestAddSymbolNotification_ChannelMismatchRejected pre-seeds a notifications
// channel, then calls Subscribe with a DIFFERENT channel. The
// pre-check inside Subscribe (under notifications.lock, BEFORE
// any PLC roundtrip) rejects with an error.
//
// Validates: R-NOT-001 (single channel per Connection).
func TestAddSymbolNotification_ChannelMismatchRejected(t *testing.T) {
	sess := newNotifTestSession()
	preSeedSymbol(sess, "MAIN.x")
	preSeedSymbol(sess, "MAIN.y")

	// Pre-set the notifications channel to chA.
	chA := make(chan *Update, 1)
	sess.notifications.lock.Lock()
	sess.notifications.notificationChannel = chA
	sess.notifications.lock.Unlock()

	// Attempt Subscribe with chB ≠ chA.
	chB := make(chan *Update, 1)
	_, err := sess.Subscribe(context.Background(), "MAIN.x", 0, 0, ams.TransModeServerOnChange, chB)
	if err == nil {
		t.Fatal("Subscribe with mismatched channel: err = nil, want error")
	}
	if !strings.Contains(err.Error(), "same updateReceiver channel") {
		t.Errorf("err = %v, want channel-mismatch message", err)
	}
}

// TestAddSymbolNotifications_DuplicateRejected exercises three paths
// (single-call duplicate, in-batch duplicate, cross-batch duplicate) at
// the pre-check stage that does NOT require a working Client. The
// production code marks the duplicate result.Skipped and continues.
//
// (a) Cross-batch — pre-existing config rejects the second.
// (b) In-batch — same name twice in one configs[] slice.
// (c) Single-call — direct Subscribe after a prior one is
//
//	covered by R-NOT-001 (channel-mismatch) elsewhere; here we use
//	the pre-existing-config pre-check inside SubscribeAll.
//
// Validates: R-NOT-002 (duplicate-symbol rejected).
func TestAddSymbolNotifications_DuplicateRejected(t *testing.T) {
	t.Run("cross_batch_existing", func(t *testing.T) {
		sess := newNotifTestSession()

		ch := make(chan *Update, 1)
		// Pre-stage one LIVE subscription so the pre-check rejects the second batch.
		seedLiveNotification(sess, "MAIN.x", 0x1001, ch)

		results, err := sess.subscribeAll(context.Background(), []NotificationConfig{
			{Symbol: "MAIN.x", Mode: ams.TransModeServerOnChange},
		}, ch)
		if err != nil {
			t.Fatalf("SubscribeAll: %v", err)
		}
		if len(results) != 1 {
			t.Fatalf("results len = %d, want 1", len(results))
		}
		if results[0].Skipped == nil {
			t.Errorf("Skipped = nil, want duplicate-rejection error")
		} else if !strings.Contains(results[0].Skipped.Error(), "already subscribed") {
			t.Errorf("Skipped = %v, want 'already subscribed'", results[0].Skipped)
		}
	})

	t.Run("in_batch", func(t *testing.T) {
		sess := newNotifTestSession()

		ch := make(chan *Update, 1)
		// Pre-seed a LIVE subscription so the FIRST entry is rejected as
		// already-subscribed; the second entry hits the in-batch dup
		// branch. Both are Skipped at the pre-check stage so requests[]
		// stays empty and SumAddDeviceNotification is not invoked
		// (nil client would panic).
		seedLiveNotification(sess, "MAIN.dup", 0x1002, ch)

		results, err := sess.subscribeAll(context.Background(), []NotificationConfig{
			{Symbol: "MAIN.dup", Mode: ams.TransModeServerOnChange},
			{Symbol: "MAIN.dup", Mode: ams.TransModeServerOnChange},
		}, ch)
		if err != nil {
			t.Fatalf("SubscribeAll: %v", err)
		}
		if len(results) != 2 {
			t.Fatalf("results len = %d, want 2", len(results))
		}
		// Both entries Skipped: first by existing-config, second by either
		// existing-config or in-batch-dup. Spec asks the in-batch path to
		// be flagged; the production code currently flags it as
		// "already subscribed" because the existing pre-check fires first.
		// Either branch yields a non-nil Skipped — that is the user-
		// observable contract.
		for i, r := range results {
			if r.Skipped == nil {
				t.Errorf("results[%d].Skipped = nil, want duplicate rejection", i)
			}
		}
	})

	t.Run("channel_mismatch", func(t *testing.T) {
		sess := newNotifTestSession()
		preSeedSymbol(sess, "MAIN.x")

		// Pre-set the channel.
		chA := make(chan *Update, 1)
		sess.notifications.lock.Lock()
		sess.notifications.notificationChannel = chA
		sess.notifications.lock.Unlock()

		chB := make(chan *Update, 1)
		_, err := sess.subscribeAll(context.Background(), []NotificationConfig{
			{Symbol: "MAIN.x", Mode: ams.TransModeServerOnChange},
		}, chB)
		if err == nil {
			t.Errorf("SubscribeAll with mismatched channel: err = nil, want error")
		}
	})
}

// TestAddSymbolNotification_StrandedSymbol_DetectedByEpoch drives the
// post-roundtrip stranded-symbol detection in Subscribe
// (subscribe.go). Two production branches detect strands:
//
//	(a) fresh == nil: cache.symbols no longer contains the key after roundtrip.
//	    Returns "removed from cache during subscribe (likely online change
//	    or LoadSymbols)" and releases the just-acquired PLC handle.
//
//	(b) epoch != cacheGen: another reload landed AFTER the post-roundtrip
//	    cache.lock release but BEFORE notifications.lock acquire (the
//	    residual race window). Returns "stranded by concurrent cache reload
//	    during subscribe" and releases the handle.
//
// This test exercises (a), the deterministic vanish path: pre-seed cache,
// kick off Subscribe, and during the in-flight roundtrip
// delete the symbol from cache + bumpEpoch (mimicking loadSymbols). After
// the network roundtrip, fresh is nil → branch (a) fires.
//
// Branch (b) is a narrow race window (between two specific lock release/
// acquire points) and is not deterministically reproducible from a test;
// branch (a) fully exercises the orphan-handle release path that R-NOT-004
// guards.
//
// Validates: R-NOT-004 (post-roundtrip stranded-symbol detected; handle released).
func TestAddSymbolNotification_StrandedSymbol_DetectedByEpoch(t *testing.T) {
	srv := fakeplc.StartPLC(t)
	defer srv.Stop()

	const fakeHandle uint32 = 0xBEEF0001

	srv.OnAddDeviceNotification(func(_ fakeplc.AddNotifRequest) fakeplc.AddNotifResponse {
		return fakeplc.AddNotifResponse{Handle: fakeHandle, Error: ams.ReturnCodeNoErrors}
	})
	// 100ms server-side delay gives the test goroutine time to bump epoch
	// + delete the symbol before the response is sent.
	srv.DelayBefore(ams.CommandAddDeviceNotification, 0, 100*time.Millisecond)

	var deletes atomic.Int32
	srv.OnDeleteDeviceNotification(func(h uint32) ams.ReturnCode {
		if h == fakeHandle {
			deletes.Add(1)
		}
		return ams.ReturnCodeNoErrors
	})

	sess, _ := newWiredTestSession(t, srv)
	preSeedSymbol(sess, "MAIN.x")

	ch := make(chan *Update, 1)
	addErr := make(chan error, 1)
	go func() {
		_, err := sess.Subscribe(context.Background(), "MAIN.x", 0, 0, ams.TransModeServerOnChange, ch)
		addErr <- err
	}()

	// Mid-roundtrip: simulate loadSymbols swap by deleting MAIN.x and
	// bumping the epoch. After the response returns, the post-roundtrip
	// re-fetch finds nil → fresh==nil branch fires.
	time.Sleep(30 * time.Millisecond)
	sess.cache.lock.Lock()
	delete(sess.cache.symbols, symtab.Key("MAIN.x"))
	sess.bumpEpoch()
	sess.cache.lock.Unlock()

	select {
	case err := <-addErr:
		if err == nil {
			t.Fatal("Subscribe: err = nil, want vanished-cache error")
		}
		// Accept either branch's wording; both are valid R-NOT-004 outcomes.
		if !strings.Contains(err.Error(), "removed from cache") &&
			!strings.Contains(err.Error(), "stranded by concurrent cache reload") {
			t.Errorf("Subscribe err = %v, want 'removed from cache' OR 'stranded by concurrent cache reload'", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Subscribe: timeout (>5s)")
	}

	// Production releases the orphaned PLC handle. Wait briefly for the
	// async-issued DeleteDeviceNotification to land.
	deadline := time.Now().Add(2 * time.Second)
	for deletes.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := deletes.Load(); got < 1 {
		t.Errorf("DeleteDeviceNotification calls = %d, want at least 1 (handle release after stranding)", got)
	}

	// activeNotifications must NOT contain the stranded handle.
	sess.notifications.lock.Lock()
	_, present := sess.notifications.activeNotifications[fakeHandle]
	sess.notifications.lock.Unlock()
	if present {
		t.Errorf("activeNotifications still contains stranded handle 0x%X", fakeHandle)
	}
}

// TestAddSymbolNotification_TOCTOURecheck drives the post-roundtrip duplicate
// re-check (R-NOT-003) in Subscribe. Two concurrent calls for the
// same symbol must both pass the pre-check but only one can commit; the loser
// observes the duplicate-already-subscribed re-check and returns an error
// after releasing the just-acquired PLC handle.
//
// Strategy: server adds 100ms delay before each AddDeviceNotification response.
// Both goroutines enter the roundtrip, both pass the pre-check (no existing
// config), the PLC issues both handles. The first to reach the post-roundtrip
// check commits; the second re-check finds the duplicate and rejects.
//
// Validates: R-NOT-003 (TOCTOU re-check after PLC roundtrip).
func TestAddSymbolNotification_TOCTOURecheck(t *testing.T) {
	srv := fakeplc.StartPLC(t)
	defer srv.Stop()

	var nextHandle atomic.Uint32
	nextHandle.Store(0xAB000001)
	srv.OnAddDeviceNotification(func(_ fakeplc.AddNotifRequest) fakeplc.AddNotifResponse {
		h := nextHandle.Add(1) - 1
		return fakeplc.AddNotifResponse{Handle: h, Error: ams.ReturnCodeNoErrors}
	})
	srv.DelayBefore(ams.CommandAddDeviceNotification, 0, 100*time.Millisecond)

	var deletes atomic.Int32
	srv.OnDeleteDeviceNotification(func(_ uint32) ams.ReturnCode {
		deletes.Add(1)
		return ams.ReturnCodeNoErrors
	})

	sess, _ := newWiredTestSession(t, srv)
	preSeedSymbol(sess, "MAIN.x")

	ch := make(chan *Update, 4)
	type result struct {
		handle uint32
		err    error
	}
	resCh := make(chan result, 2)
	for i := 0; i < 2; i++ {
		go func() {
			h, err := sess.Subscribe(context.Background(), "MAIN.x", 0, 0, ams.TransModeServerOnChange, ch)
			resCh <- result{handle: h, err: err}
		}()
	}

	res := make([]result, 2)
	for i := 0; i < 2; i++ {
		select {
		case r := <-resCh:
			res[i] = r
		case <-time.After(5 * time.Second):
			t.Fatalf("Subscribe[%d] timeout", i)
		}
	}

	// One success, one duplicate-rejection error.
	successes, errors := 0, 0
	var errStr string
	for _, r := range res {
		if r.err == nil {
			successes++
		} else {
			errors++
			errStr = r.err.Error()
		}
	}
	if successes != 1 || errors != 1 {
		t.Fatalf("results: successes=%d errors=%d (want 1/1); res=%+v", successes, errors, res)
	}
	if !strings.Contains(errStr, "already has an active notification") {
		t.Errorf("loser err = %q, want 'already has an active notification'", errStr)
	}
	// Loser must release its just-acquired PLC handle.
	deadline := time.Now().Add(2 * time.Second)
	for deletes.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := deletes.Load(); got < 1 {
		t.Errorf("DeleteDeviceNotification calls = %d, want at least 1 (loser releases handle)", got)
	}

	// Exactly one entry in activeNotifications.
	sess.notifications.lock.Lock()
	got := len(sess.notifications.activeNotifications)
	sess.notifications.lock.Unlock()
	if got != 1 {
		t.Errorf("activeNotifications size = %d, want 1", got)
	}
}

// TestNotificationChannel_SetOnFirstSuccess pins the invariant that
// notificationChannel is set ONLY on first successful subscribe. The
// production code at subscribe.go sets `notificationChannel = ch`
// only when `successes > 0`. With no successes (all-Skipped batch), the
// field MUST stay nil.
//
// We drive this by passing an empty configs slice and an all-duplicate
// batch — both result in 0 successes, channel must stay nil.
//
// Validates: R-NOT-010 (channel set only on first success).
func TestNotificationChannel_SetOnFirstSuccess(t *testing.T) {
	t.Run("empty_configs_no_change", func(t *testing.T) {
		sess := newNotifTestSession()
		ch := make(chan *Update, 1)

		_, err := sess.subscribeAll(context.Background(), nil, ch)
		if err != nil {
			t.Fatalf("SubscribeAll nil configs: %v", err)
		}
		sess.notifications.lock.Lock()
		got := sess.notifications.notificationChannel
		sess.notifications.lock.Unlock()
		if got != nil {
			t.Errorf("notificationChannel = %v, want nil after empty batch", got)
		}
	})

	t.Run("all_skipped_no_channel_set", func(t *testing.T) {
		sess := newNotifTestSession()

		// Pre-stage a live subscription so every config is rejected as duplicate.
		// Its channel is not the one under test below.
		seedLiveNotification(sess, "MAIN.dup", 0x1003, make(chan *Update, 1))
		ch := make(chan *Update, 1)

		_, _ = sess.subscribeAll(context.Background(), []NotificationConfig{
			{Symbol: "MAIN.dup", Mode: ams.TransModeServerOnChange},
		}, ch)
		// Channel was never set by SubscribeAll since all entries
		// were Skipped pre-flight (no roundtrip even occurred — len(requests)==0
		// short-circuits). Confirm nil.
		sess.notifications.lock.Lock()
		got := sess.notifications.notificationChannel
		sess.notifications.lock.Unlock()
		if got != nil {
			t.Errorf("notificationChannel = %v, want nil — no successful subscribes", got)
		}
	})

	t.Run("concurrent_calls_no_channel_race", func(t *testing.T) {
		sess := newNotifTestSession()

		// Pre-set channel + pre-stage a LIVE subscription so every
		// concurrent SubscribeAll call results in all-Skipped
		// (no PLC roundtrip needed). The race detector watches the
		// notificationChannel field for torn writes during the
		// concurrent pre-checks.
		ch := make(chan *Update, 1)
		seedLiveNotification(sess, "MAIN.x", 0x1004, ch)
		sess.notifications.lock.Lock()
		sess.notifications.notificationChannel = ch
		sess.notifications.lock.Unlock()

		var wg sync.WaitGroup
		for i := 0; i < 20; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, _ = sess.subscribeAll(context.Background(), []NotificationConfig{
					{Symbol: "MAIN.x", Mode: ams.TransModeServerOnChange},
				}, ch)
			}()
		}
		wg.Wait()

		sess.notifications.lock.Lock()
		got := sess.notifications.notificationChannel
		sess.notifications.lock.Unlock()
		if got != ch {
			t.Errorf("notificationChannel changed under concurrent calls: got %v, want %v", got, ch)
		}
	})
}

// TestSumNotificationResultTriState drives the production
// Session.SubscribeAll path through the scriptable PLC stub
// and asserts the three+TOCTOU classification of the
// SumNotificationResult struct returned to the caller:
//
//  1. success — Handle != 0, Error == NoErrors, Skipped == nil.
//  2. PLC error — Handle == 0, Error != NoErrors, Skipped == nil.
//  3. library skip (duplicate name in batch) — Skipped != nil.
//  4. TOCTOU loss (PLC accepted, library found stranded *symbol
//     post-roundtrip) — Skipped != nil, Handle may be non-zero so
//     caller must release.
//
// Validates: R-NOT-009 (per-config result contract) / R-SUM-004
// (sum-batch tri-state).
func TestSumNotificationResultTriState(t *testing.T) {
	srv := fakeplc.StartPLC(t)
	defer srv.Stop()
	sess, _ := newWiredTestSession(t, srv)

	// Three symbols cached up-front: x, y, z.
	for _, name := range []string{"MAIN.x", "MAIN.y", "MAIN.z"} {
		sess.cache.symbols[symtab.Key(name)] = &symtab.Symbol{
			FullName:    name,
			DataType:    "INT",
			Length:      2,
			Handle:      0xA1B2C3D4, // any non-zero handle so symbolSumAddress takes the handle path
			ContextMask: 0,
		}
	}

	// Sum-add response: per-item shape based on inbound count. Item 0
	// (x) succeeds with handle 0x1001. Item 1 (y) returns PLC error.
	// Item 2 (z) succeeds with handle 0x1003 — but the test mutates
	// the cache mid-handler so the post-roundtrip re-fetch finds the
	// orphan and reports Skipped+Handle (TOCTOU race).
	srv.OnWriteRead(ams.GroupSumupAddDeviceNotification, func(req []byte) []byte {
		// Mid-roundtrip: delete z from the cache so the post-roundtrip
		// re-resolve fails for that handle, triggering the TOCTOU branch.
		sess.cache.lock.Lock()
		delete(sess.cache.symbols, symtab.Key("MAIN.z"))
		sess.cache.lock.Unlock()
		return fakeplc.SumAddNotifPayload([]fakeplc.SumNotifResponse{
			{Handle: 0x1001, Error: ams.ReturnCodeNoErrors},
			{Handle: 0, Error: ams.ReturnCodeDeviceInvalidParam},
			{Handle: 0x1003, Error: ams.ReturnCodeNoErrors},
		})
	})
	// bestEffortDelete uses SumDelete for the orphan release.
	srv.OnWriteRead(ams.GroupSumupDeleteDeviceNotification, func(req []byte) []byte {
		nItems := len(req) / 4
		codes := make([]ams.ReturnCode, nItems)
		for i := range codes {
			codes[i] = ams.ReturnCodeNoErrors
		}
		return fakeplc.SumDeleteNotifPayload(codes)
	})

	ch := make(chan *Update, 4)
	configs := []NotificationConfig{
		{Symbol: "MAIN.x", Mode: ams.TransModeServerOnChange},
		// Library-skip case: duplicate name within the batch.
		{Symbol: "MAIN.x", Mode: ams.TransModeServerOnChange},
		{Symbol: "MAIN.y", Mode: ams.TransModeServerOnChange},
		{Symbol: "MAIN.z", Mode: ams.TransModeServerOnChange},
	}

	results, err := sess.subscribeAll(context.Background(), configs, ch)
	if err != nil {
		t.Fatalf("SubscribeAll: %v", err)
	}
	if len(results) != len(configs) {
		t.Fatalf("got %d results, want %d", len(results), len(configs))
	}

	// Assert: configs[0] success
	r0 := results[0]
	if r0.Skipped != nil || r0.Error != ams.ReturnCodeNoErrors || r0.Handle == 0 {
		t.Errorf("config[0] (success): got Handle=%d Error=%v Skipped=%v",
			r0.Handle, r0.Error, r0.Skipped)
	}
	// Assert: configs[1] library-skip duplicate (Skipped != nil)
	r1 := results[1]
	if r1.Skipped == nil {
		t.Errorf("config[1] (duplicate): Skipped should be non-nil; got %+v", r1)
	}
	// Assert: configs[2] PLC error (Skipped nil, Error != NoErrors, Handle == 0)
	r2 := results[2]
	if r2.Skipped != nil || r2.Error == ams.ReturnCodeNoErrors || r2.Handle != 0 {
		t.Errorf("config[2] (PLC error): got Handle=%d Error=%v Skipped=%v",
			r2.Handle, r2.Error, r2.Skipped)
	}
	// Assert: configs[3] TOCTOU loss (Skipped != nil, Handle non-zero from
	// the PLC because the cache vanished mid-roundtrip — caller MUST
	// release this handle on the PLC side via DeleteDeviceNotification).
	r3 := results[3]
	if r3.Skipped == nil {
		t.Errorf("config[3] (TOCTOU): Skipped should be non-nil; got %+v", r3)
	}
}

// TestAddSymbolNotification_DeclaredButNotLiveSymbolIsNotADuplicate: a symbol that
// is on file but has no live handle must still be subscribable.
//
// This is the other half of the nil-channel fix, and a defect in its own right.
// pending is the declared intent and legitimately outlives a handle — a resubscribe
// re-queues whatever the PLC refused for a retryable reason — but the duplicate
// check asked pending, not activeNotifications. So a re-queued entry answered
// "symbol already has an active notification" for a symbol with no notification at
// all, and DeleteDeviceNotification works by handle, so the caller had no way to
// clear it: the symbol was soft-locked for the life of the session.
//
// Retaining the intent (the test above) is what makes this state persist rather
// than being accidentally cleaned up, so the two must land together.
func TestAddSymbolNotification_DeclaredButNotLiveSymbolIsNotADuplicate(t *testing.T) {
	srv := fakeplc.StartPLC(t)
	defer srv.Stop()

	const fakeHandle uint32 = 0x22220002
	srv.OnAddDeviceNotification(func(_ fakeplc.AddNotifRequest) fakeplc.AddNotifResponse {
		return fakeplc.AddNotifResponse{Handle: fakeHandle, Error: ams.ReturnCodeNoErrors}
	})

	sess, _ := newWiredTestSession(t, srv)
	preSeedSymbol(sess, "MAIN.stranded")

	// On file, nothing live, no channel: the state a re-queued entry leaves.
	sess.notifications.lock.Lock()
	sess.notifications.addPending(pendingNotification{Config: NotificationConfig{Symbol: "MAIN.stranded"}})
	sess.notifications.lock.Unlock()

	ch := make(chan *Update, 1)
	h, err := sess.Subscribe(context.Background(), "MAIN.stranded", 0, 0, ams.TransModeServerOnChange, ch)
	if err != nil {
		t.Fatalf("Subscribe on a declared-but-not-live symbol: %v — the caller cannot clear a pending-only entry, so this is a permanently dead symbol", err)
	}
	if h != fakeHandle {
		t.Fatalf("handle = 0x%X, want 0x%X", h, fakeHandle)
	}

	sess.notifications.lock.Lock()
	pending := len(sess.notifications.pending)
	_, live := sess.notifications.activeNotifications[h]
	boundChannel := sess.notifications.notificationChannel
	sess.notifications.lock.Unlock()
	if !live {
		t.Error("the handle was never committed to activeNotifications")
	}
	// One entry, not two: configsByKey cannot hold a second entry for one symbol,
	// and a duplicated pending entry would make the next resubscribe register the
	// symbol twice on the PLC.
	if pending != 1 {
		t.Errorf("pending = %d after re-declaring a symbol already on file, want 1", pending)
	}
	if boundChannel != ch {
		t.Errorf("notificationChannel = %v, want the channel this subscribe supplied", boundChannel)
	}

	// And a genuine duplicate — the symbol now HAS a live handle — must still be
	// refused, or this fix has simply deleted the duplicate check.
	if _, err := sess.Subscribe(context.Background(), "MAIN.stranded", 0, 0, ams.TransModeServerOnChange, ch); err == nil {
		t.Error("a second subscribe of a LIVE symbol succeeded; duplicate detection is gone")
	}
}

const (
	staleTestStaleHandle uint32 = 0x1111
	staleTestFreshHandle uint32 = 0x2222
	staleTestNotifHandle uint32 = 0x9001
)

// seedStaleSymbol wires the scriptable stub for the TC3 runtime-restart case and
// returns a live Session plus the cached symbol carrying the dead handle.
//
// The stub answers every AddDeviceNotification that carries staleHandle with
// 0x710 and counts it, resolves the name to freshHandle and counts that, and
// REFUSES the symbol upload so a reload can never be what rescues the session —
// recovery has to come from the on-demand re-resolve. uploadInfoReads therefore
// counts reload attempts, which is how the no-storm assertions are made.
func seedStaleSymbol(t *testing.T, srv *fakeplc.PLC, handleLookups, staleAdds, uploadInfoReads *atomic.Int32, opts ...Option) (*Session, *symtab.Symbol) {
	t.Helper()
	srv.OnWriteRead(ams.GroupSymbolHandleByName, func(_ []byte) []byte {
		handleLookups.Add(1)
		return fakeplc.HandlePayload(staleTestFreshHandle)
	})
	srv.OnAddDeviceNotification(func(req fakeplc.AddNotifRequest) fakeplc.AddNotifResponse {
		if req.Offset == staleTestStaleHandle {
			staleAdds.Add(1)
			return fakeplc.AddNotifResponse{Error: ams.ReturnCodeDeviceSymbolNoFound}
		}
		return fakeplc.AddNotifResponse{Handle: staleTestNotifHandle}
	})
	srv.OnRead(ams.GroupSymbolUploadInfo, func(_, _, _ uint32) (ams.ReturnCode, []byte) {
		uploadInfoReads.Add(1)
		return ams.ReturnCodeDeviceError, nil
	})

	// The heartbeat is off so the only AddDeviceNotification traffic in the test is
	// the caller's: the internal beat subscribes on GroupSymbolVersion, which would
	// add a second registration and a second failure path to reason about here.
	sess, _ := newWiredTestSession(t, srv, append([]Option{WithoutNotificationHeartbeat()}, opts...)...)
	// Mirrors NewSession's defaults (session.go): the helper leaves them zero, and
	// maxReloadAttempts=0 means the cap is exhausted on the first attempt, which
	// would skip the invalidation for a reason that never happens in production.
	sess.maxReloadAttempts = 3
	sess.reloadWindow = 60 * time.Second

	sym := &symtab.Symbol{
		Name: "MAIN.a", FullName: "MAIN.a", DataType: "INT",
		Length: 2, Handle: staleTestStaleHandle, Valid: true,
	}
	sess.cache.lock.Lock()
	sess.cache.symbols[symtab.Key("MAIN.a")] = sym
	sess.cache.lock.Unlock()
	return sess, sym
}

// awaitHandleZeroed polls the cached handle until it is invalidated, bounded and
// short so a regression fails fast instead of hanging to the package timeout.
func awaitHandleZeroed(t *testing.T, sess *Session, sym *symtab.Symbol) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		sess.cache.lock.Lock()
		h := sym.Handle
		sess.cache.lock.Unlock()
		if h == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("cached handle is still 0x%X after a 0x710 on AddDeviceNotification: nothing invalidates it, "+
				"and since TC3 does not bump the symbol version every later subscribe repeats the same dead handle", h)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestAddSymbolNotification_SymbolNotFoundInvalidatesTheCachedHandle pins the
// TC3 runtime-restart case on the SINGLE-symbol subscribe path.
//
// Measured on hardware: after a TC3 restart the PLC refuses a cached symbol
// handle with 0x710 (symbol not found) and does NOT bump the symbol version. So
// nothing version-driven can save the session — not checkSymbolVersion, not the
// heartbeat watcher. The 0x710 on the subscribe itself is the only signal there
// is, and if it does not invalidate the handle, every later Subscribe
// repeats the same doomed request forever, with no callback and no Update.
//
// The wiring under test: AddDeviceNotification gets 0x710 → handleStaleDetection
// → AutoReload zeroes the cached handles → the next Subscribe sees
// Handle==0 and re-resolves via GetHandleByName.
//
// Detection only, no retry: subscribe is not idempotent, so the first caller
// after the restart still legitimately gets the error — the reload is async.
// Self-healing lands on the NEXT call. That two-call shape is the contract, and
// it is what this test asserts.
func TestAddSymbolNotification_SymbolNotFoundInvalidatesTheCachedHandle(t *testing.T) {
	srv := fakeplc.StartPLC(t)
	defer srv.Stop()

	var handleLookups, staleAdds, uploadInfoReads atomic.Int32
	sess, sym := seedStaleSymbol(t, srv, &handleLookups, &staleAdds, &uploadInfoReads)

	ctx := context.Background()
	ch := make(chan *Update, 1)

	_, err := sess.Subscribe(ctx, "MAIN.a", 0, time.Second, ams.TransModeServerOnChange, ch)
	if err == nil {
		t.Fatal("subscribe against the stale handle succeeded; the stub was supposed to refuse it with 0x710")
	}
	var rc ams.ReturnCode
	if !errors.As(err, &rc) || rc != ams.ReturnCodeDeviceSymbolNoFound {
		t.Fatalf("first subscribe error = %v, want one carrying %v", err, ams.ReturnCodeDeviceSymbolNoFound)
	}
	if got := staleAdds.Load(); got != 1 {
		t.Fatalf("subscribes against the stale handle = %d, want 1", got)
	}

	awaitHandleZeroed(t, sess, sym)

	// And the recovery has to be real: the next subscribe re-resolves and succeeds.
	handle, err := sess.Subscribe(ctx, "MAIN.a", 0, time.Second, ams.TransModeServerOnChange, ch)
	if err != nil {
		t.Fatalf("subscribe after invalidation: %v", err)
	}
	if handle != staleTestNotifHandle {
		t.Errorf("notification handle = 0x%X, want 0x%X", handle, staleTestNotifHandle)
	}
	if n := handleLookups.Load(); n != 1 {
		t.Errorf("GetHandleByName calls = %d, want 1 (the handle must be re-resolved exactly once)", n)
	}
	if n := staleAdds.Load(); n != 1 {
		t.Errorf("subscribes against the stale handle = %d, want 1 (the second one must carry the fresh handle)", n)
	}
	// No reload storm: N triggers collapse to one reload via the reloadInProgress
	// CAS, and the 3-per-60s cap bounds anything landing after it.
	if n := uploadInfoReads.Load(); n != 1 {
		t.Errorf("symbol-upload requests = %d, want 1 (one reload, not a storm)", n)
	}
}

// TestAddSymbolNotifications_SymbolNotFoundInvalidatesTheCachedHandle is the
// batch half of the same scenario. It is green before the single-symbol fix and
// documents the asymmetry that motivated it: a caller using the batch API
// recovered on its next call while a caller subscribing one symbol at a time was
// stuck forever. Kept alongside so a later refactor cannot level the two paths
// down instead of up.
func TestAddSymbolNotifications_SymbolNotFoundInvalidatesTheCachedHandle(t *testing.T) {
	srv := fakeplc.StartPLC(t)
	defer srv.Stop()

	var handleLookups, staleAdds, uploadInfoReads atomic.Int32
	sess, sym := seedStaleSymbol(t, srv, &handleLookups, &staleAdds, &uploadInfoReads)

	var sumAdds atomic.Int32
	srv.OnWriteRead(ams.GroupSumupAddDeviceNotification, func(_ []byte) []byte {
		if sumAdds.Add(1) == 1 {
			return fakeplc.SumAddNotifPayload([]fakeplc.SumNotifResponse{{Error: ams.ReturnCodeDeviceSymbolNoFound}})
		}
		return fakeplc.SumAddNotifPayload([]fakeplc.SumNotifResponse{{Handle: staleTestNotifHandle}})
	})

	ctx := context.Background()
	ch := make(chan *Update, 1)
	cfg := NotificationConfig{Symbol: "MAIN.a", CycleTime: time.Second, Mode: ams.TransModeServerOnChange}

	results, err := sess.subscribeAll(ctx, []NotificationConfig{cfg}, ch)
	if err != nil {
		t.Fatalf("batch subscribe: %v", err)
	}
	if len(results) != 1 || results[0].Error != ams.ReturnCodeDeviceSymbolNoFound {
		t.Fatalf("batch results = %+v, want one entry carrying %v", results, ams.ReturnCodeDeviceSymbolNoFound)
	}

	awaitHandleZeroed(t, sess, sym)

	results, err = sess.subscribeAll(ctx, []NotificationConfig{cfg}, ch)
	if err != nil {
		t.Fatalf("batch subscribe after invalidation: %v", err)
	}
	if len(results) != 1 || results[0].Error != ams.ReturnCodeNoErrors || results[0].Handle != staleTestNotifHandle {
		t.Fatalf("batch results after invalidation = %+v, want one committed entry with handle 0x%X", results, staleTestNotifHandle)
	}
	if n := handleLookups.Load(); n != 1 {
		t.Errorf("GetHandleByName calls = %d, want 1", n)
	}
	if n := uploadInfoReads.Load(); n != 1 {
		t.Errorf("symbol-upload requests = %d, want 1 (one reload, not a storm)", n)
	}
}

// TestAddSymbolNotification_SymbolNotFoundIgnoreKeepsTheCachedHandle guards the
// strategy contract, which is the easy thing to break when someone later
// "simplifies" the detection into an unconditional invalidation.
//
// SymbolVersionIgnore means the PLC error surfaces verbatim and the handles are
// left alone (samples get flagged Stale instead) — that is its documented
// meaning, not a bug. So: callback fires, handle unchanged, no reload.
func TestAddSymbolNotification_SymbolNotFoundIgnoreKeepsTheCachedHandle(t *testing.T) {
	srv := fakeplc.StartPLC(t)
	defer srv.Stop()

	reasons := make(chan Reason, 4)
	var handleLookups, staleAdds, uploadInfoReads atomic.Int32
	sess, sym := seedStaleSymbol(t, srv, &handleLookups, &staleAdds, &uploadInfoReads,
		WithSymbolVersionStrategy(SymbolVersionIgnore),
		WithOnSymbolVersionChanged(func(r Reason) { reasons <- r }))

	ch := make(chan *Update, 1)
	if _, err := sess.Subscribe(context.Background(), "MAIN.a", 0, time.Second, ams.TransModeServerOnChange, ch); err == nil {
		t.Fatal("subscribe against the stale handle succeeded; the stub was supposed to refuse it with 0x710")
	}

	// Wait on the callback rather than a sleep: it is the observable proof that
	// detection ran, so what follows is not a race against work not yet done.
	select {
	case got := <-reasons:
		if got != ReasonSymbolNotFound {
			t.Errorf("callback reason = %q, want %q", got, ReasonSymbolNotFound)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("SymbolVersionIgnore: OnSymbolVersionChanged never fired after a 0x710 subscribe")
	}

	sess.cache.lock.Lock()
	h := sym.Handle
	sess.cache.lock.Unlock()
	if h != staleTestStaleHandle {
		t.Errorf("cached handle = 0x%X, want it left at 0x%X: SymbolVersionIgnore must not invalidate", h, staleTestStaleHandle)
	}
	if n := uploadInfoReads.Load(); n != 0 {
		t.Errorf("symbol-upload requests = %d, want 0: SymbolVersionIgnore must not reload", n)
	}
	if n := handleLookups.Load(); n != 0 {
		t.Errorf("GetHandleByName calls = %d, want 0: nothing should be re-resolved under Ignore", n)
	}
}

// TestAddSymbolNotifications_StaleItemLogsAtWarnNotError: a per-item code that the
// library is about to recover from on its own belongs at Warn; one nobody can fix
// without a human belongs at Error.
//
// Measured on 192.168.3.118: restarting the TwinCAT runtime made a routine
// resubscribe emit 22 ERROR lines in one second, one per stale handle, for a
// condition that healed a second later — the same log-flood shape as the 1468-line
// episode this branch already fixed elsewhere. The per-item code reaches the caller
// in the results either way, so nothing is lost by demoting it.
//
// Asserts BOTH directions on purpose: a blanket demotion to Warn would satisfy the
// first half and quietly gag the codes an operator does need to see.
func TestAddSymbolNotifications_StaleItemLogsAtWarnNotError(t *testing.T) {
	cases := []struct {
		name      string
		code      ams.ReturnCode
		wantLevel slog.Level
		why       string
	}{
		{
			name:      "stale handle after a runtime restart",
			code:      ams.ReturnCodeDeviceSymbolNoFound, // 0x710, in the stale-detection set
			wantLevel: slog.LevelWarn,
			why:       "the library invalidates the handle and the next call re-resolves it",
		},
		{
			name:      "symbol version invalid",
			code:      ams.ReturnCodeDeviceSymbolVersionInvalid, // 0x711, also self-healing
			wantLevel: slog.LevelWarn,
			why:       "same: detection fires and the cache reloads",
		},
		{
			name:      "service not supported",
			code:      ams.ReturnCodeDeviceServiceNotSupported, // 0x701, not recoverable here
			wantLevel: slog.LevelError,
			why:       "nothing in the library can make this succeed; an operator has to look",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := fakeplc.StartPLC(t)
			defer srv.Stop()

			logs := &testlog.Handler{}
			var handleLookups, staleAdds, uploadInfoReads atomic.Int32
			sess, _ := seedStaleSymbol(t, srv, &handleLookups, &staleAdds, &uploadInfoReads,
				WithLogger(slog.New(logs)))

			srv.OnWriteRead(ams.GroupSumupAddDeviceNotification, func(_ []byte) []byte {
				return fakeplc.SumAddNotifPayload([]fakeplc.SumNotifResponse{{Error: tc.code}})
			})

			ch := make(chan *Update, 1)
			cfg := NotificationConfig{Symbol: "MAIN.a", CycleTime: time.Second, Mode: ams.TransModeServerOnChange}
			if _, err := sess.subscribeAll(context.Background(), []NotificationConfig{cfg}, ch); err != nil {
				t.Fatalf("batch subscribe: %v", err)
			}

			rec := logs.FindByMessage("error adding notification in batch")
			if rec == nil {
				rec = logs.FindByMessage("notification batch: item rejected")
			}
			if rec == nil {
				t.Fatalf("no per-item log record for code %v; the code still reaches the caller in the results, "+
					"but the operator-facing signal disappeared entirely", tc.code)
			}
			if rec.Level != tc.wantLevel {
				t.Errorf("per-item log for %v logged at %v, want %v — %s",
					tc.code, rec.Level, tc.wantLevel, tc.why)
			}
		})
	}
}

// release_cleanup_ctx_test.go — the context a batch uses to release PLC handles
// it declined to bind.
//
// The handles exist on the PLC regardless of what the caller's context is doing,
// and nothing else will ever clean them up: they are by definition absent from
// activeNotifications, so Close's releasePLCResources cannot see them either. A
// release that fails before it is sent leaks a subscription streaming to nobody
// until the PLC's route-idle timeout (~10 min), which is how the TwinCAT handle
// table fills up.

// TestReleaseCleanupCtx_PassesThroughLiveCaller: the common case must not add a
// timer or shorten the caller's own budget.
func TestReleaseCleanupCtx_PassesThroughLiveCaller(t *testing.T) {
	sess := newNotifTestSession()
	ctx := context.Background()

	got, cancel := sess.releaseCleanupCtx(ctx)
	defer cancel()
	if got != ctx {
		t.Error("a live caller context was replaced; the release should use it as-is")
	}
}

// TestReleaseCleanupCtx_ExpiredCallerGetsUsableCtx: the deadline the caller gave
// up on says nothing about whether the PLC still holds the handles.
func TestReleaseCleanupCtx_ExpiredCallerGetsUsableCtx(t *testing.T) {
	sess := newNotifTestSession()
	expired, cancelExpired := context.WithCancel(context.Background())
	cancelExpired()

	got, cancel := sess.releaseCleanupCtx(expired)
	defer cancel()
	if err := got.Err(); err != nil {
		t.Fatalf("replacement context is already done (%v) — the delete fails before it is sent", err)
	}
	if _, ok := got.Deadline(); !ok {
		t.Error("replacement context has no deadline; a closing session could block on the release")
	}
}

// TestReleaseCleanupCtx_SurvivesClosingSession is the shape that actually leaks.
// A batch aborts BECAUSE the session is shutting down, and the caller's context
// is derived from the lifecycle one (resubscribeNotifications passes it
// directly), so both are done. Deriving the replacement from a cancelled
// lifecycle context yields a context that is born dead — exactly the case this
// replacement exists for.
func TestReleaseCleanupCtx_SurvivesClosingSession(t *testing.T) {
	sess := newNotifTestSession()

	sess.lifecycle.ctxMu.Lock()
	lifeCtx, cancelLife := context.WithCancel(context.Background())
	sess.lifecycle.ctx, sess.lifecycle.shutdown = lifeCtx, cancelLife
	sess.lifecycle.ctxMu.Unlock()
	cancelLife() // session closing

	got, cancel := sess.releaseCleanupCtx(lifeCtx)
	defer cancel()
	if err := got.Err(); err != nil {
		t.Fatalf("release context inherited the closing session's cancellation (%v); the refused handles leak until route-idle timeout", err)
	}
	deadline, ok := got.Deadline()
	if !ok {
		t.Fatal("no deadline: a closing session must not block on cleanup")
	}
	if remaining := time.Until(deadline); remaining > notificationReleaseTimeout+time.Second {
		t.Errorf("deadline is %v away, want at most %v", remaining, notificationReleaseTimeout)
	}
}

// TestReleaseCleanupCtx_NilLifecycleCtxDoesNotPanic: Session struct literals with
// no lifecycle context are a shape this codebase already handles defensively
// (see tearDownAndReset's parent fallback). context.WithTimeout panics on a nil
// parent, so a batch reaching cleanup on such a session would take the process
// down.
func TestReleaseCleanupCtx_NilLifecycleCtxDoesNotPanic(t *testing.T) {
	sess := &Session{
		lifecycle:     &sessionLifecycle{closedCh: make(chan struct{})},
		notifications: &notificationManager{},
		logger:        slog.Default(),
	}
	expired, cancelExpired := context.WithCancel(context.Background())
	cancelExpired()

	got, cancel := sess.releaseCleanupCtx(expired)
	defer cancel()
	if got == nil {
		t.Fatal("nil context returned")
	}
	if err := got.Err(); err != nil {
		t.Errorf("replacement context is done (%v) on a session with no lifecycle context", err)
	}
}
