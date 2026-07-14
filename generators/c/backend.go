// Package c is the embedded-C backend (PLAN §6.2): it emits descriptor-driven
// code against corelib-c-cpp's object.h API — a struct + a static
// sofab_object_descr_field_t[] table + a sofab_object_descr_t per object, plus
// thin encode/decode/init wrappers. No heap; static const descriptors live in
// .rodata.
//
// It traverses the frozen IR (Visitor role) and constructs source through a
// small Builder (cfile), never ad-hoc cross-file string concatenation. It is
// registered with internal/generator from init(); cmd/sofabgen blank-imports it.
package c

import (
	"fmt"
	"sort"
	"strings"

	"github.com/sofa-buffers/generator/internal/generator"
	"github.com/sofa-buffers/generator/internal/ir"
)

func init() { generator.Register(&Backend{}) }

// Backend implements generator.Backend for embedded C.
type Backend struct{}

func (*Backend) Lang() string { return "c" }

// Generate emits one .h + one .c per message (file_per_message). When
// cfg["emit"] == "project" it additionally scaffolds a buildable root project
// (build files + devcontainer wiring + encode/decode harness, §9.1), with the
// message sources placed under generated/.
func (*Backend) Generate(s *ir.Schema, cfg map[string]any) ([]generator.File, error) {
	g := &gen{schema: s, prefix: cfgString(cfg, "symbol_prefix", "message_"), banner: cfgString(cfg, "tool_banner", "sofabgen"), license: generator.LicenseID(cfg)}
	if err := checkBounded(s); err != nil {
		return nil, err
	}
	project := cfgString(cfg, "emit", "sources") == "project"
	srcDir := ""
	if project {
		srcDir = "generated/"
	}
	var files []generator.File
	for _, m := range s.Messages {
		h, c, err := g.message(m)
		if err != nil {
			return nil, err
		}
		base := strings.ToLower(m.Name)
		files = append(files,
			generator.File{Path: srcDir + base + ".h", Content: h},
			generator.File{Path: srcDir + base + ".c", Content: c},
		)
	}
	if project {
		files = append(files, g.projectFiles(s)...)
	}
	return files, nil
}

type gen struct {
	schema  *ir.Schema
	prefix  string
	banner  string
	license string // SPDX id, "" to omit the header line
}

// objectPlan is the fully-resolved emission plan for one C object (the message,
// a struct/union, or a synthetic array-of-string/blob element holder).
type objectPlan struct {
	key      string // unique plan key
	cType    string // C struct type name (with _t)
	descr    string // descriptor symbol
	members  []member
	fields   []fieldEntry
	nested   []string      // child object keys in nested_list order
	defaults []defaultInit // non-zero leaf-field defaults, for the const image
	maxField int64
}

// defaultInit is one designated-initializer entry (".field = expr") in an
// object's const default image. Only leaf fields whose default differs from
// all-zero storage are recorded; when the slice is empty no image is emitted and
// the descriptor keeps the plain SOFAB_OBJECT_DESCR form (zero .rodata cost).
type defaultInit struct {
	ident string // C member name (matches the struct decl)
	expr  string // C initializer RHS
}

type member struct {
	decl  string // e.g. "uint16_t u16;"
	align int    // storage alignment in bytes, for widest-first member ordering
	doc   string // field description (+unit), single-lined; "" => no member comment
}

// memberDoc derives a member's Doxygen text from the field's description and
// unit: the description, with " (unit: <Unit>)" appended when a unit is set (or
// just "(unit: <Unit>)" when there is no description). Multi-line descriptions
// are collapsed to a single line so the text fits a trailing /**< ... */. Empty
// when the field carries neither (the member is emitted byte-identically).
func memberDoc(f *ir.Field) string {
	d := strings.NewReplacer("\r\n", " ", "\r", " ", "\n", " ").Replace(f.Description)
	if f.Unit != "" {
		if d != "" {
			d += " (unit: " + f.Unit + ")"
		} else {
			d = "(unit: " + f.Unit + ")"
		}
	}
	// Neutralise a comment terminator so a description containing "*/" cannot
	// close the trailing /**< ... */ member comment early.
	return strings.ReplaceAll(d, "*/", "* /")
}

type fieldEntry struct {
	macro string // a full SOFAB_OBJECT_FIELD* invocation line
}

// ---- message emission ---------------------------------------------------

