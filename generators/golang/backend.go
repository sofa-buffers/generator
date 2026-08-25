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
		limits:  resolveLimits(s, cfg),
		size:    generator.NewSizePolicy(cfg),
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
	if g.sizeErr != nil {
		return nil, g.sizeErr
	}
	return files, nil
}

type gen struct {
	schema  *ir.Schema
	pkg     string
	banner  string
	license string // SPDX id, "" to omit the header line
	limits  limitSet
	// size is the max_message_size policy; sizeErr carries a violation out of
	// the emit path, which has no error channel of its own.
	size    generator.SizePolicy
	sizeErr error
}

// messageSize resolves a message's worst-case encoded size via the shared walk
// (ir.MaxWireSize), falling back to the configured max_message_size ceiling when
// a field is unbounded. The emit path has no error channel, so a violation of an
// explicitly configured ceiling is recorded here and surfaced by Generate.
func (g *gen) messageSize(name string, fields []*ir.Field) generator.MessageSize {
	ms, err := g.size.Resolve(name, fields)
	if err != nil && g.sizeErr == nil {
		g.sizeErr = err
	}
	return ms
}

// limitSet is the receiver-side decode-limit configuration (generator#102),
// resolved against the schema: each active entry is the configured cap raised
// to the largest schema bound of its kind, so a schema-bounded field larger
// than the cap stays governed by its schema bound alone (the corelib enforces
// these globally per decode). Every cap is always SET — the target carries a
// finite default that the config key only overrides (§9.5, generator#385) — so
// an entry is active exactly when the schema actually has an unbounded field of
// that kind; otherwise the cap would be inert and no plumbing is emitted.
type limitSet struct {
	arrayCount, stringLen, blobLen int64
	arrayHas, stringHas, blobHas   bool
}

func (l limitSet) any() bool { return l.arrayHas || l.stringHas || l.blobHas }

// resolveLimits resolves the max_dyn_* caps over the target's finite defaults
// and against the schema's bounds (see limitSet).
func resolveLimits(s *ir.Schema, cfg map[string]any) limitSet {
	var all []*ir.Field
	for _, m := range s.Messages {
		all = append(all, m.Fields...)
	}
	b := ir.Bounds(all)
	d := generator.ServerDynLimits.Resolve(cfg)
	var l limitSet
	if b.HasDynArray {
		l.arrayCount, l.arrayHas = max(d.ArrayCount, b.MaxCount), true
	}
	if b.HasDynString {
		l.stringLen, l.stringHas = max(d.StringLen, b.MaxStringLen), true
	}
	if b.HasDynBlob {
		l.blobLen, l.blobHas = max(d.BlobLen, b.MaxBlobLen), true
	}
	return l
}

// hasObject reports whether the schema emits at least one struct/union/message —
// i.e. at least one sofab.Visitor implementation, so the once-per-package prelude
// (the isDefault contract, the receiver-side limit constants) is needed.
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

// preludeFile is the once-per-package decode support. Everything in it that was
// schema-independent -- the no-op visitor base, the string/blob/object/nested
// collectors, row placement and the matrix collectors -- is corelib-go's now
// (sofab.VisitorBase, sofab.StringSeq and siblings, generator#345), so what is
// left is what names a GENERATED symbol: the isDefault contract, plus the
// receiver-side limit constants the config bakes in.
func (g *gen) preludeFile() []byte {
	f := newGoFile(g.pkg)
	f.line(`// _isDefaulter is implemented by every generated struct/union type: isDefault
// reports whether the object equals its declared default, compared per child
// field and recursively (S2) -- never as a byte image. It is the explicit form of
// the predicate lazy framing applies implicitly ("not one child was written"),
// generated from the very same per-field expressions the writer uses so the two
// cannot drift apart.
type _isDefaulter interface{ isDefault() bool }`)
	if g.limits.any() {
		f.blank()
		f.line("// Receiver-side decode limits, baked from the sofabgen config")
		f.line("// (max_dyn_array_count / max_dyn_string_len / max_dyn_blob_len). They govern")
		f.line("// only fields the schema left unbounded; each cap is raised to the largest")
		f.line("// schema bound of its kind, so a schema-bounded field stays governed by its")
		f.line("// own bound alone. Exceeding a cap fails Decode with sofab.ErrLimitExceeded.")
		f.line("const (")
		if g.limits.arrayHas {
			f.line("\tMaxDynArrayCount = %d", g.limits.arrayCount)
		}
		if g.limits.stringHas {
			f.line("\tMaxDynStringLen = %d", g.limits.stringLen)
		}
		if g.limits.blobHas {
			f.line("\tMaxDynBlobLen = %d", g.limits.blobLen)
		}
		f.line(")")
	}
	return f.bytes(g.banner, g.license)
}

// acceptOpts renders the sofab decode options for the active receiver-side
// limits ("" when none), appended to every generated AcceptBytes call.
func (g *gen) acceptOpts() string {
	var opts []string
	if g.limits.arrayHas {
		opts = append(opts, "sofab.WithMaxArrayCount(MaxDynArrayCount)")
	}
	if g.limits.stringHas {
		opts = append(opts, "sofab.WithMaxStringLen(MaxDynStringLen)")
	}
	if g.limits.blobHas {
		opts = append(opts, "sofab.WithMaxBlobLen(MaxDynBlobLen)")
	}
	if len(opts) == 0 {
		return ""
	}
	return ", " + strings.Join(opts, ", ")
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
		doc := oneline(fl.Description)
		if fl.HasDefault {
			note := "(default: false)"
			if fl.Default {
				note = "(default: true)"
			}
			if doc != "" {
				doc += " "
			}
			doc += note
		}
		if doc != "" {
			doc = " // " + doc
		}
		f.line("\t%s%s %s = 1 << %d%s", tn, exported(fl.Name), tn, fl.Pos, doc)
	}
	f.line(")")
	f.blank()
}

