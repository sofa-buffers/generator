package typescript

import (
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"

	"github.com/sofa-buffers/generator/internal/generator"
	"github.com/sofa-buffers/generator/internal/ir"
)

func cfgString(cfg map[string]any, key, dflt string) string {
	if v, ok := cfg[key].(string); ok && v != "" {
		return v
	}
	return dflt
}

// int64Mode selects the TS representation of 64-bit integer fields (config key
// `int64`, à la protobufjs). bigint in the 64-bit hot path is the dominant cost
// of the TS codec (JavaScriptCore optimizes bigint far worse than V8), so the
// long/number modes trade the bigint-everywhere API for a bigint-free
// encode/decode hot path. All three modes are wire-identical.
type int64Mode int

const (
	// int64Bigint is the default: bigint scalars, bigint[] arrays.
	int64Bigint int64Mode = iota
	// int64Long backs every u64/i64 position — scalars and arrays alike — with
	// corelib Long behind a get/set accessor pair (assignment accepts
	// Long | bigint | number and converts once, off the per-encode path).
	int64Long
	// int64Number is int64Long plus u64/i64 scalars as number — the caller
	// guarantees scalar values fit the +/-2^53 safe-integer range.
	int64Number
)

func cfgInt64Mode(cfg map[string]any) int64Mode {
	switch cfgString(cfg, "int64", "bigint") {
	case "long":
		return int64Long
	case "number":
		return int64Number
	}
	return int64Bigint
}

// longArrays reports whether 64-bit integer arrays are Long-backed.
func (g *gen) longArrays() bool { return g.i64rep != int64Bigint }

// The opt-in `Visitor.longs` channel this backend used to negotiate per schema is
// withdrawn (corelib-ts#161). Every integer hook now carries `lo` / `hi` — the
// exact wire halves the varint reader already holds — so a Long-backed
// destination takes its value with no conversion and no flag, and a narrow field
// in the same message no longer pays for that choice: the flag was read once from
// the ROOT visitor and covered every field alike, which is what made it a trade
// at all. With the trade gone, `longsThreshold` and the position count that fed
// it have nothing left to decide.

// longScalars reports whether 64-bit integer SCALARS are Long-backed. Only
// `int64: long` does that: `number` deliberately keeps its scalars on the
// corelib writers' number fast path (the caller having guaranteed the range),
// and `bigint` is the default full-range representation.
func (g *gen) longScalars() bool { return g.i64rep == int64Long }

// numberScalars reports whether 64-bit integer scalars are plain numbers.
func (g *gen) numberScalars() bool { return g.i64rep == int64Number }

// longElem reports whether an array element chain terminates in a 64-bit
// integer (u64/i64 directly, or through nested arrays).
func longElem(elem ir.Kind, items *ir.ArrayElem) bool {
	for {
		if isBig(elem) {
			return true
		}
		if elem != ir.KindArray {
			return false
		}
		elem, items = items.Elem, items.ElemItems
	}
}

// longBacked reports whether a field is stored as a private Long / Long[]
// backing field behind a get/set accessor pair: a 64-bit array under
// int64: long/number, or a 64-bit SCALAR under int64: long.
func (g *gen) longBacked(f *ir.Field) bool {
	if isBig(f.Kind) {
		return g.longScalars()
	}
	return g.longArrays() && f.Kind == ir.KindArray && longElem(f.Elem, f.ElemItems)
}

// storage returns the expression the hot paths (marshal/decode/toJSON) use to
// reach a field's storage: the private backing field for a Long-backed array
// (bypassing the accessor pair — no getter call or setter conversion in the hot
// loop), the plain property otherwise.
func (g *gen) storage(recv string, f *ir.Field) string {
	if g.longBacked(f) {
		return recv + "._" + f.Name
	}
	return recv + "." + f.Name
}

