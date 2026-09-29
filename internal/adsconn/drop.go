package adsconn

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"syscall"
	"time"

	"github.com/siyka-au/go-ads/v3/ams"
)

// resetAfterConnectHint explains a reset landing right after a successful connect.
// Shared by the log line and the returned error so they cannot drift, and because
// a consumer that surfaces only the error would otherwise see "client transport
// closed" and nothing else.
//
// All five causes stay, in the order worth checking: naming only the route one has
// sent people after a mistyped NetID, and eviction by another client on this IP
// (Beckhoff #49) looks identical on the wire to a missing route.
func ResetAfterConnectHint(source, target ams.Address) string {
	return fmt.Sprintf("a reset right after TCP connect means one of: "+
		"no route is registered on the PLC for our NetID (%s), "+
		"the target NetID (%s) does not exist on this PLC, "+
		"the route credentials were rejected, "+
		"AMS port %d addresses no running runtime (expect 851 on TwinCAT 3, 801 on TwinCAT 2), "+
		"or another client on this host IP took the router's single per-host TCP slot and evicted us",
		source.NetID.String(), target.NetID.String(), target.Port)
}

// framesSeen reports how many AMS frames this client has decoded across both
// socket directions.
func (c *Conn) FramesSeen() uint64 {
	return c.framesPrimary.Load() + c.framesPeer.Load()
}

// wasEstablished reports whether this client ever carried an AMS frame, which is
// what separates a drop worth diagnosing as a route problem from one worth
// diagnosing as a lost connection.
//
// Note what it does NOT mean: a frame carrying an AMS ErrorCode (a router
// rejection, a port with no runtime behind it) counts. "The router talked to us"
// is the claim, not "the route worked".
func (c *Conn) Established() bool { return c.FramesSeen() > 0 }

// uptimeAttr renders how long this client's connection had been up, for a drop
// log line. Returns a nil-valued attr for a zero dialedAt so the paths that never
// set it (raw Dial before its DialTimeout, test-built literals) print nothing
// instead of a duration measured from the zero time.
func (c *Conn) uptimeAttr() slog.Attr {
	if c.dialedAt.IsZero() {
		return slog.Attr{}
	}
	return slog.Duration("uptime", time.Since(c.dialedAt))
}

// localPort reports the local TCP port of the primary connection, or 0 when
// there is none.
//
// Read it BEFORE the socket is closed: LocalAddr on a closed connection is not
// reliable, and the port is the field every drop investigation needed to
// correlate the event against a packet capture.
func (c *Conn) LocalPort() int {
	c.tx.connMu.Lock()
	conn := c.tx.connection
	c.tx.connMu.Unlock()
	if conn == nil {
		return 0
	}
	addr, ok := conn.LocalAddr().(*net.TCPAddr)
	if !ok {
		return 0
	}
	return addr.Port
}

// logDropVerdict reports a primary-transport drop, split by whether the connection
// ever carried a frame: EOF/ECONNRESET look identical whether the socket is 20ms
// or 20h old, and one shared route hint cost a field investigation hours. Both
// branches keep "transport down" and go through transportFaultLevel, so an
// expected RST during a cold-start probe is not an ERROR; tests pin both.
func (c *Conn) logDropVerdict(err error) {
	attrs := []any{
		"error", err,
		"localPort", c.LocalPort(),
		"framesPrimary", c.framesPrimary.Load(),
		"framesPeer", c.framesPeer.Load(),
	}
	if up := c.uptimeAttr(); up.Key != "" {
		attrs = append(attrs, up)
	}
	if c.Established() {
		c.logger.Log(c.ctx, c.transportFaultLevel(),
			"PLC dropped an established connection, transport down",
			append(attrs,
				"hint", EstablishedDropHint(),
				"sourceNetID", c.tx.Source().NetID.String(),
				"targetNetID", c.target.NetID.String(),
				"targetPort", c.target.Port)...)
		return
	}
	if isLikelyMissingRoute(err) {
		c.logger.Log(c.ctx, c.transportFaultLevel(), "PLC closed connection, transport down",
			append(attrs,
				"hint", ResetAfterConnectHint(c.tx.Source(), c.target),
				"sourceNetID", c.tx.Source().NetID.String(),
				"targetNetID", c.target.NetID.String(),
				"targetPort", c.target.Port)...)
		return
	}
	c.logger.Log(c.ctx, c.transportFaultLevel(), "listen read error, transport down", attrs...)
}

