package ams

// ADST_ data type IDs from the ADS protocol (ADSDATATYPEID enum).
// The PLC sends these numeric codes in symbolEntry.DataType to identify
// the base type of a variable. Works on both TwinCAT 2 and TwinCAT 3.
// Source: Beckhoff TC2_Utilities ADSDATATYPEID
// DataType identifies a primitive IEC 61131-3 data type by the numeric
// code Beckhoff publishes in symbolEntry.DataType. Works on both TwinCAT 2
// and TwinCAT 3. The companion DataTypeBigType code is the catch-all for
// composite types (structs, enums, type aliases, arrays).
type DataType uint32

const (
	DataTypeVoid    DataType = 0
	DataTypeInt16   DataType = 2  // INT
	DataTypeInt32   DataType = 3  // DINT
	DataTypeReal32  DataType = 4  // REAL
	DataTypeReal64  DataType = 5  // LREAL
	DataTypeInt8    DataType = 16 // SINT
	DataTypeUint8   DataType = 17 // USINT/BYTE
	DataTypeUint16  DataType = 18 // UINT/WORD
	DataTypeUint32  DataType = 19 // UDINT/DWORD
	DataTypeInt64   DataType = 20 // LINT
	DataTypeUint64  DataType = 21 // ULINT/LWORD
	DataTypeString  DataType = 30 // STRING
	DataTypeWString DataType = 31 // WSTRING
	DataTypeBool    DataType = 33 // BOOL
	// DataTypeBigType is the PLC's catch-all for composite/user-defined types:
	// structs, enums, type aliases, arrays. PLC sends this when the leaf does
	// not have a primitive ADST_ code (e.g. TC2 always reports enums this way;
	// TC3 sometimes reports the underlying primitive but falls back to BIGTYPE
	// for opaque types). Structs are caught earlier by the parse path's
	// Children branch, so a symbol reaching inferBaseType with BIGTYPE + a
	// 1/2/4/8 byte size is enum-like — safe to interpret as signed integer.
	DataTypeBigType DataType = 65
)

// IECName returns the IEC 61131-3 type name for a primitive code, e.g. "DINT"
// for DataTypeInt32. Returns "" for unknown or composite types.
func (code DataType) IECName() string {
	switch code {
	case DataTypeBool:
		return "BOOL"
	case DataTypeInt8:
		return "SINT"
	case DataTypeUint8:
		return "USINT"
	case DataTypeInt16:
		return "INT"
	case DataTypeUint16:
		return "UINT"
	case DataTypeInt32:
		return "DINT"
	case DataTypeUint32:
		return "UDINT"
	case DataTypeReal32:
		return "REAL"
	case DataTypeReal64:
		return "LREAL"
	case DataTypeInt64:
		return "LINT"
	case DataTypeUint64:
		return "ULINT"
	case DataTypeString:
		return "STRING"
	case DataTypeWString:
		return "WSTRING"
	default:
		return ""
	}
}

// Size returns the fixed width in bytes of a scalar ADST_ code, or 0
// when the width is not fixed (STRING/WSTRING) or the code is not scalar.
// Used to tell an aggregate apart from a scalar: an array reports its element's
// ADST_ code alongside the whole array's length, so a symbol whose length
// disagrees with its base type's width is not the scalar the code claims.
func (code DataType) Size() uint32 {
	switch code {
	case DataTypeBool, DataTypeInt8, DataTypeUint8:
		return 1
	case DataTypeInt16, DataTypeUint16:
		return 2
	case DataTypeInt32, DataTypeUint32, DataTypeReal32:
		return 4
	case DataTypeInt64, DataTypeUint64, DataTypeReal64:
		return 8
	default:
		return 0
	}
}
