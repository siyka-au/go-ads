package symtab

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"

	"github.com/siyka-au/go-ads/v3/ams"
)

// attrKV is one {attribute 'name':='value'} pragma for buildEnumDatatypeEntry.
type attrKV struct{ name, value string }

// buildEnumDatatypeEntry constructs the raw bytes for one datatype-table
// entry shaped like a real TwinCAT enum: header, strings, TypeGuid,
// Attributes, EnumInfo. Flags is fixed at
// datatypeFlagTypeGuid|datatypeFlagAttributes|datatypeFlagEnumInfo (0x3081,
// matching every FB_EnumTest type observed live), overridable via
// extraFlags for the Methods-unsupported test.
func buildEnumDatatypeEntry(t *testing.T, name, baseType string, baseADST ams.DataType, size uint32, attrs []attrKV, members []EnumConstant, extraFlags uint32) []byte {
	t.Helper()

	var body bytes.Buffer
	writeCStr := func(s string) {
		body.WriteString(s)
		body.WriteByte(0)
	}

	// TypeGuid: 16 arbitrary bytes.
	body.Write(bytes.Repeat([]byte{0xAB}, 16))

	// Attributes: uint16 count, then per entry [nameLen byte][valueLen byte][name+\0][value+\0].
	binary.Write(&body, binary.LittleEndian, uint16(len(attrs)))
	for _, a := range attrs {
		body.WriteByte(byte(len(a.name)))
		body.WriteByte(byte(len(a.value)))
		writeCStr(a.name)
		writeCStr(a.value)
	}

	// EnumInfo: uint16 count, then per member [nameLen byte][name+\0][value, `size` bytes LE].
	binary.Write(&body, binary.LittleEndian, uint16(len(members)))
	for _, m := range members {
		body.WriteByte(byte(len(m.Name)))
		writeCStr(m.Name)
		valBuf := make([]byte, 8)
		binary.LittleEndian.PutUint64(valBuf, uint64(m.Value))
		body.Write(valBuf[:size])
	}

	nameLen, typeLen := uint16(len(name)), uint16(len(baseType))
	entry := DatatypeEntry{
		Size:          size,
		DataType:      uint32(baseADST),
		Flags:         datatypeFlagTypeGuid | datatypeFlagAttributes | datatypeFlagEnumInfo | extraFlags,
		NameLength:    nameLen,
		TypeLength:    typeLen,
		CommentLength: 0,
		ArrayDim:      0,
		SubItems:      0,
	}
	headerLen := 42 + int(nameLen) + 1 + int(typeLen) + 1 + 1 // +1 per null terminator (name, type, empty comment)
	entry.EntryLength = uint32(headerLen + body.Len())

	var out bytes.Buffer
	binary.Write(&out, binary.LittleEndian, entry)
	out.WriteString(name)
	out.WriteByte(0)
	out.WriteString(baseType)
	out.WriteByte(0)
	out.WriteByte(0) // empty comment + terminator
	out.Write(body.Bytes())
	return out.Bytes()
}

func defaultAttrs() []attrKV {
	return []attrKV{
		{"qualified_only", ""},
		{"strict", ""},
		{"to_string", ""},
		{"generate_implicit_init_function", ""},
	}
}

