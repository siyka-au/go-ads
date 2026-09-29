package ads

// ADSState is the run-time state of an ADS device (typically the PLC runtime),
// as reported by Client.ReadState.
type ADSState uint16

const (
	ADSStateInvalid      ADSState = 0
	ADSStateIdle         ADSState = 1
	ADSStateReset        ADSState = 2
	ADSStateInit         ADSState = 3
	ADSStateStart        ADSState = 4
	ADSStateRun          ADSState = 5
	ADSStateStop         ADSState = 6
	ADSStateSaveCfg      ADSState = 7
	ADSStateLoadCfg      ADSState = 8
	ADSStatePowerFailure ADSState = 9
	ADSStatePowerGood    ADSState = 10
	ADSStateError        ADSState = 11
	ADSStateShutdown     ADSState = 12
	ADSStateSuspend      ADSState = 13
	ADSStateResume       ADSState = 14
	ADSStateConfig       ADSState = 15 // System Is In Config Mode
	ADSStateReconfig     ADSState = 16 // System Should Restart In Config Mode
	ADSStateMaxStates    ADSState = 255
)

// States holds the ADS and device state returned by ReadState.
type States struct {
	ADSState    ADSState
	DeviceState uint16
}

// DeviceInfo is the PLC's self-reported identity (returned by ReadDeviceInfo).
// DeviceName is a 16-byte null-padded ASCII field; trim at the first null byte
// for display.
type DeviceInfo struct {
	Major      uint8
	Minor      uint8
	Version    uint16
	DeviceName [16]byte
}
