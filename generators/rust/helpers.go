package rust

import (
	"encoding/base64"
	"fmt"
	"sort"
	"strings"

	"github.com/sofa-buffers/generator/internal/ir"
)

func cfgString(cfg map[string]any, key, dflt string) string {
	if v, ok := cfg[key].(string); ok && v != "" {
		return v
	}
	return dflt
}

func cfgBool(cfg map[string]any, key string) bool {
	b, _ := cfg[key].(bool)
	return b
}

// rustFieldDefault is the value used in a manual `impl Default` (schema default
// or type-zero) — needed so sparse-canonical decode reconstructs the right value.
func (g *gen) rustFieldDefault(f *ir.Field) string {
	switch f.Kind {
	case ir.KindString:
		if s, ok := f.Default.(string); ok {
			return fmt.Sprintf("%q.to_string()", s)
		}
		return "String::new()"
	case ir.KindBool:
		if b, ok := f.Default.(bool); ok && b {
			return "true"
		}
		return "false"
	case ir.KindFP32, ir.KindFP64:
		if f.Default != nil {
			return rustFloat(f.Default)
		}
		return "0.0"
	case ir.KindEnum, ir.KindBitfield, ir.KindU8, ir.KindU16, ir.KindU32, ir.KindU64,
		ir.KindI8, ir.KindI16, ir.KindI32, ir.KindI64:
		return g.rustIntDefault(f)
	case ir.KindBlob:
		// blob is a leaf: materialize its default so decode reconstructs it and
		// marshal can compare against it (empty Vec when there is no default).
		if lit, ok := g.rustBlobLiteral(f); ok {
			return lit
		}
		return "Vec::new()"
	case ir.KindArray:
		// A native scalar array is a leaf: materialize its schema default so an
		// omitted default array reconstructs correctly. A fixed-count native array
		// is a stack `[elem; N]`; a dynamic one stays a heap Vec. Composite arrays
		// are wrapper sequences (always framed) and stay an empty Vec.
		if elem, n, ok := g.fixedNativeArray(f); ok {
			return g.rustFixedArrayDefault(f, elem, n)
		}
		if isNativeArrayElem(f.Elem) {
			if lit, ok := g.rustNativeArrayLiteral(f); ok {
				return lit
			}
		}
		return "Vec::new()"
	default: // struct/union: all children default, so Default::default() is right
		return "Default::default()"
	}
}

// rustNativeArrayParts renders a native scalar array's schema default element
// list (comma-joined, no brackets); ("", false) when there is no default.
// Element literals are unconstrained and infer to the field's element type.
func (g *gen) rustNativeArrayParts(f *ir.Field) (string, bool) {
	vals, ok := f.Default.([]any)
	if !ok {
		return "", false
	}
	parts := make([]string, len(vals))
	for i, v := range vals {
		switch f.Elem {
		case ir.KindBool:
			parts[i] = fmt.Sprintf("%v", v)
		case ir.KindFP32, ir.KindFP64:
			parts[i] = rustFloat(v)
		default: // numeric / enum / bitfield (int64 or a decimal string)
			parts[i] = fmt.Sprintf("%v", v)
		}
	}
	return strings.Join(parts, ", "), true
}

// rustNativeArrayLiteral renders a native scalar array's schema default as a Rust
// `vec![...]` literal (for the dynamic/`Vec` path); ("", false) when no default.
func (g *gen) rustNativeArrayLiteral(f *ir.Field) (string, bool) {
	parts, ok := g.rustNativeArrayParts(f)
	if !ok {
		return "", false
	}
	return "vec![" + parts + "]", true
}

// rustBlobLiteral renders a blob field's base64 schema default as a Rust
// `vec![...]` of bytes; ("", false) when there is no (decodable) default.
func (g *gen) rustBlobLiteral(f *ir.Field) (string, bool) {
	s, ok := f.Default.(string)
	if !ok {
		return "", false
	}
	raw, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(s), ""))
	if err != nil {
		return "", false
	}
	parts := make([]string, len(raw))
	for i, b := range raw {
		parts[i] = fmt.Sprintf("%d", b)
	}
	return fmt.Sprintf("vec![%s]", strings.Join(parts, ", ")), true
}

// rustCompare is the RHS of `self.field != X` for omission.
func (g *gen) rustCompare(f *ir.Field) string {
	if f.Kind == ir.KindString {
		if s, ok := f.Default.(string); ok {
			return fmt.Sprintf("%q", s)
		}
		return `""`
	}
	return g.rustFieldDefault(f)
}

