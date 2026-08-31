package python

import (
	"encoding/base64"
	"fmt"
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

// exported -> PascalCase class name.
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

// pyAnnot is the dataclass field type annotation (string, lazy via __future__).
func (g *gen) pyAnnot(f *ir.Field) string {
	switch f.Kind {
	case ir.KindFP32, ir.KindFP64:
		return "float"
	case ir.KindBool:
		return "bool"
	case ir.KindString:
		return "str"
	case ir.KindBlob:
		return "bytes"
	case ir.KindEnum, ir.KindBitfield, ir.KindStruct, ir.KindUnion:
		return g.typeName(f.Ref.Key)
	case ir.KindArray:
		return g.pyArrayAnnot(f.Elem, f.ElemRef, f.ElemItems)
	default: // integers
		return "int"
	}
}

// pyArrayAnnot returns the list[...] annotation for an array element, recursing
// for nested arrays.
func (g *gen) pyArrayAnnot(elem ir.Kind, ref *ir.TypeRef, items *ir.ArrayElem) string {
	switch elem {
	case ir.KindString:
		return "list[str]"
	case ir.KindBlob:
		return "list[bytes]"
	case ir.KindFP32, ir.KindFP64:
		return "list[float]"
	case ir.KindBool:
		return "list[bool]"
	case ir.KindEnum, ir.KindStruct, ir.KindUnion:
		return "list[" + g.typeName(ref.Key) + "]"
	case ir.KindArray:
		return "list[" + g.pyArrayAnnot(items.Elem, items.ElemRef, items.ElemItems) + "]"
	default: // integers, bitfield
		return "list[int]"
	}
}

// pyDefault produces a dataclass default (literal or field(default_factory=...)).
func (g *gen) pyDefault(f *ir.Field) string {
	switch f.Kind {
	case ir.KindU8, ir.KindU16, ir.KindU32, ir.KindU64, ir.KindI8, ir.KindI16, ir.KindI32, ir.KindI64:
		if f.Default != nil {
			return scalarLit(f.Default)
		}
		return "0"
	case ir.KindBool:
		if b, ok := f.Default.(bool); ok && b {
			return "True"
		}
		return "False"
	case ir.KindFP32, ir.KindFP64:
		if f.Default != nil {
			return fmt.Sprintf("%v", f.Default)
		}
		return "0.0"
	case ir.KindString:
		if s, ok := f.Default.(string); ok {
			return fmt.Sprintf("%q", s)
		}
		return `""`
	case ir.KindBlob:
		if s, ok := f.Default.(string); ok {
			if raw, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(s), "")); err == nil {
				return fmt.Sprintf("bytes(%s)", intListLit(raw))
			}
		}
		return "b\"\""
	case ir.KindEnum:
		tn := g.typeName(f.Ref.Key)
		if f.Default != nil {
			return fmt.Sprintf("%s(%s)", tn, scalarLit(f.Default))
		}
		return tn + "(0)"
	case ir.KindBitfield:
		return fmt.Sprintf("%d", g.bitfieldDefault(f))
	case ir.KindStruct, ir.KindUnion:
		// lazy lambda so the referenced class need not be defined yet.
		return fmt.Sprintf("field(default_factory=lambda: %s())", g.typeName(f.Ref.Key))
	case ir.KindArray:
		// An array field gets exactly its declared `default` and nothing else. A
		// declared `count: N` is a CAPACITY, not a length (MESSAGE_SPEC §3), so a
		// fresh count:N array is the EMPTY list -- not N element defaults -- and a
		// `default` shorter than N stands for itself rather than being padded out to
		// N. That is also what the field's omit test compares against, and what an
		// absent field decodes back to.
		if lit, ok := g.pyNativeArrayDefault(f); ok {
			return fmt.Sprintf("field(default_factory=lambda: %s)", lit)
		}
		return "field(default_factory=list)"
	}
	return "None"
}

// isNativeArrayElem reports whether an array element uses a native scalar array
// wire type (vs. a wrapper sequence). Native arrays are a leaf field (omitted as
// a whole when equal to their default); a composite/dynamic-element array is a
// wrapper sequence, opened lazily and closed with the dropping end at field level
// (MESSAGE_SPEC §2).
func isNativeArrayElem(elem ir.Kind) bool {
	switch elem {
	case ir.KindU8, ir.KindU16, ir.KindU32, ir.KindU64,
		ir.KindI8, ir.KindI16, ir.KindI32, ir.KindI64,
		ir.KindFP32, ir.KindFP64, ir.KindBool, ir.KindEnum, ir.KindBitfield:
		return true
	}
	return false
}

