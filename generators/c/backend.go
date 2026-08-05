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
	g := &gen{schema: s, prefix: cfgString(cfg, "symbol_prefix", "message_"), banner: cfgString(cfg, "tool_banner", "sofabgen"), license: generator.LicenseID(cfg), size: generator.NewSizePolicy(cfg)}
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
	license string               // SPDX id, "" to omit the header line
	size    generator.SizePolicy // max_message_size ceiling for unbounded messages
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
	// blobLenInits materialize a sized blob's declared default used-length in the
	// generated _init: sofab_object_init copies the blob buffer from the default
	// image but not the companion _len member (it is not a descriptor field), so
	// _init sets it explicitly, otherwise the declared default decodes as empty.
	blobLenInits []blobLenInit
	maxField     int64
	// hasDeprecated is set when any field lowers to a deprecated struct member.
	// The generated .c references members by name via sizeof(((T*)0)->field) in
	// the descriptor table (and by designated initializer in the defaults image),
	// both of which warn under -Wdeprecated-declarations; emitDescriptor wraps its
	// output in a diagnostic push/pop when this is set.
	hasDeprecated bool
	// fixedSeq marks a synthetic fixed-count sequence holder (buildHolder): its
	// fields are the element slots 0..field_count-1 of a bounded string/blob/
	// struct/union/nested array. emitDescriptor emits SOFAB_OBJECT_DESCR_SEQ_SIZED,
	// which does two things: the corelib rejects an over-index element id (>= N) as
	// INVALID instead of skipping it like an unknown message field (MESSAGE_SPEC
	// §7/§7.1, generator#149 / corelib-c-cpp#94), and the holder's leading `len`
	// member gives it every length 0..N as MESSAGE_SPEC §5.1 requires (highest
	// present id + 1) instead of only 0 and N. A message / struct / union object
	// leaves this false.
	fixedSeq bool
}

// defaultInit is one designated-initializer entry (".field = expr") in an
// object's const default image. Only leaf fields whose default differs from
// all-zero storage are recorded; when the slice is empty no image is emitted and
// the descriptor keeps the plain SOFAB_OBJECT_DESCR form (zero .rodata cost).
type defaultInit struct {
	ident string // C member name (matches the struct decl)
	expr  string // C initializer RHS
}

// blobLenInit records a sized blob whose schema default is non-empty: _init sets
// member+"_len" to length so the declared default materializes on init/decode.
type blobLenInit struct {
	member string // blob member name (its length companion is member+"_len")
	length int64  // decoded default byte length (0..maxlen)
}

