// Package python is the Python throughput backend (PLAN §6.4): it emits
// dataclasses with serialize (Encoder) and, per class, a FLAT decode visitor
// against corelib-py -- whose pull API is gone, CORELIB_PLAN §5.3.1 making the
// visitor the only decode surface. See visitor.go for the decode half. Plus JSON
// helpers for the conformance harness.
package python

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/sofa-buffers/generator/internal/generator"
	"github.com/sofa-buffers/generator/internal/ir"
)

func init() { generator.Register(&Backend{}) }

// Backend implements generator.Backend for Python.
type Backend struct{}

func (*Backend) Lang() string { return "python" }

// Generate emits a single module (all enums/bitfields/dataclasses + messages).
// In project mode it adds a harness and pyproject.toml.
func (*Backend) Generate(s *ir.Schema, cfg map[string]any) ([]generator.File, error) {
	g := &gen{
		schema:  s,
		banner:  cfgString(cfg, "tool_banner", "sofabgen"),
		license: generator.LicenseID(cfg),
		limits:  resolveLimits(s, cfg),
		size:    generator.NewSizePolicy(cfg),
	}
	g.resolveReassembly(s)
	// No name check: every name the module declares comes from a channel that
	// keeps distinct schema names distinct (ARCHITECTURE §8, "Naming"). Enum
	// constants and bitfield flags are upper-cased, which the fold rule keeps
	// apart; union members are escaped by shape (unionDerivedShape).
	module := g.module(s)
	files := []generator.File{{Path: "message.py", Content: module}}
	if cfgString(cfg, "emit", "sources") == "project" {
		files = append(files, g.projectFiles(s, cfg)...)
	}
	if g.sizeErr != nil {
		return nil, g.sizeErr
	}
	return files, nil
}

type gen struct {
	schema  *ir.Schema
	banner  string
	license string // SPDX id, "" to omit the header line
	limits  limitSet
	// span is the largest single construct this schema can carry, and reassembly
	// the buffer size the streaming reader is built with (see cost.go and
	// resolveReassembly). corelib-py takes the buffer as a REQUIRED argument and
	// holds no size of its own (corelib-py#139), so both are derived here.
	span       int64
	reassembly int64
	// size is the max_message_size policy; sizeErr carries a violation out of
	// the emit path, which has no error channel of its own.
	size    generator.SizePolicy
	sizeErr error
	// The decode half's destination tables (binding.go), decided for every class
	// BEFORE any of them is emitted: a class's own decode() has to know whether
	// its visitor carries a scatter, and the shared _StreamDecoder has to know
	// whether ANY of them does. scopes caches the tree each plan was built from,
	// which the visitor emitter walks again.
	plans  map[string]*bindPlan
	scopes map[string][]*pyScope
	// bind is the plan of the class being emitted, nil while none is.
	bind *bindPlan
	// unions caches each union type's options and derived names (union.go).
	unions map[*ir.NamedType]*unionShape
	// bases maps a class name to its unescaped type identifier, which its private
	// names (visitor, locations, tables) are derived from.
	bases map[string]string
	// defBase is the unescaped type identifier of the class being emitted, and
	// defs the float array defaults it has referenced so far, flushed after the
	// class as module-level constants (floatArrayDefault).
	defBase string
	defs    []pyDefConst
}

// pyDefConst is one module-level constant holding a float array's default.
type pyDefConst struct{ name, lit string }

// scopesFor returns a class's scope tree, built once and reused: buildBindPlan
// walks it before emission and emitVisitor walks it again.
func (g *gen) scopesFor(c decodeClass) []*pyScope {
	if sc, ok := g.scopes[c.name]; ok {
		return sc
	}
	sc := g.buildScopes(c)
	if g.scopes == nil {
		g.scopes = map[string][]*pyScope{}
	}
	g.scopes[c.name] = sc
	return sc
}

// anyBind reports whether any class in the module carries a destination table --
// what decides whether the shared _StreamDecoder calls scatter() at all.
func (g *gen) anyBind() bool { return len(g.plans) > 0 }

// The floor corelib-py puts on a reassembly buffer: sofab.MIN_REASSEMBLY, what a
// single construct's framing (an id header and a length word, ten bytes each at
// the outside) can need. A schema of nothing but bools still has to clear it.
const minReassembly = 16

// Room for one incoming chunk on top of the largest construct.
//
// corelib-py appends a fed chunk into the reassembly buffer whenever anything is
// carried, so the buffer has to hold the carry AND the chunk — and only the
// caller knows how big its chunks are. 64 KiB is the size a socket reader
// usually lands on, so it is what the streaming reader is built with by default;
// a caller streaming larger pieces passes its own size to “decoder()“, which
// takes one for exactly this reason. The one-shot “decode()“ needs none of it:
// a message fed in a single call never touches the buffer, and the most a
// truncated one can leave behind is the construct in flight.
const streamChunkRoom = 64 * 1024

// resolveReassembly derives the two byte counts the generated module states for
// corelib-py's reassembly buffer. An unbounded construct (which cannot occur
// while every max_dyn_* cap is finite, §9.5) leaves the span at the floor rather
// than emitting a number smaller than the schema needs.
func (g *gen) resolveReassembly(s *ir.Schema) {
	caps := &ir.DynCaps{
		ArrayCount: g.limits.arrayCount, HasArray: true,
		StringLen: g.limits.stringLen, HasString: true,
		BlobLen: g.limits.blobLen, HasBlob: true,
	}
	span, ok := maxConstructSpan(s, caps)
	if !ok {
		span = 0
	}
	g.span = max(span, minReassembly)
	g.reassembly = g.span + streamChunkRoom
}

// messageSize resolves a class's worst-case encoded size via the shared walk
// (ir.MaxWireSize), falling back to the configured max_message_size ceiling when
// a field is unbounded. The emit path has no error channel, so a violation of an
// explicitly configured ceiling is recorded here and surfaced by Generate.
//
// A union class encodes one option, so it is sized by its largest option.
func (g *gen) messageSize(name string, fields []*ir.Field, union bool) generator.MessageSize {
	resolve := g.size.Resolve
	if union {
		resolve = g.size.ResolveUnion
	}
	ms, err := resolve(name, fields)
	if err != nil && g.sizeErr == nil {
		g.sizeErr = err
	}
	return ms
}

// limitSet is the receiver-side decode-limit configuration (generator#102),
// resolved against the schema. Every cap is always set -- the target carries a
// finite default that the config key only overrides (§9.5, generator#385) -- so
// an entry is active exactly when the schema actually has an unbounded field of
// that kind; otherwise the cap would be inert and no plumbing is emitted.
//
// The configured value is emitted AS CONFIGURED. It used to be raised to the
// largest schema bound of its kind, because a Decoder applies its caps per
// Decoder and would otherwise reject a schema-bounded field larger than the cap
// -- which §6.2.1 forbids, since there the schema bound governs. That raise kept
// such messages decodable by loosening the cap for the UNBOUNDED fields too,
// which is exactly the protection §6.2.1 wants kept tight. Generated code
// applies the caps itself instead, to exactly the ids the schema leaves open, so
// the cap can stay at the number the deployment chose (generator#325).
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
	// The VALUES are kept whatever the schema declares, because every Decoder
	// takes all three and a missing one is a caller defect, not a looser bound.
	// The Has flags say only whether a MODULE CONSTANT is worth exporting: a cap
	// no field of that kind can ever be judged against is a name nothing reads.
	return limitSet{
		arrayCount: d.ArrayCount, arrayHas: b.HasDynArray,
		stringLen: d.StringLen, stringHas: b.HasDynString,
		blobLen: d.BlobLen, blobHas: b.HasDynBlob,
	}
}

