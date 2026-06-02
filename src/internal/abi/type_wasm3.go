// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build wasm3

// wasm3 mirror of type.go. Identical to the upstream file except for
// the helpers that walk linker-emitted trailing bytes via pointer
// arithmetic — addChecked / UncommonType.Methods /
// UncommonType.ExportedMethods / FuncType.InSlice / FuncType.OutSlice
// and the four Name accessors (DataChecked, Data, ReadVarint, Name,
// Tag) — which rely on the standard
// `unsafe.Pointer(uintptr(p) + n)` idiom that the wasm3 backend
// rejects (anyref has no scalar form). Those helpers panic on wasm3
// until M6 wires up compiler-generated per-type accessors hung off
// the WasmGC type descriptor.

package abi

import (
	"internal/goarch"
	"unsafe"
)

// Type is the runtime representation of a Go type.
//
// Be careful about accessing this type at build time, as the version
// of this type in the compiler/linker may not have the same layout
// as the version in the target binary, due to pointer width
// differences and any experiments. Use cmd/compile/internal/rttype
// or the functions in compiletype.go to access this type instead.
// (TODO: this admonition applies to every type in this package.
// Put it in some shared location?)
type Type struct {
	Size_       uintptr
	PtrBytes    uintptr // number of (prefix) bytes in the type that can contain pointers
	Hash        uint32  // hash of type; avoids computation in hash tables
	TFlag       TFlag   // extra type information flags
	Align_      uint8   // alignment of variable with this type
	FieldAlign_ uint8   // alignment of struct field with this type
	Kind_       Kind    // what kind of type this is (string, int, ...)
	// function for comparing objects of this type
	// (ptr to object A, ptr to object B) -> ==?
	Equal func(unsafe.Pointer, unsafe.Pointer) bool
	// GCData stores the GC type data for the garbage collector.
	// Normally, GCData points to a bitmask that describes the
	// ptr/nonptr fields of the type. The bitmask will have at
	// least PtrBytes/ptrSize bits.
	// If the TFlagGCMaskOnDemand bit is set, GCData is instead a
	// **byte and the pointer to the bitmask is one dereference away.
	// The runtime will build the bitmask if needed.
	// (See runtime/type.go:getGCMask.)
	// Note: multiple types may have the same value of GCData,
	// including when TFlagGCMaskOnDemand is set. The types will, of course,
	// have the same pointer layout (but not necessarily the same size).
	GCData    *byte
	Str       NameOff // string form
	PtrToThis TypeOff // type for pointer to this type, may be zero
}

// A Kind represents the specific kind of type that a Type represents.
// The zero Kind is not a valid kind.
type Kind uint8

const (
	Invalid Kind = iota
	Bool
	Int
	Int8
	Int16
	Int32
	Int64
	Uint
	Uint8
	Uint16
	Uint32
	Uint64
	Uintptr
	Float32
	Float64
	Complex64
	Complex128
	Array
	Chan
	Func
	Interface
	Map
	Pointer
	Slice
	String
	Struct
	UnsafePointer
)

// TFlag is used by a Type to signal what extra type information is
// available in the memory directly following the Type value.
type TFlag uint8

const (
	TFlagUncommon       TFlag = 1 << 0
	TFlagExtraStar      TFlag = 1 << 1
	TFlagNamed          TFlag = 1 << 2
	TFlagRegularMemory  TFlag = 1 << 3
	TFlagGCMaskOnDemand TFlag = 1 << 4
	TFlagDirectIface    TFlag = 1 << 5

	// Leaving this breadcrumb behind for dlv. It should not be used, and no
	// Kind should be big enough to set this bit.
	KindDirectIface Kind = 1 << 5
)

type NameOff int32
type TypeOff int32
type TextOff int32

func (k Kind) String() string {
	if int(k) < len(kindNames) {
		return kindNames[k]
	}
	return kindNames[0]
}

var kindNames = []string{
	Invalid:       "invalid",
	Bool:          "bool",
	Int:           "int",
	Int8:          "int8",
	Int16:         "int16",
	Int32:         "int32",
	Int64:         "int64",
	Uint:          "uint",
	Uint8:         "uint8",
	Uint16:        "uint16",
	Uint32:        "uint32",
	Uint64:        "uint64",
	Uintptr:       "uintptr",
	Float32:       "float32",
	Float64:       "float64",
	Complex64:     "complex64",
	Complex128:    "complex128",
	Array:         "array",
	Chan:          "chan",
	Func:          "func",
	Interface:     "interface",
	Map:           "map",
	Pointer:       "ptr",
	Slice:         "slice",
	String:        "string",
	Struct:        "struct",
	UnsafePointer: "unsafe.Pointer",
}

func TypeOf(a any) *Type {
	eface := *(*EmptyInterface)(unsafe.Pointer(&a))
	return (*Type)(NoEscape(unsafe.Pointer(eface.Type)))
}

