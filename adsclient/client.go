// Package adsclient is a raw ADS client: one TCP connection to a TwinCAT AMS
// router, with requests multiplexed over it. It has no symbol cache and does
// not reconnect — once the connection drops, every call returns
// ErrTransportClosed and the Client has to be replaced.
//
// Most applications want the root ads package instead, whose Session adds
// symbol names, typed values, persistent subscriptions and automatic
// reconnection. Use this package for index-group level access, tooling, or a
// Session's own connection via Session.Client.
package adsclient

import (
	"context"
	"fmt"
	"time"

	"github.com/siyka-au/go-ads/v3/ams"
	"github.com/siyka-au/go-ads/v3/internal/adsconn"
)

// ErrTransportClosed is returned by every call once the connection is gone.
var ErrTransportClosed = adsconn.ErrTransportClosed

// NotificationHandler receives the samples of device notifications registered
// with AddDeviceNotification. It runs on the connection's receive workers, so
// it must not block for long.
type NotificationHandler func(handle uint32, timestamp time.Time, data []byte)

// Client is a raw ADS connection. Its methods are safe for concurrent use.
type Client struct {
	// conn returns the live connection, or nil when there is none: a Client from
	// Dial owns one fixed connection, one bound to a Session follows that
	// session's reconnects.
	conn  func() *adsconn.Conn
	owned *adsconn.Conn
}

func init() {
	adsconn.BindClient = func(current func() *adsconn.Conn) any {
		return &Client{conn: current}
	}
}

// Dial opens a connection to the AMS router at host and addresses requests to
// target. host may carry a port; the default is DefaultPort. The source
// address defaults to the local IP of the connection plus ".1.1", which the
// device must have a route for (see the router package).
func Dial(ctx context.Context, host string, target ams.Address, opts ...Option) (*Client, error) {
	cfg := config{port: DefaultPort, requestTimeout: 5 * time.Second}
	for _, o := range opts {
		o(&cfg)
	}
	c, err := adsconn.DialContext(ctx, adsconn.DialConfig{
		Host:           host,
		Port:           cfg.port,
		Target:         target,
		Source:         cfg.source,
		RequestTimeout: cfg.requestTimeout,
		Logger:         cfg.logger,
		DisableSum:     cfg.disableSum,
	})
	if err != nil {
		return nil, err
	}
	if cfg.onDrop != nil {
		c.SetOnDrop(cfg.onDrop)
	}
	if cfg.onNotify != nil {
		c.SetNotificationHandler(adaptHandler(cfg.onNotify))
	}
	c.Start()
	return &Client{conn: func() *adsconn.Conn { return c }, owned: c}, nil
}

func adaptHandler(fn NotificationHandler) adsconn.NotificationHandler {
	return func(_ context.Context, handle uint32, ts uint64, data []byte) {
		fn(handle, adsconn.FiletimeToTime(ts), data)
	}
}

// Close closes a Client from Dial and waits for its workers. On a Client from
// Session.Client it does nothing: the session owns that connection.
func (c *Client) Close() error {
	if c.owned == nil {
		return nil
	}
	return c.owned.Close()
}

// SetNotificationHandler installs (or, with nil, removes) the handler for
// device notification samples. It fails on a Client bound to a Session, which
// routes notifications to its own subscriptions.
func (c *Client) SetNotificationHandler(fn NotificationHandler) error {
	if c.owned == nil {
		return fmt.Errorf("adsclient: notifications on a session's connection belong to the session; use Session.Subscribe")
	}
	if fn == nil {
		c.owned.SetNotificationHandler(nil)
		return nil
	}
	c.owned.SetNotificationHandler(adaptHandler(fn))
	return nil
}

func (c *Client) cur() (*adsconn.Conn, error) {
	if conn := c.conn(); conn != nil {
		return conn, nil
	}
	return nil, ErrTransportClosed
}

// Read reads length bytes from index group/offset.
func (c *Client) Read(ctx context.Context, group ams.Group, offset, length uint32) ([]byte, error) {
	conn, err := c.cur()
	if err != nil {
		return nil, err
	}
	return conn.Read(ctx, uint32(group), offset, length)
}

