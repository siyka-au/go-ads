package ads

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/siyka-au/go-ads/v3/ams"
	"github.com/siyka-au/go-ads/v3/router"
)

// randomAMSPort returns a random AMS source port in the dynamic range. The PLC
// keys its notification table by {source NetID, port, handle}, so a fresh port per
// session means a prior process's subscriptions age out instead of competing with
// the new connection. WithLocalAMS overrides it where a stable port is needed.
func randomAMSPort() ams.Port {
	const minPort, span = 32768, 49151 - 32768 + 1
	return ams.Port(minPort + rand.IntN(span)) //nolint:gosec // non-cryptographic port selection
}

// secret wraps credential strings so String() and slog.LogValuer return
// "[REDACTED]", defending against fmt.Sprintf("%+v", sess) and slog.Any. Kept
// unexported; the public API takes plain strings and converts at the boundary.
type secret string

func (s secret) String() string {
	return "[REDACTED]"
}

func (s secret) LogValue() slog.Value {
	return slog.StringValue("[REDACTED]")
}

// Session is the managed wrapper around one ADS *Client: symbol cache, persistent
// notifications, lifecycle FSM, auto-reconnect and route state. It does NOT embed
// *Client -- raw RPCs live on a *Client from Dial.
type Session struct {
	ip   string
	port int

	// Underlying RPC client. nil until Connect succeeds; replaced on
	// Reconnect; shut down by Close. atomic.Pointer so concurrent reads on
	// user RPC paths cannot race the publish in Connect / dialAndStart.
	client atomic.Pointer[Client]

	// TCP socket + request multiplexing + listen/transmit channels.
	tx *transport

	target      ams.Address
	source      ams.Address
	callbackIP  string // IP PLC uses to reach us (for Docker/VPN; set via WithHostIP)
	localBindIP net.IP // Force outbound TCP source IP (multi-session per host; set via WithLocalBindIP). nil = OS default routing.

	// symbol cache + data-type table + discovery-mode flags.
	cache *symbolCache

	// Notification state (activeNotifications, notificationConfigs,
	// notificationChannel, lastSubscribeNs).
	notifications *notificationManager

	// Lifecycle FSM (ctx, shutdown, waitGroup, reconnect/closed flags + channels,
	// generation counter, retry policy).
	lifecycle *sessionLifecycle

	requestTimeout time.Duration
	isLocal        bool

	// peerListenPort, when non-zero, is the TCP port on which the session accepts
	// a connection the PLC opens back to us, for devices that answer there rather
	// than on the connection we opened. See WithAmsPeerListen.
	peerListenPort int
	peerLn         net.Listener
	// peerWG tracks the accept loop. Deliberately NOT lifecycle.waitGroup: that
	// one is waited on by tearDownAndReset, which runs on every reconnect, while
	// this listener lives for the whole session and only closes in Close — so
	// sharing the group deadlocks the first teardown.
	peerWG sync.WaitGroup
	// peerMu guards peerLn: sync.Once orders only the goroutines calling Do, and
	// Close never does. Unsynchronised, Close reads nil, skips the listener and
	// blocks for ever in peerWG.Wait on an accept loop nothing wakes.
	peerMu      sync.Mutex
	peerStopped bool
	// peerFallbackDisabled turns off the automatic attempt described in
	// tryPeerFallback. See WithoutAmsPeerFallback.
	peerFallbackDisabled bool
	// Inbound connections handed to a Client. Non-zero means the device really does
	// answer on one it opens to us, which is what forgetPeerRouteHostIfUnused needs.
	// Atomic: the accept loop writes it while Connect reads it.
	peerConnsAdopted atomic.Int64

	// Heartbeat: an internal cyclic notification whose silence proves the caller's
	// subscriptions have died. See notificationManager.heartbeatHandle.
	heartbeatInterval time.Duration
	heartbeatMissed   int
	heartbeatDisabled bool
	heartbeatOnce     sync.Once
	heartbeatWG       sync.WaitGroup
	// What WithNotificationSilenceTimeout asked for, converted to heartbeatMissed
	// once after the option loop (normalizeHeartbeatOptions). Not lazily at first
	// use: the watcher reads it every tick, so writing there would be a data race.
	heartbeatSilence time.Duration
	// heartbeatRecovery selects what happens when the heartbeat goes silent. Plain
	// field, written during option application, read by the watcher.
	heartbeatRecovery HeartbeatRecovery

	// stateWatchInterval is how often the runtime-state poller asks the system
	// service, and stateWatchDisabled turns it off. Independent of the heartbeat
	// since 2026-08: the poll used to run at heartbeatCycle(), so
	// WithNotificationHeartbeat(30s, ...) silently made the state poll 30s too.
	stateWatchInterval time.Duration
	stateWatchDisabled bool

	// Last ADS state read from the system service port, and when. Zero means "not
	// known yet": the gates refuse only on a positive non-RUN reading, never on the
	// absence of one. Without asking port 10000 a session cannot tell "the runtime
	// is in CONFIG" from "this device is broken", and retries blindly either way.
	runtimeState   atomic.Uint32
	runtimeStateNs atomic.Int64
	// stateWG, not lifecycle.waitGroup: the watch lives for the whole session, and
	// tearDownAndReset waits lifecycle.waitGroup on every reconnect — putting it
	// there deadlocked the first reconnect, exactly as it once did for the peer
	// accept loop.
	stateWG   sync.WaitGroup
	stateOnce sync.Once

	// Event callbacks (run in goroutine, must not block)
	onDisconnect func()
	onReconnect  func()

	// Online-change handling (R-SES-011, R-CACHE-013).
	versionStrategy   SymbolVersionStrategy
	versionCallback   func(reason Reason)
	maxReloadAttempts int
	reloadWindow      time.Duration
	reloadAttempts    []time.Time
	reloadMu          sync.Mutex
	reloadInProgress  atomic.Bool
	staleHandles      map[uint32]Reason
	staleHandlesMu    sync.Mutex

	// Route registration config (populated by WithRoute / WithForceRouteRegistration).
	route *routeManager

	// targetCheck is the policy for a caller-supplied target NetID that the
	// device disagrees with. Write-once at construction via WithTargetCheck.
	targetCheck TargetCheck

	// routerPort is the UDP port for route registration and identify. Set from
	// AMSEndpoint.RouterPort; 0 means the protocol default.
	routerPort int

	logger *slog.Logger
}

