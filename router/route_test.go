package router

import (
	"bytes"
	"context"
	"encoding/binary"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/siyka-au/go-ads/v3/internal/fakeplc"
)

var testRoute = Route{
	Name:         "go-ads-test",
	LocalNetID:   [6]byte{192, 168, 3, 52, 1, 1},
	ComputerName: "127.0.0.1",
	Username:     "Administrator",
	Password:     "1",
}

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)) }

// F-24: parseRouteResponse must reject a response whose invokeID does not
// match the expected value. This is the spoof defense.
//
// Validates: R-CMD-008.
func TestParseRouteResponse_RejectsInvokeIdMismatch(t *testing.T) {
	resp := make([]byte, 24)
	binary.LittleEndian.PutUint32(resp[0:], routeCookie)
	binary.LittleEndian.PutUint32(resp[4:], 0xCAFEBABE)                    // response invokeID
	binary.LittleEndian.PutUint32(resp[8:], responseFlag|serviceAddRoute) // valid serviceId

	err := parseRouteResponse(quietLogger(), resp, 0xDEADBEEF) // expecting different invokeID
	if err == nil {
		t.Fatalf("expected invokeID mismatch error, got nil")
	}
	if !strings.Contains(err.Error(), "invokeID") {
		t.Errorf("expected error to mention invokeID, got: %v", err)
	}
}

// parseRouteResponse with a matching invokeID but NO error tag must return an
// error. Previously the absence of tagResponseError was treated as success,
// which masked malformed/truncated PLC responses — caller would see Connect
// succeed and every subsequent ADS command fail with TargetNotFound.
//
// Validates: R-CMD-008.
func TestParseRouteResponse_NoErrorTagRejected(t *testing.T) {
	resp := make([]byte, 24)
	binary.LittleEndian.PutUint32(resp[0:], routeCookie)
	binary.LittleEndian.PutUint32(resp[4:], 0x12345678)
	binary.LittleEndian.PutUint32(resp[8:], responseFlag|serviceAddRoute)
	// tagCount=0; no error tag → must surface as error.

	if err := parseRouteResponse(quietLogger(), resp, 0x12345678); err == nil {
		t.Errorf("expected error for missing error tag, got nil")
	}
}

// F-24: buildRoutePacket must encode the provided invokeID at offset 4.
//
// Validates: R-CMD-008.
func TestBuildRoutePacket_EncodesInvokeId(t *testing.T) {
	pkt := buildRoutePacket(Route{LocalNetID: [6]byte{1, 2, 3, 4, 5, 6}, Name: "route", ComputerName: "host", Username: "admin", Password: "pwd"}, 0xABCDEF12)
	if len(pkt) < 24 {
		t.Fatalf("packet too short: %d", len(pkt))
	}
	if got := binary.LittleEndian.Uint32(pkt[4:]); got != 0xABCDEF12 {
		t.Errorf("invokeID in packet = 0x%08X, want 0xABCDEF12", got)
	}
}

// Validates: R-ROUTE-001.
func TestBuildRoutePacket(t *testing.T) {
	r := Route{
		Name:         "TestRoute",
		LocalNetID:   [6]byte{192, 168, 1, 100, 1, 1},
		ComputerName: "192.168.1.100",
		Username:     "Admin",
		Password:     "secret",
	}
	packet := buildRoutePacket(r, 0)

	if len(packet) < 24 {
		t.Fatalf("packet too short: %d bytes", len(packet))
	}
	if cookie := binary.LittleEndian.Uint32(packet[0:]); cookie != routeCookie {
		t.Errorf("cookie = 0x%08X, want 0x%08X", cookie, routeCookie)
	}
	if invokeID := binary.LittleEndian.Uint32(packet[4:]); invokeID != 0 {
		t.Errorf("invokeID = %d, want 0", invokeID)
	}
	if serviceID := binary.LittleEndian.Uint32(packet[8:]); serviceID != serviceAddRoute {
		t.Errorf("serviceID = %d, want %d", serviceID, serviceAddRoute)
	}

	// AmsAddr: NetID at offset 12, Port at offset 18
	var parsedNetID [6]byte
	copy(parsedNetID[:], packet[12:18])
	if parsedNetID != r.LocalNetID {
		t.Errorf("NetID = %v, want %v", parsedNetID, r.LocalNetID)
	}
	if port := binary.LittleEndian.Uint16(packet[18:]); port != 0 {
		t.Errorf("port = %d, want 0", port)
	}
	if tagCount := binary.LittleEndian.Uint32(packet[20:]); tagCount != 5 {
		t.Errorf("tagCount = %d, want 5", tagCount)
	}

	tagIDs := make(map[uint16]bool)
	offset := 24
	for offset+4 <= len(packet) {
		tid := binary.LittleEndian.Uint16(packet[offset:])
		tlen := binary.LittleEndian.Uint16(packet[offset+2:])
		tagIDs[tid] = true
		offset += 4 + int(tlen)
	}
	for _, expected := range []uint16{tagNetID, tagPassword, tagComputerName, tagRouteName, tagUsername} {
		if !tagIDs[expected] {
			t.Errorf("missing tag ID %d in packet", expected)
		}
	}
}