func (g *gen) message(m *ir.Message) (hdr, src []byte, err error) {
	// Collect objects in post-order (nested before parents), deduped.
	plans := map[string]*objectPlan{}
	var order []string
	msgKey := "message/" + m.Name
	if err := g.collect(msgKey, g.cType(msgKey, m.Name), m.Fields, plans, &order); err != nil {
		return nil, nil, err
	}

	caps := g.capabilities(m)
	guardName := strings.ToUpper(g.prefix + m.Name + "_H")
	msgType := g.cType(msgKey, m.Name)
	maxField := plans[msgKey].maxField

	h := &cfile{}
	h.banner(g.banner, g.license, strings.ToLower(m.Name)+".h", m.Name)
	h.line("#ifndef %s", guardName)
	h.line("#define %s", guardName)
	h.blank()
	h.line("#include <stdint.h>")
	h.line("#include <stddef.h>")
	h.line(`#include "sofab/sofab.h"`)
	h.line(`#include "sofab/object.h"`)
	h.blank()
	g.emitGuards(h, m, caps, maxField, msgType)
	h.blank()
	if m.Summary != "" {
		h.doc("%s", m.Summary)
	}
	// struct typedefs (post-order so nested types precede their users)
	for _, k := range order {
		g.emitStruct(h, plans[k])
	}
	// max serialized size
	if size, bounded := g.maxSize(m.Fields); bounded {
		h.line("/*! Worst-case serialized size of %s (every field present, all maxlen/count). */", m.Name)
		h.line("#define %s %d", strings.ToUpper(g.prefix+m.Name+"_MAX_SIZE"), size)
	} else {
		h.line("/* %s is unbounded (a variable-length field has no maxlen): use streaming. */", m.Name)
	}
	h.blank()
	// public API prototypes
	g.emitProtos(h, m, msgType)
	h.blank()
	h.line("#endif /* %s */", guardName)

	c := &cfile{}
	c.banner(g.banner, g.license, strings.ToLower(m.Name)+".c", m.Name)
	c.line(`#include "%s.h"`, strings.ToLower(m.Name))
	c.blank()
	c.line("#include <string.h>")
	c.blank()
	for _, k := range order {
		g.emitDescriptor(c, plans[k])
	}
	g.emitFuncs(c, m, msgType, plans[msgKey])

	return h.bytes(), c.bytes(), nil
}

// collect walks an id scope, appending object plans in post-order.
func (g *gen) collect(key, cType string, fields []*ir.Field, plans map[string]*objectPlan, order *[]string) error {
	if _, done := plans[key]; done {
		return nil
	}
	p := &objectPlan{key: key, cType: cType, descr: g.descrSym(key)}
	// First, recurse into children so nested plans are emitted before this one.
	nestedIdx := map[string]int{}
	for _, f := range fields {
		if f.ID > p.maxField {
			p.maxField = f.ID
		}
		switch {
		case f.Kind == ir.KindStruct || f.Kind == ir.KindUnion:
			ck := "named/" + f.Ref.Key
			if err := g.collect(ck, g.cType(ck, f.Ref.Target.Name), f.Ref.Target.Fields, plans, order); err != nil {
				return err
			}
			if _, ok := nestedIdx[ck]; !ok {
				nestedIdx[ck] = len(p.nested)
				p.nested = append(p.nested, ck)
			}
			p.members = append(p.members, member{decl: fmt.Sprintf("%s %s;", plans[ck].cType, cIdent(f.Name)), align: ir.AlignRank(f), doc: memberDoc(f)})
			p.fields = append(p.fields, fieldEntry{macro: fmt.Sprintf(
				"    SOFAB_OBJECT_FIELD_SEQUENCE(%d, %s, %s, SOFAB_OBJECT_FIELDTYPE_SEQUENCE, %d),",
				f.ID, p.cType, cIdent(f.Name), nestedIdx[ck])})
		case f.Kind == ir.KindArray && isHolderElem(f.Elem):
			// string/blob/struct/union/nested-array elements lower to a wrapper
			// sequence: a synthetic holder object with one field per element.
			ck := key + "/" + f.Name + "#elems"
			ep := g.buildHolder(ck, specOfField(f), plans, order)
			if _, ok := nestedIdx[ck]; !ok {
				nestedIdx[ck] = len(p.nested)
				p.nested = append(p.nested, ck)
			}
			p.members = append(p.members, member{decl: fmt.Sprintf("%s %s;", ep.cType, cIdent(f.Name)), align: ir.AlignRank(f), doc: memberDoc(f)})
			p.fields = append(p.fields, fieldEntry{macro: fmt.Sprintf(
				"    SOFAB_OBJECT_FIELD_SEQUENCE(%d, %s, %s, SOFAB_OBJECT_FIELDTYPE_SEQUENCE, %d),",
				f.ID, p.cType, cIdent(f.Name), nestedIdx[ck])})
		default:
			decl, entry, err := g.scalarMember(p.cType, f)
			if err != nil {
				return err
			}
			p.members = append(p.members, member{decl: decl, align: ir.AlignRank(f), doc: memberDoc(f)})
			p.fields = append(p.fields, fieldEntry{macro: entry})
			if expr, ok := g.cDefaultInit(f); ok {
				p.defaults = append(p.defaults, defaultInit{ident: cIdent(f.Name), expr: expr})
			}
		}
	}
	// Order the struct members widest-first to minimise padding. The descriptor
	// (p.fields) and the wire format are unaffected — encode walks the descriptor
	// in id order and decode keys off the field id, both independent of the C
	// member layout (offsets are resolved with offsetof at compile time).
	sort.SliceStable(p.members, func(i, j int) bool { return p.members[i].align > p.members[j].align })
	plans[key] = p
	*order = append(*order, key)
	return nil
}

