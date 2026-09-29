package ads

import (
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/siyka-au/go-ads/v3/internal/adsconn"

	"github.com/siyka-au/go-ads/v3/internal/symtab"

	"github.com/siyka-au/go-ads/v3/ams"
)

// notificationManager owns the connection-level notification state: the per-handle
// symbol map, the configs for reconnect re-subscribe, the user channel, and the
// last-subscribe timestamp that suppresses race-window warnings.
//
// Lock ordering: NEVER hold cache.lock and notifications.lock at once.
// activeNotification pairs the symbol with its channel here rather than on the
// symbol struct, keeping notifications.lock-guarded state out of cache.symbols.
type activeNotification struct {
	Sym *symtab.Symbol
	Ch  chan<- *Update
}

type notificationManager struct {
	lock                sync.Mutex
	activeNotifications map[uint32]activeNotification
	// pending holds the resubscribe-aware copy of every active user
	// subscription. Internal type — exposed via NotificationConfig in public
	// API. configsByKey mirrors pending for O(1) duplicate-subscribe probes
	// (hot path under bulk Add). Keys are lower-cased symbol names to match
	// the EqualFold semantic used elsewhere. MUST be kept in lockstep with
	// pending — use addConfig / removeConfigByName / resetConfigs to mutate.
	pending             []pendingNotification
	configsByKey        map[string]struct{}
	notificationChannel chan *Update

	// lastSubscribeNs is the last time anything in the subscribe lifecycle
	// happened — a subscribe started, a subscribe finished, or a sweep refreshed
	// it. Deliberately all three: it drives only the short subscribeRaceWindow
	// tail that covers the gap after the last commit, and that tail should extend
	// past whichever of those happened most recently. It says nothing about what
	// is in flight; openSubscribes answers that.
	lastSubscribeNs atomic.Int64

	// Each in-flight subscribe with the time it began, so each is bounded by its own
	// age. A shared counter plus one timestamp could not express this: the pair was
	// not atomic, and one slot recorded when the last quiet period ended rather than
	// when anything in flight began. Lock order: openMu then earlyMu, never reverse.
	openMu             sync.Mutex
	openSubscribes     map[subscribeToken]int64
	nextSubscribeToken uint64

	// subscribeInFlight mirrors len(openSubscribes) for lock-free reads on the
	// dispatch path and in tests. Written under openMu; authoritative answers
	// about the window come from newestOpenSubscribe. A subscribe is in flight
	// once it has issued the PLC-side Add and before it has committed every
	// handle into activeNotifications: during that gap an unknown handle is
	// presumed ours, so the sample is buffered (see earlySamples) and the orphan
	// reaper stays out.
	subscribeInFlight atomic.Int64

	// earlySamples holds samples that arrived for a handle before its
	// activeNotifications insert landed, keyed by handle. Guarded by
	// earlyMu (separate from lock so buffering never serializes against
	// the dispatch hot path). Only the most recent sample per handle is
	// kept — the point is not to replay history but to not lose the ONLY
	// sample a static symbol ever emits, and a live symbol's later samples
	// arrive on the normal path anyway.
	earlyMu      sync.Mutex
	earlySamples map[uint32]earlySample
	// earlyBytes is the total content held in earlySamples. The handle cap alone
	// does not bound memory: a sample is network-sized, and struct or array
	// symbols run to tens of KB, so 4096 of them is hundreds of MB decided
	// entirely by what the PLC sends for handles we do not recognise.
	earlyBytes int

	// heartbeatHandle is an internal CYCLIC notification on the symbol-version
	// index group. A subscription can die silently (TC3 across CONFIG->RUN: TCP up,
	// symbol version unchanged, no error, no more samples), and an on-change
	// subscription may be silent legitimately -- so only a cyclic beat's silence is
	// conclusive. 0xF008 dies with the runtime's notification table and its payload
	// is the symbol version, so each beat doubles as online-change detection.
	heartbeatHandle atomic.Uint32
	// heartbeatBeats counts delivered beats; the watchdog compares it against the
	// previous tick so the decision never touches a clock. A wall-clock step -- an
	// IPC with no RTC on its first NTP sync, a resumed VM -- otherwise reads as
	// elapsed time and kills every live subscription.
	heartbeatBeats atomic.Uint64
	// heartbeatEstablishFailures counts consecutive failures to register the beat,
	// so a PLC that refuses it permanently costs one Warn rather than one per retry.
	heartbeatEstablishFailures atomic.Int64
	// registered is what a healthy session holds, compared against
	// len(activeNotifications) to spot subscriptions that died unasked. Not
	// len(pending), which is a superset and outlives a handle. Raised by any commit;
	// lowered only by a caller teardown or a symbol the PLC no longer has.
	registered atomic.Int64
	// Samples for handles we do not own, and the deletes that follow, counted since
	// the last report. Reported on a timer rather than per episode: after a
	// reconnect orphans interleave with healthy samples, so a counter reset by the
	// next owned sample ends the episode at once and every orphan reports again.
	orphanSamples  atomic.Int64
	orphanWarnNs   atomic.Int64
	orphanDeletes  atomic.Int64
	orphanDeleteNs atomic.Int64
	// heartbeatLastNs is kept for the log line only ("silentFor"), never for the
	// decision. A stepped clock makes it a confusing number, not a wrong outcome.
	heartbeatLastNs atomic.Int64

	// resubscribeMu serialises whole re-subscribe sequences. Three paths run one --
	// reconnect, auto-reload after an online change, heartbeat recovery -- and each
	// snapshots pending and clears it, so two at once leaves one reading an empty
	// intent or both registering the same symbols twice on the PLC. Not `lock`,
	// which is the dispatch hot path's; never held while taking it beyond a snapshot.
	resubscribeMu sync.Mutex

	// orphanDelete tracks unknown-handle Delete attempts so we don't spam
	// the PLC when a previously-leaked subscription keeps firing. Guarded
	// by orphanMu (separate from notifications.lock so the throttle check
	// doesn't serialize against the dispatch hot path).
	orphanMu   sync.Mutex
	orphanSeen map[uint32]time.Time
	orphanSem  chan struct{} // bounded concurrency: cap = orphanDeleteMaxConcurrency
}

