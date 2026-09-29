//go:build integration

package ads

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/siyka-au/go-ads/v3/ams"
	"github.com/siyka-au/go-ads/v3/router"
)

// TestIntegrationIdentifyRemote checks discovery against the configured target:
// whatever the device reports must match the NetID the env file was written
// with. That makes this test double as a check that the env file has not gone
// stale, which is a failure mode that otherwise looks like a dead PLC.
func TestIntegrationIdentifyRemote(t *testing.T) {
	host := getEnvOrDefault("ADS_PLC_IP", "192.168.3.224")
	want := os.Getenv("ADS_TARGET_AMS")

	id, err := router.Identify(context.Background(), host)
	if err != nil {
		t.Fatalf("IdentifyRemote(%s): %v", host, err)
	}
	t.Logf("%s: netID=%s host=%q twinCAT=%s runtimePort=%d",
		host, id.Address.NetID.String(), id.HostName, id.Version(), id.RuntimePort())

	if want != "" && id.Address.NetID.String() != want {
		t.Errorf("discovered NetID = %s, want %s (ADS_TARGET_AMS) — the env file or the device changed",
			id.Address.NetID.String(), want)
	}
	if id.Address.Port != 10000 {
		t.Errorf("reported port = %d, want 10000 (the router's own port)", id.Address.Port)
	}
	if id.HostName == "" {
		t.Error("HostName empty; every tested TwinCAT reports one")
	}
	if id.Major == 0 {
		t.Error("TwinCAT major version not reported")
	}
	// The port convention has to agree with how this PLC is actually addressed.
	if envPort := os.Getenv("ADS_TARGET_PORT"); envPort == "801" && id.RuntimePort() != 801 {
		t.Errorf("RuntimePort() = %d on a TwinCAT %s device configured for 801", id.RuntimePort(), id.Version())
	}
}

// TestIntegrationTargetCheckCatchesWrongNetID is the payoff for verification: a
// wrong target NetID normally produces a session that connects and then times
// out on everything, with nothing pointing at the address. Under
// TargetCheckError it fails at construction, naming both NetIDs.
func TestIntegrationTargetCheckCatchesWrongNetID(t *testing.T) {
	host := getEnvOrDefault("ADS_PLC_IP", "192.168.3.224")
	real := os.Getenv("ADS_TARGET_AMS")
	if real == "" {
		t.Skip("ADS_TARGET_AMS not set")
	}
	wrong, err := ams.NewAddress("5.99.99.99.1.1", 851)
	if err != nil {
		t.Fatalf("wrong address: %v", err)
	}

	// NewSession must stay I/O-free for a fully specified target: the check
	// belongs to Connect.
	t.Run("NewSession does not verify", func(t *testing.T) {
		sess, err := NewSession(context.Background(),
			Endpoint{Host: host, Target: wrong},
			WithTargetCheck(TargetCheckError))
		if err != nil {
			t.Fatalf("NewSession must not verify a supplied target: %v", err)
		}
		t.Cleanup(func() { sess.Close() })
		if sess.target.NetID.String() != "5.99.99.99.1.1" {
			t.Errorf("NewSession altered the target: %s", sess.target.NetID.String())
		}
	})

	t.Run("Connect refuses in error mode", func(t *testing.T) {
		sess, err := NewSession(context.Background(),
			Endpoint{Host: host, Target: wrong},
			WithTargetCheck(TargetCheckError))
		if err != nil {
			t.Fatalf("NewSession: %v", err)
		}
		t.Cleanup(func() { sess.Close() })
		// Verification runs before the dial, so this costs one UDP round-trip
		// and leaves no route or connection behind on the PLC.
		err = sess.Connect(context.Background())
		if err == nil {
			t.Fatal("Connect accepted a NetID the device disagrees with")
		}
		for _, want := range []string{"5.99.99.99.1.1", real} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error missing %q: %v", want, err)
			}
		}
		t.Logf("refused as expected: %v", err)
	})

	t.Run("warn mode passes verification", func(t *testing.T) {
		sess, err := NewSession(context.Background(),
			Endpoint{Host: host, Target: wrong},
			WithTargetCheck(TargetCheckWarn))
		if err != nil {
			t.Fatalf("NewSession: %v", err)
		}
		t.Cleanup(func() { sess.Close() })
		// Exercise the check itself rather than a full Connect: warn mode is
		// defined by not blocking, and a genuinely wrong NetID cannot complete
		// a connection anyway.
		if err := sess.verifyTarget(context.Background()); err != nil {
			t.Errorf("warn mode must not fail verification: %v", err)
		}
		if sess.target.NetID.String() != "5.99.99.99.1.1" {
			t.Errorf("verification altered the target: %s", sess.target.NetID.String())
		}
	})

	t.Run("correct NetID passes error mode", func(t *testing.T) {
		right, err := ams.NewAddress(real, 851)
		if err != nil {
			t.Fatalf("real address: %v", err)
		}
		sess, err := NewSession(context.Background(),
			Endpoint{Host: host, Target: right},
			WithTargetCheck(TargetCheckError))
		if err != nil {
			t.Fatalf("NewSession: %v", err)
		}
		t.Cleanup(func() { sess.Close() })
		if err := sess.verifyTarget(context.Background()); err != nil {
			t.Errorf("error mode rejected the correct NetID: %v", err)
		}
	})
}

// TestIntegrationSessionDiscoversTarget leaves the target AMS address out
// entirely and requires NewSession to resolve it and then actually work.
func TestIntegrationSessionDiscoversTarget(t *testing.T) {
	host := getEnvOrDefault("ADS_PLC_IP", "192.168.3.224")
	wantNetID := os.Getenv("ADS_TARGET_AMS")

	var opts []Option
	if hostIP := os.Getenv("ADS_HOST_IP"); hostIP != "" {
		opts = append(opts, WithHostIP(hostIP))
	}
	if user, pass := os.Getenv("ADS_ROUTE_USER"), os.Getenv("ADS_ROUTE_PASS"); user != "" && pass != "" {
		routeName := "go-ads-discover"
		opts = append(opts, WithRoute(routeName, user, pass))
	}
	if localAMS := os.Getenv("ADS_LOCAL_AMS"); localAMS != "" {
		local, err := ams.NewAddress(localAMS, 10500)
		if err != nil {
			t.Fatalf("ADS_LOCAL_AMS: %v", err)
		}
		opts = append(opts, WithLocalAddress(local))
	}

	// No AMS field at all: NetID and port both come from the device.
	sess, err := NewSession(context.Background(), Endpoint{Host: host}, opts...)
	if err != nil {
		t.Fatalf("NewSession without a target AMS address: %v", err)
	}
	t.Cleanup(func() { sess.Close() })

	if wantNetID != "" && sess.target.NetID.String() != wantNetID {
		t.Errorf("resolved NetID = %s, want %s", sess.target.NetID.String(), wantNetID)
	}
	if envPort := os.Getenv("ADS_TARGET_PORT"); envPort != "" {
		t.Logf("resolved port %d, env says %s", sess.target.Port, envPort)
	}

	// Resolution is only worth anything if the session then works.
	if err := sess.Connect(context.Background()); err != nil {
		t.Fatalf("Connect with a discovered target: %v", err)
	}
	version, err := sess.client.Load().GetSymbolVersion(context.Background())
	if err != nil {
		t.Fatalf("GetSymbolVersion over a discovered target: %v", err)
	}
	t.Logf("connected via discovered target %s: symbol version %d", sess.target.String(), version)
}
