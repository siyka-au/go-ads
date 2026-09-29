package ads

import (
	"bytes"
	"context"
	"encoding/binary"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/siyka-au/go-ads/v3/internal/symtab"

	"github.com/siyka-au/go-ads/v3/internal/testlog"
)

// Verify that sending to a closed user channel does NOT panic the listen goroutine.
// Go runtime panics on send-to-closed-channel regardless of select default,
// so deliverNotification must guard with defer recover().
// Validates: R-NOT-006.
func TestDeliverNotification_ClosedChannelDoesNotPanic(t *testing.T) {
	ch := make(chan *Update, 1)
	close(ch)

	defer func() {
		if r := recover(); r != nil {
			t.Errorf("expected no panic, got recovered panic: %v", r)
		}
	}()

	conn := &Session{logger: slog.Default()}
	ctx := context.Background()
	update := &Update{Symbol: "x", Value: "1", Time: time.Now()}

	conn.deliverNotification(ctx, ch, update, 42, "x")
}

// Verify normal happy-path delivery on an open buffered channel.
// Validates: R-NOT-006.
func TestDeliverNotification_DeliversOnOpenChannel(t *testing.T) {
	ch := make(chan *Update, 1)
	conn := &Session{logger: slog.Default()}
	ctx := context.Background()
	update := &Update{Symbol: "x", Value: "1", Time: time.Now()}

	conn.deliverNotification(ctx, ch, update, 42, "x")

	select {
	case got := <-ch:
		if got != update {
			t.Errorf("got %v, want %v", got, update)
		}
	default:
		t.Errorf("update was not delivered to channel")
	}
}

// Verify drop on full buffered channel (default branch of select fires).
// Validates: R-NOT-006.
func TestDeliverNotification_DropsWhenChannelFull(t *testing.T) {
	ch := make(chan *Update, 1)
	ch <- &Update{Symbol: "filler"} // fill buffer

	defer func() {
		if r := recover(); r != nil {
			t.Errorf("expected no panic, got recovered panic: %v", r)
		}
	}()

	conn := &Session{logger: slog.Default()}
	ctx := context.Background()
	update := &Update{Symbol: "x", Value: "1", Time: time.Now()}

	conn.deliverNotification(ctx, ch, update, 42, "x")
	// Should drop without panic; channel still has only the filler.
	if len(ch) != 1 {
		t.Errorf("expected channel to keep filler, got len=%d", len(ch))
	}
}

// ==========================================================================
// DeviceNotification parsing — binary packet tests
// ==========================================================================