// subscribeToken identifies one in-flight subscribe. Returned by beginSubscribe
// and handed back to endSubscribe so each subscribe closes its own entry rather
// than sharing a counter with every other one.
type subscribeToken uint64

// earlySample is one buffered notification sample awaiting its handle's
// activeNotifications insert.
type earlySample struct {
	timestamp uint64
	content   []byte
}

// addConfig wraps cfg into a fresh pendingNotification (resubscribeAttempts=0)
// and files it under its symbol, so a successful subscribe naturally resets any
// prior retry counter. Caller must hold lock.
func (m *notificationManager) addConfig(cfg NotificationConfig) {
	m.setPending(pendingNotification{Config: cfg})
}

// addPending files an already-wrapped pending entry (preserving its
// resubscribeAttempts counter). Used by resubscribeNotifications when
// re-queueing Skipped retries. Caller must hold lock.
func (m *notificationManager) addPending(p pendingNotification) {
	m.setPending(p)
}

// setPending files one entry per symbol, REPLACING any entry already on file for
// that symbol rather than appending a second. Caller must hold lock.
//
// configsByKey cannot represent two entries for one symbol, and pending must not
// either: a duplicated entry there makes the next resubscribe register the symbol
// twice on the PLC, and the caller can only ever delete one of them. The case that
// reaches this with the key already present is a legal re-declaration of a symbol
// that is on file but not live — see hasLiveNotification.
func (m *notificationManager) setPending(p pendingNotification) {
	key := symtab.Key(p.Config.SymbolName)
	if _, onFile := m.configsByKey[key]; onFile {
		for i := range m.pending {
			if symtab.Key(m.pending[i].Config.SymbolName) == key {
				m.pending[i] = p
				return
			}
		}
	}
	m.pending = append(m.pending, p)
	m.configsByKey[key] = struct{}{}
}