// capsArgs renders the three receiver-cap keyword arguments every Decoder in the
// module is built with. The exported constant is preferred where one exists, so
// the module keeps one number per kind; where the schema bounds every field of
// that kind no constant is emitted and the configured value goes in as a literal
// rather than the module growing a name nothing else reads.
func (g *gen) capsArgs() string {
	name := func(has bool, konst string, v int64) string {
		if has {
			return konst
		}
		return strconv.FormatInt(v, 10)
	}
	return fmt.Sprintf("max_dyn_array_count=%s, max_dyn_string_len=%s, max_dyn_blob_len=%s",
		name(g.limits.arrayHas, "MAX_DYN_ARRAY_COUNT", g.limits.arrayCount),
		name(g.limits.stringHas, "MAX_DYN_STRING_LEN", g.limits.stringLen),
		name(g.limits.blobHas, "MAX_DYN_BLOB_LEN", g.limits.blobLen))
}

type pyfile struct{ b strings.Builder }

func (f *pyfile) line(format string, args ...any) {
	fmt.Fprintf(&f.b, format, args...)
	f.b.WriteByte('\n')
}

// lines writes each of ls verbatim (no formatting), prefixed with ind.
func (f *pyfile) lines(ind string, ls []string) {
	for _, l := range ls {
		f.b.WriteString(ind)
		f.b.WriteString(l)
		f.b.WriteByte('\n')
	}
}
func (f *pyfile) blank()        { f.b.WriteByte('\n') }
func (f *pyfile) bytes() []byte { return []byte(f.b.String()) }

// sofabImports is the `from sofab import` list of a module whose decode section
// is decodeSection, sorted. Every name is read off the emitted text (see below);
// TestTypeReservedCoversImports feeds it a text that reaches every branch, so
// each upper-case name it can return is checked against pyTypeReserved.
func sofabImports(decodeSection, typeSection string) []string {
	// SofaDecodeError and SofaIncompleteError are unconditional: every class's
	// decode() surfaces the three-valued outcome through them (MESSAGE_SPEC §7).
	names := []string{
		"Decoder", "Encoder", "SofaDecodeError", "SofaIncompleteError",
		"Status", "Visitor",
	}
	// SofaLimitError is imported only if emitted text still raises it (no
	// emitter does today, so no module imports it; the test still holds the
	// name to pyTypeReserved through this branch). The
	// wrapper-array element INDEX bound -- a field id, not a count/length word, so
	// the Decoder cannot see it -- is judged by the corelib's reserve_* helpers,
	// which raise it themselves (reserveCall). Each helper, and the UNBOUNDED
	// sentinel, is imported exactly where a call names it. Read off the emitted
	// text rather than re-derived, for the reason visitorNeeds is: a second walk
	// over the schema has to agree with the emitter by hand.
	if usesName(decodeSection, "SofaLimitError", "(") {
		names = append(names, "SofaLimitError")
	}
	// SofaArgumentError is what serialize() raises for a wrapper array past its
	// schema count (encodeGuard); a schema with none emits and imports none.
	if usesName(typeSection, "SofaArgumentError", "(") {
		names = append(names, "SofaArgumentError")
	}
	for _, fn := range []string{"reserve_elem", "reserve_leaf", "reserve_row"} {
		if strings.Contains(decodeSection, fn+"(") {
			names = append(names, fn)
		}
	}
	if strings.Contains(decodeSection, ", UNBOUNDED, ") {
		names = append(names, "UNBOUNDED")
	}
	// Binding is the destination table (binding.go); a schema whose fields the
	// table cannot carry emits none and imports none. Matched on the call's
	// opening parenthesis, not on `Binding()`: a module whose every table is
	// closed spells each one `Binding(closed=True)`, and matching the empty call
	// left exactly those modules without the import -- a NameError at import.
	if strings.Contains(decodeSection, "(Binding(") {
		names = append(names, "Binding")
	}
	// Field is the on_field argument, and WireType / FixlenSubtype are the tags
	// its §7.3 tests compare against; each appears only where a bound exists.
	needField, needWire, needFixlen := visitorNeeds(decodeSection)
	if needField {
		names = append(names, "Field")
	}
	if needFixlen {
		names = append(names, "FixlenSubtype")
	}
	if needWire {
		names = append(names, "WireType")
	}
	// The default of a float array is a FloatArrayDefault in the type section.
	if strings.Contains(typeSection, "FloatArrayDefault(") {
		names = append(names, "FloatArrayDefault")
	}
	sort.Strings(names)
	return names
}

func (g *gen) module(s *ir.Schema) []byte {
	f := &pyfile{}
	f.line("# Code generated by %s; DO NOT EDIT.", g.banner)
	if g.license != "" {
		f.line("# SPDX-License-Identifier: %s", g.license)
	}
	f.line("from __future__ import annotations")
	// The type and decode sections are emitted FIRST, into buffers, so every
	// import line can be read off what the module actually references
	// (visitorNeeds) instead of a second walk over the schema that has to agree
	// with the emitter by hand. An import nothing uses is a pyflakes finding
	// (F401) in the user's lint run. Decode goes first: it decides the
	// destination tables (g.plans) the dataclasses' decode() reads.
	decodeSection := g.decodeSection(s)
	typeSection := g.typeSection(s)
	if strings.Contains(typeSection, "math.copysign(") {
		f.line("import math")
	}
	if imp := stdlibImport("dataclasses", typeSection, dataclassNames); imp != "" {
		f.line("%s", imp)
	}
	if imp := stdlibImport("enum", typeSection, enumNames); imp != "" {
		f.line("%s", imp)
	}
	if imp := stdlibImport("typing", typeSection, typingNames); imp != "" {
		f.line("%s", imp)
	}
	f.line("from sofab import %s", strings.Join(sofabImports(decodeSection, typeSection), ", "))
	f.blank()

	if g.limits.any() {
		f.line("# Receiver-side decode limits, baked from the sofabgen config")
		f.line("# (max_dyn_array_count / max_dyn_string_len / max_dyn_blob_len). They govern")
		f.line("# only fields the schema left unbounded -- a cap must never bind a field")
		f.line("# the schema already bounds -- and every Decoder built below is handed all")
		f.line("# three. It applies them at the count/length header, before any allocation,")
		f.line("# and takes one back off a field on_schema_bound declares or a field that")
		f.line("# is skipped; exceeding one raises sofab.SofaLimitError.")
		f.line("#")
		f.line("# The numbers are the caller's, not the library's: it holds none, defaults")
		f.line("# none, and reads no omitted argument as unlimited, so all three are")
		f.line("# required arguments.")
		if g.limits.arrayHas {
			f.line("MAX_DYN_ARRAY_COUNT = %d", g.limits.arrayCount)
		}
		if g.limits.stringHas {
			f.line("MAX_DYN_STRING_LEN = %d", g.limits.stringLen)
		}
		if g.limits.blobHas {
			f.line("MAX_DYN_BLOB_LEN = %d", g.limits.blobLen)
		}
		f.blank()
	}

	// The reassembly buffer's size, derived the same way and required for the
	// same reason (corelib-py#139): a construct split across fed chunks is
	// joined in storage the CALLER supplies, and §6.2.1 leaves the codec no size
	// to invent any more than it leaves it a cap. See resolveReassembly.
	f.line("# Bytes of reassembly space, derived from the schema and the decode limits.")
	f.line("#")
	f.line("# A construct split across two fed chunks is joined in a buffer the caller")
	f.line("# supplies, sized once and never grown; the library holds no size of its")
	f.line("# own, so -- like the limits -- the number is stated here.")
	f.line("#")
	f.line("# MAX_FIELD_SPAN is the largest single value this schema can carry: one")
	f.line("# string or blob payload, or one native array's whole element run, with")
	f.line("# every varint at its widest and each decode limit standing in for a")
	f.line("# missing schema bound. A nested message is NOT one construct -- its fields")
	f.line("# are read one at a time -- and neither is a wrapper array of strings,")
	f.line("# blobs or structs. A field the receiver SKIPS never enters the buffer at")
	f.line("# all, whatever its size, so this covers what is READ.")
	f.line("MAX_FIELD_SPAN = %d", g.span)
	f.line("#")
	f.line("# The streaming reader also holds the chunk it was just fed, and only the")
	f.line("# caller knows how large those are. %d bytes is what decoder() assumes;", streamChunkRoom)
	f.line("# a caller streaming larger pieces passes its own size. The one-shot")
	f.line("# decode() needs none of it -- one chunk spans no boundary.")
	f.line("REASSEMBLY = MAX_FIELD_SPAN + %d", streamChunkRoom)
	f.blank()

	f.b.WriteString(typeSection)
	// Decode last: an array scope hands the corelib an element CONSTRUCTOR to
	// grow the list with, so every dataclass a visitor can name must already be
	// defined.
	f.b.WriteString(decodeSection)
	return f.bytes()
}

