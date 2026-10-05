package golang

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/sofa-buffers/generator/internal/generator"
	"github.com/sofa-buffers/generator/internal/ir"
)

func init() { generator.Register(&Backend{}) }

const corelibImport = "github.com/sofa-buffers/corelib-go"

// Backend implements generator.Backend for Go.
type Backend struct{}

func (*Backend) Lang() string { return "go" }

// Generate emits a shared sofab_types.go (all named struct/union/enum/bitfield) plus
// one file per message. When emit==project it also scaffolds a buildable module
// with an encode/decode JSON harness.
func (*Backend) Generate(s *ir.Schema, cfg map[string]any) ([]generator.File, error) {
	g := &gen{
		schema:   s,
		pkg:      cfgString(cfg, "package", "message"),
		banner:   cfgString(cfg, "tool_banner", "sofabgen"),
		license:  generator.LicenseID(cfg),
		limits:   resolveLimits(s, cfg),
		size:     generator.NewSizePolicy(cfg),
		needsDef: map[string]bool{},
		idConsts: map[string]bool{},
	}
	// No name check: every identifier below is built from the channels of
	// ARCHITECTURE §8 ("Naming") -- type identifiers, roles, children, private
	// names, the escape -- so two of them cannot collide for a schema the
	// validator accepted, and no schema is refused for its names.
	project := cfgString(cfg, "emit", "sources") == "project"
	// In a project the package gets its own directory so the harness can import
	// it; in sources mode the files are emitted flat for the caller to place.
	pkgDir := ""
	if project {
		pkgDir = g.pkg + "/"
	}
	// The fixed files start with "sofab_"; a message file (msgFile) has no "_"
	// but the trailing one of a device name, so no message can take -- or
	// overwrite -- one of them.
	var files []generator.File
	if tf := g.typesFile(); tf != nil {
		files = append(files, generator.File{Path: pkgDir + typesFileName, Content: tf})
	}
	if g.hasObject() {
		files = append(files, generator.File{Path: pkgDir + "sofab_visitor.go", Content: g.preludeFile()})
	}
	for _, m := range s.Messages {
		files = append(files, generator.File{Path: pkgDir + msgFile(m), Content: g.messageFile(m)})
	}
	if project {
		files = append(files, g.projectFiles(s, cfg)...)
	}
	if g.sizeErr != nil {
		return nil, g.sizeErr
	}
	if g.fmtErr != nil {
		return nil, g.fmtErr
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
	// fmtErr carries the first file go/format could not parse (see render).
	fmtErr error
	// needsDef memoises needsDefaults per named struct/union key.
	needsDef map[string]bool
	// idConsts records the union paths whose option-id constants are emitted:
	// the variants of a split union share them.
	idConsts map[string]bool
	// owner is the type being emitted; defVars/defDecls queue the package-level
	// copies of long float array defaults it needs (floatDefaultVar).
	owner    string
	defVars  map[string]bool
	defDecls []string
}

// typesFileName is the shared file holding every named type.
const typesFileName = "sofab_types.go"

// render finishes a file through gofile.bytes. The emit path has no error
// channel, so a file go/format rejects is recorded here, named, and returned by
// Generate instead of any output.
func (g *gen) render(f *gofile, name string) []byte {
	out, err := f.bytes(g.banner, g.license)
	if err != nil {
		if g.fmtErr == nil {
			g.fmtErr = fmt.Errorf("go backend: generated %s is not valid Go (a generator bug): %w", name, err)
		}
		return nil
	}
	return out
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

// limitSet is the receiver-side decode-limit configuration (generator#102).
//
// Every cap is always SET — the target carries a finite default that the config
// key only overrides (§9.5, generator#385) — so the three values are always
// available to emit. The `*Has` flags say something narrower: whether the schema
// actually has an unbounded field of that kind, and therefore whether the
// EXPORTED constant is emitted at all. A cap nothing in the schema can reach is
// inert, and an inert constant is dead code in every generated package.
type limitSet struct {
	arrayCount, stringLen, blobLen int64
	arrayHas, stringHas, blobHas   bool
}

func (l limitSet) any() bool { return l.arrayHas || l.stringHas || l.blobHas }

// resolveLimits resolves the max_dyn_* caps over the target's finite defaults,
// and reads off the schema which of them the package actually exports.
//
// The values are emitted AS CONFIGURED. They used to be raised to the largest
// schema bound of their kind, because the caps rode into the corelib as
// sofab.WithMax* options that apply GLOBALLY per decode: corelib-go measured
// every fixlen length and every array count against them with no schema
// exemption, so a cap below a sibling's `maxlen`/`count` would have rejected a
// field the schema declares perfectly legal. Keeping those decodable cost every
// UNBOUNDED field in the message exactly that much tightness. Enforced per
// field, where the schema is known, no raise is needed anywhere: the corelib
// holds no cap at all now (corelib-go#133), and the caps a wrapper array's
// collector takes are exclusive with the schema bounds beside them
// (corelib-go#132).
func resolveLimits(s *ir.Schema, cfg map[string]any) limitSet {
	var all []*ir.Field
	for _, m := range s.Messages {
		all = append(all, m.Fields...)
	}
	b := ir.Bounds(all)
	d := generator.ServerDynLimits.Resolve(cfg)
	return limitSet{
		arrayCount: d.ArrayCount, arrayHas: b.HasDynArray,
		stringLen: d.StringLen, stringHas: b.HasDynString,
		blobLen: d.BlobLen, blobHas: b.HasDynBlob,
	}
}

// capsExpr renders the sofab.Caps value every corelib call that COMPARES a
// receiver cap is handed (§6.2.1): the three max_dyn_* numbers under that
// section's own names, as this package's config chose them.
//
// It is the caller's number, passed for one comparison and not retained. All
// three are ALWAYS filled, including where every field of that kind is
// schema-bounded and the entry is therefore never consulted: corelib-go has no
// fallback for a missing one -- §6.2.1 forbids it to default, to read the
// omission as unlimited, or to reach for the format ceiling -- so an omitted
// entry is ErrArgument, a caller defect, not a looser bound.
//
// The exported constant is preferred so the package keeps one number per kind;
// it exists only where the schema has an unbounded field of that kind, and where
// it does not the entry is inert, so the configured value goes in as a literal
// rather than the package growing a constant nothing reads.
// It is emitted ONCE per package, as _caps, and every collector is handed that
// one value: three numbers repeated at a dozen construction sites read as three
// independent policies, and they are not -- they are this deployment's, stated
// once.
func (g *gen) capsExpr() string { return "_caps" }

// capsDecl renders the package-level _caps value capsExpr names.
func (g *gen) capsDecl() string {
	return fmt.Sprintf("var _caps = sofab.Caps{ArrayCount: %s, StringLen: %s, BlobLen: %s}",
		g.arrayCapExpr(), g.elemMaxExpr(ir.KindString), g.elemMaxExpr(ir.KindBlob))
}

// boundsExpr renders the sofab.Bounds value carrying what the SCHEMA declared
// about one array axis -- `count:` and, for string/blob elements, `maxlen:`.
// A bound the schema omits is left at the zero value, which is how Bounds
// spells "none"; the cap in Caps governs there instead, and the two are
// mutually exclusive (§6.2.1).
func boundsExpr(hasCount bool, count int64, hasMax bool, max int64) string {
	switch {
	case hasCount && hasMax:
		return fmt.Sprintf("sofab.Bounds{Count: %d, ElemLen: %d}", count, max)
	case hasCount:
		return fmt.Sprintf("sofab.Bounds{Count: %d}", count)
	case hasMax:
		return fmt.Sprintf("sofab.Bounds{ElemLen: %d}", max)
	}
	return "sofab.Bounds{}"
}

func (g *gen) arrayCapExpr() string {
	if g.limits.arrayHas {
		return "MaxDynArrayCount"
	}
	return strconv.FormatInt(g.limits.arrayCount, 10)
}

func (g *gen) elemMaxExpr(elem ir.Kind) string {
	if elem == ir.KindBlob {
		if g.limits.blobHas {
			return "MaxDynBlobLen"
		}
		return strconv.FormatInt(g.limits.blobLen, 10)
	}
	if g.limits.stringHas {
		return "MaxDynStringLen"
	}
	return strconv.FormatInt(g.limits.stringLen, 10)
}

// hasCollector reports whether any scope in the schema binds a WRAPPER-sequence
// array -- the shape whose elements never reach the generated visitor and are
// gathered by a corelib collector instead. Every collector constructor takes the
// receiver caps, so this is exactly the condition under which the package needs
// the _caps value; a schema with only native arrays and scalars builds none and
// gets none.
func (g *gen) hasCollector() bool {
	has := func(fields []*ir.Field) bool {
		for _, f := range fields {
			if f.Kind == ir.KindArray && !isNativeArrayElem(f.Elem) {
				return true
			}
		}
		return false
	}
	for _, m := range g.schema.Messages {
		if has(m.Fields) {
			return true
		}
	}
	for _, nt := range g.schema.Named {
		if has(nt.Fields) {
			return true
		}
	}
	return false
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
		f.line("// ONLY the fields the schema left unbounded: a field with its own count/maxlen")
		f.line("// is judged against that alone, and its violation is sofab.ErrInvalidMsg. The")
		f.line("// numbers travel AS CONFIGURED -- nothing raises them to a sibling's schema")
		f.line("// bound, because no cap can reach a bounded field to begin with.")
		f.line("// Exceeding a cap fails the decode with sofab.ErrLimitExceeded, a policy")
		f.line("// category distinct from INVALID: the same bytes decode under a looser cap.")
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
	if g.hasCollector() {
		f.imp(corelibImport)
		f.blank()
		f.line("// _caps carries this deployment's receiver-side limits to every corelib")
		f.line("// collector that compares one. A wrapper array's elements never reach the")
		f.line("// callbacks below -- neither their index nor their length word -- so the")
		f.line("// collector gathering them is where those elements are bounded, and these")
		f.line("// are the numbers it bounds them with.")
		f.line("//")
		f.line("// All three entries are filled even where every field of that kind carries a")
		f.line("// schema bound and the entry is never consulted. The library has no fallback")
		f.line("// for a missing one -- it holds no limit of its own and invents none -- so an")
		f.line("// entry left out is a caller mistake (sofab.ErrArgument), not a looser bound.")
		f.line("%s", g.capsDecl())
	}
	return g.render(f, "sofab_visitor.go")
}

// ---- sofab_types.go : all named types -----------------------------------------

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
		case ir.CatStruct:
			g.emitObject(f, g.typeName(key), nt.Fields)
			g.emitSetDefaults(f, key, g.typeName(key), nt.Fields)
		case ir.CatUnion:
			g.emitUnion(f, nt)
		}
	}
	return g.render(f, typesFileName)
}

