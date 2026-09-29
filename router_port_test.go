package ads

import (
	"context"
	"testing"

	"github.com/siyka-au/go-ads/v3/ams"
	"github.com/siyka-au/go-ads/v3/internal/fakeplc"
	"github.com/siyka-au/go-ads/v3/router"
)

// router_port_test.go — Endpoint.RouterPort.
//
// A PLC behind NAT is reached on forwarded ports, and NAT maps one external
// port per internal port: the forwarded UDP port is a different number than the
// forwarded TCP port and cannot be derived from it. So the router port has to be
// independently settable, and the UDP calls (route registration, identify) have
// to use it rather than the protocol constant.

// TestNewSession_RouterPortDefaultsToProtocolPort: leaving it unset keeps the
// TwinCAT default, so nothing changes for a directly-reachable PLC.
func TestNewSession_RouterPortDefaultsToProtocolPort(t *testing.T) {
	sess, err := NewSession(context.Background(), Endpoint{
		Host:   "127.0.0.1",
		Target: ams.Address{NetID: [6]byte{5, 1, 2, 3, 1, 1}, Port: 851},
	}, WithTargetCheck(TargetCheckOff))
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	t.Cleanup(func() { sess.Close() })
	if got := sess.effectiveRouterPort(); got != router.DefaultPort {
		t.Errorf("router port = %d, want the protocol default %d", got, router.DefaultPort)
	}
	if sess.port != 48898 {
		t.Errorf("TCP port = %d, want the protocol default 48898", sess.port)
	}
}

// TestNewSession_RouterPortIndependentOfTCPPort is the NAT shape: two unrelated
// forwarded numbers, neither derived from the other.
func TestNewSession_RouterPortIndependentOfTCPPort(t *testing.T) {
	sess, err := NewSession(context.Background(), Endpoint{
		Host:       "127.0.0.1",
		Port:       5534, // external TCP -> 48898 on the PLC
		RouterPort: 6499, // external UDP -> 48899 on the PLC
		Target:     ams.Address{NetID: [6]byte{5, 1, 2, 3, 1, 1}, Port: 851},
	}, WithTargetCheck(TargetCheckOff))
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	t.Cleanup(func() { sess.Close() })
	if sess.port != 5534 {
		t.Errorf("TCP port = %d, want 5534", sess.port)
	}
	if got := sess.effectiveRouterPort(); got != 6499 {
		t.Errorf("router port = %d, want 6499", got)
	}
}

// TestSessionUsesRouterPortForIdentify proves the plumbing end to end for the
// UDP half: a responder on an arbitrary port is found only if the session
// actually probes RouterPort rather than the constant.
func TestSessionUsesRouterPortForIdentify(t *testing.T) {
	r := fakeplc.NewRouter(t)
	if r.Port == router.DefaultPort {
		// On the default port the test would pass even if the session ignored
		// RouterPort entirely.
		t.Fatal("the fake router landed on the protocol default port")
	}
	r.SetIdentity(fakeplc.Identity{
		NetID: [6]byte{5, 9, 8, 7, 1, 1}, Port: 10000,
		Tags: []fakeplc.Tag{fakeplc.NameTag("NAT-PLC"), fakeplc.VersionTag(3, 1, 4024)},
	})

	// No target AMS at all, so NewSession must discover it — over RouterPort.
	sess, err := NewSession(context.Background(), Endpoint{
		Host:       r.Host,
		Port:       5534, // nothing listens here; discovery is UDP-only
		RouterPort: r.Port,
	})
	if err != nil {
		t.Fatalf("NewSession with discovery over a forwarded router port: %v", err)
	}
	t.Cleanup(func() { sess.Close() })

	if got := sess.target.NetID.String(); got != "5.9.8.7.1.1" {
		t.Errorf("discovered NetID = %s, want 5.9.8.7.1.1", got)
	}
	if sess.target.Port != 851 {
		t.Errorf("discovered runtime port = %d, want 851 for the reported TwinCAT 3", sess.target.Port)
	}
}
