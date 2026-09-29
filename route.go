package ads

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/siyka-au/go-ads/v3/ams"
	"github.com/siyka-au/go-ads/v3/router"
)

// routeManager holds the credentials and policy state used for AMS route
// registration. The caller's WithRoute(name, user, password) option populates
// these fields; Connect/Reconnect read them when probing the PLC's route
// table and registering if needed.
//
// name/username/password/forceRouteRegistration are write-once at construction
// (via WithRoute). routeProbeFailures is read+written from both Connect (caller
// goroutine) and Reconnect (lifecycle goroutine) - atomic.Int32 makes that
// race-free without imposing a lock on the hot reconnect path.
type routeManager struct {
	name                   string
	username               string
	password               secret
	forceRouteRegistration bool
	// activationTimeout caps the post-registration wait for the PLC's router
	// to start serving the new route. 0 means use defaultRouteActivationTimeout;
	// set via WithRouteActivationTimeout.
	activationTimeout  time.Duration
	skipRegistration   bool // set via WithSkipRouteRegistration — caller manages routes externally
	routeProbeFailures atomic.Int32

	// registered stops a reconnect storm re-registering the route over and over.
	// Not absolute: re-registering the CORRECT route is the documented recovery for
	// a muted router, since TC3 keys its table by address and the right NetID
	// rebinds it. Cleared whenever the session concludes the PLC stopped answering,
	// permitting one healing registration per cooldown cycle.
	registered atomic.Bool
}

// mayRegister reports whether this session may register its route. True exactly
// once; see routeManager.registered for why that is the rule.
func (r *routeManager) mayRegister() bool {
	return !r.registered.Load()
}

// markRegistered records that this session has registered its route.
func (r *routeManager) markRegistered() {
	r.registered.Store(true)
}

// allowHealingRegistration permits one further registration, for use when the PLC
// has stopped answering and re-registering the correct route is the way back.
func (r *routeManager) allowHealingRegistration() {
	r.registered.Store(false)
}

// shouldSkip reports whether route registration must be bypassed entirely.
// True when no route name was configured (default) or when the caller
// explicitly opted out via WithSkipRouteRegistration.
func (r *routeManager) shouldSkip() bool {
	return r.skipRegistration || r.name == ""
}

// routeProbeRetryDelay is how long we wait between the first probe attempt
// and the redial-retry. Picked to give a TwinCAT PLC time to release a TCP
// slot held by a recently-closed connection from the same source IP
// (~500ms is empirically enough on TC3 4024.x; tunable if needed).
const routeProbeRetryDelay = 500 * time.Millisecond

const (
	// How long a probe failure is treated as a briefly deaf router rather than a
	// missing route, and how often identify is retried inside it. Measured ~8s
	// of silence after a client restart on a TC3 4024; 20s leaves margin.
	routerAwakePoll = 1 * time.Second
	// How long to keep re-probing a live router before concluding the route is
	// genuinely missing. Measured: a probe that failed at 1.6s succeeded 36s later
	// with nothing registered in between.
	routeProbeGrace = 30 * time.Second
)

// var, not const: tests shorten it rather than waiting out the real grace.
var routerDeafGrace = 20 * time.Second

// awaitRouterAwake waits until the PLC's AMS router answers identify, or reports
// ErrRouterUnresponsive after routerDeafGrace. Identify needs no route and no TCP
// slot, so silence there means the router is serving nobody -- the one case where
// a failed route probe says nothing about whether the route exists.
func (sess *Session) awaitRouterAwake(ctx context.Context) error {
	deadline := time.Now().Add(routerDeafGrace)
	for attempt := 1; ; attempt++ {
		probeCtx, cancel := context.WithTimeout(ctx, routerAwakePoll)
		_, err := router.Identify(probeCtx, sess.ip, sess.routerOptions()...)
		cancel()
		if err == nil {
			if attempt > 1 {
				sess.logger.Info("the PLC's AMS router is answering again",
					"waited", time.Since(deadline.Add(-routerDeafGrace)).Round(time.Second))
			}
			return nil
		}
		if ctx.Err() != nil {
			return fmt.Errorf("waiting for the PLC's AMS router: %w", ctx.Err())
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%w: no identify answer in %v", ErrRouterUnresponsive, routerDeafGrace)
		}
		if attempt == 1 {
			sess.logger.Info("the PLC's AMS router is not answering; waiting for it rather than assuming the route is missing",
				"grace", routerDeafGrace, "error", err)
		}
		select {
		case <-time.After(routerAwakePoll):
		case <-ctx.Done():
			return fmt.Errorf("waiting for the PLC's AMS router: %w", ctx.Err())
		}
	}
}

