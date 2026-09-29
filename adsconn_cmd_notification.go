package ads

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"time"
)

// Single-symbol device-notification raw RPCs on *Client:
// AddDeviceNotification, DeleteDeviceNotification. Notification persistence
// and activeNotifications cleanup is the Session's wrapper concern (see
// Session.DeleteDeviceNotification below). The cache-aware
// handleNotification dispatcher also lives in this file because it shares
// Update / handle bookkeeping; it is wired into the Client via
// Client.SetNotificationHandler at Session.Connect and Session.dialAndStart
// (the two Client-allocation sites in session.go).

// durationToADSTicks converts a time.Duration to ADS 100ns tick units (uint32).
// Returns an error if d is negative or exceeds the ADS 32-bit limit (~429.5 s).
func durationToADSTicks(d time.Duration, name string) (uint32, error) {
	if d < 0 {
		return 0, fmt.Errorf("%s must be non-negative, got %v", name, d)
	}
	ticks := d.Nanoseconds() / 100
	if ticks > math.MaxUint32 {
		return 0, fmt.Errorf("%s exceeds ADS 32-bit 100ns limit (~429.5s), got %v", name, d)
	}
	return uint32(ticks), nil
}

// AddDeviceNotification registers a device notification with the PLC and
// returns the PLC-assigned handle. Raw RPC: no Session-side persistence.
// Callers wanting auto-resubscribe-on-reconnect use Session.AddSymbolNotification.
func (c *Client) AddDeviceNotification(
	ctx context.Context,
	group uint32,
	offset uint32,
	length uint32,
	transmissionMode TransMode,
	maxDelay time.Duration,
	cycleTime time.Duration,
) (handle uint32, err error) {
	request := new(bytes.Buffer)
	type addDeviceNotificationCommandPacket struct {
		Group            uint32
		Offset           uint32
		Length           uint32
		TransmissionMode uint32
		MaxDelay         uint32
		CycleTime        uint32
		Reserved         [16]byte
	}
	maxDelayTicks, err := durationToADSTicks(maxDelay, "maxDelay")
	if err != nil {
		return 0, err
	}
	cycleTimeTicks, err := durationToADSTicks(cycleTime, "cycleTime")
	if err != nil {
		return 0, err
	}
	content := addDeviceNotificationCommandPacket{
		group,
		offset,
		length,
		uint32(transmissionMode),
		maxDelayTicks,
		cycleTimeTicks,
		[16]byte{},
	}
	if err = binary.Write(request, binary.LittleEndian, content); err != nil {
		return 0, fmt.Errorf("binary.Write failed: %w", err)
	}
	type addDeviceNotificationResponse struct {
		Error  ReturnCode
		Handle uint32
	}
	resp, err := c.sendRequest(ctx, CommandIDAddDeviceNotification, request.Bytes())
	if err != nil {
		return
	}
	respBuffer := bytes.NewBuffer(resp)
	notificationResponse := addDeviceNotificationResponse{}
	if err = binary.Read(respBuffer, binary.LittleEndian, &notificationResponse); err != nil {
		// Error stays: a response that will not decode is broken framing, not a PLC
		// saying no. Retrying cannot fix it and someone has to look.
		c.logger.Error("failed to parse notification response", "error", err)
		return 0, err
	}
	if notificationResponse.Error != 0 {
		// Warn, not Error: the PLC refusing an Add is the normal answer while the
		// runtime is in CONFIG, and the heartbeat watcher retries on a cadence — an
		// Error per attempt is what produced 28% of one integration log. The code is
		// returned to the caller either way, so nothing loses its signal.
		c.logger.Warn("failed to add notification handler", "errorCode", uint32(notificationResponse.Error))
		return 0, fmt.Errorf("unable to create notification: %w", notificationResponse.Error)
	}
	c.logger.Log(context.Background(), LevelTrace, "added notification handler", "handle", notificationResponse.Handle)
	return notificationResponse.Handle, nil
}

// DeleteDeviceNotification deletes a device notification by handle. Raw RPC:
// returns the wire-level success/error. Callers that maintain
// activeNotifications must clean up themselves (Session does this in its
// wrapper Session.DeleteDeviceNotification below).
func (c *Client) DeleteDeviceNotification(ctx context.Context, handle uint32) error {
	request := &bytes.Buffer{}
	type deleteNotificationCommandPacket struct {
		Handle uint32
	}
	content := deleteNotificationCommandPacket{handle}
	if err := binary.Write(request, binary.LittleEndian, content); err != nil {
		return fmt.Errorf("binary.Write failed: %w", err)
	}
	resp, err := c.sendRequest(ctx, CommandIDDeleteDeviceNotification, request.Bytes())
	if err != nil {
		c.logger.Warn("error deleting handle", "handle", handle, "error", err)
		return err
	}
	respBuffer := bytes.NewBuffer(resp)
	var adsError ReturnCode
	if err = binary.Read(respBuffer, binary.LittleEndian, &adsError); err != nil {
		return fmt.Errorf("failed to parse DeleteDeviceNotification response: %w", err)
	}
	if adsError > 0 {
		c.logger.Warn("error deleting handle", "handle", handle, "errorCode", uint32(adsError))
		return fmt.Errorf("ADS error in DeleteDeviceNotification: %w", adsError)
	}
	// Debug, not Info: at this layer there is no symbol name to report, and
	// teardown of an N-symbol subscription would print N unhelpful lines. The
	// Session wrappers log the named line and the summary.
	c.logger.Debug("deleted notification handle", "handle", handle)
	return nil
}
