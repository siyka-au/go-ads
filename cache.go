package ads

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/siyka-au/go-ads/v3/internal/symtab"
)

// zeroOldSymbolHandles invalidates each symbol in the map: Handle=0 forces
// re-resolution, defending against the PLC reusing a handle for a different
// symbol, and clearing the cached value stops a view of it showing
// pre-disconnect data. Nil-safe.
func zeroOldSymbolHandles(m map[string]*symtab.Symbol) {
	for _, s := range m {
		if s != nil {
			s.Handle = 0
			s.Value = nil
			s.Valid = false
			s.ValueParsed = false
			s.LastUpdateTime = time.Time{}
		}
	}
}

// loadSymbols loads symbol table and datatypes from the PLC, and saves the symbol version.
func (sess *Session) loadSymbols(ctx context.Context) error {
	c := sess.client.Load()
	// Read and store symbol version
	version, err := c.GetSymbolVersion(ctx)
	if err != nil {
		sess.logger.Warn("failed to read symbol version, continuing with symbol load", "error", err)
	} else {
		sess.cache.lock.Lock()
		sess.cache.symbolVersion = version
		sess.cache.lock.Unlock()
	}

	res, err := c.GetSymbolUploadInfo(ctx)
	if err != nil {
		return fmt.Errorf("failed to get symbol upload info: %w", err)
	}
	datatypesResponse, err := c.DownloadDataTypes(ctx, res.DataTypeLength)
	if err != nil {
		return fmt.Errorf("failed to upload datatypes: %w", err)
	}
	datatypes, err := symtab.ParseDataTypes(datatypesResponse, sess.logger)
	if err != nil {
		return fmt.Errorf("failed to parse datatypes: %w", err)
	}
	symbolsResponse, err := c.DownloadSymbolList(ctx, res.SymbolLength)
	if err != nil {
		return fmt.Errorf("failed to upload symbols: %w", err)
	}
	symbols, err := symtab.ParseSymbols(symbolsResponse, datatypes, sess.logger)
	if err != nil {
		return fmt.Errorf("failed to parse symbols: %w", err)
	}
	sess.cache.lock.Lock()
	// Invalidate Handle on every old symbol before the swap, so a caller holding an
	// old pointer fails fast and re-resolves instead of using a handle the PLC may
	// have reassigned to a different symbol.
	zeroOldSymbolHandles(sess.cache.symbols)
	sess.cache.datatypes = datatypes
	// Stamp the session's logger onto every symbol as the cache takes ownership,
	// so records produced later while parsing or serialising them reach the
	// caller's handler instead of stderr. See symbol.logger.
	symtab.StampLoggerOnAll(symbols, sess.logger)
	sess.cache.symbols = symbols
	sess.bumpEpoch()
	sess.cache.lock.Unlock()
	return nil
}

// symbolCache owns the connection-level symbol metadata: the symbol map, the
// datatype table, the reported symbol version, and which load function was used.
// The lock also covers parse(), which rewrites symbols in place.
//
// Lock ordering: NEVER hold cache.lock and notifications.lock at once.
//
// Generation tracking lives on sessionFSM.epoch: a symbols swap bumps it, an
// on-demand insert does not. Publishing a *symbol obtained pre-roundtrip elsewhere
// MUST capture the epoch before resolve and recheck before commit.
type symbolCache struct {
	lock               sync.Mutex
	symbols            map[string]*symtab.Symbol
	datatypes          map[string]symtab.TypeInfo
	symbolVersion      uint8
	onDemandSymbols    map[string]bool
	symbolListLoaded   bool
	symbolsFullyLoaded bool
	datatypesLoaded    bool
}