// Validates: R-NOT-005, R-NOT-012.
func TestDeviceNotification_SingleSample(t *testing.T) {
	conn := newTestConnection()
	defer conn.lifecycle.shutdown()

	// Register a notification for handle 42
	ch := make(chan *Update, 10)
	sym := &symtab.Symbol{
		FullName: "MAIN.testVar",
		DataType: "INT",
		Length:   2,
	}
	conn.notifications.activeNotifications[42] = activeNotification{Sym: sym, Ch: ch}
	conn.cache.symbols[symtab.Key(sym.FullName)] = sym

	// Build INT value = 1234
	data := make([]byte, 2)
	binary.LittleEndian.PutUint16(data, 1234)

	// Windows FILETIME for 2024-01-01 00:00:00 UTC
	// = (Unix epoch offset + unix timestamp) * ticks per second
	unixTS := int64(1704067200)                           // 2024-01-01 00:00:00 UTC
	filetime := uint64((unixTS + 11644473600) * 10000000) // seconds 1601->1970, 100ns ticks

	packet := buildNotificationPacket(42, filetime, data)
	err := conn.drivePacket(conn.lifecycle.ctx, packet)
	if err != nil {
		t.Fatalf("DeviceNotification error: %v", err)
	}

	select {
	case update := <-ch:
		if update.Symbol != "MAIN.testVar" {
			t.Errorf("variable = %q, want %q", update.Symbol, "MAIN.testVar")
		}
		if update.Value != int16(1234) {
			t.Errorf("value = %#v, want int16(1234)", update.Value)
		}
		// Verify timestamp is approximately correct (within a second)
		expectedTime := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
		if update.Time.Sub(expectedTime).Abs() > time.Second {
			t.Errorf("timestamp = %v, want ~%v", update.Time, expectedTime)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for notification")
	}
}

// Validates: R-NOT-007 (partial).
func TestDeviceNotification_UnknownHandle(t *testing.T) {
	conn := newTestConnection()
	defer conn.lifecycle.shutdown()

	// No notifications registered — handle 99 is unknown
	data := make([]byte, 2)
	binary.LittleEndian.PutUint16(data, 42)
	packet := buildNotificationPacket(99, 0, data)

	// Should not error, just log warning
	err := conn.drivePacket(conn.lifecycle.ctx, packet)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// Validates: R-NOT-007.
func TestDeviceNotification_UnknownHandleDuringClose(t *testing.T) {
	handler := &testlog.Handler{}
	conn := newTestConnection()
	conn.logger = slog.New(handler)
	conn.lifecycle.closedCh = make(chan struct{})
	defer conn.lifecycle.shutdown()

	// Mark connection as closed via the FSM (flag removed in Phase 3.b).
	conn.lifecycle.state.transitionTo(SessionStateClosed)
	close(conn.lifecycle.closedCh)

	data := make([]byte, 2)
	binary.LittleEndian.PutUint16(data, 42)
	packet := buildNotificationPacket(99, 0, data)

	err := conn.drivePacket(conn.lifecycle.ctx, packet)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// During Close(), stale notifications should be Debug, not Warn
	rec := handler.FindByMessage("received notification for deleted handle")
	if rec == nil {
		t.Fatal("expected debug log for stale notification during close")
	}
	if rec.Level != slog.LevelDebug {
		t.Errorf("expected Debug level during close, got %v", rec.Level)
	}
}

// Validates: R-NOT-007.
func TestDeviceNotification_UnknownHandleNormalCondition(t *testing.T) {
	handler := &testlog.Handler{}
	conn := newTestConnection()
	conn.logger = slog.New(handler)
	defer conn.lifecycle.shutdown()

	// No close, no recent reconnect — should be Warn
	data := make([]byte, 2)
	binary.LittleEndian.PutUint16(data, 42)
	packet := buildNotificationPacket(99, 0, data)

	err := conn.drivePacket(conn.lifecycle.ctx, packet)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	rec := handler.FindByMessage("received notification for unknown handle")
	if rec == nil {
		t.Fatal("expected warn log for unknown handle in normal conditions")
	}
	if rec.Level != slog.LevelWarn {
		t.Errorf("expected Warn level in normal conditions, got %v", rec.Level)
	}
}

// Validates: R-NOT-005 (partial).
func TestDeviceNotification_MultipleStampsAndSamples(t *testing.T) {
	conn := newTestConnection()
	defer conn.lifecycle.shutdown()

	ch := make(chan *Update, 10)
	sym1 := &symtab.Symbol{FullName: "var1", DataType: "BYTE", Length: 1}
	sym2 := &symtab.Symbol{FullName: "var2", DataType: "BYTE", Length: 1}
	conn.notifications.activeNotifications[1] = activeNotification{Sym: sym1, Ch: ch}
	conn.notifications.activeNotifications[2] = activeNotification{Sym: sym2, Ch: ch}
	conn.cache.symbols[symtab.Key(sym1.FullName)] = sym1
	conn.cache.symbols[symtab.Key(sym2.FullName)] = sym2

	stamps := []struct {
		timestamp uint64
		samples   []struct {
			handle uint32
			data   []byte
		}
	}{
		{
			timestamp: 0,
			samples: []struct {
				handle uint32
				data   []byte
			}{
				{1, []byte{10}},
				{2, []byte{20}},
			},
		},
		{
			timestamp: 0,
			samples: []struct {
				handle uint32
				data   []byte
			}{
				{1, []byte{30}},
			},
		},
	}

	packet := buildNotificationPacketMultiSample(stamps)
	err := conn.drivePacket(conn.lifecycle.ctx, packet)
	if err != nil {
		t.Fatalf("error: %v", err)
	}

	// Should receive 3 updates total
	var updates []*Update
	for i := 0; i < 3; i++ {
		select {
		case u := <-ch:
			updates = append(updates, u)
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out after %d updates", len(updates))
		}
	}
	if len(updates) != 3 {
		t.Errorf("expected 3 updates, got %d", len(updates))
	}
}

// Validates: R-CMD-007.
func TestDeviceNotification_EmptyPacket(t *testing.T) {
	conn := newTestConnection()
	defer conn.lifecycle.shutdown()

	// Too short — should return error
	err := conn.drivePacket(conn.lifecycle.ctx, []byte{1, 2, 3})
	if err == nil {
		t.Error("expected error for truncated packet")
	}
}

// Validates: NO-SPEC.
func TestDeviceNotification_ZeroStamps(t *testing.T) {
	conn := newTestConnection()
	defer conn.lifecycle.shutdown()

	// Valid header with 0 stamps
	buf := new(bytes.Buffer)
	binary.Write(buf, binary.LittleEndian, uint32(8)) // length
	binary.Write(buf, binary.LittleEndian, uint32(0)) // 0 stamps

	err := conn.drivePacket(conn.lifecycle.ctx, buf.Bytes())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// Validates: R-CMD-007 (sum-up).
func TestDeviceNotification_SampleSizeExceedsData(t *testing.T) {
	conn := newTestConnection()
	defer conn.lifecycle.shutdown()

	buf := new(bytes.Buffer)
	binary.Write(buf, binary.LittleEndian, uint32(100))      // length (fake)
	binary.Write(buf, binary.LittleEndian, uint32(1))        // 1 stamp
	binary.Write(buf, binary.LittleEndian, uint64(0))        // timestamp
	binary.Write(buf, binary.LittleEndian, uint32(1))        // 1 sample
	binary.Write(buf, binary.LittleEndian, uint32(42))       // handle
	binary.Write(buf, binary.LittleEndian, uint32(99999999)) // size > remaining

	err := conn.drivePacket(conn.lifecycle.ctx, buf.Bytes())
	if err == nil {
		t.Error("expected error for sample size exceeding data")
	}
}

// Validates: R-PARSE-007 (BOOL).
func TestDeviceNotification_BoolType(t *testing.T) {
	conn := newTestConnection()
	defer conn.lifecycle.shutdown()

	ch := make(chan *Update, 5)
	sym := &symtab.Symbol{FullName: "MAIN.bFlag", DataType: "BOOL", Length: 1}
	conn.notifications.activeNotifications[10] = activeNotification{Sym: sym, Ch: ch}
	conn.cache.symbols[symtab.Key(sym.FullName)] = sym

	packet := buildNotificationPacket(10, 0, []byte{1})
	err := conn.drivePacket(conn.lifecycle.ctx, packet)
	if err != nil {
		t.Fatalf("error: %v", err)
	}

	select {
	case u := <-ch:
		if u.Value != true {
			t.Errorf("BOOL value = %#v, want true", u.Value)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out")
	}
}

// Validates: R-PARSE-005 + R-PARSE-007 (STRING).
func TestDeviceNotification_StringType(t *testing.T) {
	conn := newTestConnection()
	defer conn.lifecycle.shutdown()

	ch := make(chan *Update, 5)
	sym := &symtab.Symbol{FullName: "MAIN.sName", DataType: "STRING", Length: 20}
	conn.notifications.activeNotifications[11] = activeNotification{Sym: sym, Ch: ch}
	conn.cache.symbols[symtab.Key(sym.FullName)] = sym

	strData := make([]byte, 20)
	copy(strData, "Hello\x00")

	packet := buildNotificationPacket(11, 0, strData)
	err := conn.drivePacket(conn.lifecycle.ctx, packet)
	if err != nil {
		t.Fatalf("error: %v", err)
	}

	select {
	case u := <-ch:
		if u.Value != "Hello" {
			t.Errorf("STRING value = %q, want %q", u.Value, "Hello")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out")
	}
}

// ==========================================================================
// Notification timestamp conversion
// ==========================================================================

// TestWindowsFiletimeConversion drives hard-coded raw Windows FILETIME
// values through the production deviceNotification → handleNotification
// path and asserts the resulting Update.TimeStamp.
//
// The previous version computed the encoding (filetime = (unixSec +
// secToUnixEpoch) * windowsTick) and then immediately reversed it with
// the same constants — neither side touched the production handler, so
// a constant drift in the production code would shift both sides in
// lockstep and the test would still pass. This rewrite pins literal
// FILETIME values from external authority (Wolfram Alpha / Boost
// reference) so a constant drift surfaces here as an explicit failure.
//
// Validates: R-NOT-012 (Windows-100ns to time.Time conversion).
func TestWindowsFiletimeConversion(t *testing.T) {
	tests := []struct {
		name     string
		filetime uint64    // raw Windows FILETIME, externally calculated.
		want     time.Time // expected UTC instant.
	}{
		// Unix epoch, 1970-01-01 00:00:00 UTC.
		// 11644473600 sec since 1601-01-01, * 10^7 ticks/sec.
		{"unix_epoch", 116444736000000000, time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC)},
		// Y2K, 2000-01-01 00:00:00 UTC.
		// (11644473600 + 946684800) * 10^7.
		{"y2k", 125911584000000000, time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)},
		// 2024-01-01 00:00:00 UTC.
		// (11644473600 + 1704067200) * 10^7.
		{"2024", 133485408000000000, time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			conn := newTestConnection()
			defer conn.lifecycle.shutdown()

			ch := make(chan *Update, 1)
			sym := &symtab.Symbol{
				FullName: "MAIN.x",
				DataType: "INT",
				Length:   2,
			}
			conn.notifications.activeNotifications[7] = activeNotification{Sym: sym, Ch: ch}
			conn.cache.symbols[symtab.Key(sym.FullName)] = sym

			data := make([]byte, 2)
			binary.LittleEndian.PutUint16(data, 1)
			packet := buildNotificationPacket(7, tt.filetime, data)

			if err := conn.drivePacket(conn.lifecycle.ctx, packet); err != nil {
				t.Fatalf("drivePacket: %v", err)
			}

			select {
			case update := <-ch:
				if !update.Time.Equal(tt.want) {
					t.Errorf("TimeStamp = %v, want %v", update.Time.UTC(), tt.want)
				}
			case <-time.After(time.Second):
				t.Fatal("no update received")
			}
		})
	}
}

// TestNotification_TerminalZeroByteSample_TriggersDetection validates that
// the notification listener intercepts a 0-byte terminal sample (TwinCAT
// signal that the symbol is gone post-online-change) BEFORE the
// parse-error log path executes, classifying it as a R-CACHE-009
// supplementary signal and firing the configured online-change callback
// with ReasonSymbolNotFound.
//
// Hardware finding (TC3 sweep): symbol deletion via online change → PLC
// drops the old notification handle silently and emits one terminal
// 0-byte sample on the now-dead handle. Without interception, the parser
// errors with "symbol.Length 4 exceeds data buffer size 0" and the user
// gets noise instead of a structured stale-cache signal.
//
// Validates: R-CACHE-009 supplementary detection + R-NOT-016 (callback
// reason) + no Update delivered for the dead handle.
func TestNotification_TerminalZeroByteSample_TriggersDetection(t *testing.T) {
	conn := newTestConnection()
	defer conn.lifecycle.shutdown()

	cbReason := make(chan Reason, 1)
	conn.versionStrategy = SymbolVersionIgnore
	conn.versionCallback = func(r Reason) {
		select {
		case cbReason <- r:
		default:
		}
	}

	ch := make(chan *Update, 4)
	sym := &symtab.Symbol{
		FullName: "MAIN.x",
		DataType: "DINT",
		Length:   4,
	}
	conn.notifications.activeNotifications[42] = activeNotification{Sym: sym, Ch: ch}
	conn.cache.symbols[symtab.Key(sym.FullName)] = sym

	// Inject 0-byte terminal sample. drivePacket may or may not return an
	// error from the now-skipped parse path — what matters is the callback
	// firing with ReasonSymbolNotFound BEFORE the (downgraded) parse log.
	packet := buildNotificationPacket(42, 0, []byte{})
	if err := conn.drivePacket(conn.lifecycle.ctx, packet); err != nil {
		t.Logf("drivePacket err (acceptable, detection still must fire): %v", err)
	}

	select {
	case r := <-cbReason:
		if r != ReasonSymbolNotFound {
			t.Errorf("callback reason = %q, want %q", r, ReasonSymbolNotFound)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("0-byte terminal sample did not trigger online-change callback")
	}

	// No Update delivered for dead handle — terminal sample, not a value.
	select {
	case u := <-ch:
		t.Errorf("unexpected Update delivered for terminal 0-byte sample: %+v", u)
	case <-time.After(50 * time.Millisecond):
		// expected: nothing on the channel
	}
}

// TestNotification_ListenerPathTriggersIgnoreMarksOtherHandles validates
// the END-TO-END wire from listener-path 0-byte terminal detection through
// the Ignore branch: a 0-byte terminal sample on handle Hdead must mark
// every OTHER active handle stale so its next delivery carries Stale=true.
//
// Hardware regression target: TestSymbolVersionIgnore_RemovedSymbolStops
// (symbol_version_hardware_test.go) couldn't observe Stale via the dead
// handle (terminal sample is suppressed by design — see
// TestNotification_TerminalZeroByteSample_TriggersDetection). This test
// proves the marking still propagates to surviving subscriptions.
//
// Validates: R-CACHE-009 (listener-path detection) + R-CACHE-012 +
// R-NOT-016 (callback) + R-NOT-017 (Ignore branch marks all handles).
func TestNotification_ListenerPathTriggersIgnoreMarksOtherHandles(t *testing.T) {
	conn := newTestConnection()
	defer conn.lifecycle.shutdown()
	conn.versionStrategy = SymbolVersionIgnore

	cbReason := make(chan Reason, 1)
	conn.versionCallback = func(r Reason) {
		select {
		case cbReason <- r:
		default:
		}
	}

	const hDead, hLive uint32 = 100, 200
	chDead := make(chan *Update, 4)
	chLive := make(chan *Update, 4)
	symDead := &symtab.Symbol{FullName: "MAIN.dead", DataType: "DINT", Length: 4}
	symLive := &symtab.Symbol{FullName: "MAIN.live", DataType: "INT", Length: 2}

	conn.notifications.activeNotifications[hDead] = activeNotification{Sym: symDead, Ch: chDead}
	conn.notifications.activeNotifications[hLive] = activeNotification{Sym: symLive, Ch: chLive}
	conn.cache.symbols[symtab.Key(symDead.FullName)] = symDead
	conn.cache.symbols[symtab.Key(symLive.FullName)] = symLive

	// Inject 0-byte terminal sample on hDead — listener path fires
	// handleStaleDetection(0x710), Ignore branch must mark BOTH handles.
	terminal := buildNotificationPacket(hDead, 0, []byte{})
	if err := conn.drivePacket(conn.lifecycle.ctx, terminal); err != nil {
		t.Logf("drivePacket terminal err (acceptable): %v", err)
	}

	// Callback must fire with symbol-not-found.
	select {
	case r := <-cbReason:
		if r != ReasonSymbolNotFound {
			t.Errorf("callback reason = %q, want %q", r, ReasonSymbolNotFound)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("callback did not fire from listener-path detection")
	}

	// Dead handle: by design no Update emitted.
	select {
	case u := <-chDead:
		t.Errorf("unexpected Update on dead handle: %+v", u)
	case <-time.After(50 * time.Millisecond):
	}

	// Live handle: next normal sample must carry Stale=true,
	// Reason=ReasonSymbolNotFound (Ignore branch marked it).
	livePkt := buildNotificationPacket(hLive, 0, []byte{0x05, 0x00})
	if err := conn.drivePacket(conn.lifecycle.ctx, livePkt); err != nil {
		t.Fatalf("drivePacket live: %v", err)
	}
	select {
	case u := <-chLive:
		if u.Stale == nil || u.Stale.Reason != ReasonSymbolNotFound {
			t.Errorf("live first post-detection sample: Stale=%v, want non-nil with Reason=%q",
				u.Stale, ReasonSymbolNotFound)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no live sample after listener-path detection")
	}

	// Live handle: second sample must NOT be Stale (one-shot consumed).
	livePkt2 := buildNotificationPacket(hLive, 0, []byte{0x06, 0x00})
	if err := conn.drivePacket(conn.lifecycle.ctx, livePkt2); err != nil {
		t.Fatalf("drivePacket live #2: %v", err)
	}
	select {
	case u := <-chLive:
		if u.Stale != nil {
			t.Errorf("live second sample: Stale=%+v, want nil", u.Stale)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no live sample #2")
	}
}

// Orphan samples interleaved with healthy ones must still produce one Warn.
// The previous throttle reset its counter on every owned sample, so after a
// reconnect -- the one time orphans actually arrive in bulk -- the episode ended
// on the next healthy sample and every orphan warned again.
func TestDeviceNotification_UnknownHandleWarnsOnceWhileInterleaved(t *testing.T) {
	handler := &testlog.Handler{}
	conn := newTestConnection()
	conn.logger = slog.New(handler)
	defer conn.lifecycle.shutdown()

	owned := uint32(0xA1)
	preSeedTypedSymbol(conn, "MAIN.owned", owned)
	ch := make(chan *Update, 64)
	conn.notifications.lock.Lock()
	conn.notifications.activeNotifications[owned] = activeNotification{
		Sym: conn.cache.symbols[symtab.Key("MAIN.owned")], Ch: ch,
	}
	conn.notifications.lock.Unlock()

	data := make([]byte, 2)
	binary.LittleEndian.PutUint16(data, 42)
	for i := 0; i < 20; i++ {
		// One orphan, then one owned, repeatedly: the shape after a reconnect.
		if err := conn.drivePacket(conn.lifecycle.ctx, buildNotificationPacket(uint32(0x900+i), 0, data)); err != nil {
			t.Fatalf("orphan packet %d: %v", i, err)
		}
		if err := conn.drivePacket(conn.lifecycle.ctx, buildNotificationPacket(owned, 0, data)); err != nil {
			t.Fatalf("owned packet %d: %v", i, err)
		}
	}

	warns := 0
	for _, rec := range handler.RecordsByLevel(slog.LevelWarn) {
		if strings.Contains(rec.Message, "received notification for unknown handle") {
			warns++
		}
	}
	if warns != 1 {
		t.Errorf("got %d warns for 20 interleaved orphans, want 1 — the throttle resets on owned samples again", warns)
	}
}
