// Package c is the embedded-C backend (PLAN §6.2): it emits descriptor-driven
// code against corelib-c-cpp's object.h API — a struct + a static
// sofab_object_descr_field_t[] table + a sofab_object_descr_t per object, plus
// thin encode/decode/init wrappers. No heap; static const descriptors live in
// .rodata.
//
// It traverses the frozen IR (Visitor role) and constructs source through a
// small Builder (cfile), never ad-hoc cross-file string concatenation. It is
// registered with internal/generator from init(); cmd/sbufgen blank-imports it.
package c

import (
	"fmt"
	"strings"

	"github.com/sofa-buffers/generator/internal/generator"
	"github.com/sofa-buffers/generator/internal/ir"
)

func init() { generator.Register(&Backend{}) }

// Backend implements generator.Backend for embedded C.
type Backend struct{}

func (*Backend) Lang() string { return "c" }

// Generate emits one .h + one .c per message (file_per_message). Shared $defs
// types reachable from a message are emitted alongside it.
func (*Backend) Generate(s *ir.Schema, cfg map[string]any) ([]generator.File, error) {
	g := &gen{schema: s, prefix: cfgString(cfg, "symbol_prefix", "sofab_"), banner: cfgString(cfg, "tool_banner", "sbufgen")}
	var files []generator.File
	for _, m := range s.Messages {
		h, c, err := g.message(m)
		if err != nil {
			return nil, err
		}
		base := strings.ToLower(m.Name)
		files = append(files,
			generator.File{Path: base + ".h", Content: h},
			generator.File{Path: base + ".c", Content: c},
		)
	}
	return files, nil
}

type gen struct {
	schema *ir.Schema
	prefix string
	banner string
}

// objectPlan is the fully-resolved emission plan for one C object (the message,
// a struct/union, or a synthetic array-of-string/blob element holder).
type objectPlan struct {
	key      string // unique plan key
	cType    string // C struct type name (with _t)
	descr    string // descriptor symbol
	members  []member
	fields   []fieldEntry
	nested   []string // child object keys in nested_list order
	maxField int64
}

type member struct {
	decl string // e.g. "uint16_t u16;"
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
	h.banner(g.banner, strings.ToLower(m.Name)+".h", m.Name)
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
	c.banner(g.banner, strings.ToLower(m.Name)+".c", m.Name)
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
			p.members = append(p.members, member{decl: fmt.Sprintf("%s %s;", plans[ck].cType, f.Name)})
			p.fields = append(p.fields, fieldEntry{macro: fmt.Sprintf(
				"    SOFAB_OBJECT_FIELD_SEQUENCE(%d, %s, %s, SOFAB_OBJECT_FIELDTYPE_SEQUENCE, %d),",
				f.ID, p.cType, f.Name, nestedIdx[ck])})
		case f.Kind == ir.KindArray && (f.Elem == ir.KindString || f.Elem == ir.KindBlob):
			ck := key + "/" + f.Name + "#elems"
			ep := g.elemHolder(ck, f)
			plans[ck] = ep
			*order = append(*order, ck)
			if _, ok := nestedIdx[ck]; !ok {
				nestedIdx[ck] = len(p.nested)
				p.nested = append(p.nested, ck)
			}
			p.members = append(p.members, member{decl: fmt.Sprintf("%s %s;", ep.cType, f.Name)})
			p.fields = append(p.fields, fieldEntry{macro: fmt.Sprintf(
				"    SOFAB_OBJECT_FIELD_SEQUENCE(%d, %s, %s, SOFAB_OBJECT_FIELDTYPE_SEQUENCE, %d),",
				f.ID, p.cType, f.Name, nestedIdx[ck])})
		default:
			decl, entry, err := g.scalarMember(p.cType, f)
			if err != nil {
				return err
			}
			p.members = append(p.members, member{decl: decl})
			p.fields = append(p.fields, fieldEntry{macro: entry})
		}
	}
	plans[key] = p
	*order = append(*order, key)
	return nil
}

// elemHolder builds the synthetic object holding the elements of an
// array-of-string/blob (a fixed sequence of string/blob fields, §5).
func (g *gen) elemHolder(key string, f *ir.Field) *objectPlan {
	p := &objectPlan{key: key, cType: g.cType(key, "elems"), descr: g.descrSym(key)}
	maxlen := f.ElemMax
	if !f.ElemMaxHas {
		maxlen = 0
	}
	var elemDecl, ftype string
	if f.Elem == ir.KindString {
		elemDecl = fmt.Sprintf("char items[%d][%d];", f.Count, maxlen)
		ftype = "SOFAB_OBJECT_FIELDTYPE_STRING"
	} else {
		elemDecl = fmt.Sprintf("uint8_t items[%d][%d];", f.Count, maxlen)
		ftype = "SOFAB_OBJECT_FIELDTYPE_BLOB"
	}
	p.members = append(p.members, member{decl: elemDecl})
	for i := int64(0); i < f.Count; i++ {
		p.fields = append(p.fields, fieldEntry{macro: fmt.Sprintf(
			"    SOFAB_OBJECT_FIELD(%d, %s, items[%d], %s),", i, p.cType, i, ftype)})
		if i > p.maxField {
			p.maxField = i
		}
	}
	return p
}

