package golang

import (
	"fmt"
	"strings"

	"github.com/sofa-buffers/generator/internal/generator"
	"github.com/sofa-buffers/generator/internal/ir"
)

func init() { generator.Register(&Backend{}) }

const corelibImport = "github.com/sofa-buffers/corelib-go"

// Backend implements generator.Backend for Go.
type Backend struct{}

func (*Backend) Lang() string { return "go" }

// Generate emits a shared types.go (all named struct/union/enum/bitfield) plus
// one file per message. When emit==project it also scaffolds a buildable module
// with an encode/decode JSON harness.
func (*Backend) Generate(s *ir.Schema, cfg map[string]any) ([]generator.File, error) {
	g := &gen{
		schema:  s,
		pkg:     cfgString(cfg, "package", "message"),
		banner:  cfgString(cfg, "tool_banner", "sofabgen"),
		license: generator.LicenseID(cfg),
	}
	project := cfgString(cfg, "emit", "sources") == "project"
	// In a project the package gets its own directory so the harness can import
	// it; in sources mode the files are emitted flat for the caller to place.
	pkgDir := ""
	if project {
		pkgDir = g.pkg + "/"
	}
	var files []generator.File
	if tf := g.typesFile(); tf != nil {
		files = append(files, generator.File{Path: pkgDir + "types.go", Content: tf})
	}
	for _, m := range s.Messages {
		files = append(files, generator.File{Path: pkgDir + strings.ToLower(m.Name) + ".go", Content: g.messageFile(m)})
	}
	if project {
		files = append(files, g.projectFiles(s, cfg)...)
	}
	return files, nil
}

type gen struct {
	schema  *ir.Schema
	pkg     string
	banner  string
	license string // SPDX id, "" to omit the header line
}

// ---- types.go : all named types -----------------------------------------

func (g *gen) typesFile() []byte {
	if len(g.schema.NamedOrder) == 0 {
		return nil
	}
	f := newGoFile(g.pkg)
	// sofab is imported by emitObject only (structs/unions use the codec); an
	// enum/bitfield-only types file must not import it unused.
	for _, key := range g.schema.NamedOrder {
		nt := g.schema.Named[key]
		switch nt.Category {
		case ir.CatEnum:
			g.emitEnum(f, nt)
		case ir.CatBitfield:
			g.emitBitfield(f, nt)
		case ir.CatStruct, ir.CatUnion:
			g.emitObject(f, g.typeName(key), nt.Fields)
		}
	}
	return f.bytes(g.banner, g.license)
}

func (g *gen) emitEnum(f *gofile, nt *ir.NamedType) {
	tn := g.typeName(nt.Key)
	f.line("// %s is a generated enum (signed wire varint).", tn)
	f.line("type %s %s", tn, enumGoType(nt))
	f.line("const (")
	for _, c := range nt.Consts {
		doc := ""
		if c.Description != "" {
			doc = " // " + oneline(c.Description)
		}
		f.line("\t%s%s %s = %d%s", tn, exported(c.Name), tn, c.Value, doc)
	}
	f.line(")")
	f.blank()
}

func (g *gen) emitBitfield(f *gofile, nt *ir.NamedType) {
	tn := g.typeName(nt.Key)
	f.line("// %s is a generated bitfield (unsigned wire varint).", tn)
	f.line("type %s %s", tn, bitfieldGoType(nt))
	f.line("const (")
	for _, fl := range nt.Flags {
		doc := ""
		if fl.Description != "" {
			doc = " // " + oneline(fl.Description)
		}
		f.line("\t%s%s %s = 1 << %d%s", tn, exported(fl.Name), tn, fl.Pos, doc)
	}
	f.line(")")
	f.blank()
}

