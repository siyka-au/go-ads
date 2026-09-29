package ads

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/siyka-au/go-ads/v3/internal/adsconn"

	"github.com/siyka-au/go-ads/v3/internal/fakeplc"

	"github.com/siyka-au/go-ads/v3/internal/symtab"

	"github.com/siyka-au/go-ads/v3/ams"
)

// --- Notification packet builders ---

// buildNotificationPacket constructs a valid ADS DeviceNotification payload
// with one stamp containing one sample.
func buildNotificationPacket(handle uint32, timestamp uint64, data []byte) []byte {
	buf := new(bytes.Buffer)
	// NotificationStream: Length + Stamps
	streamLen := uint32(8 + 12 + 8 + len(data)) // stream header + stamp header + sample header + data
	binary.Write(buf, binary.LittleEndian, streamLen)
	binary.Write(buf, binary.LittleEndian, uint32(1)) // 1 stamp

	// StampHeader: Timestamp + Samples
	binary.Write(buf, binary.LittleEndian, timestamp)
	binary.Write(buf, binary.LittleEndian, uint32(1)) // 1 sample

	// NotificationSample: Handle + Size
	binary.Write(buf, binary.LittleEndian, handle)
	binary.Write(buf, binary.LittleEndian, uint32(len(data)))

	// Data
	buf.Write(data)
	return buf.Bytes()
}

func buildNotificationPacketMultiSample(stamps []struct {
	timestamp uint64
	samples   []struct {
		handle uint32
		data   []byte
	}
},
) []byte {
	// Calculate total length
	buf := new(bytes.Buffer)
	totalLen := uint32(8) // stream header
	for _, s := range stamps {
		totalLen += 12 // stamp header
		for _, samp := range s.samples {
			totalLen += 8 + uint32(len(samp.data)) // sample header + data
		}
	}
	binary.Write(buf, binary.LittleEndian, totalLen)
	binary.Write(buf, binary.LittleEndian, uint32(len(stamps)))

	for _, s := range stamps {
		binary.Write(buf, binary.LittleEndian, s.timestamp)
		binary.Write(buf, binary.LittleEndian, uint32(len(s.samples)))
		for _, samp := range s.samples {
			binary.Write(buf, binary.LittleEndian, samp.handle)
			binary.Write(buf, binary.LittleEndian, uint32(len(samp.data)))
			buf.Write(samp.data)
		}
	}
	return buf.Bytes()
}

// testEndpoint returns the conventional Endpoint used by unit tests:
// loopback IP, TwinCAT TCP default port, fixed AMS NetID 1.2.3.4.1.1,
// AMS port 851 (PortR0PlcTc3). Tests that need a different target should
// build their own Endpoint inline.
func testEndpoint() Endpoint {
	return Endpoint{
		Host:   "127.0.0.1",
		Port:   48898,
		Target: ams.Address{NetID: [6]byte{1, 2, 3, 4, 1, 1}, Port: 851},
	}
}

// newTestConnection creates a minimal Session for unit testing notification parsing.
// The Session has a synthetic *Client wired with its handleNotification installed
// so packet-level tests can drive `conn.client.Load().deviceNotification(ctx, packet)`
// and exercise the cache-aware handler.
func newTestConnection() *Session {
	ctx, cancel := context.WithCancel(context.Background())
	conn := &Session{
		lifecycle:     &sessionLifecycle{ctx: ctx, shutdown: cancel},
		notifications: &notificationManager{activeNotifications: make(map[uint32]activeNotification), configsByKey: make(map[string]struct{}), orphanSeen: make(map[uint32]time.Time), orphanSem: make(chan struct{}, orphanDeleteMaxConcurrency)},
		cache:         &symbolCache{symbols: map[string]*symtab.Symbol{}, onDemandSymbols: map[string]bool{}},
		logger:        slog.Default(),
	}
	conn.client.Store(adsconn.New(adsconn.Config{Logger: conn.logger, Ctx: ctx, Cancel: cancel}))
	conn.client.Load().SetNotificationHandler(conn.handleNotification)
	return conn
}

