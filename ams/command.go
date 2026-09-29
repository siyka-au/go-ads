package ams

// Command is the ADS command opcode carried in the AMS header (see
// Beckhoff ADS specification for the wire format).
type Command uint16

const (
	CommandInvalid Command = iota
	CommandReadDeviceInfo
	CommandRead
	CommandWrite
	CommandReadState
	CommandWriteControl
	CommandAddDeviceNotification
	CommandDeleteDeviceNotification
	CommandDeviceNotification
	CommandReadWrite
)
