// Package ads is a pure-Go client for the Beckhoff TwinCAT ADS protocol.
//
// Two layers, chosen at construction. Client (this file) is a thin RPC layer: one
// TCP connection, raw AMS framing, no cache or reconnect -- once it drops, build a
// new one. Session (session.go) wraps it with the symbol cache, persistent
// notifications, auto-reconnect and an FSM.
//
// Session does NOT embed *Client: raw methods live on *Client, cache-aware ones on
// *Session. See docs/archive/specs/09-fsm-design.md.
package ads

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/siyka-au/go-ads/v3/ams"
)

// droppedResponseGrace is how long a request already waiting keeps waiting after
// the transport is seen dead. The drop closes its channel immediately while a
// reply parsed a moment earlier is still in flight to a recvWorker, and without
// the grace the drop wins that race -- measured at 40/40 replies discarded against
// a stub that answers then closes. Only a request with a reply in flight pays it.
const droppedResponseGrace = 100 * time.Millisecond

// NotificationHandler is the callback the recvWorker invokes when it decodes
// a DeviceNotification packet. Session installs its handleNotification here;
// raw Client consumers (web ADS browser, CLI tools, AMS routers) install
// their own. ctx is the Client's internal worker context — observe Done()
// to abort long-running handler work on Close. handle, timestamp, content
// are the parsed notification fields (handle = PLC-assigned ID, timestamp =
// Windows FILETIME, content = raw payload bytes).
type NotificationHandler func(ctx context.Context, handle uint32, timestamp uint64, content []byte)

// Client is the Beckhoff-equivalent thin RPC layer. One TCP connection, raw
// AMS framing, request multiplexing via InvokeID, listen + transmit + recv
// worker goroutines. No cache, no FSM, no reconnect, no callbacks.
//
// Lifetime states are exactly two: alive (Dial succeeded, Close not yet
// called, transport not yet observed as dropped) and closed. Once closed,
// every public method returns ErrTransportClosed.
type Client struct {
	ip   string
	port int

	target ams.Address
	source ams.Address

	requestTimeout time.Duration
	logger         *slog.Logger

	tx *transport

	capabilities capabilities //nolint:unused // capability state lives here, accessed via Client methods.

	// notify is invoked from recvWorker when a DeviceNotification packet
	// arrives. nil means raw Client (no dispatch). Session installs a
	// closure pointing at its handleNotification method.
	notifyMu sync.RWMutex
	notify   NotificationHandler

	// ondrop is invoked once on listen / transmitWorker error. nil means
	// raw Client (no auto-recovery — caller observes via ErrTransportClosed
	// from subsequent RPCs). Session installs s.triggerReconnect.
	ondropMu sync.RWMutex
	ondrop   func()

	// handshaking counts route probe / registration regions in flight; see
	// beginHandshake for why transport faults are demoted to Debug then. A
	// counter rather than a flag so overlapping regions cannot have the inner
	// one re-enable ERROR logging while the outer is still running — the same
	// reason subscribeInFlight is a counter.
	handshaking atomic.Int64

	// AMS frames decoded, split by which socket they arrived on. Together they
	// answer what decides how a drop is reported: did this connection ever work?
	// Two counters because either alone is wrong -- a peer-answering device decodes
	// zero frames on its primary for its whole healthy life, while one shared
	// counter lets a peer frame vote the primary "established". The verdict is any
	// frame on any socket; both counts are logged so the silent one is visible.
	framesPrimary atomic.Uint64
	framesPeer    atomic.Uint64

	// When this connection was established, for the uptime on a drop. Set in the
	// literal before publishing and never written again -- readFrames reads it from
	// the listen goroutine. Zero is legitimate, so every log site goes through
	// uptimeAttr, which prints nothing rather than a ~2000-year duration.
	dialedAt time.Time

	// dropped closes when THIS client's connection is gone: disconnected stops new
	// requests, this releases the ones already blocked on a reply that will never
	// come. Per-Client, not per-transport -- a Session reuses one transport across
	// reconnects, and on the transport a stale client's listen goroutine signalled
	// it after the replacement had re-armed it, killing every new request.
	dropped  chan struct{}
	dropOnce sync.Once

	// Internal cancellation for the worker goroutines. Independent of any
	// caller context — Close cancels this to stop workers.
	ctx       context.Context
	cancel    context.CancelFunc
	waitGroup sync.WaitGroup

	closeOnce sync.Once

	// peerConns are inbound connections the PLC opened to us; see AcceptPeerConn.
	// peerClosed latches once closePeerConns has run: the accept loop outlives a
	// teardown by design, so it can still offer connections to a Client that is
	// going away, and adopting one then breaks the wait for this Client's workers.
	peerMu     sync.Mutex
	peerConns  []net.Conn
	peerClosed bool
}

// setSource replaces the source AMS address stamped on every request. Local mode
// learns its real address from the router only after the Client is published,
// since the handshake needs a live Client -- without this every later request
// carried the auto-derived placeholder. It is also what makes encode's connMu
// snapshot of c.source mean anything.
func (c *Client) setSource(addr ams.Address) {
	c.tx.connMu.Lock()
	c.source = addr
	c.tx.connMu.Unlock()
}

// sourceAddr returns the source AMS address under the mutex that guards it.
// readFrames logs it from the listen goroutine while a local-mode handshake may
// be writing it via setSource — both callers of setSource publish the Client,
// and so start the workers, before the handshake that assigns the address.
// encodeTo (ams.go) already takes connMu for the same reason.
func (c *Client) sourceAddr() ams.Address {
	c.tx.connMu.Lock()
	defer c.tx.connMu.Unlock()
	return c.source
}