// arraySpec captures an array's element type — the element kind plus the extra
// IR carried for composite (ElemRef) and nested-array (ElemItems) elements, and
// the element capacity/maxlen. It lets buildHolder and the harness treat an
// outer field and a nested inner array uniformly.
type arraySpec struct {
	elem   ir.Kind
	ref    *ir.TypeRef
	items  *ir.ArrayElem
	count  int64
	maxHas bool
	max    int64
}

func specOfField(f *ir.Field) arraySpec {
	return arraySpec{elem: f.Elem, ref: f.ElemRef, items: f.ElemItems, count: f.Count, maxHas: f.ElemMaxHas, max: f.ElemMax}
}

func specOfItems(a *ir.ArrayElem) arraySpec {
	return arraySpec{elem: a.Elem, ref: a.ElemRef, items: a.ElemItems, count: a.Count, maxHas: a.ElemMaxHas, max: a.ElemMax}
}

// isHolderElem reports whether an array element kind lowers to a wrapper
// sequence (a holder object) rather than a native array wire type. Numeric,
// enum, boolean and bitfield elements stay native; string/blob/struct/union and
// nested arrays become holders.
func isHolderElem(k ir.Kind) bool {
	return k == ir.KindString || k == ir.KindBlob || k == ir.KindStruct || k == ir.KindUnion || k == ir.KindArray
}