// Write writes data to index group/offset.
func (c *Client) Write(ctx context.Context, group ams.Group, offset uint32, data []byte) error {
	conn, err := c.cur()
	if err != nil {
		return err
	}
	return conn.Write(ctx, uint32(group), offset, data)
}

// WriteRead writes data to index group/offset and reads up to readLength bytes
// of the answer in the same request.
func (c *Client) WriteRead(ctx context.Context, group ams.Group, offset, readLength uint32, data []byte) ([]byte, error) {
	conn, err := c.cur()
	if err != nil {
		return nil, err
	}
	return conn.WriteRead(ctx, uint32(group), offset, readLength, data)
}

// ReadState reads the target's ADS and device state.
func (c *Client) ReadState(ctx context.Context) (ams.StateInfo, error) {
	conn, err := c.cur()
	if err != nil {
		return ams.StateInfo{}, err
	}
	return conn.ReadState(ctx)
}

// ReadStateOnPort reads the state of another AMS port on the same device, such
// as ams.PortSystemService, over this connection.
func (c *Client) ReadStateOnPort(ctx context.Context, port ams.Port) (ams.StateInfo, error) {
	conn, err := c.cur()
	if err != nil {
		return ams.StateInfo{}, err
	}
	return conn.ReadStateOnPort(ctx, port)
}

// ReadDeviceInfo reads the target's name and version.
func (c *Client) ReadDeviceInfo(ctx context.Context) (ams.DeviceInfo, error) {
	conn, err := c.cur()
	if err != nil {
		return ams.DeviceInfo{}, err
	}
	return conn.ReadDeviceInfo(ctx)
}

// SumRead performs several reads in one request. It falls back to one request
// per item on devices without sum-command support.
func (c *Client) SumRead(ctx context.Context, requests []ams.SumReadRequest) ([]ams.SumReadResult, error) {
	conn, err := c.cur()
	if err != nil {
		return nil, err
	}
	return conn.SumRead(ctx, requests)
}

// SumWrite performs several writes in one request, with the same fallback as
// SumRead.
func (c *Client) SumWrite(ctx context.Context, requests []ams.SumWriteRequest) ([]ams.SumWriteResult, error) {
	conn, err := c.cur()
	if err != nil {
		return nil, err
	}
	return conn.SumWrite(ctx, requests)
}

// AddDeviceNotification asks the device to send samples of length bytes at
// group/offset, and returns the handle they will carry. Samples go to the
// Client's NotificationHandler.
func (c *Client) AddDeviceNotification(ctx context.Context, group ams.Group, offset, length uint32, mode ams.TransMode, maxDelay, cycleTime time.Duration) (uint32, error) {
	conn, err := c.cur()
	if err != nil {
		return 0, err
	}
	return conn.AddDeviceNotification(ctx, uint32(group), offset, length, mode, maxDelay, cycleTime)
}

// DeleteDeviceNotification removes a notification registered with
// AddDeviceNotification.
func (c *Client) DeleteDeviceNotification(ctx context.Context, handle uint32) error {
	conn, err := c.cur()
	if err != nil {
		return err
	}
	return conn.DeleteDeviceNotification(ctx, handle)
}

// SumAddDeviceNotification registers several notifications in one request.
func (c *Client) SumAddDeviceNotification(ctx context.Context, requests []ams.SumNotificationRequest) ([]ams.SumNotificationResult, error) {
	conn, err := c.cur()
	if err != nil {
		return nil, err
	}
	return conn.SumAddDeviceNotification(ctx, requests)
}

// SumDeleteDeviceNotification removes several notifications in one request.
func (c *Client) SumDeleteDeviceNotification(ctx context.Context, handles []uint32) ([]ams.ReturnCode, error) {
	conn, err := c.cur()
	if err != nil {
		return nil, err
	}
	return conn.SumDeleteDeviceNotification(ctx, handles)
}

