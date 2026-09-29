package ads

// Group is an ADS index group identifier. The high-byte hex values address
// reserved system tables (symbol cache, process image, device data) per
// Beckhoff's published index-group reservations.
type Group uint32

const (
	GroupSymbolTab   Group = 0xf000
	GroupSymbolName  Group = 0xf001
	GroupSymbolValue Group = 0xf002

	GroupSymbolHandleByName  Group = 0xF003
	GroupSymbolValueByName   Group = 0xF004
	GroupSymbolValueByHandle Group = 0xF005
	GroupSymbolReleaseHandle Group = 0xF006
	GroupSymbolInfoByName    Group = 0xF007
	GroupSymbolVersion       Group = 0xF008
	GroupSymbolInfoByNameEx  Group = 0xF009

	GroupSymbolDownload       Group = 0xF00A
	GroupSymbolUpload         Group = 0xF00B
	GroupSymbolUploadInfo     Group = 0xF00C
	GroupSymbolDownload2      Group = 0xF00D
	GroupSymbolDataTypeUpload Group = 0xF00E
	GroupSymbolUploadInfo2    Group = 0xF00F

	GroupSymbolNotification Group = 0xF010 // notification of named handle

	GroupSumupRead                     Group = 0xF080
	GroupSumupWrite                    Group = 0xF081
	GroupSumupReadWrite                Group = 0xF082
	GroupSumupReadEx                   Group = 0xF083
	GroupSumupReadEx2                  Group = 0xF084
	GroupSumupAddDeviceNotification    Group = 0xF085
	GroupSumupDeleteDeviceNotification Group = 0xF086

	GroupIoImageRwib   Group = 0xF020 // read/write input byte(s)
	GroupIoImageRwix   Group = 0xF021 // read/write input bit
	GroupIoImageRisize Group = 0xF025 // read input size (in byte)
	GroupIoImageRwob   Group = 0xF030 // read/write output byte(s)
	GroupIoImageRwox   Group = 0xF031 // read/write output bit
	GroupIoImageCleari Group = 0xF040 // write inputs to null
	GroupIoImageClearo Group = 0xF050 // write outputs to null
	GroupIoImageRwiob  Group = 0xF060 // read input and write output byte(s)

	GroupDeviceData Group = 0xF100 // state, name, etc...
)

// Offset is the index-offset half of an ADS group/offset address; the
// interpretation depends on the index group (symbol-relative byte offset,
// process-image bit/byte index, device-data field, ...).
type Offset uint32

const (
	OffsetDeviceDataADSState    Offset = 0x0000 // ads state of device
	OffsetDeviceDataDeviceState Offset = 0x0002 // device state
)