// emitObject emits a struct + marshal + a sofab.Visitor decode implementation
// for an id scope. Decode is push/visitor: the struct embeds sofab.VisitorBase
// (no-op defaults) and overrides the callbacks its fields need.
//
// One visitor, two entry points. DecodeX feeds the corelib's decoder a buffer
// the caller already holds; DecodeXFrom feeds it whatever a reader delivers, so
// nothing larger than one fed chunk is ever resident (§5.6). Both are Feed, on
// the same state machine, so what is emitted here serves both and neither can
// tell which is driving it.
//
// The object carries two pieces of decode STATE, and both are consequences of
// CORELIB_PLAN §6.6: the codec builds no aggregate, so a string or blob arrives
// in pieces and the destination assembles it.
//
//   - _acc is the assembly buffer (sofab.PayloadAcc). ONE per object is enough
//     and correct: a fixlen payload is contiguous on the wire, so two of this
//     object's fields can never be in flight at once, and Take opens a fresh
//     payload at every offset == 0. It costs nothing while payloads arrive
//     whole -- the single-piece case hands the fed chunk straight back.
//   - sofab.StringCheck is the decode's SOFAB_STRICT_UTF8 policy (§6.4),
//     delivered by the decoder before this scope's first string. Embedding it
//     promotes UTF8Valid onto the object, so the check a generated arm runs is
//     the one the caller configured rather than the build tag alone.
func (g *gen) emitObject(f *gofile, typeName string, fields []*ir.Field) {
	f.imp(corelibImport)
	f.line("// %s is a generated SofaBuffers object.", typeName)
	f.line("type %s struct {", typeName)
	f.line("\tsofab.VisitorBase")
	if hasStringField(fields) {
		f.line("\tsofab.StringCheck")
	}
	// Declare fields widest-first to minimise struct padding; marshal/decode stay
	// in schema/id order, so the wire bytes are unchanged.
	for _, fld := range ir.SortedForLayout(fields) {
		tag := fmt.Sprintf("`json:%q`", fld.Name)
		name := goFieldName(fld.Name)
		note := generator.BoundNote(fld, generator.StorageDynamic)
		if note != "" && !fld.Deprecated {
			// A schema bound does not fit the trailing comment, so the field takes
			// the leading doc-block form (generator#308) -- the same shape a
			// deprecated field already uses.
			if doc := fieldDocText(fld); doc != "" {
				f.line("\t// %s %s", name, doc)
				f.line("\t//")
			}
			f.line("\t// %s", note)
			f.line("\t%s %s %s", name, g.goType(fld), tag)
			continue
		}
		if fld.Deprecated {
			// Go has no deprecation attribute; the godoc convention is the marker.
			// A "Deprecated:" paragraph must stand on its own line, so a deprecated
			// field carries a leading doc block (keeping its description) instead of
			// the trailing description comment used elsewhere.
			if doc := fieldDocText(fld); doc != "" {
				f.line("\t// %s %s", name, doc)
				f.line("\t//")
			}
			if note != "" {
				f.line("\t// %s", note)
				f.line("\t//")
			}
			f.line("\t// Deprecated: retained for backward compatibility only; do not use in new code.")
			f.line("\t%s %s %s", name, g.goType(fld), tag)
			continue
		}
		f.line("\t%s %s %s%s", name, g.goType(fld), tag, fieldDoc(fld))
	}
	if hasFixlenField(fields) {
		f.line("\t// _acc assembles a string or blob payload the codec delivers in pieces")
		f.line("\t// (S6.6.3). Unexported, so it is not part of the object's JSON form.")
		f.line("\t_acc sofab.PayloadAcc")
	}
	f.line("}")
	f.blank()

	// marshal
	f.line("func (m *%s) Serialize(e *sofab.Encoder) {", typeName)
	for _, fld := range fields {
		g.emitMarshalField(f, fld)
	}
	f.line("}")
	f.blank()

	g.emitIsDefault(f, typeName, fields)

	g.emitVisitorMethods(f, typeName, fields)
}

// emitIsDefault emits the object's all-default predicate. It is the exact
// negation of what marshal writes: the object is default iff marshal would emit
// no child at all, evaluated per field and recursively (MESSAGE_SPEC §2). Keep
// this in lockstep with emitMarshalField -- both are generated from
// fieldIsDefaultExpr's per-field expressions for exactly that reason. A predicate
// that disagrees with the writer omits a field that is on the wire, or keeps one
// that is not.
func (g *gen) emitIsDefault(f *gofile, typeName string, fields []*ir.Field) {
	f.line("func (m *%s) isDefault() bool {", typeName)
	if len(fields) == 0 {
		f.line("\treturn true")
		f.line("}")
		f.blank()
		return
	}
	for _, fld := range fields {
		f.line("\tif !(%s) {", g.fieldIsDefaultExpr(f, fld))
		f.line("\t\treturn false")
		f.line("\t}")
	}
	f.line("\treturn true")
	f.line("}")
	f.blank()
}

// fieldIsDefaultExpr is the boolean expression "this field equals its default",
// i.e. the negation of emitMarshalField's write guard for the same field.
func (g *gen) fieldIsDefaultExpr(f *gofile, fld *ir.Field) string {
	acc := "m." + goFieldName(fld.Name)
	switch fld.Kind {
	case ir.KindBlob:
		if def, ok := g.defaultLiteral(fld); ok {
			f.imp("bytes")
			return fmt.Sprintf("bytes.Equal(%s, %s)", acc, def)
		}
		return fmt.Sprintf("len(%s) == 0", acc)
	case ir.KindStruct, ir.KindUnion:
		// Lazily framed: the frame survives iff the nested marshal wrote a child,
		// which is exactly "the nested object is not default".
		return fmt.Sprintf("%s.isDefault()", acc)
	case ir.KindArray:
		return g.arrayIsDefaultExpr(f, fld, acc)
	}
	return fmt.Sprintf("%s == %s", acc, g.defaultCompare(fld))
}