// Validates: R-ROUTE-001.
func TestParseRouteResponse_Success(t *testing.T) {
	result := make([]byte, 4)
	resp := fakeplc.Response(0, serviceAddRoute, [6]byte{}, 0, fakeplc.Tag{ID: fakeplc.TagResponseError, Data: result})
	if err := parseRouteResponse(slog.Default(), resp, 0); err != nil {
		t.Errorf("expected success, got error: %v", err)
	}
}

// Validates: R-ROUTE-001.
func TestParseRouteResponse_ErrorCode(t *testing.T) {
	resp := fakeplc.Response(0, serviceAddRoute, [6]byte{}, 0, fakeplc.Tag{ID: fakeplc.TagResponseError, Data: []byte{7, 0, 0, 0}})
	if err := parseRouteResponse(slog.Default(), resp, 0); err == nil {
		t.Error("expected error for non-zero error code")
	}
}

// Validates: R-ROUTE-001.
func TestParseRouteResponse_TooShort(t *testing.T) {
	if err := parseRouteResponse(slog.Default(), []byte{1, 2, 3}, 0); err == nil {
		t.Error("expected error for short response")
	}
}

// Validates: R-ROUTE-001.
func TestParseRouteResponse_WrongCookie(t *testing.T) {
	resp := make([]byte, 24)
	binary.LittleEndian.PutUint32(resp[0:], 0xDEADBEEF)
	binary.LittleEndian.PutUint32(resp[8:], responseFlag|serviceAddRoute)
	if err := parseRouteResponse(slog.Default(), resp, 0); err == nil {
		t.Error("expected error for wrong cookie")
	}
}

// Validates: R-ROUTE-001.
func TestParseRouteResponse_WrongServiceID(t *testing.T) {
	resp := make([]byte, 24)
	binary.LittleEndian.PutUint32(resp[0:], routeCookie)
	binary.LittleEndian.PutUint32(resp[8:], 0x12345678)
	if err := parseRouteResponse(slog.Default(), resp, 0); err == nil {
		t.Error("expected error for wrong serviceId")
	}
}

// F-25: a nil logger must not panic.
//
// Aimed at a local responder on an ephemeral port, never at 48899: passing a
// bare "127.0.0.1" made this test write a real registration datagram (credentials
// in cleartext) to the host's own AMS router port, and burn the full retransmit
// budget whenever anything held that port. Asserting err and the registration
// count also covers the host:port form.
func TestAddRoute_NilLogger(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("panicked with nil logger: %v", r)
		}
	}()
	r := fakeplc.NewRouter(t)
	if err := AddRoute(t.Context(), r.Addr(), testRoute, WithLogger(nil)); err != nil {
		t.Fatalf("registration against the local responder failed: %v", err)
	}
	if got := r.Registrations(); got != 1 {
		t.Errorf("registration datagrams = %d, want 1", got)
	}
}

// TestAddRoute_RetransmitsOnALostDatagram: one dropped UDP datagram must not fail
// a registration.
//
// Identify already retransmits three times, with a comment recording why: a single
// dropped datagram was observed failing NewSession outright. Registration runs on
// the same plant networks over the same shared port 48899, and its failure is the
// costlier one — Connect aborts on it.
func TestAddRoute_RetransmitsOnALostDatagram(t *testing.T) {
	r := fakeplc.NewRouter(t)
	r.DropAdds.Store(1) // the first attempt is lost

	if err := AddRoute(t.Context(), r.Addr(), testRoute, WithLogger(quietLogger())); err != nil {
		t.Fatalf("AddRoute gave up after one lost datagram: %v", err)
	}
	if got := r.Registrations(); got < 2 {
		t.Errorf("registration datagrams sent = %d, want at least 2: the first was dropped, so a single-shot send "+
			"cannot have succeeded", got)
	}
}

