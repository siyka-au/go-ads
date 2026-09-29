package adsconn

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"time"

	"github.com/siyka-au/go-ads/v3/ams"

	"github.com/siyka-au/go-ads/v3/internal/logging"
)

// send is the local-mode handshake primitive. NOT safe for concurrent use —
// it consumes from the shared systemResponse channel. Used by Session for
// the local AMS handshake during Connect/Reconnect.
func (c *Conn) send(data []byte) ([]byte, error) {
	c.tx.currentRequest.Add(1)
	c.tx.chanMu.RLock()
	sendCh := c.tx.sendChannel
	sysCh := c.tx.systemResponse
	c.tx.chanMu.RUnlock()
	dropped := c.dropped
	ctx, cancel := context.WithTimeout(c.ctx, c.requestTimeout)
	defer cancel()
	select {
	case <-ctx.Done():
		return nil, fmt.Errorf("send aborted, context canceled: %w", ctx.Err())
	case <-dropped:
		return nil, ErrTransportClosed
	case sendCh <- data:
	}
	select {
	case <-ctx.Done():
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			err := fmt.Errorf("request aborted, deadline exceeded: %w", ctx.Err())
			c.logger.Log(ctx, c.transportFaultLevel(), "send aborted due to timeout", "error", err)
			return nil, err
		}
		err := fmt.Errorf("request aborted, shutdown initiated: %w", ctx.Err())
		c.logger.Log(ctx, c.transportFaultLevel(), "send aborted due to shutdown", "error", err)
		return nil, err
	case <-dropped:
		grace := time.NewTimer(droppedResponseGrace)
		defer grace.Stop()
		select {
		case response := <-sysCh:
			return response, nil
		case <-grace.C:
			return nil, ErrTransportClosed
		case <-ctx.Done():
			return nil, ErrTransportClosed
		}
	case response := <-sysCh:
		return response, nil
	}
}

// sendRequest is the single-shot RPC primitive behind every Client method: encode
// the frame, register a response channel, hand it to the transmit worker, wait.
// The caller's ctx is merged with requestTimeout, whichever fires first. Returns
// ErrTransportClosed at once on a known-dead transport; otherwise no retry, and
// mid-flight drops surface as ctx errors for Session to handle.
func (c *Conn) sendRequest(ctx context.Context, command ams.Command, data []byte) ([]byte, error) {
	return c.sendRequestTo(ctx, c.target, command, data)
}

// sendRequestTo is sendRequest addressed to an explicit AMS target — used to reach
// another port on the same device (the system service) over this one connection.
func (c *Conn) sendRequestTo(ctx context.Context, target ams.Address, command ams.Command, data []byte) ([]byte, error) {
	if c.tx.disconnected.Load() {
		return nil, ErrTransportClosed
	}
	c.tx.activeRequestLock.Lock()
	id := c.tx.currentRequest.Add(1)
	responseCh := make(chan amsReply, 1)
	c.tx.activeRequests[id] = responseCh
	c.tx.activeRequestLock.Unlock()
	defer func() {
		c.tx.activeRequestLock.Lock()
		delete(c.tx.activeRequests, id)
		c.tx.activeRequestLock.Unlock()
	}()
	c.logger.Log(context.Background(), logging.LevelTrace, "encoding packet",
		"command", command, "data", data, "id", id)

	pack, err := c.encodeTo(target, command, data, id)
	if err != nil {
		c.logger.Error("Error during sendRequest encode", "error", err)
		return nil, err
	}
	c.tx.chanMu.RLock()
	sendCh := c.tx.sendChannel
	c.tx.chanMu.RUnlock()
	dropped := c.dropped
	// Merge caller ctx with c.requestTimeout. ctx==nil falls back to the
	// Client's own ctx so callers passing context.Background() still get
	// the configured request timeout AND respect Close-driven cancel.
	if ctx == nil {
		ctx = c.ctx
	}
	ctx, cancel := context.WithTimeout(ctx, c.requestTimeout)
	defer cancel()
	select {
	case <-ctx.Done():
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			c.logger.Log(ctx, c.transportFaultLevel(), "sendRequest aborted due to timeout")
		} else {
			c.logger.Info("sendRequest aborted due to shutdown")
		}
		return nil, ctx.Err()
	case <-dropped:
		return nil, ErrTransportClosed
	case sendCh <- pack:
	}
	select {
	case <-ctx.Done():
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			c.logger.Log(ctx, c.transportFaultLevel(), "sendRequest aborted due to timeout")
		} else {
			c.logger.Info("sendRequest aborted due to shutdown")
		}
		return nil, ctx.Err()
	case <-dropped:
		// The transport died while this request was in flight. Give a reply that
		// is already in the receive pipeline a bounded moment to land before
		// giving up — see droppedResponseGrace.
		grace := time.NewTimer(droppedResponseGrace)
		defer grace.Stop()
		select {
		case reply := <-responseCh:
			return reply.payload()
		case <-grace.C:
			return nil, ErrTransportClosed
		case <-ctx.Done():
			return nil, ErrTransportClosed
		}
	case reply := <-responseCh:
		return reply.payload()
	}
}

// encode lives on *Client. Session callers reach it via s.client.encode
// at the rare sites still on Session.
func (c *Conn) encode(command ams.Command, data []byte, invokeID uint32) ([]byte, error) {
	return c.encodeTo(c.target, command, data, invokeID)
}

// encodeTo is encode addressed to an explicit target. Requests to a different AMS
// port on the same device (the system service, say) travel over this same
// connection: the AMS header carries the port, and the router allows only one TCP
// connection per remote IP, so opening a second one is not an option.
func (c *Conn) encodeTo(target ams.Address, command ams.Command, data []byte, invokeID uint32) ([]byte, error) {
	// One snapshot of source: a local-mode handshake can replace it while requests
	// are already in flight. target is set at construction and never mutated.
	source := c.tx.Source()
	c.logger.Log(context.Background(), logging.LevelTrace, "Starting encoding of AMS header",
		"command", command,
		"target", target,
		"source", source,
		"ID", invokeID,
		"length of data", len(data))
	tcpHeader := &ams.TCPHeader{
		Unknown1: 0,
		System:   0,
		Length:   uint32(ams.HeaderSize + len(data)),
	}
	header := &ams.Header{
		Target:    target,
		Source:    source,
		Command:   command,
		State:     uint16(4),
		Length:    uint32(len(data)),
		ErrorCode: uint32(0),
		InvokeID:  invokeID,
	}

	buff := new(bytes.Buffer)
	if err := binary.Write(buff, binary.LittleEndian, tcpHeader); err != nil {
		return nil, err
	}
	if err := binary.Write(buff, binary.LittleEndian, header); err != nil {
		return nil, err
	}
	if err := binary.Write(buff, binary.LittleEndian, data); err != nil {
		c.logger.Error("binary.Write failed", "error", err)
		return nil, err
	}
	c.logger.Log(context.Background(), logging.LevelTrace, "data to transmit", "data", data)
	c.logger.Log(context.Background(), logging.LevelTrace, "The encoded AMS header:", logging.HexAttr("bytes", buff.Bytes()))
	return buff.Bytes(), nil
}
