package fakeplc

import (
	"encoding/binary"

	"github.com/siyka-au/go-ads/v3/ams"
)

// craftSumReadResponse builds a byte response in the [N×(error,length)][data] format.
// errors[i] and dataLengths[i] go in the header section; data is the concatenated payload.
func SumReadResponse(errs []ams.ReturnCode, dataLengths []uint32, data []byte) []byte {
	n := len(errs)
	buf := make([]byte, n*8+len(data))
	for i := 0; i < n; i++ {
		binary.LittleEndian.PutUint32(buf[i*8:], uint32(errs[i]))
		binary.LittleEndian.PutUint32(buf[i*8+4:], dataLengths[i])
	}
	copy(buf[n*8:], data)
	return buf
}
