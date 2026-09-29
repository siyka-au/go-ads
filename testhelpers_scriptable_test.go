package ads

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/siyka-au/go-ads/v3/internal/fakeplc"

	"github.com/siyka-au/go-ads/v3/internal/symtab"

	"github.com/siyka-au/go-ads/v3/ams"
)

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

	c, err := Dial(srv.Host, srv.Port, ams.Address{}, ams.Address{}, 2*time.Second)
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

// newWiredTestSession builds a Session whose .client is the *Client wired
// to srv. The FSM is transitioned to Connected so isClosed/isReconnecting
// short-circuits behave like a live session.
//
// Optional SessionOptions are applied after construction so tests can
// override defaults (e.g. WithSymbolVersionStrategy, WithOnDisconnect).
// lifecycle.ctx + lifecycle.shutdown are pre-initialised so sess.Close()
// is safe to call from tests; autoReconnect defaults to false to avoid
// spawning the Reconnect goroutine on disconnect.
//
// Caller is responsible for c.Close() at end of test (typically via t.Cleanup).
func newWiredTestSession(t *testing.T, srv *fakeplc.PLC, opts ...SessionOption) (*Session, *Client) {
	t.Helper()
	c, err := Dial(srv.Host, srv.Port, ams.Address{}, ams.Address{}, 5*time.Second)
	if err != nil {
		t.Fatalf("Dial scriptable server: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	// Sessions from this helper cannot be Close()d — the Client comes from Dial with
	// a context of its own, so Close's wait for its workers never returns (see
	// newDialableTestSession). Any background goroutine keyed on closedCh therefore
	// outlives the test unless it is signalled here. Left unsignalled, a heartbeat
	// watcher from one test kept running for the remainder of the process; in a real
	// integration run that was 1468 warning lines across 666 later tests.

	sess := &Session{
		tx:            c.tx,
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