// hasConfig returns true if any existing config matches symbolName
// (case-insensitive). Caller must hold lock.
//
// This answers "did the caller declare this symbol", NOT "is it subscribed" — use
// hasLiveNotification for the duplicate decision.
func (m *notificationManager) hasConfig(symbolName string) bool {
	_, ok := m.configsByKey[symtab.Key(symbolName)]
	return ok
}

// hasLiveNotification reports whether symbolName has a committed handle. Caller
// must hold lock. This, not hasConfig, is what makes a subscribe a duplicate:
// pending is the caller's intent and outlives a handle, since a resubscribe
// re-queues retryable refusals that have no handle at all. Deciding on pending
// left such a symbol un-subscribable for the life of the session, with no exported
// way out because deletes work by handle.
func (m *notificationManager) subscriptionGap() (want, have int) {
	m.lock.Lock()
	defer m.lock.Unlock()
	return int(m.registered.Load()), len(m.activeNotifications)
}

// reportNow reports true at most once per interval, so a burst produces one line
// instead of one per event. Only the caller winning the CAS reports.
func reportNow(last *atomic.Int64, interval time.Duration) bool {
	now := time.Now().UnixNano()
	prev := last.Load()
	if now-prev < int64(interval) {
		return false
	}
	return last.CompareAndSwap(prev, now)
}

// orphanReportInterval bounds how often the orphan sample/delete lines appear.
const orphanReportInterval = 30 * time.Second

// raiseRegistered lifts the baseline to what is held now and never lowers it: a
// commit proves the session can hold that many, not that fewer is the new normal.
// Storing the count outright let a partial re-subscribe redefine healthy as the
// smaller set, leaving no gap for the symbols it failed to restore.
// Caller must hold lock.
func (m *notificationManager) raiseRegistered() {
	if n := int64(len(m.activeNotifications)); n > m.registered.Load() {
		m.registered.Store(n)
	}
}

// lowerRegisteredTo drops the baseline to n when it currently sits higher, for
// the cases where a healthy session genuinely holds less than it used to: a
// symbol the PLC no longer has, or a config abandoned after too many retries.
// Without it the gap check would chase handles that can never come back.
// Caller must hold lock.
func (m *notificationManager) lowerRegisteredTo(n int) {
	if int64(n) < m.registered.Load() {
		m.registered.Store(int64(n))
	}
}

func (m *notificationManager) hasLiveNotification(symbolName string) bool {
	key := symtab.Key(symbolName)
	for _, entry := range m.activeNotifications {
		if entry.Sym != nil && symtab.Key(entry.Sym.FullName) == key {
			return true
		}
	}
	return false
}

// liveNotificationNames snapshots the symbol keys that have a committed handle, so
// a batch can dup-check every candidate against a per-call copy instead of
// re-scanning under the manager lock per item. Caller must hold lock.
func (m *notificationManager) liveNotificationNames() map[string]struct{} {
	live := make(map[string]struct{}, len(m.activeNotifications))
	for _, entry := range m.activeNotifications {
		if entry.Sym != nil {
			live[symtab.Key(entry.Sym.FullName)] = struct{}{}
		}
	}
	return live
}

// resetConfigs swaps the entire slice and rebuilds the key index. Used by
// resubscribeNotifications during the save/rollback dance. Caller must hold lock.
func (m *notificationManager) resetConfigs(p []pendingNotification) {
	m.pending = p
	m.configsByKey = make(map[string]struct{}, len(p))
	for _, entry := range p {
		m.configsByKey[symtab.Key(entry.Config.SymbolName)] = struct{}{}
	}
}

// restoreConfigs puts a snapshot back WITHOUT discarding anything added since it
// was taken. Caller must hold lock.
//
// A recovery snapshots the caller's intent, tries, and puts the snapshot back if it
// failed. Doing that with resetConfigs is an overwrite: a subscribe that landed
// during the attempt is erased from the configs while its handle stays registered
// on the PLC, so it is never resubscribed after a reconnect and subscribing it
// again duplicates the registration. Anything already on file wins — it is newer
// than the snapshot by construction.
func (m *notificationManager) restoreConfigs(snapshot []pendingNotification) {
	for _, entry := range snapshot {
		if _, present := m.configsByKey[symtab.Key(entry.Config.SymbolName)]; present {
			continue
		}
		m.addPending(entry)
	}
}