func TypeFor[T any]() *Type {
	return (*PtrType)(unsafe.Pointer(TypeOf((*T)(nil)))).Elem
}

func (t *Type) Kind() Kind { return t.Kind_ }

func (t *Type) HasName() bool {
	return t.TFlag&TFlagNamed != 0
}

func (t *Type) Pointers() bool { return t.PtrBytes != 0 }

func (t *Type) IsDirectIface() bool {
	return t.TFlag&TFlagDirectIface != 0
}

func (t *Type) GcSlice(begin, end uintptr) []byte {
	if t.TFlag&TFlagGCMaskOnDemand != 0 {
		panic("GcSlice can't handle on-demand gcdata types")
	}
	return unsafe.Slice(t.GCData, int(end))[begin:]
}

type Method struct {
	Name NameOff
	Mtyp TypeOff
	Ifn  TextOff
	Tfn  TextOff
}

type UncommonType struct {
	PkgPath NameOff
	Mcount  uint16
	Xcount  uint16
	Moff    uint32
	_       uint32
}

// Methods / ExportedMethods walk a trailing [Mcount]Method block via
// pointer arithmetic on every other arch. wasm3 has no scalar
// pointer form, so this access pattern can't compile. Panic until
// M6 lands compiler-generated per-type accessors.
const wasm3PtrArithUnsupported = "internal/abi: pointer-arithmetic reflect helpers are not yet supported on GOARCH=wasm3 (M6 work)"

func (t *UncommonType) Methods() []Method {
	if t.Mcount == 0 {
		return nil
	}
	panic(wasm3PtrArithUnsupported)
}

func (t *UncommonType) ExportedMethods() []Method {
	if t.Xcount == 0 {
		return nil
	}
	panic(wasm3PtrArithUnsupported)
}

// addChecked is unreachable on wasm3 — kept as a panic stub so any
// future intra-package caller fails loudly instead of silently miscompiling.
func addChecked(p unsafe.Pointer, x uintptr, whySafe string) unsafe.Pointer {
	_ = p
	_ = x
	_ = whySafe
	panic(wasm3PtrArithUnsupported)
}

type Imethod struct {
	Name NameOff
	Typ  TypeOff
}

type ArrayType struct {
	Type
	Elem  *Type
	Slice *Type
	Len   uintptr
}

func (t *Type) Len() int {
	if t.Kind() == Array {
		return int((*ArrayType)(unsafe.Pointer(t)).Len)
	}
	return 0
}

func (t *Type) Common() *Type {
	return t
}

type ChanDir int

const (
	RecvDir    ChanDir = 1 << iota
	SendDir
	BothDir            = RecvDir | SendDir
	InvalidDir ChanDir = 0
)

type ChanType struct {
	Type
	Elem *Type
	Dir  ChanDir
}

type structTypeUncommon struct {
	StructType
	u UncommonType
}

func (t *Type) ChanDir() ChanDir {
	if t.Kind() == Chan {
		ch := (*ChanType)(unsafe.Pointer(t))
		return ch.Dir
	}
	return InvalidDir
}

func (t *Type) Uncommon() *UncommonType {
	if t.TFlag&TFlagUncommon == 0 {
		return nil
	}
	switch t.Kind() {
	case Struct:
		return &(*structTypeUncommon)(unsafe.Pointer(t)).u
	case Pointer:
		type u struct {
			PtrType
			u UncommonType
		}
		return &(*u)(unsafe.Pointer(t)).u
	case Func:
		type u struct {
			FuncType
			u UncommonType
		}
		return &(*u)(unsafe.Pointer(t)).u
	case Slice:
		type u struct {
			SliceType
			u UncommonType
		}
		return &(*u)(unsafe.Pointer(t)).u
	case Array:
		type u struct {
			ArrayType
			u UncommonType
		}
		return &(*u)(unsafe.Pointer(t)).u
	case Chan:
		type u struct {
			ChanType
			u UncommonType
		}
		return &(*u)(unsafe.Pointer(t)).u
	case Map:
		type u struct {
			MapType
			u UncommonType
		}
		return &(*u)(unsafe.Pointer(t)).u
	case Interface:
		type u struct {
			InterfaceType
			u UncommonType
		}
		return &(*u)(unsafe.Pointer(t)).u
	default:
		type u struct {
			Type
			u UncommonType
		}
		return &(*u)(unsafe.Pointer(t)).u
	}
}

func (t *Type) Elem() *Type {
	switch t.Kind() {
	case Array:
		tt := (*ArrayType)(unsafe.Pointer(t))
		return tt.Elem
	case Chan:
		tt := (*ChanType)(unsafe.Pointer(t))
		return tt.Elem
	case Map:
		tt := (*MapType)(unsafe.Pointer(t))
		return tt.Elem
	case Pointer:
		tt := (*PtrType)(unsafe.Pointer(t))
		return tt.Elem
	case Slice:
		tt := (*SliceType)(unsafe.Pointer(t))
		return tt.Elem
	}
	return nil
}

