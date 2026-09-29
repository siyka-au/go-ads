package adsconn

import (
	"net"
)

// AcceptPeerConn adopts a connection the PLC opened TO US, reading its frames into
// the same response mux. Some devices treat a registered route as a peer router:
// they process our requests on our connection but answer over one they open back
// to us. Frames carry their own invokeID, so responses match regardless of which
// socket they arrive on.
func (c *Conn) AcceptPeerConn(conn net.Conn) {
	// Refuse once the adopted connections were dropped for a teardown: the PLC
	// re-dials after every drop, which is exactly when teardown runs, so adopting
	// here leaks an fd, blocks tearDownAndReset's wait in io.ReadFull for ever, and
	// Adds to a WaitGroup whose Wait may already be running at zero. The Add stays
	// in the same critical section as the flag check.
	c.peerMu.Lock()
	if c.peerClosed || (c.ctx != nil && c.ctx.Err() != nil) {
		c.peerMu.Unlock()
		c.logger.Debug("refusing an inbound PLC connection: this client is being torn down (the PLC will dial again)",
			"remote", conn.RemoteAddr().String())
		_ = conn.Close()
		return
	}
	c.peerConns = append(c.peerConns, conn)
	c.waitGroup.Add(1)
	c.peerMu.Unlock()

	c.logger.Info("adopted an inbound connection from the PLC (peer-route behaviour)",
		"remote", conn.RemoteAddr().String())
	go func() {
		defer c.waitGroup.Done()
		// This reader owns the socket. readFrames returns on ctx.Done() and on any
		// read error without touching conn, so without this every connection the
		// PLC ever opened would cost an fd for the life of the Client.
		defer func() {
			_ = conn.Close()
			c.forgetPeerConn(conn)
		}()
		c.readFrames(conn, false)
	}()
}

// forgetPeerConn drops one adopted connection from the list. Without it the slice
// only ever grows: a device that re-dials on each of its own drops would
// accumulate an entry per connection for the life of the Client.
func (c *Conn) forgetPeerConn(conn net.Conn) {
	c.peerMu.Lock()
	defer c.peerMu.Unlock()
	for i, existing := range c.peerConns {
		if existing == conn {
			c.peerConns = append(c.peerConns[:i], c.peerConns[i+1:]...)
			return
		}
	}
}

// closePeerConns drops every adopted inbound connection and refuses further ones.
// Called from Close and from tearDownAndReset so a reader blocked on one cannot
// hold up the wait for this Client's workers.
func (c *Conn) closePeerConns() {
	c.peerMu.Lock()
	conns := c.peerConns
	c.peerConns = nil
	c.peerClosed = true
	c.peerMu.Unlock()
	for _, conn := range conns {
		_ = conn.Close()
	}
}