// Reasons a batch subscribe refused to commit an entry, reported via
// SumNotificationResult.Skipped. They are sentinels because the right response
// differs per reason and callers should branch with errors.Is rather than on
// message text: a stranded or transport-failed entry is worth retrying, a
// duplicate or channel mismatch is a caller bug that retrying cannot fix.
//
// Each is wrapped with the symbol name, so errors.Is matches while the message
// still says which symbol.
var (
	// ErrNotificationDuplicate — the symbol already has an active notification.
	ErrNotificationDuplicate = errors.New("symbol already subscribed")
	// ErrNotificationChannelMismatch — all notifications on one connection must
	// share a single updateReceiver channel.
	ErrNotificationChannelMismatch = errors.New("all notifications on a connection must use the same updateReceiver channel")
	// ErrNotificationSymbolVanished — the symbol left the cache between resolve
	// and commit (online change, or a concurrent LoadSymbols).
	ErrNotificationSymbolVanished = errors.New("symbol removed from cache during subscribe")
	// ErrNotificationStrandedByReload — the symbol cache was reloaded while the
	// batch was in flight, so this entry's registration no longer exists.
	// Retryable: the caller (or resubscribeNotifications) should re-subscribe.
	ErrNotificationStrandedByReload = errors.New("symbol cache reloaded during batch subscribe")
	// ErrNotificationTransportFailure — the batch could not be sent.
	ErrNotificationTransportFailure = adsconn.ErrBatchAborted
)

// StaleInfo describes why a one-shot Stale Update was delivered. Non-nil iff
// the corresponding Update's value MAY be from a pre-online-change cache
// state. Reason is one of the documented Reason* constants.
type StaleInfo struct {
	Reason Reason
}

// Update is delivered to the user channel for each PLC notification sample.
// Stale is non-nil only on the first sample following a stale-cache detection
// (R-NOT-017 one-shot); the field is nil for normal samples. Couples the
// "this sample may be stale" signal with the reason in a single check —
// callers do `if u.Stale != nil { /* handle stale */ }`.
type Update struct {
	Variable string
	// Value is the sample as its Go type, as ReadValue returns it.
	Value     any
	TimeStamp time.Time
	Stale     *StaleInfo
}

// NotificationConfig holds configuration for a symbol notification, used for
// batch add and reconnect re-subscribe. MaxDelay and CycleTime use
// time.Duration for consistency with SumNotificationRequest and the rest of
// the standard library.
type NotificationConfig struct {
	SymbolName       string
	MaxDelay         time.Duration
	CycleTime        time.Duration
	TransmissionMode ams.TransMode
}

// pendingNotification wraps a user-supplied NotificationConfig with internal
// resubscribe bookkeeping. resubscribeAttempts is incremented each time a
// reconnect re-subscribe round returns the config as Skipped (TOCTOU loss
// against a concurrent caller, cache stranded mid-batch). Above
// resubscribeMaxAttempts the library drops the entry rather than retrying
// forever.
type pendingNotification struct {
	Config              NotificationConfig
	resubscribeAttempts int
}

// resubscribeMaxAttempts caps Skipped-config retries across reconnect cycles
// to prevent infinite churn on persistently-flapping symbols. After the cap
// the config is dropped with a Warn log; the user must re-subscribe via
// AddSymbolNotification to re-establish.
const resubscribeMaxAttempts = 3

// newestOpenSubscribe returns the start time of the most recently opened
// subscribe that has not closed yet. open is false when none are in flight.
func (m *notificationManager) newestOpenSubscribe() (int64, bool) {
	m.openMu.Lock()
	defer m.openMu.Unlock()
	newest := int64(0)
	for _, start := range m.openSubscribes {
		if start > newest {
			newest = start
		}
	}
	return newest, newest != 0
}
