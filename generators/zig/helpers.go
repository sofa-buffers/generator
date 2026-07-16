package zig

import (
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/sofa-buffers/generator/internal/ir"
)

func cfgString(cfg map[string]any, key, dflt string) string {
	if v, ok := cfg[key].(string); ok && v != "" {
		return v
	}
	return dflt
}

// cfgLimit reads an integer decode-limit key (generator#102). YAML/JSON decode
// integers into different Go types depending on the path, so all are accepted.
func cfgLimit(cfg map[string]any, key string) (int64, bool) {
	switch v := cfg[key].(type) {
	case int:
		return int64(v), true
	case int64:
		return v, true
	case uint64:
		return int64(v), true
	case float64:
		return int64(v), true
	}
	return 0, false
}

// zigKeywords are reserved words that, used verbatim as a struct field name,
// are a syntax error and must be written as a quoted identifier (@"name").
// Primitive type names (u8, bool, type, ...) are NOT keywords in field
// position, so they stay unescaped.
var zigKeywords = map[string]bool{
	"addrspace": true, "align": true, "allowzero": true, "and": true,
	"anyframe": true, "anytype": true, "asm": true, "async": true,
	"await": true, "break": true, "callconv": true, "catch": true,
	"comptime": true, "const": true, "continue": true, "defer": true,
	"else": true, "enum": true, "errdefer": true, "error": true,
	"export": true, "extern": true, "fn": true, "for": true, "if": true,
	"inline": true, "linksection": true, "noalias": true, "noinline": true,
	"nosuspend": true, "opaque": true, "or": true, "orelse": true,
	"packed": true, "pub": true, "resume": true, "return": true,
	"struct": true, "suspend": true, "switch": true, "test": true,
	"threadlocal": true, "try": true, "union": true, "unreachable": true,
	"usingnamespace": true, "var": true, "volatile": true, "while": true,
	"true": true, "false": true, "null": true, "undefined": true,
}

// zigDeclClash are field names that would collide with the declarations every
// generated struct carries (Zig forbids a field and a decl sharing a name).
// They are mangled with a trailing underscore; the wire (keyed by id) and the
// JSON name (emitted from the schema name) are unaffected.
var zigDeclClash = map[string]bool{
	"marshal": true, "encode": true, "decode": true, "MAX_SIZE": true,
}

