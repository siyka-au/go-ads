package ads

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"sync"
	"time"
)

// peerFallbackProbes is how many requests the automatic fallback sends after
// starting the listener. Each carries the session's own request timeout, and the
// device has to dial in and answer one of them.
const peerFallbackProbes = 3

// peerRouteHosts remembers which devices answer only on a connection they open to
// us: learning it costs ~15s and is a property of the device, not the session.
// Keyed by host AND port, or test stubs sharing 127.0.0.1 inherit one verdict. A
// stale entry only pre-binds the listener; the normal probe still wins.
var peerRouteHosts sync.Map // "host:port" -> struct{}

func (sess *Session) peerRouteCacheKey() string {
	return net.JoinHostPort(sess.ip, strconv.Itoa(sess.port))
}

func rememberPeerRouteHost(key string) { peerRouteHosts.Store(key, struct{}{}) }

// forgetPeerRouteHost drops a remembered device. Call it through
// Session.forgetPeerRouteHostIfUnused rather than directly, so the entry is only
// dropped when the session proved the device does not need it.
func forgetPeerRouteHost(key string) { peerRouteHosts.Delete(key) }

// forgetPeerRouteHostIfUnused drops the remembered fact when a session reached
// Connected with no inbound connection. Safe unconditionally: a device that dialled
// us has peerConnsAdopted > 0 and keeps its entry. The first Connect that does not
// need it drops it, which beats any expiry.
func (sess *Session) forgetPeerRouteHostIfUnused() {
	if sess.peerConnsAdopted.Load() != 0 {
		return
	}
	forgetPeerRouteHost(sess.peerRouteCacheKey())
}

func isKnownPeerRouteHost(key string) bool {
	_, ok := peerRouteHosts.Load(key)
	return ok
}

// Returns rescued=true when a request succeeded after the listener came up.
func (sess *Session) tryPeerFallback(ctx context.Context) (rescued bool, why error) {
	if sess.peerFallbackDisabled {
		return false, fmt.Errorf("peer-route fallback disabled by WithoutAmsPeerFallback")
	}
	sess.peerMu.Lock()
	listening := sess.peerLn != nil
	sess.peerMu.Unlock()
	if !listening {
		if err := sess.startPeerListener(); err != nil {
			return false, err
		}
		sess.logger.Info("PLC answered nothing on our connection; listening for one it may open to us",
			"port", sess.peerListenPortOrDefault())
	}
	// Probe either way. Returning early when a listener existed was wrong twice: a
	// caller setting WithAmsPeerListen never got the probes, and pre-binding for a
	// device known to answer on its own connection made the fast path fail outright.

	for attempt := 1; attempt <= peerFallbackProbes; attempt++ {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if _, err := sess.client.Load().GetSymbolVersion(ctx); err == nil {
			sess.logger.Warn("PLC answers on a connection it opens to us, not on ours; using that connection",
				"attempts", attempt, "listenPort", sess.peerListenPortOrDefault(),
				"detail", "the device treats its route to this host as a peer route. "+
					"Its route table most likely holds more than one entry for this source NetID; "+
					"a client that cannot accept the inbound connection sees every request time out")
			return true, nil
		}
		select {
		case <-time.After(200 * time.Millisecond):
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}
	return false, nil
}

// peerListenPortOrDefault is the port the peer listener uses.
func (sess *Session) peerListenPortOrDefault() int {
	if sess.peerListenPort != 0 {
		return sess.peerListenPort
	}
	return amsPeerListenPort
}

// amsPeerListenPort is where a TwinCAT peer router expects to reach us: the same
// port a PLC serves ADS on. Only relevant with WithAmsPeerListen.
const amsPeerListenPort = 48898

// startPeerListener accepts connections the PLC opens to us, handing each to the
// current Client so it survives reconnects. A mutex, not sync.Once: Once runs its
// body once whether it succeeded or not, so after a failed bind every later call
// returned nil with nothing listening.
func (sess *Session) startPeerListener() error {
	sess.peerMu.Lock()
	defer sess.peerMu.Unlock()
	if sess.peerLn != nil {
		return nil // already listening
	}
	if sess.peerStopped {
		return fmt.Errorf("session is shutting down; not binding the inbound AMS port")
	}
	select {
	case <-sess.lifecycle.closedCh:
		return fmt.Errorf("session is closed; not binding the inbound AMS port")
	default:
	}
	port := sess.peerListenPort
	if port == 0 {
		port = amsPeerListenPort
	}
	ln, err := net.Listen("tcp4", net.JoinHostPort("", strconv.Itoa(port)))
	if err != nil {
		// Deliberately retryable: the port may be free by the next attempt, and a
		// caller who retries Connect is entitled to a real answer either way.
		return fmt.Errorf("listening for the PLC's own connection on port %d: %w "+
			"(a local TwinCAT router or another ADS client may already own it)", port, err)
	}
	sess.peerLn = ln
	sess.logger.Info("listening for inbound PLC connections (peer-route support)", "port", port)
	sess.peerWG.Add(1)
	go sess.peerAcceptLoop(ln)
	return nil
}

// peerAcceptLoop attaches every inbound connection to the current Client.
func (sess *Session) peerAcceptLoop(ln net.Listener) {
	defer sess.peerWG.Done()
	for {
		conn, err := ln.Accept()
		if err != nil {
			return // listener closed by Close
		}
		if sess.isClosed() {
			_ = conn.Close()
			return
		}
		c := sess.client.Load()
		if c == nil {
			// No Client yet (or between teardown and redial): the PLC will dial
			// again once we have one, so dropping this is safe.
			sess.logger.Debug("inbound PLC connection arrived with no active client; closing",
				"remote", conn.RemoteAddr().String())
			_ = conn.Close()
			continue
		}
		// Counted before the hand-off, and never decremented: the question this
		// answers is "did this device ever dial us", and a Client that refuses the
		// connection because it is being torn down does not make the answer no.
		sess.peerConnsAdopted.Add(1)
		c.AcceptPeerConn(conn)
	}
}

// stopPeerListener closes the listener, latches the session against ever binding
// again, and waits for the accept loop to exit. For terminal teardown only.
func (sess *Session) stopPeerListener() {
	sess.peerMu.Lock()
	// Latch so a Connect descheduled just before its bind cannot bind after Close
	// returns. Set BEFORE releasePeerListener takes the lock: startPeerListener
	// holds peerMu across net.Listen, so a visible latch means the bind either
	// landed or is refused.
	sess.peerStopped = true
	sess.peerMu.Unlock()
	sess.releasePeerListener()
}

// releasePeerListener closes the listener and waits for the accept loop, WITHOUT
// latching peerStopped. That is what a failed Connect needs: latching would refuse
// every retry's bind, while not releasing leaked port 48898 and its accept loop per
// failed attempt.
func (sess *Session) releasePeerListener() {
	sess.peerMu.Lock()
	ln := sess.peerLn
	sess.peerLn = nil
	sess.peerMu.Unlock()
	if ln != nil {
		_ = ln.Close()
	}
	sess.peerWG.Wait()
}