// fp32RawCompanion reports whether a field carries the fp32 raw-bits companion
// slot (generator#235): an fp32 scalar, or a NATIVE fp32 array (element kind
// fp32). Both decode through a JS number, which cannot carry an fp32 NaN payload
// (MESSAGE_SPEC §4.6), so the wire bytes are kept beside the value. An fp32 row
// nested inside a wrapper array is NOT covered — its rows have no field of their
// own to hang the companion on. See ARCHITECTURE §9.3 (decode families).
func fp32RawCompanion(f *ir.Field) bool {
	return f.Kind == ir.KindFP32 || (f.Kind == ir.KindArray && f.Elem == ir.KindFP32)
}

// fp32RawName is the property name of a field's fp32 raw-bits companion. The
// `Fp32Raw` suffix (rather than a bare `Raw`) keeps it clear what the slot holds
// and makes a collision with a sibling field's name vanishingly unlikely.
func fp32RawName(name string) string { return name + "Fp32Raw" }

// fp32RawDoc is the TSDoc on that companion slot. It says what a consumer needs
// to know and nothing more: where the bytes come from, that they are not the
// value, and that they cannot silently outvote the value.
func fp32RawDoc(f *ir.Field) string {
	if f.Kind == ir.KindFP32 {
		return "Wire bytes of `" + f.Name + "`, captured on decode only when the decoded value is a\n" +
			"NaN, so that a signaling NaN re-encodes bit-for-bit: a JS number is a 64-bit\n" +
			"double and cannot carry an fp32 NaN's payload bits.\n" +
			"\n" +
			"Not part of the value. serialize ignores these bytes unless `" + f.Name + "` is still a\n" +
			"NaN, so assigning `" + f.Name + "` by hand always wins, and they never reach\n" +
			"toJSON()/fromJSON()."
	}
	return "Wire payload of `" + f.Name + "`, captured on decode only when some element is a\n" +
		"NaN, so that a signaling NaN element re-encodes bit-for-bit: a JS number is a\n" +
		"64-bit double and cannot carry an fp32 NaN's payload bits.\n" +
		"\n" +
		"Not part of the value. serialize renders every element from `" + f.Name + "` and takes\n" +
		"these bytes only for an element that is still the NaN it decoded as, so assigning\n" +
		"an element by hand always wins, and they never reach toJSON()/fromJSON()."
}

// fp32RawStorage is the expression the generated code uses to reach that
// companion — the twin of storage() for the value slot. An fp32 field is never
// Long-backed, so there is no private backing field to bypass here.
func (g *gen) fp32RawStorage(recv string, f *ir.Field) string {
	return recv + "." + fp32RawName(f.Name)
}

func exported(name string) string {
	parts := strings.FieldsFunc(name, func(r rune) bool { return r == '_' })
	var b strings.Builder
	for _, p := range parts {
		if p == "" {
			continue
		}
		b.WriteString(strings.ToUpper(p[:1]))
		b.WriteString(p[1:])
	}
	if b.Len() == 0 {
		return "X"
	}
	return b.String()
}

func (g *gen) typeName(key string) string {
	parts := strings.FieldsFunc(key, func(r rune) bool { return r == '/' || r == '_' })
	var b strings.Builder
	for _, p := range parts {
		if p == "" {
			continue
		}
		b.WriteString(strings.ToUpper(p[:1]))
		b.WriteString(p[1:])
	}
	return b.String()
}

func isBig(k ir.Kind) bool { return k == ir.KindU64 || k == ir.KindI64 }

// blobHasNonEmptyDefault reports whether a blob field carries a non-empty schema
// default (base64 decoding to at least one byte). Only such fields need an
// element-wise equality guard in marshal; an empty default uses `.length !== 0`.
func blobHasNonEmptyDefault(f *ir.Field) bool {
	if f.Kind != ir.KindBlob {
		return false
	}
	if s, ok := f.Default.(string); ok {
		if raw, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(s), "")); err == nil {
			return len(raw) > 0
		}
	}
	return false
}

