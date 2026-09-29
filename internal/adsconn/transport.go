package adsconn

import (
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/siyka-au/go-ads/v3/ams"
)

// Transport owns the TCP socket, per-invoke request multiplexing, and the
// channels listen and transmit use. recvQueue + recvWorkers are a bounded pool for
// inbound notifications and responses: listen used to spawn a goroutine per
// packet, unbounded under a misbehaving PLC. Overflow is dropped with a Warn.
//
// A Session keeps one Transport for its whole life and wires a fresh Conn to it
// on every (re)dial, so everything that must survive a redial lives here.
type Transport struct {
	connMu     sync.Mutex // protects connection and source against concurrent Close/Reconnect
	connection net.Conn
	// source is the AMS address stamped on every request. One copy, here, for
	// every Conn wired to this transport: a local-mode handshake learns the real
	// address only after the Conn is running, and a copy per Conn let the two
	// disagree.
	source ams.Address

	chanMu         sync.RWMutex // protects sendChannel and systemResponse against concurrent access during reconnect
	sendChannel    chan []byte
	systemResponse chan []byte
	recvQueue      chan []byte // bounded inbound packet queue feeding recvWorkers

	currentRequest    atomic.Uint32
	activeRequestLock sync.Mutex // protects activeRequests against concurrent map access
	activeRequests    map[uint32]chan amsReply

	// disconnected reflects whether the underlying socket is usable for
	// sending. Flipped to false by Client.Dial after a successful TCP dial
	// (and by Session.dialAndStart on reconnect); flipped to true on
	// triggerReconnect, Reconnect entry, resetForRetry, and Close.
	// Lives on transport so the transport-down signal is co-located with the
	// socket it represents (rather than on a lifecycle struct that doesn't
	// own the connection).
	disconnected atomic.Bool
}

// NewTransport returns a transport with no connection yet.
func NewTransport(source ams.Address) *Transport {
	t := &Transport{source: source}
	t.ResetQueues()
	return t
}

// ResetQueues replaces the channels and the pending-request table, so a
// transport can be wired to a new Conn after the previous one's workers exited.
func (t *Transport) ResetQueues() {
	t.chanMu.Lock()
	t.sendChannel = make(chan []byte)
	t.systemResponse = make(chan []byte, 1)
	t.recvQueue = make(chan []byte, recvQueueSize)
	t.chanMu.Unlock()
	t.activeRequestLock.Lock()
	t.activeRequests = map[uint32]chan amsReply{}
	t.activeRequestLock.Unlock()
}

// Conn returns the current connection, or nil.
func (t *Transport) Conn() net.Conn {
	t.connMu.Lock()
	defer t.connMu.Unlock()
	return t.connection
}

// SetConn installs a freshly dialled connection.
func (t *Transport) SetConn(c net.Conn) {
	t.connMu.Lock()
	t.connection = c
	t.connMu.Unlock()
}

// CloseConn closes the connection, which is what unblocks a listen goroutine
// stuck in a read, and returns its local port: read before the Close, since
// LocalAddr on a closed connection is not reliable, and the port is what lines
// a drop up against a packet capture. It keeps the closed connection in place,
// so a late Write fails rather than dereferencing nil.
func (t *Transport) CloseConn() (localPort int) {
	t.connMu.Lock()
	defer t.connMu.Unlock()
	if t.connection == nil {
		return 0
	}
	if addr, ok := t.connection.LocalAddr().(*net.TCPAddr); ok {
		localPort = addr.Port
	}
	_ = t.connection.Close()
	return localPort
}

// DiscardConn closes and forgets the connection, for a dial that completed after
// its session was closed and so must never be used.
func (t *Transport) DiscardConn() {
	t.connMu.Lock()
	if t.connection != nil {
		_ = t.connection.Close()
		t.connection = nil
	}
	t.connMu.Unlock()
}

// Source returns the AMS address stamped on outgoing requests.
func (t *Transport) Source() ams.Address {
	t.connMu.Lock()
	defer t.connMu.Unlock()
	return t.source
}

// SetSource replaces the AMS address stamped on outgoing requests. It takes
// effect on the next request, including on a Conn already running.
func (t *Transport) SetSource(a ams.Address) {
	t.connMu.Lock()
	t.source = a
	t.connMu.Unlock()
}

// Disconnected reports whether the socket is known to be unusable. Requests
// fail fast with ErrTransportClosed while it is set.
func (t *Transport) Disconnected() bool { return t.disconnected.Load() }

// SetDisconnected marks the socket usable or not. The owner clears it only once
// a Conn's workers are running, so a request that sees false finds a transmit
// worker to take it.
func (t *Transport) SetDisconnected(v bool) { t.disconnected.Store(v) }

// recvWorkerCount is the number of goroutines consuming recvQueue. Each
// worker handles one packet at a time end-to-end. Sizing: high enough that
// notification storms don't bottleneck on cache.lock contention, low enough
// that a runaway PLC can't allocate goroutines without bound.
const recvWorkerCount = 16

// recvQueueSize is the buffer between listen and recvWorkers. Beyond this,
// listen drops packets with a Warn log.
const recvQueueSize = 256

// amsReply is one response handed back to the goroutine that issued the request.
//
// It carries the AMS header's ErrorCode alongside the payload because that field
// was previously discarded entirely: an AMS-level rejection (target port not found,
// no runtime, invalid NetID) arrives with a body that is not a valid response, and
// parsing it anyway produced fabricated diagnostics — a read of index group 0xF008
// reporting "0xF008: unknown error code", which is the group, not a code.
type amsReply struct {
	data   []byte
	amsErr ams.ReturnCode
}

// payload returns the response body, or the AMS-level error the router reported --
// in which case the request never reached a service that could answer, so the body
// is not a response. Returned as an RouterError, not a bare ReturnCode: this is where
// the two provenances used to become indistinguishable, and the abort guards tell
// them apart by type.
func (r amsReply) payload() ([]byte, error) {
	if r.amsErr != 0 {
		return nil, ams.RouterError{Code: r.amsErr}
	}
	return r.data, nil
}

// configureKeepAlive enables aggressive TCP keepalive on a connection.
// With Idle=3s, Interval=2s, Count=5: connection declared dead after ~13s of no response.
func ConfigureKeepAlive(c net.Conn) {
	if tc, ok := c.(*net.TCPConn); ok {
		tc.SetKeepAliveConfig(net.KeepAliveConfig{
			Enable:   true,
			Idle:     3 * time.Second,
			Interval: 2 * time.Second,
			Count:    5,
		})
	}
}

// MarkDisconnected sets the disconnected flag and reports whether this call
// set it, so exactly one of several concurrent detectors acts on a drop.
func (t *Transport) MarkDisconnected() bool { return t.disconnected.CompareAndSwap(false, true) }