// buildHolder builds (and registers, post-order) the synthetic object holding
// the elements of an array whose element lowers to a wrapper sequence: one field
// per element, id = 0-based index (per MESSAGE_SPEC). It handles string/blob
// (a fixlen field each), struct/union (a nested sequence each) and nested arrays
// (an inner array, or an inner holder sequence, each). Recurses for deep nesting.
func (g *gen) buildHolder(key string, spec arraySpec, plans map[string]*objectPlan, order *[]string) *objectPlan {
	p := &objectPlan{key: key, cType: g.cType(key, "elems"), descr: g.descrSym(key)}
	// checkBounded guarantees a count on every array, so the capacity is the
	// schema count directly (no zero-sizing fallback).
	cap := spec.count
	switch spec.elem {
	case ir.KindString, ir.KindBlob:
		// checkBounded guarantees the element maxlen, so the storage is the schema
		// bound directly (no zero-sizing fallback).
		var elemDecl, ftype string
		if spec.elem == ir.KindString {
			// +1 for the NUL the corelib's read_string reserves, so a maxlen-byte
			// wire string element is accepted at its schema bound (#103).
			elemDecl = fmt.Sprintf("char items[%d][%d];", cap, spec.max+1)
			ftype = "SOFAB_OBJECT_FIELDTYPE_STRING"
		} else {
			elemDecl = fmt.Sprintf("uint8_t items[%d][%d];", cap, spec.max)
			ftype = "SOFAB_OBJECT_FIELDTYPE_BLOB"
		}
		p.members = append(p.members, member{decl: elemDecl})
		for i := int64(0); i < cap; i++ {
			p.fields = append(p.fields, fieldEntry{macro: fmt.Sprintf(
				"    SOFAB_OBJECT_FIELD(%d, %s, items[%d], %s),", i, p.cType, i, ftype)})
		}
	case ir.KindStruct, ir.KindUnion:
		// Each element is itself a nested object (struct/union): the element type
		// is emitted as a normal named object, and every holder slot is a sequence
		// referencing that one descriptor (nested_idx 0).
		ek := "named/" + spec.ref.Key
		if err := g.collect(ek, g.cType(ek, spec.ref.Target.Name), spec.ref.Target.Fields, plans, order); err == nil {
			p.nested = append(p.nested, ek)
		}
		p.members = append(p.members, member{decl: fmt.Sprintf("%s items[%d];", plans[ek].cType, cap)})
		for i := int64(0); i < cap; i++ {
			p.fields = append(p.fields, fieldEntry{macro: fmt.Sprintf(
				"    SOFAB_OBJECT_FIELD_SEQUENCE(%d, %s, items[%d], SOFAB_OBJECT_FIELDTYPE_SEQUENCE, 0),", i, p.cType, i)})
		}
	case ir.KindArray:
		inner := specOfItems(spec.items)
		if isHolderElem(inner.elem) {
			// Inner element is itself a holder: each slot is a sequence to it.
			ik := key + "/inner"
			ip := g.buildHolder(ik, inner, plans, order)
			p.nested = append(p.nested, ik)
			p.members = append(p.members, member{decl: fmt.Sprintf("%s items[%d];", ip.cType, cap)})
			for i := int64(0); i < cap; i++ {
				p.fields = append(p.fields, fieldEntry{macro: fmt.Sprintf(
					"    SOFAB_OBJECT_FIELD_SEQUENCE(%d, %s, items[%d], SOFAB_OBJECT_FIELDTYPE_SEQUENCE, 0),", i, p.cType, i)})
			}
		} else {
			// Inner element is a native array: a 2-D C array, each row an array
			// field (id = index). checkBounded guarantees the inner count.
			icap := inner.count
			p.members = append(p.members, member{decl: fmt.Sprintf("%s items[%d][%d];", g.arrayElemCType(inner.elem, inner.ref), cap, icap)})
			for i := int64(0); i < cap; i++ {
				p.fields = append(p.fields, fieldEntry{macro: fmt.Sprintf(
					"    SOFAB_OBJECT_FIELD_ARRAY(%d, %s, items[%d], %s),", i, p.cType, i, arrayFieldType(inner.elem))})
			}
		}
	}
	for i := int64(0); i < cap; i++ {
		if i > p.maxField {
			p.maxField = i
		}
	}
	plans[key] = p
	*order = append(*order, key)
	return p
}

// scalarMember produces the struct member decl + descriptor entry for a
// non-composite field.
func (g *gen) scalarMember(cType string, f *ir.Field) (decl, entry string, err error) {
	mn := cIdent(f.Name)
	switch f.Kind {
	case ir.KindU8, ir.KindU16, ir.KindU32, ir.KindU64:
		decl = fmt.Sprintf("%s %s;", uintC(f.Kind), mn)
		entry = field(f.ID, cType, mn, "UNSIGNED")
	case ir.KindI8, ir.KindI16, ir.KindI32, ir.KindI64:
		decl = fmt.Sprintf("%s %s;", intC(f.Kind), mn)
		entry = field(f.ID, cType, mn, "SIGNED")
	case ir.KindBool:
		decl = fmt.Sprintf("uint8_t %s;", mn)
		entry = field(f.ID, cType, mn, "UNSIGNED")
	case ir.KindFP32:
		decl = fmt.Sprintf("float %s;", mn)
		entry = field(f.ID, cType, mn, "FP32")
	case ir.KindFP64:
		decl = fmt.Sprintf("double %s;", mn)
		entry = field(f.ID, cType, mn, "FP64")
	case ir.KindString:
		// checkBounded guarantees a maxlen on every string, so the storage is the
		// schema bound directly (no zero-usable-capacity fallback). +1 for the NUL:
		// the corelib's read_string reserves one byte for the terminator (istream.c
		// rejects length > capacity-1), so a maxlen-byte wire string needs maxlen+1
		// of storage to be accepted at its schema bound (#103).
		decl = fmt.Sprintf("char %s[%d];", mn, f.Maxlen+1)
		entry = field(f.ID, cType, mn, "STRING")
	case ir.KindBlob:
		decl = fmt.Sprintf("uint8_t %s[%d];", mn, f.Maxlen)
		entry = field(f.ID, cType, mn, "BLOB")
	case ir.KindEnum:
		decl = fmt.Sprintf("%s %s;", enumC(f.Ref.Target), mn)
		entry = field(f.ID, cType, mn, "SIGNED")
	case ir.KindBitfield:
		decl = fmt.Sprintf("%s %s;", bitfieldC(f.Ref.Target), mn)
		entry = field(f.ID, cType, mn, "UNSIGNED")
	case ir.KindArray:
		// Native array element (numeric/enum/boolean/bitfield): enum -> signed,
		// boolean/bitfield -> unsigned, value-converted (not a sequence).
		decl = fmt.Sprintf("%s %s[%d];", g.arrayElemCType(f.Elem, f.ElemRef), mn, f.Count)
		entry = fmt.Sprintf("    SOFAB_OBJECT_FIELD_ARRAY(%d, %s, %s, %s),", f.ID, cType, mn, arrayFieldType(f.Elem))
	default:
		return "", "", fmt.Errorf("field %q: unsupported kind %s for C backend", f.Name, f.Kind)
	}
	return decl, entry, nil
}

