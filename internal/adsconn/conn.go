// Package adsconn is the ADS connection: one TCP socket to an AMS router, the
// framing, the invoke-ID multiplexing, the listen/transmit/receive workers and
// every ADS command. adsclient wraps it for users; the root package's Session
// wires a fresh Conn to one long-lived Transport on every (re)dial, which is why
// the lifecycle methods (Start, Release, Wait, LocalHandshake) are exported here
// and nowhere public.
package adsconn

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/siyka-au/go-ads/v3/ams"
	"github.com/siyka-au/go-ads/v3/internal/logging"
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
type Conn struct {
	ip   string
	port int

	target ams.Address

	requestTimeout time.Duration
	logger         *slog.Logger

	tx *Transport

	capabilities Capabilities //nolint:unused // capability state lives here, accessed via Client methods.

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

// Config wires a Conn to a transport that already holds a dialled connection.
type Config struct {
	Host           string // for logs
	Port           int    // for logs
	Target         ams.Address
	RequestTimeout time.Duration
	Logger         *slog.Logger
	Transport      *Transport
	// Ctx and Cancel stop the workers. A Session passes its lifecycle context so
	// one cancel stops everything of that generation.
	Ctx    context.Context
	Cancel context.CancelFunc
	// DisableSum makes the sum commands fall back to one request per item.
	DisableSum bool
}

// New builds a Conn on cfg.Transport without starting its workers; call Start
// once it is published wherever the workers will look for it. A Conn that is
// never started is usable in tests to exercise encode/decode paths.
func New(cfg Config) *Conn {
	if cfg.RequestTimeout <= 0 {
		cfg.RequestTimeout = 5 * time.Second
	}
	if cfg.Ctx == nil {
		cfg.Ctx, cfg.Cancel = context.WithCancel(context.Background()) //nolint:gosec // cancel stored on c.cancel and called from Close.
	}
	if cfg.Transport == nil {
		cfg.Transport = NewTransport(ams.Address{})
	}
	c := &Conn{
		ip:             cfg.Host,
		port:           cfg.Port,
		target:         cfg.Target,
		requestTimeout: cfg.RequestTimeout,
		logger:         logging.Or(cfg.Logger),
		tx:             cfg.Transport,
		dropped:        make(chan struct{}),
		ctx:            cfg.Ctx,
		cancel:         cfg.Cancel,
		// dialedAt in the literal, never as a later assignment: readFrames reads it
		// from the listen goroutine for the uptime on a drop.
		dialedAt: time.Now(),
	}
	if cfg.DisableSum {
		c.capabilities.disableSum()
	}
	return c
}

// Transport returns the transport this Conn is wired to.
func (c *Conn) Transport() *Transport { return c.tx }

// Release fails every request still waiting on this Conn and closes the
// connections the device opened to us. Their readers block on sockets nothing
// else touches, so a Wait for the workers without this never returns.
func (c *Conn) Release() {
	c.markDropped()
	c.closePeerConns()
}

// Wait blocks until the Conn's workers have exited. Cancel their context and
// close the socket (and Release) first, or it waits forever.
func (c *Conn) Wait() { c.waitGroup.Wait() }

// Err reports whether the Conn's worker context has ended.
func (c *Conn) Err() error {
	if c.ctx == nil {
		return nil
	}
	return c.ctx.Err()
}

// LocalHandshake asks the local TwinCAT router which AMS address to use, and
// stamps it on every later request. Local mode learns its address only after
// the Conn is running, since the question itself needs one.
func (c *Conn) LocalHandshake() (ams.Address, error) {
	resp, err := c.send([]byte{0, 16, 2, 0, 0, 0, 0, 0})
	if err != nil {
		return ams.Address{}, fmt.Errorf("local handshake send: %w", err)
	}
	var result ams.Address
	if err := binary.Read(bytes.NewReader(resp), binary.LittleEndian, &result); err != nil {
		return ams.Address{}, fmt.Errorf("local handshake parse: %w", err)
	}
	c.tx.SetSource(result)
	return result, nil
}

// markDropped releases every request waiting on this client's connection.
// Idempotent: a drop can be observed by listen and the transmit worker both.
func (c *Conn) markDropped() {
	c.dropOnce.Do(func() {
		if c.dropped != nil {
			close(c.dropped)
		}
	})
}

// DialConfig describes a raw connection for DialContext.
type DialConfig struct {
	// Host is the device's address, optionally with a port that overrides Port.
	Host   string
	Port   int
	Target ams.Address
	// Source is stamped on every request. A zero NetID is derived from the
	// connection's local IP plus ".1.1"; a zero Port is picked at random.
	Source         ams.Address
	RequestTimeout time.Duration
	Logger         *slog.Logger
	DisableSum     bool
}

// DialContext opens one TCP connection and returns a Conn on it, not yet
// started: install the notification and drop handlers, then call Start.
func DialContext(ctx context.Context, cfg DialConfig) (*Conn, error) {
	if cfg.RequestTimeout <= 0 {
		cfg.RequestTimeout = 5 * time.Second
	}
	host, port := cfg.Host, cfg.Port
	if h, p, err := net.SplitHostPort(cfg.Host); err == nil {
		n, perr := strconv.Atoi(p)
		if perr != nil || n <= 0 || n > 65535 {
			return nil, fmt.Errorf("ads: host %q: bad port", cfg.Host)
		}
		host, port = h, n
	}
	dialer := net.Dialer{Timeout: cfg.RequestTimeout}
	tcpConn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return nil, fmt.Errorf("ads: dial %s:%d: %w", host, port, err)
	}
	ConfigureKeepAlive(tcpConn)
	source := cfg.Source
	if source.NetID.IsZero() {
		if addr, ok := tcpConn.LocalAddr().(*net.TCPAddr); ok {
			if ip := addr.IP.To4(); ip != nil {
				source.NetID = ams.NetID{ip[0], ip[1], ip[2], ip[3], 1, 1}
			}
		}
	}
	if source.Port == 0 {
		source.Port = RandomSourcePort()
	}
	tx := NewTransport(source)
	tx.SetConn(tcpConn)
	return New(Config{
		Host:           host,
		Port:           port,
		Target:         cfg.Target,
		RequestTimeout: cfg.RequestTimeout,
		Logger:         cfg.Logger,
		Transport:      tx,
		DisableSum:     cfg.DisableSum,
	}), nil
}