// arrayIsDefaultExpr mirrors emitMarshalArray. An array's declared `count: N` is
// a CAPACITY, never a length (MESSAGE_SPEC §3), so it takes no part in this test:
// the value is compared against the declared default exactly as written, with no
// padding to N on either side, and against the empty collection when none is
// declared. A count:N array is therefore default only when it is EMPTY -- an
// all-zero N-element value is a length-N array, which differs from the empty one
// and stays on the wire.
func (g *gen) arrayIsDefaultExpr(f *gofile, fld *ir.Field, acc string) string {
	if isNativeArrayElem(fld.Elem) {
		if def, ok := g.defaultLiteral(fld); ok {
			f.imp("slices")
			return fmt.Sprintf("slices.Equal(%s, %s)", acc, def)
		}
		return fmt.Sprintf("len(%s) == 0", acc)
	}
	// Wrapper array: the writer emits a child for every element it holds, because
	// the LAST element is written whatever its value (§2) -- so "no child is
	// written" is exactly "the array is empty", and the two cannot drift apart.
	return fmt.Sprintf("len(%s) == 0", acc)
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
		// blob is a leaf: omit when equal to its default. With a schema default,
		// compare against its literal via bytes.Equal (importing "bytes" into
		// whatever file holds this marshal, per-message or the shared types.go).
		// With no default the default is the empty slice, so the idiomatic
		// len()==0 test is exactly equivalent to bytes.Equal(x, nil) — matching
		// the array/string/scalar omit-checks and leaving generated code free of
		// the bytes dependency in the common case (#113).
		if def, ok := g.defaultLiteral(fld); ok {
			f.imp("bytes")
			f.line("\tif !bytes.Equal(%s, %s) {", acc, def)
		} else {
			f.line("\tif len(%s) != 0 {", acc)
		}
		f.line("\t\te.WriteBytes(%d, %s)", fld.ID, acc)
		f.line("\t}")
		return
	case ir.KindStruct, ir.KindUnion:
		// MESSAGE_SPEC S2: the != default test is per field and a sequence is no
		// exception, so the frame is opened LAZILY -- the corelib holds the header
		// back and writes it only once a child field appears. The nested marshal
		// omits every child that equals its default, so "no child was written" IS
		// "the object equals its declared default", evaluated per field and
		// recursively. WriteSequenceEnd then drops the contentless frame: an
		// all-default nested object is omitted, not emitted as an empty wrapper.
		f.line("\te.WriteSequenceBeginLazy(%d)", fld.ID)
		f.line("\t%s.Serialize(e)", acc)
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
	// array is a wrapper sequence, opened lazily and closed with the dropping end
	// (MESSAGE_SPEC §2), so an empty one is omitted rather than framed empty.
	//
	// A declared `count: N` takes no part in either test. `count` is a CAPACITY,
	// never a length (§3): it never reaches the wire, so the value is compared
	// against the declared default exactly as written -- neither side padded to N
	// -- and against the empty collection when no default is declared.
	if isNativeArrayElem(fld.Elem) {
		if def, ok := g.defaultLiteral(fld); ok {
			f.imp("slices")
			f.line("\tif !slices.Equal(%s, %s) {", acc, def)
		} else {
			f.line("\tif len(%s) != 0 {", acc)
		}
		g.marshalArray(f, "\t\t", fmt.Sprintf("%d", fld.ID), acc, fld.Elem, fld.ElemRef, fld.ElemItems, 0, "")
		f.line("\t}")
		return
	}
	// The field-level wrapper frame is dropped when no element is written, and
	// absence then reconstructs the field's default. That is correct because a
	// wrapper array's declared `default` is not materialized today (New<Msg>
	// leaves it the empty collection), so absent and explicitly-empty denote the
	// same value. If that gap is ever closed, this call needs a guard --
	// `if !equal(value, default) { ... WriteSequenceEndKeep() }` -- so that a value
	// differing from a non-empty default still reaches the wire as the empty
	// wrapper, the only encoding of "explicitly empty" (MESSAGE_SPEC §2, §3).
	g.marshalArray(f, "\t", fmt.Sprintf("%d", fld.ID), acc, fld.Elem, fld.ElemRef, fld.ElemItems, 0, "")
}

// lastElemExpr is the "this element is the array's last" test, at loop position
// iv over the value val.
//
// It is the whole of the positional half of MESSAGE_SPEC §2's element rule. A
// wrapper array carries no length field: its decoded length is *highest present
// id + 1* (§5.1), so the element at the highest index is the only one whose
// PRESENCE carries the length, and nothing that carries the length may be elided.
// Everything before it may be: an interior element equal to the element default
// is indistinguishable from an absent one, because the decoder restores an absent
// id from that same default. Hence: interior sparse, last always written.
//
// A declared `count: N` changes nothing here. N is a capacity, not a length (§3),
// so it can never restore an elided tail -- the same test applies with or without
// one.
func lastElemExpr(iv, val string) string {
	return fmt.Sprintf("%s == len(%s)-1", iv, val)
}

// emitSeqEnd closes the wrapper sequence opened at ind, choosing between the two
// closers the corelib offers. Every sequence is opened LAZILY (the corelib holds
// the header back until a child is written), so the closer alone decides whether
// a contentless one survives: WriteSequenceEnd drops it, WriteSequenceEndKeep
// forces the empty frame out.
//
// keepIf is the condition under which an empty frame must survive:
//   - "" -- never. A sequence-typed FIELD (a struct/union field, an array
//     wrapper): an all-default one is omitted and absence reconstructs it (§2).
//   - a lastElemExpr -- a sequence-form array ELEMENT, kept only at the array's
//     last index. In the interior it is dropped and leaves an id GAP, which is
//     what makes an all-default element sparse like any other default value.
//     Note this is decided from the position in the VALUE, at run time; the
//     schema cannot answer it.
func emitSeqEnd(f *gofile, ind, keepIf string) {
	if keepIf == "" {
		f.line("%se.WriteSequenceEnd()", ind)
		return
	}
	f.line("%sif %s {", ind, keepIf)
	f.line("%s\te.WriteSequenceEndKeep()", ind)
	f.line("%s} else {", ind)
	f.line("%s\te.WriteSequenceEnd()", ind)
	f.line("%s}", ind)
}