// zigIdent renders a schema field name as a Zig identifier: @"name" for a
// keyword, name_ for a decl-clashing name, else unchanged.
func zigIdent(name string) string {
	if zigDeclClash[name] {
		return name + "_"
	}
	if zigKeywords[name] {
		return `@"` + name + `"`
	}
	return name
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

// isWrapperElem reports whether an array element lowers to a wrapper sequence
// (vs a native array), i.e. it needs its own decode frame.
func isWrapperElem(k ir.Kind) bool {
	switch k {
	case ir.KindString, ir.KindBlob, ir.KindStruct, ir.KindUnion, ir.KindArray:
		return true
	}
	return false
}

// isNativeArrayElem reports whether an array element uses a native array wire
// type (numeric/fp/enum/boolean/bitfield), delivered via arrayBegin + scalar
// callbacks rather than a wrapper sequence.
func isNativeArrayElem(k ir.Kind) bool {
	switch k {
	case ir.KindU8, ir.KindU16, ir.KindU32, ir.KindU64,
		ir.KindI8, ir.KindI16, ir.KindI32, ir.KindI64,
		ir.KindFP32, ir.KindFP64, ir.KindEnum, ir.KindBool, ir.KindBitfield:
		return true
	}
	return false
}

func isUnsignedElem(k ir.Kind) bool {
	return k == ir.KindU8 || k == ir.KindU16 || k == ir.KindU32 || k == ir.KindU64
}
func isSignedElem(k ir.Kind) bool {
	return k == ir.KindI8 || k == ir.KindI16 || k == ir.KindI32 || k == ir.KindI64
}

func numZigType(k ir.Kind) string {
	switch k {
	case ir.KindU8:
		return "u8"
	case ir.KindU16:
		return "u16"
	case ir.KindU32:
		return "u32"
	case ir.KindU64:
		return "u64"
	case ir.KindI8:
		return "i8"
	case ir.KindI16:
		return "i16"
	case ir.KindI32:
		return "i32"
	case ir.KindI64:
		return "i64"
	case ir.KindFP32:
		return "f32"
	case ir.KindFP64:
		return "f64"
	}
	return "u8"
}

// enumBacking is the narrowest signed integer type that holds every member
// value (same rule as the Rust backend, so the two ports agree).
func enumBacking(nt *ir.NamedType) string {
	var lo, hi int64
	for _, c := range nt.Consts {
		if c.Value < lo {
			lo = c.Value
		}
		if c.Value > hi {
			hi = c.Value
		}
	}
	switch {
	case lo >= -128 && hi <= 127:
		return "i8"
	case lo >= -32768 && hi <= 32767:
		return "i16"
	default:
		return "i32"
	}
}

func bitfieldBacking(nt *ir.NamedType) string {
	var max int64
	for _, fl := range nt.Flags {
		if fl.Pos > max {
			max = fl.Pos
		}
	}
	switch {
	case max <= 7:
		return "u8"
	case max <= 15:
		return "u16"
	case max <= 31:
		return "u32"
	default:
		return "u64"
	}
}

// fixedNativeArray reports whether an array field is a native-element array
// with a statically known length -- the case that lowers to a fixed Zig array
// [N]T (stack, allocation-free) instead of a heap slice. Returns the element
// Zig type and N.
func (g *gen) fixedNativeArray(f *ir.Field) (elem string, n int64, ok bool) {
	if f.Kind != ir.KindArray || !isNativeArrayElem(f.Elem) || !f.HasCount {
		return "", 0, false
	}
	return g.zigArrayElem(f.Elem, f.ElemRef, f.ElemItems), f.Count, true
}

func (g *gen) zigType(f *ir.Field) string {
	switch f.Kind {
	case ir.KindU8, ir.KindU16, ir.KindU32, ir.KindU64,
		ir.KindI8, ir.KindI16, ir.KindI32, ir.KindI64, ir.KindFP32, ir.KindFP64:
		return numZigType(f.Kind)
	case ir.KindBool:
		return "bool"
	case ir.KindString, ir.KindBlob:
		return "[]const u8"
	case ir.KindEnum:
		return enumBacking(f.Ref.Target)
	case ir.KindBitfield:
		return bitfieldBacking(f.Ref.Target)
	case ir.KindStruct, ir.KindUnion:
		return g.typeName(f.Ref.Key)
	case ir.KindArray:
		if elem, n, ok := g.fixedNativeArray(f); ok {
			return fmt.Sprintf("[%d]%s", n, elem)
		}
		return "[]const " + g.zigArrayElem(f.Elem, f.ElemRef, f.ElemItems)
	}
	return "void"
}

// zigArrayElem is the Zig type of an array element, recursing for nested
// arrays. Numeric/bool map to their scalar Zig type; enum/bitfield to their
// integer backing; struct/union to the shared type name; a nested array is
// always a slice (only a direct field lowers to a fixed [N]T).
func (g *gen) zigArrayElem(elem ir.Kind, ref *ir.TypeRef, items *ir.ArrayElem) string {
	switch elem {
	case ir.KindString, ir.KindBlob:
		return "[]const u8"
	case ir.KindBool:
		return "bool"
	case ir.KindEnum:
		return enumBacking(ref.Target)
	case ir.KindBitfield:
		return bitfieldBacking(ref.Target)
	case ir.KindStruct, ir.KindUnion:
		return g.typeName(ref.Key)
	case ir.KindArray:
		return "[]const " + g.zigArrayElem(items.Elem, items.ElemRef, items.ElemItems)
	default: // numeric
		return numZigType(elem)
	}
}

// ---- defaults --------------------------------------------------------------

// zigFieldDefault is the field initializer in the generated struct declaration
// (schema default or type zero) -- needed so sparse-canonical decode
// reconstructs the right value from a plain `.{}` message.
func (g *gen) zigFieldDefault(f *ir.Field) string {
	switch f.Kind {
	case ir.KindString:
		if lit, ok := f.Default.(string); ok {
			return fmt.Sprintf("%q", lit)
		}
		return `""`
	case ir.KindBool:
		if b, ok := f.Default.(bool); ok && b {
			return "true"
		}
		return "false"
	case ir.KindFP32, ir.KindFP64:
		if f.Default != nil {
			return zigFloat(f.Default)
		}
		return "0.0"
	case ir.KindEnum, ir.KindBitfield, ir.KindU8, ir.KindU16, ir.KindU32, ir.KindU64,
		ir.KindI8, ir.KindI16, ir.KindI32, ir.KindI64:
		return g.zigIntDefault(f)
	case ir.KindBlob:
		// blob is a leaf: materialize its default so decode reconstructs it and
		// marshal can compare against it (empty slice when there is no default).
		if raw, ok := g.blobBytes(f); ok {
			return byteSliceLit(raw)
		}
		return `""`
	case ir.KindArray:
		// A native scalar array is a leaf: materialize its schema default so an
		// omitted default array reconstructs correctly. A fixed-count native
		// array is a stack [N]T; a dynamic one stays a slice. Composite arrays
		// are wrapper sequences (always framed) and stay an empty slice.
		if elem, n, ok := g.fixedNativeArray(f); ok {
			return g.zigFixedArrayDefault(f, elem, n)
		}
		if isNativeArrayElem(f.Elem) {
			if parts, ok := g.zigNativeArrayParts(f); ok {
				return "&.{ " + parts + " }"
			}
		}
		return "&.{}"
	default: // struct/union: all children default, so .{} is right
		return ".{}"
	}
}

// zigNativeArrayParts renders a native scalar array's schema default element
// list (comma-joined, no brackets); ("", false) when there is no default.
func (g *gen) zigNativeArrayParts(f *ir.Field) (string, bool) {
	vals, ok := f.Default.([]any)
	if !ok {
		return "", false
	}
	parts := make([]string, len(vals))
	for i, v := range vals {
		parts[i] = zigElemLit(f.Elem, v)
	}
	return strings.Join(parts, ", "), true
}

// zigElemLit renders one native array element literal: fp gets a decimal point,
// bool renders as true/false, numeric/enum/bitfield as an int64 or a decimal
// string.
func zigElemLit(elem ir.Kind, v any) string {
	switch elem {
	case ir.KindFP32, ir.KindFP64:
		return zigFloat(v)
	default:
		return fmt.Sprintf("%v", v)
	}
}

// zigFixedArrayNeedsReset reports whether a fixed native array field's decode
// must clear the destination on arrayBegin before the wire elements land.
//
// A `count: N` array decodes to exactly N elements: M from the wire, the ELEMENT
// default (zero) at [M,N) (MESSAGE_SPEC S3). The [N]T destination starts at the
// field's declaration default, so with a non-zero SCHEMA default the untouched
// tail would wrongly keep that schema default: with `default: [1,2,3]` on
// `count: 5`, a value of [1,2,0,0,0] encodes (trimmed) to the 2-element wire
// [1,2] and would decode back as [1,2,3,0,0] -- a corrupted round-trip. Clearing
// first makes the tail the element default, matching the other backends.
//
// A field with no schema default (or an all-zero one) already declares an
// all-zero array, so it needs no reset and its generated code is unchanged.
func (g *gen) zigFixedArrayNeedsReset(f *ir.Field) bool {
	if _, _, ok := g.fixedNativeArray(f); !ok {
		return false
	}
	vals, ok := f.Default.([]any)
	if !ok {
		return false
	}
	zero := zigElemZero(f.Elem)
	for _, v := range vals {
		if zigElemLit(f.Elem, v) != zero {
			return true
		}
	}
	return false
}

// zigFixedArrayDefault renders the initializer of a fixed native array [N]T.
// With a schema default it is an explicit N-element literal -- the given
// values, tail-padded with the element zero (matching the Rust/C++ backends,
// so every port encodes the same N elements). With no default it is the
// splat of the element zero.
func (g *gen) zigFixedArrayDefault(f *ir.Field, elem string, n int64) string {
	zero := "0"
	switch f.Elem {
	case ir.KindFP32, ir.KindFP64:
		zero = "0.0"
	case ir.KindBool:
		zero = "false"
	}
	if vals, ok := f.Default.([]any); ok {
		parts := make([]string, 0, n)
		for _, v := range vals {
			if f.Elem == ir.KindFP32 || f.Elem == ir.KindFP64 {
				parts = append(parts, zigFloat(v))
			} else {
				parts = append(parts, fmt.Sprintf("%v", v))
			}
		}
		for int64(len(parts)) < n {
			parts = append(parts, zero)
		}
		return ".{ " + strings.Join(parts, ", ") + " }"
	}
	return fmt.Sprintf("@splat(%s)", zero)
}

func (g *gen) zigIntDefault(f *ir.Field) string {
	if f.Kind == ir.KindBitfield {
		var bits uint64
		for _, fl := range f.Ref.Target.Flags {
			if fl.HasDefault && fl.Default {
				bits |= 1 << uint(fl.Pos)
			}
		}
		return fmt.Sprintf("%d", bits)
	}
	if f.Default == nil {
		return "0"
	}
	// int64 or a decimal string (u64/i64); Zig integer literals are arbitrary
	// precision at comptime, so i64 MIN needs no special case.
	return fmt.Sprintf("%v", f.Default)
}

func zigFloat(v any) string {
	s := fmt.Sprintf("%v", v)
	if !strings.ContainsAny(s, ".eE") {
		s += ".0"
	}
	return s
}

// zigElemZero is the zero literal of a native array element kind.
func zigElemZero(k ir.Kind) string {
	switch k {
	case ir.KindFP32, ir.KindFP64:
		return "0.0"
	case ir.KindBool:
		return "false"
	}
	return "0"
}

// blobBytes decodes a blob field's base64 schema default; (nil, false) when
// there is no (decodable) default.
func (g *gen) blobBytes(f *ir.Field) ([]byte, bool) {
	s, ok := f.Default.(string)
	if !ok {
		return nil, false
	}
	raw, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(s), ""))
	if err != nil {
		return nil, false
	}
	return raw, true
}

