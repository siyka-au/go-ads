package ads

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"syscall"
	"time"

	"github.com/siyka-au/go-ads/v3/internal/adsconn"

	"github.com/siyka-au/go-ads/v3/ams"
)

// dialTCP opens the outbound connection, honouring localBindIP. Shared by Connect
// and Reconnect so both bind the same way. Empty lets the OS pick; setting it pins
// each Session to a distinct local IP so the PLC sees separate hosts, one TCP slot
// each (Beckhoff #49/#72).
func (sess *Session) dialTCP() (net.Conn, error) {
	dialer := net.Dialer{Timeout: sess.requestTimeout}
	if sess.localBindIP != nil {
		dialer.LocalAddr = &net.TCPAddr{IP: sess.localBindIP}
	}
	return dialer.Dial("tcp", net.JoinHostPort(sess.ip, strconv.Itoa(sess.port)))
}

// Connect dials the PLC and transitions to Connected. May bind a listening socket
// on the AMS port unasked, since some devices answer only on a connection they open
// to us. A failed Connect rolls back and stays usable for a retry. Not safe for
// concurrent use on one Session.
func (sess *Session) Connect(ctx context.Context) (retErr error) {
	local := sess.isLocal
	// transitionToOnce returns ok=false if another goroutine already won
	// the Constructed→Connecting transition; reject concurrent calls rather
	// than letting both race on socket + client publish.
	if _, ok := sess.lifecycle.state.transitionToOnce(SessionStateConnecting); !ok {
		return fmt.Errorf("ads: Connect already in progress or session past Constructed state")
	}
	// Suppress the auto-reconnect spawn for the whole of Connect: see
	// lifecycle.connecting. Registered BEFORE the rollback and listener defers so
	// it runs AFTER them (LIFO) — the flag has to still be set while Connect does
	// its own teardown, or the rival is merely delayed into the same window.
	sess.lifecycle.connecting.Store(true)
	defer func() {
		sess.lifecycle.connecting.Store(false)
		// A drop suppressed by the gate that Connect never noticed is Connect's to
		// adopt, or the suppression trades a rival Reconnect for a lost drop. Success
		// path only: after an error the caller discards the session and adopting
		// would race their retry.
		if retErr == nil && !sess.isClosed() && sess.tx.Disconnected() {
			sess.triggerReconnect()
		}
	}()
	// Roll back Connecting→Disconnected on any error return so the caller
	// can retry Connect via the Disconnected→Connecting edge. Without this,
	// the FSM is stranded in Connecting and Reconnect (auto-path only) is
	// the sole recovery, forcing callers to construct a new Session.
	defer func() {
		if retErr != nil && sess.lifecycle.state.load() == SessionStateConnecting {
			sess.lifecycle.state.transitionTo(SessionStateDisconnected)
		}
	}()
	// Release the inbound listener on every error return, and only those: Connect
	// binds it in three places and callers throw a failed session away without
	// calling Close, so the leak was unbounded. releasePeerListener, not
	// stopPeerListener -- the latter latches and would refuse a legal retry's bind.
	defer func() {
		if retErr != nil {
			sess.releasePeerListener()
		}
	}()
	// Before dialing: some devices answer only on a connection they open to us,
	// so the listener has to be up before the first request goes out or its
	// response has nowhere to land. See WithAmsPeerListen.
	if sess.peerListenPort != 0 {
		if lerr := sess.startPeerListener(); lerr != nil {
			return lerr
		}
	}

	var err error
	sess.logger.Debug("dialing", "ip", sess.ip, "port", sess.port)
	if local {
		// Keep a caller-supplied NetID: a usermode runtime on this host has its own
		// NetID behind the local router, distinct from the system's 127.0.0.1.1.1.
		if sess.target.NetID == [6]byte{} {
			sess.target.NetID = [6]byte{127, 0, 0, 1, 1, 1}
		}
		sess.ip = "127.0.0.1"
	}
	// Check the target NetID against the device before spending a dial on it.
	// Skipped in local mode, where the target was just overwritten with the
	// loopback NetID and there is nothing to compare against.
	if !local && sess.targetCheck != TargetCheckOff {
		if err := sess.verifyTarget(ctx); err != nil {
			return err
		}
	}
	tcpConn, err := sess.dialTCP()
	if err != nil {
		// Warn, not Error: the error is also returned, the session stays retryable
		// (the deferred rollback restores Disconnected), and a reconnecting caller
		// dials again — so this is a transient condition being retried, not
		// something only a human can clear.
		sess.logger.Warn("could not dial the PLC", "ip", sess.ip, "port", sess.port, "error", err)
		return err
	}
	sess.tx.SetConn(tcpConn)
	// Enable aggressive TCP keepalive to detect dead connections quickly.
	// With Idle=3s, Interval=2s, Count=5: connection declared dead after ~13s of no response.
	// This ensures cable unplugs (>13s) are detected and trigger reconnect,
	// while not affecting slow-changing notification data (keepalive is TCP-level, not app-level).
	adsconn.ConfigureKeepAlive(tcpConn)
	// Log TCP socket (transport-level only — ADS route validation happens on first ADS command)
	sess.logger.Info("TCP socket established (ADS route not yet verified)",
		"local", tcpConn.LocalAddr().String(),
		"remote", tcpConn.RemoteAddr().String())

	// Auto-derive source AMS NetID from local IP if source NetID is all zeros.
	// No worker is running yet (the Conn is published below) and Connect is
	// exclusive, so nothing else reads or writes the source until SetSource.
	if source := sess.tx.Source(); source.NetID.IsZero() {
		localAddr, ok := tcpConn.LocalAddr().(*net.TCPAddr)
		if !ok {
			return fmt.Errorf("unexpected local address type: %T", tcpConn.LocalAddr())
		}
		ip := localAddr.IP.To4()
		if ip != nil {
			// If callbackIP is set (WithHostIP), use it for source NetID so that AMS
			// packet headers match the route the PLC registered. On multi-homed machines
			// the TCP source IP can differ from the IP used for route registration.
			if sess.callbackIP != "" {
				if cbIP := net.ParseIP(sess.callbackIP).To4(); cbIP != nil {
					ip = cbIP
				}
			}
			source.NetID = ams.NetID{ip[0], ip[1], ip[2], ip[3], 1, 1}
			sess.tx.SetSource(source)
			sess.logger.Info("auto-derived source AMS NetID from local IP",
				"netid", source.NetID.String())
		}

		// NAT/Docker detection: compare TCP and UDP source IPs
		if sess.callbackIP == "" && ip != nil {
			udpConn, udpErr := net.DialTimeout("udp4", net.JoinHostPort(sess.ip, strconv.Itoa(sess.effectiveRouterPort())), 2*time.Second)
			if udpErr == nil {
				udpAddr, ok := udpConn.LocalAddr().(*net.UDPAddr)
				udpConn.Close()
				if ok {
					udpIP := udpAddr.IP.To4()
					if udpIP != nil && !ip.Equal(udpIP) {
						sess.logger.Warn("TCP and UDP source IPs differ — possible NAT/Docker/VPN",
							"tcpIP", ip.String(), "udpIP", udpIP.String(),
							"hint", "set WithHostIP() to the IP the PLC can reach")
					}
				}
			}
		}
	}

	// One snapshot for the three log lines below, taken under the lock that guards
	// the field (see sourceAddr): a rival Reconnect's localHandshake can be writing
	// it, and these lines are exactly the diagnostics an operator uses to decide
	// whether their NetID configuration is wrong.
	sourceAddr := sess.sourceAddr()
	sourceIP := sourceAddr.NetID
	// Log container detection — auto-derived NetID works in containers because
	// the PLC stores the UDP source IP (post-NAT) for routes, not the computerName tag.
	if sess.callbackIP == "" && isRunningInContainer() {
		sess.logger.Info("container detected — auto-derived NetID will be used for route registration",
			"netidIP", fmt.Sprintf("%d.%d.%d.%d", sourceIP[0], sourceIP[1], sourceIP[2], sourceIP[3]))
	}

	// Log ADS-level addressing (what matters for AMS routing, may differ from TCP)
	routeHostIP := sess.callbackIP
	if routeHostIP == "" {
		routeHostIP = fmt.Sprintf("%d.%d.%d.%d (from NetID, PLC will use UDP source IP)", sourceIP[0], sourceIP[1], sourceIP[2], sourceIP[3])
	}
	sess.logger.Info("ADS addressing",
		"sourceNetID", sourceAddr.NetID.String(),
		"routeHostIP", routeHostIP,
		"target", sess.target.String())

	sess.logger.Log(context.Background(), LevelTrace, "connected")
	// Session and Client share the *transport pointer (no re-dial); the Client
	// owns the listen / transmit / recvWorker goroutines.
	newClient := sess.publishWiredClient()
	// Clear AFTER the workers are up, so disconnected=false implies transmitWorker
	// is running. Connect never cleared it and got away with it only because a rival
	// Reconnect did; with that spawn suppressed the stale true survived into the
	// retry and failed every request on a perfectly good socket.
	sess.tx.SetDisconnected(false)
	// If this device is already known to answer on a connection it opens to us, bind
	// before probing: otherwise every session pays the probe timeout plus the
	// activation budget to rediscover it and registers a route it did not need --
	// ~18s per Connect, against ~18ms once the answer can be heard.
	if !sess.peerFallbackDisabled && isKnownPeerRouteHost(sess.peerRouteCacheKey()) {
		if err := sess.startPeerListener(); err != nil {
			// Warn, not Debug: this names a port conflict an operator has to resolve
			// (usually a local TwinCAT router owning 48898), and swallowing it is
			// what made the same failure invisible before.
			sess.logger.Warn("could not bind the inbound AMS port for a device known to need it; requests may all time out",
				"host", sess.ip, "error", err,
				"hint", "pass WithAmsPeerListen(port) to use a different port, or WithoutAmsPeerFallback() to opt out")
		} else {
			sess.logger.Info("device is known to answer on its own connection; listening before probing", "host", sess.ip)
		}
	}
	if local {
		// The Conn has to be running before it can ask, so it starts out stamping
		// the placeholder; LocalHandshake replaces it for every later request.
		result, err := newClient.LocalHandshake()
		if err != nil {
			sess.tearDownAndReset()
			return fmt.Errorf("local mode %w", err)
		}
		sess.logger.Info("local mode handshake result", "result", result)
	}

	// A successful route-activation probe already carries the symbol version, so
	// track whether we have one and skip the duplicate read further down.
	var (
		symbolVersion uint8
		haveVersion   bool
	)
	// Probe first, register only if the route is missing. Registration is UDP but
	// the probe is an ADS command, so the workers must be running; after a
	// registration the TCP connection is rebuilt, since the PLC may close one from a
	// previously unknown NetID.
	if !sess.route.shouldSkip() {
		registered, err := sess.ensureRouteOnConnect(ctx)
		if err != nil {
			// WithRoute is explicit: swallowing this leaves every later command failing
			// with TargetNotFound on a connection that "succeeded". Tear down first --
			// a Client is already published on an open socket.
			sess.tearDownAndReset()
			return fmt.Errorf("route registration failed during connect: %w", err)
		}
		if registered {
			// TCP reconnect — PLC may reset connections from previously-unknown NetIDs.
			// Shut down goroutines, close TCP, redial, restart.
			sess.tearDownAndReset()
			sess.lifecycle.reconnectAttempts.Add(1)
			if err := sess.dialAndStart(); err != nil {
				return fmt.Errorf("TCP reconnect after route registration failed: %w", err)
			}
			sess.logger.Info("TCP reconnected after route registration")
			// Do not report success until the PLC actually serves the new route, or
			// the caller gets a Connect that worked and a session that times out.
			// Connect's own ctx is safe to pass per attempt: no teardown replaces it.
			probedVersion, err := sess.awaitRouteActive(func() context.Context { return ctx })
			if err != nil {
				// A device answering on a connection IT opens cannot pass this probe
				// however healthy the route is -- our socket stays silent because the
				// reply goes to the other one. Try the fallback before condemning the
				// route; the route branch used to return before it could run.
				if errors.Is(err, ErrRuntimeNotRunning) {
					// The route works; the runtime does not. Come up and wait — the
					// state poll and the gates on the symbol calls carry it from here.
					haveVersion = false
				} else if rescued, ferr := sess.tryPeerFallback(ctx); rescued {
					rememberPeerRouteHost(sess.peerRouteCacheKey())
					sess.logger.Warn("route probe was silent, but the PLC answers on a connection it opens to us; continuing",
						"host", sess.ip)
					haveVersion = false
				} else {
					if ferr != nil {
						err = fmt.Errorf("%w (the peer-route fallback could not run: %w)", err, ferr)
					}
					// The redial published a Client, so this path owns tearing it down.
					sess.tearDownAndReset()
					return fmt.Errorf("route registration during connect: %w", err)
				}
			} else {
				// The ordinary probe worked, so this device no longer needs the inbound
				// listener; forgetPeerRouteHostIfUnused drops the fact for every path.
				// The winning probe was a GetSymbolVersion, so seed the cache from it
				// rather than repeating the request.
				haveVersion, symbolVersion = true, probedVersion
			}
		}

	}

	// Read the symbol version for change detection. Failing is not fatal -- TC2 can
	// refuse this one service while alive -- but total SILENCE is. Lives here, not in
	// the route branch, so callers with a pre-registered route are checked too.
	if !haveVersion {
		symbolVersion, err = sess.client.Load().GetSymbolVersion(ctx)
		haveVersion = err == nil
		switch {
		case err == nil:
		case errors.Is(err, ErrTransportClosed):
			// The link died mid-connect; reporting success hands back a dead session.
			// The addressing hint goes in the RETURNED error, not just the log, and
			// which hint depends on whether the connection ever carried a frame. A
			// sentinel carries it so callers need not match strings.
			hint := adsconn.ResetAfterConnectHint(sess.sourceAddr(), sess.target)
			verdict := ErrRouteNotServed
			if c := sess.client.Load(); c != nil && c.Established() {
				hint = adsconn.EstablishedDropHint()
				verdict = ErrEstablishedDropped
			}
			sess.tearDownAndReset()
			sess.transitionState(SessionStateDisconnected)
			return fmt.Errorf("transport dropped during connect to %s: %w: %w (%s)",
				sess.ip, verdict, err, hint)
		case isUnservedError(err):
			// Second opinion before condemning the link: ReadState is the most
			// universally supported service there is, so if THAT is also met with
			// silence the device is not talking to us at all.
			if _, serr := sess.client.Load().ReadState(ctx); isUnservedError(serr) {
				// Total silence. Before giving up: the device may be answering on a
				// connection it opens to us rather than on ours.
				rescued, ferr := sess.tryPeerFallback(ctx)
				if rescued {
					rememberPeerRouteHost(sess.peerRouteCacheKey())
					haveVersion = false // the probe above never got a value
					break
				}
				sess.tearDownAndReset()
				sess.transitionState(SessionStateDisconnected)
				hint := "a stale or duplicate route entry for source NetID " + sess.sourceAddr().NetID.String() +
					", or another client using this IP, can hold a TwinCAT router in this state"
				if ferr != nil {
					hint += "; the peer-route fallback could not run: " + ferr.Error()
				}
				return fmt.Errorf("PLC at %s accepted the connection but answered neither GetSymbolVersion nor ReadState: %w (%s)",
					sess.ip, err, hint)
			}
			sess.logger.Debug("could not read symbol version during connect, but the PLC is answering", "error", err)
		default:
			sess.logger.Debug("could not read symbol version during connect", "error", err)
		}
	}
	if haveVersion {
		sess.cache.lock.Lock()
		sess.cache.symbolVersion = symbolVersion
		sess.cache.lock.Unlock()
	}
	// Poll the system service for the runtime state from here on. Connect itself
	// deliberately still succeeds against a runtime in CONFIG — the session is
	// usable, it just has no runtime to talk to yet — and the poll is what lets the
	// symbol and subscription calls say so instead of failing obscurely.
	sess.startRuntimeStateWatch()
	// One synchronous read before returning, so the very first LoadSymbols or
	// subscribe already has evidence. Waiting for the poller's first tick would
	// leave that call to fail the old obscure way — measured on a PLC in CONFIG:
	// "ADS error in Read: 0xF008", which is an index group, not a return code.
	if state, serr := sess.runtimeStateQuietly(ctx); serr != nil {
		sess.logger.Debug("could not read the runtime state from the system service at connect", "error", serr)
	} else if state != ams.StateRun {
		sess.logger.Warn("connected, but the PLC runtime is not in RUN: symbol and subscription calls will refuse until it returns",
			"state", uint16(state), "detail", "in CONFIG the runtime port does not exist, so those calls cannot succeed")
	}
	// This Connect worked, and if it never heard from the device on a connection the
	// device opened, the listener is not needed whatever an earlier session decided.
	// Here rather than in the branches: only a Connect about to report success has
	// earned the right to overwrite what a previous one learned.
	sess.forgetPeerRouteHostIfUnused()
	sess.enterConnected()
	sess.lifecycle.flapMu.Lock()
	sess.lifecycle.lastConnectedAt = time.Now()
	sess.lifecycle.flapMu.Unlock()
	return nil
}

