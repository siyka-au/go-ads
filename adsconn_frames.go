package ads

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"time"
)

func (c *Client) listen() {
	defer c.waitGroup.Done()
	// Snapshot under the mutex that guards it. A reconnect writes tx.connection
	// while dialing, and reading it bare here raced that write — reported by -race
	// against the reconnect loop, which dials a fresh connection per attempt.
	c.tx.connMu.Lock()
	conn := c.tx.connection
	c.tx.connMu.Unlock()
	if conn == nil {
		c.logger.Debug("listen: no connection to read from")
		return
	}
	c.readFrames(conn, true)
}

// readFrames pumps AMS frames from conn into the transport's queues.
//
// primary distinguishes our own connection from an adopted inbound one. Losing
// the primary connection means the transport is down and reconnect must fire;
// losing an inbound one does not — the PLC can simply dial again, and treating it
// as a drop would tear down a working session.
func (c *Client) readFrames(conn net.Conn, primary bool) {
	reader := bufio.NewReader(conn)
	const maxAMSPacket = 4 * 1024 * 1024
	var hdrBytes [6]byte
	for {
		select {
		case <-c.ctx.Done():
			c.logger.Info("exit listen")
			return
		default:
		}
		if _, err := io.ReadFull(reader, hdrBytes[:]); err != nil {
			select {
			case <-c.ctx.Done():
				return
			default:
			}
			if !primary {
				c.logger.Debug("inbound PLC connection closed", "error", err)
				return
			}
			c.logDropVerdict(err)
			c.callOnDrop()
			return
		}
		var tcpHeader amsTCPHeader
		if err := binary.Read(bytes.NewReader(hdrBytes[:]), binary.LittleEndian, &tcpHeader); err != nil {
			c.logger.Error("listen header decode error, transport down", "error", err, "primary", primary)
			if primary {
				c.callOnDrop()
			}
			return
		}
		if tcpHeader.Length > maxAMSPacket {
			c.logger.Error("AMS packet length exceeds sanity limit, transport down",
				"length", tcpHeader.Length, "primary", primary)
			if primary {
				c.callOnDrop()
			}
			return
		}
		data := make([]byte, tcpHeader.Length)
		select {
		case <-c.ctx.Done():
			return
		default:
		}
		if _, err := io.ReadFull(reader, data); err != nil {
			select {
			case <-c.ctx.Done():
				return
			default:
			}
			if !primary {
				c.logger.Debug("inbound PLC connection failed mid-frame", "error", err)
				return
			}
			c.logger.Log(c.ctx, c.transportFaultLevel(), "listen body read error, transport down", "error", err)
			c.callOnDrop()
			return
		}
		// A full frame arrived. Counted here rather than in listen because this is
		// the only decode loop and it serves both socket directions; see
		// framesPrimary for why the split matters.
		if primary {
			c.framesPrimary.Add(1)
		} else {
			c.framesPeer.Add(1)
		}
		if tcpHeader.System > 0 {
			select {
			case c.tx.systemResponse <- data:
			case <-c.ctx.Done():
				return
			}
		} else {
			select {
			case c.tx.recvQueue <- data:
			case <-c.ctx.Done():
				return
			default:
				c.logger.Warn("recvQueue full, dropping inbound packet (PLC overrun or slow handler)",
					"queue_size", recvQueueSize,
					"workers", recvWorkerCount,
					"packet_bytes", len(data))
			}
		}
	}
}

func (c *Client) recvWorker() {
	defer c.waitGroup.Done()
	for {
		select {
		case <-c.ctx.Done():
			return
		case data, ok := <-c.tx.recvQueue:
			if !ok {
				return
			}
			c.handleReceive(c.ctx, data)
		}
	}
}

func (c *Client) handleReceive(ctx context.Context, data []byte) {
	c.logger.Log(context.Background(), LevelTrace, "in read")
	if len(data) < 32 {
		c.logger.Error("header too short")
		return
	}
	buff := bytes.NewBuffer(data)
	header := AMSHeader{}
	if err := binary.Read(buff, binary.LittleEndian, &header); err != nil {
		c.logger.Error("Error parsing header", "error", err)
		return
	}
	c.logger.Log(context.Background(), LevelTrace, "header info", "header", header)
	adsData := data[32:]
	if len(adsData) != int(header.Length) {
		c.logger.Error("Error parsing body")
		return
	}
	switch header.Command {
	case CommandIDDeviceNotification:
		if err := c.deviceNotification(ctx, adsData); err != nil {
			c.logger.Error("device notification decode failed", "error", err)
		}
	default:
		c.logger.Log(context.Background(), LevelTrace, "default receive")
		c.tx.activeRequestLock.Lock()
		response, ok := c.tx.activeRequests[header.InvokeID]
		c.tx.activeRequestLock.Unlock()
		if ok {
			select {
			case <-ctx.Done():
				c.logger.Info("receive channel timed out",
					"id", header.InvokeID, "command", header.Command)
				return
			case response <- amsReply{data: adsData, amsErr: ReturnCode(header.ErrorCode)}:
				c.logger.Log(context.Background(), LevelTrace, "Successfully delivered answer",
					"id", header.InvokeID, "command", header.Command)
			}
		} else {
			// Stale invokeID. Expected during reconnect cleanup or shutdown:
			// activeRequests was cleared, late PLC responses arrive after.
			// Always Debug — Client has no FSM context to distinguish more
			// precisely; production debugging uses the InvokeID + command
			// pair to spot true protocol bugs.
			c.logger.Debug("received packet with unknown invokeID",
				"invokeID", header.InvokeID,
				"command", header.Command)
		}
	}
}

