package ads

import (
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// transport owns the TCP socket, per-invoke request multiplexing, and the
// channels listen and transmit use. recvQueue + recvWorkers are a bounded pool for
// inbound notifications and responses: listen used to spawn a goroutine per
// packet, unbounded under a misbehaving PLC. Overflow is dropped with a Warn.
type transport struct {
	connMu         sync.Mutex   // protects connection field against concurrent Close/Reconnect
	chanMu         sync.RWMutex // protects sendChannel and systemResponse against concurrent access during reconnect
	connection     net.Conn
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
	amsErr ReturnCode
}

// payload returns the response body, or the AMS-level error the router reported --
// in which case the request never reached a service that could answer, so the body
// is not a response. Returned as an AMSError, not a bare ReturnCode: this is where
// the two provenances used to become indistinguishable, and the abort guards tell
// them apart by type.
func (r amsReply) payload() ([]byte, error) {
	if r.amsErr != 0 {
		return nil, AMSError{Code: r.amsErr}
	}
	return r.data, nil
}

// configureKeepAlive enables aggressive TCP keepalive on a connection.
// With Idle=3s, Interval=2s, Count=5: connection declared dead after ~13s of no response.
func configureKeepAlive(c net.Conn) {
	if tc, ok := c.(*net.TCPConn); ok {
		tc.SetKeepAliveConfig(net.KeepAliveConfig{
			Enable:   true,
			Idle:     3 * time.Second,
			Interval: 2 * time.Second,
			Count:    5,
		})
	}
}
