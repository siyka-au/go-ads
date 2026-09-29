package ads

import (
	"context"
	"errors"
	"fmt"
)

// WriteValue writes value, a Go value of the type ReadValue returns for the
// symbol, to a PLC symbol by name (handle resolved on demand and cached).
// Integer types also take any Go integer in range. A struct takes a map naming
// every member; an array a slice of exactly its element count, nested per
// dimension. Structs and arrays need the datatype table (LoadSymbols).
func (sess *Session) WriteValue(ctx context.Context, symbolName string, value any) error {
	return sess.writeValueRetry(ctx, symbolName, value, 1)
}

func (sess *Session) writeValueRetry(ctx context.Context, symbolName string, value any, retriesLeft int) error {
	gen := sess.epoch()

	symbol, err := sess.getSymbol(ctx, symbolName)
	if err != nil {
		return fmt.Errorf("write to %q: %w", symbolName, err)
	}

	// Snapshot datatypes and handle under lock, then encode without holding the lock.
	// encode reads symbol.Length, DataType and Children, which are set at
	// construction and never mutated, so no lock is needed for them.
	sess.cache.lock.Lock()
	datatypes := sess.cache.datatypes
	handle := symbol.Handle
	sess.cache.lock.Unlock()

	data, err := symbol.encode(value, datatypes)
	if err != nil {
		return fmt.Errorf("write to %q: %w", symbolName, err)
	}

	// Network I/O without lock
	err = sess.client.Load().Write(ctx, uint32(GroupSymbolValueByHandle), handle, data)
	if err != nil {
		// Online-change detection (R-CACHE-009).
		var rc ReturnCode
		if errors.As(err, &rc) {
			sess.handleStaleDetection(rc)
		}
		// If a reconnect happened during our operation, retry once with fresh handles
		sess.waitForReconnect()
		if retriesLeft > 0 && sess.epoch() != gen {
			return sess.writeValueRetry(ctx, symbolName, value, retriesLeft-1)
		}
		return fmt.Errorf("write to %q: %w", symbolName, err)
	}

	// Invalidate cached value so the next ReadValue fetches fresh data.
	// Re-resolve via cache.symbols: the symbol pointer captured at the top of
	// this function may be stranded if loadSymbols swapped the cache during
	// the Write roundtrip. writeValuesRetry uses the same pattern at the
	// per-item commit site; symmetric here.
	sess.cache.lock.Lock()
	if live := sess.cache.symbols[symbolKey(symbolName)]; live != nil {
		live.invalidate()
	}
	sess.cache.lock.Unlock()

	sess.logger.Log(context.Background(), LevelTrace, "wrote to symbol",
		"symbol", symbolName,
		"value", value)
	return nil
}

// invalidate drops the cached value so the next read goes to the PLC. Caller
// holds cache.lock.
func (s *symbol) invalidate() {
	s.Value = nil
	s.ValueParsed = false
}

// ReadValue reads a PLC symbol by name (handle resolved on demand and cached)
// and returns its value as a Go type: see the table in value.go. A struct or
// array needs the datatype table (LoadSymbols) and comes back as
// map[string]any or []any, freshly allocated per call.
func (sess *Session) ReadValue(ctx context.Context, symbolName string) (any, error) {
	return sess.readValueRetry(ctx, symbolName, 1)
}