// ---- emit pieces --------------------------------------------------------

func (g *gen) emitStruct(h *cfile, p *objectPlan) {
	h.line("typedef struct {")
	for _, m := range p.members {
		if m.doc != "" {
			h.line("    %s  /**< %s */", m.decl, m.doc)
		} else {
			h.line("    %s", m.decl)
		}
	}
	h.line("} %s;", p.cType)
	h.blank()
}

func (g *gen) emitDescriptor(c *cfile, p *objectPlan) {
	c.line("static const sofab_object_descr_field_t %s[] = {", g.fieldsSym(p.key))
	for _, fe := range p.fields {
		c.line("%s", fe.macro)
	}
	c.line("};")

	// nested_list / nested_count arguments (NULL, 0 when the object has no
	// struct/union/sequence children — byte-identical to the historical form).
	nested, nestedCount := "NULL", 0
	if len(p.nested) > 0 {
		c.line("static const sofab_object_descr_t *const %s[] = {", g.nestedSym(p.key))
		for _, nk := range p.nested {
			c.line("    &%s,", g.descrSym(nk))
		}
		c.line("};")
		nested, nestedCount = g.nestedSym(p.key), len(p.nested)
	}

	// A const default image seeds sofab_object_init and is the corelib's
	// omission baseline (fields equal to it are dropped). Emit it only when a
	// leaf field carries a non-zero default; otherwise the plain descriptor
	// compares against zero and costs no .rodata. Designated initializers are
	// order-independent, so the widest-first member reordering is irrelevant.
	if len(p.defaults) > 0 {
		c.line("static const %s %s = {", p.cType, g.defaultsSym(p.key))
		for _, d := range p.defaults {
			c.line("    .%s = %s,", d.ident, d.expr)
		}
		c.line("};")
		c.line("const sofab_object_descr_t %s = SOFAB_OBJECT_DESCR_WITH_DEFAULTS(%s, %d, %s, %d, &%s);",
			p.descr, g.fieldsSym(p.key), len(p.fields), nested, nestedCount, g.defaultsSym(p.key))
	} else {
		c.line("const sofab_object_descr_t %s = SOFAB_OBJECT_DESCR(%s, %d, %s, %d);",
			p.descr, g.fieldsSym(p.key), len(p.fields), nested, nestedCount)
	}
	c.blank()
}

func (g *gen) emitProtos(h *cfile, m *ir.Message, msgType string) {
	pfx := g.prefix + strings.ToLower(m.Name)
	h.doc("Initialize a %s with its schema defaults (non-default fields zeroed).", m.Name)
	h.line("void %s_init(%s *msg);", pfx, msgType)
	h.doc("Encode msg into buf[buflen]; *used receives the byte count. Returns sofab_ret_t.")
	h.line("sofab_ret_t %s_encode(const %s *msg, uint8_t *buf, size_t buflen, size_t *used);", pfx, msgType)
	h.doc("Decode buf[len] into msg (call %s_init first to apply defaults). Returns sofab_ret_t.", pfx)
	h.line("sofab_ret_t %s_decode(%s *msg, const uint8_t *buf, size_t len);", pfx, msgType)
}

