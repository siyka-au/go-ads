//go:build integration

package integration

import (
	"context"
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/siyka-au/go-ads/v3"

	"github.com/siyka-au/go-ads/v3/ams"
	"github.com/siyka-au/go-ads/v3/internal/testproxy"
)

// connDefaults holds default values for integration test connections.
type connDefaults struct {
	ip        string // default PLC IP
	targetAMS string // default AMS NetID
	routeName string // route name registered on PLC
}

// sessionConfig resolves the environment into a PLC host, target and options.
// hostIP overrides ADS_HOST_IP when non-empty.
func sessionConfig(t *testing.T, d connDefaults, hostIP string) (host string, target ams.Address, opts []ads.Option) {
	t.Helper()

	host = getEnvOrDefault("ADS_PLC_IP", d.ip)
	targetAMS := getEnvOrDefault("ADS_TARGET_AMS", d.targetAMS)
	targetPortStr := getEnvOrDefault("ADS_TARGET_PORT", "851")
	targetPort, err := strconv.Atoi(targetPortStr)
	if err != nil {
		t.Fatalf("invalid ADS_TARGET_PORT %q: %v", targetPortStr, err)
	}
	localAMS := getEnvOrDefault("ADS_LOCAL_AMS", "auto")

	if hostIP == "" {
		hostIP = os.Getenv("ADS_HOST_IP")
	}
	if hostIP != "" {
		opts = append(opts, ads.WithHostIP(hostIP))
	}
	// ADS_LOCAL_BIND_IP forces the outbound source IP. Needed on a multi-homed
	// host: with wifi and ethernet on the same subnet the OS picks one by route
	// metric, so testing the other NIC deliberately means binding to it. Leaving
	// it unset keeps OS-default routing, which is what production usually wants.
	if bindIP := os.Getenv("ADS_LOCAL_BIND_IP"); bindIP != "" {
		opts = append(opts, ads.WithLocalBindIP(bindIP))
	}
	// ADS_TEST_DEBUG=1 turns on Debug logging for the session under test. The
	// reconnect path logs its decisions at Debug, and without them a stall inside a
	// step is invisible — which cost real time diagnosing one.
	if os.Getenv("ADS_TEST_DEBUG") != "" {
		opts = append(opts, ads.WithLogger(slog.New(slog.NewTextHandler(os.Stderr,
			&slog.HandlerOptions{Level: slog.LevelDebug}))))
	}
	routeUser := os.Getenv("ADS_ROUTE_USER")
	routePass := os.Getenv("ADS_ROUTE_PASS")
	// ADS_SKIP_ROUTE_REGISTER=true → don't pass WithRoute → ensureRoute no-op.
	// Requires route to be pre-registered on PLC. Used for multi-session tests
	// where AddRoute UDP would terminate sibling TCP connections.
	skipRouteReg := strings.EqualFold(os.Getenv("ADS_SKIP_ROUTE_REGISTER"), "true")
	// Resolve effective route name:
	//   1. ADS_ROUTE_NAME explicit override → use as-is
	//   2. a host IP is known → derive "go-ads-<ip>" so each source IP creates a
	//      distinct PLC route entry. Avoids the duplicate-name collision
	//      observed when the same host's IP changes (wifi↔ethernet, DHCP
	//      lease) and a stale entry blocks new registrations.
	//   3. Fall back to the connDefaults.routeName supplied by the caller.
	routeName := d.routeName
	if envName := os.Getenv("ADS_ROUTE_NAME"); envName != "" {
		routeName = envName
	} else if hostIP != "" {
		routeName = "go-ads-" + strings.ReplaceAll(hostIP, ".", "-")
	}
	if !skipRouteReg && routeUser != "" && routePass != "" {
		opts = append(opts, ads.WithRoute(routeName, routeUser, routePass))
	}

	target, err = ams.NewAddress(targetAMS, ams.Port(targetPort))
	if err != nil {
		t.Fatalf("invalid target AMS: %v", err)
	}
	opts = append(opts, ads.WithRequestTimeout(5*time.Second), ads.WithLocalAddress(ams.Address{Port: 10500}))
	if localAMS != "auto" && localAMS != "" {
		local, err := ams.NewAddress(localAMS, 10500)
		if err != nil {
			t.Fatalf("invalid local AMS: %v", err)
		}
		opts = append(opts, ads.WithLocalAddress(local))
	}
	return host, target, opts
}

func connectSession(t *testing.T, ep ads.Endpoint, opts []ads.Option) *ads.Session {
	t.Helper()
	conn, err := ads.NewSession(context.Background(), ep, opts...)
	if err != nil {
		t.Fatalf("NewSession failed: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	if err := conn.Connect(context.Background()); err != nil {
		t.Fatalf("Connect failed: %v", err)
	}
	return conn
}

// setupConnectionWithDefaults creates a PLC connection using env vars with
// provided fallbacks. extra Options are appended last, so a test can
// override anything derived from the environment (e.g. WithLogger).
func setupConnectionWithDefaults(t *testing.T, d connDefaults, extra ...ads.Option) *ads.Session {
	t.Helper()
	host, target, opts := sessionConfig(t, d, "")
	return connectSession(t, ads.Endpoint{Host: host, Port: 48898, Target: target}, append(opts, extra...))
}

// setupProxiedConnection is setupConnectionWithDefaults with the session's TCP
// connection and its UDP router traffic both passing through a local proxy, so
// a test can drop the link from outside. The route still names this host's real
// address: the TCP connection now starts from loopback, so the callback address
// has to be given rather than derived.
func setupProxiedConnection(t *testing.T, d connDefaults, extra ...ads.Option) (*ads.Session, *testproxy.Proxy) {
	t.Helper()
	plc := getEnvOrDefault("ADS_PLC_IP", d.ip)
	hostIP := os.Getenv("ADS_HOST_IP")
	if hostIP == "" {
		hostIP = testproxy.LocalIPFor(t, plc)
	}
	host, target, opts := sessionConfig(t, d, hostIP)
	p := testproxy.Start(t, net.JoinHostPort(host, "48898"))
	relay := testproxy.StartUDPRelay(t, net.JoinHostPort(host, "48899"))
	ep := ads.Endpoint{Host: p.Host(), Port: p.Port(), RouterPort: relay.Port(), Target: target}
	return connectSession(t, ep, append(opts, extra...)), p
}

// dropLink breaks the session's connection from outside and lets it come back,
// the way a switch port bouncing would.
func dropLink(p *testproxy.Proxy) {
	p.Cut()
	p.Restore()
}