// helperUse records which module-level helpers/imports the schema's emitted
// classes actually reference, so unused ones are not emitted.
type helperUse struct {
	elemEq      bool // element-wise !== compare: blob or non-Long native array with a value default
	longArrEq   bool // (low, high) word compare: Long-backed 64-bit array with a value default
	long        bool // any Long-backed field -> import Long from the corelib
	countedArr  bool // count-bearing native array -> import SofabError for the over-count reject (generator#100)
	overIdxArr  bool // count-bearing wrapper array -> import SofabError for the over-index reject (generator#142)
	maxlenField bool // bounded string/blob (scalar or wrapper element) -> import SofabError for the over-maxlen reject (MESSAGE_SPEC §7.1)
	narrowInt   bool // narrow integer destination (scalar or native array element) -> import SofabError for the over-width reject (MESSAGE_SPEC §7.1, generator#266)
	fp32Raw     bool // fp32 scalar field -> emit _fp32FromRaw (the §4.6 bit-exact scalar channel, generator#235)
	fp32ArrRaw  bool // native fp32 array field -> emit _fp32ArrayRaw (its array half)
}

// arrayOverIndexed reports whether an array field (recursively through nested
// element items) is a fixed-count wrapper-sequence array (string/blob/struct/
// union/nested-array element) — the shape whose decode emits the generator#142
// over-index SofabError guard.
func arrayOverIndexed(elem ir.Kind, items *ir.ArrayElem, hasCount bool) bool {
	if hasCount && !nativeArrayElem(elem) {
		return true
	}
	if elem == ir.KindArray && items != nil {
		return arrayOverIndexed(items.Elem, items.ElemItems, items.HasCount)
	}
	return false
}

// arrayHasBoundedStrBlob reports whether an array field (recursively through
// nested element items) has a string/blob element carrying a schema maxlen — the
// shape whose decode emits the over-maxlen SofabError guard (MESSAGE_SPEC §7.1).
func arrayHasBoundedStrBlob(elem ir.Kind, items *ir.ArrayElem, elemMaxHas bool) bool {
	if (elem == ir.KindString || elem == ir.KindBlob) && elemMaxHas {
		return true
	}
	if elem == ir.KindArray && items != nil {
		return arrayHasBoundedStrBlob(items.Elem, items.ElemItems, items.ElemMaxHas)
	}
	return false
}

// arrayHasNarrowInt reports whether an array field (recursively through nested
// element items) has a narrow-integer element — the shape whose decode emits the
// over-width SofabError guard (MESSAGE_SPEC §7.1, generator#266).
func arrayHasNarrowInt(elem ir.Kind, items *ir.ArrayElem) bool {
	if ir.IsNarrow(elem) {
		return true
	}
	if elem == ir.KindArray && items != nil {
		return arrayHasNarrowInt(items.Elem, items.ElemItems)
	}
	return false
}