// typeSection renders the enums, the bitfield constants and the dataclasses
// (structs/unions, then messages) as text, so module() can size its stdlib
// imports from it.
func (g *gen) typeSection(s *ir.Schema) string {
	f := &pyfile{}
	// enums + bitfield constants first
	for _, key := range s.NamedOrder {
		nt := s.Named[key]
		switch nt.Category {
		case ir.CatEnum:
			g.emitEnum(f, nt)
		case ir.CatBitfield:
			g.emitBitfieldConsts(f, nt)
		}
	}
	// struct/union dataclasses, then messages
	for _, key := range s.NamedOrder {
		nt := s.Named[key]
		switch nt.Category {
		case ir.CatStruct:
			g.emitDataclass(f, typeIdent(nt), nt.Summary, nt.Fields, nil)
		case ir.CatUnion:
			g.emitDataclass(f, typeIdent(nt), nt.Summary, nt.Fields, g.unionShapeOf(nt))
		}
	}
	for _, m := range s.Messages {
		g.emitDataclass(f, msgIdent(m), m.Summary, m.Fields, nil)
	}
	return f.b.String()
}

// stdlibName is one name the type section may import, and the text that marks
// its use: what the emitter writes when it uses the name, never the bare name,
// which a field or doc comment may spell too.
type stdlibName struct{ name, use string }

// dataclassNames and enumNames are matched against the rendered type section,
// the only place the module uses them: `@dataclass` heads every class,
// `field(...)` is pyDefault's mutable default, and the two enum bases are what
// emitEnum and emitBitfieldConsts derive from.
var (
	dataclassNames = []stdlibName{{"dataclass", "@dataclass\n"}, {"field", "field(default_factory="}}
	enumNames      = []stdlibName{{"IntEnum", "(IntEnum):\n"}, {"IntFlag", "(IntFlag):\n"}}
	// ClassVar keeps a union's option-id constants out of the dataclass fields;
	// dataclasses recognises the string annotation only through this import.
	typingNames = []stdlibName{{"ClassVar", ": ClassVar[int] = "}}
)

// stdlibImport returns `from <mod> import <names>` for the names the text uses,
// or "" when it uses none of them.
func stdlibImport(mod, text string, names []stdlibName) string {
	var used []string
	for _, n := range names {
		if strings.Contains(text, n.use) {
			used = append(used, n.name)
		}
	}
	if len(used) == 0 {
		return ""
	}
	return fmt.Sprintf("from %s import %s", mod, strings.Join(used, ", "))
}

// decodeSection renders the module's decode half -- the streaming reader plus
// one flat visitor per generated class -- as text, so module() can size its
// import line from it.
func (g *gen) decodeSection(s *ir.Schema) string {
	// Every class's table is decided BEFORE any class is emitted: a class's
	// decode() has to know whether its visitor carries a scatter, the dataclasses
	// are emitted ahead of this section, and the shared _StreamDecoder has to know
	// whether any visitor in the module carries one.
	g.plans = map[string]*bindPlan{}
	g.bases = map[string]string{}
	for _, c := range g.decodeClasses(s) {
		g.bases[c.name] = c.base
		if p := g.buildBindPlan(c, g.scopesFor(c)); p != nil {
			g.plans[c.name] = p
		}
	}

	body := &pyfile{}
	for _, c := range g.decodeClasses(s) {
		g.emitVisitor(body, c)
	}

	f := &pyfile{}
	f.line("# --- decode ---------------------------------------------------------------")
	f.blank()
	if g.anyBind() {
		f.line("# The slot value that says a field never arrived. Storage a destination table")
		f.line("# writes into starts filled with it, and the decoder overwrites only what the")
		f.line("# wire carried, so a slot still holding it is a field the message omitted --")
		f.line("# which is how absence is reported without inventing a value for it.")
		f.line("_ABSENT = %s", bindAbsent)
		f.blank()
	}
	g.emitStreamDecoder(f)
	f.b.WriteString(body.b.String())
	return f.b.String()
}

// decodeClass is one generated class with a visitor: the struct/union
// dataclasses, then the messages, in emission order.
type decodeClass struct {
	name   string // the class name
	base   string // its identifier before the escape: what private names derive from
	fields []*ir.Field
	union  *unionShape // non-nil for a union type
}

func (g *gen) decodeClasses(s *ir.Schema) []decodeClass {
	var out []decodeClass
	for _, key := range s.NamedOrder {
		nt := s.Named[key]
		switch nt.Category {
		case ir.CatStruct:
			out = append(out, decodeClass{typeIdent(nt), baseIdent(nt), nt.Fields, nil})
		case ir.CatUnion:
			out = append(out, decodeClass{typeIdent(nt), baseIdent(nt), nt.Fields, g.unionShapeOf(nt)})
		}
	}
	for _, m := range s.Messages {
		out = append(out, decodeClass{msgIdent(m), msgBase(m), m.Fields, nil})
	}
	return out
}

// emitStreamDecoder writes the generated reader §6.1.1 requires: the streaming
// half of the decode pair, obtained from a class's decoder() and fed chunks of
// any size. There is deliberately no finish()/end() — the status each feed
// returns IS the outcome so far, and whether an INCOMPLETE at end-of-input is
// acceptable is the caller's framing decision (CORELIB_PLAN §5.2.4).
//
// It remembers nothing (generator#541). §5.2.4 gives one channel per fact, and
// feed's return value is it; the `_st` copy #461 kept here could only restate
// what the corelib already holds. Both refusals are terminal and latched THERE:
// an INVALID is re-returned by every later feed (`decoder.py`'s `if self._status
// is Status.INVALID: return Status.INVALID`), and a receiver cap is re-raised
// from `self._limit`. Asking a second time therefore needs no memory of its own —
// feeding an empty chunk re-delivers the same answer.
func (g *gen) emitStreamDecoder(f *pyfile) {
	f.line("class _StreamDecoder:")
	f.line("    \"\"\"Streaming reader: feed chunks, read the message when it is COMPLETE.")
	f.line("")
	f.line("    Every feed() returns the outcome for the bytes so far --")
	f.line("    COMPLETE / INCOMPLETE / INVALID. There is no finalize step: an")
	f.line("    INCOMPLETE tail is retained and continued by the next chunk, and only")
	f.line("    the caller's framing knows whether more can still come.")
	f.line("")
	f.line("    Both refusals are terminal and the corelib latches them: an INVALID")
	f.line("    comes back from every later feed(), and a receiver cap is re-raised.")
	f.line("    So there is no status to remember here -- feed's return is the answer,")
	f.line("    and feeding an empty chunk asks again.")
	f.line(`    """`)
	f.line("")
	if g.anyBind() {
		f.line("    __slots__ = (\"message\", \"_d\", \"_v\")")
	} else {
		f.line("    __slots__ = (\"message\", \"_d\")")
	}
	f.line("")
	f.line("    def __init__(self, msg_cls, vis_cls, reassembly=REASSEMBLY) -> None:")
	f.line("        self.message = msg_cls()")
	if g.anyBind() {
		f.line("        self._v = vis_cls(self.message)")
		f.line("        self._d = Decoder(visitor=self._v, %s,", g.capsArgs())
	} else {
		f.line("        self._d = Decoder(visitor=vis_cls(self.message), %s,", g.capsArgs())
	}
	f.line("                          reassembly=reassembly)")
	f.line("")
	f.line("    def feed(self, chunk) -> Status:")
	if g.anyBind() {
		f.line("        st = self._d.feed(chunk)")
		f.line("        if st is Status.COMPLETE:")
		f.line("            # The fields the destination table carries land on the message")
		f.line("            # here, in one pass, rather than one callback at a time during")
		f.line("            # the walk. Everything the visitor handles is already on it.")
		f.line("            self._v.scatter()")
		f.line("        return st")
	} else {
		f.line("        return self._d.feed(chunk)")
	}
	f.line("")
	f.line("    @property")
	f.line("    def error(self):")
	f.line("        return self._d.error")
	f.blank()
}

