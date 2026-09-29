package ads

import (
	"errors"
	"net"
	"net/netip"

	"github.com/siyka-au/go-ads/v3/adsclient"
	"github.com/siyka-au/go-ads/v3/ams"
	"github.com/siyka-au/go-ads/v3/internal/adsconn"
	"github.com/siyka-au/go-ads/v3/internal/symtab"
)

// ErrClosed is what Err reports after Close.
var ErrClosed = errors.New("ads: session closed")

// State reports where the session is in its lifecycle.
func (sess *Session) State() SessionState { return sess.lifecycle.state.load() }

// Done is closed when the session has ended for good: Close was called, or
// automatic reconnection gave up. Err then says which.
func (sess *Session) Done() <-chan struct{} { return sess.lifecycle.closedCh }

// Err is nil while the session is alive. Once Done is closed it returns
// ErrClosed after Close, or the reason reconnection gave up.
func (sess *Session) Err() error {
	select {
	case <-sess.lifecycle.closedCh:
	default:
		return nil
	}
	if p := sess.lifecycle.closeErr.Load(); p != nil {
		return *p
	}
	return ErrClosed
}

// setCloseErr records why the session ended. The first cause wins: Close after
// a give-up must not overwrite the reason the caller is looking for.
func (sess *Session) setCloseErr(err error) {
	sess.lifecycle.closeErr.CompareAndSwap(nil, &err)
}

// SessionInfo is a snapshot of how a session is connected.
type SessionInfo struct {
	State SessionState
	// Host is the PLC's address as configured (127.0.0.1 in local mode).
	Host string
	// Target is the PLC runtime's AMS address, after discovery filled in
	// anything the Endpoint left out.
	Target ams.Address
	// Local is the AMS address this session sends from.
	Local ams.Address
	// LocalTCP is the local end of the TCP connection; zero when not connected.
	LocalTCP netip.AddrPort
	// PeerListening reports whether the session listens for a connection the
	// PLC opens back to it (see WithAmsPeerListen).
	PeerListening bool
	// Runtime is the PLC runtime state from the last poll of the system
	// service, valid when RuntimeKnown.
	Runtime      ams.State
	RuntimeKnown bool
}

// Info returns a snapshot of the session's connection. It does no I/O.
func (sess *Session) Info() SessionInfo {
	info := SessionInfo{
		State:  sess.State(),
		Host:   sess.ip,
		Target: sess.target,
		Local:  sess.sourceAddr(),
	}
	if conn := sess.tx.Conn(); conn != nil && !sess.tx.Disconnected() {
		if a, ok := conn.LocalAddr().(*net.TCPAddr); ok {
			info.LocalTCP = a.AddrPort()
		}
	}
	sess.peerMu.Lock()
	info.PeerListening = sess.peerLn != nil
	sess.peerMu.Unlock()
	info.Runtime, info.RuntimeKnown = sess.knownRuntimeState()
	return info
}

// SubscriptionStatus is one subscription the session is keeping alive.
type SubscriptionStatus struct {
	Config NotificationConfig
	// Handle is the PLC's handle for the subscription, 0 while it is not
	// registered (between a drop and the resubscribe that restores it).
	Handle uint32
	// Active reports whether the PLC currently has it registered.
	Active bool
}

// Subscriptions lists the subscriptions the session is keeping alive, in the
// order they were made, with their current registration on the PLC.
func (sess *Session) Subscriptions() []SubscriptionStatus {
	m := sess.notifications
	hb := m.heartbeatHandle.Load()
	m.lock.Lock()
	defer m.lock.Unlock()
	handles := make(map[string]uint32, len(m.activeNotifications))
	for h, n := range m.activeNotifications {
		if h == hb || n.Sym == nil {
			continue
		}
		key := n.Name
		if key == "" {
			key = n.Sym.FullName
		}
		handles[symtab.Key(key)] = h
	}
	out := make([]SubscriptionStatus, 0, len(m.pending))
	for _, p := range m.pending {
		h, ok := handles[symtab.Key(p.Config.Symbol)]
		out = append(out, SubscriptionStatus{Config: p.Config, Handle: h, Active: ok})
	}
	return out
}

// Client returns a raw client on this session's connection, for index-group
// access, device info or the process image without opening a second TCP
// connection — which the PLC's router would treat as this host replacing the
// session's. The Client follows the session across reconnects; while it is
// reconnecting, calls fail with ErrTransportClosed. Closing it does nothing.
func (sess *Session) Client() *adsclient.Client {
	return adsconn.BindClient(func() *adsconn.Conn {
		if sess.tx.Disconnected() {
			return nil
		}
		return sess.client.Load()
	}).(*adsclient.Client) // importing adsclient runs its init, which sets BindClient
}