// scanHelpers walks every emitted class's fields and reports which helpers the
// module needs. A Long-backed array with a value default needs longArrEq (Long
// identity !== fails element-wise compare); other defaulted leaf arrays/blobs
// take the corelib's elementsEqual.
func (g *gen) scanHelpers(s *ir.Schema) helperUse {
	var use helperUse
	scan := func(fields []*ir.Field) {
		for _, fld := range fields {
			if blobHasNonEmptyDefault(fld) {
				use.elemEq = true
			}
			if g.longBacked(fld) {
				use.long = true
			}
			if fld.Kind == ir.KindArray && arrayOverIndexed(fld.Elem, fld.ElemItems, fld.HasCount) {
				use.overIdxArr = true
			}
			// A bounded string/blob (scalar field or wrapper element) decodes with an
			// over-maxlen reject that throws SofabError (MESSAGE_SPEC §7.1).
			if (fld.Kind == ir.KindString || fld.Kind == ir.KindBlob) && fld.HasMaxlen {
				use.maxlenField = true
			}
			if fld.Kind == ir.KindArray && arrayHasBoundedStrBlob(fld.Elem, fld.ElemItems, fld.ElemMaxHas) {
				use.maxlenField = true
			}
			// A narrow integer destination decodes with the over-width reject, which
			// throws SofabError (MESSAGE_SPEC §7.1, generator#266). Scalar fields and
			// native array elements alike — nested rows reach this through their own
			// field, so the element check follows the same recursion as the bounds
			// above.
			if ir.IsNarrow(fld.Kind) {
				use.narrowInt = true
			}
			if fld.Kind == ir.KindArray && arrayHasNarrowInt(fld.Elem, fld.ElemItems) {
				use.narrowInt = true
			}
			// The fp32 raw-bits channel (§4.6): the scalar half widens the wire bytes
			// through _fp32FromRaw, the array half re-renders them through
			// _fp32ArrayRaw. Each helper is emitted only where its position occurs.
			if fld.Kind == ir.KindFP32 {
				use.fp32Raw = true
			}
			if fld.Kind == ir.KindArray && fld.Elem == ir.KindFP32 {
				// The array half re-renders through _fp32ArrayRaw on encode and widens
				// each element through the scalar helper _fp32FromRaw on decode, so an
				// fp32 ARRAY needs both halves even when the schema has no fp32 scalar.
				use.fp32Raw = true
				use.fp32ArrRaw = true
			}
			if fld.Kind == ir.KindArray && nativeArrayElem(fld.Elem) {
				// A `count: N` native array decodes with the over-count reject, which
				// throws SofabError (generator#100).
				if fld.HasCount {
					use.countedArr = true
				}
				if _, ok := g.nativeArrayDefault(fld); ok {
					if g.longBacked(fld) {
						use.longArrEq = true
					} else {
						use.elemEq = true
					}
				}
			}
		}
	}
	for _, key := range s.NamedOrder {
		nt := s.Named[key]
		if nt.Category == ir.CatStruct || nt.Category == ir.CatUnion {
			scan(nt.Fields)
		}
	}
	for _, m := range s.Messages {
		scan(m.Fields)
	}
	return use
}

// emitDoc writes a TSDoc/JSDoc `/** ... */` block immediately before the
// declaration it documents, at the given indent. Single-line text becomes
// `/** text */`; multi-line text becomes a starred block. Any `*/` inside the
// text is defanged to `* /` so it cannot close the comment early. Empty text
// emits nothing, so it never leaves a dangling comment.
func (f *tsfile) emitDoc(indent, text string) {
	if text == "" {
		return
	}
	text = strings.ReplaceAll(text, "*/", "* /")
	lines := strings.Split(text, "\n")
	if len(lines) == 1 {
		f.line("%s/** %s */", indent, lines[0])
		return
	}
	f.line("%s/**", indent)
	for _, ln := range lines {
		if ln == "" {
			f.line("%s *", indent)
		} else {
			f.line("%s * %s", indent, ln)
		}
	}
	f.line("%s */", indent)
}

// fieldDoc builds a field's TSDoc text from its Description and Unit: the
// description with " (unit: <Unit>)" appended when a unit is set, or just
// "(unit: <Unit>)" when only a unit is present. A deprecated field appends an
// `@deprecated` JSDoc tag on its own line so the doc tool flags callers (tsc
// does not error on the generated code's own internal use, so no local
// suppression is needed). Empty when there is nothing to document.
func fieldDoc(fld *ir.Field, note string) string {
	var doc string
	switch {
	case fld.Description != "" && fld.Unit != "":
		doc = fld.Description + " (unit: " + fld.Unit + ")"
	case fld.Description != "":
		doc = fld.Description
	case fld.Unit != "":
		doc = "(unit: " + fld.Unit + ")"
	}
	doc = generator.AppendDoc(doc, note)
	if fld.Deprecated {
		if doc != "" {
			doc += "\n"
		}
		doc += "@deprecated"
	}
	return doc
}

// flagDoc builds a bitfield flag's TSDoc text: its Description, with a
// " (default: true)" / " (default: false)" note appended when the flag carries
// a schema default. Empty when there is nothing to document.
func flagDoc(fl *ir.BitfieldFlag) string {
	doc := fl.Description
	if fl.HasDefault {
		if doc != "" {
			doc += " "
		}
		if fl.Default {
			doc += "(default: true)"
		} else {
			doc += "(default: false)"
		}
	}
	return doc
}