// RandomSourcePort returns a random AMS source port in the dynamic range. The
// PLC keys its notification table by {source NetID, port, handle}, so a fresh
// port per connection keeps one process's notifications from being mistaken
// for another's.
func RandomSourcePort() ams.Port {
	const minPort, span = 32768, 49151 - 32768 + 1
	return ams.Port(minPort + rand.IntN(span)) //nolint:gosec // non-cryptographic port selection
}

// Close cancels worker goroutines, closes the TCP connection, and waits for
// workers to exit. Idempotent: subsequent calls are no-ops returning nil.
// Sets tx.disconnected so any subsequent RPC method returns
// ErrTransportClosed immediately.
func (c *Conn) Close() error {
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

// SetNotificationHandler installs (or replaces) the callback for inbound
// DeviceNotification packets. nil disables dispatch (packets dropped after
// a Debug log entry). Concurrent-safe; the recvWorker reads under RLock.
func (c *Conn) SetNotificationHandler(fn NotificationHandler) {
	c.notifyMu.Lock()
	c.notify = fn
	c.notifyMu.Unlock()
}

// SetOnDrop registers a callback fired on unexpected transport drop.
// Prefer WithOnDrop at construction time.
func (c *Conn) SetOnDrop(fn func()) {
	c.ondropMu.Lock()
	c.ondrop = fn
	c.ondropMu.Unlock()
}

func (c *Conn) callOnDrop() {
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

// Start spawns the listen, transmit, and recvWorker goroutines. Called once per
// Conn, after DialContext or after a Session wires a fresh dial. Each goroutine ends on c.ctx.Done() or
// transport-level error.
func (c *Conn) Start() {
	c.waitGroup.Add(2 + recvWorkerCount)
	go c.listen()
	go c.transmitWorker()
	for i := 0; i < recvWorkerCount; i++ {
		go c.recvWorker()
	}
}
