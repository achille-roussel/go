// Code generated from _gen/Wasm3latelower.rules using 'go generate'; DO NOT EDIT.

package ssa

func rewriteValueWasm3latelower(v *Value) bool {
	switch v.Op {
	case OpOffPtr:
		return rewriteValueWasm3latelower_OpOffPtr(v)
	}
	return false
}
func rewriteValueWasm3latelower_OpOffPtr(v *Value) bool {
	v_0 := v.Args[0]
	// match: (OffPtr <t> [off] base)
	// cond: base.Type.IsPtr() && base.Type.Elem() != nil && base.Type.Elem().IsStruct() && wasm3IsFieldInteriorPtr(t)
	// result: (MakeFieldPtr <t> {base.Type.Elem()} [off] base)
	for {
		t := v.Type
		off := auxIntToInt64(v.AuxInt)
		base := v_0
		if !(base.Type.IsPtr() && base.Type.Elem() != nil && base.Type.Elem().IsStruct() && wasm3IsFieldInteriorPtr(t)) {
			break
		}
		v.reset(OpWasm3MakeFieldPtr)
		v.Type = t
		v.AuxInt = int64ToAuxInt(off)
		v.Aux = typeToAux(base.Type.Elem())
		v.AddArg(base)
		return true
	}
	return false
}
func rewriteBlockWasm3latelower(b *Block) bool {
	return false
}
