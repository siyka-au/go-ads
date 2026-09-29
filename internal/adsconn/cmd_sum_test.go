package adsconn

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/siyka-au/go-ads/v3/ams"
	"github.com/siyka-au/go-ads/v3/internal/fakeplc"
)

// --- downgradeTransMode ---

// Validates: R-NOT-011.
func TestDowngradeTransMode(t *testing.T) {
	tests := []struct {
		input    ams.TransMode
		expected ams.TransMode
	}{
		{ams.TransModeServerOnChange2, ams.TransModeServerOnChange},
		{ams.TransModeServerCycle2, ams.TransModeServerCycle},
		{ams.TransModeServerOnChange, ams.TransModeServerOnChange},
		{ams.TransModeServerCycle, ams.TransModeServerCycle},
		{ams.TransModeClientCycle, ams.TransModeClientCycle},
		{ams.TransModeNoTransmission, ams.TransModeNoTransmission},
	}

	for _, tt := range tests {
		t.Run(tt.input.String(), func(t *testing.T) {
			result := DowngradeTransMode(tt.input)
			if result != tt.expected {
				t.Errorf("downgradeTransMode(%v) = %v, want %v", tt.input, result, tt.expected)
			}
		})
	}
}

// F-09: a malicious / buggy PLC sending lengths[i] = 0xFFFFFFFE must not cause
// a negative int cast (32-bit Go) or huge make() allocation.
// On 64-bit Go this is defense-in-depth; on 32-bit it is a real bug.
// Validates: R-SUM-006.
func TestParseSumReadResponse_LengthOverflow(t *testing.T) {
	conn := &Conn{logger: slog.Default()}

	resp := fakeplc.SumReadResponse(
		[]ams.ReturnCode{ams.ReturnCodeNoErrors},
		[]uint32{0xFFFFFFFE},
		[]byte{0x01, 0x02, 0x03, 0x04},
	)
	requests := []ams.SumReadRequest{{Length: 4}}

	results, err := conn.parseSumReadResponse(resp, 1, requests)
	if err != nil {
		t.Fatalf("unexpected outer error: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].Error != ams.ReturnCodeDeviceInvalidSize {
		t.Errorf("expected ReturnCodeDeviceInvalidSize, got %v", results[0].Error)
	}
}

// F-10: truncated response must emit an Error log so wire corruption is
// distinguishable from genuine PLC errors.
// Validates: R-SUM-006.
func TestParseSumReadResponse_TruncationLogsError(t *testing.T) {
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	conn := &Conn{logger: logger}

	// Two items: first declares length=8, second declares length=4. Data section
	// has only 4 bytes total — first item alone exceeds remaining bytes.
	resp := fakeplc.SumReadResponse(
		[]ams.ReturnCode{ams.ReturnCodeNoErrors, ams.ReturnCodeNoErrors},
		[]uint32{8, 4},
		[]byte{0x01, 0x02, 0x03, 0x04},
	)
	requests := []ams.SumReadRequest{{Length: 8}, {Length: 4}}

	results, err := conn.parseSumReadResponse(resp, 2, requests)
	if err != nil {
		t.Fatalf("unexpected outer error: %v", err)
	}
	if results[0].Error != ams.ReturnCodeDeviceInvalidSize {
		t.Errorf("results[0].Error = %v, want ReturnCodeDeviceInvalidSize", results[0].Error)
	}
	if results[1].Error != ams.ReturnCodeDeviceInvalidSize {
		t.Errorf("results[1].Error = %v, want ReturnCodeDeviceInvalidSize", results[1].Error)
	}

	logOut := logBuf.String()
	if !strings.Contains(logOut, "SumRead truncated") {
		t.Errorf("expected truncation log, got: %s", logOut)
	}
}

// F-11: PLC oversizing one item's response shifts later items' offsets.
// Defense: reject when lengths[i] > requests[i].Length even if total bytes
// fit in the response.
// Validates: R-SUM-006.
func TestParseSumReadResponse_PerItemOversize(t *testing.T) {
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	conn := &Conn{logger: logger}

	// Two items each requested as 4 bytes, but item 0 declares 8 bytes returned.
	// Total response size accommodates 8+4=12 data bytes so the gross truncation
	// guard (F-09) does NOT fire; only the per-item check (F-11) catches it.
	resp := fakeplc.SumReadResponse(
		[]ams.ReturnCode{ams.ReturnCodeNoErrors, ams.ReturnCodeNoErrors},
		[]uint32{8, 4},
		[]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12},
	)
	requests := []ams.SumReadRequest{{Length: 4}, {Length: 4}}

	results, err := conn.parseSumReadResponse(resp, 2, requests)
	if err != nil {
		t.Fatalf("unexpected outer error: %v", err)
	}
	if results[0].Error != ams.ReturnCodeDeviceInvalidSize {
		t.Errorf("results[0].Error = %v, want ReturnCodeDeviceInvalidSize", results[0].Error)
	}
	if results[1].Error != ams.ReturnCodeDeviceInvalidSize {
		t.Errorf("results[1].Error = %v, want ReturnCodeDeviceInvalidSize", results[1].Error)
	}

	logOut := logBuf.String()
	if !strings.Contains(logOut, "per-item oversize") {
		t.Errorf("expected per-item oversize log, got: %s", logOut)
	}
}