// drivePacket feeds a wire-format DeviceNotification packet into the
// Client.deviceNotification decoder, which then dispatches to
// Session.handleNotification via the installed callback.
func (conn *Session) drivePacket(ctx context.Context, packet []byte) error {
	return conn.client.Load().DecodeNotification(ctx, packet)
}

// --- helpers for the AMS peer-listener tests ---

// freeLocalPort reserves and releases a loopback port, returning its number. The
// small race (something else could take it) is acceptable in tests and keeps the
// stub and the listener agreeing on one number.
func freeLocalPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		_ = ln.Close()
		t.Fatalf("unexpected addr type %T", ln.Addr())
	}
	_ = ln.Close()
	return addr.Port
}

func localAddr(port int) string {
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
}

// fastBackoff keeps a reconnect loop quick without making the delays the thing
// under test. Slow enough that an ignored cancellation is visibly slower than an
// honoured one.
func fastBackoff() BackoffConfig {
	return BackoffConfig{
		InitialInterval: 50 * time.Millisecond,
		InitialAttempts: 100,
		MidInterval:     50 * time.Millisecond,
		MidAttempts:     100,
		SlowInterval:    50 * time.Millisecond,
		SlowAttempts:    100,
		MaxInterval:     50 * time.Millisecond,
	}
}

func newTestNotificationManager() *notificationManager {
	return &notificationManager{
		activeNotifications: make(map[uint32]activeNotification),
		configsByKey:        make(map[string]struct{}),
		orphanSeen:          make(map[uint32]time.Time),
		orphanSem:           make(chan struct{}, orphanDeleteMaxConcurrency),
	}
}

// newDialableTestSession builds a Session the reconnect loop can drive against a
// real address.
//
// Deliberately NOT newWiredTestSession: that helper's Client comes from Dial,
// which gives it a Background context of its own, so tearDownAndReset's wait for
// the worker goroutines never returns. Production Clients take the session's
// context (see publishWiredClient), which is what lets a teardown stop them.
func newDialableTestSession(t *testing.T, host string, port int, maxAttempts int) *Session {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	sess := &Session{
		ip:            host,
		port:          port,
		tx:            adsconn.NewTransport(ams.Address{}),
		notifications: newTestNotificationManager(),
		cache:         &symbolCache{symbols: map[string]*symtab.Symbol{}, onDemandSymbols: map[string]bool{}},
		// Production always has one; ensureRoute dereferences it unconditionally.
		route:          &routeManager{},
		logger:         slog.Default(),
		requestTimeout: 500 * time.Millisecond,
		lifecycle: &sessionLifecycle{
			closedCh:             make(chan struct{}),
			parentCtx:            context.Background(),
			maxReconnectAttempts: maxAttempts,
			backoffConfig:        fastBackoff(),
			ctx:                  ctx,
			shutdown:             cancel,
		},
	}
	sess.lifecycle.state.transitionTo(SessionStateConstructed)
	sess.lifecycle.state.transitionTo(SessionStateConnecting)
	sess.lifecycle.state.transitionTo(SessionStateConnected)
	return sess
}

// gateOnLog pins the goroutine that logs a chosen message until the test releases
// it, which is how these tests reach a window that is otherwise nanoseconds wide.
//
// Reconnect's tail — mark the transport live, transition to Connected, log
// "reconnect successful", return, run the defers — has no seam a test can drive:
// the whole stretch is CPU-only, so a drop injected from outside lands after the
// defers essentially every time. The log call inside that stretch is the one place
// the session hands control to something the test owns.
//
// Only the FIRST match gates. Later ones pass straight through, so the recovery
// this pins the way open for is free to log the same line.
// watch is the shared state, so handlers cloned by WithAttrs/WithGroup gate on the
// same channels as the original.
type gateWatch struct {
	match     string
	reached   chan struct{}
	release   chan struct{}
	gateOnce  sync.Once
	signal    string
	signalled chan struct{}
	sigOnce   sync.Once
}

type gateOnLog struct {
	inner slog.Handler
	w     *gateWatch
}