// pyFixlenSubtype returns the FixlenSubtype member a fixlen kind must carry, or
// "" for a kind that is not fixlen-framed.
func pyFixlenSubtype(k ir.Kind) string {
	switch k {
	case ir.KindFP32:
		return "FixlenSubtype.FP32"
	case ir.KindFP64:
		return "FixlenSubtype.FP64"
	case ir.KindString:
		return "FixlenSubtype.STRING"
	case ir.KindBlob:
		return "FixlenSubtype.BLOB"
	}
	return ""
}

// pyExpectedWire returns the WireType member a field's header must carry for its
// schema-typed reader to be the right one, mirroring the encode side
// (emitMarshal / marshalArray): unsigned integers, bool and bitfield ->
// UNSIGNED; signed integers and enum -> SIGNED; fp32/fp64, string and blob ->
// FIXLEN; nested messages and composite (wrapper) arrays -> SEQUENCE_START;
// native scalar arrays -> the matching ARRAY_* wire type.
func pyExpectedWire(fld *ir.Field) string {
	switch fld.Kind {
	case ir.KindU8, ir.KindU16, ir.KindU32, ir.KindU64, ir.KindBool, ir.KindBitfield:
		return "WireType.UNSIGNED"
	case ir.KindI8, ir.KindI16, ir.KindI32, ir.KindI64, ir.KindEnum:
		return "WireType.SIGNED"
	case ir.KindFP32, ir.KindFP64, ir.KindString, ir.KindBlob:
		return "WireType.FIXLEN"
	case ir.KindStruct, ir.KindUnion:
		return "WireType.SEQUENCE_START"
	case ir.KindArray:
		if !isNativeArrayElem(fld.Elem) {
			return "WireType.SEQUENCE_START"
		}
		switch fld.Elem {
		case ir.KindI8, ir.KindI16, ir.KindI32, ir.KindI64, ir.KindEnum:
			return "WireType.ARRAY_SIGNED"
		case ir.KindFP32, ir.KindFP64:
			return "WireType.ARRAY_FIXLEN"
		default: // u8/u16/u32/u64, bool, bitfield
			return "WireType.ARRAY_UNSIGNED"
		}
	}
	return "WireType.SEQUENCE_START" // unreachable: keeps the switch total
}

func (g *gen) emitEnum(f *pyfile, nt *ir.NamedType) {
	f.line("class %s(IntEnum):", typeIdent(nt))
	for _, c := range nt.Consts {
		// Sphinx attribute comment(s) carrying the constant's description, above
		// the member so pydoc/Sphinx attaches it to the enum value.
		if c.Description != "" {
			for _, dl := range strings.Split(c.Description, "\n") {
				f.line("    #: %s", dl)
			}
		}
		f.line("    %s = %d", strings.ToUpper(c.Name), c.Value)
	}
	// The name a dataclass default reads the enum through (enumAlias): a class
	// body could have rebound the enum's own name by then.
	f.line("%s = %s", enumAlias(nt), typeIdent(nt))
	f.blank()
}

// emitBitfieldConsts writes the flag constants as an IntFlag, not an IntEnum.
//
// A bitfield's VALUE is a combination -- any subset of the declared flags -- and
// IntEnum admits only the members themselves: `Flags(A | B)` raises ValueError
// for two flags a schema declares side by side, which is the ordinary case
// rather than an edge one. IntFlag composes, so every declared combination and
// the zero value construct. An enum is the opposite shape (one value out of a
// set) and keeps IntEnum, where refusing an undeclared value is correct.
//
// No `boundary=` argument: it arrives in 3.11 and the generated project declares
// requires-python >= 3.9. Rejecting an UNDECLARED bit is the decoder's job in any
// case (MESSAGE_SPEC S1, enforced in on_field), not the type's.
func (g *gen) emitBitfieldConsts(f *pyfile, nt *ir.NamedType) {
	f.line("class %s(IntFlag):", typeIdent(nt))
	for _, fl := range nt.Flags {
		// Sphinx attribute comment(s): the flag description, with the schema
		// default appended as "(default: true/false)" when the flag has one.
		for _, dl := range bitfieldFlagDocLines(fl) {
			f.line("    #: %s", dl)
		}
		f.line("    %s = 1 << %d", strings.ToUpper(fl.Name), fl.Pos)
	}
	f.blank()
}

// bitfieldFlagDocLines builds the Sphinx attribute-comment text (without the
// leading "#: ") for a bitfield flag: its Description, plus a trailing
// "(default: true)" / "(default: false)" note appended to the last line when
// the flag declares a default. Returns nil when there is nothing to document.
func bitfieldFlagDocLines(fl *ir.BitfieldFlag) []string {
	var lines []string
	if fl.Description != "" {
		lines = strings.Split(fl.Description, "\n")
	}
	if fl.HasDefault {
		note := "(default: false)"
		if fl.Default {
			note = "(default: true)"
		}
		if len(lines) == 0 {
			lines = []string{note}
		} else {
			lines[len(lines)-1] = lines[len(lines)-1] + " " + note
		}
	}
	return lines
}

// emitClassDoc writes a triple-quoted class docstring as the first statement of
// the class body when the summary is non-empty. A single-line summary becomes
// """<summary>"""; a multi-line summary opens with """ on the first line and
// closes with """ on its own line. UTF-8 passes through byte-for-byte.
func emitClassDoc(f *pyfile, summary string) {
	if summary == "" {
		return
	}
	lines := strings.Split(summary, "\n")
	if len(lines) == 1 {
		f.line(`    """%s"""`, lines[0])
		return
	}
	f.line(`    """%s`, lines[0])
	for _, ln := range lines[1:] {
		f.line("    %s", ln)
	}
	f.line(`    """`)
}

// pyFieldDocLines builds the Sphinx attribute-comment text (without the leading
// "#: ") from a field's Description and Unit. A multi-line description yields one
// line per source line; a non-empty Unit is appended as " (unit: <Unit>)" to the
// last line. A deprecated field gets a trailing ".. deprecated::" directive so
// Sphinx renders the deprecation and pydoc surfaces the note. Returns nil when
// there is nothing to document.
func pyFieldDocLines(fld *ir.Field) []string {
	var lines []string
	if fld.Description != "" {
		lines = strings.Split(fld.Description, "\n")
	}
	if fld.Unit != "" {
		if len(lines) == 0 {
			lines = []string{fmt.Sprintf("(unit: %s)", fld.Unit)}
		} else {
			lines[len(lines)-1] = fmt.Sprintf("%s (unit: %s)", lines[len(lines)-1], fld.Unit)
		}
	}
	if note := generator.BoundNote(fld, generator.StorageDynamic); note != "" {
		lines = append(lines, note)
	}
	if fld.Deprecated {
		lines = append(lines,
			".. deprecated::",
			"   Deprecated and retained only for backward compatibility; do not use in new code.")
	}
	return lines
}