// marshalArray writes the array val as field idExpr. Numeric/enum/boolean/
// bitfield elements use the native array wire type (enum->signed, bool/bitfield->
// unsigned); string/blob/struct/union/array elements lower to a wrapper sequence
// whose child ids are the 0-based index (per MESSAGE_SPEC). Recurses for nested
// arrays, depth-suffixing loop vars to avoid collisions.
//
// Every element the value holds is written -- no trailing run is elided, of
// either element kind, because the wire count IS the array's length (§3) and the
// highest wrapper id IS its last index (§5.1). What the interior may drop is a
// value that is indistinguishable from absence, and only that.
//
// keepIf is the closer this call's own wrapper takes (see emitSeqEnd); the native
// element kinds open no sequence and ignore it.
func (g *gen) marshalArray(f *gofile, ind, idExpr, val string, elem ir.Kind, ref *ir.TypeRef, items *ir.ArrayElem, depth int, keepIf string) {
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
		// A string element is a leaf: in the array's INTERIOR it is omitted when it
		// equals the element default (empty), leaving an id gap the decoder restores
		// from that same default -- the ordinary sparse-field rule of MESSAGE_SPEC
		// §2, applied to an element. At the LAST index it is written whatever its
		// value: see lastElemExpr.
		f.line("%se.WriteSequenceBeginLazy(%s)", ind, idExpr)
		f.line("%sfor %s, %s := range %s {", ind, iv, ev, val)
		f.line("%s\tif %s != \"\" || %s {", ind, ev, lastElemExpr(iv, val))
		f.line("%s\t\te.WriteString(sofab.ID(%s), %s)", ind, iv, ev)
		f.line("%s\t}", ind)
		f.line("%s}", ind)
		emitSeqEnd(f, ind, keepIf)
	case ir.KindBlob:
		// A blob element is a leaf, exactly like the string element above.
		f.line("%se.WriteSequenceBeginLazy(%s)", ind, idExpr)
		f.line("%sfor %s, %s := range %s {", ind, iv, ev, val)
		f.line("%s\tif len(%s) != 0 || %s {", ind, ev, lastElemExpr(iv, val))
		f.line("%s\t\te.WriteBytes(sofab.ID(%s), %s)", ind, iv, ev)
		f.line("%s\t}", ind)
		f.line("%s}", ind)
		emitSeqEnd(f, ind, keepIf)
	case ir.KindStruct, ir.KindUnion:
		// A sequence-form element obeys the SAME rule as the leaf elements above --
		// one rule for both kinds -- and the lazily-held frame is where it is
		// applied. The nested marshal writes no child exactly when the element
		// equals its declared default, so the CLOSER alone decides: the dropping one
		// in the interior, where an all-default element vanishes into an id gap; the
		// keeping one at the last index, where it survives as an empty frame because
		// that presence is what fixes the array's length.
		f.line("%se.WriteSequenceBeginLazy(%s)", ind, idExpr)
		f.line("%sfor %s, %s := range %s {", ind, iv, ev, val)
		f.line("%s\te.WriteSequenceBeginLazy(sofab.ID(%s))", ind, iv)
		f.line("%s\t%s.Serialize(e)", ind, ev)
		emitSeqEnd(f, ind+"\t", lastElemExpr(iv, val))
		f.line("%s}", ind)
		emitSeqEnd(f, ind, keepIf)
	case ir.KindArray:
		f.line("%se.WriteSequenceBeginLazy(%s)", ind, idExpr)
		f.line("%sfor %s, %s := range %s {", ind, iv, ev, val)
		if isNativeArrayElem(items.Elem) {
			// A native row is a single count-prefixed value with no frame of its own,
			// so the rule lands on the WRITE rather than on a closer: an interior row
			// equal to the element default (the empty row) is not written at all, and
			// the last row always is.
			f.line("%s\tif len(%s) != 0 || %s {", ind, ev, lastElemExpr(iv, val))
			g.marshalArray(f, ind+"\t\t", fmt.Sprintf("sofab.ID(%s)", iv), ev, items.Elem, items.ElemRef, items.ElemItems, depth+1, "")
			f.line("%s\t}", ind)
		} else {
			// A wrapper row has its own frame, so it takes the closer instead -- the
			// same interior/last choice, expressed the same way as for a struct
			// element above.
			g.marshalArray(f, ind+"\t", fmt.Sprintf("sofab.ID(%s)", iv), ev, items.Elem, items.ElemRef, items.ElemItems, depth+1, lastElemExpr(iv, val))
		}
		f.line("%s}", ind)
		emitSeqEnd(f, ind, keepIf)
	}
}