// emitObject emits a struct + marshal + unmarshal for an id scope.
func (g *gen) emitObject(f *gofile, typeName string, fields []*ir.Field) {
	f.imp(corelibImport)
	f.line("// %s is a generated SofaBuffers object.", typeName)
	f.line("type %s struct {", typeName)
	// Declare fields widest-first to minimise struct padding; marshal/unmarshal
	// below stay in schema/id order, so the wire bytes are unchanged.
	for _, fld := range ir.SortedForLayout(fields) {
		tag := fmt.Sprintf("`json:%q`", fld.Name)
		f.line("\t%s %s %s%s", exported(fld.Name), g.goType(fld), tag, fieldDoc(fld))
	}
	f.line("}")
	f.blank()

	// marshal
	f.line("func (m *%s) marshal(e *sofab.Encoder) {", typeName)
	for _, fld := range fields {
		g.emitMarshalField(f, fld)
	}
	f.line("}")
	f.blank()

	// unmarshal (pull-parser; returns on EOF or sequence end)
	f.imp("io")
	f.line("func (m *%s) unmarshal(d *sofab.Decoder) error {", typeName)
	f.line("\tfor {")
	f.line("\t\tfld, err := d.Next()")
	f.line("\t\tif err == io.EOF {")
	f.line("\t\t\treturn nil")
	f.line("\t\t}")
	f.line("\t\tif err != nil {")
	f.line("\t\t\treturn err")
	f.line("\t\t}")
	f.line("\t\tif fld.Type == sofab.TypeSequenceEnd {")
	f.line("\t\t\treturn nil")
	f.line("\t\t}")
	f.line("\t\tswitch fld.ID {")
	for _, fld := range fields {
		f.line("\t\tcase %d:", fld.ID)
		g.emitUnmarshalField(f, fld)
	}
	f.line("\t\tdefault:")
	f.line("\t\t\tif err := d.Skip(); err != nil {")
	f.line("\t\t\t\treturn err")
	f.line("\t\t\t}")
	f.line("\t\t}")
	f.line("\t}")
	f.line("}")
	f.blank()
}

// ---- per-field marshal/unmarshal ----------------------------------------

func (g *gen) emitMarshalField(f *gofile, fld *ir.Field) {
	acc := "m." + exported(fld.Name)
	var write string
	switch fld.Kind {
	case ir.KindU8, ir.KindU16, ir.KindU32, ir.KindU64:
		write = fmt.Sprintf("e.WriteUnsigned(%d, uint64(%s))", fld.ID, acc)
	case ir.KindI8, ir.KindI16, ir.KindI32, ir.KindI64:
		write = fmt.Sprintf("e.WriteSigned(%d, int64(%s))", fld.ID, acc)
	case ir.KindBool:
		write = fmt.Sprintf("e.WriteBool(%d, %s)", fld.ID, acc)
	case ir.KindFP32:
		write = fmt.Sprintf("e.WriteFloat32(%d, %s)", fld.ID, acc)
	case ir.KindFP64:
		write = fmt.Sprintf("e.WriteFloat64(%d, %s)", fld.ID, acc)
	case ir.KindString:
		write = fmt.Sprintf("e.WriteString(%d, %s)", fld.ID, acc)
	case ir.KindEnum:
		write = fmt.Sprintf("e.WriteSigned(%d, int64(%s))", fld.ID, acc)
	case ir.KindBitfield:
		write = fmt.Sprintf("e.WriteUnsigned(%d, uint64(%s))", fld.ID, acc)
	case ir.KindBlob:
		// blob is a leaf: omit when equal to its default (empty if none).
		def := "nil"
		if lit, ok := g.defaultLiteral(fld); ok {
			def = lit
		}
		f.line("\tif !bytes.Equal(%s, %s) {", acc, def)
		f.line("\t\te.WriteBytes(%d, %s)", fld.ID, acc)
		f.line("\t}")
		return
	case ir.KindStruct, ir.KindUnion:
		// A sequence is always framed; its child fields are omitted per-field by
		// the nested marshal (MESSAGE_SPEC S2). An all-default nested object thus
		// becomes an empty wrapper sequence, not a dropped field.
		f.line("\te.WriteSequenceBegin(%d)", fld.ID)
		f.line("\t%s.marshal(e)", acc)
		f.line("\te.WriteSequenceEnd()")
		return
	case ir.KindArray:
		g.emitMarshalArray(f, fld, acc)
		return
	}
	// Scalar/string/enum/bitfield leaf: always omit when equal to the default;
	// sparse encoding is canonical (MESSAGE_SPEC S2) and the decoder reconstructs
	// the omitted field from its default.
	f.line("\tif %s != %s {", acc, g.defaultCompare(fld))
	f.line("\t\t%s", write)
	f.line("\t}")
}