// pyNativeArrayDefault renders a native array field's DECLARED default as a
// Python list literal; ("", false) when the field declares none (a
// wrapper-sequence array, or any array without a schema `default` — both start
// empty). It is the single gate for "does this array have a default literal?",
// shared by the dataclass field default and marshal's omit-when-default compare,
// which must agree exactly.
//
// A declared `count: N` does not create a default: it is a capacity, not a length
// (MESSAGE_SPEC §3), so a fresh count:N array is the empty list.
func (g *gen) pyNativeArrayDefault(f *ir.Field) (string, bool) {
	if !isNativeArrayElem(f.Elem) || f.Default == nil {
		return "", false
	}
	return g.pyNativeArrayLiteral(f)
}

// pyNativeArrayLiteral renders a native scalar array's default as a Python list
// literal ([...]); ("", false) when the Default is not a list.
func (g *gen) pyNativeArrayLiteral(f *ir.Field) (string, bool) {
	vals, ok := f.Default.([]any)
	if !ok && f.Default != nil {
		return "", false
	}
	parts := make([]string, len(vals))
	for i, v := range vals {
		if f.Elem == ir.KindBool {
			if b, _ := v.(bool); b {
				parts[i] = "True"
			} else {
				parts[i] = "False"
			}
			continue
		}
		parts[i] = scalarLit(v)
	}
	// Not padded to a declared `count: N`: that is a capacity, not a length
	// (MESSAGE_SPEC §3), so the default stands exactly as written -- and so does
	// the value it is compared against, which is what keeps a length-N all-zero
	// array distinct from the empty one.
	return "[" + strings.Join(parts, ", ") + "]", true
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

// ---- JSON helpers (canonical: blob as list[int], to match the C harness) ----

// pyScratchSize is the fixed output buffer the unbounded encode arm writes
// through. It is not a limit on the message: the buffer is drained into the
// caller's storage every time it fills, so it only trades sink calls against
// resident bytes. Matches the Go and Rust backends.
const pyScratchSize = 512

func (g *gen) emitJSON(f *pyfile, name string, fields []*ir.Field, ms generator.MessageSize) {
	// to_jsonable
	f.line("    def to_jsonable(self) -> dict:")
	f.line("        return {")
	for _, fld := range fields {
		f.line("            %q: %s,", fld.Name, g.toJSONExpr(fld))
	}
	f.line("        }")
	f.blank()
	// from_jsonable
	f.line("    @classmethod")
	f.line("    def from_jsonable(cls, d: dict) -> %q:", name)
	f.line("        o = cls()")
	for _, fld := range fields {
		f.line("        if %q in d:", fld.Name)
		g.fromJSONStmt(f, fld)
	}
	f.line("        return o")
	f.blank()
	// encode / decode
	//
	// The encode buffer belongs to the CALLER (CORELIB_PLAN §5.1): the corelib
	// writes into storage it is handed and never grows or reallocates it, so the
	// allocation is made here. Which shape that takes is a property of the
	// SCHEMA, not of the value, hence the two arms.
	if ms.Bounded {
		// One exactly-sized buffer, no sink: MAX_SIZE comes from the schema, so
		// every schema-conformant value fits. A value the caller filled past its
		// own declared count/maxlen does not, and SofaBufferError propagates out of
		// serialize rather than a short message being handed back as if it were
		// whole (§5.1 forbids returning partial output as complete).
		f.line("    def encode(self) -> bytes:")
		f.line(`        """Encode into a buffer this call allocates and owns.`)
		f.line("")
		f.line("        The buffer is exactly ``MAX_SIZE`` bytes -- the schema's worst case --")
		f.line("        so any conformant value fits. A value filled past a declared")
		f.line("        count/maxlen raises :class:`sofab.SofaBufferError` instead of being")
		f.line("        truncated, and nothing is returned.")
		f.line(`        """`)
		f.line("        buf = bytearray(%s.MAX_SIZE)", name)
		f.line("        e = Encoder.over_buffer(buf, 0)")
		f.line("        self.serialize(e)")
		f.line("        # Through a memoryview: slicing the bytearray itself would copy the")
		f.line("        # prefix once more before bytes() copies it again.")
		f.line("        return bytes(memoryview(buf)[: e.bytes_used()])")
	} else {
		// An unbounded field has no worst case, so MAX_SIZE here is a configured
		// ceiling rather than a size the message cannot reach. Sizing a buffer from
		// it would silently refuse a larger message the caller legitimately built,
		// so the shape is a fixed scratch drained into caller-owned storage: the
		// corelib still allocates nothing, and the ceiling never bounds a value.
		//
		// This is a COPYING sink in §5.1's terms: it takes a copy and returns
		// without installing a replacement buffer, so the encoder keeps the scratch
		// and resumes at 0, with no take-and-replace handover.
		//
		// The copy is the sink's own job, and not optional. §5.1.6 hands a sink the
		// INSTALLED buffer -- here a memoryview over `scratch` -- rather than a
		// snapshot of it, precisely so that a sink which wants to keep the bytes
		// has to say so. Appending the view itself would append the same live
		// window every time and every element would alias the scratch's final
		// contents.
		f.line("    def encode(self) -> bytes:")
		f.line(`        """Encode into storage this call allocates and owns.`)
		f.line("")
		f.line("        A field of this class is unbounded, so there is no worst-case size to")
		f.line("        hand the encoder. It writes through a fixed %d-byte scratch buffer", pyScratchSize)
		f.line("        that is copied out each time it fills: the message may be any size,")
		f.line("        and ``MAX_SIZE`` never bounds it.")
		f.line(`        """`)
		f.line("        out: list[bytes] = []")
		f.line("        scratch = bytearray(%d)", pyScratchSize)
		f.line("        # The sink is handed a view over ``scratch``, which the encoder goes")
		f.line("        # on writing into, so each piece is copied out here.")
		f.line("        e = Encoder.over_buffer(scratch, 0, lambda _v: out.append(bytes(_v)))")
		f.line("        self.serialize(e)")
		f.line("        e.flush()")
		f.line("        # One drain is the common case (a message below the scratch size), and")
		f.line("        # that piece is already an owned copy -- joining it with itself would")
		f.line("        # copy the whole message a second time.")
		f.line("        return out[0] if len(out) == 1 else b\"\".join(out)")
	}
	f.blank()
	f.line("    @classmethod")
	f.line("    def decoder(cls) -> _StreamDecoder:")
	f.line(`        """The streaming reader: feed it chunks of any size.`)
	f.line("")
	f.line("        The half-built message is on ``.message`` throughout; each ``feed``")
	f.line("        returns the outcome for the bytes so far.")
	f.line(`        """`)
	f.line("        return _StreamDecoder(cls, _%sVisitor)", name)
	f.blank()
	f.line("    @classmethod")
	f.line("    def decode(cls, data: bytes) -> %q:", name)
	f.line(`        """Decode a message that OWNS its bytes.`)
	f.line("")
	f.line("        Every destination holds a copy -- ``str``/``bytes`` values the corelib")
	f.line("        built, never a window into ``data`` -- so the message outlives the")
	f.line("        input and ``data`` may be reused or mutated the moment this returns.")
	f.line("")
	f.line("        One-shot over the streaming pair: the three-valued outcome is not")
	f.line("        hidden, so anything short of COMPLETE is raised, and")
	f.line("        INCOMPLETE stays distinguishable from INVALID.")
	f.line(`        """`)
	f.line("        o = cls()")
	// The receiver caps ride the construction call the decode path already
	// makes (CORELIB_PLAN §6.2.1): corelib-py performs the comparison, at the
	// count/length header, and holds no limit of its own -- so all three are
	// required arguments and are always stated.
	f.line("        d = Decoder(")
	f.line("            visitor=_%sVisitor(o),", name)
	f.line("            max_dyn_array_count=MAX_DYN_ARRAY_COUNT,")
	f.line("            max_dyn_string_len=MAX_DYN_STRING_LEN,")
	f.line("            max_dyn_blob_len=MAX_DYN_BLOB_LEN,")
	f.line("        )")
	f.line("        st = d.feed(data)")
	f.line("        if st is Status.INVALID:")
	f.line(`            raise SofaDecodeError(d.error or "invalid message")`)
	f.line("        if st is Status.INCOMPLETE:")
	f.line(`            raise SofaIncompleteError(d.error or "truncated message")`)
	f.line("        return o")
	f.blank()
}

func (g *gen) toJSONExpr(f *ir.Field) string {
	acc := "self." + pyIdent(f.Name)
	switch f.Kind {
	case ir.KindBlob:
		return fmt.Sprintf("list(%s)", acc)
	case ir.KindEnum, ir.KindBitfield:
		return fmt.Sprintf("int(%s)", acc)
	case ir.KindStruct, ir.KindUnion:
		return acc + ".to_jsonable()"
	case ir.KindArray:
		return g.pyArrayToJSON(acc, f.Elem, f.ElemRef, f.ElemItems, 0)
	default:
		return acc
	}
}

// pyArrayToJSON builds a JSON-able expression for an array value: blob->list[int],
// enum/bitfield->int, struct/union->to_jsonable(); recurses for nested arrays.
func (g *gen) pyArrayToJSON(val string, elem ir.Kind, ref *ir.TypeRef, items *ir.ArrayElem, depth int) string {
	v := fmt.Sprintf("_x%d", depth)
	switch elem {
	case ir.KindBlob:
		return fmt.Sprintf("[list(%s) for %s in %s]", v, v, val)
	case ir.KindEnum, ir.KindBitfield:
		return fmt.Sprintf("[int(%s) for %s in %s]", v, v, val)
	case ir.KindStruct, ir.KindUnion:
		return fmt.Sprintf("[%s.to_jsonable() for %s in %s]", v, v, val)
	case ir.KindArray:
		return fmt.Sprintf("[%s for %s in %s]", g.pyArrayToJSON(v, items.Elem, items.ElemRef, items.ElemItems, depth+1), v, val)
	default:
		return fmt.Sprintf("list(%s)", val)
	}
}

func (g *gen) fromJSONStmt(f *pyfile, fld *ir.Field) {
	acc := "o." + pyIdent(fld.Name)
	src := fmt.Sprintf("d[%q]", fld.Name)
	switch fld.Kind {
	case ir.KindBlob:
		f.line("            %s = bytes(%s)", acc, src)
	case ir.KindStruct, ir.KindUnion:
		f.line("            %s = %s.from_jsonable(%s)", acc, g.typeName(fld.Ref.Key), src)
	case ir.KindArray:
		f.line("            %s = %s", acc, g.pyArrayFromJSON(src, fld.Elem, fld.ElemRef, fld.ElemItems, 0))
	default:
		f.line("            %s = %s", acc, src)
	}
}

// pyArrayFromJSON builds an expression rebuilding an array from JSON: blob->bytes,
// struct/union->from_jsonable(); recurses for nested arrays. enum/bitfield/bool
// stay plain ints/bools (list()).
func (g *gen) pyArrayFromJSON(src string, elem ir.Kind, ref *ir.TypeRef, items *ir.ArrayElem, depth int) string {
	v := fmt.Sprintf("_x%d", depth)
	switch elem {
	case ir.KindBlob:
		return fmt.Sprintf("[bytes(%s) for %s in %s]", v, v, src)
	case ir.KindStruct, ir.KindUnion:
		return fmt.Sprintf("[%s.from_jsonable(%s) for %s in %s]", g.typeName(ref.Key), v, v, src)
	case ir.KindArray:
		return fmt.Sprintf("[%s for %s in %s]", g.pyArrayFromJSON(v, items.Elem, items.ElemRef, items.ElemItems, depth+1), v, src)
	default:
		return fmt.Sprintf("list(%s)", src)
	}
}

// pyKeywords are Python's (hard) reserved words — invalid as attribute names.
// (`match`/`case` are soft keywords, valid as identifiers, so not included.) No
// escape exists, so such a field is mangled (trailing underscore); the JSON key
// (a separate string literal) keeps the original name.
var pyKeywords = map[string]bool{
	"False": true, "None": true, "True": true, "and": true, "as": true,
	"assert": true, "async": true, "await": true, "break": true, "class": true,
	"continue": true, "def": true, "del": true, "elif": true, "else": true,
	"except": true, "finally": true, "for": true, "from": true, "global": true,
	"if": true, "import": true, "in": true, "is": true, "lambda": true,
	"nonlocal": true, "not": true, "or": true, "pass": true, "raise": true,
	"return": true, "try": true, "while": true, "with": true, "yield": true,
}

// pyIdent mangles a field name that is a Python keyword (trailing underscore).
func pyIdent(name string) string {
	if pyKeywords[name] {
		return name + "_"
	}
	return name
}