// newGateOnLog gates the goroutine that logs `match`, and separately signals —
// without blocking — when some other goroutine logs `signal`.
func newGateOnLog(match, signal string) *gateOnLog {
	return &gateOnLog{
		inner: slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelWarn}),
		w: &gateWatch{
			match:     match,
			reached:   make(chan struct{}),
			release:   make(chan struct{}),
			signal:    signal,
			signalled: make(chan struct{}),
		},
	}
}

func (g *gateOnLog) Enabled(context.Context, slog.Level) bool { return true }

func (g *gateOnLog) Handle(ctx context.Context, r slog.Record) error {
	if g.w.signal != "" && strings.Contains(r.Message, g.w.signal) {
		g.w.sigOnce.Do(func() { close(g.w.signalled) })
	}
	if strings.Contains(r.Message, g.w.match) {
		first := false
		g.w.gateOnce.Do(func() { first = true })
		if first {
			close(g.w.reached)
			<-g.w.release
		}
	}
	return g.inner.Handle(ctx, r)
}

func (g *gateOnLog) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &gateOnLog{inner: g.inner.WithAttrs(attrs), w: g.w}
}

func (g *gateOnLog) WithGroup(name string) slog.Handler {
	return &gateOnLog{inner: g.inner.WithGroup(name), w: g.w}
}