func (g *gen) tsType(f *ir.Field) string {
	switch f.Kind {
	case ir.KindU64, ir.KindI64:
		if g.numberScalars() {
			return "number"
		}
		if g.longScalars() {
			return "Long"
		}
		return "bigint"
	case ir.KindU8, ir.KindU16, ir.KindU32, ir.KindI8, ir.KindI16, ir.KindI32, ir.KindBitfield, ir.KindFP32, ir.KindFP64:
		return "number"
	case ir.KindBool:
		return "boolean"
	case ir.KindString:
		return "string"
	case ir.KindBlob:
		return "Uint8Array"
	case ir.KindEnum, ir.KindStruct, ir.KindUnion:
		return g.typeName(f.Ref.Key)
	case ir.KindArray:
		return g.tsArrayType(f.Elem, f.ElemRef, f.ElemItems)
	}
	return "unknown"
}

// tsArrayType returns the `T[]` member type for an array element, recursing for
// nested arrays (array-of-array -> T[][]).
func (g *gen) tsArrayType(elem ir.Kind, ref *ir.TypeRef, items *ir.ArrayElem) string {
	switch elem {
	case ir.KindString:
		return "string[]"
	case ir.KindBlob:
		return "Uint8Array[]"
	case ir.KindU64, ir.KindI64:
		if g.longArrays() {
			return "Long[]"
		}
		return "bigint[]"
	case ir.KindBool:
		return "boolean[]"
	case ir.KindEnum, ir.KindStruct, ir.KindUnion:
		return g.typeName(ref.Key) + "[]"
	case ir.KindArray:
		return g.tsArrayType(items.Elem, items.ElemRef, items.ElemItems) + "[]"
	default: // integers, bitfield
		return "number[]"
	}
}

func (g *gen) tsDefault(f *ir.Field) string {
	switch f.Kind {
	case ir.KindU64, ir.KindI64:
		if g.numberScalars() {
			if f.Default != nil {
				return scalarLit(f.Default)
			}
			return "0"
		}
		if g.longScalars() {
			// The zero default is the overwhelmingly common case, and Long is
			// immutable: hand out the shared Long.ZERO rather than running
			// Long.fromValue(0n)'s bigint arithmetic once per constructed object.
			if f.Default == nil || scalarLit(f.Default) == "0" {
				return "Long.ZERO"
			}
			return "Long.fromValue(" + scalarLit(f.Default) + "n)"
		}
		if f.Default != nil {
			return scalarLit(f.Default) + "n"
		}
		return "0n"
	case ir.KindU8, ir.KindU16, ir.KindU32, ir.KindI8, ir.KindI16, ir.KindI32:
		if f.Default != nil {
			return scalarLit(f.Default)
		}
		return "0"
	case ir.KindBitfield:
		return fmt.Sprintf("%d", g.bitfieldDefault(f))
	case ir.KindFP32, ir.KindFP64:
		if f.Default != nil {
			return fmt.Sprintf("%v", f.Default)
		}
		return "0"
	case ir.KindBool:
		if b, ok := f.Default.(bool); ok && b {
			return "true"
		}
		return "false"
	case ir.KindString:
		if s, ok := f.Default.(string); ok {
			return fmt.Sprintf("%q", s)
		}
		return `""`
	case ir.KindBlob:
		if s, ok := f.Default.(string); ok {
			if raw, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(s), "")); err == nil {
				return fmt.Sprintf("new Uint8Array(%s)", intListLit(raw))
			}
		}
		return "new Uint8Array()"
	case ir.KindEnum:
		tn := g.typeName(f.Ref.Key)
		if f.Default != nil {
			if name, ok := g.enumMember(f.Ref.Target, f.Default); ok {
				return tn + "." + name
			}
			return fmt.Sprintf("(%s as %s)", scalarLit(f.Default), tn)
		}
		return fmt.Sprintf("(0 as %s)", tn)
	case ir.KindStruct, ir.KindUnion:
		return "new " + g.typeName(f.Ref.Key) + "()"
	case ir.KindArray:
		// A native scalar array is a leaf field: materialize its schema default so
		// an omitted (default-valued) array reconstructs correctly on decode and so
		// marshal can compare against it. Composite arrays are wrapper sequences
		// whose declared default is not materialized, so they start empty -- which is
		// what makes their dropping closer correct (§2).
		//
		// A declared `count: N` adds nothing on either side. `count` is a CAPACITY,
		// not a length (MESSAGE_SPEC §3): a fresh count:N array is the EMPTY array,
		// not N element defaults, and a `default` shorter than N stands for itself
		// rather than being padded out to N. That is also what the field's omit test
		// compares against, and what an absent field decodes back to.
		if nativeArrayElem(f.Elem) {
			if lit, ok := g.nativeArrayDefault(f); ok {
				return lit
			}
		}
		return "[]"
	}
	return "undefined as never"
}