// byteSliceLit renders bytes as a Zig slice literal `&.{ 10, 20, 30 }` (an
// empty string literal for no bytes).
func byteSliceLit(raw []byte) string {
	if len(raw) == 0 {
		return `""`
	}
	parts := make([]string, len(raw))
	for i, b := range raw {
		parts[i] = fmt.Sprintf("%d", b)
	}
	return "&.{ " + strings.Join(parts, ", ") + " }"
}

// zigLeafNe is the boolean omit-guard `<lhs> != <default>` for a scalar/string
// leaf field. Strings/blobs compare as slices (std.mem.eql), with a shorter
// .len check against an empty default.
func (g *gen) zigLeafNe(acc string, f *ir.Field) string {
	switch f.Kind {
	case ir.KindString:
		lit, _ := f.Default.(string)
		if lit == "" {
			return fmt.Sprintf("%s.len != 0", acc)
		}
		return fmt.Sprintf("!std.mem.eql(u8, %s, %q)", acc, lit)
	case ir.KindBlob:
		if raw, ok := g.blobBytes(f); ok && len(raw) > 0 {
			return fmt.Sprintf("!std.mem.eql(u8, %s, %s)", acc, byteSliceLit(raw))
		}
		return fmt.Sprintf("%s.len != 0", acc)
	}
	return fmt.Sprintf("%s != %s", acc, g.zigFieldDefault(f))
}

