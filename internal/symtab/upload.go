package symtab

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"log/slog"
	"math"
	"slices"
	"time"

	"github.com/siyka-au/go-ads/v3/ams"

	"github.com/siyka-au/go-ads/v3/internal/logging"
)

// datatypeFlagBitValues marks a datatype entry whose Offs and Size count bits,
// not bytes: a BIT member of a struct (ADSDATATYPEFLAG_BITVALUES).
const datatypeFlagBitValues = 0x20

type DatatypeEntry struct {
	EntryLength   uint32
	Version       uint32
	HashValue     uint32
	TypeHashValue uint32
	Size          uint32
	Offs          uint32
	DataType      uint32
	Flags         uint32
	NameLength    uint16
	TypeLength    uint16
	CommentLength uint16
	ArrayDim      uint16
	SubItems      uint16
}

type datatypeArrayInfo struct {
	LBound   uint32
	Elements uint32
}

type TypeInfo struct {
	DatatypeEntry DatatypeEntry
	Name          string
	DataType      string
	// RangeMin/RangeMax are the declared bounds of an IEC 61131-3 subrange type
	// (e.g. INT(-10..10)); both nil when DataType carries no subrange.
	RangeMin, RangeMax *int64
	Comment            string
	Children           map[string]*TypeInfo
}

type SymbolEntry struct {
	EntryLength   uint32
	IGroup        uint32
	IOffs         uint32
	Size          uint32
	DataType      uint32
	Flags         uint32
	NameLength    uint16
	TypeLength    uint16
	CommentLength uint16
}

type UploadSymbol struct {
	SymbolEntry        SymbolEntry
	Name               string
	DataType           string
	RangeMin, RangeMax *int64
	Comment            string
	Children           map[string]*UploadSymbol
}

func ParseSymbols(data []byte, datatypes map[string]TypeInfo, lg *slog.Logger) (symbols map[string]*Symbol, err error) {
	symbols = map[string]*Symbol{}
	buff := bytes.NewBuffer(data)

	for buff.Len() > 0 {
		begBuff := buff.Len()
		result := SymbolEntry{}
		if err := binary.Read(buff, binary.LittleEndian, &result); err != nil {
			return nil, fmt.Errorf("reading symbol entry: %w", err)
		}

		name := make([]byte, result.NameLength)
		dt := make([]byte, result.TypeLength)
		comment := make([]byte, result.CommentLength)
		if err := binary.Read(buff, binary.LittleEndian, name); err != nil {
			return nil, fmt.Errorf("reading symbol name: %w", err)
		}
		buff.Next(1)
		if err := binary.Read(buff, binary.LittleEndian, dt); err != nil {
			return nil, fmt.Errorf("reading symbol type: %w", err)
		}
		buff.Next(1)
		if err := binary.Read(buff, binary.LittleEndian, comment); err != nil {
			return nil, fmt.Errorf("reading symbol comment: %w", err)
		}
		buff.Next(1)
		item := UploadSymbol{}
		item.Name = string(name)
		item.DataType, item.RangeMin, item.RangeMax = resolveDataType(string(dt))
		item.Comment = string(comment)
		item.SymbolEntry = result
		endBuff := buff.Len()
		symbol := AddSymbol(item, datatypes, lg)

		symbols[Key(item.Name)] = symbol
		AddChildren(symbol, symbols)

		skip := int(item.SymbolEntry.EntryLength) - (begBuff - endBuff)
		if skip < 0 {
			return nil, fmt.Errorf("symbol %q: EntryLength %d is smaller than bytes consumed %d",
				item.Name, item.SymbolEntry.EntryLength, begBuff-endBuff)
		}
		if skip > 0 {
			buff.Next(skip)
		}
	}
	return
}

func AddChildren(s *Symbol, symbols map[string]*Symbol) {
	for _, child := range s.Children {
		if _, ok := symbols[Key(child.FullName)]; !ok {
			symbols[Key(child.FullName)] = child
			AddChildren(child, symbols)
		}
	}
}

func AddSymbol(uploadSym UploadSymbol, datatypes map[string]TypeInfo, lg *slog.Logger) *Symbol {
	flags := ams.SymbolFlag(uploadSym.SymbolEntry.Flags)
	sym := &Symbol{
		Name:           uploadSym.Name,
		LastUpdateTime: time.Now(),
		FullName:       uploadSym.Name,
		DataType:       uploadSym.DataType,
		RangeMin:       uploadSym.RangeMin,
		RangeMax:       uploadSym.RangeMax,
		Comment:        uploadSym.Comment,
		Length:         uploadSym.SymbolEntry.Size,
		BaseType:       ams.DataType(uploadSym.SymbolEntry.DataType),
		Group:          uploadSym.SymbolEntry.IGroup,
		Offset:         uploadSym.SymbolEntry.IOffs,
		Flags:          flags,
		ContextMask:    flags.ContextMask(),
	}

	dt, ok := datatypes[uploadSym.DataType]
	if ok {
		sym.Children = dt.AddOffset(sym, datatypes, sym.Group, lg)
	}

	return sym
}