// emitVisitorMethods emits the sofab.Visitor callbacks a type's fields need.
// Scalars bind straight into a struct member; native arrays arrive widened and
// narrow to the declared element width; nested structs/unions and every
// wrapper-sequence array descend via BeginSequence into a child visitor (a
// nested object, or a collector from arrayCollector). Unused callbacks fall back
// to the embedded sofab.VisitorBase no-ops.
func (g *gen) emitVisitorMethods(f *gofile, typeName string, fields []*ir.Field) {
	recv := "func (m *" + typeName + ") "

	// scalar callbacks
	var uns, sig, f32, f64, str, blob []string
	// The two HEADER callbacks. They carry every bound that is decided by a count
	// or a length WORD, which is where §5.2 requires it: INVALID dominates
	// INCOMPLETE, so a field whose header already breaches the schema must stay
	// INVALID even when the message then ends before the payload or the elements
	// arrive. A guard on the assembled value cannot say that -- it never runs for
	// a field that never completes (generator#216 / F-0032).
	var fixBegin, arrBegin []string
	// The per-ELEMENT array callbacks. A native array is delivered one element at
	// a time now (§6.6.3), so the declared element width is checked as each one
	// lands -- again for §5.2: an over-width element followed by a truncation is
	// INVALID where it lands, not INCOMPLETE at the end (generator#267,
	// Crucible F-0043).
	var uArr, sArr, f32Arr, f64Arr []string
	// sequence descents (nested object + wrapper-sequence arrays)
	var seq []string

	arm := func(id int64, body string) string { return fmt.Sprintf("case %d:\n%s", id, body) }
	// widthGuard rejects a value outside the range its declared integer width
	// allows (MESSAGE_SPEC §7.1, documentation#32). The width is a normative
	// validity bound, not a storage hint: the `uint8(v)` conversion that follows
	// IS the mask §7.1 forbids, so the check has to precede it. "" for u64/i64
	// (and for bool/enum/bitfield), whose range is the callback parameter's own.
	//
	// It serves the scalar callbacks and the array-element ones alike: both name
	// the value `v`, and the bound is the same statement about the same width.
	//
	// No negative-value term is needed on the unsigned side: Unsigned delivers a
	// uint64, so the comparison is already unsigned.
	widthGuard := func(k ir.Kind) string {
		lo, hi, ok := ir.NarrowRange(k)
		if !ok {
			return ""
		}
		if lo < 0 {
			return fmt.Sprintf("if v < %d || v > %d {\n\t\t\treturn sofab.ErrInvalidMsg\n\t\t}\n\t\t", lo, hi)
		}
		return fmt.Sprintf("if v > %d {\n\t\t\treturn sofab.ErrInvalidMsg\n\t\t}\n\t\t", hi)
	}
	// takePayload is the first two lines of every String/Bytes arm: contribute
	// this piece and do nothing until the payload is whole. The bound was already
	// taken at the length word (fixlenBeginBody), so what is left here is the
	// assembly and the store.
	takePayload := "_b, _done := m._acc.Take(total, offset, chunk)\n\t\tif !_done {\n\t\t\treturn nil\n\t\t}\n\t\t"
	// utf8Guard rejects invalid UTF-8 in a `string` being MATERIALIZED. It is
	// emitted inside the arm that resolves the destination and nowhere else:
	// validation belongs where a string is read into a field, never on a payload
	// the decoder is skipping (CORELIB_PLAN §6.4, generator#257). The corelib's
	// visitor path deliberately does not validate -- it cannot tell a field this
	// visitor binds from one it skips -- so the check is ours to make here.
	//
	// m.UTF8Valid, not the package-level sofab.UTF8Valid: the object embeds
	// sofab.StringCheck, so this reads the policy the decoder resolved for this
	// decode (WithStrictUTF8) and not only the build-tag gate.
	utf8Guard := "if !m.UTF8Valid(_b) {\n\t\t\treturn sofab.ErrInvalidMsg\n\t\t}\n\t\t"
	for _, fld := range fields {
		acc := "m." + goFieldName(fld.Name)
		switch fld.Kind {
		case ir.KindU8, ir.KindU16, ir.KindU32, ir.KindU64:
			uns = append(uns, arm(fld.ID, widthGuard(fld.Kind)+fmt.Sprintf("%s = %s(v)", acc, goNumType(fld.Kind))))
		case ir.KindBitfield:
			uns = append(uns, arm(fld.ID, fmt.Sprintf("%s = %s(v)", acc, g.typeName(fld.Ref.Key))))
		case ir.KindBool:
			uns = append(uns, arm(fld.ID, acc+" = v != 0"))
		case ir.KindI8, ir.KindI16, ir.KindI32, ir.KindI64:
			sig = append(sig, arm(fld.ID, widthGuard(fld.Kind)+fmt.Sprintf("%s = %s(v)", acc, goNumType(fld.Kind))))
		case ir.KindEnum:
			sig = append(sig, arm(fld.ID, fmt.Sprintf("%s = %s(v)", acc, g.typeName(fld.Ref.Key))))
		case ir.KindFP32:
			f32 = append(f32, arm(fld.ID, acc+" = v"))
		case ir.KindFP64:
			f64 = append(f64, arm(fld.ID, acc+" = v"))
		case ir.KindString:
			// A string is a byte container in Go (§6.4): the wire bytes pass
			// through verbatim and are validated here, at the destination.
			str = append(str, arm(fld.ID, takePayload+utf8Guard+acc+" = string(_b)"))
			if fld.HasMaxlen {
				fixBegin = append(fixBegin, arm(fld.ID, fixlenBeginBody("sofab.FixlenStr", fld.Maxlen)))
			}
		case ir.KindBlob:
			// _b may alias the caller's fed chunk -- a payload that arrived whole
			// in one piece is handed back as that piece (§6.7) -- so what is kept
			// is a copy. A split payload arrives in storage the accumulator hands
			// over, which needs no copy, but the arm cannot tell the two apart and
			// the copy is what makes the message outlive the input either way.
			blob = append(blob, arm(fld.ID, takePayload+acc+" = append([]byte(nil), _b...)"))
			if fld.HasMaxlen {
				fixBegin = append(fixBegin, arm(fld.ID, fixlenBeginBody("sofab.FixlenBlob", fld.Maxlen)))
			}
		case ir.KindStruct, ir.KindUnion:
			seq = append(seq, arm(fld.ID, fmt.Sprintf("return &%s, nil", acc)))
		case ir.KindArray:
			// The wire count M IS the array's length (MESSAGE_SPEC §3): the M
			// elements that arrived are the whole value. A declared `count: N` is a
			// capacity and bounds M at the header (arrayBeginBody); it never adds
			// elements, so there is nothing to fill in at [M, N).
			switch {
			case isNativeArrayElem(fld.Elem):
				arrBegin = append(arrBegin, arm(fld.ID, g.arrayBeginBody(fld, acc, g.goArrayElem(fld.Elem, fld.ElemRef, fld.ElemItems))))
				elemArm := arm(fld.ID, widthGuard(fld.Elem)+g.elemAppendStmt(acc, fld.Elem, fld.ElemRef))
				switch {
				case isUnsignedNativeArray(fld.Elem):
					uArr = append(uArr, elemArm)
				case isSignedNativeArray(fld.Elem):
					sArr = append(sArr, elemArm)
				case fld.Elem == ir.KindFP32:
					f32Arr = append(f32Arr, elemArm)
				default:
					f64Arr = append(f64Arr, elemArm)
				}
			default: // wrapper-sequence array (string/blob/struct/union/nested)
				seq = append(seq, arm(fld.ID, fmt.Sprintf("%s = %s[:0]\n\t\treturn %s, nil", acc, acc, g.arrayCollector("&"+acc, fld.Elem, fld.ElemRef, fld.ElemItems, capOf(fld.HasCount, fld.Count), emaxOf(fld.ElemMaxHas, fld.ElemMax)))))
			}
		}
	}

	emitIDSwitch(f, recv, "Unsigned(id sofab.ID, v uint64) error", uns)
	emitIDSwitch(f, recv, "Signed(id sofab.ID, v int64) error", sig)
	emitIDSwitch(f, recv, "Float32(id sofab.ID, v float32) error", f32)
	emitIDSwitch(f, recv, "Float64(id sofab.ID, v float64) error", f64)
	// The header pair. Both are ordinary Visitor methods now -- the corelib's
	// optional HeaderVisitor is gone, and with it the trap that emitting only one
	// of them left the interface assertion failing and BOTH hooks silently dead.
	// A type with no bound of that kind simply does not override the method and
	// sofab.VisitorBase's no-op stands.
	emitIDSwitch(f, recv, "FixlenBegin(id sofab.ID, sub sofab.FixlenSubtype, total int) error", fixBegin)
	emitIDSwitch(f, recv, "ArrayBegin(id sofab.ID, kind sofab.ArrayKind, count int) error", arrBegin)
	emitIDSwitch(f, recv, "String(id sofab.ID, total, offset int, chunk []byte) error", str)
	emitIDSwitch(f, recv, "Bytes(id sofab.ID, total, offset int, chunk []byte) error", blob)
	emitIDSwitch(f, recv, "ArrayUnsigned(id sofab.ID, _ int, v uint64) error", uArr)
	emitIDSwitch(f, recv, "ArraySigned(id sofab.ID, _ int, v int64) error", sArr)
	emitIDSwitch(f, recv, "ArrayFloat32(id sofab.ID, _ int, v float32) error", f32Arr)
	emitIDSwitch(f, recv, "ArrayFloat64(id sofab.ID, _ int, v float64) error", f64Arr)

	if len(seq) > 0 {
		f.line("%sBeginSequence(id sofab.ID) (sofab.Visitor, error) {", recv)
		f.line("\tswitch id {")
		for _, a := range seq {
			f.line("\t%s", a)
		}
		f.line("\t}")
		// An id this scope does not declare has no destination, so it is DECLINED:
		// corelib-go#121 made a nil child mean "skip this subtree", which delivers
		// nothing and builds nothing. Handing back a no-op visitor instead — what
		// this emitted until that landed — decoded every value and copied every
		// string out of the buffer before dropping it.
		f.line("\treturn nil, nil")
		f.line("}")
		f.blank()
	}
}

// fixlenBeginBody is the FixlenBegin arm rejecting a string/blob whose wire byte
// length exceeds the schema maxlen, at the length word and before a byte of
// payload is read (MESSAGE_SPEC §7.1, §5.2).
//
// It is the ONLY place that bound is taken now. The old whole-value guard beside
// it (`len(v) > N` on the assembled string) was the one that fired for a field
// that arrives and stayed silent for one that does not; this fires for both, and
// the payload callbacks below it therefore carry no bound at all.
//
// The compare sits inside the declared-subtype test. FixlenBegin fires for ANY
// fixlen subtype at a field id -- the corelib resolves what ARRIVED but cannot
// know what was declared, which is schema knowledge only generated code has --
// and a fixlen value whose subtype contradicts the declaration is SKIPPED, not
// measured against this field's maxlen (MESSAGE_SPEC §7.3, generator#224).
// Without the gate an fp64 (8 bytes) landing on a `blob` with `maxlen: 4` was
// rejected as INVALID instead of skipped.
func fixlenBeginBody(sub string, n int64) string {
	return fmt.Sprintf("if sub != %s {\n\t\t\treturn nil\n\t\t}\n\t\tif total > %d {\n\t\t\treturn sofab.ErrInvalidMsg\n\t\t}", sub, n)
}