func (c *Client) transmitWorker() {
	defer c.waitGroup.Done()
	c.tx.connMu.Lock()
	conn := c.tx.connection
	c.tx.connMu.Unlock()
	if conn == nil {
		c.logger.Debug("transmitWorker: no connection to write to")
		return
	}
	writer := bufio.NewWriter(conn)
	for {
		select {
		case <-c.ctx.Done():
			c.logger.Debug("Exit transmitWorker")
			return
		case data := <-c.tx.sendChannel:
			c.logger.Log(context.Background(), LevelTrace, fmt.Sprintf("Sending %d bytes", len(data)))
			// Bound the write. Without a deadline a send into a backed-up buffer
			// blocks here for as long as the kernel retries (tcp_retries2, ~15
			// minutes), and callOnDrop below is only reached on an error, so a
			// wedged link never reports itself. The deadline turns that into the
			// error case this loop already handles. Same budget as a request: a
			// write that cannot drain in the time a caller waits for its reply is
			// not going to.
			if err := conn.SetWriteDeadline(time.Now().Add(c.requestTimeout)); err != nil {
				c.logger.Log(c.ctx, c.transportFaultLevel(), "error setting the write deadline, transport down", "error", err)
				c.callOnDrop()
				return
			}
			if _, err := writer.Write(data); err != nil {
				c.logger.Log(c.ctx, c.transportFaultLevel(), "error sending data on conn, transport down", "error", err)
				c.callOnDrop()
				return
			}
			if err := writer.Flush(); err != nil {
				c.logger.Log(c.ctx, c.transportFaultLevel(), "error flushing data on conn, transport down", "error", err)
				c.callOnDrop()
				return
			}
			// Cleared so an idle socket is never judged by a stale deadline: the
			// next write sets its own.
			if err := conn.SetWriteDeadline(time.Time{}); err != nil {
				c.logger.Log(c.ctx, c.transportFaultLevel(), "error clearing the write deadline, transport down", "error", err)
				c.callOnDrop()
				return
			}
		}
	}
}

func (c *Client) deviceNotification(ctx context.Context, in []byte) error {
	var stream NotificationStream
	var header StampHeader
	var sample NotificationSample
	var content []byte
	data := bytes.NewBuffer(in)
	if err := binary.Read(data, binary.LittleEndian, &stream); err != nil {
		return fmt.Errorf("unable to read notification: %w", err)
	}
	for i := uint32(0); i < stream.Stamps; i++ {
		if err := binary.Read(data, binary.LittleEndian, &header); err != nil {
			return fmt.Errorf("error reading stamp header: %w", err)
		}
		for j := uint32(0); j < header.Samples; j++ {
			if err := binary.Read(data, binary.LittleEndian, &sample); err != nil {
				return fmt.Errorf("error reading notification sample: %w", err)
			}
			if sample.Size > uint32(data.Len()) {
				return fmt.Errorf("notification sample size %d exceeds remaining data %d",
					sample.Size, data.Len())
			}
			content = make([]byte, sample.Size)
			n, err := data.Read(content)
			if err != nil {
				return fmt.Errorf("error reading notification content: %w", err)
			}
			if n != int(sample.Size) {
				return fmt.Errorf("short read on notification content: got %d of %d bytes",
					n, sample.Size)
			}
			c.dispatchNotification(ctx, sample.Handle, header.Timestamp, content)
		}
	}
	return nil
}

func (c *Client) dispatchNotification(ctx context.Context, handle uint32, ts uint64, content []byte) {
	c.notifyMu.RLock()
	fn := c.notify
	c.notifyMu.RUnlock()
	if fn == nil {
		c.logger.Debug("DeviceNotification dropped (no handler installed)", "handle", handle)
		return
	}
	fn(ctx, handle, ts, content)
}

const (
	windowsTick    int64 = 10000000
	secToUnixEpoch int64 = 11644473600
)

// NotificationStream is the outer header of an ADS DeviceNotification
// payload: Length is the total payload length in bytes, Stamps is the
// number of StampHeader records that follow.
type NotificationStream struct {
	Length uint32
	Stamps uint32
}

// StampHeader prefixes the samples that share a single PLC sample
// timestamp. Timestamp is in Windows FILETIME ticks (100 ns since 1601-01-01
// UTC). Samples is the count of NotificationSample records that follow.
type StampHeader struct {
	Timestamp uint64
	Samples   uint32
}

// NotificationSample is the per-handle prefix inside a stamp record:
// Handle identifies the subscription that produced the value; Size is the
// byte length of the value bytes that follow.
type NotificationSample struct {
	Handle uint32
	Size   uint32
}
