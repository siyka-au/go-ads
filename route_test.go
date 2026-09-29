package ads

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/siyka-au/go-ads/v3/ams"
	"github.com/siyka-au/go-ads/v3/internal/fakeplc"
)

// TestAddRoute_ReadsTheSourceNetIDUnderItsLock: Session.AddRoute is exported and
// callable from any goroutine, and it read sess.source with no lock while
// localHandshake writes that field under tx.connMu from the reconnect goroutine.
//
// The read that matters is the one INSIDE the spawned goroutine: the code's own
// comment says that goroutine outlives the AddRoute call when the caller's context
// is cancelled, so the racing read is on a detached goroutine with no join. A torn
// NetID there registers a route for a spliced identity — the PLC answers nothing,
// the session burns its whole unserved/cooldown budget, and a junk entry is left in
// the device's route table, which is the failure shape that took two TC3 devices
// mute (see route.go on duplicate entries for one NetID).
//
// The assertion is the race detector's; this test earns its place in the -race job.
// It is deterministic in the accesses it makes, not in whether the detector sees
// them, so the value assertion below stands in when it does not: every registration
// must carry one of the two whole NetIDs, never a mixture of both.
func TestAddRoute_ReadsTheSourceNetIDUnderItsLock(t *testing.T) {
	router := fakeplc.NewRouter(t)

	sess := newDialableTestSession(t, "127.0.0.1", 1, 1)
	sess.routerPort = router.Port
	sess.route = &routeManager{name: "go-ads-test", username: "Administrator", password: secret("1")}
	// No callbackIP: that is what makes AddRoute derive the host IP from the NetID,
	// which is the second bare read.
	sess.callbackIP = ""

	first := [6]byte{10, 0, 0, 1, 1, 1}
	second := [6]byte{192, 168, 3, 52, 1, 1}
	sess.tx.SetSource(ams.Address{NetID: first, Port: 10500})

	// The writer, doing exactly what localHandshake does: replace the whole address
	// under tx.connMu. Every byte differs between the two NetIDs, so a torn read is
	// visible in the value and not only to the detector.
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		for i := 0; i < 500; i++ {
			next := first
			if i%2 == 0 {
				next = second
			}
			sess.tx.SetSource(ams.Address{NetID: next, Port: 10500})
		}
	}()

	for i := 0; i < 40; i++ {
		if err := sess.AddRoute(context.Background(), sess.route.name, sess.route.username, string(sess.route.password)); err != nil {
			t.Fatalf("AddRoute against the stub router: %v", err)
		}
	}
	<-writerDone

	if got := router.Registrations(); got < 40 {
		t.Errorf("registrations = %d, want at least 40: the test did not exercise the read it is about", got)
	}
	for _, netID := range router.RegisteredNetIDs() {
		if netID != first && netID != second {
			t.Errorf("a registration carried NetID %v, which is neither of the two written: the read was torn", netID)
		}
	}
}

// A router that answers nothing must be reported as retryable, not read as a
// missing route. Measured at ~8s of silence on a TC3 4024 after the client
// restarted; the old code spent that window registering a route that existed.
func TestAwaitRouterAwake_DeafRouterIsRetryable(t *testing.T) {
	deaf := fakeplc.NewRouter(t) // no identity: never answers identify
	defer func(d time.Duration) { routerDeafGrace = d }(routerDeafGrace)
	routerDeafGrace = 2 * time.Second

	sess := &Session{
		ip:         deaf.Host,
		routerPort: deaf.Port,
		logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	start := time.Now()
	err := sess.awaitRouterAwake(context.Background())
	if !errors.Is(err, ErrRouterUnresponsive) {
		t.Fatalf("got %v, want ErrRouterUnresponsive — a deaf router reads as a missing route again", err)
	}
	if waited := time.Since(start); waited < routerDeafGrace {
		t.Errorf("gave up after %v, before the %v grace", waited, routerDeafGrace)
	}
}

// And it must not wait when the router is answering, which is every other call.
func TestAwaitRouterAwake_ReturnsWhileTheRouterAnswers(t *testing.T) {
	awake := fakeplc.NewRouter(t)
	awake.SetIdentity(fakeplc.Identity{NetID: [6]byte{5, 1, 2, 3, 1, 1}, Port: 10000})
	defer func(d time.Duration) { routerDeafGrace = d }(routerDeafGrace)
	routerDeafGrace = 5 * time.Second

	sess := &Session{
		ip:         awake.Host,
		routerPort: awake.Port,
		logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	start := time.Now()
	if err := sess.awaitRouterAwake(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if waited := time.Since(start); waited > routerAwakePoll {
		t.Errorf("waited %v on a router that was answering", waited)
	}
}
