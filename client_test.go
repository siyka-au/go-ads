package ads

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/siyka-au/go-ads/v3/internal/fakeplc"

	"github.com/siyka-au/go-ads/v3/ams"
)

// TestGetSymbol_TraceLogDoesNotRaceNotificationWriter: the trace line at the end of
// getSymbol must not hand the live *symbol to the logger.
//
// slog formats a *symbol by reflection and reads Value / Valid / ValueParsed /
// LastUpdateTime. Those are written by updateValue under cache.lock, from the
// Client's recvWorker via handleNotification → dispatchSample. getSymbol logged the
// pointer after releasing cache.lock, so a subscribed session with trace logging on
// read a string header and a multi-word time.Time unsynchronised.
//
// The configuration no other test had is the enabled LevelTrace handler: slog skips
// its args at a disabled level, which is the only reason the suite was green. So this
// test enables LevelTrace while notifications flow. -race is the oracle.
//
// Lives in client_test.go rather than beside getSymbol only because of file ownership
// during the concurrent fix waves.
func TestGetSymbol_TraceLogDoesNotRaceNotificationWriter(t *testing.T) {
	srv := fakeplc.StartPLC(t)
	defer srv.Stop()

	srv.OnWriteRead(ams.GroupSymbolInfoByNameEx, func(req []byte) []byte {
		name := strings.TrimRight(string(req), "\x00")
		return fakeplc.SymbolInfoPayload(name, "INT", "", 0x4040, 0x100, 2, ams.DataTypeInt16, 0)
	})
	var nextHandle atomic.Uint32
	srv.OnWriteRead(ams.GroupSymbolHandleByName, func(_ []byte) []byte {
		return fakeplc.HandlePayload(nextHandle.Add(1))
	})

	sess, client := newWiredTestSession(t, srv)
	// An ENABLED trace handler that actually formats its attrs. io.Discard keeps
	// the output cost off the test, but the reflection over the attr still happens.
	sess.logger = slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: LevelTrace}))
	client.SetNotificationHandler(sess.handleNotification)
	ctx := context.Background()

	const symbolName = "MAIN.traced"
	const notifHandle uint32 = 0x0BAD0002
	sym, err := sess.getSymbol(ctx, symbolName)
	if err != nil {
		t.Fatalf("resolving %s: %v", symbolName, err)
	}
	updates := make(chan *Update, 1)
	sess.notifications.lock.Lock()
	sess.notifications.activeNotifications[notifHandle] = activeNotification{Sym: sym, Ch: updates}
	sess.notifications.lock.Unlock()

	sample := make([]byte, 2)
	binary.LittleEndian.PutUint16(sample, 7)
	packet := buildNotificationPacket(notifHandle, 0, sample)

	const (
		readers     = 4
		dispatchers = 4
		iterations  = 200
	)
	var wg sync.WaitGroup
	var readErr atomic.Value

	// Readers — production getSymbol on a cached symbol reaches the trace line.
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				got, err := sess.getSymbol(ctx, symbolName)
				switch {
				case err != nil:
					readErr.Store(err)
					return
				case got != sym:
					readErr.Store(fmt.Errorf("getSymbol returned %p, want the cached %p", got, sym))
					return
				}
			}
		}()
	}
	// Writers — production dispatch mutates Value / Valid / LastUpdateTime under cache.lock.
	for i := 0; i < dispatchers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				if err := sess.drivePacket(ctx, packet); err != nil {
					readErr.Store(fmt.Errorf("drivePacket: %w", err))
					return
				}
			}
		}()
	}

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("getSymbol / dispatch workers did not finish in 30s — a lock-order inversion, not a race")
	}
	if err, ok := readErr.Load().(error); ok && err != nil {
		t.Fatalf("worker failed: %v", err)
	}
	if !sym.ValueParsed {
		t.Error("no notification sample was ever parsed, so the writer half never ran")
	}
}
