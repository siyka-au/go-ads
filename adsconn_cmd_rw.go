package ads

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"

	"github.com/siyka-au/go-ads/v3/ams"
)

// Single-symbol ADS commands on *Client: Read, Write, WriteRead,
// ReadState, ReadDeviceInfo. Beckhoff-equivalent thin RPC surface.
// Cache-aware Session methods (ReadFromSymbol etc.) in symbol_access.go
// call s.client.Read/Write internally.

// Read issues ADS Read (cmd 2) against the given index group/offset.
func (c *Client) Read(ctx context.Context, group uint32, offset uint32, length uint32) (data []byte, err error) {
	request := new(bytes.Buffer)
	type readCommandPacket struct {
		Group  uint32
		Offset uint32
		Length uint32
	}
	content := readCommandPacket{
		group,
		offset,
		length,
	}

	err = binary.Write(request, binary.LittleEndian, content)
	if err != nil {
		c.logger.Error("binary.Write failed", "error", err)
		return nil, err
	}

	c.logger.Log(context.Background(), LevelTrace, "request", "request", content)

	// Try to send the request
	resp, err := c.sendRequest(ctx, ams.CommandRead, request.Bytes())
	if err != nil {
		c.logger.Log(ctx, c.transportFaultLevel(), "send request failed", "error", err)
		return
	}

	// Check the result error code
	type readResponse struct {
		Error  ams.ReturnCode
		Length uint32
	}
	respBuff := bytes.NewBuffer(resp)
	response := &readResponse{}
	err = binary.Read(respBuff, binary.LittleEndian, response)
	if err != nil {
		return nil, fmt.Errorf("failed to parse Read response: %w", err)
	}
	if response.Error > 0 {
		err = fmt.Errorf("ADS error in Read: %w", response.Error)
		return
	}
	// validate that the body has the bytes the response header declared.
	// Returning the raw remaining buffer would silently pass through truncated
	// or padded payloads. Trust the declared Length; reject undersized.
	if uint64(respBuff.Len()) < uint64(response.Length) {
		return nil, fmt.Errorf("Read: declared length %d, body has %d bytes", response.Length, respBuff.Len())
	}
	return respBuff.Next(int(response.Length)), nil
}

// Write issues ADS Write (cmd 3) to the PLC at the given index group/offset.
func (c *Client) Write(ctx context.Context, group uint32, offset uint32, data []byte) error {
	type writeCommandPacket struct {
		Group  uint32
		Offset uint32
		Length uint32
	}
	request := new(bytes.Buffer)
	writeRequest := writeCommandPacket{
		group,
		offset,
		uint32(len(data)),
	}

	err := binary.Write(request, binary.LittleEndian, writeRequest)
	if err != nil {
		c.logger.Error("binary.Write failed", "error", err)
		return err
	}
	err = binary.Write(request, binary.LittleEndian, data)
	if err != nil {
		c.logger.Error("binary.Write failed", "error", err)
		return err
	}

	// Try to send the request
	resp, err := c.sendRequest(ctx, ams.CommandWrite, request.Bytes())
	if err != nil {
		c.logger.Log(ctx, c.transportFaultLevel(), "error during send request for write", "error", err)
		return err
	}
	respBuffer := bytes.NewBuffer(resp)
	var respCode ams.ReturnCode
	// Check the result error code
	if err = binary.Read(respBuffer, binary.LittleEndian, &respCode); err != nil {
		return fmt.Errorf("failed to parse Write response: %w", err)
	}
	if respCode > 0 {
		return fmt.Errorf("ADS error in Write: %w", respCode)
	}

	return nil
}

// WriteRead issues ADS ReadWrite (cmd 9): writes send and reads back up to
// readLength bytes in a single round-trip.
func (c *Client) WriteRead(ctx context.Context, group uint32, offset uint32, readLength uint32, send []byte) (data []byte, err error) {
	request := new(bytes.Buffer)
	type writeReadCommandPacket struct {
		Group       uint32
		Offset      uint32
		ReadLength  uint32
		WriteLength uint32
	}
	content := writeReadCommandPacket{
		group,
		offset,
		readLength,
		uint32(len(send)),
	}

	type readResponse struct {
		Error  ams.ReturnCode
		Length uint32
	}

	err = binary.Write(request, binary.LittleEndian, content)
	if err != nil {
		return nil, fmt.Errorf("binary.Write failed: %w", err)
	}
	err = binary.Write(request, binary.LittleEndian, send)
	if err != nil {
		return nil, fmt.Errorf("binary.Write failed: %w", err)
	}

	c.logger.Log(context.Background(), LevelTrace, "request", "request", request)

	// Try to send the request
	resp, err := c.sendRequest(ctx, ams.CommandReadWrite, request.Bytes())
	if err != nil {
		return
	}

	// Check the result error code
	respBuff := bytes.NewBuffer(resp)
	response := &readResponse{}
	if err = binary.Read(respBuff, binary.LittleEndian, response); err != nil {
		return nil, fmt.Errorf("failed to parse WriteRead response: %w", err)
	}
	if response.Error > 0 {
		return nil, fmt.Errorf("ADS error in WriteRead: %w", response.Error)
	}
	// validate body length against declared Length (same rationale as Read above).
	if uint64(respBuff.Len()) < uint64(response.Length) {
		return nil, fmt.Errorf("WriteRead: declared length %d, body has %d bytes", response.Length, respBuff.Len())
	}
	return respBuff.Next(int(response.Length)), nil
}