// ---- max-size cost model (mirrors the Rust backend, PLAN 5.5) --------------

func (g *gen) maxSize(fields []*ir.Field) (int64, bool) {
	var total int64
	seen := map[string]bool{}
	for _, f := range fields {
		c, ok := g.fieldCost(f, seen)
		if !ok {
			return 8192, true
		}
		total += c
	}
	if total < 64 {
		total = 64
	}
	return total, true
}

func (g *gen) fieldCost(f *ir.Field, seen map[string]bool) (int64, bool) {
	hdr := varintLen(uint64(f.ID)<<3 | 7)
	switch f.Kind {
	case ir.KindU8, ir.KindU16, ir.KindU32, ir.KindU64,
		ir.KindI8, ir.KindI16, ir.KindI32, ir.KindI64, ir.KindBool, ir.KindEnum, ir.KindBitfield:
		return hdr + 10, true
	case ir.KindFP32:
		return hdr + 1 + 4, true
	case ir.KindFP64:
		return hdr + 1 + 8, true
	case ir.KindString, ir.KindBlob:
		if !f.HasMaxlen {
			return 0, false
		}
		return hdr + varintLen(uint64(f.Maxlen)<<3) + f.Maxlen, true
	case ir.KindArray:
		return g.arrayCost(hdr, f.Elem, f.ElemRef, f.ElemItems, f.Count, f.ElemMaxHas, f.ElemMax, seen)
	case ir.KindStruct, ir.KindUnion:
		if seen[f.Ref.Key] {
			return 0, false
		}
		seen[f.Ref.Key] = true
		var inner int64
		for _, c := range f.Ref.Target.Fields {
			cc, ok := g.fieldCost(c, seen)
			if !ok {
				delete(seen, f.Ref.Key)
				return 0, false
			}
			inner += cc
		}
		delete(seen, f.Ref.Key)
		return hdr + inner + 1, true
	}
	return hdr, true
}