// emitDataclass writes one generated class. `u` is non-nil for a union type,
// whose fields are its options and which holds exactly one of them (union.go).
func (g *gen) emitDataclass(f *pyfile, name, summary string, fields []*ir.Field, u *unionShape) {
	g.defBase, g.defs = g.bases[name], nil
	g.emitDataclassBody(f, name, summary, fields, u)
	// The constants follow the class: its methods read them only when called.
	for _, d := range g.defs {
		f.line("%s = FloatArrayDefault(%s)", d.name, d.lit)
		f.blank()
	}
	g.defs = nil
}

func (g *gen) emitDataclassBody(f *pyfile, name, summary string, fields []*ir.Field, u *unionShape) {
	f.line("@dataclass")
	f.line("class %s:", name)
	// Class docstring (pydoc/Sphinx) as the first statement in the body, when the
	// summary is non-empty. It also satisfies the body for a field-less class.
	emitClassDoc(f, summary)
	if u != nil {
		g.emitUnionFields(f, u)
	} else if len(fields) == 0 && summary == "" {
		f.line("    pass")
	}
	for _, fld := range fields {
		if u != nil {
			break
		}
		// Sphinx attribute comment(s) immediately before the declaration.
		for _, dl := range pyFieldDocLines(fld) {
			f.line("    #: %s", dl)
		}
		f.line("    %s: %s = %s", pyIdent(fld.Name), g.pyAnnot(fld), g.pyDefault(fld))
	}
	f.blank()

	// Worst-case encoded size. It is what sizes the buffer encode() hands the
	// encoder: the corelib is given storage and never grows or reallocates it
	// (CORELIB_PLAN §5.1), so the number has to come from the schema, here.
	//
	// Deliberately unannotated: an annotated class attribute in a @dataclass
	// becomes a FIELD, which would put MAX_SIZE on the wire and in __init__.
	ms := g.messageSize(name, fields, u != nil)
	if ms.Bounded {
		f.line("    # Worst-case encoded size, derived from the schema: no value of this")
		f.line("    # class can encode to more, which is why encode() can size one exact")
		f.line("    # buffer from it.")
		f.line("    MAX_SIZE = %d", ms.Size)
	} else {
		f.line("    # Configured ceiling (max_message_size), NOT a size this class cannot")
		f.line("    # exceed: a field of it is unbounded, so the schema supplies no worst")
		f.line("    # case and encode() must not size a buffer from this number.")
		f.line("    MAX_SIZE_LIMIT = %d", ms.Size)
		f.line("    MAX_SIZE = MAX_SIZE_LIMIT")
	}
	f.blank()

	if u != nil {
		g.emitUnionAccessors(f, u)
		g.emitUnionIsDefault(f, u)
		g.emitUnionSerialize(f, u)
		g.emitUnionJSON(f, u)
		g.emitCodec(f, name, ms)
		return
	}

	g.emitIsDefault(f, fields)

	// _marshal
	f.line("    def serialize(self, e: Encoder) -> None:")
	if len(fields) == 0 {
		f.line("        pass")
	}
	for _, fld := range fields {
		g.emitMarshal(f, fld)
	}
	f.blank()

	// Decode lives in the flat visitor emitted for this class (visitor.go);
	// §7.3's "skip a contradicting field" needs no code at all there, because a
	// field whose wire type contradicts the schema is delivered to a DIFFERENT
	// typed hook, where its (location, id) matches no arm and it falls through.

	// JSON helpers + encode/decode
	g.emitJSON(f, name, fields, ms)
}

// emitIsDefault emits the object's all-default predicate. It is the exact
// negation of what _marshal writes: the object is default iff _marshal would
// emit no child at all, evaluated per field and recursively (MESSAGE_SPEC §2).
// Keep this in lockstep with emitMarshal -- both are generated from
// fieldIsDefaultExpr's per-field expressions for exactly that reason. A predicate
// that disagrees with the writer omits a field that is on the wire, or keeps one
// that is not.
func (g *gen) emitIsDefault(f *pyfile, fields []*ir.Field) {
	f.line("    def _is_default(self) -> bool:")
	if len(fields) == 0 {
		f.line("        return True")
		f.blank()
		return
	}
	for _, fld := range fields {
		f.line("        if not (%s):", g.fieldIsDefaultExpr(fld))
		f.line("            return False")
	}
	f.line("        return True")
	f.blank()
}

// fieldIsDefaultExpr is the boolean expression "this field equals its default",
// i.e. the negation of emitMarshal's write guard for the same field.
func (g *gen) fieldIsDefaultExpr(fld *ir.Field) string {
	return g.fieldIsDefaultExprAt(fld, "self."+pyIdent(fld.Name))
}

// fieldIsDefaultExprAt is fieldIsDefaultExpr over the value `acc` names -- a
// union's default option tests its held `_value` with the same predicate a field
// of that kind uses.
func (g *gen) fieldIsDefaultExprAt(fld *ir.Field, acc string) string {
	switch fld.Kind {
	case ir.KindBlob:
		// emitMarshal compares bytes(acc) so a bytearray/memoryview value still
		// matches the literal default.
		return fmt.Sprintf("bytes(%s) == %s", acc, g.pyDefault(fld))
	case ir.KindStruct, ir.KindUnion:
		// Lazily framed: the frame survives iff the nested _marshal wrote a child,
		// which is exactly "the nested object is not default".
		return fmt.Sprintf("%s._is_default()", acc)
	case ir.KindArray:
		return g.arrayIsDefaultExpr(fld, acc)
	}
	if cmp, ok := floatZeroCmp(fld, acc, false); ok {
		return cmp
	}
	return fmt.Sprintf("%s == %s", acc, g.pyDefault(fld))
}

// floatZeroCmp is the default test of a float scalar whose default is a zero,
// by BIT PATTERN (CORELIB_PLAN §4.6): `-0.0 == 0.0`, so an `==` / `!=` alone
// would drop a -0.0 at a +0.0 default. The sign is read with math.copysign, so
// the common non-zero value costs the one compare it cost before. differs
// selects the write guard (the negation). ok is false for every other field,
// whose default is not a zero: there `==` already tells every value apart.
func floatZeroCmp(fld *ir.Field, acc string, differs bool) (string, bool) {
	if fld.Kind != ir.KindFP32 && fld.Kind != ir.KindFP64 {
		return "", false
	}
	neg := false
	switch v := fld.Default.(type) {
	case nil:
	case float64:
		if v != 0 {
			return "", false
		}
		neg = math.Signbit(v)
	case int, int64, uint64:
		if fmt.Sprint(v) != "0" {
			return "", false
		}
	default:
		return "", false
	}
	// The sign the DEFAULT carries: a value is the default iff it is a zero of it.
	same, other := ">", "<"
	if neg {
		same, other = "<", ">"
	}
	if differs {
		return fmt.Sprintf("%s != 0.0 or math.copysign(1.0, %s) %s 0.0", acc, acc, other), true
	}
	return fmt.Sprintf("%s == 0.0 and math.copysign(1.0, %s) %s 0.0", acc, acc, same), true
}

