package adsconn

import (
	"context"
	"errors"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/siyka-au/go-ads/v3/internal/fakeplc"

	"github.com/siyka-au/go-ads/v3/ams"
)

// TestPeerRoute_AdoptionAfterTeardownIsRefused: once the adopted connections have
// been dropped for a teardown, the Client must refuse any further one.
//
// A peer-route device dials us again after every drop, which is precisely when
// teardown runs. tearDownAndReset does closePeerConns() and then waits on the
// Client's WaitGroup, so an adoption landing in between is three bugs at once: the
// connection is appended to a slice that was just nil'd so nobody ever closes it
// (one leaked fd per reconnect, and a half-open socket the PLC counts against its
// one-connection-per-IP limit), its reader blocks in io.ReadFull so the wait never
// returns, and the WaitGroup.Add races a Wait that may already be at zero, which
// is documented misuse and panics the process.
func TestPeerRoute_AdoptionAfterTeardownIsRefused(t *testing.T) {
	srv := fakeplc.StartPLC(t)
	defer srv.Stop()

	c, err := dialTest(srv.Host, srv.Port, ams.Address{}, ams.Address{}, 2*time.Second)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	// Teardown has begun: the adopted connections were dropped and the caller is
	// about to wait for the readers.
	c.closePeerConns()

	inbound, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(srv.Port)))
	if err != nil {
		t.Fatalf("dial inbound: %v", err)
	}
	defer func() { _ = inbound.Close() }()

	c.AcceptPeerConn(inbound)

	c.peerMu.Lock()
	adopted := len(c.peerConns)
	c.peerMu.Unlock()
	if adopted != 0 {
		t.Errorf("peerConns holds %d connection(s) after closePeerConns: appended to a slice nobody will close again", adopted)
	}

	// The refusal has to close the socket, or it leaks: our end stays readable
	// forever instead of seeing the peer hang up.
	_ = inbound.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 1)
	_, rerr := inbound.Read(buf)
	if rerr == nil {
		t.Fatal("read succeeded on a connection that should have been refused and closed")
	}
	var ne net.Error
	if errors.As(rerr, &ne) && ne.Timeout() {
		t.Error("the refused connection was left open (read timed out instead of seeing EOF): " +
			"its reader is still blocked in io.ReadFull, which is what hangs tearDownAndReset's wait")
	}
}

// TestPeerRoute_AdoptedConnectionIsClosedWhenItsReaderExits: the reader owns the
// socket it was handed.
//
// readFrames returns on ctx.Done() and on any read error without touching conn,
// and nothing pruned peerConns, so every connection the PLC ever opened cost an fd
// and a slice entry for the life of the Client. A device that re-dials on each of
// its own drops accumulates them.
func TestPeerRoute_AdoptedConnectionIsClosedWhenItsReaderExits(t *testing.T) {
	srv := fakeplc.StartPLC(t)
	defer srv.Stop()

	c, err := dialTest(srv.Host, srv.Port, ams.Address{}, ams.Address{}, 2*time.Second)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	ours, theirs := net.Pipe()
	c.AcceptPeerConn(theirs)
	c.peerMu.Lock()
	adopted := len(c.peerConns)
	c.peerMu.Unlock()
	if adopted != 1 {
		t.Fatalf("peerConns = %d after a live adoption, want 1", adopted)
	}

	// End the reader the way a PLC hanging up does.
	_ = ours.Close()

	deadline := time.Now().Add(3 * time.Second)
	for {
		c.peerMu.Lock()
		n := len(c.peerConns)
		c.peerMu.Unlock()
		if n == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("peerConns still holds %d entry after its reader exited: entries are never pruned, so they accumulate for the life of the Client", n)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// --- Session wiring helper ---

// TestScriptableServer_Smoke validates the helper itself: register a
// Read handler, drive a single Read via *Client, and assert the canonical
// response builder + frame recording work end-to-end.
func TestScriptableServer_Smoke(t *testing.T) {
	srv := fakeplc.StartPLC(t)
	defer srv.Stop()

	srv.OnRead(ams.GroupSymbolVersion, func(_, _, _ uint32) (ams.ReturnCode, []byte) {
		return ams.ReturnCodeNoErrors, []byte{0x42}
	})

	c, err := dialTest(srv.Host, srv.Port, ams.Address{}, ams.Address{}, 2*time.Second)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	got, err := c.Read(context.Background(), uint32(ams.GroupSymbolVersion), 0, 1)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(got) != 1 || got[0] != 0x42 {
		t.Errorf("Read = %v, want [0x42]", got)
	}
	if len(srv.Frames()) == 0 {
		t.Errorf("frames() = 0, want at least 1")
	}
}