// arrayCost bounds the encoded size of an array field body (hdr is the field
// header). Native (numeric/enum/bool/bitfield) elements use a native array;
// string/blob/struct/union/nested-array elements lower to a wrapper sequence.
// A dynamic (count == 0) wrapper-sequence array is unbounded, so it forces the
// default cap.
func (g *gen) arrayCost(hdr int64, elem ir.Kind, ref *ir.TypeRef, items *ir.ArrayElem, count int64, elemMaxHas bool, elemMax int64, seen map[string]bool) (int64, bool) {
	ihdr := varintLen(uint64(count)<<3 | 7) // per-element wrapper/child header estimate
	switch elem {
	case ir.KindString, ir.KindBlob:
		if !elemMaxHas || count == 0 {
			return 0, false
		}
		per := ihdr + varintLen(uint64(elemMax)<<3) + elemMax
		return hdr + 1 + count*per + 1, true
	case ir.KindStruct, ir.KindUnion:
		if count == 0 || seen[ref.Key] {
			return 0, false
		}
		seen[ref.Key] = true
		var inner int64
		for _, c := range ref.Target.Fields {
			cc, ok := g.fieldCost(c, seen)
			if !ok {
				delete(seen, ref.Key)
				return 0, false
			}
			inner += cc
		}
		delete(seen, ref.Key)
		per := ihdr + inner + 1 // element sequence header + body + sequence end
		return hdr + 1 + count*per + 1, true
	case ir.KindArray:
		if count == 0 {
			return 0, false
		}
		cc, ok := g.arrayCost(ihdr, items.Elem, items.ElemRef, items.ElemItems, items.Count, items.ElemMaxHas, items.ElemMax, seen)
		if !ok {
			return 0, false
		}
		return hdr + 1 + count*cc + 1, true
	default: // numeric / enum / bool / bitfield -> native array
		return hdr + varintLen(uint64(count)) + count*10, true
	}
}

func varintLen(x uint64) int64 {
	n := int64(1)
	for x >= 0x80 {
		x >>= 7
		n++
	}
	return n
}