// SymbolInfo looks up one symbol's address and type by name.
func (c *Client) SymbolInfo(ctx context.Context, name string) (ams.SymbolInfo, error) {
	conn, err := c.cur()
	if err != nil {
		return ams.SymbolInfo{}, err
	}
	return conn.GetSymbolInfoByName(ctx, name)
}

// Handle acquires a handle for a symbol name, to read and write it through
// GroupSymbolValueByHandle. Release it with ReleaseHandle.
func (c *Client) Handle(ctx context.Context, name string) (uint32, error) {
	conn, err := c.cur()
	if err != nil {
		return 0, err
	}
	return conn.GetHandleByName(ctx, name)
}

// ReleaseHandle releases a handle from Handle.
func (c *Client) ReleaseHandle(ctx context.Context, handle uint32) error {
	conn, err := c.cur()
	if err != nil {
		return err
	}
	return conn.ReleaseHandle(ctx, handle)
}

// SymbolUploadInfo reads the sizes of the device's symbol and data type tables.
func (c *Client) SymbolUploadInfo(ctx context.Context) (ams.SymbolUploadInfo, error) {
	conn, err := c.cur()
	if err != nil {
		return ams.SymbolUploadInfo{}, err
	}
	return conn.GetSymbolUploadInfo(ctx)
}

// SymbolVersion reads the symbol table version, which changes when the PLC
// program is downloaded again.
func (c *Client) SymbolVersion(ctx context.Context) (uint8, error) {
	conn, err := c.cur()
	if err != nil {
		return 0, err
	}
	return conn.GetSymbolVersion(ctx)
}

// ReadChunked reads totalLength bytes from group in chunkSize pieces, pausing
// delay between them, to spare a busy PLC one large request.
func (c *Client) ReadChunked(ctx context.Context, group ams.Group, totalLength, chunkSize uint32, delay time.Duration) ([]byte, error) {
	conn, err := c.cur()
	if err != nil {
		return nil, err
	}
	return conn.DownloadInChunks(ctx, uint32(group), totalLength, chunkSize, delay)
}

// ReadProcessInput reads length bytes of the process input image.
func (c *Client) ReadProcessInput(ctx context.Context, byteOffset, length uint32) ([]byte, error) {
	conn, err := c.cur()
	if err != nil {
		return nil, err
	}
	return conn.ReadProcessInput(ctx, byteOffset, length)
}

// ReadProcessOutput reads length bytes of the process output image.
func (c *Client) ReadProcessOutput(ctx context.Context, byteOffset, length uint32) ([]byte, error) {
	conn, err := c.cur()
	if err != nil {
		return nil, err
	}
	return conn.ReadProcessOutput(ctx, byteOffset, length)
}

// WriteProcessOutput writes data into the process output image.
func (c *Client) WriteProcessOutput(ctx context.Context, byteOffset uint32, data []byte) error {
	conn, err := c.cur()
	if err != nil {
		return err
	}
	return conn.WriteProcessOutput(ctx, byteOffset, data)
}

// ReadProcessInputBit reads one bit (0-7) of the process input image.
func (c *Client) ReadProcessInputBit(ctx context.Context, byteOffset uint32, bit uint8) (bool, error) {
	conn, err := c.cur()
	if err != nil {
		return false, err
	}
	return conn.ReadProcessInputBit(ctx, byteOffset, bit)
}

// WriteProcessOutputBit sets or clears one bit (0-7) of the process output image.
func (c *Client) WriteProcessOutputBit(ctx context.Context, byteOffset uint32, bit uint8, value bool) error {
	conn, err := c.cur()
	if err != nil {
		return err
	}
	return conn.WriteProcessOutputBit(ctx, byteOffset, bit, value)
}

// ReadProcessInputSize reads the size of the process input image in bytes.
func (c *Client) ReadProcessInputSize(ctx context.Context) (uint32, error) {
	conn, err := c.cur()
	if err != nil {
		return 0, err
	}
	return conn.ReadProcessInputSize(ctx)
}
