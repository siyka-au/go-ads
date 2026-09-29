package ads

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"time"

	"github.com/siyka-au/go-ads/v3/internal/symtab"

	"github.com/siyka-au/go-ads/v3/ams"
)

// sleepCtx sleeps for d or returns early on ctx cancellation.
// Returns ctx.Err() on cancellation, nil otherwise. Non-positive d
// returns nil immediately without checking ctx (matches time.Sleep
// semantics).
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// isChunkedDownloadUnsupportedErr reports whether err from the first
// chunked-download Read indicates the PLC genuinely does not implement
// offset-based reads on the upload groups (TwinCAT 2 behaviour).
// Conservative: only ADS-level rejections that name the service / offset
// as the cause count. Transport, context, marshaling, and arbitrary
// device errors are deliberately excluded so a transient failure cannot
// poison the session-wide ChunkedDownloadSupported flag.
func isChunkedDownloadUnsupportedErr(err error) bool {
	var rc ams.ReturnCode
	if !errors.As(err, &rc) {
		return false
	}
	switch rc {
	case ams.ReturnCodeDeviceServiceNotSupported, // 0x701
		ams.ReturnCodeDeviceInvalidOffset: // 0x703
		return true
	default:
		return false
	}
}

// downloadInChunks reads a large blob in chunkSize pieces via the Read command's
// offset, with an optional inter-chunk delay, so the loaders do not overwhelm the
// PLC's real-time loop. Returns immediately when chunking is already known to be
// unsupported, so the caller takes the single-request fallback.
func (c *Client) DownloadInChunks(ctx context.Context, group uint32, totalLength uint32, chunkSize uint32, delay time.Duration) ([]byte, error) {
	if totalLength == 0 {
		return []byte{}, nil
	}
	if c.capabilities.ChunkedDownloadCheckedLoad() && !c.capabilities.ChunkedDownloadSupportedLoad() {
		return nil, fmt.Errorf("chunked download not supported by this PLC")
	}
	const maxDownloadSize = 64 * 1024 * 1024 // 64 MB sanity limit
	if totalLength > maxDownloadSize {
		return nil, fmt.Errorf("download size %d exceeds sanity limit of %d bytes", totalLength, maxDownloadSize)
	}
	result := make([]byte, 0, totalLength)
	var offset uint32
	for offset < totalLength {
		remaining := totalLength - offset
		readLen := chunkSize
		if remaining < readLen {
			readLen = remaining
		}
		chunk, err := c.Read(ctx, group, offset, readLen)
		if err != nil {
			// Flip the capability only on an unambiguous "not implemented", and only
			// on the first chunk: a transient error would otherwise poison the
			// session-wide flag and send every later call down the fallback.
			if offset == 0 && !c.capabilities.ChunkedDownloadCheckedLoad() && isChunkedDownloadUnsupportedErr(err) {
				c.capabilities.ChunkedDownloadSupportedStore(false)
				c.capabilities.ChunkedDownloadCheckedStore(true)
			}
			return nil, fmt.Errorf("chunk read at offset %d failed: %w", offset, err)
		}
		if len(chunk) == 0 {
			return nil, fmt.Errorf("chunk read at offset %d returned empty response", offset)
		}
		result = append(result, chunk...)
		offset += uint32(len(chunk))
		if offset < totalLength && delay > 0 {
			if err := sleepCtx(ctx, delay); err != nil {
				return nil, err
			}
		}
	}
	if !c.capabilities.ChunkedDownloadCheckedLoad() {
		c.capabilities.ChunkedDownloadSupportedStore(true)
		c.capabilities.ChunkedDownloadCheckedStore(true)
	}
	if uint32(len(result)) != totalLength {
		return nil, fmt.Errorf("downloaded %d bytes but expected %d", len(result), totalLength)
	}
	return result, nil
}

// getSymbolInfoByName queries a single symbol's metadata from the PLC
// using GroupSymbolInfoByNameEx (0xF009).
// GetSymbolInfoByName resolves a single symbol on the PLC and returns a
// populated symbol with Group, Offset, Length, DataType, etc. Does NOT
// populate Children (struct / array children require full discovery via
// LoadSymbols / LoadSymbolList + LoadDataTypes). This is a raw RPC and bypasses the symbol cache; the caller is responsible for decoding the response.
func (c *Client) GetSymbolInfoByName(ctx context.Context, symbolName string) (ams.SymbolInfo, error) {
	resp, err := c.WriteRead(
		ctx,
		uint32(ams.GroupSymbolInfoByNameEx),
		0,
		2048,
		append([]byte(symbolName), 0),
	)
	if err != nil {
		return ams.SymbolInfo{}, fmt.Errorf("GetSymbolInfoByName(%s) failed: %w", symbolName, err)
	}
	info, err := symtab.ParseSymbolInfo(resp)
	if err != nil {
		return ams.SymbolInfo{}, fmt.Errorf("symbol info for %s: %w", symbolName, err)
	}
	return info, nil
}

