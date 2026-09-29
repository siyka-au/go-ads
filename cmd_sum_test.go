package ads

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/siyka-au/go-ads/v3/internal/adsconn"

	"github.com/siyka-au/go-ads/v3/internal/fakeplc"

	"github.com/siyka-au/go-ads/v3/internal/symtab"

	"github.com/siyka-au/go-ads/v3/ams"
)

// bestEffortDeleteNotifications returns 0 for an empty input slice and never
// touches the network.
// Validates: R-NOT-015.
func TestBestEffortDeleteNotifications_Empty(t *testing.T) {
	conn := &Session{logger: slog.Default()}
	conn.client.Store(adsconn.New(adsconn.Config{Logger: conn.logger}))
	got := conn.bestEffortDeleteNotifications(context.Background(), nil)
	if got != 0 {
		t.Errorf("expected 0, got %d", got)
	}
	got = conn.bestEffortDeleteNotifications(context.Background(), []uint32{})
	if got != 0 {
		t.Errorf("expected 0, got %d", got)
	}
}

// seedSymbol installs a minimal cache symbol entry so getSymbol short-circuits.
// Length=1 + DataType="BOOL" parses cleanly; Handle is non-zero so getSymbol
// skips the PLC GetHandleByName roundtrip.
func seedSymbol(sess *Session, name string, handle uint32) {
	sym := &symtab.Symbol{
		FullName: name,
		Handle:   handle,
		Length:   1,
		DataType: "BOOL",
	}
	sess.cache.symbols[symtab.Key(name)] = sym
}

