package ads

import (
	"fmt"
)

// TransMode is the transmission mode for an ADS device notification — how the
// PLC decides when to deliver a sample to the subscriber.
type TransMode uint32

const (
	TransModeNoTransmission TransMode = 0
	TransModeClientCycle    TransMode = 1
	TransModeClientOnChange TransMode = 2
	TransModeServerCycle    TransMode = 3
	TransModeServerOnChange TransMode = 4
	// The "InContext" variants run the notification check inside the PLC task cycle
	// rather than a separate ADS thread, for more deterministic timing. They need a
	// non-zero ContextMask (the owning task), which only variables local to a
	// PROGRAM POU in a multi-task project have -- GVLs and single-task projects are
	// always 0, and TC3 then rejects with 0x070B while TC2 silently never fires.
	// The AddSymbolNotification paths fall back automatically when it is 0.
	TransModeServerCycle2    TransMode = 5 // CyclicInContext
	TransModeServerOnChange2 TransMode = 6 // OnChangeInContext
	TransModeClient1Request  TransMode = 10
)

// String returns a human-readable name for the transmission mode.
func (tm TransMode) String() string {
	switch tm {
	case TransModeNoTransmission:
		return "NoTransmission"
	case TransModeClientCycle:
		return "ClientCycle"
	case TransModeClientOnChange:
		return "ClientOnChange"
	case TransModeServerCycle:
		return "ServerCycle"
	case TransModeServerOnChange:
		return "ServerOnChange"
	case TransModeServerCycle2:
		return "ServerCycle2/CyclicInContext"
	case TransModeServerOnChange2:
		return "ServerOnChange2/OnChangeInContext"
	case TransModeClient1Request:
		return "Client1Request"
	default:
		return fmt.Sprintf("Unknown(%d)", uint32(tm))
	}
}