// isTransportDead reports whether the connection is gone, as opposed to a request
// that failed on one that still works. Deliberately not isProbeRetryable, which
// answers "is a redial worth trying" and excludes DeadlineExceeded; this answers
// "is there still a socket to probe on".
func isTransportDead(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, ErrTransportClosed) ||
		errors.Is(err, io.EOF) ||
		errors.Is(err, syscall.ECONNRESET)
}

// tearDownAndReset cancels the goroutines, closes the connection and resets
// ctx/channels/activeRequests so it can be re-dialed. Used by Connect's
// post-route teardown, Reconnect's pre-retry reset, and resetForRetry.
func (sess *Session) tearDownAndReset() {
	// Capture cancel under RLock then release before invoking. Calling the
	// cancel under RLock would deadlock against the subsequent ctxMu.Lock
	// at the ctx replacement below if shutdown ever became a function that
	// took the same lock.
	sess.lifecycle.ctxMu.RLock()
	cancel := sess.lifecycle.shutdown
	sess.lifecycle.ctxMu.RUnlock()
	cancel()
	if localPort := sess.tx.CloseConn(); localPort != 0 {
		// INFO, not Debug. With debug_level on, the consumer's log rotated every
		// ~9s in the field and destroyed the evidence window repeatedly; one line
		// per teardown at INFO survives that.
		sess.logger.Info("closed the TCP connection for a session reset", "localPort", localPort)
	}
	// Wait out the previous Client's workers; the cancel above plus the closed
	// socket make them exit. Adopted inbound connections close first: their readers
	// block on a socket nothing else touches, and leaving them open deadlocks this
	// wait.
	if c := sess.client.Load(); c != nil {
		// Release anything waiting on this transport with the reason, rather than
		// letting it sit out its full request timeout: readFrames returns on
		// ctx.Done() without calling callOnDrop, so nothing else closes `dropped`
		// on a session-initiated teardown.
		c.Release()
		c.Wait()
	}
	// spawnMu across the Wait: trackGoroutineOn takes it to Add, so holding it here
	// makes "no new goroutines while we wait for the current ones" true. Without it
	// an orphan delete registering during a Connect-phase teardown can Add while
	// this Wait is in progress, which is documented WaitGroup misuse and panics.
	sess.lifecycle.spawnMu.Lock()
	sess.lifecycle.waitGroup.Wait()
	sess.lifecycle.spawnMu.Unlock()
	sess.lifecycle.ctxMu.Lock()
	// Re-derive lifecycle.ctx from the original NewSession parent so caller
	// cancellation continues to shut the session down after Reconnect. Prior
	// behaviour used context.Background() here, which detached the session
	// from its constructor parent after the first tearDownAndReset.
	parent := sess.lifecycle.parentCtx
	if parent == nil {
		// Defensive: test fixtures that build a Session literal directly
		// without going through NewSession may leave parentCtx nil. Fall
		// back to Background so tearDownAndReset stays panic-free; the
		// production constructor always sets parentCtx.
		parent = context.Background()
	}
	sess.lifecycle.ctx, sess.lifecycle.shutdown = context.WithCancel(parent) //nolint:gosec // cancel stored in lifecycle.shutdown, called from Close
	sess.lifecycle.ctxMu.Unlock()
	sess.tx.ResetQueues()
	// Capability state lives on the Conn, so the fresh one dialAndStart wires on
	// each attempt probes sum-command support again.
}