// defaultCompare is the RHS to compare a field against for omission: its schema
// default if present, else the Go zero value (matching New<Msg>'s init).
func (g *gen) defaultCompare(fld *ir.Field) string {
	if lit, ok := g.defaultLiteral(fld); ok {
		return lit
	}
	switch fld.Kind {
	case ir.KindBool:
		return "false"
	case ir.KindString:
		return `""`
	case ir.KindEnum, ir.KindBitfield:
		return g.typeName(fld.Ref.Key) + "(0)"
	default:
		return "0"
	}
}

func (g *gen) emitMarshalArray(f *gofile, fld *ir.Field, acc string) {
	// A native scalar array is a leaf field: omit it when equal to its default
	// (materialized in New<Msg>), else when empty. A composite/dynamic-element
	// array is a wrapper sequence and is always framed (never whole-omitted).
	if isNativeArrayElem(fld.Elem) {
		if def, ok := g.defaultLiteral(fld); ok {
			f.imp("slices")
			f.line("\tif !slices.Equal(%s, %s) {", acc, def)
		} else {
			f.line("\tif len(%s) != 0 {", acc)
		}
		g.marshalArray(f, "\t\t", fmt.Sprintf("%d", fld.ID), acc, fld.Elem, fld.ElemRef, fld.ElemItems, 0)
		f.line("\t}")
		return
	}
	g.marshalArray(f, "\t", fmt.Sprintf("%d", fld.ID), acc, fld.Elem, fld.ElemRef, fld.ElemItems, 0)
}

// marshalArray writes the array val as field idExpr. Numeric/enum/boolean/
// bitfield elements use the native array wire type (enum->signed, bool/bitfield->
// unsigned); string/blob/struct/union/array elements lower to a wrapper sequence
// whose child ids are the 0-based index (per MESSAGE_SPEC). Recurses for nested
// arrays, depth-suffixing loop vars to avoid collisions.
func (g *gen) marshalArray(f *gofile, ind, idExpr, val string, elem ir.Kind, ref *ir.TypeRef, items *ir.ArrayElem, depth int) {
	iv := fmt.Sprintf("_i%d", depth)
	ev := fmt.Sprintf("_e%d", depth)
	switch elem {
	case ir.KindU8, ir.KindU16, ir.KindU32, ir.KindU64, ir.KindBitfield:
		f.line("%ssofab.WriteUnsignedArray(e, %s, %s)", ind, idExpr, val)
	case ir.KindI8, ir.KindI16, ir.KindI32, ir.KindI64, ir.KindEnum:
		f.line("%ssofab.WriteSignedArray(e, %s, %s)", ind, idExpr, val)
	case ir.KindBool:
		// bool is outside the integer array constraint; lower to 0/1 unsigned.
		bv := fmt.Sprintf("_b%d", depth)
		f.line("%s{", ind)
		f.line("%s\t%s := make([]uint8, len(%s))", ind, bv, val)
		f.line("%s\tfor %s, %s := range %s {", ind, iv, ev, val)
		f.line("%s\t\tif %s {", ind, ev)
		f.line("%s\t\t\t%s[%s] = 1", ind, bv, iv)
		f.line("%s\t\t}", ind)
		f.line("%s\t}", ind)
		f.line("%s\tsofab.WriteUnsignedArray(e, %s, %s)", ind, idExpr, bv)
		f.line("%s}", ind)
	case ir.KindFP32:
		f.line("%se.WriteFloat32Array(%s, %s)", ind, idExpr, val)
	case ir.KindFP64:
		f.line("%se.WriteFloat64Array(%s, %s)", ind, idExpr, val)
	case ir.KindString:
		f.line("%se.WriteSequenceBegin(%s)", ind, idExpr)
		f.line("%sfor %s, %s := range %s {", ind, iv, ev, val)
		f.line("%s\te.WriteString(sofab.ID(%s), %s)", ind, iv, ev)
		f.line("%s}", ind)
		f.line("%se.WriteSequenceEnd()", ind)
	case ir.KindBlob:
		f.line("%se.WriteSequenceBegin(%s)", ind, idExpr)
		f.line("%sfor %s, %s := range %s {", ind, iv, ev, val)
		f.line("%s\te.WriteBytes(sofab.ID(%s), %s)", ind, iv, ev)
		f.line("%s}", ind)
		f.line("%se.WriteSequenceEnd()", ind)
	case ir.KindStruct, ir.KindUnion:
		f.line("%se.WriteSequenceBegin(%s)", ind, idExpr)
		f.line("%sfor %s, %s := range %s {", ind, iv, ev, val)
		f.line("%s\te.WriteSequenceBegin(sofab.ID(%s))", ind, iv)
		f.line("%s\t%s.marshal(e)", ind, ev)
		f.line("%s\te.WriteSequenceEnd()", ind)
		f.line("%s}", ind)
		f.line("%se.WriteSequenceEnd()", ind)
	case ir.KindArray:
		f.line("%se.WriteSequenceBegin(%s)", ind, idExpr)
		f.line("%sfor %s, %s := range %s {", ind, iv, ev, val)
		g.marshalArray(f, ind+"\t", fmt.Sprintf("sofab.ID(%s)", iv), ev, items.Elem, items.ElemRef, items.ElemItems, depth+1)
		f.line("%s}", ind)
		f.line("%se.WriteSequenceEnd()", ind)
	}
}

