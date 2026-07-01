package python

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
		// A NATIVE scalar array is a leaf field: materialize its schema default so
		// an omitted default array reconstructs correctly and marshal can compare
		// against it. Composite arrays are wrapper sequences (always framed) and
		// start empty.
		if isNativeArrayElem(f.Elem) {
			if lit, ok := g.pyNativeArrayLiteral(f); ok {
				return fmt.Sprintf("field(default_factory=lambda: %s)", lit)
			}
		}
		return "field(default_factory=list)"
	}
	return "None"
}

// isNativeArrayElem reports whether an array element uses a native scalar array
// wire type (vs. a wrapper sequence). Native arrays are a leaf field (omitted as
// a whole when equal to their default); composite/dynamic-element arrays are
// always framed.
func isNativeArrayElem(elem ir.Kind) bool {
	switch elem {
	case ir.KindU8, ir.KindU16, ir.KindU32, ir.KindU64,
		ir.KindI8, ir.KindI16, ir.KindI32, ir.KindI64,
		ir.KindFP32, ir.KindFP64, ir.KindBool, ir.KindEnum, ir.KindBitfield:
		return true
	}
	return false
}

// pyNativeArrayLiteral renders a native scalar array's schema default as a Python
// list literal ([...]); ("", false) when there is no default.
func (g *gen) pyNativeArrayLiteral(f *ir.Field) (string, bool) {
	vals, ok := f.Default.([]any)
	if !ok {
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

func (g *gen) emitJSON(f *pyfile, name string, fields []*ir.Field) {
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
	f.line("    def encode(self) -> bytes:")
	f.line("        e = Encoder()")
	f.line("        self._marshal(e)")
	f.line("        return e.getvalue()")
	f.blank()
	f.line("    @classmethod")
	f.line("    def decode(cls, data: bytes) -> %q:", name)
	f.line("        o = cls()")
	f.line("        o._unmarshal(Decoder(io.BytesIO(data)))")
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
