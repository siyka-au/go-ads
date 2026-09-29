// Package router talks to a TwinCAT AMS router over its UDP service port
// (48899): Identify asks a device for its NetID and TwinCAT version, and
// AddRoute registers a route so the device accepts ADS connections from this
// host.
//
// Neither needs an ADS connection. Identify works before any route exists, so
// it can bootstrap one.
package router

import (
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"strconv"
)

// DefaultPort is the AMS router's UDP service port.
const DefaultPort = 48899

// Option configures Identify and AddRoute.
type Option func(*config)

type config struct {
	logger  *slog.Logger
	localIP net.IP
	port    int
}

// WithLogger sets the logger. The default is slog.Default().
func WithLogger(logger *slog.Logger) Option {
	return func(c *config) {
		if logger != nil {
			c.logger = logger
		}
	}
}

// WithLocalIP sends from the given local address instead of letting the OS pick
// the interface. It matters on a multi-homed host: TwinCAT 3 records a route
// against the UDP source address, so the request must leave on the interface
// the ADS traffic will use.
func WithLocalIP(ip netip.Addr) Option {
	return func(c *config) {
		if ip.IsValid() {
			c.localIP = net.IP(ip.AsSlice())
		}
	}
}

// WithPort sends to the given UDP port instead of DefaultPort, for a device
// behind a NAT that forwards the router service elsewhere. A port in the host
// string does the same; setting both is an error.
func WithPort(port int) Option {
	return func(c *config) { c.port = port }
}

// resolve applies opts and splits a "host:port" host.
func resolve(host string, opts []Option) (string, config, error) {
	c := config{logger: slog.Default()}
	for _, o := range opts {
		o(&c)
	}
	h, port, err := splitHostPort(host)
	if err != nil {
		return "", c, err
	}
	switch {
	case port != 0 && c.port != 0 && port != c.port:
		return "", c, fmt.Errorf("host %q names router port %d but WithPort(%d) was also given", host, port, c.port)
	case port != 0:
		c.port = port
	case c.port == 0:
		c.port = DefaultPort
	}
	if c.port <= 0 || c.port > 65535 {
		return "", c, fmt.Errorf("router port %d is out of range", c.port)
	}
	if h == "" {
		return "", c, fmt.Errorf("host must be set")
	}
	return h, c, nil
}

// splitHostPort accepts a bare host or host:port. It returns port 0 for a bare
// host.
//
// A present-but-unusable port is an error, not a fallback: folding it back into
// the hostname turned a typo into a resolution failure for an address nobody
// typed, and defaulting to 48899 can reach a different device entirely.
func splitHostPort(host string) (string, int, error) {
	h, portStr, err := net.SplitHostPort(host)
	if err != nil {
		// No port in there at all (a bare host, or a bare IPv6 literal). A
		// genuinely malformed host is diagnosed by the resolver, which can say
		// more about it than this can.
		return host, 0, nil
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return "", 0, fmt.Errorf("host %q: router port %q is not a number", host, portStr)
	}
	if port <= 0 || port > 65535 {
		return "", 0, fmt.Errorf("host %q: router port %d is out of range", host, port)
	}
	return h, port, nil
}
