package ams

// Port is a well-known AMS port number for a TwinCAT service. PortR0PlcTc3
// (851) is the typical PLC runtime port for TwinCAT 3.
type Port uint16

const (
	PortLogger    Port = 100
	PortR0Rtime   Port = 200
	PortR0Trace   Port = (PortR0Rtime + 90)
	PortR0Io      Port = 300
	PortR0Sps     Port = 400
	PortR0Nc      Port = 500
	PortR0Isg     Port = 550
	PortR0Pcs     Port = 600
	PortR0Plc     Port = 801
	PortR0PlcRts1 Port = 801
	PortR0PlcRts2 Port = 811
	PortR0PlcRts3 Port = 821
	PortR0PlcRts4 Port = 831
	PortR0PlcTc3  Port = 851
	// PortSystemService is the TwinCAT system service. Unlike the runtime ports it
	// stays up while the system is in CONFIG — measured on TC3.1.4024: every request
	// to 851 came back with AMS ErrorCode 6 (target port not found), while 10000
	// answered ADSState=15 (CONFIG). So it is the only place to ask what state the
	// system is actually in.
	PortSystemService Port = 10000
)