// markDropped releases every request waiting on this client's connection.
// Idempotent: a drop can be observed by listen and the transmit worker both.
func (c *Client) markDropped() {
	c.dropOnce.Do(func() {
		if c.dropped != nil {
			close(c.dropped)
		}
	})
}

// Dial opens one TCP connection to ip:port, configures TCP keepalive, and
// spawns the listen / transmit / recvWorker goroutines. Returns a usable
// Client. See docs/archive/specs/09-fsm-design.md "Layer 2: Client (raw RPC)".
func Dial(
	ip string,
	port int,
	target, source ams.Address,
	requestTimeout time.Duration,
	opts ...ClientOption,
) (*Client, error) {
	if requestTimeout <= 0 {
		requestTimeout = 5 * time.Second
	}
	c := &Client{
		ip:             ip,
		port:           port,
		target:         target,
		source:         source,
		requestTimeout: requestTimeout,
		logger:         slog.Default(),
		dropped:        make(chan struct{}),
		tx: &transport{
			sendChannel:    make(chan []byte),
			systemResponse: make(chan []byte, 1),
			recvQueue:      make(chan []byte, recvQueueSize),
			activeRequests: map[uint32]chan amsReply{},
		},
	}
	for _, opt := range opts {
		opt(c)
	}
	c.ctx, c.cancel = context.WithCancel(context.Background()) //nolint:gosec // cancel stored on c.cancel and called from Close.

	tcpConn, err := net.DialTimeout(
		"tcp",
		net.JoinHostPort(ip, strconv.Itoa(port)),
		requestTimeout,
	)
	if err != nil {
		c.cancel()
		return nil, fmt.Errorf("ads: dial %s:%d: %w", ip, port, err)
	}
	c.tx.connection = tcpConn
	configureKeepAlive(tcpConn)
	// Before startWorkers, so nothing can be reading it concurrently. The literal
	// above is built before the dial, so this is the first point at which there is
	// a connection time to record.
	c.dialedAt = time.Now()
	c.startWorkers()
	return c, nil
}

// Close cancels worker goroutines, closes the TCP connection, and waits for
// workers to exit. Idempotent: subsequent calls are no-ops returning nil.
// Sets tx.disconnected so any subsequent RPC method returns
// ErrTransportClosed immediately.
func (c *Client) Close() error {
	c.closeOnce.Do(func() {
		c.tx.disconnected.Store(true)
		c.closePeerConns()
		c.markDropped()
		c.cancel()
		c.tx.connMu.Lock()
		if c.tx.connection != nil {
			_ = c.tx.connection.Close()
		}
		c.tx.connMu.Unlock()
		c.waitGroup.Wait()
	})
	return nil
}

// ClientOption configures optional construction parameters for Dial.
type ClientOption func(*Client)

// WithClientLogger sets the slog.Logger for a Client. Nil is ignored.
func WithClientLogger(logger *slog.Logger) ClientOption {
	return func(c *Client) {
		if logger != nil {
			c.logger = logger
		}
	}
}

// WithClientRequestTimeout overrides the per-request and dial timeout.
// Values <= 0 are ignored (the default of 5s applies).
func WithClientRequestTimeout(d time.Duration) ClientOption {
	return func(c *Client) {
		if d > 0 {
			c.requestTimeout = d
		}
	}
}

// WithNotificationHandler installs a callback for inbound DeviceNotification
// packets. Session installs its own handler internally; raw Client consumers
// (CLI, web ADS browser) install their own to receive notifications, or
// leave nil to drop them silently.
func WithNotificationHandler(fn NotificationHandler) ClientOption {
	return func(c *Client) {
		c.notify = fn
	}
}

// SetNotificationHandler installs (or replaces) the callback for inbound
// DeviceNotification packets. nil disables dispatch (packets dropped after
// a Debug log entry). Concurrent-safe; the recvWorker reads under RLock.
func (c *Client) SetNotificationHandler(fn NotificationHandler) {
	c.notifyMu.Lock()
	c.notify = fn
	c.notifyMu.Unlock()
}

// WithOnDrop registers a callback fired on unexpected transport drop.
// See SetOnDrop for the runtime equivalent.
func WithOnDrop(fn func()) ClientOption {
	return func(c *Client) {
		c.SetOnDrop(fn)
	}
}

// SetOnDrop registers a callback fired on unexpected transport drop.
// Prefer WithOnDrop at construction time.
func (c *Client) SetOnDrop(fn func()) {
	c.ondropMu.Lock()
	c.ondrop = fn
	c.ondropMu.Unlock()
}

func (c *Client) callOnDrop() {
	// Release every request on THIS client so they fail fast instead of waiting out
	// a timeout on a dead socket: a 40-symbol batch that lost the link at symbol 3
	// took 3m10s to return, holding the subscribe window open throughout.
	//
	// Deliberately does NOT touch tx.disconnected, which lives on the transport a
	// Session reuses and which Session owns. Setting it here let a stale client's
	// listen goroutine flip it back after the replacement had cleared it.
	c.markDropped()
	c.ondropMu.RLock()
	fn := c.ondrop
	c.ondropMu.RUnlock()
	if fn != nil {
		fn()
	}
}

// startWorkers spawns the listen, transmit, and recvWorker goroutines.
// Called by Dial after the TCP socket is established, and by Session at
// every successful dial / redial. Each goroutine ends on c.ctx.Done() or
// transport-level error.
func (c *Client) startWorkers() {
	c.waitGroup.Add(2 + recvWorkerCount)
	go c.listen()
	go c.transmitWorker()
	for i := 0; i < recvWorkerCount; i++ {
		go c.recvWorker()
	}
}
