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
		schema: s,
		pkg:    cfgString(cfg, "package", "messages"),
		banner: cfgString(cfg, "tool_banner", "sofabgen"),
		omit:   cfgBool(cfg, "omit_defaults"),
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
	schema *ir.Schema
	pkg    string
	banner string
	omit   bool // omit fields equal to their default (omit_defaults config)
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
	return f.bytes(g.banner)
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
		f.line("\te.WriteBytes(%d, %s)", fld.ID, acc)
		return
	case ir.KindStruct, ir.KindUnion:
		f.line("\te.WriteSequenceBegin(%d)", fld.ID)
		f.line("\t%s.marshal(e)", acc)
		f.line("\te.WriteSequenceEnd()")
		return
	case ir.KindArray:
		g.emitMarshalArray(f, fld, acc)
		return
	}
	// Scalar/string/enum/bitfield: optionally omit when equal to the default.
	if g.omit {
		f.line("\tif %s != %s {", acc, g.defaultCompare(fld))
		f.line("\t\t%s", write)
		f.line("\t}")
	} else {
		f.line("\t%s", write)
	}
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
	switch fld.Elem {
	case ir.KindU8, ir.KindU16, ir.KindU32, ir.KindU64:
		f.line("\tsofab.WriteUnsignedArray(e, %d, %s)", fld.ID, acc)
	case ir.KindI8, ir.KindI16, ir.KindI32, ir.KindI64:
		f.line("\tsofab.WriteSignedArray(e, %d, %s)", fld.ID, acc)
	case ir.KindFP32:
		f.line("\te.WriteFloat32Array(%d, %s)", fld.ID, acc)
	case ir.KindFP64:
		f.line("\te.WriteFloat64Array(%d, %s)", fld.ID, acc)
	case ir.KindString:
		f.line("\te.WriteSequenceBegin(%d)", fld.ID)
		f.line("\tfor i, s := range %s {", acc)
		f.line("\t\te.WriteString(sofab.ID(i), s)")
		f.line("\t}")
		f.line("\te.WriteSequenceEnd()")
	case ir.KindBlob:
		f.line("\te.WriteSequenceBegin(%d)", fld.ID)
		f.line("\tfor i, b := range %s {", acc)
		f.line("\t\te.WriteBytes(sofab.ID(i), b)")
		f.line("\t}")
		f.line("\te.WriteSequenceEnd()")
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
	switch fld.Elem {
	case ir.KindU8, ir.KindU16, ir.KindU32, ir.KindU64:
		f.line("\t\t\t%s, _ = sofab.ReadUnsignedArray[%s](d)", acc, goNumType(fld.Elem))
	case ir.KindI8, ir.KindI16, ir.KindI32, ir.KindI64:
		f.line("\t\t\t%s, _ = sofab.ReadSignedArray[%s](d)", acc, goNumType(fld.Elem))
	case ir.KindFP32:
		f.line("\t\t\t%s, _ = d.ReadFloat32Array()", acc)
	case ir.KindFP64:
		f.line("\t\t\t%s, _ = d.ReadFloat64Array()", acc)
	case ir.KindString:
		f.line("\t\t\t%s = %s[:0]", acc, acc)
		f.line("\t\t\tfor {")
		f.line("\t\t\t\tef, _ := d.Next()")
		f.line("\t\t\t\tif ef.Type == sofab.TypeSequenceEnd {")
		f.line("\t\t\t\t\tbreak")
		f.line("\t\t\t\t}")
		f.line("\t\t\t\ts, _ := d.String()")
		f.line("\t\t\t\t%s = append(%s, s)", acc, acc)
		f.line("\t\t\t}")
	case ir.KindBlob:
		f.line("\t\t\t%s = %s[:0]", acc, acc)
		f.line("\t\t\tfor {")
		f.line("\t\t\t\tef, _ := d.Next()")
		f.line("\t\t\t\tif ef.Type == sofab.TypeSequenceEnd {")
		f.line("\t\t\t\t\tbreak")
		f.line("\t\t\t\t}")
		f.line("\t\t\t\tb, _ := d.Bytes()")
		f.line("\t\t\t\t%s = append(%s, b)", acc, acc)
		f.line("\t\t\t}")
	}
}

// ---- per-message file ----------------------------------------------------

func (g *gen) messageFile(m *ir.Message) []byte {
	f := newGoFile(g.pkg)
	f.imp(corelibImport)
	f.imp("bytes")

	typeName := exported(m.Name)
	if m.Summary != "" {
		f.line("// %s — %s", typeName, oneline(m.Summary))
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
	return f.bytes(g.banner)
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