func TestDecodeEnumInfo_RealWireFormat(t *testing.T) {
	tests := []struct {
		name     string
		baseType string
		adst     ams.DataType
		size     uint32
		members  []EnumConstant
	}{
		{"E_Byte", "BYTE", ams.DataTypeUint8, 1, []EnumConstant{{"eOptionA", 0}, {"eOptionB", 1}, {"eOptionC", 2}}},
		{"E_Default", "INT", ams.DataTypeInt16, 2, []EnumConstant{{"eOptionA", 0}, {"eOptionB", 1}, {"eOptionC", 2}}},
		{"E_Dint", "DINT", ams.DataTypeInt32, 4, []EnumConstant{{"eOptionA", 0}, {"eOptionB", 1}, {"eOptionC", 2}}},
		{"E_Lint", "LINT", ams.DataTypeInt64, 8, []EnumConstant{{"eOptionA", 0}, {"eOptionB", 1}, {"eOptionC", 2}}},
		{"E_Negative", "INT", ams.DataTypeInt16, 2, []EnumConstant{{"eOptionA", -1}, {"eOptionB", 0}, {"eOptionC", 1}}},
		{"E_Sparse", "INT", ams.DataTypeInt16, 2, []EnumConstant{{"eOptionA", -10}, {"eOptionB", 0}, {"eOptionC", 100}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw := buildEnumDatatypeEntry(t, tt.name, tt.baseType, tt.adst, tt.size, defaultAttrs(), tt.members, 0)
			buf := bytes.NewBuffer(raw)
			header, err := decodeSymbolUploadDataType(buf, "", nil)
			if err != nil {
				t.Fatalf("decodeSymbolUploadDataType: %v", err)
			}
			if header.Name != tt.name {
				t.Errorf("Name = %q, want %q", header.Name, tt.name)
			}
			if header.DataType != tt.baseType {
				t.Errorf("DataType = %q, want %q", header.DataType, tt.baseType)
			}
			if len(header.Constants) != len(tt.members) {
				t.Fatalf("Constants = %v, want %v", header.Constants, tt.members)
			}
			for i, m := range tt.members {
				if header.Constants[i] != m {
					t.Errorf("Constants[%d] = %+v, want %+v", i, header.Constants[i], m)
				}
			}
			if buf.Len() != 0 {
				t.Errorf("%d unconsumed bytes remain after decode", buf.Len())
			}
		})
	}
}

// TestDecodeEnumInfo_Implicit mirrors eImplicit: no strict/to_string
// attributes (only 2, not 4), since those pragmas only attach to a named
// TYPE...END_TYPE declaration.
func TestDecodeEnumInfo_Implicit(t *testing.T) {
	attrs := []attrKV{{"qualified_only", ""}, {"generate_implicit_init_function", ""}}
	members := []EnumConstant{{"eImplicitOptionA", 0}, {"eImplicitOptionB", 1}, {"eImplicitOptionC", 2}}
	raw := buildEnumDatatypeEntry(t, "Implicit_Enum__FB_EnumTest__eImplicit", "INT", ams.DataTypeInt16, 2, attrs, members, 0)
	header, err := decodeSymbolUploadDataType(bytes.NewBuffer(raw), "", nil)
	if err != nil {
		t.Fatalf("decodeSymbolUploadDataType: %v", err)
	}
	if len(header.Constants) != 3 {
		t.Fatalf("Constants = %v, want 3 entries", header.Constants)
	}
	for i, m := range members {
		if header.Constants[i] != m {
			t.Errorf("Constants[%d] = %+v, want %+v", i, header.Constants[i], m)
		}
	}
}

func TestDecodeEnumInfo_MethodsFlagUnsupported(t *testing.T) {
	raw := buildEnumDatatypeEntry(t, "E_WithMethods", "INT", ams.DataTypeInt16, 2, nil, nil, datatypeFlagMethods)
	_, err := decodeSymbolUploadDataType(bytes.NewBuffer(raw), "", nil)
	if err == nil || !strings.Contains(err.Error(), "Methods") {
		t.Fatalf("err = %v, want an error mentioning the unsupported Methods flag", err)
	}
}

// TestDecodeEnumInfo_InArray builds ARRAY[0..1] OF E_Byte and confirms each
// element is classified and decoded as a plain scalar (not mis-expanded) and
// still carries the referenced enum's Constants.
func TestDecodeEnumInfo_InArray(t *testing.T) {
	elemMembers := []EnumConstant{{"eOptionA", 0}, {"eOptionB", 1}, {"eOptionC", 2}}
	elemRaw := buildEnumDatatypeEntry(t, "E_Byte", "BYTE", ams.DataTypeUint8, 1, defaultAttrs(), elemMembers, 0)
	elemHeader, err := decodeSymbolUploadDataType(bytes.NewBuffer(elemRaw), "", nil)
	if err != nil {
		t.Fatalf("decoding element type: %v", err)
	}

	datatypes := map[string]TypeInfo{"E_Byte": elemHeader}
	arrayInfo := TypeInfo{
		Name:     "aEnumArray",
		DataType: "E_Byte",
		DatatypeEntry: DatatypeEntry{
			ArrayDim: 1,
			Size:     2,
		},
		Children: makeArrayChildren([]datatypeArrayInfo{{LBound: 0, Elements: 2}}, "E_Byte", 2, nil),
	}

	parent := &Symbol{Name: "aEnumArray", FullName: "MAIN.aEnumArray", DataType: "aEnumArray", Length: 2}
	children := arrayInfo.AddOffset(parent, datatypes, 0, nil)
	if len(children) != 2 {
		t.Fatalf("got %d array element symbols, want 2", len(children))
	}
	for _, name := range []string{"[0]", "[1]"} {
		child, ok := children[name]
		if !ok {
			t.Fatalf("missing array element %s", name)
		}
		if len(child.Children) != 0 {
			t.Errorf("%s: has %d children, want 0 (enum elements must decode as scalars)", name, len(child.Children))
		}
		if len(child.Constants) != len(elemMembers) {
			t.Errorf("%s: Constants = %v, want %v", name, child.Constants, elemMembers)
		}
	}
}