func (g *gen) emitFuncs(c *cfile, m *ir.Message, msgType string, root *objectPlan) {
	pfx := g.prefix + strings.ToLower(m.Name)
	depth := g.maxDepth(m.Fields) // decoder stack size

	c.line("void %s_init(%s *msg) {", pfx, msgType)
	c.line("    sofab_object_init(&%s, msg);", root.descr)
	c.line("}")
	c.blank()

	c.line("sofab_ret_t %s_encode(const %s *msg, uint8_t *buf, size_t buflen, size_t *used) {", pfx, msgType)
	c.line("    sofab_ostream_t ctx;")
	c.line("    sofab_ret_t ret;")
	c.line("    sofab_ostream_init(&ctx, buf, buflen, 0, NULL, NULL);")
	c.line("    ret = sofab_object_encode(&ctx, &%s, msg);", root.descr)
	c.line("    if (used) { *used = sofab_ostream_flush(&ctx); }")
	c.line("    return ret;")
	c.line("}")
	c.blank()

	c.line("sofab_ret_t %s_decode(%s *msg, const uint8_t *buf, size_t len) {", pfx, msgType)
	c.line("    sofab_istream_t ctx;")
	c.line("    sofab_object_decoder_t dec[%d];", depth+1)
	c.line("    memset(dec, 0, sizeof(dec));")
	c.line("    dec[0].info = &%s;", root.descr)
	c.line("    dec[0].dst = (uint8_t *)msg;")
	c.line("    dec[0].depth = (uint8_t)(sizeof(dec) / sizeof(dec[0]) - 1);")
	c.line("    sofab_istream_init(&ctx, sofab_object_field_cb, (void *)&dec[0]);")
	c.line("    return sofab_istream_feed(&ctx, buf, len);")
	c.line("}")
}

// emitGuards writes the §5.4 capability guards + the API-version guard + the
// descriptor id-width guard.
func (g *gen) emitGuards(h *cfile, m *ir.Message, caps capset, maxField int64, msgType string) {
	h.line("/* --- API-version guard: this code was generated against C API v1 --- */")
	h.line("#if SOFAB_API_VERSION != 1")
	h.line(`# error "SofaBuffers: generated against C API v1, but the linked corelib reports a different SOFAB_API_VERSION. Regenerate or update the corelib."`)
	h.line("#endif")
	h.blank()
	h.line("/* --- capability guards: a feature-stripped corelib fails loudly --- */")
	type cg struct {
		on    bool
		macro string
		msg   string
	}
	for _, c := range []cg{
		{caps.fixlen, "SOFAB_DISABLE_FIXLEN_SUPPORT", "uses fixed-length fields (string/blob/fp), but the corelib was built with SOFAB_DISABLE_FIXLEN_SUPPORT"},
		{caps.fp64, "SOFAB_DISABLE_FP64_SUPPORT", "uses fp64/double, but the corelib was built with SOFAB_DISABLE_FP64_SUPPORT"},
		{caps.array, "SOFAB_DISABLE_ARRAY_SUPPORT", "uses numeric arrays, but the corelib was built with SOFAB_DISABLE_ARRAY_SUPPORT"},
		{caps.sequence, "SOFAB_DISABLE_SEQUENCE_SUPPORT", "uses nested framing (struct/union/array-of-string), but the corelib was built with SOFAB_DISABLE_SEQUENCE_SUPPORT"},
		{caps.value64, "SOFAB_DISABLE_INT64_SUPPORT", "uses 64-bit integers, but the corelib was built with SOFAB_DISABLE_INT64_SUPPORT"},
	} {
		if !c.on {
			continue
		}
		h.line("#if defined(%s)", c.macro)
		h.line(`# error "SofaBuffers: message %s %s."`, m.Name, c.msg)
		h.line("#endif")
	}
	h.blank()
	h.line("/* --- descriptor width guard: field ids must fit the configured profile --- */")
	h.line("#if %d > SOFAB_OBJECT_DESCR_ID_MAX", maxField)
	h.line(`# error "SofaBuffers: field ids in %s exceed the configured SOFAB_OBJECT_DESCR_PROFILE id width."`, m.Name)
	h.line("#endif")
}

// ---- capability derivation ---------------------------------------------