// addOffsetMaxDepth caps recursion depth in datatype tree expansion. Real
// PLC nesting is at most a few dozen levels; the cap defends against a
// malformed datatype table forming a self-cycle (forbidden by IEC 61131-3
// but not enforced over the wire).
const addOffsetMaxDepth = 256

func (data *TypeInfo) AddOffset(parent *Symbol, datatypes map[string]TypeInfo, group uint32, lg *slog.Logger) (children map[string]*Symbol) {
	return data.addOffsetDepth(parent, datatypes, group, 0, lg)
}

func (data *TypeInfo) addOffsetDepth(parent *Symbol, datatypes map[string]TypeInfo, group uint32, depth int, lg *slog.Logger) (children map[string]*Symbol) {
	children = map[string]*Symbol{}
	if depth >= addOffsetMaxDepth {
		logging.Or(lg).Warn("addOffset hit depth cap; possible datatype self-cycle in PLC response",
			"parent", parent.FullName,
			"datatype", data.DataType,
			"max_depth", addOffsetMaxDepth)
		return
	}

	for key, segment := range data.Children {
		var path string
		if len(segment.Name) == 0 {
			continue
		}
		if segment.Name[0:1] != "[" {
			path = fmt.Sprint(parent.FullName, ".", segment.Name)
		} else {
			path = fmt.Sprint(parent.FullName, segment.Name)
		}

		child := Symbol{
			// Children built here reach the cache through their parent, so the
			// stamping at ingest never sees them — and rebuildSymbolChildren runs
			// this path again when the datatype table arrives after the symbol list.
			// Without this their parse and serialise warnings fall back to the
			// package default logger, which is the bypass this change set removes.
			logger:         lg,
			Name:           segment.Name,
			LastUpdateTime: time.Now(),
			FullName:       path,
			DataType:       segment.DataType,
			RangeMin:       segment.RangeMin,
			RangeMax:       segment.RangeMax,
			Comment:        segment.Comment,
			Length:         segment.DatatypeEntry.Size,
			// Left at DataTypeVoid, a member resolved by guess instead: BOOL became
			// BYTE on TC3, SINT on TC2. Composites still report DataTypeBigType.
			BaseType: ams.DataType(segment.DatatypeEntry.DataType),
			// Update with area and offset
			Group:     group,
			Offset:    segment.DatatypeEntry.Offs,
			BitMember: segment.DatatypeEntry.Flags&datatypeFlagBitValues != 0,
			Parent:    parent,
		}

		// An outer dimension of a multi-dimensional array carries its inner
		// dimension as prebuilt children (makeArrayChildren), while its DataType
		// names the element type -- looking that up would make the whole row one
		// element and fail its parse on size. Expand the prebuilt level instead.
		// Otherwise check if subitems exist — but skip enum types.
		// Enums have children (enum constants) but should be parsed as
		// their base type (e.g. INT), not expanded as struct fields.
		if segment.Name[0] == '[' && len(segment.Children) > 0 {
			child.Children = segment.addOffsetDepth(&child, datatypes, child.Group, depth+1, lg)
		} else if dt, ok := datatypes[segment.DataType]; ok && !isEnumDataType(&dt) {
			child.Children = dt.addOffsetDepth(&child, datatypes, child.Group, depth+1, lg)
		}

		children[key] = &child
	}

	return
}

// isEnumDataType returns true if a datatype represents an enum.
// Enums have a parseable base type (e.g. INT, UINT), children
// (enum constants), and ArrayDim == 0. Arrays of parseable types
// also have children and a parseable base type but have ArrayDim > 0.
func isEnumDataType(dt *TypeInfo) bool {
	return len(dt.Children) > 0 &&
		dt.DatatypeEntry.ArrayDim == 0 &&
		slices.Contains(ParseableTypes, dt.DataType)
}

func ParseDataTypes(data []byte, lg *slog.Logger) (datatypes map[string]TypeInfo, err error) {
	buff := bytes.NewBuffer(data)
	datatypes = make(map[string]TypeInfo)
	for buff.Len() > 0 {
		header, err := decodeSymbolUploadDataType(buff, "", lg)
		if err != nil {
			return nil, fmt.Errorf("parsing datatype entry: %w", err)
		}
		datatypes[header.Name] = header
	}
	return
}