func (t *Type) StructType() *StructType {
	if t.Kind() != Struct {
		return nil
	}
	return (*StructType)(unsafe.Pointer(t))
}

func (t *Type) MapType() *MapType {
	if t.Kind() != Map {
		return nil
	}
	return (*MapType)(unsafe.Pointer(t))
}

func (t *Type) PointerType() *PtrType {
	if t.Kind() != Pointer {
		return nil
	}
	return (*PtrType)(unsafe.Pointer(t))
}

func (t *Type) SliceType() *SliceType {
	if t.Kind() != Slice {
		return nil
	}
	return (*SliceType)(unsafe.Pointer(t))
}

func (t *Type) ArrayType() *ArrayType {
	if t.Kind() != Array {
		return nil
	}
	return (*ArrayType)(unsafe.Pointer(t))
}

func (t *Type) ChanType() *ChanType {
	if t.Kind() != Chan {
		return nil
	}
	return (*ChanType)(unsafe.Pointer(t))
}

func (t *Type) FuncType() *FuncType {
	if t.Kind() != Func {
		return nil
	}
	return (*FuncType)(unsafe.Pointer(t))
}

func (t *Type) InterfaceType() *InterfaceType {
	if t.Kind() != Interface {
		return nil
	}
	return (*InterfaceType)(unsafe.Pointer(t))
}

func (t *Type) Size() uintptr { return t.Size_ }

func (t *Type) Align() int { return int(t.Align_) }

func (t *Type) FieldAlign() int { return int(t.FieldAlign_) }

type InterfaceType struct {
	Type
	PkgPath Name
	Methods []Imethod
}

func (t *Type) ExportedMethods() []Method {
	ut := t.Uncommon()
	if ut == nil {
		return nil
	}
	return ut.ExportedMethods()
}

func (t *Type) NumMethod() int {
	if t.Kind() == Interface {
		tt := (*InterfaceType)(unsafe.Pointer(t))
		return tt.NumMethod()
	}
	return len(t.ExportedMethods())
}

func (t *InterfaceType) NumMethod() int { return len(t.Methods) }

func (t *Type) Key() *Type {
	if t.Kind() == Map {
		return (*MapType)(unsafe.Pointer(t)).Key
	}
	return nil
}

type SliceType struct {
	Type
	Elem *Type
}

type FuncType struct {
	Type
	InCount  uint16
	OutCount uint16
}

func (t *FuncType) In(i int) *Type {
	return t.InSlice()[i]
}

func (t *FuncType) NumIn() int {
	return int(t.InCount)
}

func (t *FuncType) NumOut() int {
	return int(t.OutCount & (1<<15 - 1))
}

func (t *FuncType) Out(i int) *Type {
	return (t.OutSlice()[i])
}

func (t *FuncType) InSlice() []*Type {
	if t.InCount == 0 {
		return nil
	}
	panic(wasm3PtrArithUnsupported)
}

func (t *FuncType) OutSlice() []*Type {
	if t.NumOut() == 0 {
		return nil
	}
	panic(wasm3PtrArithUnsupported)
}

func (t *FuncType) IsVariadic() bool {
	return t.OutCount&(1<<15) != 0
}

type PtrType struct {
	Type
	Elem *Type
}

type StructField struct {
	Name   Name
	Typ    *Type
	Offset uintptr
}

func (f *StructField) Embedded() bool {
	return f.Name.IsEmbedded()
}

type StructType struct {
	Type
	PkgPath Name
	Fields  []StructField
}

type Name struct {
	Bytes *byte
}

func (n Name) DataChecked(off int, whySafe string) *byte {
	_ = off
	_ = whySafe
	panic(wasm3PtrArithUnsupported)
}

func (n Name) Data(off int) *byte {
	_ = off
	panic(wasm3PtrArithUnsupported)
}

func (n Name) IsExported() bool {
	return (*n.Bytes)&(1<<0) != 0
}

func (n Name) HasTag() bool {
	return (*n.Bytes)&(1<<1) != 0
}

func (n Name) IsEmbedded() bool {
	return (*n.Bytes)&(1<<3) != 0
}

func (n Name) ReadVarint(off int) (int, int) {
	_ = off
	panic(wasm3PtrArithUnsupported)
}

func (n Name) IsBlank() bool {
	if n.Bytes == nil {
		return false
	}
	panic(wasm3PtrArithUnsupported)
}

func writeVarint(buf []byte, n int) int {
	for i := 0; ; i++ {
		b := byte(n & 0x7f)
		n >>= 7
		if n == 0 {
			buf[i] = b
			return i + 1
		}
		buf[i] = b | 0x80
	}
}