func (g *gen) emitEnum(f *gofile, nt *ir.NamedType) {
	tn, base := typeIdent(nt), typeBase(nt)
	f.line("// %s is a generated enum (signed wire varint).", tn)
	f.line("type %s %s", tn, enumGoType(nt))
	f.line("const (")
	for _, c := range nt.Consts {
		doc := ""
		if c.Description != "" {
			doc = " // " + oneline(c.Description)
		}
		f.line("\t%s_%s %s = %d%s", base, exported(c.Name), tn, c.Value, doc)
	}
	f.line(")")
	f.blank()
}

func (g *gen) emitBitfield(f *gofile, nt *ir.NamedType) {
	tn, base := typeIdent(nt), typeBase(nt)
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
		f.line("\t%s_%s %s = 1 << %d%s", base, exported(fl.Name), tn, fl.Pos, doc)
	}
	f.line(")")
	f.blank()
}

// emitObject emits a struct + marshal + a sofab.Visitor decode implementation
// for an id scope. Decode is push/visitor: the struct embeds sofab.VisitorBase
// (no-op defaults) and overrides the callbacks its fields need.
//
// One visitor, two entry points. X__Decode feeds the corelib's decoder a buffer
// the caller already holds; X__DecodeFrom feeds it whatever a reader delivers, so
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
	g.owner = typeName
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
	g.flushDefaultVars(f)

	g.emitVisitorMethods(f, typeName, fields, nil)
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
	return g.fieldIsDefaultExprAt(f, fld, "m."+goFieldName(fld.Name))
}

// fieldIsDefaultExprAt is fieldIsDefaultExpr for the value acc.
func (g *gen) fieldIsDefaultExprAt(f *gofile, fld *ir.Field, acc string) string {
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
	if cmp := g.floatBitsCmp(f, fld, acc, "=="); cmp != "" {
		return cmp
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
			return g.arrayEqualCall(f, fld, acc, def)
		}
		return fmt.Sprintf("len(%s) == 0", acc)
	}
	// Wrapper array: the writer emits a child for every element it holds, because
	// the LAST element is written whatever its value (§2) -- so "no child is
	// written" is exactly "the array is empty", and the two cannot drift apart.
	return fmt.Sprintf("len(%s) == 0", acc)
}

// arrayEqualCall is the "array equals its default" expression. A float array
// is compared by BIT PATTERN (CORELIB_PLAN §4.6): slices.Equal would treat -0.0
// as the default 0 and drop the element, and never equal a NaN. A default of up
// to floatArrayUnrollMax elements is compared by a generated function of its own
// (floatArrayIsDefaultFunc); a longer one by the corelib's sofab.BitsEqual
// against a package-level copy of the default.
func (g *gen) arrayEqualCall(f *gofile, fld *ir.Field, acc, def string) string {
	if fld.Elem == ir.KindFP32 || fld.Elem == ir.KindFP64 {
		if bits := g.floatArrayBits(fld); bits != nil {
			return g.floatArrayIsDefaultFunc(f, fld, bits) + "(" + acc + ")"
		}
		f.imp(corelibImport)
		return fmt.Sprintf("sofab.BitsEqual(%s, %s)", acc, g.floatDefaultVar(fld, def))
	}
	f.imp("slices")
	return fmt.Sprintf("slices.Equal(%s, %s)", acc, def)
}

// floatArrayUnrollMax is the longest float array default compared inline,
// element by element, against bit-pattern constants. Longer ones go through
// sofab.BitsEqual against a package-level copy of the default.
//
// This is the measured section-8 override (maxspeed instructions per call,
// tests/bench): the function's shape is the same for every schema, but
// sofab.BitsEqual(m.A, []float32{...}) cost 1.2x to 2.5x the instructions of the
// slices.Equal it replaced on a 1 to 16 element default (the call is not a leaf,
// the loop is not unrolled, the default literal is rebuilt on the stack per
// call), and the unrolled function costs 0.4x to 1.0x of it.
const floatArrayUnrollMax = 16

