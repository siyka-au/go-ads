package adsconn

import (
	"sync/atomic"
)

// capabilities holds per-connection feature detection. sumReadCmd is 0 unchecked,
// the group constant for the variant in use, or 1 for no sum read at all; the
// sum*State fields are 0 unchecked / 1 supported / 2 unsupported, tracked
// separately because a PLC may serve Add but not Delete. Reset is implicit -- a
// fresh Client is allocated on every dial and starts zeroed.
type Capabilities struct {
	sumReadCmd               atomic.Uint32
	sumWriteState            atomic.Uint32
	sumAddNotifState         atomic.Uint32
	sumDeleteNotifState      atomic.Uint32
	chunkedDownloadSupported atomic.Bool
	chunkedDownloadChecked   atomic.Bool
}

func (c *Capabilities) SumReadCmdLoad() uint32    { return c.sumReadCmd.Load() }
func (c *Capabilities) SumReadCmdStore(v uint32)  { c.sumReadCmd.Store(v) }
func (c *Capabilities) SumWriteStateLoad() uint32 { return c.sumWriteState.Load() }
func (c *Capabilities) SumWriteStateStore(v uint32) {
	c.sumWriteState.Store(v)
}

func (c *Capabilities) SumWriteStateCAS(old, new uint32) bool {
	return c.sumWriteState.CompareAndSwap(old, new)
}
func (c *Capabilities) SumAddNotifStateLoad() uint32 { return c.sumAddNotifState.Load() }
func (c *Capabilities) SumAddNotifStateStore(v uint32) {
	c.sumAddNotifState.Store(v)
}

func (c *Capabilities) SumAddNotifStateCAS(old, new uint32) bool {
	return c.sumAddNotifState.CompareAndSwap(old, new)
}
func (c *Capabilities) SumDeleteNotifStateLoad() uint32 { return c.sumDeleteNotifState.Load() }
func (c *Capabilities) SumDeleteNotifStateStore(v uint32) {
	c.sumDeleteNotifState.Store(v)
}

func (c *Capabilities) SumDeleteNotifStateCAS(old, new uint32) bool {
	return c.sumDeleteNotifState.CompareAndSwap(old, new)
}

func (c *Capabilities) ChunkedDownloadCheckedLoad() bool   { return c.chunkedDownloadChecked.Load() }
func (c *Capabilities) ChunkedDownloadCheckedStore(v bool) { c.chunkedDownloadChecked.Store(v) }
func (c *Capabilities) ChunkedDownloadSupportedLoad() bool {
	return c.chunkedDownloadSupported.Load()
}

func (c *Capabilities) ChunkedDownloadSupportedStore(v bool) {
	c.chunkedDownloadSupported.Store(v)
}

// Reset is implicit: capabilities lives on *Client and a fresh Client is
// allocated on every Connect / dialAndStart, so the per-attempt struct
// value starts zeroed.

// disableSum marks every sum command unsupported, so each falls back to one
// request per item without probing.
func (c *Capabilities) disableSum() {
	c.sumReadCmd.Store(1)
	c.sumWriteState.Store(2)
	c.sumAddNotifState.Store(2)
	c.sumDeleteNotifState.Store(2)
}

// Capabilities exposes the probed sum-command support, so a caller (and tests)
// can force the per-item fallbacks on a live Conn.
func (c *Conn) Capabilities() *Capabilities { return &c.capabilities }