type capset struct {
	fixlen, fp64, array, sequence, value64 bool
}

func (g *gen) capabilities(m *ir.Message) capset {
	var caps capset
	seen := map[string]bool{}
	var walk func(fields []*ir.Field)
	// arrCaps folds in the capabilities an array's element type needs, recursing
	// through nested arrays and into struct/union element fields.
	var arrCaps func(spec arraySpec)
	arrCaps = func(spec arraySpec) {
		switch spec.elem {
		case ir.KindString, ir.KindBlob:
			caps.sequence = true
			caps.fixlen = true
		case ir.KindStruct, ir.KindUnion:
			caps.sequence = true
			if !seen[spec.ref.Key] {
				seen[spec.ref.Key] = true
				walk(spec.ref.Target.Fields)
			}
		case ir.KindArray:
			caps.sequence = true // holder wrapper sequence
			arrCaps(specOfItems(spec.items))
		case ir.KindFP64:
			caps.array = true
			caps.fixlen = true
			caps.fp64 = true
		case ir.KindFP32:
			caps.array = true
			caps.fixlen = true
		case ir.KindU64, ir.KindI64:
			caps.array = true
			caps.value64 = true
		default: // unsigned/signed numeric, enum, boolean, bitfield
			caps.array = true
		}
	}
	walk = func(fields []*ir.Field) {
		for _, f := range fields {
			switch f.Kind {
			case ir.KindString, ir.KindBlob, ir.KindFP32, ir.KindFP64:
				caps.fixlen = true
				if f.Kind == ir.KindFP64 {
					caps.fp64 = true
				}
			case ir.KindU64, ir.KindI64:
				caps.value64 = true
			case ir.KindStruct, ir.KindUnion:
				caps.sequence = true
				if !seen[f.Ref.Key] {
					seen[f.Ref.Key] = true
					walk(f.Ref.Target.Fields)
				}
			case ir.KindArray:
				arrCaps(specOfField(f))
			}
		}
	}
	walk(m.Fields)
	return caps
}

// ---- naming + small helpers --------------------------------------------

func (g *gen) cType(key, name string) string { return g.prefix + sanitize(key, name) + "_t" }
func (g *gen) descrSym(key string) string    { return "_" + g.prefix + "descr_" + sanitizeKey(key) }
func (g *gen) fieldsSym(key string) string   { return "_" + g.prefix + "fields_" + sanitizeKey(key) }
func (g *gen) nestedSym(key string) string   { return "_" + g.prefix + "nested_" + sanitizeKey(key) }
func (g *gen) defaultsSym(key string) string { return "_" + g.prefix + "defaults_" + sanitizeKey(key) }

func sanitize(key, name string) string {
	// message/<name> and named/<cat>/<Name> -> a readable, unique identifier.
	switch {
	case strings.HasPrefix(key, "message/"):
		return sanitizeKey(strings.TrimPrefix(key, "message/"))
	case strings.HasPrefix(key, "named/"):
		return sanitizeKey(strings.TrimPrefix(key, "named/"))
	default:
		return sanitizeKey(key)
	}
}

func sanitizeKey(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}

func field(id int64, cType, name, ftype string) string {
	return fmt.Sprintf("    SOFAB_OBJECT_FIELD(%d, %s, %s, SOFAB_OBJECT_FIELDTYPE_%s),", id, cType, name, ftype)
}

func uintC(k ir.Kind) string {
	switch k {
	case ir.KindU8:
		return "uint8_t"
	case ir.KindU16:
		return "uint16_t"
	case ir.KindU32:
		return "uint32_t"
	default:
		return "uint64_t"
	}
}

func intC(k ir.Kind) string {
	switch k {
	case ir.KindI8:
		return "int8_t"
	case ir.KindI16:
		return "int16_t"
	case ir.KindI32:
		return "int32_t"
	default:
		return "int64_t"
	}
}

func arrayElemC(k ir.Kind) string {
	switch k {
	case ir.KindU8, ir.KindU16, ir.KindU32, ir.KindU64:
		return uintC(k)
	case ir.KindI8, ir.KindI16, ir.KindI32, ir.KindI64:
		return intC(k)
	case ir.KindFP32:
		return "float"
	case ir.KindFP64:
		return "double"
	}
	return "uint8_t"
}