type member struct {
	decl       string // e.g. "uint16_t u16;"
	align      int    // storage alignment in bytes, for widest-first member ordering
	doc        string // field description (+unit), single-lined; "" => no member comment
	note       string // schema-bound note (generator#308); "" => the field has no bound
	deprecated bool   // field carries deprecated:true — emit the native attribute + doc note
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

// memberNote is the field's schema-bound note, wired to how the C target
// actually stores the field (generator#308). C is the target where this matters
// most: a bounded array and a bounded blob keep their LENGTH in a separate
// member, so a caller that fills the storage and forgets the length encodes an
// empty field — no crash, no warning, and valid bytes on the wire.
func memberNote(f *ir.Field) string {
	name := cIdent(f.Name)
	switch {
	case f.Kind == ir.KindString:
		// char[maxlen+1], NUL-terminated: the capacity IS the type and there is
		// no companion to forget.
		return generator.BoundNote(f, generator.StorageFixed)
	case f.Kind == ir.KindBlob:
		return generator.BoundDoc{Storage: generator.StorageCompanion, LenMember: name + "_len"}.Note(f)
	case f.Kind == ir.KindArray && isHolderElem(f.Elem):
		// A wrapper array lowers to a holder struct, so its length lives inside
		// the member rather than beside it.
		return generator.BoundDoc{Storage: generator.StorageCompanion, LenMember: name + ".len"}.Note(f)
	case f.Kind == ir.KindArray:
		return generator.BoundDoc{Storage: generator.StorageCompanion, LenMember: name + "_len"}.Note(f)
	}
	return ""
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
	// max serialized size (ir.MaxWireSize — one walk shared by every backend)
	ms, err := g.size.Resolve(m.Name, m.Fields)
	if err != nil {
		return nil, nil, err
	}
	if ms.Bounded {
		h.line("/*! Worst-case serialized size of %s (every field present, all maxlen/count). */", m.Name)
		h.line("#define %s %d", strings.ToUpper(g.prefix+m.Name+"_MAX_SIZE"), ms.Size)
	} else {
		// Unreachable for C today: the fixed-storage target already rejects an
		// unbounded field before this point. Kept so the two constants stay
		// distinguishable if that ever changes.
		h.line("/*! Configured ceiling: %s has an unbounded field, so its size is imposed, not derived. */", m.Name)
		h.line("#define %s %d", strings.ToUpper(g.prefix+m.Name+"_MAX_SIZE_LIMIT"), ms.Size)
		h.line("#define %s %s", strings.ToUpper(g.prefix+m.Name+"_MAX_SIZE"),
			strings.ToUpper(g.prefix+m.Name+"_MAX_SIZE_LIMIT"))
	}
	h.blank()
	// public API prototypes
	g.emitProtos(h, m, msgType, plans[msgKey])
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
		if f.Deprecated {
			p.hasDeprecated = true
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
			p.members = append(p.members, member{decl: fmt.Sprintf("%s %s;", plans[ck].cType, cIdent(f.Name)), align: ir.AlignRank(f), doc: memberDoc(f), note: memberNote(f), deprecated: f.Deprecated})
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
			p.members = append(p.members, member{decl: fmt.Sprintf("%s %s;", ep.cType, cIdent(f.Name)), align: ir.AlignRank(f), doc: memberDoc(f), note: memberNote(f), deprecated: f.Deprecated})
			p.fields = append(p.fields, fieldEntry{macro: fmt.Sprintf(
				"    SOFAB_OBJECT_FIELD_SEQUENCE(%d, %s, %s, SOFAB_OBJECT_FIELDTYPE_SEQUENCE, %d),",
				f.ID, p.cType, cIdent(f.Name), nestedIdx[ck])})
		default:
			decl, entry, err := g.scalarMember(p.cType, f)
			if err != nil {
				return err
			}
			p.members = append(p.members, member{decl: decl, align: ir.AlignRank(f), doc: memberDoc(f), note: memberNote(f), deprecated: f.Deprecated})
			p.fields = append(p.fields, fieldEntry{macro: entry})
			// A compact array's declared default is an array of its OWN length, not
			// one padded out to the capacity (§2/§3/§6): `default: [1,2,3]` on a
			// `count: 5` field is the three-element [1,2,3]. The length lives in the
			// companion member, which sofab_object_init seeds from the default image
			// (at offset - width) exactly as it seeds the buffer — so the image has to
			// carry it, including when every declared element is zero and the value
			// image itself is elided ([0,0,0] is the LENGTH-3 array of zeros, not the
			// empty array).
			if n, ok := arrayDefaultLen(f); ok {
				p.defaults = append(p.defaults, defaultInit{ident: cIdent(f.Name) + "_len", expr: fmt.Sprintf("%d", n)})
			}
			if expr, ok := g.cDefaultInit(f); ok {
				p.defaults = append(p.defaults, defaultInit{ident: cIdent(f.Name), expr: expr})
				if n, ok := blobDefaultRawLen(f); ok {
					p.blobLenInits = append(p.blobLenInits, blobLenInit{member: cIdent(f.Name), length: n})
				}
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
	// A holder's fields are the fixed element slots 0..N-1, so an over-index element
	// id (>= N) is INVALID, not an unknown-field skip: mark it a fixed-seq holder.
	p := &objectPlan{key: key, cType: g.cType(key, "elems"), descr: g.descrSym(key), fixedSeq: true}
	// checkBounded guarantees a count on every array, so the capacity is the
	// schema count directly (no zero-sizing fallback).
	cap := spec.count
	// Every holder leads with its element-count member (0..N): MESSAGE_SPEC §5.1
	// gives a wrapper array the length *highest present id + 1*, and `count` is only
	// its capacity, so without one the C holder could express nothing but 0 (every
	// slot default -> the enclosing object omits the field) and N.
	//
	// It has to be the FIRST member of the holder: SOFAB_OBJECT_DESCR_SEQ_SIZED
	// reads it at offset 0 (and asserts offsetof == 0 at compile time). That anchor
	// is what makes the count work for every element kind — a blob element and a
	// native inner-array row are themselves SIZED and start with their own
	// used-length, so an anchor relative to slot 0 addressed that instead, and those
	// two kinds had to go without.
	lead := fmt.Sprintf("%s len; ", lenC(g.cAlignArray(spec)))
	switch spec.elem {
	case ir.KindString, ir.KindBlob:
		// checkBounded guarantees the element maxlen, so the storage is the schema
		// bound directly (no zero-sizing fallback).
		if spec.elem == ir.KindString {
			// +1 for the NUL the corelib's read_string reserves, so a maxlen-byte
			// wire string element is accepted at its schema bound (#103). A string
			// element recovers its length from the NUL, so no companion is needed.
			p.members = append(p.members, member{decl: fmt.Sprintf("%schar items[%d][%d];", lead, cap, spec.max+1)})
			for i := int64(0); i < cap; i++ {
				p.fields = append(p.fields, fieldEntry{macro: fmt.Sprintf(
					"    SOFAB_OBJECT_FIELD(%d, %s, items[%d], SOFAB_OBJECT_FIELDTYPE_STRING),", i, p.cType, i)})
			}
		} else {
			// A blob element is opaque bytes and may be shorter than its maxlen, so —
			// like a scalar blob (issue #128) — each element is a sized blob: a
			// companion used-length immediately before its buffer. Without it a
			// sub-maxlen element re-encodes zero-padded and an all-zero element drops
			// (issue #130). Emit each element as a { len; buf[M]; } struct so the
			// length abuts the byte buffer (alignment 1) for the BLOB_SIZED macro.
			// The holder's own count is `lead`, at offset 0 — the per-slot length in
			// the way of the old anchor is items[0].len, which is why this kind could
			// not carry a count until the anchor moved.
			lenT := blobLenC(spec.max)
			p.members = append(p.members, member{decl: fmt.Sprintf("%sstruct { %s len; uint8_t buf[%d]; } items[%d];", lead, lenT, spec.max, cap)})
			for i := int64(0); i < cap; i++ {
				p.fields = append(p.fields, fieldEntry{macro: fmt.Sprintf(
					"    SOFAB_OBJECT_FIELD_BLOB_SIZED(%d, %s, items[%d].buf, items[%d].len),", i, p.cType, i, i)})
			}
		}
	case ir.KindStruct, ir.KindUnion:
		// Each element is itself a nested object (struct/union): the element type
		// is emitted as a normal named object, and every holder slot is a sequence
		// referencing that one descriptor (nested_idx 0).
		ek := "named/" + spec.ref.Key
		if err := g.collect(ek, g.cType(ek, spec.ref.Target.Name), spec.ref.Target.Fields, plans, order); err == nil {
			p.nested = append(p.nested, ek)
		}
		p.members = append(p.members, member{decl: fmt.Sprintf("%s%s items[%d];", lead, plans[ek].cType, cap)})
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
			p.members = append(p.members, member{decl: fmt.Sprintf("%s%s items[%d];", lead, ip.cType, cap)})
			for i := int64(0); i < cap; i++ {
				p.fields = append(p.fields, fieldEntry{macro: fmt.Sprintf(
					"    SOFAB_OBJECT_FIELD_SEQUENCE(%d, %s, items[%d], SOFAB_OBJECT_FIELDTYPE_SEQUENCE, 0),", i, p.cType, i)})
			}
		} else {
			// Inner element is a native array: each row is a compact array field
			// (id = index), and by §3 the row's wire count is its LENGTH — so each
			// row is a SIZED array, a { len; vals[icap]; } slot exactly like a sized
			// blob element. checkBounded guarantees the inner count. The per-row
			// length occupies the byte before items[0].vals, which is why this kind
			// could not carry a holder count until the anchor moved to offset 0;
			// `lead` is that count, and at offset 0 the two never meet.
			icap := inner.count
			et := g.arrayElemCType(inner.elem, inner.ref)
			iw := lenWidth(icap, cScalarWidth(et))
			p.members = append(p.members, member{decl: fmt.Sprintf("%sstruct { %s len; %s vals[%d]; } items[%d];", lead, lenC(iw), et, icap, cap)})
			for i := int64(0); i < cap; i++ {
				p.fields = append(p.fields, fieldEntry{macro: fmt.Sprintf(
					"    SOFAB_OBJECT_FIELD_ARRAY_SIZED(%d, %s, items[%d].vals, items[%d].len, %s),", i, p.cType, i, i, arrayFieldType(inner.elem))})
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
		// A blob is opaque bytes and may be shorter than its maxlen, so it needs a
		// companion used-length: a bare uint8_t[N] cannot represent "3 of a possible
		// 4" — on re-encode it emits the full N (zero-padded) and an all-zero short
		// blob collapses to empty (issue #128, silent round-trip data loss). The
		// sized descriptor pairs the buffer with a length member that MUST
		// immediately precede it (offsetof(dfield) == offsetof(lfield)+sizeof(lfield));
		// emit both as one adjacent decl so the widest-first member reorder can't
		// separate them, and because a byte buffer has alignment 1 it always abuts
		// the length with no padding. This is the C counterpart of C++ FixedBytes.
		lenT := blobLenC(f.Maxlen)
		decl = fmt.Sprintf("%s %s_len; uint8_t %s[%d];", lenT, mn, mn, f.Maxlen)
		entry = fmt.Sprintf("    SOFAB_OBJECT_FIELD_BLOB_SIZED(%d, %s, %s, %s_len),", f.ID, cType, mn, mn)
	case ir.KindEnum:
		decl = fmt.Sprintf("%s %s;", enumC(f.Ref.Target), mn)
		entry = field(f.ID, cType, mn, "SIGNED")
	case ir.KindBitfield:
		decl = fmt.Sprintf("%s %s;", bitfieldC(f.Ref.Target), mn)
		entry = field(f.ID, cType, mn, "UNSIGNED")
	case ir.KindArray:
		// Native array element (numeric/enum/boolean/bitfield): enum -> signed,
		// boolean/bitfield -> unsigned, value-converted (not a sequence).
		//
		// MESSAGE_SPEC §3: `count: N` is the array's CAPACITY and the wire count M
		// is its LENGTH — every element held is written, trailing element defaults
		// included ([1,2,3,0,0] and [1,2,3] are different values). A plain
		// SOFAB_OBJECT_FIELD_ARRAY derives the count structurally from
		// sizeof(field)/sizeof(field[0]), i.e. the capacity, so it can express only
		// the length N and a decode of M < N re-encodes as N. The sized descriptor
		// pairs the buffer with a companion length member holding 0..N — the array
		// counterpart of the sized blob above, with the same adjacency requirement
		// (see lenWidth for how the width is chosen).
		et := g.arrayElemCType(f.Elem, f.ElemRef)
		w := lenWidth(f.Count, cScalarWidth(et))
		decl = fmt.Sprintf("%s %s_len; %s %s[%d];", lenC(w), mn, et, mn, f.Count)
		entry = fmt.Sprintf("    SOFAB_OBJECT_FIELD_ARRAY_SIZED(%d, %s, %s, %s_len, %s),", f.ID, cType, mn, mn, arrayFieldType(f.Elem))
	default:
		return "", "", fmt.Errorf("field %q: unsupported kind %s for C backend", f.Name, f.Kind)
	}
	return decl, entry, nil
}

// ---- emit pieces --------------------------------------------------------

func (g *gen) emitStruct(h *cfile, p *objectPlan) {
	h.line("typedef struct {")
	for _, m := range p.members {
		decl, doc := m.decl, m.doc
		if m.deprecated {
			// Emit the native marker so callers touching the field warn, and add a
			// Doxygen @deprecated note so the doc tool renders a deprecation section.
			decl = deprecatedDecl(decl)
			if doc != "" {
				doc += " @deprecated"
			} else {
				doc = "@deprecated"
			}
		}
		// A field with a schema bound takes the leading block form: the note does
		// not fit a trailing /**< ... */, and it documents the length member the
		// declaration line declares alongside the storage.
		switch {
		case m.note != "":
			h.line("    /**")
			if doc != "" {
				h.line("     * %s", doc)
				h.line("     *")
			}
			h.line("     * %s", m.note)
			h.line("     */")
			h.line("    %s", decl)
		case doc != "":
			h.line("    %s  /**< %s */", decl, doc)
		default:
			h.line("    %s", decl)
		}
	}
	h.line("} %s;", p.cType)
	h.blank()
}

// blobLenC picks the narrowest unsigned C type that can hold a used-length in
// 0..maxlen for a sized blob's companion length member. The width is recorded in
// the descriptor (SOFAB_OBJECT_FIELD_BLOB_SIZED reads sizeof(lfield)), so keeping
// it minimal costs no wire bytes and only a byte or two of struct storage.
func blobLenC(maxlen int64) string { return lenC(countWidth(maxlen)) }

// lenC names the unsigned C type of a companion length member of byte width w
// (the descriptor stores only sizeof(lfield), one of 1/2/4/8).
func lenC(w int64) string {
	switch w {
	case 1:
		return "uint8_t"
	case 2:
		return "uint16_t"
	case 4:
		return "uint32_t"
	default:
		return "uint64_t"
	}
}

// countWidth is the narrowest 1/2/4/8-byte width that can hold 0..n.
func countWidth(n int64) int64 {
	switch {
	case n <= 0xFF:
		return 1
	case n <= 0xFFFF:
		return 2
	case n <= 0xFFFFFFFF:
		return 4
	default:
		return 8
	}
}

// lenWidth picks a companion length member's byte width for a run of `count`
// elements whose storage alignment is `align`.
//
// Two constraints, and the wider one wins. It must hold 0..count, and it must be
// at least as wide as one element: the descriptor records only the width and
// reads the length at <buffer offset − width>, so the two members have to be
// ADJACENT, and a length narrower than the buffer's alignment is padded away
// from it ({ uint8_t len; uint32_t v[4]; } puts v at offset 4, three bytes past
// the length). corelib-c-cpp asserts that adjacency at compile time
// (SOFAB_OBJECT_ASSERT_LEN_ADJACENT, a negative array bound), so getting this
// wrong is a build error rather than a silent misread — but it is chosen here
// deliberately, not discovered by trial. Both inputs are powers of two ≤ 8, so
// the maximum is one too and a member of that width sits at a multiple of it,
// which makes the following buffer's alignment automatic.
func lenWidth(count, align int64) int64 {
	w := countWidth(count)
	if align > w {
		w = align
	}
	return w
}

// cScalarWidth is the size (and, for every type the backend emits, the
// alignment) of a scalar C type named by uintC/intC/enumC/bitfieldC/arrayElemC.
func cScalarWidth(t string) int64 {
	switch t {
	case "uint8_t", "int8_t", "char":
		return 1
	case "uint16_t", "int16_t":
		return 2
	case "uint32_t", "int32_t", "float":
		return 4
	default: // uint64_t, int64_t, double
		return 8
	}
}

// cAlign is the storage alignment of the C member a field lowers to. It is
// derived from the schema alone (never from a built plan), so the message
// emitter and the JSON harness agree without sharing state. Alignment matters
// here because a sized array / sized wrapper holder places its length member
// immediately before the storage it describes: see lenWidth.
func (g *gen) cAlign(f *ir.Field) int64 {
	switch f.Kind {
	case ir.KindU8, ir.KindI8, ir.KindBool:
		return 1
	case ir.KindU16, ir.KindI16:
		return 2
	case ir.KindU32, ir.KindI32, ir.KindFP32:
		return 4
	case ir.KindU64, ir.KindI64, ir.KindFP64:
		return 8
	case ir.KindEnum:
		return cScalarWidth(enumC(f.Ref.Target))
	case ir.KindBitfield:
		return cScalarWidth(bitfieldC(f.Ref.Target))
	case ir.KindString:
		return 1 // char[]
	case ir.KindBlob:
		return cScalarWidth(blobLenC(f.Maxlen)) // { len; uint8_t buf[]; }
	case ir.KindStruct, ir.KindUnion:
		return g.cAlignFields(f.Ref.Target.Fields)
	case ir.KindArray:
		return g.cAlignArray(specOfField(f))
	}
	return 1
}

// cAlignFields is the alignment of a generated struct: the widest of its members.
func (g *gen) cAlignFields(fields []*ir.Field) int64 {
	var a int64 = 1
	for _, f := range fields {
		if x := g.cAlign(f); x > a {
			a = x
		}
	}
	return a
}

// cAlignArray is the alignment of the member an array field lowers to — which is
// also the width of the length member that member leads with, for every form: a
// compact array leads with its element count, and every wrapper holder leads with
// its element count too (buildHolder's `lead`). lenWidth is that width: wide
// enough to hold 0..N, and never narrower than the storage it precedes.
//
// For a compact array (SOFAB_OBJECT_FIELD_ARRAY_SIZED) the second half is a hard
// requirement — the descriptor finds the length at <offset − width>, so padding
// between them would be read as the length. For a holder the corelib reads the
// count at offset 0 and no longer cares (SOFAB_OBJECT_DESCR_SEQ_SIZED), but the
// rule is kept: a narrower count buys nothing, it only turns its own spare bytes
// into dead padding ahead of the aligned slots.
func (g *gen) cAlignArray(spec arraySpec) int64 {
	switch spec.elem {
	case ir.KindString:
		return lenWidth(spec.count, 1) // char items[N][max+1]
	case ir.KindBlob:
		// struct { blobLen len; uint8_t buf[max]; } items[N]
		return lenWidth(spec.count, cScalarWidth(blobLenC(spec.max)))
	case ir.KindStruct, ir.KindUnion:
		return lenWidth(spec.count, g.cAlignFields(spec.ref.Target.Fields))
	case ir.KindArray:
		// Either an inner holder or a struct { rowLen len; T vals[icap]; } row —
		// cAlignArray(inner) is the slot's alignment in both cases.
		return lenWidth(spec.count, g.cAlignArray(specOfItems(spec.items)))
	}
	// Compact (native) array: <len> <elem>[count].
	return lenWidth(spec.count, cScalarWidth(g.arrayElemCType(spec.elem, spec.ref)))
}

// deprecatedDecl inserts the GCC/Clang deprecated attribute onto a struct-member
// declaration ("uint32_t legacyId;" -> "uint32_t legacyId __attribute__((deprecated));").
func deprecatedDecl(decl string) string {
	if strings.HasSuffix(decl, ";") {
		return decl[:len(decl)-1] + " __attribute__((deprecated));"
	}
	return decl + " __attribute__((deprecated))"
}

func (g *gen) emitDescriptor(c *cfile, p *objectPlan) {
	// The field table's sizeof(((T*)0)->field) and the defaults image's
	// designated initializers both name deprecated members, which warn under
	// -Wdeprecated-declarations. Suppress locally so the generated .c stays
	// warning-clean; offsetof (also in the macro) does not warn on its own.
	if p.hasDeprecated {
		c.line("#pragma GCC diagnostic push")
		c.line(`#pragma GCC diagnostic ignored "-Wdeprecated-declarations"`)
	}
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
		// A holder (fixedSeq) never carries a defaults image (its elements default to
		// empty/zero), so WITH_DEFAULTS and SEQ are mutually exclusive in practice.
		c.line("static const %s %s = {", p.cType, g.defaultsSym(p.key))
		for _, d := range p.defaults {
			c.line("    .%s = %s,", d.ident, d.expr)
		}
		c.line("};")
		c.line("const sofab_object_descr_t %s = SOFAB_OBJECT_DESCR_WITH_DEFAULTS(%s, %d, %s, %d, &%s);",
			p.descr, g.fieldsSym(p.key), len(p.fields), nested, nestedCount, g.defaultsSym(p.key))
	} else if p.fixedSeq {
		c.line("const sofab_object_descr_t %s = SOFAB_OBJECT_DESCR_SEQ_SIZED(%s, %d, %s, %d, %s, len);",
			p.descr, g.fieldsSym(p.key), len(p.fields), nested, nestedCount, p.cType)
	} else {
		c.line("const sofab_object_descr_t %s = SOFAB_OBJECT_DESCR(%s, %d, %s, %d);",
			p.descr, g.fieldsSym(p.key), len(p.fields), nested, nestedCount)
	}
	if p.hasDeprecated {
		c.line("#pragma GCC diagnostic pop")
	}
	c.blank()
}

func (g *gen) emitProtos(h *cfile, m *ir.Message, msgType string, root *objectPlan) {
	pfx := g.prefix + strings.ToLower(m.Name)
	h.doc("Initialize a %s with its schema defaults (non-default fields zeroed).", m.Name)
	h.line("void %s_init(%s *msg);", pfx, msgType)
	h.doc("Encode msg into buf[buflen]; *used receives the byte count. Returns sofab_ret_t.")
	h.line("sofab_ret_t %s_encode(const %s *msg, uint8_t *buf, size_t buflen, size_t *used);", pfx, msgType)
	h.doc("Decode buf[len] into msg (call %s_init first to apply defaults). Returns sofab_ret_t.", pfx)
	h.line("sofab_ret_t %s_decode(%s *msg, const uint8_t *buf, size_t len);", pfx, msgType)
	h.blank()

	// Streaming. The one-shot functions above own their stream for the length of
	// one call, which forces the whole message through a single buffer in each
	// direction. The corelib does not require that -- sofab_ostream_init takes a
	// flush callback and sofab_istream_feed is incremental -- but neither was
	// reachable, because the descriptor was defined in the .c without a
	// declaration and the decoder state was a local. Both are exposed here.
	h.doc("Object descriptor for %s, for use with the sofab_object_* API directly.", m.Name)
	h.line("extern const sofab_object_descr_t %s;", root.descr)
	h.blank()
	h.doc("Encode msg into a stream the caller owns. With a flush callback on that\n" +
		"stream the message may exceed its buffer: the buffer is drained as it\n" +
		"fills, so what bounds memory is the buffer, not the message. The caller\n" +
		"flushes the tail with sofab_ostream_flush().")
	h.line("sofab_ret_t %s_encode_to(sofab_ostream_t *os, const %s *msg);", pfx, msgType)
	h.blank()
	h.doc("Incremental decoder: hold one and feed the message as bytes arrive,\n" +
		"instead of buffering it whole first.\n" +
		"\n" +
		"The wire format has no end marker at the top level -- a message ends\n" +
		"where its bytes end -- so a feed cannot report that the MESSAGE is\n" +
		"complete, only that the bytes handed in ended on a field boundary\n" +
		"(SOFAB_RET_OK) or mid-field (SOFAB_RET_INCOMPLETE). Neither is a failure\n" +
		"mid-stream; the caller's own framing decides when the input is over, and\n" +
		"the last verdict says whether it ended half-read.")
	h.line("typedef struct {")
	h.line("    sofab_istream_t is;")
	h.line("    sofab_object_decoder_t dec[%d];", g.maxDepth(m.Fields)+1)
	h.line("} %s_decoder_t;", pfx)
	h.blank()
	h.doc("Bind a decoder to msg. Call %s_init on msg first to apply defaults.", pfx)
	h.line("void %s_decoder_init(%s_decoder_t *d, %s *msg);", pfx, pfx, msgType)
	h.blank()
	h.doc("Feed the next chunk. See %s_decoder_t for what the return value means.", pfx)
	h.line("sofab_ret_t %s_decoder_feed(%s_decoder_t *d, const void *buf, size_t len);", pfx, pfx)
}

func (g *gen) emitFuncs(c *cfile, m *ir.Message, msgType string, root *objectPlan) {
	pfx := g.prefix + strings.ToLower(m.Name)

	c.line("void %s_init(%s *msg) {", pfx, msgType)
	// Zero first: sofab_object_init only writes descriptor fields, so a sized
	// blob's companion _len member (not a descriptor field) would otherwise be
	// left uninitialized and drive a garbage-length encode (issue #128).
	c.line("    memset(msg, 0, sizeof(*msg));")
	c.line("    sofab_object_init(&%s, msg);", root.descr)
	for _, b := range root.blobLenInits {
		c.line("    msg->%s_len = %d;", b.member, b.length)
	}
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

	c.line("sofab_ret_t %s_encode_to(sofab_ostream_t *os, const %s *msg) {", pfx, msgType)
	c.line("    return sofab_object_encode(os, &%s, msg);", root.descr)
	c.line("}")
	c.blank()

	c.line("void %s_decoder_init(%s_decoder_t *d, %s *msg) {", pfx, pfx, msgType)
	c.line("    memset(d->dec, 0, sizeof(d->dec));")
	c.line("    d->dec[0].info = &%s;", root.descr)
	c.line("    d->dec[0].dst = (uint8_t *)msg;")
	c.line("    d->dec[0].depth = (uint8_t)(sizeof(d->dec) / sizeof(d->dec[0]) - 1);")
	c.line("    sofab_istream_init(&d->is, sofab_object_field_cb, (void *)&d->dec[0]);")
	c.line("}")
	c.blank()

	c.line("sofab_ret_t %s_decoder_feed(%s_decoder_t *d, const void *buf, size_t len) {", pfx, pfx)
	c.line("    return sofab_istream_feed(&d->is, buf, len);")
	c.line("}")
	c.blank()

	// One-shot decode is the incremental one fed once, so the two cannot drift.
	c.line("sofab_ret_t %s_decode(%s *msg, const uint8_t *buf, size_t len) {", pfx, msgType)
	c.line("    %s_decoder_t d;", pfx)
	c.line("    %s_decoder_init(&d, msg);", pfx)
	c.line("    return %s_decoder_feed(&d, buf, len);", pfx)
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
		// Every array form now carries a companion length member somewhere (the
		// compact array's, the wrapper holder's element count, a sized blob/row
		// element's), and the corelib reads it through _load_uint — whose 8-byte
		// case is compiled out by SOFAB_DISABLE_INT64_SUPPORT. An 8-byte-aligned
		// element forces an 8-byte length (lenWidth), so such a schema genuinely
		// needs 64-bit value support even when no field is a 64-bit integer: guard
		// it loudly instead of silently reading every length as 0.
		if g.cAlignArray(spec) == 8 {
			caps.value64 = true
		}
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