// arrayEqExpr compares a native array with its default literal lit. A float
// array is compared by BIT PATTERN (CORELIB_PLAN §4.6): `[-0.0, 1.5] == [0.0, 1.5]`
// is true in Python, which would drop the sign of the zero. Its default is held
// by the corelib's FloatArrayDefault, built once per field (floatArrayDefault),
// which decides how that one default is told apart; the compare is its
// matches(). differs selects the write guard (the negation).
func (g *gen) arrayEqExpr(fld *ir.Field, acc, lit string, differs bool) string {
	if fld.Elem == ir.KindFP32 || fld.Elem == ir.KindFP64 {
		def := g.floatArrayDefault(fld, lit)
		if differs {
			return fmt.Sprintf("not %s.matches(%s)", def, acc)
		}
		return fmt.Sprintf("%s.matches(%s)", def, acc)
	}
	if differs {
		return fmt.Sprintf("%s != %s", acc, lit)
	}
	return fmt.Sprintf("%s == %s", acc, lit)
}

// arrayIsDefaultExpr mirrors emitMarshalArray. An array's declared `count: N` is
// a CAPACITY, never a length (MESSAGE_SPEC §3), so it takes no part in this test:
// the value is compared against the declared default exactly as written, with no
// padding to N on either side, and against the empty collection when none is
// declared. A count:N array is therefore default only when it is EMPTY -- an
// all-zero N-element value is a length-N array, which differs from the empty one
// and stays on the wire.
func (g *gen) arrayIsDefaultExpr(fld *ir.Field, acc string) string {
	if isNativeArrayElem(fld.Elem) {
		if lit, ok := g.pyNativeArrayDefault(fld); ok {
			return g.arrayEqExpr(fld, acc, lit, false)
		}
		return fmt.Sprintf("len(%s) == 0", acc)
	}
	// Wrapper array: the writer emits a child for every element it holds, because
	// the LAST element is written whatever its value (§2) -- so "no child is
	// written" is exactly "the array is empty", and the two cannot drift apart.
	return fmt.Sprintf("len(%s) == 0", acc)
}

func (g *gen) emitMarshal(f *pyfile, fld *ir.Field) {
	g.emitMarshalAt(f, fld, "self."+pyIdent(fld.Name), "        ", false)
}