// floatArrayBits returns the bit-pattern literals of a float array's default,
// one per element, or nil when the default is too long to compare inline
// (an empty default is a non-nil empty list).
func (g *gen) floatArrayBits(fld *ir.Field) []string {
	vals, _ := fld.Default.([]any)
	if len(vals) > floatArrayUnrollMax {
		return nil
	}
	bits := make([]string, len(vals))
	for i, v := range vals {
		bits[i] = floatBitsLit(fld.Elem, fmt.Sprintf("%v", v))
	}
	return bits
}

// floatArrayIsDefaultFunc names the function that compares a float array with
// its short default, and queues its declaration for the end of the type being
// emitted. The length is tested first and every element after it, each in a
// statement of its own: the same terms joined by && or || in one condition are
// materialised as bools by the compiler, which keeps a bounds check per element
// and cannot leave at the first mismatch. The slice is a parameter, so its header
// is in registers; read through m.A it is reloaded before every element.
func (g *gen) floatArrayIsDefaultFunc(f *gofile, fld *ir.Field, bits []string) string {
	f.imp("math")
	name := "_" + g.owner + "__" + goFieldName(fld.Name) + "IsDefault"
	if g.defVars == nil {
		g.defVars = map[string]bool{}
	}
	if !g.defVars[name] {
		g.defVars[name] = true
		elem := "float32"
		if fld.Elem == ir.KindFP64 {
			elem = "float64"
		}
		var b strings.Builder
		fmt.Fprintf(&b, "// %s reports whether a equals the declared default of %s, bit for bit.\n", name, goFieldName(fld.Name))
		fmt.Fprintf(&b, "func %s(a []%s) bool {\n", name, elem)
		if len(bits) <= 1 {
			// One term: a single return inlines into the caller's branch, where
			// several returns leave a materialised bool behind (+4 instructions).
			b.WriteString("\treturn " + strings.Join(g.floatArrayEqTerms(fld, bits), " && ") + "\n}")
		} else {
			fmt.Fprintf(&b, "\tif len(a) != %d {\n\t\treturn false\n\t}\n", len(bits))
			for i, c := range bits {
				fmt.Fprintf(&b, "\tif %s(a[%d]) != %s {\n\t\treturn false\n\t}\n", floatBitsFunc(fld.Elem), i, c)
			}
			b.WriteString("\treturn true\n}")
		}
		g.defDecls = append(g.defDecls, b.String())
	}
	return name
}

// floatArrayEqTerms are the && terms of the one-return form: the length, then
// the element's bits, each against its constant.
func (g *gen) floatArrayEqTerms(fld *ir.Field, bits []string) []string {
	terms := []string{fmt.Sprintf("len(a) == %d", len(bits))}
	for i, c := range bits {
		terms = append(terms, fmt.Sprintf("%s(a[%d]) == %s", floatBitsFunc(fld.Elem), i, c))
	}
	return terms
}

// floatDefaultVar names the package-level copy of a long float array default
// and queues its declaration for the end of the type being emitted. Building
// the literal inside the guard would copy the whole default onto the stack on
// every call.
func (g *gen) floatDefaultVar(fld *ir.Field, def string) string {
	name := "_" + g.owner + "__" + goFieldName(fld.Name) + "Default"
	if g.defVars == nil {
		g.defVars = map[string]bool{}
	}
	if !g.defVars[name] {
		g.defVars[name] = true
		g.defDecls = append(g.defDecls, fmt.Sprintf("var %s = %s", name, def))
	}
	return name
}

// flushDefaultVars writes the queued float default declarations after a type.
func (g *gen) flushDefaultVars(f *gofile) {
	for _, d := range g.defDecls {
		f.line("%s", d)
		f.blank()
	}
	g.defDecls = nil
}

// ---- per-field marshal/unmarshal ----------------------------------------

func (g *gen) emitMarshalField(f *gofile, fld *ir.Field) {
	g.emitMarshalFieldAt(f, fld, "m."+goFieldName(fld.Name), "\t", false)
}

// emitMarshalFieldAt writes the field whose value is acc, at indent ind.
//
// forced drops the ≠-default guard: the value is written whatever it is, a
// string/blob/compact array as its (possibly empty) payload and a sequence-framed
// kind (struct, union, wrapper array) closed with WriteSequenceEndKeep, so even an
// all-default one reaches the wire as a present, empty frame. A union writes its
// held option that way when that option is not its default_id (MESSAGE_SPEC §4.2):
// the receiver's fresh union holds default_id, so omitting the option would read
// back as a different option, not as this one at its default.
func (g *gen) emitMarshalFieldAt(f *gofile, fld *ir.Field, acc, ind string, forced bool) {
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
		write = fmt.Sprintf("e.WriteBytes(%d, %s)", fld.ID, acc)
		if forced {
			break
		}
		// blob is a leaf: omit when equal to its default. With a schema default,
		// compare against its literal via bytes.Equal (importing "bytes" into
		// whatever file holds this marshal, per-message or the shared sofab_types.go).
		// With no default the default is the empty slice, so the idiomatic
		// len()==0 test is exactly equivalent to bytes.Equal(x, nil) — matching
		// the array/string/scalar omit-checks and leaving generated code free of
		// the bytes dependency in the common case (#113).
		if def, ok := g.defaultLiteral(fld); ok {
			f.imp("bytes")
			f.line("%sif !bytes.Equal(%s, %s) {", ind, acc, def)
		} else {
			f.line("%sif len(%s) != 0 {", ind, acc)
		}
		f.line("%s\t%s", ind, write)
		f.line("%s}", ind)
		return
	case ir.KindStruct, ir.KindUnion:
		// MESSAGE_SPEC S2: the != default test is per field and a sequence is no
		// exception, so the frame is opened LAZILY -- the corelib holds the header
		// back and writes it only once a child field appears. The nested marshal
		// omits every child that equals its default, so "no child was written" IS
		// "the object equals its declared default", evaluated per field and
		// recursively. WriteSequenceEnd then drops the contentless frame: an
		// all-default nested object is omitted, not emitted as an empty wrapper.
		// A forced one closes with WriteSequenceEndKeep instead, so it survives as
		// a present, empty frame.
		f.line("%se.WriteSequenceBeginLazy(%d)", ind, fld.ID)
		f.line("%s%s.Serialize(e)", ind, acc)
		if forced {
			f.line("%se.WriteSequenceEndKeep()", ind)
		} else {
			f.line("%se.WriteSequenceEnd()", ind)
		}
		return
	case ir.KindArray:
		g.emitMarshalArray(f, fld, acc, ind, forced)
		return
	}
	if forced {
		f.line("%s%s", ind, write)
		return
	}
	// Scalar/string/enum/bitfield leaf: always omit when equal to the default;
	// sparse encoding is canonical (MESSAGE_SPEC S2) and the decoder reconstructs
	// the omitted field from its default.
	if cmp := g.floatBitsCmp(f, fld, acc, "!="); cmp != "" {
		f.line("%sif %s {", ind, cmp)
	} else {
		f.line("%sif %s != %s {", ind, acc, g.defaultCompare(fld))
	}
	f.line("%s\t%s", ind, write)
	f.line("%s}", ind)
}