// isProbeRetryable reports whether a probe error is a transport flap worth a
// redial before falling back to AddRoute: a missing route and a stale TCP slot look
// identical, and registering on the transient one evicts sibling TCPs (Beckhoff
// #49). Excludes DeadlineExceeded and ADS-level rejections.
func isProbeRetryable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrTransportClosed) || errors.Is(err, io.EOF) {
		return true
	}
	if errors.Is(err, syscall.ECONNRESET) {
		return true
	}
	return false
}

// ensureRouteOnConnect probes the PLC and registers a route if needed. Reports
// whether one was added, in which case the caller should reconnect. On a
// transport-level probe failure it redials and retries rather than registering,
// since a transient slot conflict looks identical to a missing route.
func (sess *Session) ensureRouteOnConnect(ctx context.Context) (registered bool, err error) {
	if sess.isClosed() {
		return false, fmt.Errorf("connection closed")
	}

	// Disarm ondrop for the whole call: an RST during the probe is normal when the
	// route is missing, and would otherwise spawn a Reconnect racing our own
	// AddRoute/redial. Re-armed by defer. beginHandshake rides along so the
	// expected probe faults do not surface as ERROR.
	if oldClient := sess.client.Load(); oldClient != nil {
		oldClient.SetOnDrop(nil)
		oldClient.beginHandshake()
	}
	defer func() {
		if c := sess.client.Load(); c != nil {
			c.endHandshake()
			c.SetOnDrop(sess.triggerReconnect)
		}
	}()

	// Force mode → always register
	if sess.route.forceRouteRegistration {
		sess.logger.Info("registering route (force mode)")
		err = sess.AddRoute(ctx, sess.route.name, sess.route.username, string(sess.route.password))
		if err == nil {
			sess.route.markRegistered()
		}
		return err == nil, err
	}

	// First probe attempt
	_, probeErr := sess.probeRouteVersion(ctx)
	if probeErr == nil {
		sess.logger.Info("route already exists on PLC, skipping registration")
		sess.route.routeProbeFailures.Store(0)
		return false, nil
	}

	if sess.isClosed() {
		return false, fmt.Errorf("connection closed during route probe")
	}

	// Transport-level failure may be transient (PLC slot conflict from
	// previous TCP not yet released). Redial + retry probe once before
	// concluding the route is missing.
	if isProbeRetryable(probeErr) {
		// A deaf router cannot answer a probe OR a registration, so registering is
		// pointless; report it as retryable instead. Measured at ~8s on a TC3 4024.
		if err := sess.awaitRouterAwake(ctx); err != nil {
			return false, err
		}
		// The router is alive, so the route it holds is intact and only the TCP
		// path is failing. Keep re-probing for the grace rather than registering a
		// route that already exists: measured on a TC3 4024, one retry at 1.6s was
		// short and the same probe succeeded 36s later, having registered nothing.
		deadline := time.Now().Add(routeProbeGrace)
		for attempt := 1; ; attempt++ {
			sess.logger.Info("route probe failed at transport layer; retrying rather than registering a route the PLC may already hold",
				"error", probeErr, "delay", routeProbeRetryDelay, "attempt", attempt)
			// ctx-aware sleep: honor caller cancellation. Plain time.Sleep
			// would block the full delay even if the caller has given up.
			select {
			case <-time.After(routeProbeRetryDelay):
			case <-ctx.Done():
				return false, fmt.Errorf("route probe retry aborted: %w", ctx.Err())
			}
			if dialErr := sess.redialDuringHandshake(); dialErr != nil {
				return false, fmt.Errorf("redial during route probe retry: %w", dialErr)
			}
			// redialDuringHandshake leaves ondrop disarmed and the new Client in a
			// handshake region, which is what the rest of ensureRouteOnConnect needs;
			// the deferred re-arm at function exit restores the production handler.
			if sess.isClosed() {
				return false, fmt.Errorf("connection closed during route probe retry")
			}
			_, retryErr := sess.probeRouteVersion(ctx)
			if retryErr == nil {
				sess.logger.Info("route already exists on PLC (confirmed after retry)", "attempts", attempt)
				sess.route.routeProbeFailures.Store(0)
				return false, nil
			}
			probeErr = fmt.Errorf("probe failed after %d retries: %w", attempt, retryErr)
			// Anything but a transport flap is a real answer: stop and let the
			// registration below deal with it.
			if !isProbeRetryable(retryErr) || time.Now().After(deadline) {
				break
			}
		}
	}

	// Definite probe failure → register, unless this session did so recently.
	sess.route.routeProbeFailures.Add(1)
	if !sess.route.mayRegister() {
		sess.logger.Info("route probe failed but this session already registered the route; not registering again",
			"error", probeErr)
		return false, nil
	}
	sess.logger.Info("route probe failed, registering route", "error", probeErr)
	err = sess.AddRoute(ctx, sess.route.name, sess.route.username, string(sess.route.password))
	if err == nil {
		sess.route.markRegistered()
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// Route activation: the router acks an AddRoute before it necessarily serves it,
// and until then requests are dropped in silence. Re-probe until it answers, so
// Connect either works or fails honestly. The ceiling is generous because only a
// route that never comes live pays it.
const (
	defaultRouteActivationTimeout = 10 * time.Second
	routeActivationPollDelay      = 250 * time.Millisecond
	minRouteActivationProbe       = 500 * time.Millisecond
	maxRouteActivationProbe       = 2 * time.Second

	// Caps the TCP connections one activation window may burn -- uncapped it
	// redialled on every poll, ~40 sockets in 11s, each evicting its own predecessor
	// since the router keeps one TCP per host (Beckhoff #49).
	maxRouteActivationRedials = 3
	// redialBackoffBase/redialBackoffMax bound the wait between redials. The wait
	// happens BEFORE the redial, which is what gives the PLC time to release the
	// slot the previous connection held -- see awaitRouteActive.
	redialBackoffBase = 250 * time.Millisecond
	redialBackoffMax  = 2 * time.Second
)

// redialBackoff returns the wait before the nth redial of an activation window
// (n=0 is the first). Shift-with-cap: 250ms, 500ms, 1s, then flat. Pure and
// separate because a loop that waits wrong is invisible -- it just retries at the
// wrong rate.
func redialBackoff(n int) time.Duration {
	if n < 0 {
		n = 0
	}
	if n > maxFailureBackoffShift {
		n = maxFailureBackoffShift
	}
	d := redialBackoffBase << n
	if d > redialBackoffMax || d <= 0 {
		return redialBackoffMax
	}
	return d
}

// routeActivationBudget returns the total wait and the per-probe timeout
// derived from it. Deriving the probe timeout keeps a shortened total
// coherent — a fixed probe timeout larger than the total would allow exactly
// one attempt, and that attempt would overrun the budget.
func (sess *Session) routeActivationBudget() (total, probe time.Duration) {
	total = sess.route.activationTimeout
	if total <= 0 {
		total = defaultRouteActivationTimeout
	}
	probe = total / 4
	if probe < minRouteActivationProbe {
		probe = minRouteActivationProbe
	}
	if probe > maxRouteActivationProbe {
		probe = maxRouteActivationProbe
	}
	return total, probe
}

// effectiveRouterPort is the UDP router port for this session, falling back to
// the protocol default for a Session built without going through NewSession
// (test helpers do this).
func (sess *Session) effectiveRouterPort() int {
	if sess.routerPort > 0 {
		return sess.routerPort
	}
	return router.DefaultPort
}

// currentLifecycleCtx returns the live lifecycle context. tearDownAndReset cancels
// the old one and installs a fresh one under ctxMu, so anything spanning a
// teardown must re-read rather than capture it, and a bare read races the swap.
func (sess *Session) currentLifecycleCtx() context.Context {
	sess.lifecycle.ctxMu.RLock()
	defer sess.lifecycle.ctxMu.RUnlock()
	return sess.lifecycle.ctx
}

// waitDuringActivation sleeps during an activation window, honouring three stop
// signals. The third is why it exists: the attempt ctx is the caller's own, which
// Close does not cancel, so a two-arm select would wake after Close returned and
// dial a fresh socket.
func (sess *Session) waitDuringActivation(ctxFor func() context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-sess.lifecycle.closedCh:
		return fmt.Errorf("connection closed while waiting for route activation")
	case <-ctxFor().Done():
		return fmt.Errorf("route activation wait aborted: %w", ctxFor().Err())
	}
}

// redialDuringHandshake replaces the transport mid-handshake, setting
// tx.disconnected across the gap and re-disarming ondrop, which publishWiredClient
// arms on every new Client -- without that an RST spawns the rival Reconnect the
// disarm prevents. Holds dialMu for the pair.
func (sess *Session) redialDuringHandshake() error {
	sess.lifecycle.dialMu.Lock()
	defer sess.lifecycle.dialMu.Unlock()
	sess.tx.disconnected.Store(true)
	sess.tearDownAndReset()
	if err := sess.dialAndStart(); err != nil {
		return err
	}
	if c := sess.client.Load(); c != nil {
		c.SetOnDrop(nil)
		c.beginHandshake()
	}
	return nil
}

// awaitRouteActive re-probes after a registration until one round-trips, redialing
// when the PLC drops mid-probe. ctxFor is a function because the redial replaces
// lifecycle.ctx, and passing it by value would cancel the loop's own context.
// ondrop stays disarmed so an RST cannot spawn a rival Reconnect.
func (sess *Session) awaitRouteActive(ctxFor func() context.Context) (uint8, error) {
	if c := sess.client.Load(); c != nil {
		c.SetOnDrop(nil)
		c.beginHandshake()
	}
	defer func() {
		if c := sess.client.Load(); c != nil {
			c.endHandshake()
			c.SetOnDrop(sess.triggerReconnect)
		}
	}()

	total, probeTimeout := sess.routeActivationBudget()
	deadline := time.Now().Add(total)
	var lastErr error
	// redials counts sockets this window has burned, capped by
	// maxRouteActivationRedials. Goroutine-local: one activation window has one
	// owner, either Connect or the reconnect goroutine.
	redials := 0
	for attempt := 1; ; attempt++ {
		if sess.isClosed() {
			return 0, fmt.Errorf("connection closed while waiting for route activation")
		}
		ctx := ctxFor()
		probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
		version, err := sess.probeRouteVersion(probeCtx)
		cancel()
		lastErr = err
		if lastErr == nil {
			sess.route.routeProbeFailures.Store(0)
			if attempt > 1 {
				sess.logger.Info("route active after registration", "probeAttempts", attempt)
			}
			return version, nil
		}
		// The probe reads the symbol version off the RUNTIME port, which does not
		// exist in CONFIG. Ask the system service instead: if it answers, the route
		// is being served and only the runtime is missing -- otherwise a session
		// starting while the PLC is in CONFIG dies here instead of waiting.
		if c := sess.client.Load(); c != nil {
			stateCtx, stateCancel := context.WithTimeout(ctxFor(), probeTimeout)
			state, serr := c.ReadStateOnPort(stateCtx, ams.PortSystemService)
			stateCancel()
			if serr == nil {
				sess.recordRuntimeState(state.State)
				if state.State != ams.StateRun {
					sess.route.routeProbeFailures.Store(0)
					sess.logger.Warn("route is active but the PLC runtime is not in RUN; connecting anyway and waiting for it",
						"state", uint16(state.State), "probeAttempts", attempt)
					// ErrRuntimeNotRunning, not (0, nil): the route is proven but no version
					// was read, and reporting 0 as the real version disables
					// online-change detection and skips the liveness block.
					return 0, fmt.Errorf("route is served but %w (ADS state %d)", ErrRuntimeNotRunning, uint16(state.State))
				}
			}
		}
		if !time.Now().Before(deadline) {
			break
		}
		if isProbeRetryable(lastErr) {
			// Budget spent and no socket left: every further probe fails instantly, so
			// polling on is a tight spin that cannot discover anything. Without this,
			// capping the redials makes the loop worse rather than better.
			if redials >= maxRouteActivationRedials && isTransportDead(lastErr) {
				sess.logger.Error("route activation gave up: redial budget spent and the transport is gone",
					"attempt", attempt, "redials", redials, "error", lastErr)
				break
			}
			wait := redialBackoff(redials)
			sess.logger.Debug("route not served by PLC yet, waiting then redialing",
				"attempt", attempt, "error", lastErr, "delay", wait, "redials", redials)
			// The wait comes BEFORE the redial: dialing straight after our own close is
			// the worst moment, since the PLC still holds the slot the closed
			// connection occupied and the new one gets refused. Costs nothing -- the
			// budget is spent waiting either way.
			if err := sess.waitDuringActivation(ctxFor, wait); err != nil {
				return 0, err
			}
			if sess.isClosed() {
				return 0, fmt.Errorf("connection closed while waiting for route activation")
			}
			if err := sess.redialDuringHandshake(); err != nil {
				return 0, fmt.Errorf("redial while waiting for route activation: %w", err)
			}
			redials++
			continue
		}
		sess.logger.Debug("route not served by PLC yet, re-probing",
			"attempt", attempt, "error", lastErr, "delay", routeActivationPollDelay)
		if err := sess.waitDuringActivation(ctxFor, routeActivationPollDelay); err != nil {
			return 0, err
		}
	}
	return 0, fmt.Errorf("route %q was registered but the PLC did not serve it within %v (%d redials): %w",
		sess.route.name, total, redials, lastErr)
}

// probeRouteVersion sends a GetSymbolVersion to verify the PLC accepts our source
// NetID. The route is proven by the round-trip succeeding at all; the version is a
// free by-product, saving a caller an identical second trip.
func (sess *Session) probeRouteVersion(ctx context.Context) (uint8, error) {
	return sess.client.Load().GetSymbolVersion(ctx)
}

// ensureRoute checks if the route exists (via probe) and registers if needed.
// On force mode or after repeated probe failures, skips the probe.
// Returns a non-nil error only if registration was attempted and failed critically
// (requiring a TCP reset / retry).
func (sess *Session) ensureRoute() error {
	if sess.route.shouldSkip() {
		return nil
	}
	if sess.isClosed() {
		return fmt.Errorf("connection closed")
	}

	// Register on force or repeated probe failures, unless this session registered
	// recently -- re-registering fixes nothing and on some firmware leaves a
	// duplicate entry. Force bypasses the latch, or the option would mean "register
	// once, then stop" and fail the reboot case it exists for.
	probeFailures := sess.route.routeProbeFailures.Load()
	if sess.route.forceRouteRegistration || probeFailures >= 3 {
		if !sess.route.forceRouteRegistration && !sess.route.mayRegister() {
			sess.logger.Debug("route already registered by this session; not registering again",
				"probeFailures", probeFailures)
			_, err := sess.awaitRouteActive(sess.currentLifecycleCtx)
			return err
		}
		sess.logger.Info("registering route (forced/fallback)", "probeFailures", probeFailures)
		if err := sess.AddRoute(sess.currentLifecycleCtx(), sess.route.name, sess.route.username, string(sess.route.password)); err != nil {
			return fmt.Errorf("route registration failed: %w", err)
		}
		sess.route.markRegistered()
		// Same activation lag as on connect. Failing here feeds the reconnect
		// loop's own retry rather than letting reloadSymbols be the first thing
		// to discover the route isn't live yet. currentLifecycleCtx, not a
		// captured ctx: awaitRouteActive's redial replaces lifecycle.ctx.
		if _, err := sess.awaitRouteActive(sess.currentLifecycleCtx); err != nil {
			return err
		}
		sess.route.routeProbeFailures.Store(0)
		return nil
	}

	// Probe: try a lightweight ADS command to see if route already exists
	_, probeErr := sess.probeRouteVersion(sess.currentLifecycleCtx())
	if probeErr == nil {
		sess.logger.Debug("route still valid, skipping re-registration")
		sess.route.routeProbeFailures.Store(0)
		return nil
	}

	if sess.isClosed() {
		return fmt.Errorf("connection closed during route probe")
	}

	// Probe failed → register with credentials, unless we already did recently.
	failuresAfter := sess.route.routeProbeFailures.Add(1)
	if !sess.route.mayRegister() {
		sess.logger.Debug("route probe failed but this session already registered the route; waiting for it to be served instead of registering again",
			"error", probeErr, "probeFailures", failuresAfter)
		_, err := sess.awaitRouteActive(sess.currentLifecycleCtx)
		return err
	}
	sess.logger.Info("route probe failed, registering route", "error", probeErr, "probeFailures", failuresAfter)
	if err := sess.AddRoute(sess.currentLifecycleCtx(), sess.route.name, sess.route.username, string(sess.route.password)); err != nil {
		return fmt.Errorf("route registration failed after probe: %w", err)
	}
	sess.route.markRegistered()
	_, err := sess.awaitRouteActive(sess.currentLifecycleCtx)
	return err
}

// AddRoute registers a route on the remote PLC using this connection's settings.
// It uses callbackIP (from WithHostIP) if set, otherwise derives the callback
// address from the source AMS NetID (first 4 bytes = IP).
func (sess *Session) AddRoute(ctx context.Context, routeName, username, password string) error {
	// One snapshot on the caller's goroutine, for both the derived host IP and the
	// registration. AddRoute is callable from any goroutine while localHandshake
	// writes sess.source, and the goroutine below outlives this call -- a torn NetID
	// registers a route for an identity that exists nowhere.
	netID := sess.sourceAddr().NetID
	hostIP := sess.callbackIP
	if hostIP == "" {
		hostIP = fmt.Sprintf("%d.%d.%d.%d", netID[0], netID[1], netID[2], netID[3])
	}
	return router.AddRoute(ctx, sess.ip, router.Route{
		Name:         routeName,
		LocalNetID:   netID,
		ComputerName: hostIP,
		Username:     username,
		Password:     password,
	}, sess.routerOptions()...)
}

// routerOptions addresses the AMS router's UDP service the way this session's
// ADS traffic is addressed: same local interface, same (possibly NAT-forwarded)
// router port. Otherwise discovery and registration can traverse a different
// NIC than the connection they are meant to serve.
func (sess *Session) routerOptions() []router.Option {
	opts := []router.Option{router.WithLogger(sess.logger), router.WithPort(sess.effectiveRouterPort())}
	if ip, ok := netip.AddrFromSlice(sess.localBindIP); ok {
		opts = append(opts, router.WithLocalIP(ip.Unmap()))
	}
	return opts
}

// isRunningInContainer returns true if the process is running inside a
// Docker, Podman, or Kubernetes container. Uses filesystem markers rather
// than IP range heuristics (10.x is common in industrial OT networks).
func isRunningInContainer() bool {
	// Docker/Podman marker file
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return true
	}
	// Check cgroup for container runtime (Linux only)
	data, err := os.ReadFile("/proc/1/cgroup")
	if err != nil {
		return false
	}
	s := string(data)
	return strings.Contains(s, "docker") || strings.Contains(s, "containerd") ||
		strings.Contains(s, "kubepods") || strings.Contains(s, "lxc")
}