func (g *gen) rustIntDefault(f *ir.Field) string {
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
	s := fmt.Sprintf("%v", f.Default) // int64 or a decimal string (u64/i64)
	if f.Kind == ir.KindI64 && s == "-9223372036854775808" {
		return "i64::MIN" // the literal would overflow before negation
	}
	return s
}

func rustFloat(v any) string {
	s := fmt.Sprintf("%v", v)
	if !strings.ContainsAny(s, ".eE") {
		s += ".0"
	}
	return s
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

// fixedNativeArray reports whether an array field is a native-element array with
// a statically known length — the case that lowers to a fixed Rust array
// `[elem; N]` (stack, heap-free) instead of a heap `Vec<elem>`, mirroring the C++
// backend's `std::array<T, N>`. Returns the element Rust type and N. Native but
// count-less (dynamic) arrays, and composite-element arrays, keep `Vec`.
func (g *gen) fixedNativeArray(f *ir.Field) (elem string, n int64, ok bool) {
	if f.Kind != ir.KindArray || !isNativeArrayElem(f.Elem) || !f.HasCount {
		return "", 0, false
	}
	return g.rustArrayElem(f.Elem, f.ElemRef, f.ElemItems), f.Count, true
}

// rustFixedArrayDefault renders the Default value of a fixed native array
// `[elem; N]`. With a schema default it is an explicit array literal of exactly N
// elements — the given values, tail-padded with the element zero (matching the
// C++ `std::array` aggregate-init that zero-fills unspecified trailing elements,
// so both backends encode the same N elements). With no default it is the
// type-zero repeat literal (`[0; N]` / `[0.0; N]` / `[false; N]`).
func (g *gen) rustFixedArrayDefault(f *ir.Field, elem string, n int64) string {
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
				parts = append(parts, rustFloat(v))
			} else {
				parts = append(parts, fmt.Sprintf("%v", v))
			}
		}
		for int64(len(parts)) < n {
			parts = append(parts, zero)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	}
	return fmt.Sprintf("[%s; %d]", zero, n)
}

func (g *gen) rustType(f *ir.Field) string {
	switch f.Kind {
	case ir.KindU8, ir.KindU16, ir.KindU32, ir.KindU64, ir.KindI8, ir.KindI16, ir.KindI32, ir.KindI64:
		return numRustType(f.Kind)
	case ir.KindFP32:
		return "f32"
	case ir.KindFP64:
		return "f64"
	case ir.KindBool:
		return "bool"
	case ir.KindString:
		return "String"
	case ir.KindBlob:
		return "Vec<u8>"
	case ir.KindEnum:
		return enumBacking(f.Ref.Target)
	case ir.KindBitfield:
		return bitfieldBacking(f.Ref.Target)
	case ir.KindStruct, ir.KindUnion:
		return g.typeName(f.Ref.Key)
	case ir.KindArray:
		if elem, n, ok := g.fixedNativeArray(f); ok {
			return fmt.Sprintf("[%s; %d]", elem, n)
		}
		return "Vec<" + g.rustArrayElem(f.Elem, f.ElemRef, f.ElemItems) + ">"
	}
	return "()"
}

// rustArrayElem is the Rust type of an array element, recursing for nested
// arrays. Numeric/bool map to their scalar Rust type; enum/bitfield to their
// integer backing; struct/union to the shared type name; a nested array wraps
// another Vec level.
func (g *gen) rustArrayElem(elem ir.Kind, ref *ir.TypeRef, items *ir.ArrayElem) string {
	switch elem {
	case ir.KindString:
		return "String"
	case ir.KindBlob:
		return "Vec<u8>"
	case ir.KindBool:
		return "bool"
	case ir.KindEnum:
		return enumBacking(ref.Target)
	case ir.KindBitfield:
		return bitfieldBacking(ref.Target)
	case ir.KindStruct, ir.KindUnion:
		return g.typeName(ref.Key)
	case ir.KindArray:
		return "Vec<" + g.rustArrayElem(items.Elem, items.ElemRef, items.ElemItems) + ">"
	default: // numeric
		return numRustType(elem)
	}
}

