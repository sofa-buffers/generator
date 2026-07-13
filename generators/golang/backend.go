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
	if g.hasObject() {
		files = append(files, generator.File{Path: pkgDir + "sofab_visitor.go", Content: g.preludeFile()})
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

// hasObject reports whether the schema emits at least one struct/union/message —
// i.e. at least one sofab.Visitor implementation, so the shared decode prelude
// (_visitorBase, narrow helpers, sequence collectors) is needed.
func (g *gen) hasObject() bool {
	if len(g.schema.Messages) > 0 {
		return true
	}
	for _, key := range g.schema.NamedOrder {
		switch g.schema.Named[key].Category {
		case ir.CatStruct, ir.CatUnion:
			return true
		}
	}
	return false
}

// preludeFile is the once-per-package decode support: the no-op _visitorBase that
// every generated object embeds, the integer-array narrowing helpers, and the
// collector visitors that gather the elements of a wrapper-sequence array. These
// are schema-independent, so the whole set is emitted unconditionally (unused
// generic types/functions are legal Go) and shared by every message file.
func (g *gen) preludeFile() []byte {
	f := newGoFile(g.pkg)
	f.imp(corelibImport)
	f.line(`// _visitorBase supplies no-op defaults for every sofab.Visitor method, so a
// generated object overrides only the callbacks its fields actually use.
type _visitorBase struct{}

func (_visitorBase) Unsigned(sofab.ID, uint64) error               { return nil }
func (_visitorBase) Signed(sofab.ID, int64) error                  { return nil }
func (_visitorBase) Float32(sofab.ID, float32) error               { return nil }
func (_visitorBase) Float64(sofab.ID, float64) error               { return nil }
func (_visitorBase) String(sofab.ID, string) error                 { return nil }
func (_visitorBase) Bytes(sofab.ID, []byte) error                  { return nil }
func (_visitorBase) UnsignedArray(sofab.ID, []uint64) error        { return nil }
func (_visitorBase) SignedArray(sofab.ID, []int64) error           { return nil }
func (_visitorBase) Float32Array(sofab.ID, []float32) error        { return nil }
func (_visitorBase) Float64Array(sofab.ID, []float64) error        { return nil }
func (_visitorBase) BeginSequence(sofab.ID) (sofab.Visitor, error) { return _visitorBase{}, nil }
func (_visitorBase) EndSequence() error                            { return nil }

// _narrowU / _narrowS copy a 64-bit-widened native array down to its declared
// element width.
func _narrowU[T ~uint8 | ~uint16 | ~uint32 | ~uint64](v []uint64) []T {
	out := make([]T, len(v))
	for i, x := range v {
		out[i] = T(x)
	}
	return out
}

func _narrowS[T ~int8 | ~int16 | ~int32 | ~int64](v []int64) []T {
	out := make([]T, len(v))
	for i, x := range v {
		out[i] = T(x)
	}
	return out
}

// _strSeq / _bytesSeq collect the elements of a string / blob array. Elements are
// keyed by index id (MESSAGE_SPEC S2): a default (empty) element is omitted on the
// wire, so we place each value at its id and fill any gap with the element default
// ("" / nil). Blob copies (the corelib value aliases the decode buffer).
type _strSeq struct {
	_visitorBase
	out *[]string
}

func (s *_strSeq) String(id sofab.ID, v string) error {
	for len(*s.out) <= int(id) {
		*s.out = append(*s.out, "")
	}
	(*s.out)[id] = v
	return nil
}

type _bytesSeq struct {
	_visitorBase
	out *[][]byte
}

func (s *_bytesSeq) Bytes(id sofab.ID, v []byte) error {
	for len(*s.out) <= int(id) {
		*s.out = append(*s.out, nil)
	}
	(*s.out)[id] = append([]byte(nil), v...)
	return nil
}

// _objSeq collects the elements of a struct/union array: each element is a nested
// sequence decoded into a freshly appended T (PT is *T and a Visitor).
type _objSeq[T any, PT interface {
	*T
	sofab.Visitor
}] struct {
	_visitorBase
	out *[]T
}

func (s *_objSeq[T, PT]) BeginSequence(_ sofab.ID) (sofab.Visitor, error) {
	var zero T
	*s.out = append(*s.out, zero)
	return PT(&(*s.out)[len(*s.out)-1]), nil
}

// _uMatSeq / _sMatSeq / _f32MatSeq / _f64MatSeq / _boolMatSeq collect the rows of
// a matrix (array whose elements are native arrays); each row arrives widened.
type _uMatSeq[T ~uint8 | ~uint16 | ~uint32 | ~uint64] struct {
	_visitorBase
	out *[][]T
}

func (s *_uMatSeq[T]) UnsignedArray(_ sofab.ID, v []uint64) error {
	*s.out = append(*s.out, _narrowU[T](v))
	return nil
}

type _sMatSeq[T ~int8 | ~int16 | ~int32 | ~int64] struct {
	_visitorBase
	out *[][]T
}

func (s *_sMatSeq[T]) SignedArray(_ sofab.ID, v []int64) error {
	*s.out = append(*s.out, _narrowS[T](v))
	return nil
}

type _f32MatSeq struct {
	_visitorBase
	out *[][]float32
}

func (s *_f32MatSeq) Float32Array(_ sofab.ID, v []float32) error {
	*s.out = append(*s.out, v)
	return nil
}

type _f64MatSeq struct {
	_visitorBase
	out *[][]float64
}

func (s *_f64MatSeq) Float64Array(_ sofab.ID, v []float64) error {
	*s.out = append(*s.out, v)
	return nil
}

type _boolMatSeq struct {
	_visitorBase
	out *[][]bool
}

func (s *_boolMatSeq) UnsignedArray(_ sofab.ID, v []uint64) error {
	row := make([]bool, len(v))
	for i, x := range v {
		row[i] = x != 0
	}
	*s.out = append(*s.out, row)
	return nil
}

// _seqSeq collects an array whose elements are themselves wrapper-sequence arrays:
// each element opens a sequence collected into a fresh inner slice by mk.
type _seqSeq[T any] struct {
	_visitorBase
	out *[][]T
	mk  func(*[]T) sofab.Visitor
}

func (s *_seqSeq[T]) BeginSequence(_ sofab.ID) (sofab.Visitor, error) {
	*s.out = append(*s.out, nil)
	return s.mk(&(*s.out)[len(*s.out)-1]), nil
}`)
	return f.bytes(g.banner, g.license)
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

// emitObject emits a struct + marshal + a sofab.Visitor decode implementation
// for an id scope. Decode is push/visitor: the struct embeds _visitorBase (no-op
// defaults) and overrides the callbacks its fields need, so DecodeX runs the
// corelib's zero-copy AcceptBytes cursor over the buffer instead of pulling each
// varint byte through a bufio.Reader.
func (g *gen) emitObject(f *gofile, typeName string, fields []*ir.Field) {
	f.imp(corelibImport)
	f.line("// %s is a generated SofaBuffers object.", typeName)
	f.line("type %s struct {", typeName)
	f.line("\t_visitorBase")
	// Declare fields widest-first to minimise struct padding; marshal/decode stay
	// in schema/id order, so the wire bytes are unchanged.
	for _, fld := range ir.SortedForLayout(fields) {
		tag := fmt.Sprintf("`json:%q`", fld.Name)
		f.line("\t%s %s %s%s", goFieldName(fld.Name), g.goType(fld), tag, fieldDoc(fld))
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

	g.emitVisitorMethods(f, typeName, fields)
}

// ---- per-field marshal/unmarshal ----------------------------------------

func (g *gen) emitMarshalField(f *gofile, fld *ir.Field) {
	acc := "m." + goFieldName(fld.Name)
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
		// bytes.Equal below is emitted into whatever file holds this marshal
		// (per-message or the shared types.go), so import "bytes" here rather
		// than relying on the message file's own bytes.Buffer import.
		f.imp("bytes")
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
		// A string element is a leaf: omit it when equal to the element default
		// (empty), leaving an id gap the decoder restores (MESSAGE_SPEC S2).
		f.line("%se.WriteSequenceBegin(%s)", ind, idExpr)
		f.line("%sfor %s, %s := range %s {", ind, iv, ev, val)
		f.line("%s\tif %s != \"\" {", ind, ev)
		f.line("%s\t\te.WriteString(sofab.ID(%s), %s)", ind, iv, ev)
		f.line("%s\t}", ind)
		f.line("%s}", ind)
		f.line("%se.WriteSequenceEnd()", ind)
	case ir.KindBlob:
		// A blob element is a leaf: omit it when equal to the element default
		// (empty), leaving an id gap the decoder restores (MESSAGE_SPEC S2).
		f.line("%se.WriteSequenceBegin(%s)", ind, idExpr)
		f.line("%sfor %s, %s := range %s {", ind, iv, ev, val)
		f.line("%s\tif len(%s) != 0 {", ind, ev)
		f.line("%s\t\te.WriteBytes(sofab.ID(%s), %s)", ind, iv, ev)
		f.line("%s\t}", ind)
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

// emitVisitorMethods emits the sofab.Visitor callbacks a type's fields need.
// Scalars bind straight into a struct member; native arrays arrive widened and
// narrow to the declared element width; nested structs/unions and every
// wrapper-sequence array descend via BeginSequence into a child visitor (a
// nested object, or a collector from arrayCollector). Unused callbacks fall back
// to the embedded _visitorBase no-ops.
func (g *gen) emitVisitorMethods(f *gofile, typeName string, fields []*ir.Field) {
	recv := "func (m *" + typeName + ") "

	// scalar callbacks
	var uns, sig, f32, f64, str, blob []string
	// array callbacks (native, delivered widened)
	var uArr, sArr, f32Arr, f64Arr []string
	// sequence descents (nested object + wrapper-sequence arrays)
	var seq []string

	arm := func(id int64, body string) string { return fmt.Sprintf("case %d:\n%s", id, body) }
	for _, fld := range fields {
		acc := "m." + goFieldName(fld.Name)
		switch fld.Kind {
		case ir.KindU8, ir.KindU16, ir.KindU32, ir.KindU64:
			uns = append(uns, arm(fld.ID, fmt.Sprintf("%s = %s(v)", acc, goNumType(fld.Kind))))
		case ir.KindBitfield:
			uns = append(uns, arm(fld.ID, fmt.Sprintf("%s = %s(v)", acc, g.typeName(fld.Ref.Key))))
		case ir.KindBool:
			uns = append(uns, arm(fld.ID, acc+" = v != 0"))
		case ir.KindI8, ir.KindI16, ir.KindI32, ir.KindI64:
			sig = append(sig, arm(fld.ID, fmt.Sprintf("%s = %s(v)", acc, goNumType(fld.Kind))))
		case ir.KindEnum:
			sig = append(sig, arm(fld.ID, fmt.Sprintf("%s = %s(v)", acc, g.typeName(fld.Ref.Key))))
		case ir.KindFP32:
			f32 = append(f32, arm(fld.ID, acc+" = v"))
		case ir.KindFP64:
			f64 = append(f64, arm(fld.ID, acc+" = v"))
		case ir.KindString:
			str = append(str, arm(fld.ID, acc+" = v"))
		case ir.KindBlob:
			// v aliases the decode buffer (AcceptBytes) — copy what we keep.
			blob = append(blob, arm(fld.ID, acc+" = append([]byte(nil), v...)"))
		case ir.KindStruct, ir.KindUnion:
			seq = append(seq, arm(fld.ID, fmt.Sprintf("return &%s, nil", acc)))
		case ir.KindArray:
			// A wire element count above the schema `count` capacity is INVALID
			// per MESSAGE_SPEC §3+§7 — reject, never clamp or keep-all
			// (generator#100). Count-less (dynamic) arrays have no bound.
			guard := ""
			if fld.HasCount {
				guard = fmt.Sprintf("if len(v) > %d {\n\t\t\treturn sofab.ErrInvalidMsg\n\t\t}\n\t\t", fld.Count)
			}
			switch {
			case isUnsignedNativeArray(fld.Elem):
				uArr = append(uArr, arm(fld.ID, guard+g.narrowArrayStmt(acc, fld.Elem, fld.ElemRef)))
			case isSignedNativeArray(fld.Elem):
				sArr = append(sArr, arm(fld.ID, guard+g.narrowArrayStmt(acc, fld.Elem, fld.ElemRef)))
			case fld.Elem == ir.KindFP32:
				f32Arr = append(f32Arr, arm(fld.ID, guard+acc+" = v"))
			case fld.Elem == ir.KindFP64:
				f64Arr = append(f64Arr, arm(fld.ID, guard+acc+" = v"))
			default: // wrapper-sequence array (string/blob/struct/union/nested)
				seq = append(seq, arm(fld.ID, fmt.Sprintf("%s = %s[:0]\n\t\treturn %s, nil", acc, acc, g.arrayCollector("&"+acc, fld.Elem, fld.ElemRef, fld.ElemItems))))
			}
		}
	}

	emitIDSwitch(f, recv, "Unsigned(id sofab.ID, v uint64) error", uns)
	emitIDSwitch(f, recv, "Signed(id sofab.ID, v int64) error", sig)
	emitIDSwitch(f, recv, "Float32(id sofab.ID, v float32) error", f32)
	emitIDSwitch(f, recv, "Float64(id sofab.ID, v float64) error", f64)
	emitIDSwitch(f, recv, "String(id sofab.ID, v string) error", str)
	emitIDSwitch(f, recv, "Bytes(id sofab.ID, v []byte) error", blob)
	emitIDSwitch(f, recv, "UnsignedArray(id sofab.ID, v []uint64) error", uArr)
	emitIDSwitch(f, recv, "SignedArray(id sofab.ID, v []int64) error", sArr)
	emitIDSwitch(f, recv, "Float32Array(id sofab.ID, v []float32) error", f32Arr)
	emitIDSwitch(f, recv, "Float64Array(id sofab.ID, v []float64) error", f64Arr)

	if len(seq) > 0 {
		f.line("%sBeginSequence(id sofab.ID) (sofab.Visitor, error) {", recv)
		f.line("\tswitch id {")
		for _, a := range seq {
			f.line("\t%s", a)
		}
		f.line("\t}")
		f.line("\treturn _visitorBase{}, nil")
		f.line("}")
		f.blank()
	}
}

// emitIDSwitch emits `func … { switch id { <arms> }; return nil }` for a scalar/
// native-array callback, or nothing when the type has no field for it (the
// embedded _visitorBase no-op then applies).
func emitIDSwitch(f *gofile, recv, sig string, arms []string) {
	if len(arms) == 0 {
		return
	}
	f.line("%s%s {", recv, sig)
	f.line("\tswitch id {")
	for _, a := range arms {
		f.line("\t%s", a)
	}
	f.line("\t}")
	f.line("\treturn nil")
	f.line("}")
	f.blank()
}

// narrowArrayStmt assigns a widened native array (v) into the field, narrowing to
// the declared element width. 64-bit widths (and bitfield/enum at 64-bit) assign
// the widened slice directly; narrower widths allocate via _narrowU/_narrowS.
func (g *gen) narrowArrayStmt(acc string, elem ir.Kind, ref *ir.TypeRef) string {
	switch elem {
	case ir.KindU64:
		return acc + " = v"
	case ir.KindI64:
		return acc + " = v"
	case ir.KindBool:
		return fmt.Sprintf("%s = make([]bool, len(v))\n\t\tfor _i, _x := range v {\n\t\t\t%s[_i] = _x != 0\n\t\t}", acc, acc)
	case ir.KindBitfield:
		return fmt.Sprintf("%s = _narrowU[%s](v)", acc, g.typeName(ref.Key))
	case ir.KindEnum:
		return fmt.Sprintf("%s = _narrowS[%s](v)", acc, g.typeName(ref.Key))
	case ir.KindU8, ir.KindU16, ir.KindU32:
		return fmt.Sprintf("%s = _narrowU[%s](v)", acc, goNumType(elem))
	default: // i8/i16/i32
		return fmt.Sprintf("%s = _narrowS[%s](v)", acc, goNumType(elem))
	}
}

// arrayCollector returns an expression constructing the sofab.Visitor that
// collects a wrapper-sequence array's elements into the slice at ptr (an address
// expression like "&m.Field" or a "*[]T" pointer). It recurses for nested arrays.
func (g *gen) arrayCollector(ptr string, elem ir.Kind, ref *ir.TypeRef, items *ir.ArrayElem) string {
	switch elem {
	case ir.KindString:
		return fmt.Sprintf("&_strSeq{out: %s}", ptr)
	case ir.KindBlob:
		return fmt.Sprintf("&_bytesSeq{out: %s}", ptr)
	case ir.KindStruct, ir.KindUnion:
		t := g.typeName(ref.Key)
		return fmt.Sprintf("&_objSeq[%s, *%s]{out: %s}", t, t, ptr)
	case ir.KindArray:
		if isNativeArrayElem(items.Elem) {
			return g.matrixCollector(ptr, items.Elem, items.ElemRef)
		}
		// Array of wrapper-sequence arrays: each element is itself a sequence
		// collected into an inner slice by a recursively-built collector.
		inner := g.goArrayElem(items.Elem, items.ElemRef, items.ElemItems)
		mk := g.arrayCollector("p", items.Elem, items.ElemRef, items.ElemItems)
		return fmt.Sprintf("&_seqSeq[%s]{out: %s, mk: func(p *[]%s) sofab.Visitor { return %s }}", inner, ptr, inner, mk)
	}
	return "_visitorBase{}"
}

// matrixCollector builds the row collector for an array whose elements are native
// arrays ([][]elem): rows arrive via the widened *Array callbacks.
func (g *gen) matrixCollector(ptr string, elem ir.Kind, ref *ir.TypeRef) string {
	switch elem {
	case ir.KindU8, ir.KindU16, ir.KindU32, ir.KindU64:
		return fmt.Sprintf("&_uMatSeq[%s]{out: %s}", goNumType(elem), ptr)
	case ir.KindI8, ir.KindI16, ir.KindI32, ir.KindI64:
		return fmt.Sprintf("&_sMatSeq[%s]{out: %s}", goNumType(elem), ptr)
	case ir.KindBitfield:
		return fmt.Sprintf("&_uMatSeq[%s]{out: %s}", g.typeName(ref.Key), ptr)
	case ir.KindEnum:
		return fmt.Sprintf("&_sMatSeq[%s]{out: %s}", g.typeName(ref.Key), ptr)
	case ir.KindFP32:
		return fmt.Sprintf("&_f32MatSeq{out: %s}", ptr)
	case ir.KindFP64:
		return fmt.Sprintf("&_f64MatSeq{out: %s}", ptr)
	case ir.KindBool:
		return fmt.Sprintf("&_boolMatSeq{out: %s}", ptr)
	}
	return "_visitorBase{}"
}

func isUnsignedNativeArray(k ir.Kind) bool {
	return k == ir.KindU8 || k == ir.KindU16 || k == ir.KindU32 || k == ir.KindU64 || k == ir.KindBitfield || k == ir.KindBool
}
func isSignedNativeArray(k ir.Kind) bool {
	return k == ir.KindI8 || k == ir.KindI16 || k == ir.KindI32 || k == ir.KindI64 || k == ir.KindEnum
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
	f.line("// Decode runs the corelib's zero-copy AcceptBytes cursor over the buffer,")
	f.line("// dispatching each field to the message's sofab.Visitor implementation.")
	f.line("func Decode%s(data []byte) (*%s, error) {", typeName, typeName)
	f.line("\tm := New%s()", typeName)
	f.line("\tif err := sofab.AcceptBytes(data, m); err != nil {")
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
		f.line("\tm.%s = %s", goFieldName(fld.Name), lit)
	}
}
