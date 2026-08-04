package rust

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

func cfgBool(cfg map[string]any, key string) bool {
	b, _ := cfg[key].(bool)
	return b
}

func cfgBoolDefault(cfg map[string]any, key string, dflt bool) bool {
	if b, ok := cfg[key].(bool); ok {
		return b
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

// checkBounded enforces the no_std profile's sizing policy (the Rust analog of the
// C++ c-cpp checkBounded): every field must be sized by the schema. A string/blob
// needs a maxlen; an array needs a count (and a string/blob element needs its own
// maxlen). An unbounded such field is a hard error.
//
// The bound is mandatory in BOTH storage modes: allow_dynamic chooses the
// container a *bounded* field lives in, never whether a bound is needed. That is
// what keeps one schema valid for every no_std target — same maxlen/count, same
// wire bytes, only the storage differs — so the switch can be flipped per device
// without touching the schema.
func (g *gen) checkBounded(s *ir.Schema) error {
	seen := map[string]bool{}
	var walk func(owner string, fields []*ir.Field) error
	walk = func(owner string, fields []*ir.Field) error {
		for _, f := range fields {
			if err := g.checkField(owner, f, seen, walk); err != nil {
				return err
			}
		}
		return nil
	}
	for _, m := range s.Messages {
		if err := walk(m.Name, m.Fields); err != nil {
			return err
		}
	}
	return nil
}

func (g *gen) checkField(owner string, f *ir.Field, seen map[string]bool, walk func(string, []*ir.Field) error) error {
	switch f.Kind {
	case ir.KindString, ir.KindBlob:
		if !f.HasMaxlen {
			return fmt.Errorf("no_std: field %q of %q is an unbounded %s (no maxlen); add a maxlen. The bound is required in both storage modes - allow_dynamic chooses the container, not whether a bound is needed (use corelib: rs for genuinely unbounded fields)", f.Name, owner, kindName(f.Kind))
		}
	case ir.KindArray:
		if !f.HasCount {
			return fmt.Errorf("no_std: array field %q of %q has no count; add a count. The bound is required in both storage modes (use corelib: rs for genuinely unbounded fields)", f.Name, owner)
		}
		if (f.Elem == ir.KindString || f.Elem == ir.KindBlob) && !f.ElemMaxHas {
			return fmt.Errorf("no_std: %s-array field %q of %q has no element maxlen; add items.maxlen. The bound is required in both storage modes (use corelib: rs for genuinely unbounded fields)", kindName(f.Elem), f.Name, owner)
		}
	case ir.KindStruct, ir.KindUnion:
		if !seen[f.Ref.Key] {
			seen[f.Ref.Key] = true
			return walk(f.Ref.Key, f.Ref.Target.Fields)
		}
	}
	return nil
}

func kindName(k ir.Kind) string {
	switch k {
	case ir.KindString:
		return "string"
	case ir.KindBlob:
		return "blob"
	}
	return "field"
}

// rustFieldDefault is the value used in a manual `impl Default` (schema default
// or type-zero) — needed so sparse-canonical decode reconstructs the right value.
func (g *gen) rustFieldDefault(f *ir.Field) string {
	switch f.Kind {
	case ir.KindString:
		lit, hasLit := f.Default.(string)
		if g.noStd || g.staticStore {
			return g.rustStrNew(f.HasMaxlen, lit, hasLit)
		}
		if hasLit {
			return fmt.Sprintf("%q.to_string()", lit)
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
		// serialize can compare against it (empty container when there is no default).
		if raw, ok := g.blobBytes(f); ok {
			return g.rustBlobNew(f.HasMaxlen, byteSliceLit(raw), true)
		}
		return g.rustBlobNew(f.HasMaxlen, "", false)
	case ir.KindArray:
		// A native scalar array is a leaf: materialize its schema default so an
		// omitted default array reconstructs correctly.
		//
		// A declared `count: N` materializes NOTHING. It is a CAPACITY, not a
		// length (MESSAGE_SPEC §3): it sizes the container's capacity and bounds
		// the decode, but it never adds elements, so a fresh `count: N` array --
		// native or wrapper -- is the EMPTY array, and a declared `default`
		// shorter than N stands exactly as written rather than being padded out
		// to N. That is also what the field's omit test compares against, and
		// what an absent field decodes back to.
		if isNativeArrayElem(f.Elem) {
			if parts, ok := g.rustNativeArrayParts(f); ok {
				switch {
				case g.staticStore && f.HasCount:
					// heapless::Vec<T, N>: fill by extend_from_slice, the same
					// heap-free builder shape rustBlobNew uses.
					return fmt.Sprintf("{ let mut _v = %s; let _ = _v.extend_from_slice(&[%s]); _v }", g.rustSeqNew(f.HasCount), parts)
				case !g.noStd:
					return "vec![" + parts + "]"
				default:
					// alloc storage: an array literal converts straight into it.
					return "[" + parts + "].to_vec()"
				}
			}
		}
		return g.rustSeqNew(f.HasCount)
	default: // struct/union: all children default, so Default::default() is right
		return "Default::default()"
	}
}

// rustNativeArrayParts renders a native scalar array's schema default element
// list (comma-joined, no brackets); ("", false) when there is no default.
// Element literals are unconstrained and infer to the field's element type.
//
// It is NOT padded out to a declared `count: N`: that is a capacity, not a
// length (MESSAGE_SPEC §3), so the default stands exactly as written -- and so
// does the value it is compared against, which is what keeps a length-N all-zero
// array distinct from the empty one.
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

// blobBytes decodes a blob field's base64 schema default; (nil, false) when there
// is no (decodable) default.
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

// byteSliceLit renders bytes as a Rust array literal `[10, 20, 30]`.
func byteSliceLit(raw []byte) string {
	parts := make([]string, len(raw))
	for i, b := range raw {
		parts[i] = fmt.Sprintf("%d", b)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// rustStrNew builds a string field's Default expression per profile. Under no_std
// a bounded string is a heapless::String<N> filled by push_str (heap-free); an
// unbounded one falls back to alloc::String.
func (g *gen) rustStrNew(hasMax bool, lit string, hasLit bool) string {
	if g.staticStore && hasMax {
		if hasLit {
			return fmt.Sprintf("{ let mut _s = heapless::String::new(); let _ = _s.push_str(%q); _s }", lit)
		}
		return "heapless::String::new()"
	}
	if hasLit {
		return fmt.Sprintf("%s::from(%q)", g.dynStr(), lit)
	}
	return g.dynStr() + "::new()"
}

// rustBlobNew builds a blob field's Default expression per profile (sliceLit is a
// `[..]` byte-array literal, used only when hasLit). std uses vec!; no_std bounded
// builds a heapless::Vec by extend_from_slice; unbounded falls back to alloc::Vec.
func (g *gen) rustBlobNew(hasMax bool, sliceLit string, hasLit bool) string {
	if g.staticStore && hasMax {
		if hasLit {
			return fmt.Sprintf("{ let mut _v = heapless::Vec::new(); let _ = _v.extend_from_slice(&%s); _v }", sliceLit)
		}
		return "heapless::Vec::new()"
	}
	if !g.noStd {
		if hasLit {
			return "vec!" + sliceLit
		}
		return "Vec::new()"
	}
	if hasLit {
		return sliceLit + ".to_vec()"
	}
	return "alloc::vec::Vec::new()"
}

// rustSeqNew is the empty-container Default for a wrapper sequence / dynamic array
// per profile.
func (g *gen) rustSeqNew(hasCount bool) string {
	switch {
	case g.staticStore && hasCount:
		return "heapless::Vec::new()"
	case !g.noStd:
		return "Vec::new()"
	default:
		return "alloc::vec::Vec::new()"
	}
}

// rustLeafNe is the `!= default` omit-guard for a scalar/string leaf field
// (MESSAGE_SPEC §2). A string compares against its &str default (as_str() under
// no_std, where the field is a heapless/alloc string); other scalars against
// their materialized default value -- including a NaN default, which the field is
// then never equal to, so it always reaches the wire.
func (g *gen) rustLeafNe(acc string, f *ir.Field) string {
	if f.Kind == ir.KindString {
		lit, _ := f.Default.(string)
		// heapless::String and alloc::String both compare through as_str(); only a
		// std String is directly PartialEq<&str> here.
		if g.noStd || (g.staticStore && f.HasMaxlen) {
			return fmt.Sprintf("%s.as_str() != %q", acc, lit)
		}
		return fmt.Sprintf("%s != %q", acc, lit)
	}
	return fmt.Sprintf("%s != %s", acc, g.rustFieldDefault(f))
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

// Native `count: N` arrays deliberately have NO inline `[T; N]` storage. `count`
// is a capacity, not a length (MESSAGE_SPEC §3): a field holds 0..N elements and
// the wire count M IS its length, which a Rust array of exactly N cannot express
// -- it would round-trip a 3-element message into a 5-element value. Every native
// array therefore lives in the profile's variable-length container: `Vec<T>` on
// std, and `heapless::Vec<T, N>` under no_std, whose capacity is N, whose Default
// has length 0, and which is still inline and heap-free.

// rustStr / rustBlob / rustSeq map a variable-length string, blob, or wrapper
// sequence to its storage type per profile: std String/Vec (default), or under
// no_std either fixed heapless storage sized from the schema (the default) or an
// alloc container holding what the message actually carries (allow_dynamic).
//
// Both no_std modes take the same schema — checkBounded requires the bound
// either way — so hasMax/hasCount are always true there. What changes is where
// the bytes live, and therefore what a message costs to hold and to move: with a
// large declared bound the inline object is the worst case whether the message
// uses it or not.
// dynStr / dynVec name the DYNAMIC container for the environment. This is the
// noStd axis and nothing else: alloc:: paths where there is no std prelude,
// plain String/Vec where there is.
func (g *gen) dynStr() string {
	if g.noStd {
		return "alloc::string::String"
	}
	return "String"
}

func (g *gen) dynVec(elem string) string {
	if g.noStd {
		return "alloc::vec::Vec<" + elem + ">"
	}
	return "Vec<" + elem + ">"
}

func (g *gen) rustStr(hasMax bool, max int64) string {
	if g.staticStore && hasMax {
		return fmt.Sprintf("heapless::String<%d>", max)
	}
	return g.dynStr()
}

func (g *gen) rustBlob(hasMax bool, max int64) string {
	if g.staticStore && hasMax {
		return fmt.Sprintf("heapless::Vec<u8, %d>", max)
	}
	return g.dynVec("u8")
}

func (g *gen) rustSeq(elem string, hasCount bool, count int64) string {
	if g.staticStore && hasCount {
		return fmt.Sprintf("heapless::Vec<%s, %d>", elem, count)
	}
	return g.dynVec(elem)
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
		return g.rustStr(f.HasMaxlen, f.Maxlen)
	case ir.KindBlob:
		return g.rustBlob(f.HasMaxlen, f.Maxlen)
	case ir.KindEnum:
		return enumBacking(f.Ref.Target)
	case ir.KindBitfield:
		return bitfieldBacking(f.Ref.Target)
	case ir.KindStruct, ir.KindUnion:
		return g.typeName(f.Ref.Key)
	case ir.KindArray:
		return g.rustSeq(g.rustArrayElem(f.Elem, f.ElemRef, f.ElemItems, f.ElemMaxHas, f.ElemMax), f.HasCount, f.Count)
	}
	return "()"
}

// rustArrayElem is the Rust type of an array element, recursing for nested
// arrays. Numeric/bool map to their scalar Rust type; enum/bitfield to their
// integer backing; struct/union to the shared type name; a nested array wraps
// another sequence container. String/blob elements are sized from the element
// maxlen under the no_std profile (elemMaxHas/elemMax).
func (g *gen) rustArrayElem(elem ir.Kind, ref *ir.TypeRef, items *ir.ArrayElem, elemMaxHas bool, elemMax int64) string {
	switch elem {
	case ir.KindString:
		return g.rustStr(elemMaxHas, elemMax)
	case ir.KindBlob:
		return g.rustBlob(elemMaxHas, elemMax)
	case ir.KindBool:
		return "bool"
	case ir.KindEnum:
		return enumBacking(ref.Target)
	case ir.KindBitfield:
		return bitfieldBacking(ref.Target)
	case ir.KindStruct, ir.KindUnion:
		return g.typeName(ref.Key)
	case ir.KindArray:
		return g.rustSeq(g.rustArrayElem(items.Elem, items.ElemRef, items.ElemItems, items.ElemMaxHas, items.ElemMax), items.HasCount, items.Count)
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

// hasCap reports whether a given sofab feature is provisioned in the generated
// Cargo.toml for the no_std corelib. A feature that is off is compiled out of that
// corelib entirely, so the generated code must not reference the callbacks it
// gates. Since generator#215 the decoder provisions the full wire-type set, so
// this is true for every wire type — kept so codegen stays tied to its feature.
func (g *gen) hasCap(cap string) bool {
	for _, c := range g.capabilities(g.schema) {
		if c == cap {
			return true
		}
	}
	return false
}

// capabilities returns the sofab wire-type features the generated crate needs
// from corelib-rs-no-std, for require!() and the generated Cargo.toml.
//
// A decoder needs the FULL wire-type set, not just the wire types the schema
// declares: MESSAGE_SPEC §7.3 requires a field whose wire type doesn't match its
// id's declared type to be skipped "exactly as an unknown id is skipped," and an
// unknown id may carry any wire construct — an array, an fp64, a 64-bit value.
// corelib-rs-no-std gates wire-type parse/skip (not just field storage) behind
// these Cargo features, so a decoder built with only the schema's own wire types
// cannot skip the rest — it rejects a well-formed skippable field with InvalidMsg
// (generator#215 / Crucible F-0027). So provision every wire type here regardless
// of the schema; value64 stays the value-width policy knob but is likewise needed
// to skip a 64-bit value. (The footprint saving from dropping a wire type is not
// available to a §7.3-conformant decoder; making the corelib's skip path itself
// feature-independent is the alternative that would restore it.)
func (g *gen) capabilities(s *ir.Schema) []string {
	return []string{"array", "fixlen", "fp64", "sequence", "value64"}
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