// --- SumRead overflow guard ---

// TestSumReadOverflowGuard exercises the production guard in
// Client.SumRead that rejects request slices whose summed Length plus
// per-item header overhead would overflow uint32. Drives the production
// code path with a synthetic two-request slice that crosses MaxUint32
// and asserts SumRead returns the documented error.
//
// Validates: R-SUM-006 (data-section integrity).
func TestSumReadOverflowGuard(t *testing.T) {
	// Build a Client without a live transport. SumRead's overflow check
	// runs before any wire I/O, so we never reach sendRequest. The
	// capabilities zero-value (sumReadCmd == 0) routes the call through
	// the probe path, which still computes totalReadLen first.
	c := &Conn{
		logger: slog.Default(),
		tx: &Transport{
			sendChannel:    make(chan []byte),
			systemResponse: make(chan []byte),
			recvQueue:      make(chan []byte),
			activeRequests: map[uint32]chan amsReply{},
		},
	}
	requests := []ams.SumReadRequest{
		{Group: 1, Offset: 0, Length: math.MaxUint32},
		{Group: 1, Offset: 0, Length: 1}, // total > MaxUint32
	}

	_, err := c.SumRead(context.Background(), requests)
	if err == nil {
		t.Fatal("expected overflow error, got nil")
	}
	if !strings.Contains(err.Error(), "exceeds uint32 max") {
		t.Errorf("expected 'exceeds uint32 max' error, got: %v", err)
	}
}

// --- isSumCommandUnsupportedError ---

// Validates: R-SUM-001/R-SUM-002 (partial).
func TestIsSumCommandUnsupportedError(t *testing.T) {
	tests := []struct {
		err  error
		want bool
	}{
		{ams.ReturnCodeDeviceServiceNotSupported, true},
		{ams.ReturnCodeGlobalUnknownCommandID, true},
		{ams.ReturnCodeGlobalUnknownAdsCommand, true},
		{ams.ReturnCodeDeviceBusy, false},
		{ams.ReturnCodeDeviceTimeout, false},
		{fmt.Errorf("network error"), false},
		{nil, false},
	}
	for _, tt := range tests {
		got := isSumCommandUnsupportedError(tt.err)
		if got != tt.want {
			t.Errorf("isSumCommandUnsupportedError(%v) = %v, want %v", tt.err, got, tt.want)
		}
	}
}

// --- Sum probe state CAS ---