// GetHandleByName resolves a symbol name to its PLC-side handle. Wire RPC;
// no cache. The handle is valid until the TCP transport drops or until
// the caller releases it via ReleaseHandle.
func (c *Client) GetHandleByName(ctx context.Context, symbolName string) (uint32, error) {
	resp, err := c.WriteRead(ctx, uint32(ams.GroupSymbolHandleByName), 0, 4, append([]byte(symbolName), 0))
	if err != nil {
		return 0, fmt.Errorf("getting handle for %q: %w", symbolName, err)
	}
	if len(resp) < 4 {
		return 0, fmt.Errorf("getting handle for %q: response too short (%d bytes)", symbolName, len(resp))
	}
	return binary.LittleEndian.Uint32(resp), nil
}

// GetSymbolUploadInfo reads the symbol-table size header from the PLC.
// Tries extended info (0xF00F, 24 bytes) first; falls back to basic info
// (0xF00C, 16 bytes) for older PLCs. Used by LoadSymbols / LoadSymbolList /
// LoadDataTypes to size the subsequent download. This is a raw RPC and bypasses the symbol cache; the caller is responsible for decoding the response.
func (c *Client) GetSymbolUploadInfo(ctx context.Context) (ams.SymbolUploadInfo, error) {
	var uploadInfo ams.SymbolUploadInfo
	res, err := c.Read(ctx, uint32(ams.GroupSymbolUploadInfo2), 0, 24)
	if err != nil {
		c.logger.Debug("GroupSymbolUploadInfo2 not supported, falling back to GroupSymbolUploadInfo", "error", err)
		res, err = c.Read(ctx, uint32(ams.GroupSymbolUploadInfo), 0, 16)
		if err != nil {
			return uploadInfo, fmt.Errorf("GetSymbolUploadInfo failed: %w", err)
		}
	}
	buff := bytes.NewBuffer(res)
	if err := binary.Read(buff, binary.LittleEndian, &uploadInfo.SymbolCount); err != nil {
		return uploadInfo, fmt.Errorf("reading SymbolCount: %w", err)
	}
	if err := binary.Read(buff, binary.LittleEndian, &uploadInfo.SymbolLength); err != nil {
		return uploadInfo, fmt.Errorf("reading SymbolLength: %w", err)
	}
	if err := binary.Read(buff, binary.LittleEndian, &uploadInfo.DataTypeCount); err != nil {
		return uploadInfo, fmt.Errorf("reading DataTypeCount: %w", err)
	}
	if err := binary.Read(buff, binary.LittleEndian, &uploadInfo.DataTypeLength); err != nil {
		return uploadInfo, fmt.Errorf("reading DataTypeLength: %w", err)
	}
	if buff.Len() >= 8 {
		if err := binary.Read(buff, binary.LittleEndian, &uploadInfo.ExtraCount); err != nil {
			return uploadInfo, fmt.Errorf("reading ExtraCount: %w", err)
		}
		if err := binary.Read(buff, binary.LittleEndian, &uploadInfo.ExtraLength); err != nil {
			return uploadInfo, fmt.Errorf("reading ExtraLength: %w", err)
		}
	}
	return uploadInfo, nil
}

// DownloadSymbolList downloads the raw symbol-table bytes (group 0xF00B).
// Caller decodes per parseUploadSymbolInfoSymbols. This is a raw RPC and bypasses the symbol cache; the caller is responsible for decoding the response.
func (c *Client) DownloadSymbolList(ctx context.Context, length uint32) ([]byte, error) {
	res, err := c.Read(ctx, uint32(ams.GroupSymbolUpload), 0, length)
	if err != nil {
		return nil, fmt.Errorf("DownloadSymbolList failed: %w", err)
	}
	return res, nil
}

// DownloadDataTypes downloads the raw datatype-table bytes (group 0xF00E).
// Caller decodes via parseUploadSymbolInfoDataTypes. This is a raw RPC and bypasses the symbol cache; the caller is responsible for decoding the response.
func (c *Client) DownloadDataTypes(ctx context.Context, length uint32) ([]byte, error) {
	res, err := c.Read(ctx, uint32(ams.GroupSymbolDataTypeUpload), 0x0, length)
	if err != nil {
		return nil, fmt.Errorf("DownloadDataTypes failed: %w", err)
	}
	return res, nil
}

// GetSymbolVersion reads the current PLC symbol version (single byte).
// Increments on online-change or download. This is a raw RPC and bypasses the symbol cache; the caller is responsible for decoding the response.
func (c *Client) GetSymbolVersion(ctx context.Context) (uint8, error) {
	data, err := c.Read(ctx, uint32(ams.GroupSymbolVersion), 0, 1)
	if err != nil {
		return 0, fmt.Errorf("failed to read symbol version: %w", err)
	}
	if len(data) < 1 {
		return 0, fmt.Errorf("symbol version response too short: %d bytes", len(data))
	}
	return data[0], nil
}