// TestDecodeEnumInfo_InStructArray builds a struct containing an array of a
// sub-struct with an enum member, confirming Constants survive nested
// struct+array expansion.
func TestDecodeEnumInfo_InStructArray(t *testing.T) {
	enumMembers := []EnumConstant{{"eOptionA", 0}, {"eOptionB", 1}, {"eOptionC", 2}}
	enumRaw := buildEnumDatatypeEntry(t, "E_Byte", "BYTE", ams.DataTypeUint8, 1, defaultAttrs(), enumMembers, 0)
	enumHeader, err := decodeSymbolUploadDataType(bytes.NewBuffer(enumRaw), "", nil)
	if err != nil {
		t.Fatalf("decoding enum type: %v", err)
	}

	datatypes := map[string]TypeInfo{
		"E_Byte": enumHeader,
		"ST_Sub": {
			Name:     "ST_Sub",
			DataType: "",
			DatatypeEntry: DatatypeEntry{
				Size:     1,
				SubItems: 1,
			},
			Children: map[string]*TypeInfo{
				"eField": {Name: "eField", DataType: "E_Byte", DatatypeEntry: DatatypeEntry{Size: 1, Offs: 0}},
			},
		},
	}
	outerArray := TypeInfo{
		Name:          "aSubArray",
		DataType:      "ST_Sub",
		DatatypeEntry: DatatypeEntry{ArrayDim: 1, Size: 2},
		Children:      makeArrayChildren([]datatypeArrayInfo{{LBound: 0, Elements: 2}}, "ST_Sub", 2, nil),
	}

	parent := &Symbol{Name: "aSubArray", FullName: "MAIN.aSubArray", DataType: "aSubArray", Length: 2}
	children := outerArray.AddOffset(parent, datatypes, 0, nil)
	elem0, ok := children["[0]"]
	if !ok {
		t.Fatal("missing array element [0]")
	}
	field, ok := elem0.Children["eField"]
	if !ok {
		t.Fatalf("[0] has no eField child; children = %v", elem0.Children)
	}
	if len(field.Constants) != len(enumMembers) {
		t.Errorf("[0].eField Constants = %v, want %v", field.Constants, enumMembers)
	}
}

func TestCheckEnumMember(t *testing.T) {
	byteConsts := []EnumConstant{{"eOptionA", 0}, {"eOptionB", 1}, {"eOptionC", 2}}
	negConsts := []EnumConstant{{"eOptionA", -1}, {"eOptionB", 0}, {"eOptionC", 1}}

	tests := []struct {
		name      string
		dataType  string
		v         any
		constants []EnumConstant
		wantErr   bool
	}{
		{"declared byte value", "E_Byte", uint8(1), byteConsts, false},
		{"undeclared byte value", "E_Byte", uint8(99), byteConsts, true},
		{"declared negative value", "E_Negative", int16(-1), negConsts, false},
		{"undeclared negative value", "E_Negative", int16(-5), negConsts, true},
		{"non-integer value", "E_Byte", "eOptionA", byteConsts, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkEnumMember(tt.dataType, tt.v, tt.constants)
			if (err != nil) != tt.wantErr {
				t.Errorf("checkEnumMember(%q, %v) err = %v, wantErr %v", tt.dataType, tt.v, err, tt.wantErr)
			}
		})
	}
}