func (n Name) Name() string {
	if n.Bytes == nil {
		return ""
	}
	panic(wasm3PtrArithUnsupported)
}

func (n Name) Tag() string {
	if !n.HasTag() {
		return ""
	}
	panic(wasm3PtrArithUnsupported)
}

func NewName(n, tag string, exported, embedded bool) Name {
	if len(n) >= 1<<29 {
		panic("abi.NewName: name too long: " + n[:1024] + "...")
	}
	if len(tag) >= 1<<29 {
		panic("abi.NewName: tag too long: " + tag[:1024] + "...")
	}
	var nameLen [10]byte
	var tagLen [10]byte
	nameLenLen := writeVarint(nameLen[:], len(n))
	tagLenLen := writeVarint(tagLen[:], len(tag))

	var bits byte
	l := 1 + nameLenLen + len(n)
	if exported {
		bits |= 1 << 0
	}
	if len(tag) > 0 {
		l += tagLenLen + len(tag)
		bits |= 1 << 1
	}
	if embedded {
		bits |= 1 << 3
	}

	b := make([]byte, l)
	b[0] = bits
	copy(b[1:], nameLen[:nameLenLen])
	copy(b[1+nameLenLen:], n)
	if len(tag) > 0 {
		tb := b[1+nameLenLen+len(n):]
		copy(tb, tagLen[:tagLenLen])
		copy(tb[tagLenLen:], tag)
	}

	return Name{Bytes: &b[0]}
}

func (t *Type) DescriptorSize() int {
	var baseSize, addSize int
	switch t.Kind_ {
	case Array:
		baseSize, addSize = t.ArrayType().descriptorSizes()
	case Chan:
		baseSize, addSize = t.ChanType().descriptorSizes()
	case Func:
		baseSize, addSize = t.FuncType().descriptorSizes()
	case Interface:
		baseSize, addSize = t.InterfaceType().descriptorSizes()
	case Map:
		baseSize, addSize = t.MapType().descriptorSizes()
	case Pointer:
		baseSize, addSize = t.PointerType().descriptorSizes()
	case Slice:
		baseSize, addSize = t.SliceType().descriptorSizes()
	case Struct:
		baseSize, addSize = t.StructType().descriptorSizes()
	case Bool,
		Int, Int8, Int16, Int32, Int64,
		Uint, Uint8, Uint16, Uint32, Uint64, Uintptr,
		Float32, Float64, Complex64, Complex128,
		String, UnsafePointer:

		baseSize = int(unsafe.Sizeof(*t))
		addSize = 0

	default:
		panic("DescriptorSize: invalid type descriptor")
	}

	ret := baseSize

	mcount := 0
	ut := t.Uncommon()
	if ut != nil {
		ret += int(unsafe.Sizeof(*ut))
		mcount = int(ut.Mcount)
	}

	ret += addSize

	ret += mcount * int(unsafe.Sizeof(Method{}))

	return ret
}

func (at *ArrayType) descriptorSizes() (base, add int) {
	return int(unsafe.Sizeof(*at)), 0
}

func (ct *ChanType) descriptorSizes() (base, add int) {
	return int(unsafe.Sizeof(*ct)), 0
}

func (ft *FuncType) descriptorSizes() (base, add int) {
	base = int(unsafe.Sizeof(*ft))
	add = (ft.NumIn() + ft.NumOut()) * goarch.PtrSize
	return base, add
}

func (it *InterfaceType) descriptorSizes() (base, add int) {
	base = int(unsafe.Sizeof(*it))
	add = len(it.Methods) * int(unsafe.Sizeof(Imethod{}))
	return base, add
}

func (mt *MapType) descriptorSizes() (base, add int) {
	return int(unsafe.Sizeof(*mt)), 0
}

func (pt *PtrType) descriptorSizes() (base, add int) {
	return int(unsafe.Sizeof(*pt)), 0
}

func (st *SliceType) descriptorSizes() (base, add int) {
	return int(unsafe.Sizeof(*st)), 0
}

func (st *StructType) descriptorSizes() (base, add int) {
	base = int(unsafe.Sizeof(*st))
	add = len(st.Fields) * int(unsafe.Sizeof(StructField{}))
	return base, add
}

const (
	TraceArgsLimit    = 10
	TraceArgsMaxDepth = 5
	TraceArgsMaxLen   = (TraceArgsMaxDepth*3+2)*TraceArgsLimit + 1
)

const (
	TraceArgsEndSeq         = 0xff
	TraceArgsStartAgg       = 0xfe
	TraceArgsEndAgg         = 0xfd
	TraceArgsDotdotdot      = 0xfc
	TraceArgsOffsetTooLarge = 0xfb
	TraceArgsSpecial        = 0xf0
)

const MaxPtrmaskBytes = 16