func (sess *Session) readValueRetry(ctx context.Context, symbolName string, retriesLeft int) (any, error) {
	gen := sess.epoch()

	symbol, err := sess.getSymbol(ctx, symbolName)
	if err != nil {
		return nil, fmt.Errorf("read %q: %w", symbolName, err)
	}

	// Always read from the PLC: a cached value can be stale even moments after
	// it was decoded, because a write elsewhere (or the PLC program) can change
	// it without touching this symbol's cache entry.
	sess.cache.lock.Lock()
	handle := symbol.Handle
	length := symbol.Length
	datatypes := sess.cache.datatypes
	sess.cache.lock.Unlock()

	// Network I/O without lock
	data, err := sess.client.Load().Read(ctx, uint32(GroupSymbolValueByHandle), handle, length)
	if err != nil {
		// Online-change detection (R-CACHE-009).
		var rc ReturnCode
		if errors.As(err, &rc) {
			sess.handleStaleDetection(rc)
		}
		// If a reconnect happened during our operation, retry once with fresh handles
		sess.waitForReconnect()
		if retriesLeft > 0 && sess.epoch() != gen {
			return sess.readValueRetry(ctx, symbolName, retriesLeft-1)
		}
		return nil, fmt.Errorf("read %q: %w", symbolName, err)
	}

	// R-CACHE-009 supplementary detection: the cached symbol.Length disagreeing
	// with the PLC-returned payload length indicates an online change (e.g.
	// operator toggled nProbeA INT↔LREAL). The PLC ships data of the new size,
	// but our cache still has the old size — decode() would fail with
	// "symbol.Length N exceeds data buffer size M" and never surface a
	// ReturnCode, so handleStaleDetection would be bypassed. Detect here
	// (Session-wrapper layer where sess is in scope) and dispatch the
	// configured strategy. Returns a ReturnCode-typed error so chained
	// errors.As on the caller side keeps matching.
	if data != nil && length > 0 && uint32(len(data)) != length {
		sess.handleStaleDetection(ReturnCodeDeviceInvalidSize)
		return nil, fmt.Errorf("read %q: %w", symbolName, ReturnCodeDeviceInvalidSize)
	}

	// decode() mutates symbol fields (Value, Valid, etc.) so it must
	// run under lock to avoid racing with handleNotification.
	sess.cache.lock.Lock()
	if _, err := symbol.decode(data, 0, datatypes); err != nil {
		sess.cache.lock.Unlock()
		return nil, fmt.Errorf("read %q: decode failed: %w", symbolName, err)
	}
	value := copyData(symbol.Value)
	sess.cache.lock.Unlock()

	sess.logger.Log(context.Background(), LevelTrace, "Read from symbol",
		"symbol", symbolName,
		"value", value)

	return value, nil
}

// symbolSumAddress returns the group and offset for a symbol inside a sum command.
// Prefers handle-based addressing, because direct group/offset with process image
// groups does not work inside sum reads on some TwinCAT versions even with correct
// offsets; falls back to direct when no handle exists yet. Caller MUST hold
// cache.lock -- it reads sym.Handle.
func symbolSumAddress(sym *symbol) (group, offset uint32) {
	if sym.Handle != 0 {
		return uint32(GroupSymbolValueByHandle), sym.Handle
	}
	if sym.Group != 0 {
		absOffset := sym.Offset
		for p := sym.Parent; p != nil; p = p.Parent {
			absOffset += p.Offset
		}
		return sym.Group, absOffset
	}
	// Handle and Group both zero — shouldn't happen in normal operation.
	// Return handle-based addressing; PLC will return an error for handle 0.
	return uint32(GroupSymbolValueByHandle), sym.Handle
}

// ReadValues reads several symbols in one round-trip, returning a map of name
// to value, as ReadValue returns it, for those that succeeded. Failures are
// named in a *BatchError (errors.As), and the map stays usable -- one absent
// symbol in forty leaves thirty-nine present. Any other error means the
// transport failed and no outcome is known. Reading none returns nil, nil.
func (sess *Session) ReadValues(ctx context.Context, names []string) (map[string]any, error) {
	return sess.readValuesRetry(ctx, names, 1)
}