// floatBitsCmp compares an fp32/fp64 scalar with its default by BIT PATTERN
// (CORELIB_PLAN §4.6): an IEEE compare would treat -0.0 as the default 0 and
// drop the field. The default's bits are an integer literal computed here, from
// the same literal the member is initialised with (a -0.0 default is the Go
// constant 0, so it is +0.0 here too). It returns "" for every other kind.
func (g *gen) floatBitsCmp(f *gofile, fld *ir.Field, acc, op string) string {
	if fld.Kind != ir.KindFP32 && fld.Kind != ir.KindFP64 {
		return ""
	}
	lit := "0"
	if l, ok := g.defaultLiteral(fld); ok {
		lit = l
	}
	f.imp("math")
	return fmt.Sprintf("%s(%s) %s %s", floatBitsFunc(fld.Kind), acc, op, floatBitsLit(fld.Kind, lit))
}

// floatBitsFunc is the math function that reads a float's bit pattern.
func floatBitsFunc(k ir.Kind) string {
	if k == ir.KindFP32 {
		return "math.Float32bits"
	}
	return "math.Float64bits"
}

// floatBitsLit is the integer literal of the bit pattern of the float constant
// lit, as the Go compiler reads it: a -0.0 constant is +0.0 in Go, so it is
// +0.0 here too, which is what the member is initialised with.
func floatBitsLit(k ir.Kind, lit string) string {
	if k == ir.KindFP32 {
		v, _ := strconv.ParseFloat(lit, 32)
		if v == 0 {
			v = 0 // -0 parses to -0.0, the member is +0.0
		}
		return fmt.Sprintf("0x%x", math.Float32bits(float32(v)))
	}
	v, _ := strconv.ParseFloat(lit, 64)
	if v == 0 {
		v = 0
	}
	return fmt.Sprintf("0x%x", math.Float64bits(v))
}

// defaultCompare is the RHS to compare a field against for omission: its schema
// default if present, else the Go zero value (matching <Msg>__New's init).
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

func (g *gen) emitMarshalArray(f *gofile, fld *ir.Field, acc, ind string, forced bool) {
	// A native scalar array is a leaf field: omit it when equal to its default
	// (materialized in <Msg>__New), else when empty. A composite/dynamic-element
	// array is a wrapper sequence, opened lazily and closed with the dropping end
	// (MESSAGE_SPEC §2), so an empty one is omitted rather than framed empty.
	//
	// A declared `count: N` takes no part in either test. `count` is a CAPACITY,
	// never a length (§3): it never reaches the wire, so the value is compared
	// against the declared default exactly as written -- neither side padded to N
	// -- and against the empty collection when no default is declared.
	if isNativeArrayElem(fld.Elem) {
		if forced {
			// Count + elements with no guard: an empty one is count 0 (an fp array
			// keeps its fixlen_word), which is how a union option says "held, empty".
			g.marshalArray(f, ind, fmt.Sprintf("%d", fld.ID), acc, fld.Elem, fld.ElemRef, fld.ElemItems, 0, "")
			return
		}
		if def, ok := g.defaultLiteral(fld); ok {
			f.line("%sif !%s {", ind, g.arrayEqualCall(f, fld, acc, def))
		} else {
			f.line("%sif len(%s) != 0 {", ind, acc)
		}
		g.marshalArray(f, ind+"\t", fmt.Sprintf("%d", fld.ID), acc, fld.Elem, fld.ElemRef, fld.ElemItems, 0, "")
		f.line("%s}", ind)
		return
	}
	// The field-level wrapper frame is dropped when no element is written, and
	// absence then reconstructs the field's default. That is correct because a
	// wrapper array's declared `default` is not materialized today (<Msg>__New
	// leaves it the empty collection), so absent and explicitly-empty denote the
	// same value. If that gap is ever closed, this call needs a guard --
	// `if !equal(value, default) { ... WriteSequenceEndKeep() }` -- so that a value
	// differing from a non-empty default still reaches the wire as the empty
	// wrapper, the only encoding of "explicitly empty" (MESSAGE_SPEC §2, §3).
	keep := ""
	if forced {
		// A held wrapper-array union option other than default_id: present even
		// when empty (MESSAGE_SPEC §4.2).
		keep = keepAlways
	}
	g.marshalArray(f, ind, fmt.Sprintf("%d", fld.ID), acc, fld.Elem, fld.ElemRef, fld.ElemItems, 0, keep)
}

