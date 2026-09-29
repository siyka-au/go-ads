package ads

import (
	"encoding/binary"
	"sync/atomic"
	"testing"

	"github.com/siyka-au/go-ads/v3/internal/fakeplc"

	"github.com/siyka-au/go-ads/v3/internal/symtab"

	"github.com/siyka-au/go-ads/v3/ams"
)

// TestReleasePLCResources_NotificationCleanup exercises the notification
// cleanup branch of releasePLCResources directly (rather than via Close,
// which blocks on the Client waitGroup in test harness setups where the
// Client ctx is independent of sess.lifecycle.ctx). The helper is the
// shared entry point used by both Close() and the Reconnect-exhaustion
// path; testing it directly covers both call sites.
//
// Validates: PLC-side notification delete fires for every staged handle.
func TestReleasePLCResources_NotificationCleanup(t *testing.T) {
	srv := fakeplc.StartPLC(t)
	defer srv.Stop()

	const stagedHandle uint32 = 0xC0DE
	var deletes atomic.Int32
	// releasePLCResources calls bestEffortDeleteNotifications which prefers
	// SumDeleteDeviceNotification; register that handler so the sum path
	// completes instead of falling back. Count handles passed through.
	srv.OnWriteRead(ams.GroupSumupDeleteDeviceNotification, func(req []byte) []byte {
		nItems := len(req) / 4
		codes := make([]ams.ReturnCode, nItems)
		for i := 0; i < nItems; i++ {
			h := binary.LittleEndian.Uint32(req[i*4:])
			if h == stagedHandle {
				deletes.Add(1)
			}
			codes[i] = ams.ReturnCodeNoErrors
		}
		return fakeplc.SumDeleteNotifPayload(codes)
	})

	sess, _ := newWiredTestSession(t, srv)
	sess.notifications.lock.Lock()
	sess.notifications.activeNotifications[stagedHandle] = activeNotification{Sym: &symtab.Symbol{FullName: "MAIN.x"}}
	sess.notifications.lock.Unlock()

	sess.releasePLCResources(false)

	if got := deletes.Load(); got < 1 {
		t.Errorf("staged handle delivered to SumDelete = %d, want >= 1 (must release PLC subscriptions)", got)
	}
}

// TestReleasePLCResources_SymbolHandleRelease_SkippedWhenDisconnected pins
// the wasDisconnected=true short-circuit: when the transport is already
// dead, the helper must NOT issue PLC Write commands for handle release.
// Issuing them against a dead socket would block Close behind requestTimeout
// for every staged handle.
//
// Validates: wasDisconnected gate on symbol-handle release path.
func TestReleasePLCResources_SymbolHandleRelease_SkippedWhenDisconnected(t *testing.T) {
	srv := fakeplc.StartPLC(t)
	defer srv.Stop()

	var writes atomic.Int32
	srv.OnWrite(ams.GroupSymbolReleaseHandle, func(_, _ uint32, _ []byte) ams.ReturnCode {
		writes.Add(1)
		return ams.ReturnCodeNoErrors
	})

	sess, _ := newWiredTestSession(t, srv)
	sess.cache.lock.Lock()
	sess.cache.symbols[symtab.Key("MAIN.x")] = &symtab.Symbol{
		FullName: "MAIN.x", Name: "MAIN.x", Handle: 0x12345678,
	}
	sess.cache.lock.Unlock()

	sess.releasePLCResources(true) // wasDisconnected=true

	if got := writes.Load(); got != 0 {
		t.Errorf("GroupSymbolReleaseHandle writes = %d, want 0 (must skip when disconnected)", got)
	}
}

// TestReleasePLCResources_SymbolHandleRelease_FiredWhenConnected: with the
// transport alive, the helper must issue ReleaseHandle for every cached
// symbol with a non-zero Handle.
func TestReleasePLCResources_SymbolHandleRelease_FiredWhenConnected(t *testing.T) {
	srv := fakeplc.StartPLC(t)
	defer srv.Stop()

	const stagedHandle uint32 = 0xABCD0001
	var writes atomic.Int32
	srv.OnWrite(ams.GroupSymbolReleaseHandle, func(_, _ uint32, _ []byte) ams.ReturnCode {
		writes.Add(1)
		return ams.ReturnCodeNoErrors
	})

	sess, _ := newWiredTestSession(t, srv)
	sess.cache.lock.Lock()
	sess.cache.symbols[symtab.Key("MAIN.x")] = &symtab.Symbol{
		FullName: "MAIN.x", Name: "MAIN.x", Handle: stagedHandle,
	}
	sess.cache.lock.Unlock()

	sess.releasePLCResources(false) // wasDisconnected=false

	if got := writes.Load(); got < 1 {
		t.Errorf("ReleaseHandle writes = %d, want >= 1 (alive transport must release)", got)
	}
}
