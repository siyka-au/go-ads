package ads

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/siyka-au/go-ads/v3/internal/testlog"

	"github.com/siyka-au/go-ads/v3/ams"
	"github.com/siyka-au/go-ads/v3/internal/fakeplc"
	"github.com/siyka-au/go-ads/v3/router"
)

// newTargetCheckSession builds the minimum Session applyTargetCheck touches,
// with a capturing logger.
func newTargetCheckSession(t *testing.T, netID string, check TargetCheck) (*Session, *testlog.Handler) {
	t.Helper()
	target, err := ams.NewAddress(netID, 851)
	if err != nil {
		t.Fatalf("target %q: %v", netID, err)
	}
	logs := &testlog.Handler{}
	return &Session{
		ip:          "192.168.3.118",
		target:      target,
		targetCheck: check,
		logger:      slog.New(logs),
	}, logs
}

func identityOf(t *testing.T, netID string) router.Identity {
	t.Helper()
	ams, err := ams.NewAddress(netID, 10000)
	if err != nil {
		t.Fatalf("identity %q: %v", netID, err)
	}
	return router.Identity{Address: ams, HostName: "CX-4285CB", Major: 3, Minor: 1, Build: 4024}
}

// TestApplyTargetCheck_Match: the device agrees, so nothing is reported to the
// operator beyond Debug.
func TestApplyTargetCheck_Match(t *testing.T) {
	sess, logs := newTargetCheckSession(t, "5.66.133.203.1.1", TargetCheckWarn)
	if err := sess.applyTargetCheck(identityOf(t, "5.66.133.203.1.1")); err != nil {
		t.Fatalf("applyTargetCheck on a matching NetID: %v", err)
	}
	if rec := logs.FindByMessage("differs from"); rec != nil {
		t.Errorf("unexpected mismatch log on a match: %q", rec.Message)
	}
	if rec := logs.FindByMessage("confirmed by device"); rec == nil {
		t.Error("no confirmation logged")
	} else if rec.Level != slog.LevelDebug {
		t.Errorf("confirmation logged at %v, want Debug (a match is not news)", rec.Level)
	}
}

// TestApplyTargetCheck_MismatchWarns: the default must not refuse, because the
// same signature is produced by a legitimate routed setup.
func TestApplyTargetCheck_MismatchWarns(t *testing.T) {
	sess, logs := newTargetCheckSession(t, "5.1.2.3.1.1", TargetCheckWarn)
	if err := sess.applyTargetCheck(identityOf(t, "5.66.133.203.1.1")); err != nil {
		t.Fatalf("TargetCheckWarn returned an error: %v", err)
	}
	rec := logs.FindByMessage("differs from")
	if rec == nil {
		t.Fatal("mismatch not logged")
	}
	if rec.Level != slog.LevelWarn {
		t.Errorf("mismatch logged at %v, want Warn", rec.Level)
	}
	// Downstream log-based health checks fail on WARN lines containing these
	// substrings, and a warning that trips them would be worse than useless.
	for _, banned := range []string{"failed to", "unable to", "connection lost"} {
		if strings.Contains(rec.Message, banned) {
			t.Errorf("warn message contains %q, which trips downstream log checks: %q", banned, rec.Message)
		}
	}
}

// TestApplyTargetCheck_MismatchErrors: strict mode refuses, and the error has to
// carry both NetIDs and the responder's identity — the whole point is that the
// reader can tell which end is wrong without further digging.
func TestApplyTargetCheck_MismatchErrors(t *testing.T) {
	sess, _ := newTargetCheckSession(t, "5.1.2.3.1.1", TargetCheckError)
	err := sess.applyTargetCheck(identityOf(t, "5.66.133.203.1.1"))
	if err == nil {
		t.Fatal("TargetCheckError accepted a mismatch")
	}
	for _, want := range []string{"5.1.2.3.1.1", "5.66.133.203.1.1", "192.168.3.118", "CX-4285CB", "3.1.4024"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q: %v", want, err)
		}
	}
}