// keepAlways is the emitSeqEnd condition that keeps the frame unconditionally.
const keepAlways = "true"

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
//   - keepAlways -- always. A wrapper-array union option that is not the
//     union's default_id: held, it is written even when empty (MESSAGE_SPEC §4.2).
func emitSeqEnd(f *gofile, ind, keepIf string) {
	switch keepIf {
	case "":
		f.line("%se.WriteSequenceEnd()", ind)
		return
	case keepAlways:
		f.line("%se.WriteSequenceEndKeep()", ind)
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

// widthGuard returns the §7.1 reject clause for a store into a destination the
// schema declares with Kind k -- and, for a composite kind, the named type ref
// carries the rest of that declaration -- or "" when nothing reachable can
// breach the bound: the 64-bit kinds, whose range IS the callback parameter's
// own, `bool`, an enum whose constants need the full i64 and a bitfield whose
// highest declared position is 32 or above.
//
// What the schema declares is what binds, and every declaration binds a WIDTH.
// For an integer it is the width it names (MESSAGE_SPEC §7.1, documentation#32):
// the width is a normative validity bound, not a storage hint, and the
// `uint8(v)` conversion that follows IS the mask §7.1 forbids, so the check has
// to precede it. For an `enum` or a `bitfield` it is the width the declaration
// IMPLIES (§1), and declaredWidthCond answers for those.
//
// It serves the scalar callbacks and the array-element ones alike: both name the
// value `v`, and the bound is the same statement about the same declaration. The
// scalar, struct-member, struct-array-member and union-member stores are ONE arm
// per kind serving four positions -- emitVisitorMethods runs once per id scope,
// so the frames differ and the arm does not -- and the array element arm carries
// the same clause, so a value outside the declared width gets one verdict
// wherever it lands (generator#516).
//
// No negative-value term is needed on the unsigned side: Unsigned delivers a
// uint64, so the comparison is already unsigned.
func widthGuard(k ir.Kind, ref *ir.TypeRef) string {
	cond := widthCond(k)
	if cond == "" {
		cond = declaredWidthCond(k, ref)
	}
	if cond == "" {
		return ""
	}
	return fmt.Sprintf("if %s {\n\t\t\treturn sofab.ErrInvalidMsg\n\t\t}\n\t\t", cond)
}

// widthCond is the declared-integer-width half of widthGuard's comparison.
func widthCond(k ir.Kind) string {
	lo, hi, ok := ir.NarrowRange(k)
	if !ok {
		return ""
	}
	if lo < 0 {
		return fmt.Sprintf("v < %d || v > %d", lo, hi)
	}
	return fmt.Sprintf("v > %d", hi)
}

// declaredWidthCond is the reject comparison for an `enum` and a `bitfield`,
// MESSAGE_SPEC §1: each is bound by the WIDTH its declaration implies -- for an
// enum the smallest SIGNED type holding every declared constant, for a bitfield
// the smallest UNSIGNED type holding its highest declared `pos`. It returns ""
// for every other kind (widthCond owns those) and wherever the implied width is
// the 64-bit accumulator the value arrives in, where the clause would be dead
// code and go vet would be right to say so.
//
// A value INSIDE that width is valid even when the schema names no constant for
// it and even when it carries an undeclared bit; only a value outside it is
// malformed input. So an enum {A: 0, B: 1, C: 2, Z: 10} is bounded as an i8 and
// admits 5 while refusing 200, and a bitfield declaring positions 0, 1 and 3 is
// bounded as a u8 and admits 4 -- the undeclared bit 2 -- while refusing 256. An
// undeclared bit is NOT masked away: masking would turn malformed-looking input
// into a DECLARED combination and report it Ok.
//
// This replaces the set/mask bound of generator#530, which implemented the
// closed-type reading MESSAGE_SPEC carried for six days (doc PR #89, `a50db95`)
// and doc PR #95 (`382159e`) withdrew. Both bounds are ordinary intervals now,
// which is §1's own reason for the change: an array's elements are consumed
// inside the corelib loop, so a bound must cross that channel as an interval,
// and a width fits where a set does not -- which is also what lets the matrix
// row collector state its own bound again instead of a generated wrapper.
//
// Storage is still never the bound, even where Go's coincides with it:
// enumGoType and bitfieldGoType pick the smallest integer from the same
// declaration, so the member happens to BE the declared width. The comparison
// runs on the RAW callback parameter all the same, ahead of the narrowing
// conversion, which is the only order in which an over-width value can be seen
// at all -- §1's fourth consequence, stated for the receiver that holds the
// field wider.
func declaredWidthCond(k ir.Kind, ref *ir.TypeRef) string {
	switch k {
	case ir.KindEnum:
		lo, hi, ok := ir.EnumWidthRange(ref)
		if !ok {
			return ""
		}
		return fmt.Sprintf("v < %d || v > %d", lo, hi)
	case ir.KindBitfield:
		// One comparison rather than a mask of the width: Unsigned delivers a
		// uint64, so `v > hi` already refuses everything above the width, with no
		// sign-bit case to fold in the way a signed carrier would need.
		hi, ok := ir.BitfieldWidthMax(ref)
		if !ok {
			return ""
		}
		return fmt.Sprintf("v > %d", hi)
	}
	return ""
}

// emitVisitorMethods emits the sofab.Visitor callbacks a type's fields need.
// Scalars bind straight into a struct member; native arrays arrive widened and
// narrow to the declared element width; nested structs/unions and every
// wrapper-sequence array descend via BeginSequence into a child visitor (a
// nested object, or a collector from arrayCollector). Unused callbacks fall back
// to the embedded sofab.VisitorBase no-ops.
//
// u is non-nil for a union type, whose fields are its options. The arms are the
// same ones -- the same §7.3 gates, bounds and assembly -- and differ only in
// where a value lands: a whole-value store goes through the option's setter,
// which selects it; a compact array selects in ArrayBegin, behind the kind gate
// and the count bound, where its destination is opened; a wrapper-array option
// selects in BeginSequence where its §7.4 replace truncates it; and a struct or
// union option descends through Mut<Opt>(), which selects it at its default only
// when another option is held. None of those re-selects an option that is held,
// so an occurrence is never wiped half-way through (MESSAGE_SPEC §7.4.1).
func (g *gen) emitVisitorMethods(f *gofile, typeName string, fields []*ir.Field, u *unionShape) {
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
		store := func(expr string) string { return acc + " = " + expr }
		sel := ""
		descend := fmt.Sprintf("return &%s, nil", acc)
		if u != nil {
			o := u.byField[fld]
			acc = "m." + o.slot
			store = func(expr string) string { return fmt.Sprintf("m.%s(%s)", o.setter, expr) }
			sel = fmt.Sprintf("m.which = %s\n\t\t", u.tag(o))
			descend = fmt.Sprintf("return m.%s(), nil", o.mut)
		}
		switch fld.Kind {
		case ir.KindU8, ir.KindU16, ir.KindU32, ir.KindU64:
			uns = append(uns, arm(fld.ID, widthGuard(fld.Kind, fld.Ref)+store(goNumType(fld.Kind)+"(v)")))
		case ir.KindBitfield:
			uns = append(uns, arm(fld.ID, widthGuard(fld.Kind, fld.Ref)+store(g.typeName(fld.Ref.Key)+"(v)")))
		case ir.KindBool:
			uns = append(uns, arm(fld.ID, store("v != 0")))
		case ir.KindI8, ir.KindI16, ir.KindI32, ir.KindI64:
			sig = append(sig, arm(fld.ID, widthGuard(fld.Kind, fld.Ref)+store(goNumType(fld.Kind)+"(v)")))
		case ir.KindEnum:
			sig = append(sig, arm(fld.ID, widthGuard(fld.Kind, fld.Ref)+store(g.typeName(fld.Ref.Key)+"(v)")))
		case ir.KindFP32:
			f32 = append(f32, arm(fld.ID, store("v")))
		case ir.KindFP64:
			f64 = append(f64, arm(fld.ID, store("v")))
		case ir.KindString:
			// A string is a byte container in Go (§6.4): the wire bytes pass
			// through verbatim and are validated here, at the destination. A union
			// option is selected here too, at completion -- never in FixlenBegin,
			// which also fires for a subtype this option does not declare.
			str = append(str, arm(fld.ID, takePayload+utf8Guard+store("string(_b)")))
			if body := g.fixlenBeginBody("sofab.FixlenStr", fld); body != "" {
				fixBegin = append(fixBegin, arm(fld.ID, body))
			}
		case ir.KindBlob:
			// _b may alias the caller's fed chunk -- a payload that arrived whole
			// in one piece is handed back as that piece (§6.7) -- so what is kept
			// is a copy. A split payload arrives in storage the accumulator hands
			// over, which needs no copy, but the arm cannot tell the two apart and
			// the copy is what makes the message outlive the input either way.
			blob = append(blob, arm(fld.ID, takePayload+store("append([]byte(nil), _b...)")))
			if body := g.fixlenBeginBody("sofab.FixlenBlob", fld); body != "" {
				fixBegin = append(fixBegin, arm(fld.ID, body))
			}
		case ir.KindStruct, ir.KindUnion:
			seq = append(seq, arm(fld.ID, descend))
		case ir.KindArray:
			// The wire count M IS the array's length (MESSAGE_SPEC §3): the M
			// elements that arrived are the whole value. A declared `count: N` is a
			// capacity and bounds M at the header (arrayBeginBody); it never adds
			// elements, so there is nothing to fill in at [M, N).
			switch {
			case isNativeArrayElem(fld.Elem):
				arrBegin = append(arrBegin, arm(fld.ID, g.arrayBeginBody(fld, acc, sel, g.goArrayElem(fld.Elem, fld.ElemRef, fld.ElemItems))))
				elemArm := arm(fld.ID, widthGuard(fld.Elem, fld.ElemRef)+g.elemAppendStmt(acc, fld.Elem, fld.ElemRef))
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
				seq = append(seq, arm(fld.ID, fmt.Sprintf("%s%s = %s[:0]\n\t\treturn %s, nil", sel, acc, acc, g.arrayCollector("&"+acc, fld.Elem, fld.ElemRef, fld.ElemItems, fieldBounds(fld)))))
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

// fixlenBeginBody is the FixlenBegin arm bounding a string/blob's wire byte
// length, at the length word and before a byte of payload is read
// (MESSAGE_SPEC §7.1, §5.2). "" when the field has neither bound to state.
//
// TWO bounds land here and they are mutually exclusive by rule: a field the
// schema bounds is governed by its own `maxlen` and is sofab.ErrInvalidMsg above
// it; a field the schema leaves unbounded is governed by the receiver's
// configured cap and is sofab.ErrLimitExceeded above it. CORELIB_PLAN §6.2.1
// forbids folding the two -- a cap rejects well-formed bytes that decode under a
// looser cap -- and forbids a cap reaching a field the schema already bounds.
//
// It is the ONLY place either bound is taken. The old whole-value guard beside
// it (`len(v) > N` on the assembled string) was the one that fired for a field
// that arrives and stayed silent for one that does not; this fires for both, and
// the payload callbacks below it therefore carry no bound at all.
//
// The compare sits inside the declared-subtype test. FixlenBegin fires for ANY
// fixlen subtype at a field id -- the corelib resolves what ARRIVED but cannot
// know what was declared, which is schema knowledge only generated code has --
// and a fixlen value whose subtype contradicts the declaration is SKIPPED, not
// measured against this field's maxlen or against a cap (MESSAGE_SPEC §7.3,
// CORELIB_PLAN §6.2.1 "a skipped field is never capped", generator#224).
// Without the gate an fp64 (8 bytes) landing on a `blob` with `maxlen: 4` was
// rejected as INVALID instead of skipped.
func (g *gen) fixlenBeginBody(sub string, fld *ir.Field) string {
	gate := fmt.Sprintf("if sub != %s {\n\t\t\treturn nil\n\t\t}\n\t\t", sub)
	if fld.HasMaxlen {
		return gate + fmt.Sprintf("if total > %d {\n\t\t\treturn sofab.ErrInvalidMsg\n\t\t}", fld.Maxlen)
	}
	live := g.limits.stringHas
	if fld.Kind == ir.KindBlob {
		live = g.limits.blobHas
	}
	if !live {
		return ""
	}
	return gate + fmt.Sprintf("if total > %s {\n\t\t\treturn sofab.ErrLimitExceeded\n\t\t}", g.elemMaxExpr(fld.Kind))
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
// It is sized from the wire count, which is bounded on the line above before the
// make, in one of two mutually exclusive ways: by the schema `count:` where one
// is declared (ErrInvalidMsg, MESSAGE_SPEC §7.1), and by the receiver's
// configured cap where none is (ErrLimitExceeded, CORELIB_PLAN §6.2.1). The
// corelib holds no cap of its own to fall back on (corelib-go#133) -- "the
// numbers and the allocation are not the codec's" -- so this arm is the whole
// bound on a schema-unbounded native array, and it sits at the count header,
// which is the enforcement point §6.2.1 names. Both live inside the §7.3 kind
// gate: a skipped field is never capped. §6.6.1 puts the allocation on this side
// of the callback either way: "the generated layer allocates; the codec does
// not".
//
// sel is "" for a struct member; for a union option it is the statement that
// selects the option, placed after both checks so that a header this option does
// not accept never switches the union (MESSAGE_SPEC §7.4.1).
func (g *gen) arrayBeginBody(fld *ir.Field, acc, sel, elemType string) string {
	body := fmt.Sprintf("if kind != sofab.%s {\n\t\t\treturn nil\n\t\t}\n\t\t", goArrayWireKind(fld.Elem))
	switch {
	case fld.HasCount:
		body += fmt.Sprintf("if count > %d {\n\t\t\treturn sofab.ErrInvalidMsg\n\t\t}\n\t\t", fld.Count)
	case g.limits.arrayHas:
		body += fmt.Sprintf("if count > %s {\n\t\t\treturn sofab.ErrLimitExceeded\n\t\t}\n\t\t", g.arrayCapExpr())
	}
	return body + sel + fmt.Sprintf("%s = make([]%s, 0, count)", acc, elemType)
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
//
// Every collector is handed BOTH bounds of every axis it has, and takes them as
// CONSTRUCTOR ARGUMENTS rather than settable fields (corelib-go#134): the schema
// pair as a sofab.Bounds and the receiver caps beside it as a sofab.Caps. A
// wrapper array's elements never reach the generated visitor -- neither their
// index nor their length word -- so the collector is where this shape's receiver
// caps are compared, and corelib-go keeps the two exclusive per §6.2.1: where
// the schema declares a `count`/`maxlen` the cap beside it is never consulted
// and the violation is ErrInvalidMsg, where it does not the cap governs and the
// violation is ErrLimitExceeded.
//
// Arguments and not fields is the point of the constructors: a struct literal's
// zero value made an omitted cap a silent uncapped decode, which is exactly the
// omission §6.2.1 says an API must not accept. Leaving one out is now a compile
// error here rather than an ErrArgument at decode time.
func (g *gen) arrayCollector(ptr string, elem ir.Kind, ref *ir.TypeRef, items *ir.ArrayElem, bounds string) string {
	switch elem {
	case ir.KindString:
		return fmt.Sprintf("sofab.NewStringSeq(%s, %s, %s)", ptr, bounds, g.capsExpr())
	case ir.KindBlob:
		return fmt.Sprintf("sofab.NewBlobSeq(%s, %s, %s)", ptr, bounds, g.capsExpr())
	case ir.KindStruct, ir.KindUnion:
		t := g.typeName(ref.Key)
		if g.needsDefaults(ref.Key) {
			// An element whose declared defaults are not Go's zero value: every slot
			// the collector creates starts at them, so an interior element the
			// encoder omitted for being at its default reads back as that default
			// (MESSAGE_SPEC §5.1, generator#609).
			return fmt.Sprintf("sofab.NewMessageSeqInit[%s, *%s](%s, %s, %s, (*%s).setDefaults)", t, t, ptr, bounds, g.capsExpr(), t)
		}
		return fmt.Sprintf("sofab.NewMessageSeq[%s, *%s](%s, %s, %s)", t, t, ptr, bounds, g.capsExpr())
	case ir.KindArray:
		if isNativeArrayElem(items.Elem) {
			return g.matrixCollector(ptr, items, bounds)
		}
		// Array of wrapper-sequence arrays: each element is itself a sequence
		// collected into an inner slice by a recursively-built collector. The
		// inner collector carries the inner array's own count bound.
		inner := g.goArrayElem(items.Elem, items.ElemRef, items.ElemItems)
		mk := g.arrayCollector("p", items.Elem, items.ElemRef, items.ElemItems, elemBounds(items))
		return fmt.Sprintf("sofab.NewNestedSeq[%s](%s, %s, %s, func(p *[]%s) sofab.Visitor { return %s })", inner, ptr, bounds, g.capsExpr(), inner, mk)
	}
	return "nil"
}

// fieldBounds / elemBounds render the sofab.Bounds of an array declared as a
// field and of one declared as another array's element type. `count:` is a
// CAPACITY: the collector uses it only to reject an out-of-range element id,
// never to size the result.
func fieldBounds(f *ir.Field) string {
	return boundsExpr(f.HasCount, f.Count, f.ElemMaxHas, f.ElemMax)
}

func elemBounds(items *ir.ArrayElem) string {
	return boundsExpr(items.HasCount, items.Count, items.ElemMaxHas, items.ElemMax)
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

// matrixCollector builds the row collector for an array whose elements are native
// arrays ([][]elem): rows arrive via the widened *Array callbacks, keyed by the
// row's element id. bounds is the OUTER array's sofab.Bounds, which bounds that
// id.
//
// A matrix has TWO axes, so it takes TWO sofab.Bounds. The outer one bounds the
// ROW ID, as on every collector above. The second bounds a row's OWN element
// count, which the row announces as a real count header because a row IS a
// native array -- and which nothing bounded before: `items` (the inner array's
// `count:`) was dropped on the floor here, and the codec's cap that used to
// stand in for it is gone. One sofab.Caps serves both axes: §6.2.1 states one
// max_dyn_array_count and both axes are arrays.
func (g *gen) matrixCollector(ptr string, items *ir.ArrayElem, bounds string) string {
	elem, ref := items.Elem, items.ElemRef
	// The row element's declared width travels with the collector, so the scan
	// runs before sofab.Narrow* masks anything (generator#330). NarrowRange
	// answers false for u64/i64, whose range is the callback parameter's own, so
	// the zero bound switches the scan off rather than emitting one that can
	// never fire.
	lo, hi, _ := ir.NarrowRange(elem)
	// The row axis carries only its `count:`; a row's ELEMENTS are numbers, so
	// the ElemLen half of Bounds is not a thing a matrix has.
	rows := fmt.Sprintf("%s, %s, %s", bounds, boundsExpr(items.HasCount, items.Count, false, 0), g.capsExpr())
	switch elem {
	case ir.KindU8, ir.KindU16, ir.KindU32, ir.KindU64:
		return fmt.Sprintf("sofab.NewUnsignedMatrixSeq[%s](%s, %s, %d)", goNumType(elem), ptr, rows, uint64(hi))
	case ir.KindI8, ir.KindI16, ir.KindI32, ir.KindI64:
		return fmt.Sprintf("sofab.NewSignedMatrixSeq[%s](%s, %s, %d, %d)", goNumType(elem), ptr, rows, lo, hi)
	case ir.KindBitfield:
		// A matrix row's elements never reach the generated visitor -- the
		// collector gathers them and places the finished row -- so the row element
		// bound has to be one the collector can carry, and the collector's is an
		// INTERVAL armed by a sentinel. The width §1 implies IS an interval, so it
		// travels there like every other element width, and the wrapper type
		// generator#530 needed to state a mask is gone with the mask.
		//
		// BitfieldWidthMax and EnumWidthRange answer 0 where the implied width is
		// the accumulator's own, which is exactly the value that switches the
		// collector's scan off -- the same sentinel u64/i64 use above.
		bhi, _ := ir.BitfieldWidthMax(ref)
		return fmt.Sprintf("sofab.NewUnsignedMatrixSeq[%s](%s, %s, %d)", g.typeName(ref.Key), ptr, rows, bhi)
	case ir.KindEnum:
		elo, ehi, _ := ir.EnumWidthRange(ref)
		return fmt.Sprintf("sofab.NewSignedMatrixSeq[%s](%s, %s, %d, %d)", g.typeName(ref.Key), ptr, rows, elo, ehi)
	case ir.KindFP32:
		return fmt.Sprintf("sofab.NewFloat32MatrixSeq(%s, %s)", ptr, rows)
	case ir.KindFP64:
		return fmt.Sprintf("sofab.NewFloat64MatrixSeq(%s, %s)", ptr, rows)
	case ir.KindBool:
		return fmt.Sprintf("sofab.NewBoolMatrixSeq(%s, %s)", ptr, rows)
	}
	return "nil"
}

func isUnsignedNativeArray(k ir.Kind) bool {
	return k == ir.KindU8 || k == ir.KindU16 || k == ir.KindU32 || k == ir.KindU64 || k == ir.KindBitfield || k == ir.KindBool
}
func isSignedNativeArray(k ir.Kind) bool {
	return k == ir.KindI8 || k == ir.KindI16 || k == ir.KindI32 || k == ir.KindI64 || k == ir.KindEnum
}

// decodeChunkSize is the scratch buffer <Msg>__DecodeFrom drains a reader into.
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

	// The message's companions are roles on its unescaped identifier (M__New,
	// M__Decode, M__MaxSize, ...); the private encode options are _M__EncOpts.
	typeName, base := msgIdent(m), msgBase(m)
	newFn, decFn, decFromFn := base+"__New", base+"__Decode", base+"__DecodeFrom"
	maxSize, maxSizeLimit, maxDepth := base+"__MaxSize", base+"__MaxSizeLimit", base+"__MaxDepth"
	if m.Summary != "" {
		f.line("// %s - %s", typeName, oneline(m.Summary))
	}
	g.emitObject(f, typeName, m.Fields)

	// constructor with schema defaults
	f.line("// %s returns a %s with schema defaults applied.", newFn, typeName)
	f.line("func %s() *%s {", newFn, typeName)
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
		f.line("// %s is this message's worst-case encoded size, derived from the", maxSize)
		f.line("// schema: no value of it can encode to more.")
		f.line("const %s = %d", maxSize, ms.Size)
	} else {
		f.line("// %s is the configured ceiling (max_message_size): an", maxSizeLimit)
		f.line("// unbounded field means this size is imposed, not derived from the schema,")
		f.line("// so it is NOT a size this message cannot exceed.")
		f.line("const (")
		f.line("\t%s = %d", maxSizeLimit, ms.Size)
		f.line("\t%s = %s", maxSize, maxSizeLimit)
		f.line(")")
	}
	f.blank()

	// Encode-side nesting bound (see ir.SeqDepth). It is what lets every Encoder
	// this file constructs size its lazy-sequence id stack to the schema instead
	// of to MaxDepth: up to a small inline capacity that stack then lives inside
	// the Encoder, and a one-shot encode pays no separate allocation for it.
	encOpts := ""
	if depth, ok := ir.SeqDepth(m.Fields, map[string]bool{}); ok && depth <= wireMaxDepth {
		optsVar := "_" + base + "__EncOpts"
		encOpts = ", " + optsVar + "..."
		f.line("// %s is the deepest sequence nesting encoding this message opens,", maxDepth)
		f.line("// derived from the schema: no value of it nests deeper.")
		f.line("const %s = %d", maxDepth, depth)
		f.blank()
		// WithMaxDepth(0) means "no bound" (MaxDepth), so a message that opens no
		// sequence at all still passes 1: the bound is never reached either way.
		arg := maxDepth
		if depth == 0 {
			arg = "1"
			f.line("// %s bounds this message's encoders to one level: %s", optsVar, maxDepth)
			f.line("// is 0, and WithMaxDepth(0) would mean no bound. It is package-level so")
			f.line("// passing it allocates nothing per call.")
		} else {
			f.line("// %s bounds this message's encoders to %s. It is", optsVar, maxDepth)
			f.line("// package-level so passing it allocates nothing per call.")
		}
		f.line("var %s = []sofab.Option{sofab.WithMaxDepth(%s)}", optsVar, arg)
		f.blank()
	}

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
		f.line("// The buffer is exactly %s bytes -- the schema's worst case -- so a", maxSize)
		f.line("// conformant value always fits. A value filled past a declared count/maxlen")
		f.line("// does not, and is reported rather than truncated.")
		f.line("func (m *%s) Encode() ([]byte, error) {", typeName)
		f.line("\tbuf := make([]byte, %s)", maxSize)
		f.line("\te, err := sofab.NewEncoderBuffer(buf, 0%s)", encOpts)
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
		f.line("// and %s never bounds it.", maxSize)
		f.line("func (m *%s) Encode() ([]byte, error) {", typeName)
		f.line("\tvar out []byte")
		f.line("\tvar scratch [512]byte")
		f.line("\te, err := sofab.NewEncoderSink(scratch[:], 0, func(_ *sofab.Encoder, b []byte) error {")
		f.line("\t\tout = append(out, b...)")
		f.line("\t\treturn nil")
		f.line("\t}%s)", encOpts)
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
	f.line("\t}%s)", encOpts)
	f.line("\tif err != nil {")
	f.line("\t\treturn err")
	f.line("\t}")
	f.line("\tm.Serialize(e)")
	f.line("\treturn e.Flush()")
	f.line("}")
	f.blank()
	f.line("// %s parses bytes into a new message (with defaults pre-applied).", decFn)
	f.line("// Decode feeds the buffer to the corelib's decoder in one go, dispatching")
	f.line("// each field to the message's sofab.Visitor implementation.")
	f.line("//")
	f.line("// A payload arrives as a window into data, in as many pieces as it was fed")
	f.line("// in, but the decoded message OWNS its bytes: every destination assembles")
	f.line("// and copies. The message therefore outlives data, and data may be reused")
	f.line("// or mutated the moment this returns.")
	f.line("//")
	f.line("// Use this when the message is already in memory. %s is the", decFromFn)
	f.line("// streaming twin for a message that is not.")
	f.line("func %s(data []byte) (*%s, error) {", decFn, typeName)
	f.line("\tm := %s()", newFn)
	f.line("\tif err := sofab.AcceptBytes(data, m); err != nil {")
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
	f.line("// %s parses a message straight out of r (with defaults pre-applied).", decFromFn)
	f.line("//")
	f.line("// The wire image is never held whole in memory: r is drained in chunks and")
	f.line("// each field is dispatched as its bytes arrive, so what bounds memory is")
	f.line("// the chunk plus the largest single field, not the message. %s is", decFn)
	f.line("// the in-memory path for bytes you already hold; this is the one to reach")
	f.line("// for over a network connection, a file, or any producer that outruns the")
	f.line("// memory you want to spend.")
	f.line("//")
	f.line("// The verdict is identical either way -- the same visitor sees the same")
	f.line("// events in the same order -- so a message that is INVALID whole is INVALID")
	f.line("// streamed, at every chunk boundary. A reader that ends inside a field is")
	f.line("// INCOMPLETE, which is sofab.ErrIncomplete here: only the caller's framing")
	f.line("// knows whether more could still have come (S5.2.4).")
	f.line("func %s(r io.Reader) (*%s, error) {", decFromFn, typeName)
	f.line("\tm := %s()", newFn)
	f.line("\tscratch := make([]byte, %d)", decodeChunkSize)
	f.line("\tout, err := sofab.NewDecoder(m).FeedFrom(r, scratch)")
	f.line("\tif err != nil {")
	f.line("\t\treturn nil, err")
	f.line("\t}")
	f.line("\tif out != sofab.Complete {")
	f.line("\t\treturn nil, sofab.ErrIncomplete")
	f.line("\t}")
	f.line("\treturn m, nil")
	f.line("}")
	return g.render(f, msgFile(m))
}

// wireMaxDepth is the format's MAX_DEPTH (corelib-go sofab.MaxDepth, §4.9): the
// most sequences an Encoder may hold open. A schema bound above it is not
// passed -- the corelib would ignore it, and refuse the value anyway.
const wireMaxDepth = 255

// emitDefaults applies the schema defaults <Msg>__New starts from. An array field
// gets exactly its declared `default` and nothing else: a declared `count: N` is
// a CAPACITY, not a length (MESSAGE_SPEC §3), so a fresh count:N array is the
// EMPTY array -- not N element defaults -- and a `default` shorter than N stands
// for itself rather than being padded out to N. That is also what the field's
// omit test compares against, and what an absent field decodes back to.
//
// A struct/union field is seeded through its type's setDefaults, which reaches
// every nested level (generator#609): the field's own Serialize compares its
// members against their schema defaults, so a member left at Go's zero value
// would be written from a fresh message and decode back as 0 when absent.
func (g *gen) emitDefaults(f *gofile, fields []*ir.Field) {
	for _, fld := range fields {
		if lit, ok := g.defaultLiteral(fld); ok {
			f.line("\tm.%s = %s", goFieldName(fld.Name), lit)
			continue
		}
		if (fld.Kind == ir.KindStruct || fld.Kind == ir.KindUnion) && g.needsDefaults(fld.Ref.Key) {
			f.line("\tm.%s.setDefaults()", goFieldName(fld.Name))
		}
	}
}

// emitSetDefaults emits the seeding a struct/union type needs when any of its
// members, at any depth, declares a default Go's zero value does not already
// hold. A type that declares none gets no method: its zero value IS its default.
// <Msg>__New calls it for a struct/union field, and a struct/union array hands it
// to sofab.NewMessageSeqInit so every element the collector creates -- the gap
// an omitted default element leaves included (MESSAGE_SPEC §5.1) -- starts at it.
func (g *gen) emitSetDefaults(f *gofile, key, typeName string, fields []*ir.Field) {
	if !g.needsDefaults(key) {
		return
	}
	f.line("// setDefaults seeds the schema defaults %s's members declare, at every", typeName)
	f.line("// nested level, in place.")
	f.line("func (m *%s) setDefaults() {", typeName)
	g.emitDefaults(f, fields)
	f.line("}")
	f.blank()
}

// needsDefaults reports whether the struct/union type key declares, at any
// nested level, a member default that differs from Go's zero value -- i.e.
// whether a zero-valued instance is NOT already at its schema defaults.
func (g *gen) needsDefaults(key string) bool {
	if v, ok := g.needsDef[key]; ok {
		return v
	}
	// Provisional answer while this type's members are walked, so a type that
	// reaches itself terminates. The walk never enters an array (its default is
	// the empty array, which needs no seeding), which is the only way a Go
	// struct can contain itself.
	g.needsDef[key] = false
	nt := g.schema.Named[key]
	need := false
	if nt != nil {
		fields := nt.Fields
		if nt.Category == ir.CatUnion {
			// A union's zero value holds its default_id option (the tag is stored
			// relative to it), so only that option's own default can need seeding.
			fields = nil
			if d := nt.DefaultOption(); d != nil {
				fields = []*ir.Field{d}
			}
		}
		for _, fld := range fields {
			if lit, ok := g.defaultLiteral(fld); ok && !isZeroLiteral(lit) {
				need = true
				break
			}
			if (fld.Kind == ir.KindStruct || fld.Kind == ir.KindUnion) && g.needsDefaults(fld.Ref.Key) {
				need = true
				break
			}
		}
	}
	g.needsDef[key] = need
	return need
}

// isZeroLiteral reports whether a defaultLiteral rendering is Go's zero value
// for its type -- an explicitly declared `default: 0`, `false`, `""`, enum
// constant 0 or empty array -- so declaring it asks for no seeding at all. A
// negative zero ("-0") is deliberately not one: its bits differ from +0.
func isZeroLiteral(lit string) bool {
	switch lit {
	case "0", "false", `""`:
		return true
	}
	return strings.HasSuffix(lit, "(0)") || strings.HasSuffix(lit, "{}")
}