// scalarMember produces the struct member decl + descriptor entry for a
// non-composite field.
func (g *gen) scalarMember(cType string, f *ir.Field) (decl, entry string, err error) {
	switch f.Kind {
	case ir.KindU8, ir.KindU16, ir.KindU32, ir.KindU64:
		decl = fmt.Sprintf("%s %s;", uintC(f.Kind), f.Name)
		entry = field(f.ID, cType, f.Name, "UNSIGNED")
	case ir.KindI8, ir.KindI16, ir.KindI32, ir.KindI64:
		decl = fmt.Sprintf("%s %s;", intC(f.Kind), f.Name)
		entry = field(f.ID, cType, f.Name, "SIGNED")
	case ir.KindBool:
		decl = fmt.Sprintf("uint8_t %s;", f.Name)
		entry = field(f.ID, cType, f.Name, "UNSIGNED")
	case ir.KindFP32:
		decl = fmt.Sprintf("float %s;", f.Name)
		entry = field(f.ID, cType, f.Name, "FP32")
	case ir.KindFP64:
		decl = fmt.Sprintf("double %s;", f.Name)
		entry = field(f.ID, cType, f.Name, "FP64")
	case ir.KindString:
		decl = fmt.Sprintf("char %s[%d];", f.Name, maxlenOr(f, 1))
		entry = field(f.ID, cType, f.Name, "STRING")
	case ir.KindBlob:
		decl = fmt.Sprintf("uint8_t %s[%d];", f.Name, maxlenOr(f, 1))
		entry = field(f.ID, cType, f.Name, "BLOB")
	case ir.KindEnum:
		decl = fmt.Sprintf("%s %s;", enumC(f.Ref.Target), f.Name)
		entry = field(f.ID, cType, f.Name, "SIGNED")
	case ir.KindBitfield:
		decl = fmt.Sprintf("%s %s;", bitfieldC(f.Ref.Target), f.Name)
		entry = field(f.ID, cType, f.Name, "UNSIGNED")
	case ir.KindArray:
		decl = fmt.Sprintf("%s %s[%d];", arrayElemC(f.Elem), f.Name, f.Count)
		entry = fmt.Sprintf("    SOFAB_OBJECT_FIELD_ARRAY(%d, %s, %s, %s),", f.ID, cType, f.Name, arrayFieldType(f.Elem))
	default:
		return "", "", fmt.Errorf("field %q: unsupported kind %s for C backend", f.Name, f.Kind)
	}
	return decl, entry, nil
}

// ---- emit pieces --------------------------------------------------------

func (g *gen) emitStruct(h *cfile, p *objectPlan) {
	h.line("typedef struct {")
	for _, m := range p.members {
		h.line("    %s", m.decl)
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
	if len(p.nested) > 0 {
		c.line("static const sofab_object_descr_t *const %s[] = {", g.nestedSym(p.key))
		for _, nk := range p.nested {
			c.line("    &%s,", g.descrSym(nk))
		}
		c.line("};")
		c.line("const sofab_object_descr_t %s = SOFAB_OBJECT_DESCR(%s, %d, %s, %d);",
			p.descr, g.fieldsSym(p.key), len(p.fields), g.nestedSym(p.key), len(p.nested))
	} else {
		c.line("const sofab_object_descr_t %s = SOFAB_OBJECT_DESCR(%s, %d, NULL, 0);",
			p.descr, g.fieldsSym(p.key), len(p.fields))
	}
	c.blank()
}

func (g *gen) emitProtos(h *cfile, m *ir.Message, msgType string) {
	pfx := g.prefix + strings.ToLower(m.Name)
	h.doc("Initialize a %s with schema defaults (zeroed in this build).", m.Name)
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
				switch f.Elem {
				case ir.KindString, ir.KindBlob:
					caps.sequence = true
					caps.fixlen = true
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
				default:
					caps.array = true
				}
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
	case ir.KindI8, ir.KindI16, ir.KindI32, ir.KindI64:
		return "SOFAB_OBJECT_FIELDTYPE_ARRAY_SIGNED"
	case ir.KindFP32:
		return "SOFAB_OBJECT_FIELDTYPE_ARRAY_FP32"
	case ir.KindFP64:
		return "SOFAB_OBJECT_FIELDTYPE_ARRAY_FP64"
	default:
		return "SOFAB_OBJECT_FIELDTYPE_ARRAY_UNSIGNED"
	}
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

func maxlenOr(f *ir.Field, dflt int64) int64 {
	if f.HasMaxlen {
		return f.Maxlen
	}
	return dflt
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
		case f.Kind == ir.KindArray && (f.Elem == ir.KindString || f.Elem == ir.KindBlob):
			d = 1
		}
		if d > best {
			best = d
		}
	}
	return best
}

func cfgString(cfg map[string]any, key, dflt string) string {
	if v, ok := cfg[key].(string); ok && v != "" {
		return v
	}
	return dflt
}
