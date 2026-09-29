package ads

// ADST_ data type IDs from the ADS protocol (ADSDATATYPEID enum).
// The PLC sends these numeric codes in symbolEntry.DataType to identify
// the base type of a variable. Works on both TwinCAT 2 and TwinCAT 3.
// Source: Beckhoff TC2_Utilities ADSDATATYPEID
// ADSDataType identifies a primitive IEC 61131-3 data type by the numeric
// code Beckhoff publishes in symbolEntry.DataType. Works on both TwinCAT 2
// and TwinCAT 3. The companion ADSTBigType code is the catch-all for
// composite types (structs, enums, type aliases, arrays).
type ADSDataType uint32

const (
	ADSTVoid    ADSDataType = 0
	ADSTInt16   ADSDataType = 2  // INT
	ADSTInt32   ADSDataType = 3  // DINT
	ADSTReal32  ADSDataType = 4  // REAL
	ADSTReal64  ADSDataType = 5  // LREAL
	ADSTInt8    ADSDataType = 16 // SINT
	ADSTUint8   ADSDataType = 17 // USINT/BYTE
	ADSTUint16  ADSDataType = 18 // UINT/WORD
	ADSTUint32  ADSDataType = 19 // UDINT/DWORD
	ADSTInt64   ADSDataType = 20 // LINT
	ADSTUint64  ADSDataType = 21 // ULINT/LWORD
	ADSTString  ADSDataType = 30 // STRING
	ADSTWString ADSDataType = 31 // WSTRING
	ADSTBool    ADSDataType = 33 // BOOL
	// ADSTBigType is the PLC's catch-all for composite/user-defined types:
	// structs, enums, type aliases, arrays. PLC sends this when the leaf does
	// not have a primitive ADST_ code (e.g. TC2 always reports enums this way;
	// TC3 sometimes reports the underlying primitive but falls back to BIGTYPE
	// for opaque types). Structs are caught earlier by the parse path's
	// Children branch, so a symbol reaching inferBaseType with BIGTYPE + a
	// 1/2/4/8 byte size is enum-like — safe to interpret as signed integer.
	ADSTBigType ADSDataType = 65
)

// adsTypeToString maps an ADST_ numeric type code to the corresponding
// IEC 61131-3 type name used in the parse switch statement.
// Returns "" for unknown or composite types.
func adsTypeToString(code ADSDataType) string {
	switch code {
	case ADSTBool:
		return "BOOL"
	case ADSTInt8:
		return "SINT"
	case ADSTUint8:
		return "USINT"
	case ADSTInt16:
		return "INT"
	case ADSTUint16:
		return "UINT"
	case ADSTInt32:
		return "DINT"
	case ADSTUint32:
		return "UDINT"
	case ADSTReal32:
		return "REAL"
	case ADSTReal64:
		return "LREAL"
	case ADSTInt64:
		return "LINT"
	case ADSTUint64:
		return "ULINT"
	case ADSTString:
		return "STRING"
	case ADSTWString:
		return "WSTRING"
	default:
		return ""
	}
}

// adsTypeWidth returns the fixed width in bytes of a scalar ADST_ code, or 0
// when the width is not fixed (STRING/WSTRING) or the code is not scalar.
// Used to tell an aggregate apart from a scalar: an array reports its element's
// ADST_ code alongside the whole array's length, so a symbol whose length
// disagrees with its base type's width is not the scalar the code claims.
func adsTypeWidth(code ADSDataType) uint32 {
	switch code {
	case ADSTBool, ADSTInt8, ADSTUint8:
		return 1
	case ADSTInt16, ADSTUint16:
		return 2
	case ADSTInt32, ADSTUint32, ADSTReal32:
		return 4
	case ADSTInt64, ADSTUint64, ADSTReal64:
		return 8
	default:
		return 0
	}
}