// arrayBeginBody is the ArrayBegin arm for one native array field: the §7.3 kind
// gate, the schema count bound at the header, and the destination the elements
// are appended into.
//
// The kind gate is what the old header hook's `kind ==` test was, inverted into an
// early return because the arm now does more than compare: an array whose
// element kind contradicts the declaration was never this field's value
// (MESSAGE_SPEC §7.3, generator#259 / Crucible F-0042), so neither its count nor
// its elements may touch this field -- not the bound, and not the destination.
// Un-gated, an fp64 array of 8 elements landing on a declared `array<fp32,
// count 5>` was rejected as INVALID instead of skipped.
//
// This is also why the corelib defers the hook for a fixlen array until after the
// fixlen_word: the kind handed in is the real element subtype, never a guess. A
// message that ends between the count word and the fixlen_word is therefore
// INCOMPLETE -- no bound can be judged yet -- which is the intended verdict.
//
// The destination is opened here rather than in the element arm, which is also
// what makes a repeated id REPLACE the array rather than extend it (§7.4) -- and
// what makes an array that arrives EMPTY decode as the empty array rather than
// as a nil slice, which Go's zero value would render as JSON `null`.
//
// It is sized from the wire count, which is bounded before the make in both
// directions: by the schema `count:` where one is declared (the check on the
// line above), and by the receiver cap otherwise -- corelib-go rejects a count
// over max_dyn_array_count at the count varint, one callback earlier, which is
// exactly the allocation §6.2.1 gives that cap to bound. §6.6.1 puts the
// allocation on this side of the callback either way: "the generated layer
// allocates; the codec does not".
func (g *gen) arrayBeginBody(fld *ir.Field, acc, elemType string) string {
	body := fmt.Sprintf("if kind != sofab.%s {\n\t\t\treturn nil\n\t\t}\n\t\t", goArrayWireKind(fld.Elem))
	if fld.HasCount {
		body += fmt.Sprintf("if count > %d {\n\t\t\treturn sofab.ErrInvalidMsg\n\t\t}\n\t\t", fld.Count)
	}
	return body + fmt.Sprintf("%s = make([]%s, 0, count)", acc, elemType)
}

// elemAppendStmt appends one native array element, narrowed to the declared
// element width. The widthGuard on the line above is what makes the conversion a
// narrowing and not the §7.1 mask: a value outside the width is already refused.
func (g *gen) elemAppendStmt(acc string, elem ir.Kind, ref *ir.TypeRef) string {
	switch elem {
	case ir.KindU64, ir.KindI64, ir.KindFP32, ir.KindFP64:
		return fmt.Sprintf("%s = append(%s, v)", acc, acc)
	case ir.KindBool:
		return fmt.Sprintf("%s = append(%s, v != 0)", acc, acc)
	case ir.KindBitfield, ir.KindEnum:
		return fmt.Sprintf("%s = append(%s, %s(v))", acc, acc, g.typeName(ref.Key))
	default: // u8/u16/u32, i8/i16/i32
		return fmt.Sprintf("%s = append(%s, %s(v))", acc, acc, goNumType(elem))
	}
}

// hasStringField / hasFixlenField report what decode STATE an object needs: a
// string field means the UTF-8 policy (sofab.StringCheck), and any string or
// blob field means the payload accumulator, since both arrive in pieces.
func hasStringField(fields []*ir.Field) bool {
	for _, fld := range fields {
		if fld.Kind == ir.KindString {
			return true
		}
	}
	return false
}

func hasFixlenField(fields []*ir.Field) bool {
	for _, fld := range fields {
		if fld.Kind == ir.KindString || fld.Kind == ir.KindBlob {
			return true
		}
	}
	return false
}

// emitIDSwitch emits `func … { switch id { <arms> }; return nil }` for one
// visitor callback, or nothing when the type has no field for it -- the embedded
// sofab.VisitorBase no-op then applies, which is also what keeps a decode from
// paying a call per field for a callback nobody binds.
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

// arrayCollector returns an expression constructing the sofab.Visitor that
// collects a wrapper-sequence array's elements into the slice at ptr (an address
// expression like "&m.Field" or a "*[]T" pointer). It recurses for nested arrays.
func (g *gen) arrayCollector(ptr string, elem ir.Kind, ref *ir.TypeRef, items *ir.ArrayElem, cap, emax int64) string {
	switch elem {
	case ir.KindString:
		return fmt.Sprintf("&sofab.StringSeq{Out: %s, Cap: %d, ElemMax: %d}", ptr, cap, emax)
	case ir.KindBlob:
		return fmt.Sprintf("&sofab.BlobSeq{Out: %s, Cap: %d, ElemMax: %d}", ptr, cap, emax)
	case ir.KindStruct, ir.KindUnion:
		t := g.typeName(ref.Key)
		return fmt.Sprintf("&sofab.MessageSeq[%s, *%s]{Out: %s, Cap: %d}", t, t, ptr, cap)
	case ir.KindArray:
		if isNativeArrayElem(items.Elem) {
			return g.matrixCollector(ptr, items.Elem, items.ElemRef, cap)
		}
		// Array of wrapper-sequence arrays: each element is itself a sequence
		// collected into an inner slice by a recursively-built collector. The
		// inner collector carries the inner array's own count bound.
		inner := g.goArrayElem(items.Elem, items.ElemRef, items.ElemItems)
		mk := g.arrayCollector("p", items.Elem, items.ElemRef, items.ElemItems, capOf(items.HasCount, items.Count), emaxOf(items.ElemMaxHas, items.ElemMax))
		return fmt.Sprintf("&sofab.NestedSeq[%s]{Out: %s, Cap: %d, Make: func(p *[]%s) sofab.Visitor { return %s }}", inner, ptr, cap, inner, mk)
	}
	return "nil"
}

// capOf maps a schema count bound to the collector's cap field: N when the array
// declares a count, -1 (unbounded) otherwise. N is a CAPACITY: the collector uses
// it only to reject an out-of-range element id, never to size the result.
func capOf(hasCount bool, count int64) int64 {
	if hasCount {
		return count
	}
	return -1
}