// nativeArrayDefault renders a native scalar array's default as a TS array
// literal; ("", false) when there is none. u64/i64 elements are bigint literals
// (Long.fromValue under the Long-backed modes), enum elements are cast to the
// enum type, booleans/floats/integers are their JSON-native form.
//
// Not padded to a declared `count: N`: that is a capacity, not a length
// (MESSAGE_SPEC §3), so the default stands exactly as written — and so does the
// value it is compared against, which is what keeps a length-N all-zero array
// distinct from the empty one. A counted array with no schema default therefore
// has no default beyond the empty collection.
func (g *gen) nativeArrayDefault(f *ir.Field) (string, bool) {
	vals, ok := f.Default.([]any)
	if !ok {
		return "", false
	}
	parts := make([]string, len(vals))
	for i, v := range vals {
		switch f.Elem {
		case ir.KindU64, ir.KindI64:
			if g.longArrays() {
				// The zero default is by far the common case (every fixed-count
				// array's elided trailing run, and all-zero defaults); reuse the
				// shared immutable Long.ZERO instead of Long.fromValue(0n), which
				// would run bigint arithmetic per element on construction.
				if scalarLit(v) == "0" {
					parts[i] = "Long.ZERO"
				} else {
					parts[i] = "Long.fromValue(" + scalarLit(v) + "n)"
				}
			} else {
				parts[i] = scalarLit(v) + "n"
			}
		case ir.KindBool:
			if b, ok := v.(bool); ok && b {
				parts[i] = "true"
			} else {
				parts[i] = "false"
			}
		case ir.KindFP32, ir.KindFP64:
			parts[i] = fmt.Sprintf("%v", v)
		case ir.KindEnum:
			parts[i] = fmt.Sprintf("(%s as %s)", scalarLit(v), g.typeName(f.ElemRef.Key))
		default: // u8/u16/u32, i8/i16/i32, bitfield
			parts[i] = scalarLit(v)
		}
	}
	return "[" + strings.Join(parts, ", ") + "]", true
}

func (g *gen) enumMember(nt *ir.NamedType, def any) (string, bool) {
	v, ok := asInt(def)
	if !ok {
		return "", false
	}
	for _, c := range nt.Consts {
		if c.Value == v {
			return exported(c.Name), true
		}
	}
	return "", false
}

func (g *gen) bitfieldDefault(f *ir.Field) uint64 {
	var bits uint64
	for _, fl := range f.Ref.Target.Flags {
		if fl.HasDefault && fl.Default {
			bits |= 1 << uint(fl.Pos)
		}
	}
	return bits
}

// longScalarIsDefault is the "this 64-bit scalar equals its default" test for a
// Long-backed scalar: a (low, high) word-pair compare, as longArrEq performs per
// element on the array side. `===` cannot serve — a Long is an object, so it
// compares identity — and the halves are computed HERE, at generation time, so
// the test allocates nothing per call (the array side's longArrEq(acc, [...])
// builds its default array per evaluation; a scalar need not).
//
// A default the literal parser cannot place in the 64-bit domain falls back to
// materialising it, which is correct and merely slower — and unreachable for
// anything validate() lets through.
func (g *gen) longScalarIsDefault(acc string, f *ir.Field) string {
	lo, hi, ok := longHalves(f)
	if !ok {
		return fmt.Sprintf("%s.toBigInt(%t) === %sn", acc, f.Kind == ir.KindI64, scalarLit(f.Default))
	}
	return fmt.Sprintf("%s.low === %d && %s.high === %d", acc, lo, acc, hi)
}