func decodeSymbolUploadDataType(data *bytes.Buffer, parent string, lg *slog.Logger) (header TypeInfo, err error) {
	result := DatatypeEntry{}
	header = TypeInfo{}

	totalSize := data.Len()

	if totalSize < 48 {
		err = fmt.Errorf("%s - wrong size < 48 bytes", parent)
		logging.Or(lg).Error("error during binary read", "error", err, logging.HexAttr("data", data.Bytes()))
		return
	}

	err = binary.Read(data, binary.LittleEndian, &result)
	if err != nil {
		logging.Or(lg).Error("error during binary read", "error", err)
		return
	}
	name := make([]byte, result.NameLength)
	dt := make([]byte, result.TypeLength)
	comment := make([]byte, result.CommentLength)

	err = binary.Read(data, binary.LittleEndian, name)
	if err != nil {
		logging.Or(lg).Error("error during binary read", "error", err)
		return
	}
	data.Next(1)
	err = binary.Read(data, binary.LittleEndian, dt)
	if err != nil {
		logging.Or(lg).Error("error during binary read", "error", err)
		return
	}
	data.Next(1)
	err = binary.Read(data, binary.LittleEndian, comment)
	if err != nil {
		logging.Or(lg).Error("error during binary read", "error", err)
		return
	}
	data.Next(1)

	header.Name = string(name)
	header.DataType = string(dt)
	header.Comment = string(comment)

	header.DatatypeEntry = result

	header.DataType, header.RangeMin, header.RangeMax = resolveDataType(header.DataType)

	childLen := int(result.EntryLength) - (totalSize - data.Len())
	if childLen <= 0 {
		return
	}
	if childLen > data.Len() {
		return header, fmt.Errorf("childLen %d exceeds remaining data %d for %s", childLen, data.Len(), header.Name)
	}

	childData := make([]byte, childLen)
	n, err := data.Read(childData)
	if err != nil {
		return header, fmt.Errorf("reading children of %s (got %d of %d bytes): %w", header.Name, n, childLen, err)
	}

	buff := bytes.NewBuffer(childData)
	if header.Children == nil {
		header.Children = map[string]*TypeInfo{}
	}
	if header.DatatypeEntry.ArrayDim > 0 {
		// Children is an array
		var arrayInfo datatypeArrayInfo
		arrayLevels := []datatypeArrayInfo{}

		for i := 0; i < int(header.DatatypeEntry.ArrayDim); i++ {
			err = binary.Read(buff, binary.LittleEndian, &arrayInfo)
			if err != nil {
				return header, fmt.Errorf("reading array info for %s: %w", header.Name, err)
			}
			arrayLevels = append(arrayLevels, arrayInfo)
		}
		header.Children = makeArrayChildren(arrayLevels, header.DataType, header.DatatypeEntry.Size, lg)
	} else {
		// Children is standard variables
		for j := 0; j < int(result.SubItems); j++ {
			child, err := decodeSymbolUploadDataType(buff, header.Name, lg)
			if err != nil {
				return header, fmt.Errorf("reading subitem %d of %s: %w", j, header.Name, err)
			}
			header.Children[child.Name] = &child
		}
	}

	return
}

// maxArrayElementsPerLevel caps PLC-declared array Elements per dimension to
// prevent malformed/buggy datatype responses from triggering huge map
// allocations. Real PLC arrays rarely exceed a few thousand elements;
// 1M is a safety ceiling well above any legitimate use.
const maxArrayElementsPerLevel = 1_000_000

func makeArrayChildren(levels []datatypeArrayInfo, dt string, size uint32, lg *slog.Logger) (children map[string]*TypeInfo) {
	children = map[string]*TypeInfo{}

	if len(levels) < 1 {
		return
	}

	level := levels[0]
	if level.Elements == 0 {
		return
	}
	// The lower bound is a DINT on the wire: ARRAY[-9..9] arrives as 0xFFFFFFF7.
	lbound := int64(int32(level.LBound))
	// defend against malformed/buggy PLC datatype responses.
	// (1) Cap Elements at a sanity limit to prevent DoS via huge map allocation.
	// (2) Reject when the upper bound would pass DINT's range.
	if level.Elements > maxArrayElementsPerLevel {
		logging.Or(lg).Error("makeArrayChildren: array Elements exceeds sanity cap, refusing to allocate",
			"declared_elements", level.Elements,
			"cap", maxArrayElementsPerLevel,
			"datatype", dt)
		return
	}
	if lbound+int64(level.Elements)-1 > math.MaxInt32 {
		logging.Or(lg).Error("makeArrayChildren: LBound + Elements overflows DINT, refusing",
			"lbound", lbound,
			"elements", level.Elements,
			"datatype", dt)
		return
	}
	// subChildren is shared across all array elements at this level.
	// This is intentional: children are read-only after symbol table construction,
	// and deep-copying would be expensive for large arrays (e.g., ARRAY[0..999]).
	// Recursion size shrinks by this level's element count so inner-dim
	// elements compute correct per-element byte size, not the outer total.
	subChildren := makeArrayChildren(levels[1:], dt, size/level.Elements, lg)

	var offset uint32

	for i := lbound; i < lbound+int64(level.Elements); i++ {
		name := fmt.Sprintf("[%d]", i)

		child := TypeInfo{}
		child.Name = name
		child.DataType = dt
		child.DatatypeEntry.Offs = offset
		child.DatatypeEntry.Size = size / level.Elements
		child.Children = subChildren

		children[name] = &child
		offset += size / level.Elements
	}

	return
}