// TestSession_ReadValues_StaleDetection validates R-CACHE-009
// detection in the sum-batch decode path: a per-item ReturnCode in the
// stale-cache set (e.g. 0x711 SymbolVersionInvalid) must trigger
// handleStaleDetection through the configured strategy callback even when
// the batched roundtrip itself succeeded.
//
// Validates: R-CACHE-009 (sum-batch per-item detection wiring).
func TestSession_ReadValues_StaleDetection(t *testing.T) {
	srv := fakeplc.StartPLC(t)
	defer srv.Stop()

	cbReason := make(chan Reason, 1)
	sess, _ := newWiredTestSession(t, srv,
		WithSymbolVersionStrategy(SymbolVersionIgnore),
		WithOnSymbolVersionChanged(func(r Reason) {
			select {
			case cbReason <- r:
			default:
			}
		}),
	)
	seedSymbol(sess, "MAIN.a", 0x1001)
	seedSymbol(sess, "MAIN.b", 0x1002)

	// SumReadEx2 (0xF084) handler: respond with one stale code + one OK.
	// Response shape: [N × (error(4), length(4))][data].
	srv.OnWriteRead(ams.GroupSumupReadEx2, func(_ []byte) []byte {
		resp := fakeplc.SumReadResponse(
			[]ams.ReturnCode{ams.ReturnCodeDeviceSymbolVersionInvalid, ams.ReturnCodeNoErrors},
			[]uint32{0, 1},
			[]byte{0x00},
		)
		return resp
	})

	// The stale item now comes back as a per-item failure in a *BatchError
	// (DECISIONS.md Decision 1); detection must still fire regardless.
	values, err := sess.ReadValues(context.Background(), []string{"MAIN.a", "MAIN.b"})
	batchErr := batchErrorFor(t, err)
	if len(batchErr.Items) != 1 || batchErr.Items[0].Symbol != "MAIN.a" {
		t.Errorf("got failed items %v, want just MAIN.a", batchErr.Items)
	}
	if _, ok := values["MAIN.b"]; !ok {
		t.Errorf("got values = %v, want the readable symbol MAIN.b present", values)
	}

	select {
	case got := <-cbReason:
		if got != ReasonSymbolVersionInvalid {
			t.Errorf("callback reason = %q, want %q", got, ReasonSymbolVersionInvalid)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("strategy callback did not fire within 2s for sum-batch stale code")
	}
}

// TestSession_ReadValues_FiresCallbackOncePerBatch validates that
// when N>1 items in a single batched response carry stale codes, the
// strategy callback fires exactly ONCE (R-SES-011 "once per detection").
// Verified via a buffered channel of size 1 and a drain check after a
// short settle window.
//
// Validates: R-CACHE-009 + R-SES-011 (no callback amplification on batched ops).
func TestSession_ReadValues_FiresCallbackOncePerBatch(t *testing.T) {
	srv := fakeplc.StartPLC(t)
	defer srv.Stop()

	// Buffer of 4 so a buggy implementation (one callback per stale item)
	// would visibly fill the channel; correct impl pushes exactly 1.
	calls := make(chan Reason, 4)
	sess, _ := newWiredTestSession(t, srv,
		WithSymbolVersionStrategy(SymbolVersionIgnore),
		WithOnSymbolVersionChanged(func(r Reason) { calls <- r }),
	)
	seedSymbol(sess, "MAIN.a", 0x2001)
	seedSymbol(sess, "MAIN.b", 0x2002)
	seedSymbol(sess, "MAIN.c", 0x2003)

	// Three stale codes — implementation must break after first.
	srv.OnWriteRead(ams.GroupSumupReadEx2, func(_ []byte) []byte {
		return fakeplc.SumReadResponse(
			[]ams.ReturnCode{
				ams.ReturnCodeDeviceSymbolVersionInvalid,
				ams.ReturnCodeDeviceSymbolVersionInvalid,
				ams.ReturnCodeDeviceSymbolNoFound,
			},
			[]uint32{0, 0, 0},
			nil,
		)
	})

	// All three items carry a failing code, so the call now reports an
	// all-failed batch (DECISIONS.md Decision 1) — asserted here rather than
	// ignored, so this test still notices if the read path stops reporting it.
	// The subject of the test is unchanged: detection fires exactly once.
	_, err := sess.ReadValues(context.Background(), []string{"MAIN.a", "MAIN.b", "MAIN.c"})
	batchErr := batchErrorFor(t, err)
	if len(batchErr.Items) != 3 || batchErr.Succeeded != 0 {
		t.Errorf("got Succeeded=%d failed items %v, want 0 succeeded and all three failed",
			batchErr.Succeeded, batchErr.Items)
	}

	// Wait for first callback.
	select {
	case <-calls:
	case <-time.After(2 * time.Second):
		t.Fatal("callback did not fire at all within 2s")
	}

	// Settle window: any extra (buggy-amplified) callbacks would surface here.
	select {
	case extra := <-calls:
		t.Fatalf("callback fired more than once per batch (got extra %q)", extra)
	case <-time.After(150 * time.Millisecond):
		// pass — no amplification
	}
}

// TestSession_WriteValues_StaleDetection validates R-CACHE-009
// detection in the SumWrite batch decode: a stale per-item code triggers
// the strategy callback. SumWrite response is [N × error(4)] with no data
// section, so this also exercises the bare-error-array decode path.
//
// Validates: R-CACHE-009 (SumWrite batch wiring).
func TestSession_WriteValues_StaleDetection(t *testing.T) {
	srv := fakeplc.StartPLC(t)
	defer srv.Stop()

	cbReason := make(chan Reason, 1)
	sess, _ := newWiredTestSession(t, srv,
		WithSymbolVersionStrategy(SymbolVersionIgnore),
		WithOnSymbolVersionChanged(func(r Reason) {
			select {
			case cbReason <- r:
			default:
			}
		}),
	)
	seedSymbol(sess, "MAIN.a", 0x3001)
	seedSymbol(sess, "MAIN.b", 0x3002)

	// SumWrite (0xF081) response: N × uint32 per-item error codes.
	srv.OnWriteRead(ams.GroupSumupWrite, func(_ []byte) []byte {
		buf := make([]byte, 8)
		binary.LittleEndian.PutUint32(buf[0:], uint32(ams.ReturnCodeDeviceSymbolVersionInvalid))
		binary.LittleEndian.PutUint32(buf[4:], uint32(ams.ReturnCodeNoErrors))
		return buf
	})

	// The rejected item is now named in a *BatchError (DECISIONS.md Decision 1);
	// detection must still fire.
	codes, err := sess.WriteValues(context.Background(), map[string]any{
		"MAIN.a": true,
		"MAIN.b": false,
	})
	// Which name lands in slot 0 depends on Go's map iteration order, so assert
	// the shape rather than the identity: one rejected, one accepted.
	batchErr := batchErrorFor(t, err)
	if len(batchErr.Items) != 1 || batchErr.Succeeded != 1 {
		t.Errorf("got Succeeded=%d failed items %v, want 1 of each", batchErr.Succeeded, batchErr.Items)
	}
	if len(codes) != 2 {
		t.Errorf("got codes = %v, want a code for both symbols", codes)
	}

	select {
	case got := <-cbReason:
		if got != ReasonSymbolVersionInvalid {
			t.Errorf("callback reason = %q, want %q", got, ReasonSymbolVersionInvalid)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("strategy callback did not fire within 2s for SumWrite stale code")
	}
}

// --- batch error contract (DECISIONS.md Decision 1) ---
//
// The contract these pin: a batch call returns the values it obtained plus a
// *BatchError naming every item that produced none. A bare error is reserved
// for a transport failure, where no item's outcome is known. Batch size does
// not change the shape of any of this.

// batchErrorFor extracts the *BatchError from err, failing the test if err is
// nil or is a different kind of error.
func batchErrorFor(t *testing.T, err error) *BatchError {
	t.Helper()
	if err == nil {
		t.Fatal("got err = nil, want a *BatchError naming the failed items")
	}
	var batchErr *BatchError
	if !errors.As(err, &batchErr) {
		t.Fatalf("got err = %v (%T), want a *BatchError", err, err)
	}
	return batchErr
}

// itemFor returns the BatchError entry for symbol, or fails the test.
func itemFor(t *testing.T, batchErr *BatchError, symbol string) BatchItemError {
	t.Helper()
	for _, item := range batchErr.Items {
		if item.Symbol == symbol {
			return item
		}
	}
	t.Fatalf("no BatchError item for %q; got %v", symbol, batchErr.Items)
	return BatchItemError{}
}

// TestReadValues_AllItemsFailedIsNotSuccess pins the measured TC3
// failure: after a runtime restart every cached handle is refused, so every
// item comes back 0x710. Before the batch error contract this returned an
// empty map with err == nil — total data loss reported as success.
//
// Validates: DECISIONS.md Decision 1 (per-item status, PLC-verdict state).
func TestReadValues_AllItemsFailedIsNotSuccess(t *testing.T) {
	srv := fakeplc.StartPLC(t)
	defer srv.Stop()

	sess, _ := newWiredTestSession(t, srv, WithSymbolVersionStrategy(SymbolVersionIgnore))
	seedSymbol(sess, "MAIN.a", 0x4001)
	seedSymbol(sess, "MAIN.b", 0x4002)
	seedSymbol(sess, "MAIN.c", 0x4003)

	srv.OnWriteRead(ams.GroupSumupReadEx2, func(_ []byte) []byte {
		return fakeplc.SumReadResponse(
			[]ams.ReturnCode{
				ams.ReturnCodeDeviceSymbolNoFound,
				ams.ReturnCodeDeviceSymbolNoFound,
				ams.ReturnCodeDeviceSymbolNoFound,
			},
			[]uint32{0, 0, 0},
			nil,
		)
	})

	names := []string{"MAIN.a", "MAIN.b", "MAIN.c"}
	values, err := sess.ReadValues(context.Background(), names)
	batchErr := batchErrorFor(t, err)

	if len(values) != 0 {
		t.Errorf("got %d values for a batch where every item was refused, want 0: %v", len(values), values)
	}
	if batchErr.Requested != 3 || batchErr.Succeeded != 0 || len(batchErr.Items) != 3 {
		t.Errorf("got Requested=%d Succeeded=%d items=%d, want 3/0/3",
			batchErr.Requested, batchErr.Succeeded, len(batchErr.Items))
	}
	for _, item := range batchErr.Items {
		// A refused handle is a device verdict, not a library-side skip: the
		// caller must be able to tell those apart to know whether retrying
		// the same request could ever work.
		if item.Skipped != nil {
			t.Errorf("item %s: Skipped = %v, want nil (the PLC gave a verdict)", item.Symbol, item.Skipped)
		}
		if item.Error != ams.ReturnCodeDeviceSymbolNoFound {
			t.Errorf("item %s: Error = 0x%X, want 0x%X", item.Symbol, uint32(item.Error), uint32(ams.ReturnCodeDeviceSymbolNoFound))
		}
	}
}

// TestReadValues_OneAbsentSymbolKeepsTheRest pins the constraint that
// one misspelled tag must not stop the other values flowing: the successful
// items stay in the map and only the failed one is named.
//
// Validates: DECISIONS.md Decision 1 (partial success stays usable).
func TestReadValues_OneAbsentSymbolKeepsTheRest(t *testing.T) {
	srv := fakeplc.StartPLC(t)
	defer srv.Stop()

	sess, _ := newWiredTestSession(t, srv, WithSymbolVersionStrategy(SymbolVersionIgnore))
	seedSymbol(sess, "MAIN.a", 0x4101)
	seedSymbol(sess, "MAIN.absent", 0x4102)
	seedSymbol(sess, "MAIN.c", 0x4103)

	srv.OnWriteRead(ams.GroupSumupReadEx2, func(_ []byte) []byte {
		return fakeplc.SumReadResponse(
			[]ams.ReturnCode{ams.ReturnCodeNoErrors, ams.ReturnCodeDeviceSymbolNoFound, ams.ReturnCodeNoErrors},
			[]uint32{1, 0, 1},
			[]byte{0x01, 0x00},
		)
	})

	names := []string{"MAIN.a", "MAIN.absent", "MAIN.c"}
	values, err := sess.ReadValues(context.Background(), names)
	batchErr := batchErrorFor(t, err)

	if len(values) != 2 || values["MAIN.a"] == nil || values["MAIN.c"] == nil {
		t.Errorf("got values = %v, want the two readable symbols present", values)
	}
	if len(batchErr.Items) != 1 {
		t.Fatalf("got %d failed items, want 1: %v", len(batchErr.Items), batchErr.Items)
	}
	if batchErr.Succeeded != 2 {
		t.Errorf("got Succeeded = %d, want 2", batchErr.Succeeded)
	}
	item := itemFor(t, batchErr, "MAIN.absent")
	if item.Skipped != nil || item.Error != ams.ReturnCodeDeviceSymbolNoFound {
		t.Errorf("got item %+v, want a PLC verdict of 0x710", item)
	}
	if !strings.Contains(err.Error(), "MAIN.absent") {
		t.Errorf("error message %q does not name the failed symbol", err.Error())
	}
}

// TestReadValues_UnresolvedSymbolIsReported covers the drop site where
// getSymbol fails, so the item never reaches the wire. Previously the name was
// dropped from the result map with no error whenever any other item decoded.
//
// Validates: DECISIONS.md Decision 1 (Skipped state, resolve-time drop).
func TestReadValues_UnresolvedSymbolIsReported(t *testing.T) {
	srv := fakeplc.StartPLC(t)
	defer srv.Stop()

	sess, _ := newWiredTestSession(t, srv, WithSymbolVersionStrategy(SymbolVersionIgnore))
	seedSymbol(sess, "MAIN.a", 0x4201)
	// MAIN.typo is not seeded, and the server answers no handle lookup, so
	// getSymbol fails for it before any request is built.

	srv.OnWriteRead(ams.GroupSumupReadEx2, func(_ []byte) []byte {
		return fakeplc.SumReadResponse([]ams.ReturnCode{ams.ReturnCodeNoErrors}, []uint32{1}, []byte{0x01})
	})

	values, err := sess.ReadValues(context.Background(), []string{"MAIN.a", "MAIN.typo"})
	batchErr := batchErrorFor(t, err)

	if len(values) != 1 || values["MAIN.a"] == nil {
		t.Errorf("got values = %v, want MAIN.a present", values)
	}
	item := itemFor(t, batchErr, "MAIN.typo")
	if !errors.Is(item.Skipped, ErrBatchSymbolUnresolved) {
		t.Errorf("got Skipped = %v, want it to match ErrBatchSymbolUnresolved", item.Skipped)
	}
	if !errors.Is(err, ErrBatchSymbolUnresolved) {
		t.Error("errors.Is(err, ErrBatchSymbolUnresolved) = false; the skip reason must be reachable from the returned error")
	}

	// Nothing resolvable at all takes an earlier return, and it must produce the
	// same shape — the contract cannot depend on how many items survived.
	values, err = sess.ReadValues(context.Background(), []string{"MAIN.typo", "MAIN.other"})
	batchErr = batchErrorFor(t, err)
	if len(values) != 0 {
		t.Errorf("got values = %v, want none", values)
	}
	if batchErr.Requested != 2 || len(batchErr.Items) != 2 {
		t.Errorf("got Requested=%d items=%v, want both names reported", batchErr.Requested, batchErr.Items)
	}
}

// TestReadValues_VanishedAndUnparsableAreReported covers the two
// post-roundtrip drop sites: the cache entry disappearing mid-roundtrip
// (live == nil) and the payload failing to decode against the cached type,
// which is what an undetected INT→LREAL online change looks like. Both used to
// leave the name missing from the map with err == nil, permanently.
//
// Validates: DECISIONS.md Decision 1 (Skipped state, decode-time drops).
func TestReadValues_VanishedAndUnparsableAreReported(t *testing.T) {
	srv := fakeplc.StartPLC(t)
	defer srv.Stop()

	sess, _ := newWiredTestSession(t, srv, WithSymbolVersionStrategy(SymbolVersionIgnore))
	seedSymbol(sess, "MAIN.a", 0x4301)
	seedSymbol(sess, "MAIN.gone", 0x4302)
	seedSymbol(sess, "MAIN.widened", 0x4303)

	// The handler runs after the requests are on the wire and before the decode
	// loop takes cache.lock, so it can stage exactly the two races: drop one
	// entry from the cache, and widen another's Length past the payload the PLC
	// is about to return.
	srv.OnWriteRead(ams.GroupSumupReadEx2, func(_ []byte) []byte {
		sess.cache.lock.Lock()
		delete(sess.cache.symbols, symtab.Key("MAIN.gone"))
		sess.cache.symbols[symtab.Key("MAIN.widened")].Length = 8
		sess.cache.lock.Unlock()
		return fakeplc.SumReadResponse(
			[]ams.ReturnCode{ams.ReturnCodeNoErrors, ams.ReturnCodeNoErrors, ams.ReturnCodeNoErrors},
			[]uint32{1, 1, 1},
			[]byte{0x01, 0x01, 0x01},
		)
	})

	names := []string{"MAIN.a", "MAIN.gone", "MAIN.widened"}
	values, err := sess.ReadValues(context.Background(), names)
	batchErr := batchErrorFor(t, err)

	if len(values) != 1 || values["MAIN.a"] == nil {
		t.Errorf("got values = %v, want only MAIN.a", values)
	}
	if got := itemFor(t, batchErr, "MAIN.gone"); !errors.Is(got.Skipped, ErrBatchSymbolVanished) {
		t.Errorf("MAIN.gone: Skipped = %v, want ErrBatchSymbolVanished", got.Skipped)
	}
	if got := itemFor(t, batchErr, "MAIN.widened"); !errors.Is(got.Skipped, ErrBatchValueUnparsable) {
		t.Errorf("MAIN.widened: Skipped = %v, want ErrBatchValueUnparsable", got.Skipped)
	}
}

// TestReadValues_TransportFailureIsABareError pins the other half of
// the contract: a router-level rejection is not a per-item verdict, so it must
// NOT arrive as a *BatchError. A caller that unwraps one and reads the map
// would be trusting values that were never fetched.
//
// Validates: DECISIONS.md Decision 1 (bare error reserved for transport).
func TestReadValues_TransportFailureIsABareError(t *testing.T) {
	srv := fakeplc.StartPLC(t)
	defer srv.Stop()

	sess, _ := newWiredTestSession(t, srv, WithSymbolVersionStrategy(SymbolVersionIgnore))
	seedSymbol(sess, "MAIN.a", 0x4401)
	seedSymbol(sess, "MAIN.b", 0x4402)

	// Seeded symbols need no handle lookup, so the first ReadWrite is the
	// SumRead itself: the router rejects it the way a PLC in CONFIG does.
	srv.AMSErrorAfter(ams.CommandReadWrite, 1, ams.ReturnCodeGlobalTargetPortNotFound)

	values, err := sess.ReadValues(context.Background(), []string{"MAIN.a", "MAIN.b"})
	if err == nil {
		t.Fatal("got err = nil for a router-rejected batch")
	}
	var batchErr *BatchError
	if errors.As(err, &batchErr) {
		t.Errorf("got a *BatchError (%v) for a transport failure; per-item results are not knowable here", batchErr)
	}
	if !errors.Is(err, ams.ReturnCodeGlobalTargetPortNotFound) {
		t.Errorf("got err = %v, want it to match ReturnCodeGlobalTargetPortNotFound", err)
	}
	if values != nil {
		t.Errorf("got values = %v, want nil on transport failure", values)
	}
}

// TestReadValues_EmptyRequestIsNotAnError guards the boundary the
// contract keeps unchanged: asking for nothing is not a failure.
//
// Validates: DECISIONS.md Decision 1 (empty request stays nil, nil).
func TestReadValues_EmptyRequestIsNotAnError(t *testing.T) {
	srv := fakeplc.StartPLC(t)
	defer srv.Stop()

	sess, _ := newWiredTestSession(t, srv, WithSymbolVersionStrategy(SymbolVersionIgnore))

	for _, names := range [][]string{nil, {}} {
		values, err := sess.ReadValues(context.Background(), names)
		if err != nil || values != nil {
			t.Errorf("ReadValues(%v) = %v, %v; want nil, nil", names, values, err)
		}
	}
}

// TestWriteValues_DroppedItemIsNotSuccess pins the write half. A
// dropped item is absent from the returned map, and ReturnCodeNoErrors is 0, so
// the idiomatic per-symbol check reads a write that never happened as a write
// that succeeded. Only the error can distinguish them.
//
// Validates: DECISIONS.md Decision 1 (write-path dropped item).
func TestWriteValues_DroppedItemIsNotSuccess(t *testing.T) {
	srv := fakeplc.StartPLC(t)
	defer srv.Stop()

	sess, _ := newWiredTestSession(t, srv, WithSymbolVersionStrategy(SymbolVersionIgnore))
	seedSymbol(sess, "MAIN.a", 0x4501)
	seedSymbol(sess, "MAIN.bad", 0x4502)
	// MAIN.typo is not seeded: getSymbol fails and the setpoint never reaches
	// the PLC. MAIN.bad is a BOOL handed a non-boolean, the shape a type change
	// under an online change takes, and it dies at serialization instead.

	srv.OnWriteRead(ams.GroupSumupWrite, func(_ []byte) []byte {
		buf := make([]byte, 4)
		binary.LittleEndian.PutUint32(buf, uint32(ams.ReturnCodeNoErrors))
		return buf
	})

	codes, err := sess.WriteValues(context.Background(), map[string]any{
		"MAIN.a":    true,
		"MAIN.bad":  "not-a-bool",
		"MAIN.typo": true,
	})
	batchErr := batchErrorFor(t, err)

	if codes["MAIN.a"] != ams.ReturnCodeNoErrors {
		t.Errorf("MAIN.a: code = 0x%X, want 0x0 — the write that did land must still report success", uint32(codes["MAIN.a"]))
	}
	if _, ok := codes["MAIN.typo"]; ok {
		t.Error("MAIN.typo is present in the code map; a write that never happened must not carry a code")
	}
	item := itemFor(t, batchErr, "MAIN.typo")
	if !errors.Is(item.Skipped, ErrBatchSymbolUnresolved) {
		t.Errorf("MAIN.typo: Skipped = %v, want ErrBatchSymbolUnresolved", item.Skipped)
	}
	if bad := itemFor(t, batchErr, "MAIN.bad"); !errors.Is(bad.Skipped, ErrBatchValueUnserializable) {
		t.Errorf("MAIN.bad: Skipped = %v, want ErrBatchValueUnserializable", bad.Skipped)
	}
	if batchErr.Requested != 3 || batchErr.Succeeded != 1 {
		t.Errorf("got Requested=%d Succeeded=%d, want 3/1", batchErr.Requested, batchErr.Succeeded)
	}
}

// TestWriteValues_PerItemRejectionIsAnError covers the write path's
// device-verdict state: the batch reached the PLC and one item was rejected.
//
// Validates: DECISIONS.md Decision 1 (write-path PLC verdict).
func TestWriteValues_PerItemRejectionIsAnError(t *testing.T) {
	srv := fakeplc.StartPLC(t)
	defer srv.Stop()

	sess, _ := newWiredTestSession(t, srv, WithSymbolVersionStrategy(SymbolVersionIgnore))
	seedSymbol(sess, "MAIN.a", 0x4601)

	srv.OnWriteRead(ams.GroupSumupWrite, func(_ []byte) []byte {
		buf := make([]byte, 4)
		binary.LittleEndian.PutUint32(buf, uint32(ams.ReturnCodeDeviceSymbolNoFound))
		return buf
	})

	codes, err := sess.WriteValues(context.Background(), map[string]any{"MAIN.a": true})
	batchErr := batchErrorFor(t, err)

	if codes["MAIN.a"] != ams.ReturnCodeDeviceSymbolNoFound {
		t.Errorf("MAIN.a: code = 0x%X, want 0x710", uint32(codes["MAIN.a"]))
	}
	item := itemFor(t, batchErr, "MAIN.a")
	if item.Skipped != nil || item.Error != ams.ReturnCodeDeviceSymbolNoFound {
		t.Errorf("got item %+v, want a PLC verdict of 0x710", item)
	}
}

// TestBatchSymbols_FullSuccessIsNotAnError guards the other end of the
// contract: when every item succeeds there is no error at all. Without this,
// nothing stops the batch error from firing on a healthy batch — every caller
// would see a permanent failure.
//
// Validates: DECISIONS.md Decision 1 (error only when an item failed).
func TestBatchSymbols_FullSuccessIsNotAnError(t *testing.T) {
	srv := fakeplc.StartPLC(t)
	defer srv.Stop()

	sess, _ := newWiredTestSession(t, srv, WithSymbolVersionStrategy(SymbolVersionIgnore))
	seedSymbol(sess, "MAIN.a", 0x4701)
	seedSymbol(sess, "MAIN.b", 0x4702)

	srv.OnWriteRead(ams.GroupSumupReadEx2, func(_ []byte) []byte {
		return fakeplc.SumReadResponse(
			[]ams.ReturnCode{ams.ReturnCodeNoErrors, ams.ReturnCodeNoErrors},
			[]uint32{1, 1},
			[]byte{0x01, 0x00},
		)
	})
	srv.OnWriteRead(ams.GroupSumupWrite, func(_ []byte) []byte {
		return make([]byte, 8) // two items, both ReturnCodeNoErrors
	})

	values, err := sess.ReadValues(context.Background(), []string{"MAIN.a", "MAIN.b"})
	if err != nil {
		t.Errorf("ReadValues on a healthy batch: got err = %v, want nil", err)
	}
	if len(values) != 2 {
		t.Errorf("got values = %v, want both symbols", values)
	}

	codes, err := sess.WriteValues(context.Background(), map[string]any{
		"MAIN.a": true,
		"MAIN.b": false,
	})
	if err != nil {
		t.Errorf("WriteValues on a healthy batch: got err = %v, want nil", err)
	}
	if len(codes) != 2 {
		t.Errorf("got codes = %v, want both symbols", codes)
	}
}

// TestAMSRouterErrorIsNotADeviceVerdict pins the provenance invariant at the one
// line where it used to be lost: amsReply.payload() wraps the AMS header's
// ErrorCode, and the result must not look like something the PLC said about an
// item. Every abort guard in this package (cmd_sum.go's notification fallbacks
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