func (sess *Session) readValuesRetry(ctx context.Context, names []string, retriesLeft int) (map[string]any, error) {
	if len(names) == 0 {
		return nil, nil
	}

	gen := sess.epoch()

	// Resolve symbols and build SumRead requests
	type symbolInfo struct {
		name   string
		symbol *symbol
	}
	var infos []symbolInfo
	var requests []SumReadRequest
	// failed accumulates one entry per symbol that yields no value, from every
	// site that can drop one: resolve failure here, then per-item PLC code,
	// cache swap and parse failure in the decode loop below. All of them ride
	// out in a single *BatchError so a dropped name is never silent.
	var failed []BatchItemError

	for _, name := range names {
		symbol, err := sess.getSymbol(ctx, name)
		if err != nil {
			sess.logger.Error("error getting symbol for batch read", "error", err, "symbol", name)
			failed = append(failed, BatchItemError{
				Symbol:  name,
				Skipped: fmt.Errorf("%w: %w", ErrBatchSymbolUnresolved, err),
			})
			continue
		}
		// Snapshot Handle under cache.lock — autoReload's zeroOldSymbolHandles
		// writes symbol.Handle under cache.lock, so racing reads here without
		// the lock would trip -race and could see an in-flight zero. Length is
		// write-once at construction; snapshot it together for symmetry.
		sess.cache.lock.Lock()
		group, offset := symbolSumAddress(symbol)
		length := symbol.Length
		sess.cache.lock.Unlock()
		infos = append(infos, symbolInfo{name: name, symbol: symbol})
		requests = append(requests, SumReadRequest{Group: group, Offset: offset, Length: length})
	}

	if len(requests) == 0 {
		// Every name failed to resolve. Same shape as any other all-failed
		// batch: the contract must not depend on where in the call the items
		// died, nor on how many were asked for.
		return nil, newBatchError("read", len(names), 0, failed)
	}

	results, err := sess.client.Load().SumRead(ctx, requests)
	if err != nil {
		// If a reconnect happened during our operation, retry once with fresh handles
		sess.waitForReconnect()
		if retriesLeft > 0 && sess.epoch() != gen {
			return sess.readValuesRetry(ctx, names, retriesLeft-1)
		}
		return nil, fmt.Errorf("batch read failed: %w", err)
	}

	// R-CACHE-009: fire online-change detection for first stale per-item code.
	// Once-per-batch semantics avoid callback amplification when N items in
	// the same response carry the same stale code (R-SES-011 "once per
	// detection").
	for _, r := range results {
		if r.Error == ReturnCodeNoErrors {
			continue
		}
		if stale, _ := detectStaleCache(r.Error); stale {
			sess.handleStaleDetection(r.Error)
			break
		}
	}

	values := make(map[string]any, len(results))
	sess.cache.lock.Lock()
	defer sess.cache.lock.Unlock()

	for i, result := range results {
		if i >= len(infos) {
			// More results than requests: the response cannot be attributed to
			// symbols, so stop rather than guess.
			break
		}
		if result.Error != ReturnCodeNoErrors {
			sess.logger.Warn("symbol read error in batch",
				"symbol", infos[i].name,
				"errorCode", uint32(result.Error))
			failed = append(failed, BatchItemError{Symbol: infos[i].name, Error: result.Error})
			continue
		}
		// Re-resolve via cache.symbols: infos[i].symbol may be stranded if
		// loadSymbols swapped the cache during the SumRead roundtrip. Parse
		// + Value mutation must target the live entry, otherwise
		// ReadValue serves a stale value while decode silently writes the orphan.
		live := sess.cache.symbols[symbolKey(infos[i].name)]
		if live == nil {
			sess.logger.Warn("batch read result for symbol no longer in cache; skipping",
				"symbol", infos[i].name)
			failed = append(failed, BatchItemError{Symbol: infos[i].name, Skipped: ErrBatchSymbolVanished})
			continue
		}
		value, err := live.decode(result.Data, 0, sess.cache.datatypes)
		if err != nil {
			sess.logger.Error("error parsing symbol in batch read", "error", err, "symbol", infos[i].name)
			failed = append(failed, BatchItemError{
				Symbol:  infos[i].name,
				Skipped: fmt.Errorf("%w: %w", ErrBatchValueUnparsable, err),
			})
			continue
		}
		values[infos[i].name] = copyData(value)
	}

	// Fewer results than requests: the tail has no verdict at all, which is
	// still a dropped name from the caller's side.
	for i := len(results); i < len(infos); i++ {
		sess.logger.Warn("no batch read result for symbol", "symbol", infos[i].name)
		failed = append(failed, BatchItemError{Symbol: infos[i].name, Skipped: ErrBatchNoResult})
	}

	return values, newBatchError("read", len(names), len(values), failed)
}

// WriteValues writes several symbols in one round-trip, each value as
// WriteValue takes it, returning a map of name to per-symbol code for those
// that reached the PLC.
//
// Do not read success from the map alone: NoErrors is 0, so a symbol that was
// never written is indistinguishable from one that succeeded. Failures are named
// in a *BatchError (errors.As), which says whether the PLC rejected it or the
// library never sent it; any other error means the transport failed and no
// outcome is known. Writing none returns nil, nil.
func (sess *Session) WriteValues(ctx context.Context, values map[string]any) (map[string]ReturnCode, error) {
	return sess.writeValuesRetry(ctx, values, 1)
}