func (g *gen) emitUnmarshalField(f *gofile, fld *ir.Field) {
	acc := "m." + exported(fld.Name)
	switch fld.Kind {
	case ir.KindU8, ir.KindU16, ir.KindU32, ir.KindU64:
		f.line("\t\t\tv, _ := d.Unsigned()")
		f.line("\t\t\t%s = %s(v)", acc, g.goType(fld))
	case ir.KindI8, ir.KindI16, ir.KindI32, ir.KindI64:
		f.line("\t\t\tv, _ := d.Signed()")
		f.line("\t\t\t%s = %s(v)", acc, g.goType(fld))
	case ir.KindBool:
		f.line("\t\t\t%s, _ = d.Bool()", acc)
	case ir.KindFP32:
		f.line("\t\t\t%s, _ = d.Float32()", acc)
	case ir.KindFP64:
		f.line("\t\t\t%s, _ = d.Float64()", acc)
	case ir.KindString:
		f.line("\t\t\t%s, _ = d.String()", acc)
	case ir.KindBlob:
		f.line("\t\t\t%s, _ = d.Bytes()", acc)
	case ir.KindEnum:
		f.line("\t\t\tv, _ := d.Signed()")
		f.line("\t\t\t%s = %s(v)", acc, g.typeName(fld.Ref.Key))
	case ir.KindBitfield:
		f.line("\t\t\tv, _ := d.Unsigned()")
		f.line("\t\t\t%s = %s(v)", acc, g.typeName(fld.Ref.Key))
	case ir.KindStruct, ir.KindUnion:
		f.line("\t\t\tif err := %s.unmarshal(d); err != nil {", acc)
		f.line("\t\t\t\treturn err")
		f.line("\t\t\t}")
	case ir.KindArray:
		g.emitUnmarshalArray(f, fld, acc)
	}
}

func (g *gen) emitUnmarshalArray(f *gofile, fld *ir.Field, acc string) {
	g.unmarshalArray(f, "\t\t\t", acc, fld.Elem, fld.ElemRef, fld.ElemItems, 0)
}