// newWiredTestSession builds a Session whose .client is the *Client wired
// to srv. The FSM is transitioned to Connected so isClosed/isReconnecting
// short-circuits behave like a live session.
//
// Optional Options are applied after construction so tests can
// override defaults (e.g. WithSymbolVersionStrategy, WithOnDisconnect).
// lifecycle.ctx + lifecycle.shutdown are pre-initialised so sess.Close()
// is safe to call from tests; autoReconnect defaults to false to avoid
// spawning the Reconnect goroutine on disconnect.
//
// Caller is responsible for c.Close() at end of test (typically via t.Cleanup).
func newWiredTestSession(t *testing.T, srv *fakeplc.PLC, opts ...Option) (*Session, *adsconn.Conn) {
	t.Helper()
	c, err := adsconn.DialContext(context.Background(), adsconn.DialConfig{Host: srv.Host, Port: srv.Port, RequestTimeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("Dial scriptable server: %v", err)
	}
	c.Start()
	t.Cleanup(func() { _ = c.Close() })
	// Sessions from this helper cannot be Close()d — the Client comes from Dial with
	// a context of its own, so Close's wait for its workers never returns (see
	// newDialableTestSession). Any background goroutine keyed on closedCh therefore
	// outlives the test unless it is signalled here. Left unsignalled, a heartbeat
	// watcher from one test kept running for the remainder of the process; in a real
	// integration run that was 1468 warning lines across 666 later tests.

	sess := &Session{
		tx:            c.Transport(),
		cache:         &symbolCache{symbols: map[string]*symtab.Symbol{}, onDemandSymbols: map[string]bool{}},
		notifications: &notificationManager{activeNotifications: make(map[uint32]activeNotification), configsByKey: make(map[string]struct{}), orphanSeen: make(map[uint32]time.Time), orphanSem: make(chan struct{}, orphanDeleteMaxConcurrency)},
		lifecycle:     &sessionLifecycle{closedCh: make(chan struct{})},
		logger:        slog.Default(),
	}
	sess.client.Store(c)
	// Pre-init ctx/shutdown so sess.Close() is safe in test paths that
	// exercise the close lifecycle (e.g. SymbolVersionClose strategy).
	// parentCtx too, matching NewSession: tearDownAndReset re-derives
	// lifecycle.ctx from it, so a helper that leaves it nil diverges from
	// production on every path that redials.
	sess.lifecycle.parentCtx = context.Background()
	sess.lifecycle.ctx, sess.lifecycle.shutdown = context.WithCancel(sess.lifecycle.parentCtx)
	// Walk the FSM through Connecting → Connected so isClosed/isReconnecting
	// readers see a live session. Direct atomic store keeps the test helper
	// simple — production transitions go through transitionTo.
	sess.lifecycle.state.value.Store(uint32(SessionStateConnected))
	for _, opt := range opts {
		opt(sess)
	}
	t.Cleanup(func() {
		sess.markClosed() // see the note above the Client cleanup
		sess.heartbeatWG.Wait()
	})
	return sess, c
}

// newNotifTestSession is a synthetic Session with cache + notifications +
// lifecycle. No client; tests that need network use the echo helper.
func newNotifTestSession() *Session {
	return &Session{
		cache:         &symbolCache{symbols: map[string]*symtab.Symbol{}, onDemandSymbols: map[string]bool{}},
		notifications: &notificationManager{activeNotifications: make(map[uint32]activeNotification), configsByKey: make(map[string]struct{}), orphanSeen: make(map[uint32]time.Time), orphanSem: make(chan struct{}, orphanDeleteMaxConcurrency)},
		lifecycle:     &sessionLifecycle{closedCh: make(chan struct{})},
		logger:        slog.Default(),
	}
}

// preSeedSymbol primes the cache with a symbol that has a non-zero handle
// so getSymbol returns immediately without taking the GetHandleByName
// network path.
func preSeedSymbol(sess *Session, name string) *symtab.Symbol {
	sym := &symtab.Symbol{
		FullName: name,
		Name:     name,
		DataType: "INT",
		Length:   2,
		Handle:   0xC0DE,
	}
	sess.cache.lock.Lock()
	sess.cache.symbols[symtab.Key(name)] = sym
	sess.cache.lock.Unlock()
	return sym
}

// seedLiveNotification stages a symbol as fully subscribed: a committed handle in
// activeNotifications plus its entry on file, which is what makes a further
// subscribe of that symbol a duplicate.
//
// Staging the config alone is NOT enough and never was a duplicate in any sense the
// API claims — an entry with no handle is a resubscribe retry the caller is entitled
// to re-declare (see notificationManager.hasLiveNotification).
func seedLiveNotification(sess *Session, name string, handle uint32, ch chan *Update) {
	sym := preSeedSymbol(sess, name)
	sess.notifications.lock.Lock()
	defer sess.notifications.lock.Unlock()
	sess.notifications.activeNotifications[handle] = activeNotification{Sym: sym, Ch: ch}
	sess.notifications.addConfig(NotificationConfig{Symbol: name})
}

// preSeedTypedSymbol primes the cache with a symbol whose handle is non-zero
// so getSymbol resolves without a GetHandleByName roundtrip. INT/2 parses
// against a nil datatypes map.
func preSeedTypedSymbol(sess *Session, name string, handle uint32) *symtab.Symbol {
	sym := &symtab.Symbol{
		FullName: name,
		Name:     name,
		DataType: "INT",
		Length:   2,
		Handle:   handle,
	}
	sess.cache.lock.Lock()
	sess.cache.symbols[symtab.Key(name)] = sym
	sess.cache.lock.Unlock()
	return sym
}

// servingPLC answers sum and single notification registration with fresh
// handles, and deletes.
func servingPLC(t *testing.T) *fakeplc.PLC {
	t.Helper()
	srv := fakeplc.StartPLC(t)
	t.Cleanup(srv.Stop)
	next := uint32(0x2000)
	srv.OnWriteRead(ams.GroupSumupAddDeviceNotification, func(req []byte) []byte {
		items := make([]fakeplc.SumNotifResponse, len(req)/40)
		for i := range items {
			next++
			items[i] = fakeplc.SumNotifResponse{Error: ams.ReturnCodeNoErrors, Handle: next}
		}
		return fakeplc.SumAddNotifPayload(items)
	})
	srv.OnAddDeviceNotification(func(fakeplc.AddNotifRequest) fakeplc.AddNotifResponse {
		next++
		return fakeplc.AddNotifResponse{Handle: next}
	})
	srv.OnWriteRead(ams.GroupSumupDeleteDeviceNotification, func(req []byte) []byte {
		return fakeplc.SumDeleteNotifPayload(make([]ams.ReturnCode, len(req)/4))
	})
	srv.OnDeleteDeviceNotification(func(uint32) ams.ReturnCode { return ams.ReturnCodeNoErrors })
	return srv
}