func arrayFieldType(k ir.Kind) string {
	switch k {
	case ir.KindI8, ir.KindI16, ir.KindI32, ir.KindI64, ir.KindEnum:
		return "SOFAB_OBJECT_FIELDTYPE_ARRAY_SIGNED"
	case ir.KindFP32:
		return "SOFAB_OBJECT_FIELDTYPE_ARRAY_FP32"
	case ir.KindFP64:
		return "SOFAB_OBJECT_FIELDTYPE_ARRAY_FP64"
	default: // unsigned numeric, boolean, bitfield
		return "SOFAB_OBJECT_FIELDTYPE_ARRAY_UNSIGNED"
	}
}

// arrayElemCType is the C storage type of a native array element: enum/bitfield
// take their smallest backing width, boolean is a byte, everything else follows
// arrayElemC (numeric/fp).
func (g *gen) arrayElemCType(elem ir.Kind, ref *ir.TypeRef) string {
	switch elem {
	case ir.KindEnum:
		return enumC(ref.Target)
	case ir.KindBitfield:
		return bitfieldC(ref.Target)
	case ir.KindBool:
		return "uint8_t"
	}
	return arrayElemC(elem)
}

// enumC backs an enum with the smallest SIGNED width covering its range (§6.1).
func enumC(nt *ir.NamedType) string {
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
		return "int8_t"
	case lo >= -32768 && hi <= 32767:
		return "int16_t"
	default:
		return "int32_t"
	}
}

// bitfieldC backs a bitfield with the smallest UNSIGNED width covering its bits.
func bitfieldC(nt *ir.NamedType) string {
	var max int64
	for _, fl := range nt.Flags {
		if fl.Pos > max {
			max = fl.Pos
		}
	}
	switch {
	case max <= 7:
		return "uint8_t"
	case max <= 15:
		return "uint16_t"
	case max <= 31:
		return "uint32_t"
	default:
		return "uint64_t"
	}
}

// maxDepth returns the maximum struct/union nesting under fields (for the
// decoder stack size). Array-of-string holders count as one level too.
func (g *gen) maxDepth(fields []*ir.Field) int {
	best := 0
	for _, f := range fields {
		d := 0
		switch {
		case f.Kind == ir.KindStruct || f.Kind == ir.KindUnion:
			d = 1 + g.maxDepth(f.Ref.Target.Fields)
		case f.Kind == ir.KindArray && isHolderElem(f.Elem):
			d = g.arrayDepth(specOfField(f))
		}
		if d > best {
			best = d
		}
	}
	return best
}

// arrayDepth returns the sequence-nesting depth a holder-lowered array adds to
// the decoder stack: the holder sequence itself plus whatever its elements nest.
// string/blob holders are one level; struct/union elements add a per-element
// sequence plus the element's own depth; nested arrays add their inner array's
// depth. Native array elements contribute nothing beyond the holder.
func (g *gen) arrayDepth(spec arraySpec) int {
	switch spec.elem {
	case ir.KindString, ir.KindBlob:
		return 1
	case ir.KindStruct, ir.KindUnion:
		return 2 + g.maxDepth(spec.ref.Target.Fields)
	case ir.KindArray:
		return 1 + g.arrayDepth(specOfItems(spec.items))
	}
	return 0
}

func cfgString(cfg map[string]any, key, dflt string) string {
	if v, ok := cfg[key].(string); ok && v != "" {
		return v
	}
	return dflt
}

// cKeywords are C reserved words (C99/C11). C has no identifier escape, so a
// field with such a name is mangled (trailing underscore); the struct member and
// its descriptor entry use the mangled name, while the JSON harness keys (emitted
// elsewhere as string literals) keep the original name.
var cKeywords = map[string]bool{
	"auto": true, "break": true, "case": true, "char": true, "const": true,
	"continue": true, "default": true, "do": true, "double": true, "else": true,
	"enum": true, "extern": true, "float": true, "for": true, "goto": true,
	"if": true, "inline": true, "int": true, "long": true, "register": true,
	"restrict": true, "return": true, "short": true, "signed": true, "sizeof": true,
	"static": true, "struct": true, "switch": true, "typedef": true, "union": true,
	"unsigned": true, "void": true, "volatile": true, "while": true, "bool": true,
}

// cIdent mangles a field name that is a C keyword (trailing underscore).
func cIdent(name string) string {
	if cKeywords[name] {
		return name + "_"
	}
	return name
}