// ReadState issues ADS ReadState (cmd 4) and returns the PLC's ADS+device state.
func (c *Client) ReadState(ctx context.Context) (response ams.StateInfo, err error) {
	return c.readStateOn(ctx, c.target)
}

// ReadStateOnPort reads the ADS state of another port on the same device. The
// system service (PortSystemService) is the one that matters: it answers while the
// system is in CONFIG, when the runtime ports do not exist at all.
func (c *Client) ReadStateOnPort(ctx context.Context, port ams.Port) (ams.StateInfo, error) {
	target := c.target
	target.Port = port
	return c.readStateOn(ctx, target)
}

func (c *Client) readStateOn(ctx context.Context, target ams.Address) (response ams.StateInfo, err error) {
	// Try to send the request
	resp, err := c.sendRequestTo(ctx, target, ams.CommandReadState, []byte{})
	if err != nil {
		c.logger.Log(ctx, c.transportFaultLevel(), "error during read state", "error", err)
		return
	}
	c.logger.Log(context.Background(), LevelTrace, "response from plc for state", "data", resp)
	type readStateResponse struct {
		Error ams.ReturnCode
		ams.StateInfo
	}
	stateResponse := &readStateResponse{}
	buff := bytes.NewBuffer(resp)
	if err = binary.Read(buff, binary.LittleEndian, stateResponse); err != nil {
		return response, fmt.Errorf("failed to parse ReadState response: %w", err)
	}
	if stateResponse.Error > 0 {
		return response, fmt.Errorf("ADS error in ReadState: %w", stateResponse.Error)
	}
	c.logger.Debug("read state response",
		"ADSState", uint16(stateResponse.State),
		"deviceState", stateResponse.DeviceState)

	return stateResponse.StateInfo, nil
}

// ReadDeviceInfo issues ADS ReadDeviceInfo (cmd 1).
func (c *Client) ReadDeviceInfo(ctx context.Context) (response ams.DeviceInfo, err error) {
	// Try to send the request
	resp, err := c.sendRequest(ctx, ams.CommandReadDeviceInfo, []byte{})
	if err != nil {
		return
	}

	// Check the response length
	if len(resp) != 24 {
		return response, fmt.Errorf("wrong length of response! Got %d bytes and it should be 24", len(resp))
	}
	type readDeviceInfoResponse struct {
		Error      ams.ReturnCode
		Major      uint8
		Minor      uint8
		Build      uint16
		DeviceName [16]byte // NUL-padded
	}
	respBuffer := bytes.NewBuffer(resp)
	deviceInfoResponse := readDeviceInfoResponse{}
	if err = binary.Read(respBuffer, binary.LittleEndian, &deviceInfoResponse); err != nil {
		return response, fmt.Errorf("failed to parse ReadDeviceInfo response: %w", err)
	}
	if deviceInfoResponse.Error > 0 {
		err = fmt.Errorf("ADS error in ReadDeviceInfo: %w", deviceInfoResponse.Error)
		return
	}

	name := deviceInfoResponse.DeviceName[:]
	if i := bytes.IndexByte(name, 0); i >= 0 {
		name = name[:i]
	}
	return ams.DeviceInfo{
		Name:  string(name),
		Major: deviceInfoResponse.Major,
		Minor: deviceInfoResponse.Minor,
		Build: deviceInfoResponse.Build,
	}, nil
}

// ReleaseHandle releases a symbol handle previously acquired via
// GetHandleByName. Wraps Write to GroupSymbolReleaseHandle so the
// Beckhoff-equivalent surface includes a symmetric release primitive.
func (c *Client) ReleaseHandle(ctx context.Context, handle uint32) error {
	handleBytes := make([]byte, 4)
	binary.LittleEndian.PutUint32(handleBytes, handle)
	return c.Write(ctx, uint32(ams.GroupSymbolReleaseHandle), 0, handleBytes)
}
