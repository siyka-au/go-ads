package ams

// SymbolFlag represents bits in the symbol flags field returned by INFOBYNAMEEX (0xF009)
// and the bulk symbol upload. These flags control how extended symbol info is parsed
// and which notification modes are available.
type SymbolFlag uint32

const (
	// SymbolFlagPersistent indicates the symbol value survives PLC restarts.
	SymbolFlagPersistent SymbolFlag = 0x0001
	// SymbolFlagBitValue indicates the symbol is a single bit within a byte.
	SymbolFlagBitValue SymbolFlag = 0x0002
	// SymbolFlagReferenceTo indicates the symbol is a reference/pointer.
	SymbolFlagReferenceTo SymbolFlag = 0x0004
	// SymbolFlagTypeGuid indicates a 16-byte type GUID follows after the name/type/comment strings.
	SymbolFlagTypeGuid SymbolFlag = 0x0008
	// SymbolFlagTComObj indicates the symbol is a TcCOM object.
	SymbolFlagTComObj SymbolFlag = 0x0010
	// SymbolFlagReadOnly indicates the symbol is read-only.
	SymbolFlagReadOnly SymbolFlag = 0x0020
	// SymbolFlagContextMask extracts the PLC task index from bits 8-11: non-zero
	// binds the variable to a task and enables the InContext modes, zero means they
	// are rejected (0x070B on TC3) or silently ignored (TC2). Only variables local
	// to a PROGRAM POU in a multi-task project are non-zero.
	SymbolFlagContextMask SymbolFlag = 0x0F00
	// SymbolFlagAttributes indicates attribute key-value pairs follow after the type GUID.
	SymbolFlagAttributes SymbolFlag = 0x1000
	// SymbolFlagExtendedFlags indicates additional extended flags are present.
	SymbolFlagExtendedFlags SymbolFlag = 0x8000
)

// ContextMask extracts the PLC task context index from symbol flags (bits 8-11).
// Returns 0 if the symbol is not bound to a specific task.
func (f SymbolFlag) ContextMask() uint8 {
	return uint8((f >> 8) & 0x0F)
}

// Has returns true if the flag set contains the given flag(s).
func (f SymbolFlag) Has(flag SymbolFlag) bool {
	return f&flag == flag
}

type SymbolUploadInfo struct {
	SymbolCount    uint32
	SymbolLength   uint32
	DataTypeCount  uint32
	DataTypeLength uint32
	ExtraCount     uint32
	ExtraLength    uint32
}
