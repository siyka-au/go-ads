package testproxy

import (
	"net"
	"sync"
	"testing"
	"time"
)

// UDPRelay forwards datagrams between loopback clients and a target, so a
// session whose Host is the proxy still reaches the PLC's AMS router service.
// The PLC sees them coming from this machine, as it would without the relay.
type UDPRelay struct {
	pc     *net.UDPConn
	target *net.UDPAddr
	done   chan struct{}
	wg     sync.WaitGroup

	mu       sync.Mutex
	upstream map[string]*net.UDPConn // client addr -> socket towards the target
}

// StartUDPRelay listens on loopback and relays to target ("host:port").
func StartUDPRelay(t testing.TB, target string) *UDPRelay {
	t.Helper()
	dst, err := net.ResolveUDPAddr("udp4", target)
	if err != nil {
		t.Fatalf("relay target %q: %v", target, err)
	}
	pc, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("relay listen: %v", err)
	}
	r := &UDPRelay{pc: pc, target: dst, done: make(chan struct{}), upstream: map[string]*net.UDPConn{}}
	r.wg.Add(1)
	go r.serve()
	t.Cleanup(r.Close)
	return r
}

// Port is the relay's local UDP port, for Endpoint.RouterPort.
func (r *UDPRelay) Port() int { return r.pc.LocalAddr().(*net.UDPAddr).Port }

// Close stops the relay.
func (r *UDPRelay) Close() {
	select {
	case <-r.done:
		return
	default:
	}
	close(r.done)
	_ = r.pc.Close()
	r.mu.Lock()
	for _, up := range r.upstream {
		_ = up.Close()
	}
	r.mu.Unlock()
	r.wg.Wait()
}

func (r *UDPRelay) serve() {
	defer r.wg.Done()
	buf := make([]byte, 64*1024)
	for {
		n, from, err := r.pc.ReadFromUDP(buf)
		if err != nil {
			return
		}
		up, err := r.upstreamFor(from)
		if err != nil {
			continue
		}
		_, _ = up.Write(buf[:n])
	}
}

// upstreamFor returns the socket that carries one client's traffic to the
// target, starting its reply pump on first use.
func (r *UDPRelay) upstreamFor(client *net.UDPAddr) (*net.UDPConn, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if up, ok := r.upstream[client.String()]; ok {
		return up, nil
	}
	up, err := net.DialUDP("udp4", nil, r.target)
	if err != nil {
		return nil, err
	}
	r.upstream[client.String()] = up
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		buf := make([]byte, 64*1024)
		for {
			_ = up.SetReadDeadline(time.Now().Add(time.Second))
			n, err := up.Read(buf)
			if err != nil {
				select {
				case <-r.done:
					return
				default:
				}
				if ne, ok := err.(net.Error); ok && ne.Timeout() {
					continue
				}
				return
			}
			_, _ = r.pc.WriteToUDP(buf[:n], client)
		}
	}()
	return up, nil
}

// LocalIPFor returns this machine's IPv4 address on the route towards host,
// which is what a session behind the proxy must tell the PLC to call back.
func LocalIPFor(t testing.TB, host string) string {
	t.Helper()
	c, err := net.Dial("udp4", net.JoinHostPort(host, "48899"))
	if err != nil {
		t.Fatalf("no route towards %s: %v", host, err)
	}
	defer func() { _ = c.Close() }()
	return c.LocalAddr().(*net.UDPAddr).IP.String()
}