// AMSEndpoint identifies a remote ADS endpoint at the TCP and AMS layers.
// IP+Port locate the TwinCAT runtime over TCP (port 48898 by default);
// AMS is the target Address carried in every ADS request header.
type AMSEndpoint struct {
	// IP is the host or address of the PLC (or of the NAT that forwards to it).
	IP string
	// Port is the TCP port carrying AMS. Defaults to 48898, TwinCAT's own.
	Port int
	// AMS is the target AMS address. A zero NetID and/or Port is resolved from
	// the device — see NewSession.
	AMS ams.Address
	// RouterPort is the AMS router's UDP port (default 48899), used to register a
	// route and identify the device. Set it behind NAT, where the forwarded UDP
	// port differs from the TCP one and cannot be derived from Port. Only the two
	// UDP calls use it; notifications arrive on the TCP connection we opened.
	RouterPort int
}

// NewSession creates a session targeting remote. No I/O until Connect, except to
// resolve an incomplete remote.AMS over UDP -- the port then follows the TC major
// version, so several runtimes means setting it explicitly. Local NetID and AMS
// port default so sessions cannot collide on the PLC's handle table.
func NewSession(ctx context.Context, remote AMSEndpoint, opts ...SessionOption) (sess *Session, err error) {
	if remote.IP == "" {
		return nil, fmt.Errorf("ads: NewSession: remote.IP must be set")
	}
	if remote.Port <= 0 {
		remote.Port = 48898 // TwinCAT TCP default
	}
	if remote.RouterPort <= 0 {
		remote.RouterPort = router.DefaultPort // TwinCAT UDP default
	}
	if ctx == nil {
		ctx = context.Background()
	}
	sessCtx, cancel := context.WithCancel(ctx) //nolint:gosec // cancel stored in lifecycle.shutdown, called from Close + tearDownAndReset
	sess = &Session{
		ip:             remote.IP,
		port:           remote.Port,
		routerPort:     remote.RouterPort,
		target:         remote.AMS,
		requestTimeout: 5 * time.Second,
		route:          &routeManager{},
		notifications: &notificationManager{
			activeNotifications: make(map[uint32]activeNotification),
			configsByKey:        make(map[string]struct{}),
			orphanSeen:          make(map[uint32]time.Time),
			orphanSem:           make(chan struct{}, orphanDeleteMaxConcurrency),
		},
		cache: &symbolCache{
			symbols:         map[string]*symbol{},
			onDemandSymbols: map[string]bool{},
		},
		tx: &transport{
			sendChannel:    make(chan []byte),
			systemResponse: make(chan []byte, 1),
			recvQueue:      make(chan []byte, recvQueueSize),
			activeRequests: map[uint32]chan amsReply{},
		},
		lifecycle: &sessionLifecycle{
			autoReconnect:        true,
			maxReconnectAttempts: 0, // 0 = infinite retries
			backoffConfig:        DefaultBackoffConfig(),
			closedCh:             make(chan struct{}),
			parentCtx:            ctx,
			ctx:                  sessCtx,
			shutdown:             cancel,
		},
		logger: slog.Default(),
	}
	// idempotent: zero state is Constructed already
	sess.lifecycle.state.transitionTo(SessionStateConstructed)
	// Online-change defaults (R-SES-011, R-CACHE-013). Applied before opts so
	// callers can override.
	sess.maxReloadAttempts = 3
	sess.reloadWindow = 60 * time.Second
	// Verify a caller-supplied target NetID against the device by default, but
	// only warn on a mismatch — see TargetCheck for why it cannot be an error.
	sess.targetCheck = TargetCheckWarn
	// Default local AMS port: random in IANA dynamic range so each process /
	// each session presents a distinct AMS source identity to the PLC. See
	// randomAMSPort doc for the rationale. WithLocalAMS overrides for stable-
	// port deployments (firewalled environments, container port allow-lists).
	sess.source.Port = randomAMSPort()
	for _, opt := range opts {
		opt(sess)
	}
	sess.normalizeHeartbeatOptions()
	// Fill in what the caller omitted by asking the router for its identity, read
	// from sess.target AFTER the options ran. Only when something is missing --
	// omitting the address is itself the request to look it up. Never in local
	// mode, where Connect overwrites NetID and IP anyway.
	needsDiscovery := !sess.isLocal &&
		(sess.target.NetID == [6]byte{} || sess.target.Port == 0)
	if needsDiscovery {
		if err := sess.discoverTarget(ctx); err != nil {
			cancel()
			return nil, err
		}
	}
	return sess, nil
}

// IsDisconnected returns whether the connection is currently in a disconnected state.
func (sess *Session) IsDisconnected() bool {
	return sess.isDisconnected()
}

// IsClosed reports whether the session has reached the terminal Closed
// state. A closed session cannot be reused; construct a new one via
// NewSession. Distinct from IsDisconnected (transient transport loss).
func (sess *Session) IsClosed() bool {
	return sess.isClosed()
}