// goArrayWireKind is the sofab.ArrayKind constant naming the wire element kind an
// array of `elem` is encoded with — what ArrayBegin reports for a header that IS
// this field's value. fp32 and fp64 are distinct kinds (they are two subtypes of
// the one fixlen-array wire type, told apart by the fixlen_word); bool/bitfield
// ride the unsigned array wire type and enum the signed one, exactly as
// isUnsignedNativeArray/isSignedNativeArray group them for the payload callbacks.
// The corelib spells these constants with an Array prefix because the bare
// Unsigned/Signed names are taken by its element-type constraints.
func goArrayWireKind(elem ir.Kind) string {
	switch {
	case elem == ir.KindFP32:
		return "ArrayFp32"
	case elem == ir.KindFP64:
		return "ArrayFp64"
	case isSignedNativeArray(elem):
		return "ArraySigned"
	default:
		return "ArrayUnsigned"
	}
}

// emaxOf maps a string/blob element maxlen bound to the collector's emax field:
// the maxlen when present, -1 (unbounded) otherwise. A wrapper element longer
// than emax is malformed input (MESSAGE_SPEC §7.1), rejected as INVALID.
func emaxOf(hasMax bool, max int64) int64 {
	if hasMax {
		return max
	}
	return -1
}

// matrixCollector builds the row collector for an array whose elements are native
// arrays ([][]elem): rows arrive via the widened *Array callbacks, keyed by the
// row's element id. cap is the OUTER array's count bound, which bounds that id.
func (g *gen) matrixCollector(ptr string, elem ir.Kind, ref *ir.TypeRef, cap int64) string {
	// The row element's declared width travels with the collector, so the scan
	// runs before sofab.Narrow* masks anything (generator#330). NarrowRange
	// answers false for u64/i64 and for enum/bitfield -- all four span the callback
	// parameter's own range, so the zero bound switches the scan off rather than
	// emitting one that can never fire.
	lo, hi, _ := ir.NarrowRange(elem)
	switch elem {
	case ir.KindU8, ir.KindU16, ir.KindU32, ir.KindU64:
		return fmt.Sprintf("&sofab.UnsignedMatrixSeq[%s]{Out: %s, Cap: %d, Hi: %d}", goNumType(elem), ptr, cap, uint64(hi))
	case ir.KindI8, ir.KindI16, ir.KindI32, ir.KindI64:
		return fmt.Sprintf("&sofab.SignedMatrixSeq[%s]{Out: %s, Cap: %d, Lo: %d, Hi: %d}", goNumType(elem), ptr, cap, lo, hi)
	case ir.KindBitfield:
		return fmt.Sprintf("&sofab.UnsignedMatrixSeq[%s]{Out: %s, Cap: %d, Hi: 0}", g.typeName(ref.Key), ptr, cap)
	case ir.KindEnum:
		return fmt.Sprintf("&sofab.SignedMatrixSeq[%s]{Out: %s, Cap: %d, Lo: 0, Hi: 0}", g.typeName(ref.Key), ptr, cap)
	case ir.KindFP32:
		return fmt.Sprintf("&sofab.Float32MatrixSeq{Out: %s, Cap: %d}", ptr, cap)
	case ir.KindFP64:
		return fmt.Sprintf("&sofab.Float64MatrixSeq{Out: %s, Cap: %d}", ptr, cap)
	case ir.KindBool:
		return fmt.Sprintf("&sofab.BoolMatrixSeq{Out: %s, Cap: %d}", ptr, cap)
	}
	return "nil"
}

func isUnsignedNativeArray(k ir.Kind) bool {
	return k == ir.KindU8 || k == ir.KindU16 || k == ir.KindU32 || k == ir.KindU64 || k == ir.KindBitfield || k == ir.KindBool
}
func isSignedNativeArray(k ir.Kind) bool {
	return k == ir.KindI8 || k == ir.KindI16 || k == ir.KindI32 || k == ir.KindI64 || k == ir.KindEnum
}

// decodeChunkSize is the scratch buffer Decode<Msg>From drains a reader into.
// §6.6 leaves input storage to the caller, so the corelib sizes nothing from the
// stream and this number is the generated layer's. It bounds nothing about the
// message: a field larger than one chunk simply arrives in several, which is the
// point of a piecewise callback surface. 4 KiB is one page, and eight times the
// 512-byte encode scratch beside it because a read syscall per chunk is what is
// being amortised here.
const decodeChunkSize = 4096

// ---- per-message file ----------------------------------------------------