func numRustType(k ir.Kind) string {
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

// capabilities returns the sofab features the schema needs, for require!() and
// the generated Cargo.toml.
func (g *gen) capabilities(s *ir.Schema) []string {
	caps := map[string]bool{}
	seen := map[string]bool{}
	var walk func(fields []*ir.Field)
	var walkArray func(elem ir.Kind, ref *ir.TypeRef, items *ir.ArrayElem)
	walk = func(fields []*ir.Field) {
		for _, f := range fields {
			switch f.Kind {
			case ir.KindString, ir.KindBlob, ir.KindFP32:
				caps["fixlen"] = true
			case ir.KindFP64:
				caps["fixlen"] = true
				caps["fp64"] = true
			case ir.KindU64, ir.KindI64:
				caps["value64"] = true
			case ir.KindStruct, ir.KindUnion:
				caps["sequence"] = true
				if !seen[f.Ref.Key] {
					seen[f.Ref.Key] = true
					walk(f.Ref.Target.Fields)
				}
			case ir.KindArray:
				walkArray(f.Elem, f.ElemRef, f.ElemItems)
			}
		}
	}
	// walkArray adds the capabilities an array element needs. Numeric/enum/bool/
	// bitfield elements use a native array; string/blob/struct/union/nested-array
	// elements lower to a wrapper sequence.
	walkArray = func(elem ir.Kind, ref *ir.TypeRef, items *ir.ArrayElem) {
		switch elem {
		case ir.KindString, ir.KindBlob:
			caps["sequence"] = true
			caps["fixlen"] = true
		case ir.KindStruct, ir.KindUnion:
			caps["sequence"] = true
			if !seen[ref.Key] {
				seen[ref.Key] = true
				walk(ref.Target.Fields)
			}
		case ir.KindArray:
			caps["sequence"] = true
			walkArray(items.Elem, items.ElemRef, items.ElemItems)
		case ir.KindFP64:
			caps["array"] = true
			caps["fixlen"] = true
			caps["fp64"] = true
		case ir.KindFP32:
			caps["array"] = true
			caps["fixlen"] = true
		case ir.KindU64, ir.KindI64:
			caps["array"] = true
			caps["value64"] = true
		case ir.KindBitfield:
			caps["array"] = true
			if bitfieldBacking(ref.Target) == "u64" {
				caps["value64"] = true
			}
		default: // small numeric, enum, bool
			caps["array"] = true
		}
	}
	for _, m := range s.Messages {
		walk(m.Fields)
	}
	out := make([]string, 0, len(caps))
	for c := range caps {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// ---- max-size cost model (PLAN §5.5) ----

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
// string/blob/struct/union/nested-array elements lower to a wrapper sequence. A
// dynamic (count == 0) wrapper-sequence array is unbounded, so it forces the
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

var _ = fmt.Sprintf

// rustKeywords are reserved words that, used verbatim as a struct field name,
// are a syntax error and must be written as a raw identifier (`r#name`). serde's
// derives strip the `r#` prefix, so JSON field names are unchanged.
var rustKeywords = map[string]bool{
	"as": true, "break": true, "const": true, "continue": true, "crate": true,
	"dyn": true, "else": true, "enum": true, "extern": true, "false": true,
	"fn": true, "for": true, "if": true, "impl": true, "in": true, "let": true,
	"loop": true, "match": true, "mod": true, "move": true, "mut": true,
	"pub": true, "ref": true, "return": true, "static": true, "struct": true,
	"trait": true, "true": true, "type": true, "unsafe": true, "use": true,
	"where": true, "while": true, "async": true, "await": true, "yield": true,
	"gen": true, "abstract": true, "become": true, "box": true, "do": true,
	"final": true, "macro": true, "override": true, "priv": true, "typeof": true,
	"unsized": true, "virtual": true, "try": true,
}

// rustNonRaw are the four keywords that CANNOT be written as raw identifiers
// (`r#self` etc. is rejected). A field with one of these names is mangled with a
// trailing underscore instead; rustNeedsRename then forces a serde rename so the
// JSON/wire name stays the original.
var rustNonRaw = map[string]bool{"self": true, "Self": true, "crate": true, "super": true}

// rustIdent renders a schema field name as a Rust identifier: `r#name` for a
// keyword, `name_` for the four non-raw-able keywords, else unchanged.
func rustIdent(name string) string {
	if rustNonRaw[name] {
		return name + "_"
	}
	if rustKeywords[name] {
		return "r#" + name
	}
	return name
}

// rustNeedsRename reports whether a field needs a serde rename to preserve its
// JSON name — true only for the underscore-mangled non-raw-able keywords (serde
// already strips `r#`, so r#-escaped fields don't need it).
func rustNeedsRename(name string) bool { return rustNonRaw[name] }