// Validates: R-SUM-003.
func TestSumProbeStateTransitions(t *testing.T) {
	// Verify CAS 0→1 and 0→2 work, and second CAS is rejected
	conn := &Conn{}

	// sumWriteState: 0→1
	if !conn.Capabilities().SumWriteStateCAS(0, 1) {
		t.Error("CAS 0→1 should succeed")
	}
	if conn.Capabilities().SumWriteStateLoad() != 1 {
		t.Error("state should be 1")
	}
	// Second goroutine trying 0→2 should fail
	if conn.Capabilities().SumWriteStateCAS(0, 2) {
		t.Error("CAS 0→2 should fail when state is 1")
	}

	// sumAddNotifState: 0→2
	if !conn.Capabilities().SumAddNotifStateCAS(0, 2) {
		t.Error("CAS 0→2 should succeed")
	}
	if conn.Capabilities().SumAddNotifStateLoad() != 2 {
		t.Error("state should be 2")
	}
	// sumDeleteNotifState independent of sumAddNotifState
	if conn.Capabilities().SumDeleteNotifStateLoad() != 0 {
		t.Error("delete state should still be 0 (independent of add)")
	}

	// Reset works
	conn.Capabilities().SumWriteStateStore(0)
	if conn.Capabilities().SumWriteStateLoad() != 0 {
		t.Error("reset should set state to 0")
	}
}

// Validates: R-SUM-003 / R-LOCK-003.
func TestSumProbeStateConcurrent(t *testing.T) {
	// Concurrent CAS: only one goroutine should win
	conn := &Conn{}
	const goroutines = 100
	var wg sync.WaitGroup
	wins := make(chan uint32, goroutines)

	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func(id int) {
			defer wg.Done()
			val := uint32(1)
			if id%2 == 0 {
				val = 2
			}
			if conn.Capabilities().SumWriteStateCAS(0, val) {
				wins <- val
			}
		}(i)
	}
	wg.Wait()
	close(wins)

	count := 0
	for range wins {
		count++
	}
	if count != 1 {
		t.Errorf("expected exactly 1 CAS winner, got %d", count)
	}
}

