package ams

import "fmt"

// State is the run-time state of an ADS device (typically the PLC runtime), as
// reported by ReadState.
type State uint16

const (
	StateInvalid      State = 0
	StateIdle         State = 1
	StateReset        State = 2
	StateInit         State = 3
	StateStart        State = 4
	StateRun          State = 5
	StateStop         State = 6
	StateSaveCfg      State = 7
	StateLoadCfg      State = 8
	StatePowerFailure State = 9
	StatePowerGood    State = 10
	StateError        State = 11
	StateShutdown     State = 12
	StateSuspend      State = 13
	StateResume       State = 14
	StateConfig       State = 15 // System Is In Config Mode
	StateReconfig     State = 16 // System Should Restart In Config Mode
	StateMaxStates    State = 255
)

var stateNames = map[State]string{
	StateInvalid:      "Invalid",
	StateIdle:         "Idle",
	StateReset:        "Reset",
	StateInit:         "Init",
	StateStart:        "Start",
	StateRun:          "Run",
	StateStop:         "Stop",
	StateSaveCfg:      "SaveCfg",
	StateLoadCfg:      "LoadCfg",
	StatePowerFailure: "PowerFailure",
	StatePowerGood:    "PowerGood",
	StateError:        "Error",
	StateShutdown:     "Shutdown",
	StateSuspend:      "Suspend",
	StateResume:       "Resume",
	StateConfig:       "Config",
	StateReconfig:     "Reconfig",
	StateMaxStates:    "MaxStates",
}

// String returns the state's name, e.g. "Run", or "State(n)" for an unknown code.
func (s State) String() string {
	if n, ok := stateNames[s]; ok {
		return n
	}
	return fmt.Sprintf("State(%d)", uint16(s))
}

// StateInfo holds the ADS and device state returned by ReadState.
type StateInfo struct {
	State       State
	DeviceState uint16
}

// DeviceInfo is a device's self-reported identity, returned by ReadDeviceInfo.
type DeviceInfo struct {
	Name  string // e.g. "Plc30 App"
	Major uint8
	Minor uint8
	Build uint16
}

// Version returns "Major.Minor.Build", e.g. "3.1.4024".
func (d DeviceInfo) Version() string {
	return fmt.Sprintf("%d.%d.%d", d.Major, d.Minor, d.Build)
}
