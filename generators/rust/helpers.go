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
		if g.noStd {
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
		// marshal can compare against it (empty container when there is no default).
		if raw, ok := g.blobBytes(f); ok {
			return g.rustBlobNew(f.HasMaxlen, byteSliceLit(raw), true)
		}
		return g.rustBlobNew(f.HasMaxlen, "", false)
	case ir.KindArray:
		// A native scalar array is a leaf: materialize its schema default so an
		// omitted default array reconstructs correctly. A fixed-count native array
		// is a stack `[elem; N]`; a dynamic one stays a heap Vec. Composite arrays
		// are wrapper sequences (always framed) and stay an empty container.
		if elem, n, ok := g.fixedNativeArray(f); ok {
			return g.rustFixedArrayDefault(f, elem, n)
		}
		if isNativeArrayElem(f.Elem) {
			// No declared default: a `count: N` array still defaults to N element
			// defaults, exactly as [T; N] did and as C/C++ aggregate init gives.
			if f.Default == nil && f.HasCount {
				return fmt.Sprintf("vec![%s; %d]", rustElemZeroLit(f.Elem), f.Count)
			}
			if parts, ok := g.rustNativeArrayPartsN(f); ok {
				if g.noStd {
					// dynamic (count-less) native array under allow_dynamic -> alloc Vec.
					return "[" + parts + "].to_vec()"
				}
				return "vec![" + parts + "]"
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
// rustNativeArrayPartsN is rustNativeArrayParts, tail-padded to the schema
// `count` when there is one. A `count: N` array's default is exactly N elements
// — the declared values, then the element default — in every target: C and C++
// get that from aggregate initialization, which zero-fills what a braced
// initializer leaves out. A Vec has no such rule, so the padding is explicit
// here; otherwise an omitted field would reconstruct with fewer elements than
// the same schema yields elsewhere.
func (g *gen) rustNativeArrayPartsN(f *ir.Field) (string, bool) {
	parts, ok := g.rustNativeArrayParts(f)
	if !ok || !f.HasCount {
		return parts, ok
	}
	have := int64(0)
	if parts != "" {
		have = int64(len(strings.Split(parts, ", ")))
	}
	zero := rustElemZeroLit(f.Elem)
	for ; have < f.Count; have++ {
		if parts == "" {
			parts = zero
		} else {
			parts += ", " + zero
		}
	}
	return parts, true
}

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
	if hasMax && !(g.noStd && g.allowDynamic) {
		if hasLit {
			return fmt.Sprintf("{ let mut _s = heapless::String::new(); let _ = _s.push_str(%q); _s }", lit)
		}
		return "heapless::String::new()"
	}
	if hasLit {
		return fmt.Sprintf("alloc::string::String::from(%q)", lit)
	}
	return "alloc::string::String::new()"
}

// rustBlobNew builds a blob field's Default expression per profile (sliceLit is a
// `[..]` byte-array literal, used only when hasLit). std uses vec!; no_std bounded
// builds a heapless::Vec by extend_from_slice; unbounded falls back to alloc::Vec.
func (g *gen) rustBlobNew(hasMax bool, sliceLit string, hasLit bool) string {
	if !g.noStd {
		if hasLit {
			return "vec!" + sliceLit
		}
		return "Vec::new()"
	}
	if hasMax && !g.allowDynamic {
		if hasLit {
			return fmt.Sprintf("{ let mut _v = heapless::Vec::new(); let _ = _v.extend_from_slice(&%s); _v }", sliceLit)
		}
		return "heapless::Vec::new()"
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
	case !g.noStd:
		return "Vec::new()"
	case g.allowDynamic:
		return "alloc::vec::Vec::new()"
	case hasCount:
		return "heapless::Vec::new()"
	default:
		return "alloc::vec::Vec::new()"
	}
}

// rustLeafNe is the boolean omit-guard `<lhs> != <default>` for a scalar/string
// leaf field. A string compares against its &str default (as_str() under no_std,
// where the field is a heapless/alloc string); other scalars against their
// materialized default value.
func (g *gen) rustLeafNe(acc string, f *ir.Field) string {
	if f.Kind == ir.KindString {
		lit, _ := f.Default.(string)
		if g.noStd {
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

// fixedNativeArray reports whether an array field is a native-element array with
// a statically known length — the case that lowers to a fixed Rust array
// `[elem; N]` (stack, heap-free) instead of a heap `Vec<elem>`, mirroring the C++
// backend's `std::array<T, N>`. Returns the element Rust type and N. Native but
// count-less (dynamic) arrays, and composite-element arrays, keep `Vec`.
func (g *gen) fixedNativeArray(f *ir.Field) (elem string, n int64, ok bool) {
	if f.Kind != ir.KindArray || !isNativeArrayElem(f.Elem) || !f.HasCount {
		return "", 0, false
	}
	// Under allow_dynamic a native array is a Vec like every other container; the
	// schema count stays mandatory and becomes a decode-path check instead of the
	// array's length.
	// Inline fixed-size storage belongs to exactly one profile: no_std without
	// allow_dynamic, the one with no allocator to reach for. Everywhere else a
	// bounded array goes in that profile's dynamic container, like a bounded
	// string or blob: all three are variable-length up to their declared maximum
	// (§3 trims an array's trailing default run), so the maximum is a bound, not a
	// storage decision.
	//
	// This gives up the [T; N] the std profile used to emit, measured as a
	// maxspeed win in cd8e9eb. Two things outweighed it: within one struct a
	// `count: N` array was inline while a count-less one was a Vec, so declaring a
	// bound silently changed the storage; and `len()` then meant "the schema
	// count" on one field and "what the message carried" on the next. The
	// allocation is worth revisiting once the storage model is settled.
	if !g.noStd || g.allowDynamic {
		return "", 0, false
	}
	return g.rustArrayElem(f.Elem, f.ElemRef, f.ElemItems, f.ElemMaxHas, f.ElemMax), f.Count, true
}

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
func (g *gen) rustStr(hasMax bool, max int64) string {
	switch {
	case !g.noStd, g.allowDynamic:
		if g.noStd {
			return "alloc::string::String"
		}
		return "String"
	case hasMax:
		return fmt.Sprintf("heapless::String<%d>", max)
	default:
		return "alloc::string::String"
	}
}

func (g *gen) rustBlob(hasMax bool, max int64) string {
	switch {
	case !g.noStd, g.allowDynamic:
		if g.noStd {
			return "alloc::vec::Vec<u8>"
		}
		return "Vec<u8>"
	case hasMax:
		return fmt.Sprintf("heapless::Vec<u8, %d>", max)
	default:
		return "alloc::vec::Vec<u8>"
	}
}

func (g *gen) rustSeq(elem string, hasCount bool, count int64) string {
	switch {
	case !g.noStd, g.allowDynamic:
		if g.noStd {
			return "alloc::vec::Vec<" + elem + ">"
		}
		return "Vec<" + elem + ">"
	case hasCount:
		return fmt.Sprintf("heapless::Vec<%s, %d>", elem, count)
	default:
		return "alloc::vec::Vec<" + elem + ">"
	}
}

// rustFixedArrayDefault renders the Default value of a fixed native array
// `[elem; N]`. With a schema default it is an explicit array literal of exactly N
// elements — the given values, tail-padded with the element zero (matching the
// C++ `std::array` aggregate-init that zero-fills unspecified trailing elements,
// so both backends encode the same N elements). With no default it is the
// type-zero repeat literal (`[0; N]` / `[0.0; N]` / `[false; N]`).
func (g *gen) rustFixedArrayDefault(f *ir.Field, elem string, n int64) string {
	zero := rustElemZeroLit(f.Elem)
	if vals, ok := f.Default.([]any); ok {
		parts := make([]string, 0, n)
		for _, v := range vals {
			parts = append(parts, rustElemLit(f.Elem, v))
		}
		for int64(len(parts)) < n {
			parts = append(parts, zero)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	}
	return fmt.Sprintf("[%s; %d]", zero, n)
}

// rustElemZeroLit / rustElemLit render a native array element's zero literal and
// a schema-default element literal. They are the single source of truth for both
// the Default image and the needs-reset test, so the two cannot disagree.
func rustElemZeroLit(k ir.Kind) string {
	switch k {
	case ir.KindFP32, ir.KindFP64:
		return "0.0"
	case ir.KindBool:
		return "false"
	default:
		return "0"
	}
}

func rustElemLit(k ir.Kind, v any) string {
	if k == ir.KindFP32 || k == ir.KindFP64 {
		return rustFloat(v)
	}
	return fmt.Sprintf("%v", v)
}

// rustFixedArrayZero renders the all-element-default image of a fixed native
// array `[elem; N]` (`[0; N]` / `[0.0; N]` / `[false; N]`). This is what a
// short wire count decodes the tail to (MESSAGE_SPEC §3: elements [M, N) are the
// ELEMENT default), which is not the same as the field's Default image once the
// field carries a schema `default:`.
func (g *gen) rustFixedArrayZero(f *ir.Field, n int64) string {
	return fmt.Sprintf("[%s; %d]", rustElemZeroLit(f.Elem), n)
}

// rustFixedArrayNeedsReset reports whether a fixed native array's Default image
// differs from its all-element-default image — i.e. whether the field carries a
// non-zero schema `default:`. Only such a field needs decode's array_begin to
// wipe the constructor image before the M wire elements are stored: positions
// [M, N) must read back as the ELEMENT default, and the schema default would
// otherwise leak through the untouched tail. Returns the zero image to reset to.
// Gating on this keeps every other schema's generated code byte-identical.
func (g *gen) rustFixedArrayNeedsReset(f *ir.Field) (zeroImage string, need bool) {
	_, n, ok := g.fixedNativeArray(f)
	if !ok {
		return "", false
	}
	zero := g.rustFixedArrayZero(f, n)
	vals, ok := f.Default.([]any)
	if !ok {
		return zero, false // no schema default: the constructor is already the zero image
	}
	// Compare element-wise rather than against the rendered image: an explicit
	// all-zero default (`[0, 0, 0]`) is semantically the zero image even though it
	// renders differently from the `[0; N]` repeat literal, and must not pay for a
	// reset. A -0.0 default does need one: [M, N) must read back as +0.0.
	elemZero := rustElemZeroLit(f.Elem)
	for _, v := range vals {
		if rustElemLit(f.Elem, v) != elemZero {
			return zero, true
		}
	}
	return zero, false
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
		if elem, n, ok := g.fixedNativeArray(f); ok {
			return fmt.Sprintf("[%s; %d]", elem, n)
		}
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