// emitMarshalAt writes the value `acc` names as field `fld`, at indent `ind`.
//
// `forced` is the write of a held union option other than default_id
// (MESSAGE_SPEC §2, §4.2): the option's presence is what says which one is held,
// so it is written whatever its value -- a scalar, string or blob with no
// ≠-default guard (the empty value is a zero-length payload), a native array as
// its count even when empty, and a struct, union or wrapper array as a present
// frame, closed with write_sequence_end_keep. Inside the option the ordinary
// per-field omission still applies.
func (g *gen) emitMarshalAt(f *pyfile, fld *ir.Field, acc, ind string, forced bool) {
	var write string
	switch fld.Kind {
	case ir.KindU8, ir.KindU16, ir.KindU32, ir.KindU64, ir.KindBitfield:
		write = fmt.Sprintf("e.%s(%d, int(%s))", scalarWriter("write_unsigned", fld.Kind, fld.Ref), fld.ID, acc)
	case ir.KindI8, ir.KindI16, ir.KindI32, ir.KindI64, ir.KindEnum:
		write = fmt.Sprintf("e.%s(%d, int(%s))", scalarWriter("write_signed", fld.Kind, fld.Ref), fld.ID, acc)
	case ir.KindBool:
		write = fmt.Sprintf("e.write_bool(%d, %s)", fld.ID, acc)
	case ir.KindFP32:
		write = fmt.Sprintf("e.write_float32(%d, %s)", fld.ID, acc)
	case ir.KindFP64:
		write = fmt.Sprintf("e.write_float64(%d, %s)", fld.ID, acc)
	case ir.KindString:
		write = "e." + writeCall("write_string", []string{fmt.Sprint(fld.ID), acc}, boundLit(fld.HasMaxlen, fld.Maxlen))
	case ir.KindBlob:
		write = "e." + writeCall("write_bytes", []string{fmt.Sprint(fld.ID), "bytes(" + acc + ")"}, boundLit(fld.HasMaxlen, fld.Maxlen))
		if forced {
			break
		}
		// blob is a leaf: omit when equal to its default (empty if none).
		f.line("%sif bytes(%s) != %s:", ind, acc, g.pyDefault(fld))
		f.line("%s    %s", ind, write)
		return
	case ir.KindStruct, ir.KindUnion:
		// MESSAGE_SPEC S2: the != default test is per field and a sequence is no
		// exception, so the frame is opened LAZILY -- the corelib holds the header
		// back until a child field appears. The nested marshal omits each child that
		// equals its default, so "no child was written" IS "the object equals its
		// declared default", evaluated per field and recursively. An all-default
		// nested object is therefore dropped, not emitted as an empty wrapper.
		//
		// A forced option keeps its frame even when it wrote nothing: an empty
		// frame is what selects the option at its own default.
		f.line("%se.write_sequence_begin_lazy(%d)", ind, fld.ID)
		f.line("%s%s.serialize(e)", ind, acc)
		if forced {
			f.line("%se.write_sequence_end_keep()", ind)
		} else {
			f.line("%se.write_sequence_end()", ind)
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
	if cmp, ok := floatZeroCmp(fld, acc, true); ok {
		f.line("%sif %s:", ind, cmp)
	} else {
		f.line("%sif %s != %s:", ind, acc, g.pyDefault(fld))
	}
	f.line("%s    %s", ind, write)
}

func (g *gen) emitMarshalArray(f *pyfile, fld *ir.Field, acc, ind string, forced bool) {
	// A native scalar array is a leaf field: omit it when equal to its default
	// (materialized in the dataclass), else when empty. A composite/dynamic-element
	// array is a wrapper sequence, opened lazily and closed by the dropping end
	// (see marshalArray) -- an empty one vanishes instead of being framed empty.
	//
	// A declared `count: N` takes no part in either test. `count` is a CAPACITY,
	// never a length (§3): it never reaches the wire, so the value is compared
	// against the declared default exactly as written -- neither side padded to N
	// -- and against the empty collection when no default is declared.
	//
	// A forced union option drops both guards: a native array is written as its
	// count even when empty (an fp array keeps its fixlen_word), and a wrapper
	// closes with the keeping end, so the empty frame survives.
	id := fmt.Sprintf("%d", fld.ID)
	b := arrBound{fld.Name, fld.HasCount, fld.Count, fld.ElemMaxHas, fld.ElemMax}
	if isNativeArrayElem(fld.Elem) {
		if forced {
			g.marshalArray(f, ind, id, acc, fld.Elem, fld.ElemRef, fld.ElemItems, b, 0, "")
			return
		}
		if lit, ok := g.pyNativeArrayDefault(fld); ok {
			f.line("%sif %s:", ind, g.arrayEqExpr(fld, acc, lit, true))
		} else {
			f.line("%sif len(%s) != 0:", ind, acc)
		}
		g.marshalArray(f, ind+"    ", id, acc, fld.Elem, fld.ElemRef, fld.ElemItems, b, 0, "")
		return
	}
	if forced {
		g.marshalArray(f, ind, id, acc, fld.Elem, fld.ElemRef, fld.ElemItems, b, 0, keepAlways)
		return
	}
	// The field-level wrapper frame is dropped when no element is written, and
	// absence then reconstructs the field's default. That is correct because a
	// wrapper array's declared `default` is not materialized today -- the dataclass
	// default is the empty list -- so absent and explicitly-empty denote the same
	// value. If that gap is ever closed, this call needs a guard --
	// `if value != default: ... e.write_sequence_end_keep()` -- so that a value
	// differing from a non-empty default still reaches the wire as the empty
	// wrapper, the only encoding of "explicitly empty" (MESSAGE_SPEC §2, §3).
	g.marshalArray(f, ind, id, acc, fld.Elem, fld.ElemRef, fld.ElemItems, b, 0, "")
}

// lastElemExpr is the "this element is the array's last" test, at loop position
// iv over a value whose length the local n holds (marshalArray binds it once,
// before the loop, rather than calling len() per element).
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
func lastElemExpr(iv, n string) string {
	return fmt.Sprintf("%s == %s - 1", iv, n)
}

// keepAlways is the emitSeqEnd condition that keeps the frame unconditionally.
const keepAlways = "True"

// emitSeqEnd closes the wrapper sequence opened at ind, choosing between the two
// closers the corelib offers. Every sequence is opened LAZILY (the corelib holds
// the header back until a child is written), so the closer alone decides whether
// a contentless one survives: write_sequence_end drops it, write_sequence_end_keep
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
//   - keepAlways -- always: a held union option other than default_id, whose
//     frame is what selects it (MESSAGE_SPEC §2).
func emitSeqEnd(f *pyfile, ind, keepIf string) {
	switch keepIf {
	case "":
		f.line("%se.write_sequence_end()", ind)
		return
	case keepAlways:
		f.line("%se.write_sequence_end_keep()", ind)
		return
	}
	f.line("%sif %s:", ind, keepIf)
	f.line("%s    e.write_sequence_end_keep()", ind)
	f.line("%selse:", ind)
	f.line("%s    e.write_sequence_end()", ind)
}

// marshalArray writes the array `val` as field `idExpr`. Numeric/enum/boolean/
// bitfield elements use the native array wire type (enum->signed, bool/bitfield->
// unsigned); string/blob/struct/union/array elements lower to a wrapper sequence
// whose child ids are the 0-based index (per MESSAGE_SPEC). Recurses for nested
// arrays.
//
// Every element the value holds is written -- no trailing run is elided, of
// either element kind, because the wire count IS the array's length (§3) and the
// highest wrapper id IS its last index (§5.1). What the interior may drop is a
// value that is indistinguishable from absence, and only that.
//
// keepIf is the closer this call's own wrapper takes (see emitSeqEnd); the native
// element kinds open no sequence and ignore it.
//
// b is the schema bound of `val` itself: a native array hands its count to the
// corelib writer as `cap`, a wrapper array checks it here before the frame
// opens, and a string/blob element hands its maxlen to its own write.
func (g *gen) marshalArray(f *pyfile, ind, idExpr, val string, elem ir.Kind, ref *ir.TypeRef, items *ir.ArrayElem, b arrBound, depth int, keepIf string) {
	iv := fmt.Sprintf("_i%d", depth)
	ev := fmt.Sprintf("_e%d", depth)
	capLit := boundLit(b.hasCount, b.count)
	// A wrapper array's length, bound once: the loop's last-element test reads
	// it, and so does the count guard -- a wrapper array has no writer that takes
	// it whole, so its count is checked here, before the frame opens.
	nv := fmt.Sprintf("_n%d", depth)
	if !isNativeArrayElem(elem) {
		f.line("%s%s = len(%s)", ind, nv, val)
		if b.hasCount {
			f.lines(ind, encodeGuard(fmt.Sprintf("%s > %d", nv, b.count),
				fmt.Sprintf("%s: array over count %d", b.loc, b.count)))
		}
	}
	// A native integer array's element width is its writer's name and the
	// schema count rides the call as `cap`: the writer range-checks every
	// element anyway (intArrayWrite).
	switch elem {
	case ir.KindU8, ir.KindU16, ir.KindU32, ir.KindU64:
		f.line("%se.%s", ind, intArrayWrite("write_unsigned_array", elem, ref, idExpr, val, b))
	case ir.KindI8, ir.KindI16, ir.KindI32, ir.KindI64:
		f.line("%se.%s", ind, intArrayWrite("write_signed_array", elem, ref, idExpr, val, b))
	case ir.KindEnum:
		f.line("%se.%s", ind, intArrayWrite("write_signed_array", elem, ref, idExpr, "[int(_v) for _v in "+val+"]", b))
	case ir.KindBool:
		// The corelib's own canonical writer (corelib-py#158): it tests each
		// element for truth exactly as `if` would and emits 1/0, which is what the
		// intermediate list here used to build. Byte-identical, on both engines.
		// A boolean has no width (§4.4): the cap is its only bound.
		f.line("%se.%s", ind, writeCall("write_bool_array", []string{idExpr, val}, capLit))
	case ir.KindBitfield:
		f.line("%se.%s", ind, intArrayWrite("write_unsigned_array", elem, ref, idExpr, "[int(_v) for _v in "+val+"]", b))
	case ir.KindFP32:
		f.line("%se.%s", ind, writeCall("write_float32_array", []string{idExpr, val}, capLit))
	case ir.KindFP64:
		f.line("%se.%s", ind, writeCall("write_float64_array", []string{idExpr, val}, capLit))
	case ir.KindString:
		// A string element is a leaf: in the array's INTERIOR it is omitted when it
		// equals the element default (empty), leaving an id gap the decoder restores
		// from that same default -- the ordinary sparse-field rule of MESSAGE_SPEC
		// §2, applied to an element. At the LAST index it is written whatever its
		// value: see lastElemExpr.
		f.line("%se.write_sequence_begin_lazy(%s)", ind, idExpr)
		f.line("%sfor %s, %s in enumerate(%s):", ind, iv, ev, val)
		f.line(`%s    if %s != "" or %s:`, ind, ev, lastElemExpr(iv, nv))
		f.line("%s        e.%s", ind, writeCall("write_string", []string{iv, ev}, boundLit(b.hasElemMax, b.elemMax)))
		emitSeqEnd(f, ind, keepIf)
	case ir.KindBlob:
		// A blob element is a leaf, exactly like the string element above.
		f.line("%se.write_sequence_begin_lazy(%s)", ind, idExpr)
		f.line("%sfor %s, %s in enumerate(%s):", ind, iv, ev, val)
		f.line("%s    if len(%s) != 0 or %s:", ind, ev, lastElemExpr(iv, nv))
		f.line("%s        e.%s", ind, writeCall("write_bytes", []string{iv, "bytes(" + ev + ")"}, boundLit(b.hasElemMax, b.elemMax)))
		emitSeqEnd(f, ind, keepIf)
	case ir.KindStruct, ir.KindUnion:
		// A sequence-form element obeys the SAME rule as the leaf elements above --
		// one rule for both kinds -- and the lazily-held frame is where it is
		// applied. The nested _marshal writes no child exactly when the element
		// equals its declared default, so the CLOSER alone decides: the dropping one
		// in the interior, where an all-default element vanishes into an id gap; the
		// keeping one at the last index, where it survives as an empty frame because
		// that presence is what fixes the array's length.
		f.line("%se.write_sequence_begin_lazy(%s)", ind, idExpr)
		f.line("%sfor %s, %s in enumerate(%s):", ind, iv, ev, val)
		f.line("%s    e.write_sequence_begin_lazy(%s)", ind, iv)
		f.line("%s    %s.serialize(e)", ind, ev)
		emitSeqEnd(f, ind+"    ", lastElemExpr(iv, nv))
		emitSeqEnd(f, ind, keepIf)
	case ir.KindArray:
		f.line("%se.write_sequence_begin_lazy(%s)", ind, idExpr)
		f.line("%sfor %s, %s in enumerate(%s):", ind, iv, ev, val)
		if isNativeArrayElem(items.Elem) {
			// A native row is a single count-prefixed value with no frame of its own,
			// so the rule lands on the WRITE rather than on a closer: an interior row
			// equal to the element default (the empty row) is not written at all, and
			// the last row always is.
			f.line("%s    if len(%s) != 0 or %s:", ind, ev, lastElemExpr(iv, nv))
			g.marshalArray(f, ind+"        ", iv, ev, items.Elem, items.ElemRef, items.ElemItems, rowBound(b.loc, items), depth+1, "")
		} else {
			// A wrapper row has its own frame, so it takes the closer instead -- the
			// same interior/last choice, expressed the same way as for a struct
			// element above.
			g.marshalArray(f, ind+"    ", iv, ev, items.Elem, items.ElemRef, items.ElemItems, rowBound(b.loc, items), depth+1, lastElemExpr(iv, nv))
		}
		emitSeqEnd(f, ind, keepIf)
	}
}

// arrBound is the schema bound of one array value: the field's own, or a row's
// (rowBound). loc names the field in the refusal text.
type arrBound struct {
	loc        string
	hasCount   bool
	count      int64
	hasElemMax bool
	elemMax    int64
}

// rowBound is the bound of an array-of-arrays row: the inner declaration's.
func rowBound(loc string, items *ir.ArrayElem) arrBound {
	return arrBound{loc, items.HasCount, items.Count, items.ElemMaxHas, items.ElemMax}
}

// Encode-side bounds (ARCHITECTURE §9.6). A value past its schema bound is
// refused with SofaArgumentError -- the corelib's InvalidArgument (CORELIB_PLAN
// §6.3), the code it already raises for a value the encoder cannot write -- and
// encode() returns nothing: the refusal raises out of serialize().
//
// The bound is the schema literal, emitted per field; the corelib holds none.
// A schema maxlen or count is a number only the schema knows, so it rides the
// call of the writer that already measures the value (writeCall):
//
//   - write_string_bounded(id, s, maxlen): the UTF-8 byte length exists only
//     inside the call, so this is the one place the bound can be checked
//     without encoding the string twice;
//   - write_bytes_bounded(id, b, maxlen): the length the writer takes anyway;
//   - write_{bool,float32,float64}_array_bounded(id, vals, cap).
//
// A declared integer width is a TYPE, so it is in the writer's name instead:
// write_u8 .. write_i32 for a narrow integer, enum or bitfield (scalarWriter;
// Python's unbounded int carries no width), and write_u8_array .. write_i64_array
// (id, vals, cap) for an integer array (intArrayWrite), whose writer checks every
// element in the 64-bit range check it already runs. Measured (tests/bench,
// vehicle_telemetry encode, native engine): the width as an argument cost +3.7%
// Ir/op, in the name +1.3%; the same checks as generated `if ...: raise`
// statements +8.7%. Every extra argument of a native writer is paid on every
// call.
//
// A field with no bound calls the plain writer, whose signature carries none,
// so an unbounded write costs exactly what it did. A wrapper array (strings,
// blobs, structs, rows) has no writer that takes it whole, so its count is the
// one generated guard (encodeGuard), on the length its element loop binds once
// anyway (lastElemExpr).

// encodeGuard renders `if cond: raise SofaArgumentError(msg)`.
func encodeGuard(cond, msg string) []string {
	return []string{
		fmt.Sprintf("if %s:", cond),
		fmt.Sprintf("    raise SofaArgumentError(%q)", msg),
	}
}

// writeCall renders a corelib write: the plain writer `name(args)` where every
// bound is None, else its `name_bounded(args, bounds)` twin. A bound is a Python
// literal, "None" for a side the schema leaves unchecked.
func writeCall(name string, args []string, bounds ...string) string {
	for _, b := range bounds {
		if b != "None" {
			return fmt.Sprintf("%s_bounded(%s)", name, strings.Join(append(args, bounds...), ", "))
		}
	}
	return fmt.Sprintf("%s(%s)", name, strings.Join(args, ", "))
}

// boundLit is a maxlen or count as a writer's bound literal, "None" where the
// schema states none.
func boundLit(has bool, n int64) string {
	if !has {
		return "None"
	}
	return fmt.Sprint(n)
}

// typedWidth names the width-typed writer of the declared width of kind k --
// "u8", "u16", "u32", "i8", "i16", "i32", or for an array element also "u64" /
// "i64" -- from the (min, max) declaredWidth gives: an enum's implied signed
// width and a bitfield's implied unsigned width are one of these by
// construction. ok is false where the width is the 64-bit range itself.
func typedWidth(k ir.Kind, ref *ir.TypeRef) (string, bool) {
	lo, hi := widthBounds(k, ref)
	switch [2]string{lo, hi} {
	case [2]string{"None", "255"}:
		return "u8", true
	case [2]string{"None", "65535"}:
		return "u16", true
	case [2]string{"None", "4294967295"}:
		return "u32", true
	case [2]string{"-128", "127"}:
		return "i8", true
	case [2]string{"-32768", "32767"}:
		return "i16", true
	case [2]string{"-2147483648", "2147483647"}:
		return "i32", true
	}
	return "", false
}

// scalarWriter is the writer of a narrow integer, enum or bitfield scalar: the
// width-typed one where the declared width is narrower than 64 bits, else the
// plain writer, which already checks the 64-bit range.
func scalarWriter(plain string, k ir.Kind, ref *ir.TypeRef) string {
	if w, ok := typedWidth(k, ref); ok {
		return "write_" + w
	}
	return plain
}

// intArrayWrite renders the write of a native integer, enum or bitfield array:
// write_<width>_array(id, vals, cap), cap the schema count or -1. A 64-bit
// element takes write_u64_array/write_i64_array when the array has a count, and
// the plain writer (no bound at all) when it has none.
func intArrayWrite(plain string, elem ir.Kind, ref *ir.TypeRef, idExpr, val string, b arrBound) string {
	w, ok := typedWidth(elem, ref)
	if !ok {
		if !b.hasCount {
			return fmt.Sprintf("%s(%s, %s)", plain, idExpr, val)
		}
		w = "u64"
		if plain == "write_signed_array" {
			w = "i64"
		}
	}
	return fmt.Sprintf("write_%s_array(%s, %s, %d)", w, idExpr, val, capOf(b.hasCount, b.count))
}

// widthBounds is declaredWidth as (min, max) literals, "None" for a side the
// width leaves at the 64-bit range.
func widthBounds(k ir.Kind, ref *ir.TypeRef) (lo, hi string) {
	min, max, ok := declaredWidth(k, ref)
	if !ok {
		return "None", "None"
	}
	if min == "" {
		min = "None"
	}
	return min, max
}

// capOf maps a schema fixed-count bound to a wrapper array's cap: N when the
// array declares a count, -1 (dynamic/unbounded) otherwise.
func capOf(hasCount bool, count int64) int64 {
	if hasCount {
		return count
	}
	return -1
}

// floatArrayDefault names the module-level constant holding a float array's
// declared default: `_` + class + `__Def__` + field (the `__Loc` / `__Bind`
// naming rule of visitor.go, role word `Def`), registered for emission after the
// class. Built once at import, it is what every omission test of the field
// compares against, instead of a list literal Python would rebuild per call.
func (g *gen) floatArrayDefault(fld *ir.Field, lit string) string {
	name := "_" + g.defBase + "__Def__" + pyIdent(fld.Name)
	for _, d := range g.defs {
		if d.name == name {
			return name
		}
	}
	g.defs = append(g.defs, pyDefConst{name, lit})
	return name
}

// arrayDefaultValue is a fresh list holding a native array's declared default.
// A float array's is a copy of its FloatArrayDefault's values: a field at its
// default then holds the very float objects its omission test compares against,
// so the list compare settles each element by identity, and every element is a
// float even where the schema spells a whole number. Any other array is its
// literal.
func (g *gen) arrayDefaultValue(fld *ir.Field, lit string) string {
	if fld.Elem != ir.KindFP32 && fld.Elem != ir.KindFP64 {
		return lit
	}
	return "[*" + g.floatArrayDefault(fld, lit) + ".values]"
}