// TestSumReadFallback_PreservesADSReturnCode validates that sumReadFallback
// propagates the real ADS ReturnCode from c.Read rather than masking it
// with ReturnCodeDeviceError. This is required so that
// readMultipleSymbolsRetry can detect stale-cache codes (e.g.
// ReturnCodeDeviceSymbolVersionInvalid) in per-item results.
//
// Validates: fallback error preservation (fix for sumReadFallback masking).
func TestSumReadFallback_PreservesADSReturnCode(t *testing.T) {
	srv := fakeplc.StartPLC(t)
	defer srv.Stop()

	// Register a Read handler for an arbitrary group that returns the stale code.
	const testGroup ams.Group = 0xABCD1234
	srv.OnRead(testGroup, func(_, _, _ uint32) (ams.ReturnCode, []byte) {
		return ams.ReturnCodeDeviceSymbolVersionInvalid, nil
	})

	c, err := dialTest(srv.Host, srv.Port, ams.Address{}, ams.Address{}, 2*time.Second)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	reqs := []ams.SumReadRequest{{Group: uint32(testGroup), Offset: 0, Length: 4}}
	results, err := c.sumReadFallback(context.Background(), reqs)
	if err != nil {
		t.Fatalf("sumReadFallback returned unexpected top-level error: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].Error != ams.ReturnCodeDeviceSymbolVersionInvalid {
		t.Errorf("results[0].Error = %v, want ReturnCodeDeviceSymbolVersionInvalid", results[0].Error)
	}
}

// TestParseSumReadResponse_ErroredItemAdvancesOffset pins the deliberate
// behaviour that an errored item still consumes its declared length in the
// response data section so subsequent items remain aligned. Without this,
// a successful follow-up item parses the wrong bytes and silently corrupts
// the caller's data. A regression that skips dataOffset += lengths[i] for
// errored items would break this test on item 2's payload comparison.
func TestParseSumReadResponse_ErroredItemAdvancesOffset(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
	conn := &Conn{logger: logger}

	// Three items: OK(4 bytes), ERR(declared 4 bytes), OK(4 bytes).
	// If alignment regresses, item 2 reads bytes 4..7 (item 1's payload)
	// instead of bytes 8..11.
	item1 := []byte{0xAA, 0xAA, 0xAA, 0xAA}
	item1Err := []byte{0xBB, 0xBB, 0xBB, 0xBB} // PLC may or may not send these
	item2 := []byte{0xCC, 0xCC, 0xCC, 0xCC}
	data := append(append(append([]byte{}, item1...), item1Err...), item2...)

	resp := fakeplc.SumReadResponse(
		[]ams.ReturnCode{ams.ReturnCodeNoErrors, ams.ReturnCodeDeviceSymbolVersionInvalid, ams.ReturnCodeNoErrors},
		[]uint32{4, 4, 4},
		data,
	)
	requests := []ams.SumReadRequest{{Length: 4}, {Length: 4}, {Length: 4}}

	results, err := conn.parseSumReadResponse(resp, 3, requests)
	if err != nil {
		t.Fatalf("unexpected outer error: %v", err)
	}

	if results[0].Error != ams.ReturnCodeNoErrors {
		t.Errorf("results[0].Error = %v, want NoErrors", results[0].Error)
	}
	if !bytes.Equal(results[0].Data, item1) {
		t.Errorf("results[0].Data = %v, want %v", results[0].Data, item1)
	}
	if results[1].Error != ams.ReturnCodeDeviceSymbolVersionInvalid {
		t.Errorf("results[1].Error = %v, want SymbolVersionInvalid", results[1].Error)
	}
	if results[2].Error != ams.ReturnCodeNoErrors {
		t.Errorf("results[2].Error = %v, want NoErrors (alignment preserved across errored item)", results[2].Error)
	}
	if !bytes.Equal(results[2].Data, item2) {
		t.Errorf("results[2].Data = %v, want %v — errored item must advance dataOffset", results[2].Data, item2)
	}
}

// TestParseSumReadResponse_ErroredItemOverflowsRemaining: an errored item
// declaring a length exceeding the bytes remaining in the response data
// section must cascade-mark every remaining item DeviceInvalidSize, mirroring
// the per-item-oversize / truncation path. Prevents silent garbage parsing.
func TestParseSumReadResponse_ErroredItemOverflowsRemaining(t *testing.T) {
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	conn := &Conn{logger: logger}

	// Two items, second errored with absurd declared length, only 4 actual data bytes.
	resp := fakeplc.SumReadResponse(
		[]ams.ReturnCode{ams.ReturnCodeNoErrors, ams.ReturnCodeDeviceSymbolVersionInvalid},
		[]uint32{4, 0xFFFFFFFE}, // item 1 errored, claims ~4 GiB; remaining < that
		[]byte{1, 2, 3, 4},
	)
	requests := []ams.SumReadRequest{{Length: 4}, {Length: 4}}

	results, err := conn.parseSumReadResponse(resp, 2, requests)
	if err != nil {
		t.Fatalf("unexpected outer error: %v", err)
	}
	if results[1].Error != ams.ReturnCodeDeviceInvalidSize {
		t.Errorf("results[1].Error = %v, want DeviceInvalidSize (errored-item overflow cascade)", results[1].Error)
	}
	if !strings.Contains(logBuf.String(), "errored-item declared length exceeds remaining bytes") {
		t.Errorf("expected errored-item-overflow log, got: %s", logBuf.String())
	}
}

// TestAMSReplyPayload_RouterError: a reply carrying an AMS ErrorCode is the
// router's refusal, returned as an ams.RouterError rather than a body to parse.
func TestAMSReplyPayload_RouterError(t *testing.T) {
	_, err := amsReply{amsErr: ams.ReturnCodeGlobalTargetPortNotFound}.payload()
	var re ams.RouterError
	if !errors.As(err, &re) || re.Code != ams.ReturnCodeGlobalTargetPortNotFound {
		t.Fatalf("payload() = %v, want ams.RouterError{TargetPortNotFound}", err)
	}
	// Only device codes may drive capability latching; a router code reaching it
	// would mark a sum command unsupported for the life of the connection.
	if isSumCommandUnsupportedError(fmt.Errorf("outer: %w", err)) {
		t.Error("isSumCommandUnsupportedError = true for a router rejection, want false")
	}
}