// dialAndStart dials, configures keepalive, clears the disconnected flag and
// starts the workers, for both Connect's post-route redial and Reconnect's retry
// loop. Re-checks closed before waitGroup.Add to avoid the misuse race.
func (sess *Session) dialAndStart() error {
	newConn, err := sess.dialTCP()
	if err != nil {
		return err
	}
	sess.tx.SetConn(newConn)
	adsconn.ConfigureKeepAlive(newConn)
	if sess.isClosed() {
		// Session was Closed mid-dial. Don't Add to waitGroup.
		sess.tx.DiscardConn()
		return fmt.Errorf("connection closed during dial")
	}
	c := sess.publishWiredClient()
	// The local port at INFO, on every dial this path makes — the reconnect and
	// route-activation dials, which Connect's own "TCP socket established" line
	// does not cover. Correlating a drop against a packet capture needs the
	// ephemeral port of the connection that died, and by then it is gone.
	if port := c.LocalPort(); port != 0 {
		sess.logger.Info("dialed the PLC", "localPort", port, "ip", sess.ip, "port", sess.port)
	}
	// Clear disconnected AFTER the workers are up, so a user RPC that observes
	// disconnected=false is guaranteed to find transmitWorker actually running.
	sess.tx.SetDisconnected(false)
	return nil
}

