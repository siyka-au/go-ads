package ads

// CommandID is the ADS command opcode carried in the AMS header (see
// Beckhoff ADS specification for the wire format).
type CommandID uint16

const (
	CommandIDInValueID CommandID = iota
	CommandIDReadDeviceInfo
	CommandIDRead
	CommandIDWrite
	CommandIDReadState
	CommandIDWriteControl
	CommandIDAddDeviceNotification
	CommandIDDeleteDeviceNotification
	CommandIDDeviceNotification
	CommandIDReadWrite
)