// establishedDropHint explains a reset that arrives on a connection which had
// been working. The route is demonstrably not the problem — it was being served
// one frame ago — so the causes worth naming are the ones that end a healthy
// connection.
func EstablishedDropHint() string {
	return "this connection had been carrying AMS frames, so the route was being served: " +
		"look at another client on this host IP taking the router's single per-host TCP slot, " +
		"a PLC runtime restart or CONFIG toggle, or the network path (a VPN or subnet router in between)"
}

// isLikelyMissingRoute returns true if err indicates a likely missing-AMS-route
// condition (PLC closed the TCP connection because no route exists for our
// NetID). Detects wrapped io.EOF and ECONNRESET via the standard
// errors.Is/As mechanism. Used by listen to add a hint to the
// "transport down" log line.
func isLikelyMissingRoute(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, io.EOF) {
		return true
	}
	var netErr *net.OpError
	if errors.As(err, &netErr) {
		if errors.Is(netErr.Err, syscall.ECONNRESET) {
			return true
		}
	}
	return errors.Is(err, syscall.ECONNRESET)
}

// beginHandshake marks a route probe or registration as in flight, during which a
// dropped connection and an unanswered request are expected: the cold start is
// probe -> rejected -> register -> redial -> probe. Logging those at ERROR
// misreports a connect that is progressing, and holds downstream log-based health
// checks in a starting state. Errors still reach the caller unchanged.
func (c *Conn) BeginHandshake() {
	c.handshaking.Add(1)
}

// endHandshake closes a region opened by beginHandshake. Pairs must match; a
// count that never returns to zero would silence real faults for the life of
// the client, so callers defer this immediately.
func (c *Conn) EndHandshake() {
	if c.handshaking.Add(-1) >= 0 {
		return
	}
	// Unbalanced release: a negative count reads as "not handshaking" only by
	// accident, and would then need two begins to demote again. Clamp with a CAS
	// rather than a Store so a region opened AFTER this decision is not erased —
	// note the narrower claim: a begin that raced the decrement itself is already
	// folded into it by the Add, and this cannot recover that. The branch is only
	// reachable via a begin/end imbalance, which is a programming error, so say so
	// once instead of silently repairing it forever.
	c.logger.Warn("endHandshake without a matching beginHandshake; clamping",
		"count", c.handshaking.Load())
	for {
		n := c.handshaking.Load()
		if n >= 0 {
			return
		}
		if c.handshaking.CompareAndSwap(n, 0) {
			return
		}
	}
}

// transportFaultLevel returns the level for a transport fault: Debug during a
// handshake, Error otherwise. Use it for every TRANSPORT fault, all of which are
// expected states of the probe -> register -> redial cold start. Do NOT use it for
// protocol or programming faults -- a handshake never produces those, and demoting
// them would hide corruption.
func (c *Conn) transportFaultLevel() slog.Level {
	if c.handshaking.Load() > 0 {
		return slog.LevelDebug
	}
	return slog.LevelError
}

// OnDropArmed reports whether a drop callback is installed. Session disarms it
// around route activation, where a drop is expected.
func (c *Conn) OnDropArmed() bool {
	c.ondropMu.RLock()
	defer c.ondropMu.RUnlock()
	return c.ondrop != nil
}

// Handshaking reports whether a BeginHandshake region is open.
func (c *Conn) Handshaking() bool { return c.handshaking.Load() > 0 }