// sourceAddr returns the source AMS address. AddRoute is callable from any
// goroutine while a local-mode handshake may be replacing it, so every reader
// goes through the transport's lock.
func (sess *Session) sourceAddr() ams.Address {
	return sess.tx.Source()
}

// publishWiredClient wires and starts the Client for the connection on sess.tx.
// ctx and cancel come from one RLock, or the Client gets a context from one
// generation and the cancel of the next. Publish before startWorkers: the workers
// read sess.client, and a drop in that window tears down the previous Client.
func (sess *Session) publishWiredClient() *adsconn.Conn {
	sess.lifecycle.ctxMu.RLock()
	clientCtx := sess.lifecycle.ctx
	clientCancel := sess.lifecycle.shutdown
	sess.lifecycle.ctxMu.RUnlock()

	c := adsconn.New(adsconn.Config{
		Host:           sess.ip,
		Port:           sess.port,
		Target:         sess.target,
		RequestTimeout: sess.requestTimeout,
		Logger:         sess.logger,
		Transport:      sess.tx,
		Ctx:            clientCtx,
		Cancel:         clientCancel,
	})
	// handleNotification gives the Client cache-aware dispatch for inbound
	// DeviceNotification packets; triggerReconnect routes transport-down into the
	// Session's reconnect FSM.
	c.SetNotificationHandler(sess.handleNotification)
	c.SetOnDrop(sess.triggerReconnect)
	// Publish before the workers exist, so a drop cannot reach a teardown that
	// would load a stale sess.client. See the ordering note above.
	sess.client.Store(c)
	c.Start()
	return c
}

// localHandshake performs the local-mode Address probe used after dial when
// isLocal is true, and adopts the address the router hands out.
func (sess *Session) localHandshake() error {
	_, err := sess.client.Load().LocalHandshake()
	return err
}