// TestApplyTargetCheck_ErrorModeAcceptsMatch guards against strict mode being
// so strict it rejects a correct address.
func TestApplyTargetCheck_ErrorModeAcceptsMatch(t *testing.T) {
	sess, _ := newTargetCheckSession(t, "5.66.133.203.1.1", TargetCheckError)
	if err := sess.applyTargetCheck(identityOf(t, "5.66.133.203.1.1")); err != nil {
		t.Errorf("TargetCheckError rejected a matching NetID: %v", err)
	}
}

// TestWithTargetCheck: the option overrides the default, and the zero value is
// ignored so it cannot silently disable the check.
func TestWithTargetCheck(t *testing.T) {
	tests := []struct {
		name string
		set  TargetCheck
		want TargetCheck
	}{
		{name: "error mode", set: TargetCheckError, want: TargetCheckError},
		{name: "off", set: TargetCheckOff, want: TargetCheckOff},
		{name: "zero value keeps the default", set: 0, want: TargetCheckWarn},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sess := &Session{targetCheck: TargetCheckWarn}
			WithTargetCheck(tt.set)(sess)
			if sess.targetCheck != tt.want {
				t.Errorf("targetCheck = %d, want %d", sess.targetCheck, tt.want)
			}
		})
	}
}

// TestNewSession_LocalModeSkipsDiscovery: local mode targets the in-process
// runtime and Connect overwrites NetID and IP with loopback regardless, so
// probing the caller-supplied address at construction is pointless — and its
// failure must not sink the session. The address here is chosen to be
// unroutable, so a probe would have to time out.
func TestNewSession_LocalModeSkipsDiscovery(t *testing.T) {
	start := time.Now()
	sess, err := NewSession(context.Background(),
		Endpoint{Host: "192.0.2.1"}, // RFC 5737 TEST-NET-1: guaranteed unroutable
		WithLocalMode())
	if err != nil {
		t.Fatalf("NewSession in local mode with no target AMS: %v", err)
	}
	t.Cleanup(func() { sess.Close() })

	// identifyTimeout is 3s; anything near it means a probe was attempted.
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("NewSession took %v — local mode probed the network", elapsed)
	}
	if sess.target.NetID != [6]byte{} {
		t.Errorf("target NetID = %s, want zero (Connect assigns the loopback NetID)", sess.target.NetID.String())
	}
}

// TestDiscoverTarget_RefusesPortWithoutVersion: the runtime port is a
// per-major-version convention, so with no version reported there is nothing to
// apply. Guessing would plant a port that may address no runtime — the exact
// silent failure discovery exists to prevent.
func TestDiscoverTarget_RefusesPortWithoutVersion(t *testing.T) {
	// Responder that reports a NetID and host name but no version tag.
	r := fakeplc.NewRouter(t)
	r.SetIdentity(fakeplc.Identity{
		NetID: [6]byte{5, 7, 7, 7, 1, 1}, Port: 10000,
		Tags: []fakeplc.Tag{fakeplc.NameTag("NO-VERSION-CX")},
	})

	id, err := router.Identify(t.Context(), r.Addr())
	if err != nil {
		t.Fatalf("identify: %v", err)
	}
	if id.HasVersion() {
		t.Fatalf("setup: responder reported a version (%s)", id.Version())
	}
	// RuntimePort keeps its documented convention; discoverTarget must not lean
	// on it when there is no version behind it.
	if got := id.RuntimePort(); got != 851 {
		t.Errorf("RuntimePort() = %d, want the documented 851 default", got)
	}

	sess := &Session{ip: r.Host, logger: slog.Default()}
	err = sess.applyDiscoveredIdentity(id)
	if err == nil {
		t.Fatal("discovery accepted a device that reported no version and left the port to be guessed")
	}
	for _, want := range []string{"no TwinCAT version", "801", "851"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q: %v", want, err)
		}
	}
}