// TestAddRoute_IgnoresAnUnrelatedDatagram: a datagram that is not our answer must
// be skipped, not treated as a failure.
//
// The noise has to come from the responder's own address: the client dials a
// CONNECTED socket, so it only ever receives from the address it dialled.
// Verified by mutation — with noise from a second socket, making the skip branch
// fail immediately left the test green.
func TestAddRoute_IgnoresAnUnrelatedDatagram(t *testing.T) {
	r := fakeplc.NewRouter(t)
	r.NoiseFirst.Store(1)

	if err := AddRoute(t.Context(), r.Addr(), testRoute, WithLogger(quietLogger())); err != nil {
		t.Errorf("registration failed because of a datagram that was not its answer: %v", err)
	}
	// One send: the junk must be skipped within the same window, not answered by a
	// retransmit.
	if got := r.Registrations(); got != 1 {
		t.Errorf("registration datagrams sent = %d, want 1: the junk should be skipped inside the read window, "+
			"not waited out until a retransmit", got)
	}
}

// TestAddRoute_RefusalIsNotRetransmitted: an answer that says no must be reported
// at once.
//
// parseRouteResponse's error covered both "not ours" and "ours, and refused", and
// both landed in the skip-and-keep-reading branch. So a wrong password — answered
// immediately by the router — was waited out for the whole budget and the packet,
// password included, went on the wire three times.
func TestAddRoute_RefusalIsNotRetransmitted(t *testing.T) {
	r := fakeplc.NewRouter(t)
	r.Reply.Store(int64(0x706)) // whatever the router says no with

	route := testRoute
	route.Password = "wrong"
	start := time.Now()
	err := AddRoute(t.Context(), r.Addr(), route, WithLogger(quietLogger()))
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("registration reported success although the router refused it")
	}
	if got := r.Registrations(); got != 1 {
		t.Errorf("the refusal was retransmitted %d times; the answer was already in hand, and each retransmission puts the "+
			"route password on the wire again", got)
	}
	if elapsed > 2*time.Second {
		t.Errorf("took %v to report a refusal answered immediately; the caller waited out the retransmit budget", elapsed)
	}
}

// TestAddRoute_CancelInterruptsTheRead: a cancelled context ends the call at
// once rather than when the 6s budget runs out. Session relied on a detached
// goroutine for this before AddRoute took a context.
func TestAddRoute_CancelInterruptsTheRead(t *testing.T) {
	r := fakeplc.NewRouter(t)
	r.DropAdds.Store(100) // never answers
	ctx, cancel := context.WithCancel(t.Context())
	time.AfterFunc(100*time.Millisecond, cancel)
	start := time.Now()
	if err := AddRoute(ctx, r.Addr(), testRoute, WithLogger(quietLogger())); err == nil {
		t.Fatal("registration against a silent router succeeded")
	}
	if waited := time.Since(start); waited > time.Second {
		t.Errorf("returned %v after cancellation; the read was not interrupted", waited)
	}
}

// TestRoute_LogValueHidesPassword: a Route logged as a value must not carry the
// password, since registration is logged at Info.
func TestRoute_LogValueHidesPassword(t *testing.T) {
	var buf bytes.Buffer
	route := testRoute
	route.Password = "hunter2-do-not-log"
	slog.New(slog.NewTextHandler(&buf, nil)).Info("x", "route", route)
	if strings.Contains(buf.String(), route.Password) || strings.Contains(strings.ToLower(buf.String()), "password") {
		t.Errorf("password leaked into the log: %s", buf.String())
	}
	if !strings.Contains(buf.String(), "hasAuth=true") {
		t.Errorf("log record lost the auth flag: %s", buf.String())
	}
}

func TestBuildTag(t *testing.T) {
	tag := buildTag(0x0102, []byte{0xAA, 0xBB})
	want := []byte{0x02, 0x01, 0x02, 0x00, 0xAA, 0xBB}
	if !bytes.Equal(tag, want) {
		t.Errorf("buildTag = % X, want % X", tag, want)
	}
}

func TestAppendNull(t *testing.T) {
	if got := appendNull([]byte("abc")); !bytes.Equal(got, []byte{'a', 'b', 'c', 0}) {
		t.Errorf("appendNull = %v", got)
	}
}