func (sess *Session) writeValuesRetry(ctx context.Context, values map[string]any, retriesLeft int) (map[string]ReturnCode, error) {
	if len(values) == 0 {
		return nil, nil
	}

	gen := sess.epoch()

	// Snapshot datatypes under lock
	sess.cache.lock.Lock()
	datatypes := sess.cache.datatypes
	sess.cache.lock.Unlock()

	type symbolInfo struct {
		name   string
		symbol *symbol
	}
	var infos []symbolInfo
	var requests []SumWriteRequest
	// failed accumulates one entry per symbol that was not written: the two
	// pre-flight drops here, plus the per-item PLC codes below. Without it a
	// dropped write is absent from codes and reads as the zero value, i.e. as
	// success — the one finding in this API that can move a physical output.
	var failed []BatchItemError

	for name, value := range values {
		symbol, err := sess.getSymbol(ctx, name)
		if err != nil {
			sess.logger.Error("error getting symbol for batch write", "error", err, "symbol", name)
			failed = append(failed, BatchItemError{
				Symbol:  name,
				Skipped: fmt.Errorf("%w: %w", ErrBatchSymbolUnresolved, err),
			})
			continue
		}

		data, err := symbol.encode(value, datatypes)
		if err != nil {
			sess.logger.Error("error serializing symbol for batch write", "error", err, "symbol", name)
			failed = append(failed, BatchItemError{
				Symbol:  name,
				Skipped: fmt.Errorf("%w: %w", ErrBatchValueUnserializable, err),
			})
			continue
		}

		// Snapshot Handle under cache.lock — see readValuesRetry for rationale.
		sess.cache.lock.Lock()
		group, offset := symbolSumAddress(symbol)
		sess.cache.lock.Unlock()
		req := SumWriteRequest{Group: group, Offset: offset, Data: data}

		infos = append(infos, symbolInfo{name: name, symbol: symbol})
		requests = append(requests, req)
	}

	if len(requests) == 0 {
		// Nothing reached the wire — same shape as any other all-failed batch.
		return nil, newBatchError("write", len(values), 0, sortBatchItems(failed))
	}

	results, err := sess.client.Load().SumWrite(ctx, requests)
	if err != nil {
		// If a reconnect happened during our operation, retry once with fresh handles
		sess.waitForReconnect()
		if retriesLeft > 0 && sess.epoch() != gen {
			return sess.writeValuesRetry(ctx, values, retriesLeft-1)
		}
		return nil, fmt.Errorf("batch write failed: %w", err)
	}

	// R-CACHE-009: fire online-change detection for first stale per-item code.
	// Once-per-batch semantics — see readValuesRetry for rationale.
	for _, r := range results {
		if r.Error == ReturnCodeNoErrors {
			continue
		}
		if stale, _ := detectStaleCache(r.Error); stale {
			sess.handleStaleDetection(r.Error)
			break
		}
	}

	codes := make(map[string]ReturnCode, len(results))
	succeeded := 0
	sess.cache.lock.Lock()
	for i, result := range results {
		if i >= len(infos) {
			break // more results than requests; cannot attribute them
		}
		codes[infos[i].name] = result.Error
		if result.Error == ReturnCodeNoErrors {
			succeeded++
		} else {
			failed = append(failed, BatchItemError{Symbol: infos[i].name, Error: result.Error})
		}
		// Invalidate cached value for successful writes so next read is fresh.
		// Re-resolve via cache.symbols: infos[i].symbol may be stranded if
		// loadSymbols swapped during the SumWrite roundtrip; clearing the
		// orphan would leave the live entry showing stale Value.
		if result.Error == ReturnCodeNoErrors {
			if live := sess.cache.symbols[symbolKey(infos[i].name)]; live != nil {
				live.invalidate()
			}
		}
	}
	sess.cache.lock.Unlock()

	// Fewer results than requests: those writes have no verdict at all.
	for i := len(results); i < len(infos); i++ {
		sess.logger.Warn("no batch write result for symbol", "symbol", infos[i].name)
		failed = append(failed, BatchItemError{Symbol: infos[i].name, Skipped: ErrBatchNoResult})
	}

	return codes, newBatchError("write", len(values), succeeded, sortBatchItems(failed))
}
