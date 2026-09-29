package ams

import (
	"time"
)

// SumReadRequest represents a single read request within a sum/batch read.
type SumReadRequest struct {
	Group  uint32
	Offset uint32
	Length uint32
}

// SumReadResult represents the result of a single read within a sum/batch read.
type SumReadResult struct {
	Error ReturnCode
	Data  []byte
}

// SumWriteRequest represents a single write request within a sum/batch write.
type SumWriteRequest struct {
	Group  uint32
	Offset uint32
	Data   []byte
}

// SumWriteResult represents the result of a single write within a sum/batch write.
type SumWriteResult struct {
	Error ReturnCode
}

// SumNotificationRequest represents a single notification add request within a batch.
type SumNotificationRequest struct {
	Group            uint32
	Offset           uint32
	Length           uint32
	TransmissionMode TransMode
	MaxDelay         time.Duration
	CycleTime        time.Duration
}

// SumNotificationResult is a per-item result from SumAddDeviceNotification.
// Either Skipped is non-nil (library refused to send this entry — duplicate,
// resolution failure, transport-aborted batch) or Skipped is nil and Error
// carries the PLC-side return code. Handle is valid only when Skipped == nil
// AND Error == ReturnCodeNoErrors.
type SumNotificationResult struct {
	Handle  uint32
	Error   ReturnCode // PLC-side return code; valid only when Skipped == nil
	Skipped error      // non-nil if library skipped this entry; Error/Handle not meaningful
}