func (g *gen) messageFile(m *ir.Message) []byte {
	f := newGoFile(g.pkg)
	f.imp(corelibImport)
	f.imp("io")

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

	// Worst-case encoded size. It is what sizes the buffer Encode hands the
	// encoder: the corelib owns no storage and never grows any (CORELIB_PLAN
	// §5.1), so the size has to come from the schema, here.
	ms := g.messageSize(m.Name, m.Fields)
	if ms.Bounded {
		f.line("// %sMaxSize is this message's worst-case encoded size, derived from the", typeName)
		f.line("// schema: no value of it can encode to more.")
		f.line("const %sMaxSize = %d", typeName, ms.Size)
	} else {
		f.line("// %sMaxSizeLimit is the configured ceiling (max_message_size): an", typeName)
		f.line("// unbounded field means this size is imposed, not derived from the schema,")
		f.line("// so it is NOT a size this message cannot exceed.")
		f.line("const (")
		f.line("\t%sMaxSizeLimit = %d", typeName, ms.Size)
		f.line("\t%sMaxSize      = %sMaxSizeLimit", typeName, typeName)
		f.line(")")
	}
	f.blank()

	// public Encode/Decode wrappers
	if ms.Bounded {
		// One exactly-sized buffer, allocated HERE: the corelib is handed storage
		// it neither owns nor may grow, so the allocation belongs to the caller,
		// and generated code is a caller. MaxSize comes from the schema, so it
		// always holds a schema-conformant value -- a field the caller filled past
		// its own declared bound does not fit and is reported as ErrBufferFull,
		// never emitted short (§5.1: partial output is never returned as complete).
		f.line("// Encode serializes the message into a buffer this call allocates and owns.")
		f.line("//")
		f.line("// The buffer is exactly %sMaxSize bytes -- the schema's worst case -- so a", typeName)
		f.line("// conformant value always fits. A value filled past a declared count/maxlen")
		f.line("// does not, and is reported rather than truncated.")
		f.line("func (m *%s) Encode() ([]byte, error) {", typeName)
		f.line("\tbuf := make([]byte, %sMaxSize)", typeName)
		f.line("\te, err := sofab.NewEncoderBuffer(buf, 0)")
		f.line("\tif err != nil {")
		f.line("\t\treturn nil, err")
		f.line("\t}")
		f.line("\tm.Serialize(e)")
		f.line("\tif err := e.Flush(); err != nil {")
		f.line("\t\treturn nil, err")
		f.line("\t}")
		f.line("\treturn e.Bytes(), nil")
		f.line("}")
	} else {
		// An unbounded field has no worst case, so MaxSize here is a configured
		// ceiling rather than a size the message cannot exceed. Sizing the buffer
		// from it would silently refuse a larger message the caller legitimately
		// built, so the shape is a fixed scratch drained into caller-owned storage:
		// the corelib still never allocates, and the ceiling never bounds a value.
		f.line("// Encode serializes the message into storage this call allocates and owns.")
		f.line("//")
		f.line("// A field of this message is unbounded, so there is no worst-case size to")
		f.line("// hand the encoder. It writes into a fixed scratch buffer instead, which is")
		f.line("// appended to the result each time it fills: the message may be any size,")
		f.line("// and %sMaxSize never bounds it.", typeName)
		f.line("func (m *%s) Encode() ([]byte, error) {", typeName)
		f.line("\tvar out []byte")
		f.line("\tvar scratch [512]byte")
		f.line("\te, err := sofab.NewEncoderSink(scratch[:], 0, func(_ *sofab.Encoder, b []byte) error {")
		f.line("\t\tout = append(out, b...)")
		f.line("\t\treturn nil")
		f.line("\t})")
		f.line("\tif err != nil {")
		f.line("\t\treturn nil, err")
		f.line("\t}")
		f.line("\tm.Serialize(e)")
		f.line("\tif err := e.Flush(); err != nil {")
		f.line("\t\treturn nil, err")
		f.line("\t}")
		f.line("\treturn out, nil")
		f.line("}")
	}
	f.blank()
	// Streaming encode, one shape for both arms: the writer IS the drain, so the
	// message never has to exist as one contiguous []byte -- what bounds memory is
	// the scratch buffer, not the message. The scratch is the caller's (this
	// function's) storage; io.Writer.Write may not retain what it is handed, which
	// makes w a copying sink, so it hands no buffer back.
	f.line("// EncodeTo serializes the message straight into w.")
	f.line("//")
	f.line("// The message is never held whole in memory: it is written through a small")
	f.line("// scratch buffer this call owns, drained into w each time it fills, so what")
	f.line("// bounds memory is that buffer rather than the message.")
	f.line("func (m *%s) EncodeTo(w io.Writer) error {", typeName)
	f.line("\tvar scratch [512]byte")
	f.line("\te, err := sofab.NewEncoderSink(scratch[:], 0, func(_ *sofab.Encoder, b []byte) error {")
	f.line("\t\t_, werr := w.Write(b)")
	f.line("\t\treturn werr")
	f.line("\t})")
	f.line("\tif err != nil {")
	f.line("\t\treturn err")
	f.line("\t}")
	f.line("\tm.Serialize(e)")
	f.line("\treturn e.Flush()")
	f.line("}")
	f.blank()
	f.line("// Decode%s parses bytes into a new message (with defaults pre-applied).", typeName)
	f.line("// Decode feeds the buffer to the corelib's decoder in one go, dispatching")
	f.line("// each field to the message's sofab.Visitor implementation.")
	f.line("//")
	f.line("// A payload arrives as a window into data, in as many pieces as it was fed")
	f.line("// in, but the decoded message OWNS its bytes: every destination assembles")
	f.line("// and copies. The message therefore outlives data, and data may be reused")
	f.line("// or mutated the moment this returns.")
	f.line("//")
	f.line("// Use this when the message is already in memory. Decode%sFrom is the", typeName)
	f.line("// streaming twin for a message that is not.")
	f.line("func Decode%s(data []byte) (*%s, error) {", typeName, typeName)
	f.line("\tm := New%s()", typeName)
	f.line("\tif err := sofab.AcceptBytes(data, m%s); err != nil {", g.acceptOpts())
	f.line("\t\treturn nil, err")
	f.line("\t}")
	f.line("\treturn m, nil")
	f.line("}")
	f.blank()
	// Streaming decode -- the twin of EncodeTo above, and what makes this target
	// meet CORELIB_PLAN §5.6 (generator#312). Decode%s needs the whole wire image
	// in one contiguous buffer BY CONSTRUCTION; this one hands the decoder
	// whatever the reader delivered and resumes on the next chunk, so peak memory
	// is the scratch buffer plus the largest single field, not the message.
	//
	// It is a WRAPPER over the same Feed, not a second decode surface (§5.3.1):
	// the same visitor sees the same events in the same order, so a message that
	// is INVALID whole is INVALID streamed, at every chunk boundary.
	//
	// The scratch buffer is the CALLER's by contract (§6.6: the corelib sizes no
	// buffer from a stream), so it is allocated here, once per call.
	f.line("// Decode%sFrom parses a message straight out of r (with defaults pre-applied).", typeName)
	f.line("//")
	f.line("// The wire image is never held whole in memory: r is drained in chunks and")
	f.line("// each field is dispatched as its bytes arrive, so what bounds memory is")
	f.line("// the chunk plus the largest single field, not the message. Decode%s is", typeName)
	f.line("// the in-memory path for bytes you already hold; this is the one to reach")
	f.line("// for over a network connection, a file, or any producer that outruns the")
	f.line("// memory you want to spend.")
	f.line("//")
	f.line("// The verdict is identical either way -- the same visitor sees the same")
	f.line("// events in the same order -- so a message that is INVALID whole is INVALID")
	f.line("// streamed, at every chunk boundary. A reader that ends inside a field is")
	f.line("// INCOMPLETE, which is sofab.ErrIncomplete here: only the caller's framing")
	f.line("// knows whether more could still have come (S5.2.4).")
	f.line("func Decode%sFrom(r io.Reader) (*%s, error) {", typeName, typeName)
	f.line("\tm := New%s()", typeName)
	f.line("\tscratch := make([]byte, %d)", decodeChunkSize)
	f.line("\tout, err := sofab.NewDecoder(m%s).FeedFrom(r, scratch)", g.acceptOpts())
	f.line("\tif err != nil {")
	f.line("\t\treturn nil, err")
	f.line("\t}")
	f.line("\tif out != sofab.Complete {")
	f.line("\t\treturn nil, sofab.ErrIncomplete")
	f.line("\t}")
	f.line("\treturn m, nil")
	f.line("}")
	return f.bytes(g.banner, g.license)
}

// emitDefaults applies the schema defaults New<Msg> starts from. An array field
// gets exactly its declared `default` and nothing else: a declared `count: N` is
// a CAPACITY, not a length (MESSAGE_SPEC §3), so a fresh count:N array is the
// EMPTY array -- not N element defaults -- and a `default` shorter than N stands
// for itself rather than being padded out to N. That is also what the field's
// omit test compares against, and what an absent field decodes back to.
func (g *gen) emitDefaults(f *gofile, fields []*ir.Field) {
	for _, fld := range fields {
		if lit, ok := g.defaultLiteral(fld); ok {
			f.line("\tm.%s = %s", goFieldName(fld.Name), lit)
		}
	}
}