// unmarshalArray reads an array into target, mirroring marshalArray: native
// array readers for numeric/enum/boolean/bitfield elements, a wrapper-sequence
// loop for string/blob/struct/union/array elements. Recurses for nested arrays,
// depth-suffixing locals to avoid collisions.
func (g *gen) unmarshalArray(f *gofile, ind, target string, elem ir.Kind, ref *ir.TypeRef, items *ir.ArrayElem, depth int) {
	switch elem {
	case ir.KindU8, ir.KindU16, ir.KindU32, ir.KindU64:
		f.line("%s%s, _ = sofab.ReadUnsignedArray[%s](d)", ind, target, goNumType(elem))
	case ir.KindI8, ir.KindI16, ir.KindI32, ir.KindI64:
		f.line("%s%s, _ = sofab.ReadSignedArray[%s](d)", ind, target, goNumType(elem))
	case ir.KindBitfield:
		f.line("%s%s, _ = sofab.ReadUnsignedArray[%s](d)", ind, target, g.typeName(ref.Key))
	case ir.KindEnum:
		f.line("%s%s, _ = sofab.ReadSignedArray[%s](d)", ind, target, g.typeName(ref.Key))
	case ir.KindBool:
		uv := fmt.Sprintf("_u%d", depth)
		iv := fmt.Sprintf("_i%d", depth)
		ev := fmt.Sprintf("_e%d", depth)
		f.line("%s%s, _ := sofab.ReadUnsignedArray[uint8](d)", ind, uv)
		f.line("%s%s = make([]bool, len(%s))", ind, target, uv)
		f.line("%sfor %s, %s := range %s {", ind, iv, ev, uv)
		f.line("%s\t%s[%s] = %s != 0", ind, target, iv, ev)
		f.line("%s}", ind)
	case ir.KindFP32:
		f.line("%s%s, _ = d.ReadFloat32Array()", ind, target)
	case ir.KindFP64:
		f.line("%s%s, _ = d.ReadFloat64Array()", ind, target)
	default: // string/blob/struct/union/array -> wrapper sequence
		ef := fmt.Sprintf("_ef%d", depth)
		ev := fmt.Sprintf("_e%d", depth)
		f.line("%s%s = %s[:0]", ind, target, target)
		f.line("%sfor {", ind)
		f.line("%s\t%s, _ := d.Next()", ind, ef)
		f.line("%s\tif %s.Type == sofab.TypeSequenceEnd {", ind, ef)
		f.line("%s\t\tbreak", ind)
		f.line("%s\t}", ind)
		switch elem {
		case ir.KindString:
			f.line("%s\t%s, _ := d.String()", ind, ev)
			f.line("%s\t%s = append(%s, %s)", ind, target, target, ev)
		case ir.KindBlob:
			f.line("%s\t%s, _ := d.Bytes()", ind, ev)
			f.line("%s\t%s = append(%s, %s)", ind, target, target, ev)
		case ir.KindStruct, ir.KindUnion:
			f.line("%s\tvar %s %s", ind, ev, g.typeName(ref.Key))
			f.line("%s\tif err := %s.unmarshal(d); err != nil {", ind, ev)
			f.line("%s\t\treturn err", ind)
			f.line("%s\t}", ind)
			f.line("%s\t%s = append(%s, %s)", ind, target, target, ev)
		case ir.KindArray:
			f.line("%s\tvar %s []%s", ind, ev, g.goArrayElem(items.Elem, items.ElemRef, items.ElemItems))
			g.unmarshalArray(f, ind+"\t", ev, items.Elem, items.ElemRef, items.ElemItems, depth+1)
			f.line("%s\t%s = append(%s, %s)", ind, target, target, ev)
		}
		f.line("%s}", ind)
	}
}

// ---- per-message file ----------------------------------------------------

func (g *gen) messageFile(m *ir.Message) []byte {
	f := newGoFile(g.pkg)
	f.imp(corelibImport)
	f.imp("bytes")

	typeName := exported(m.Name)
	if m.Summary != "" {
		f.line("// %s - %s", typeName, oneline(m.Summary))
	}
	g.emitObject(f, typeName, m.Fields)

	// constructor with schema defaults
	f.line("// New%s returns a %s with schema defaults applied.", typeName, typeName)
	f.line("func New%s() *%s {", typeName, typeName)
	f.line("\tm := &%s{}", typeName)
	g.emitDefaults(f, m.Fields)
	f.line("\treturn m")
	f.line("}")
	f.blank()

	// public Encode/Decode wrappers
	f.line("// Encode serializes the message to bytes.")
	f.line("func (m *%s) Encode() ([]byte, error) {", typeName)
	f.line("\tvar buf bytes.Buffer")
	f.line("\te := sofab.NewEncoder(&buf)")
	f.line("\tm.marshal(e)")
	f.line("\tif err := e.Flush(); err != nil {")
	f.line("\t\treturn nil, err")
	f.line("\t}")
	f.line("\treturn buf.Bytes(), nil")
	f.line("}")
	f.blank()
	f.line("// Decode%s parses bytes into a new message (with defaults pre-applied).", typeName)
	f.line("func Decode%s(data []byte) (*%s, error) {", typeName, typeName)
	f.line("\tm := New%s()", typeName)
	f.line("\tif err := m.unmarshal(sofab.NewDecoder(bytes.NewReader(data))); err != nil {")
	f.line("\t\treturn nil, err")
	f.line("\t}")
	f.line("\treturn m, nil")
	f.line("}")
	return f.bytes(g.banner, g.license)
}

func (g *gen) emitDefaults(f *gofile, fields []*ir.Field) {
	for _, fld := range fields {
		lit, ok := g.defaultLiteral(fld)
		if !ok {
			continue
		}
		f.line("\tm.%s = %s", exported(fld.Name), lit)
	}
}