// longHalves renders a 64-bit scalar's declared default as the two unsigned
// 32-bit halves of its two's-complement bit pattern — the shape a Long holds it
// in. No default is the 64-bit zero (Long.ZERO).
func longHalves(f *ir.Field) (uint32, uint32, bool) {
	if f.Default == nil {
		return 0, 0, true
	}
	lit := scalarLit(f.Default)
	var bits uint64
	if f.Kind == ir.KindI64 {
		v, err := strconv.ParseInt(lit, 10, 64)
		if err != nil {
			return 0, 0, false
		}
		bits = uint64(v) // two's complement, the bit pattern a Long stores
	} else {
		v, err := strconv.ParseUint(lit, 10, 64)
		if err != nil {
			return 0, 0, false
		}
		bits = v
	}
	return uint32(bits), uint32(bits >> 32), true
}

func asInt(v any) (int64, bool) {
	switch x := v.(type) {
	case int:
		return int64(x), true
	case int64:
		return x, true
	case float64:
		return int64(x), true
	}
	return 0, false
}

func scalarLit(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprintf("%v", v)
}

func intListLit(b []byte) string {
	parts := make([]string, len(b))
	for i, x := range b {
		parts[i] = fmt.Sprintf("%d", x)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// ---- JSON (canonical: blob as number[], bigint as string for self round-trip) --

func (g *gen) emitJSON(f *tsfile, name string, fields []*ir.Field) {
	f.line("  toJSON(): Record<string, unknown> {")
	f.line("    return {")
	for _, fld := range fields {
		f.line("      %q: %s,", fld.Name, g.toJSONExpr(fld))
	}
	f.line("    };")
	f.line("  }")
	f.blank()
	f.line("  static fromJSON(d: Record<string, unknown>): %s {", name)
	f.line("    const o = new %s();", name)
	for _, fld := range fields {
		f.line("    if (%q in d) %s;", fld.Name, g.fromJSONStmt(fld))
	}
	f.line("    return o;")
	f.line("  }")
	f.blank()
}

func (g *gen) toJSONExpr(f *ir.Field) string {
	acc := g.storage("this", f)
	switch f.Kind {
	case ir.KindU64, ir.KindI64:
		if g.longScalars() {
			// A Long is a raw (low, high) bit pair; only the schema kind says how to
			// print it, so the signedness goes in — as on the array side.
			return fmt.Sprintf("%s.toString(%t)", acc, f.Kind == ir.KindI64)
		}
		return acc + ".toString()"
	case ir.KindBlob:
		return "Array.from(" + acc + ")"
	case ir.KindStruct, ir.KindUnion:
		return acc + ".toJSON()"
	case ir.KindArray:
		return g.tsArrayToJSON(acc, f.Elem, f.ElemRef, f.ElemItems, 0)
	default:
		return acc
	}
}

// tsArrayToJSON builds a JSON-able expression for an array value: u64/i64 -> string,
// blob -> number[], struct/union -> toJSON(); recurses for nested arrays. enum/
// bool/bitfield/numeric/string are already JSON-native (identity). Long-backed
// 64-bit elements pass their signedness to Long.toString (a Long is a raw
// (low, high) bit pair; only the schema kind says how to print it).
func (g *gen) tsArrayToJSON(val string, elem ir.Kind, ref *ir.TypeRef, items *ir.ArrayElem, depth int) string {
	x := fmt.Sprintf("_x%d", depth)
	switch elem {
	case ir.KindU64, ir.KindI64:
		if g.longArrays() {
			return fmt.Sprintf("%s.map((%s) => %s.toString(%t))", val, x, x, elem == ir.KindI64)
		}
		return fmt.Sprintf("%s.map((%s) => %s.toString())", val, x, x)
	case ir.KindBlob:
		return fmt.Sprintf("%s.map((%s) => Array.from(%s))", val, x, x)
	case ir.KindStruct, ir.KindUnion:
		return fmt.Sprintf("%s.map((%s) => %s.toJSON())", val, x, x)
	case ir.KindArray:
		return fmt.Sprintf("%s.map((%s) => %s)", val, x, g.tsArrayToJSON(x, items.Elem, items.ElemRef, items.ElemItems, depth+1))
	default:
		return val
	}
}

func (g *gen) fromJSONStmt(f *ir.Field) string {
	acc := "o." + f.Name
	src := fmt.Sprintf("d[%q]", f.Name)
	switch f.Kind {
	case ir.KindU64, ir.KindI64:
		if g.numberScalars() {
			return fmt.Sprintf("%s = Number(%s as string | number)", acc, src)
		}
		if g.longScalars() {
			// Through the SETTER (acc is the public name), which converts once —
			// fromJSON is off the hot path, so it goes the ergonomic way, as the
			// array side does. BigInt first: the canonical JSON form of a 64-bit
			// field is a decimal string, and Long.fromValue takes no string.
			return fmt.Sprintf("%s = Long.fromValue(BigInt(%s as string | number))", acc, src)
		}
		return fmt.Sprintf("%s = BigInt(%s as string | number)", acc, src)
	case ir.KindU8, ir.KindU16, ir.KindU32, ir.KindI8, ir.KindI16, ir.KindI32, ir.KindBitfield, ir.KindFP32, ir.KindFP64:
		return fmt.Sprintf("%s = %s as number", acc, src)
	case ir.KindBool:
		return fmt.Sprintf("%s = %s as boolean", acc, src)
	case ir.KindString:
		return fmt.Sprintf("%s = %s as string", acc, src)
	case ir.KindBlob:
		return fmt.Sprintf("%s = new Uint8Array(%s as number[])", acc, src)
	case ir.KindEnum:
		return fmt.Sprintf("%s = %s as number", acc, src)
	case ir.KindStruct, ir.KindUnion:
		return fmt.Sprintf("%s = %s.fromJSON(%s as Record<string, unknown>)", acc, g.typeName(f.Ref.Key), src)
	case ir.KindArray:
		return fmt.Sprintf("%s = %s", acc, g.tsArrayFromJSON(src, f.Elem, f.ElemRef, f.ElemItems, 0))
	}
	return acc + " = undefined as never"
}

// tsArrayFromJSON rebuilds an array from JSON: u64/i64 -> bigint, blob -> Uint8Array,
// struct/union -> fromJSON(); recurses for nested arrays. enum/bool/bitfield/numeric/
// string are plain casts.
func (g *gen) tsArrayFromJSON(src string, elem ir.Kind, ref *ir.TypeRef, items *ir.ArrayElem, depth int) string {
	x := fmt.Sprintf("_x%d", depth)
	switch elem {
	case ir.KindU64, ir.KindI64:
		return fmt.Sprintf("(%s as (string | number)[]).map((%s) => BigInt(%s))", src, x, x)
	case ir.KindBlob:
		return fmt.Sprintf("(%s as number[][]).map((%s) => new Uint8Array(%s))", src, x, x)
	case ir.KindStruct, ir.KindUnion:
		return fmt.Sprintf("(%s as Record<string, unknown>[]).map((%s) => %s.fromJSON(%s))", src, x, g.typeName(ref.Key), x)
	case ir.KindEnum:
		return fmt.Sprintf("%s as %s[]", src, g.typeName(ref.Key))
	case ir.KindArray:
		return fmt.Sprintf("(%s as unknown[]).map((%s) => %s)", src, x, g.tsArrayFromJSON(x, items.Elem, items.ElemRef, items.ElemItems, depth+1))
	case ir.KindBool:
		return fmt.Sprintf("%s as boolean[]", src)
	case ir.KindString:
		return fmt.Sprintf("%s as string[]", src)
	default:
		return fmt.Sprintf("%s as number[]", src)
	}
}
