// Package rust is the Rust backend (PLAN §6.2, embedded/no_std-capable): structs
// with serialize() over OStream and a flat-visitor decode. The corelib's Visitor
// is flat (sequence_begin/end events, no child visitors), so decode is a
// (location, id) state machine with a location stack — every assignment targets
// self.m.<path> directly, which keeps the borrow checker happy.
//
// The `corelib` config key selects the Rust corelib: "rs-no-std" (default,
// corelib-rs-no-std — #![no_std], heap-free, wire types behind Cargo features;
// the generated decoder enables the full wire-type set so it can §7.3-skip any
// wire type regardless of the schema (generator#215), with a require!() guard
// asserting them) or "rs" (corelib-rs — std, high-throughput, every wire type
// always compiled in, so no features and no require! guard).
// Both expose the same sofab:: interface and produce identical wire bytes.
package rust

import (
	"fmt"
	"strings"

	"github.com/sofa-buffers/generator/internal/generator"
	"github.com/sofa-buffers/generator/internal/ir"
)

func init() { generator.Register(&Backend{}) }

// Backend implements generator.Backend for Rust.
type Backend struct{}

func (*Backend) Lang() string { return "rust" }

// Generate emits src/message.rs; project mode adds Cargo.toml + a serde-json
// harness.
func (*Backend) Generate(s *ir.Schema, cfg map[string]any) ([]generator.File, error) {
	corelib := cfgString(cfg, "corelib", "rs")
	// The no_std/heap-free profile is the point of corelib-rs-no-std, so it is on by
	// default there (opt out with no_std: false); it is never on for the std corelib.
	noStd := corelib == "rs-no-std" && cfgBoolDefault(cfg, "no_std", true)
	g := &gen{
		schema:       s,
		banner:       cfgString(cfg, "tool_banner", "sofabgen"),
		license:      generator.LicenseID(cfg),
		corelib:      corelib,
		noStd:        noStd,
		allowDynamic: cfgBool(cfg, "allow_dynamic"),
		size:         generator.NewSizePolicy(cfg),
	}
	// Receiver-side decode limits (generator#102) apply only to the std
	// corelib-rs: corelib-rs-no-std has no Error::LimitExceeded, and its heapless
	// storage is statically schema-bounded anyway, so the keys are inert there.
	if g.std() {
		g.limits = resolveLimits(s, cfg)
	}
	if noStd {
		// The heap-free profile lowers every field to fixed-capacity heapless
		// storage sized from the schema; a field with no maxlen/count cannot be
		// sized, so reject it (unless allow_dynamic keeps a heap fallback).
		if err := g.checkBounded(s); err != nil {
			return nil, err
		}
	}
	// Which types need the all-default predicate a `count: N` wrapper array uses to
	// find M, and whether the module needs the narrowing helper at all
	// (generator#248). Resolved before emit so both the writer and the predicate
	// read the same answer.
	g.scanTrim(s)
	files := []generator.File{{Path: "src/message.rs", Content: g.module(s)}}
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
	// corelib selects the Rust corelib: "rs" (default, corelib-rs — std,
	// high-throughput, every wire type always compiled in, no feature flags and no
	// require! capability guard) or "rs-no-std" (corelib-rs-no-std — #![no_std],
	// heap-free, Cargo feature flags to shrink the binary).
	corelib string
	// noStd is the genuinely heap-free profile (corelib-rs-no-std + no_std): emit a
	// #![no_std] lib crate, heapless::String<N>/Vec<T,N> fixed-capacity fields sized
	// from the schema, a bounded decode stack, and serde gated behind a cargo
	// feature. When false the crate is ordinary std (String/Vec/serde), even against
	// the no-std corelib (a std consumer can still link it).
	noStd bool
	// allowDynamic keeps an alloc::String/alloc::Vec heap fallback for genuinely
	// unbounded fields (no maxlen/count) instead of failing generation — the Rust
	// analog of the C++ c-cpp allow_dynamic. Bounded fields still go heapless.
	allowDynamic bool
	// limits are the receiver-side decode limits (generator#102); resolved only
	// for the std corelib-rs (empty — all inert — under corelib-rs-no-std).
	limits limitSet
	// size is the max_message_size policy; sizeErr carries a violation out of
	// the emit path, which has no error channel of its own.
	size    generator.SizePolicy
	sizeErr error
	// trimSeq marks that the schema has at least one `count: N` wrapper array, the
	// only construct that needs the _trim_seq narrowing helper (generator#248).
	trimSeq bool
	// isDefault is the set of generated type names (by rendered Rust name) that
	// must carry is_default(): the element type of a `count: N` struct/union array
	// and, transitively, every struct/union it holds as a field. Nothing else pays
	// for the predicate — it would be dead weight in a footprint build.
	isDefault map[string]bool
}

// scanTrim resolves trimSeq and isDefault for the whole schema. It walks exactly
// the shapes emitSerializeArray narrows, so a type gets the predicate iff some
// writer loop asks for it.
func (g *gen) scanTrim(s *ir.Schema) {
	g.isDefault = map[string]bool{}
	seen := map[string]bool{}
	var scan func(fields []*ir.Field)
	var scanElem func(elem ir.Kind, ref *ir.TypeRef, items *ir.ArrayElem)
	scan = func(fields []*ir.Field) {
		for _, fld := range fields {
			switch {
			case fld.Kind == ir.KindStruct || fld.Kind == ir.KindUnion:
				if !seen[fld.Ref.Key] {
					seen[fld.Ref.Key] = true
					scan(fld.Ref.Target.Fields)
				}
			case fld.Kind == ir.KindArray && isWrapperElem(fld.Elem):
				// Only a declared `count: N` array is narrowed; a dynamic one keeps
				// every element, so it needs no predicate.
				if fld.HasCount && (fld.Elem == ir.KindString || fld.Elem == ir.KindBlob) {
					g.trimSeq = true
				}
				if fld.HasCount && (fld.Elem == ir.KindStruct || fld.Elem == ir.KindUnion) {
					g.trimSeq = true
					g.markIsDefault(fld.ElemRef.Key, fld.ElemRef.Target.Fields)
				}
				scanElem(fld.Elem, fld.ElemRef, fld.ElemItems)
			}
		}
	}
	scanElem = func(elem ir.Kind, ref *ir.TypeRef, items *ir.ArrayElem) {
		switch elem {
		case ir.KindStruct, ir.KindUnion:
			if !seen[ref.Key] {
				seen[ref.Key] = true
				scan(ref.Target.Fields)
			}
		case ir.KindArray:
			scanElem(items.Elem, items.ElemRef, items.ElemItems)
		}
	}
	for _, m := range s.Messages {
		scan(m.Fields)
	}
}

// markIsDefault records that a type needs is_default(), and recurses into the
// struct/union fields the predicate itself calls it on.
func (g *gen) markIsDefault(key string, fields []*ir.Field) {
	name := g.typeName(key)
	if g.isDefault[name] {
		return
	}
	g.isDefault[name] = true
	for _, fld := range fields {
		switch {
		case fld.Kind == ir.KindStruct || fld.Kind == ir.KindUnion:
			g.markIsDefault(fld.Ref.Key, fld.Ref.Target.Fields)
		case fld.Kind == ir.KindArray && fld.HasCount &&
			(fld.Elem == ir.KindStruct || fld.Elem == ir.KindUnion):
			// The field's own narrowing predicate calls the element's is_default.
			g.markIsDefault(fld.ElemRef.Key, fld.ElemRef.Target.Fields)
		}
	}
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
// resolved against the schema. An entry is active only when its max_dyn_* key
// is configured AND the schema actually has an unbounded field of that kind —
// otherwise the option would be inert and no limit plumbing is emitted. The
// guards are per-field on unbounded fields only (a schema-bounded field keeps
// its own generator#100 schema guard instead), so the configured value is
// emitted as-is, with no raise to the largest schema bound.
type limitSet struct {
	arrayCount, stringLen, blobLen int64
	arrayHas, stringHas, blobHas   bool
}

func (l limitSet) any() bool { return l.arrayHas || l.stringHas || l.blobHas }

// resolveLimits reads the max_dyn_* config keys and resolves them against the
// schema's bounds (see limitSet).
func resolveLimits(s *ir.Schema, cfg map[string]any) limitSet {
	var all []*ir.Field
	for _, m := range s.Messages {
		all = append(all, m.Fields...)
	}
	b := ir.Bounds(all)
	var l limitSet
	if v, ok := cfgLimit(cfg, "max_dyn_array_count"); ok && b.HasDynArray {
		l.arrayCount, l.arrayHas = v, true
	}
	if v, ok := cfgLimit(cfg, "max_dyn_string_len"); ok && b.HasDynString {
		l.stringLen, l.stringHas = v, true
	}
	if v, ok := cfgLimit(cfg, "max_dyn_blob_len"); ok && b.HasDynBlob {
		l.blobLen, l.blobHas = v, true
	}
	return l
}

// std reports whether the std corelib-rs is selected (vs corelib-rs-no-std).
func (g *gen) std() bool { return g.corelib != "rs-no-std" }

// usesAlloc reports whether the crate needs `extern crate alloc`: true when the
// no_std profile was asked for alloc storage (allow_dynamic) and the schema has
// any variable-length field to put there.
func (g *gen) usesAlloc(s *ir.Schema) bool {
	if !g.noStd || !g.allowDynamic {
		return false
	}
	found := false
	var walk func(fields []*ir.Field)
	seen := map[string]bool{}
	walk = func(fields []*ir.Field) {
		for _, f := range fields {
			switch f.Kind {
			case ir.KindString, ir.KindBlob, ir.KindArray:
				// Every variable-length field lives in an alloc container in this
				// mode, bounded or not — the bound became a decode-path check
				// rather than the container's capacity.
				found = true
			case ir.KindStruct, ir.KindUnion:
				if !seen[f.Ref.Key] {
					seen[f.Ref.Key] = true
					walk(f.Ref.Target.Fields)
				}
			}
		}
	}
	for _, m := range s.Messages {
		walk(m.Fields)
	}
	return found
}

type rfile struct{ b strings.Builder }

func (f *rfile) line(format string, args ...any) {
	fmt.Fprintf(&f.b, format, args...)
	f.b.WriteByte('\n')
}
func (f *rfile) blank()        { f.b.WriteByte('\n') }
func (f *rfile) bytes() []byte { return []byte(f.b.String()) }

func (g *gen) module(s *ir.Schema) []byte {
	f := &rfile{}
	f.line("// Code generated by %s; DO NOT EDIT.", g.banner)
	if g.license != "" {
		f.line("// SPDX-License-Identifier: %s", g.license)
	}
	f.line("#![allow(dead_code, unused_variables, unused_imports, non_camel_case_types, clippy::all)]")
	// ArrayKind is only referenced by the per-message decoder's array_begin (and
	// only when the schema has a scalar array); it is gated behind the no-std
	// `array` feature, so it is imported there on demand, not crate-wide.
	f.line("use sofab::{OStream, IStream, Visitor, Id, Unsigned, Signed};")
	// serde is optional under no_std: the derives are gated behind a `serde` cargo
	// feature (off in the heap-free firmware build, on for the JSON harness), so the
	// import must be gated too. The std profile always derives serde.
	if g.noStd {
		f.line("#[cfg(feature = \"serde\")]")
	}
	f.line("use serde::{Serialize, Deserialize};")
	f.blank()
	// capability guard for the whole crate. corelib-rs-no-std gates wire types
	// behind Cargo features and exposes require!() to assert them; corelib-rs
	// (std) always compiles every wire type in and has no such macro.
	if !g.std() {
		caps := g.capabilities(s)
		if len(caps) > 0 {
			f.line("sofab::require!(%s);", strings.Join(caps, ", "))
			f.blank()
		}
	}
	// Receiver-side decode limits (generator#102), baked from the sofabgen config.
	if g.limits.any() {
		f.line("// Receiver-side decode limits, from the sofabgen config")
		f.line("// (max_dyn_array_count / max_dyn_string_len / max_dyn_blob_len). They govern")
		f.line("// only schema-unbounded fields (array without count, string/blob without")
		f.line("// maxlen); schema-bounded fields stay governed by their own bound. Exceeding")
		f.line("// a cap fails try_decode with sofab::Error::LimitExceeded, never a clamp.")
		if g.limits.arrayHas {
			f.line("const MAX_DYN_ARRAY_COUNT: usize = %d;", g.limits.arrayCount)
		}
		if g.limits.stringHas {
			f.line("const MAX_DYN_STRING_LEN: usize = %d;", g.limits.stringLen)
		}
		if g.limits.blobHas {
			f.line("const MAX_DYN_BLOB_LEN: usize = %d;", g.limits.blobLen)
		}
		f.blank()
	}

	// The trailing-default-run narrowing a `count: N` wrapper array's canonical
	// wire needs (generator#248), emitted only when the schema actually has one so
	// every other schema's output — and the footprint profile's `.text` — is
	// unchanged.
	if g.trimSeq {
		f.line("// _trim_seq narrows a `count: N` wrapper array to M -- one past its last")
		f.line("// element differing from the element default -- which is what its canonical")
		f.line("// wire carries (S3/S5.1, \"even for sequence-form elements\").")
		f.line("// Only the TRAILING run is dropped: an interior all-default element keeps its")
		f.line("// frame, because element presence is what carries the array's length. M == 0")
		f.line("// leaves the lazily-opened wrapper contentless, so its dropping closer omits")
		f.line("// the whole field (S2). A dynamic (count-less) array has no N to refill from")
		f.line("// and is never narrowed -- there a trailing default element is significant.")
		f.line("fn _trim_seq<T>(v: &[T], is_default: impl Fn(&T) -> bool) -> &[T] {")
		f.line("    let mut _m = v.len();")
		f.line("    while _m > 0 && is_default(&v[_m - 1]) { _m -= 1; }")
		f.line("    &v[.._m]")
		f.line("}")
		f.blank()
	}

	for _, key := range s.NamedOrder {
		nt := s.Named[key]
		switch nt.Category {
		case ir.CatEnum:
			g.emitEnum(f, nt)
		case ir.CatBitfield:
			g.emitBitfieldConsts(f, nt)
		}
	}
	for _, key := range s.NamedOrder {
		nt := s.Named[key]
		if nt.Category == ir.CatStruct || nt.Category == ir.CatUnion {
			g.emitStruct(f, g.typeName(key), nt.Fields, false, "")
		}
	}
	for _, m := range s.Messages {
		g.emitStruct(f, exported(m.Name), m.Fields, true, m.Summary)
	}
	return f.bytes()
}

func (g *gen) emitEnum(f *rfile, nt *ir.NamedType) {
	f.line("pub mod %s {", strings.ToLower(g.typeName(nt.Key)))
	for _, c := range nt.Consts {
		f.emitDoc("    ", c.Description)
		f.line("    pub const %s: %s = %d;", strings.ToUpper(c.Name), enumBacking(nt), c.Value)
	}
	f.line("}")
	f.blank()
}

func (g *gen) emitBitfieldConsts(f *rfile, nt *ir.NamedType) {
	f.line("pub mod %s {", strings.ToLower(g.typeName(nt.Key)))
	for _, fl := range nt.Flags {
		f.emitDoc("    ", flagDoc(fl))
		f.line("    pub const %s: %s = 1 << %d;", strings.ToUpper(fl.Name), bitfieldBacking(nt), fl.Pos)
	}
	f.line("}")
	f.blank()
}

// flagDoc builds a bitfield flag's rustdoc text from its Description and, when
// the flag has a schema default, an appended `(default: true/false)` note.
func flagDoc(fl *ir.BitfieldFlag) string {
	doc := fl.Description
	if fl.HasDefault {
		note := "(default: false)"
		if fl.Default {
			note = "(default: true)"
		}
		if doc != "" {
			doc += " " + note
		} else {
			doc = note
		}
	}
	return doc
}

// emitDoc writes a rustdoc `///` comment (one line per line of text) at the
// given indent. Empty text emits nothing, so it never leaves a dangling `///`.
func (f *rfile) emitDoc(indent, text string) {
	if text == "" {
		return
	}
	for _, ln := range strings.Split(text, "\n") {
		if ln == "" {
			f.line("%s///", indent) // no trailing space on a blank doc line
			continue
		}
		f.line("%s/// %s", indent, ln)
	}
}

// fieldDoc builds a field's rustdoc text from its Description and Unit. A
// deprecated field gets a trailing `**Deprecated.**` note (on its own line);
// the `#[deprecated]` attribute emitted alongside is what rustdoc renders as
// the deprecation banner, but the prose note keeps the reason legible in source.
func fieldDoc(fld *ir.Field) string {
	var doc string
	switch {
	case fld.Description != "" && fld.Unit != "":
		doc = fld.Description + " (unit: " + fld.Unit + ")"
	case fld.Description != "":
		doc = fld.Description
	case fld.Unit != "":
		doc = "(unit: " + fld.Unit + ")"
	}
	if fld.Deprecated {
		if doc != "" {
			doc += "\n\n**Deprecated.**"
		} else {
			doc = "**Deprecated.**"
		}
	}
	return doc
}

// fieldsHaveDeprecated reports whether any of the given fields is deprecated, so
// the generated impl blocks that read the field can carry #[allow(deprecated)].
func fieldsHaveDeprecated(fields []*ir.Field) bool {
	for _, fld := range fields {
		if fld.Deprecated {
			return true
		}
	}
	return false
}

func (g *gen) emitStruct(f *rfile, name string, fields []*ir.Field, isMessage bool, summary string) {
	// rustdoc summary attaches to the struct that immediately follows.
	f.emitDoc("", summary)
	// Encoding is sparse-canonical (MESSAGE_SPEC S2): a field equal to its default
	// is omitted, so decode must reconstruct schema defaults. A manual Default impl
	// carries them (native scalar arrays and blobs materialize their default too),
	// so derive(Default) type-zeros are never correct here.
	// Under no_std, serde is optional: derive it (and #[serde(default)]) only behind
	// the `serde` cargo feature so the firmware build carries no serde. The std
	// profile always derives it.
	if g.noStd {
		f.line("#[derive(Debug, Clone, PartialEq)]")
		f.line("#[cfg_attr(feature = \"serde\", derive(Serialize, Deserialize))]")
		f.line("#[cfg_attr(feature = \"serde\", serde(default))]")
	} else {
		f.line("#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]")
		f.line("#[serde(default)]")
	}
	f.line("pub struct %s {", name)
	for _, fld := range fields {
		// rustdoc attaches to the item that follows, so the doc must precede
		// any #[serde(rename = ...)] attribute and the field itself.
		f.emitDoc("    ", fieldDoc(fld))
		if fld.Deprecated {
			f.line("    #[deprecated]")
		}
		if rustNeedsRename(fld.Name) {
			if g.noStd {
				f.line("    #[cfg_attr(feature = \"serde\", serde(rename = %q))]", fld.Name)
			} else {
				f.line("    #[serde(rename = %q)]", fld.Name)
			}
		}
		f.line("    pub %s: %s,", rustIdent(fld.Name), g.rustType(fld))
	}
	f.line("}")
	f.blank()
	// The generated Default, serialize, and decode read deprecated fields directly,
	// which would trip the deprecated lint; suppress it over the impl blocks that
	// touch them so the generated crate stays warning-clean.
	deprecated := fieldsHaveDeprecated(fields)
	if deprecated {
		f.line("#[allow(deprecated)]")
	}
	f.line("impl Default for %s {", name)
	f.line("    fn default() -> Self {")
	f.line("        Self {")
	for _, fld := range fields {
		f.line("            %s: %s,", rustIdent(fld.Name), g.rustFieldDefault(fld))
	}
	f.line("        }")
	f.line("    }")
	f.line("}")
	f.blank()

	if deprecated {
		f.line("#[allow(deprecated)]")
	}
	f.line("impl %s {", name)
	ms := g.messageSize(name, fields)
	if isMessage {
		if !ms.Bounded {
			f.line("    /// Configured ceiling (max_message_size): an unbounded field means this")
			f.line("    /// size is imposed, not derived from the schema.")
			f.line("    pub const MAX_SIZE_LIMIT: usize = %d;", ms.Size)
			f.line("    pub const MAX_SIZE: usize = Self::MAX_SIZE_LIMIT;")
		} else {
			f.line("    /// Worst-case encoded size of this message, derived from the schema.")
			f.line("    pub const MAX_SIZE: usize = %d;", ms.Size)
		}
	}
	// serialize is generic over the stream's flush sink, so a caller can write
	// into a buffer (NoFlush) or straight into a transport that drains the
	// buffer as it fills -- the corelib supports both, and pinning the signature
	// to NoFlush made the streaming half unreachable from generated code.
	f.line("    pub fn serialize<_F: sofab::Flush>(&self, os: &mut OStream<'_, _F>) {")
	for _, fld := range fields {
		g.emitSerialize(f, fld)
	}
	f.line("    }")
	g.emitIsDefault(f, name, fields)

	if isMessage {
		if g.noStd {
			// Heap-free encode into a fixed-capacity heapless::Vec sized by MAX_SIZE.
			size := g.messageSize(name, fields).Size
			f.line("    pub fn encode(&self) -> heapless::Vec<u8, %d> {", size)
			f.line("        let mut buf: heapless::Vec<u8, %d> = heapless::Vec::new();", size)
			f.line("        let _ = buf.resize_default(%d);", size)
			f.line("        let used = { let mut os = OStream::new(&mut buf); self.serialize(&mut os); os.bytes_used() };")
			f.line("        buf.truncate(used);")
			f.line("        buf")
			f.line("    }")
		} else if ms.Bounded {
			// MAX_SIZE is derived from the schema, so one exactly-sized buffer
			// always holds the message.
			f.line("    pub fn encode(&self) -> Vec<u8> {")
			f.line("        let mut buf = vec![0u8; Self::MAX_SIZE];")
			f.line("        let used = { let mut os = OStream::new(&mut buf); self.serialize(&mut os); os.bytes_used() };")
			f.line("        buf.truncate(used);")
			f.line("        buf")
			f.line("    }")
		} else {
			// A field with no schema bound has no worst case, so MAX_SIZE here is
			// the configured ceiling -- a policy number, not a size this message
			// cannot exceed. Sizing the buffer from it would silently truncate a
			// larger message (the writes report failure, and encode() has nowhere
			// to report it to). This profile has a heap, so the buffer grows with
			// the message instead and the ceiling never applies to a value the
			// caller legitimately built.
			f.line("    pub fn encode(&self) -> Vec<u8> {")
			f.line("        let mut out: Vec<u8> = Vec::new();")
			f.line("        {")
			f.line("            let mut scratch = [0u8; 512];")
			f.line("            let mut os = OStream::with_flush(&mut scratch, 0, |_d: &[u8]| out.extend_from_slice(_d));")
			f.line("            self.serialize(&mut os);")
			f.line("            os.flush();")
			f.line("        }")
			f.line("        out")
			f.line("    }")
		}
		f.line("    pub fn decode(data: &[u8]) -> Self {")
		f.line("        %s_dec::decode(data)", strings.ToLower(name))
		f.line("    }")
		f.line("    pub fn try_decode(data: &[u8]) -> Result<Self, sofab::Error> {")
		f.line("        %s_dec::try_decode(data)", strings.ToLower(name))
		f.line("    }")
		f.line("    /// An incremental decoder for this message: hold it and feed chunks as")
		f.line("    /// they arrive, instead of buffering the whole message first.")
		f.line("    pub fn decoder() -> %sDecoder {", name)
		f.line("        %sDecoder::new()", name)
		f.line("    }")
	}
	f.line("}")
	f.blank()

	if isMessage {
		g.emitVisitor(f, name, fields)
	}
}

// emitIsDefault emits the object's all-default predicate, for the types
// scanTrim marked. It is the exact negation of what serialize writes: the object
// is default iff serialize would emit no child at all, evaluated per field and
// recursively (MESSAGE_SPEC S2). It exists because a `count: N` wrapper array has
// to find M -- one past its last non-default element -- BEFORE it opens the
// element loop (S3/S5.1), and the lazy framing's implicit "no child was written"
// test cannot answer that in time.
//
// Every arm is the negation of the very expression emitSerialize uses for the
// same field (rustLeafCmp / nativeArrayCmp / elemTrimExpr), so the
// writer and the predicate cannot drift: one that narrowed a field the writer
// does not would omit a field that is on the wire, and the reverse would keep one
// that is not.
func (g *gen) emitIsDefault(f *rfile, name string, fields []*ir.Field) {
	if !g.isDefault[name] {
		return
	}
	f.line("    /// True when every field equals its declared default, i.e. when")
	f.line("    /// serialize would write no child at all (S2). Used to find")
	f.line("    /// the M a `count: N` wrapper array's canonical wire stops at (S3/S5.1).")
	f.line("    fn is_default(&self) -> bool {")
	for _, fld := range fields {
		f.line("        if !(%s) { return false; }", g.fieldIsDefaultExpr(fld))
	}
	f.line("        true")
	f.line("    }")
}

// fieldIsDefaultExpr is the boolean "this field equals its default", i.e. the
// negation of emitSerialize's write guard for the same field, built from the
// same operand builders so the two cannot drift.
func (g *gen) fieldIsDefaultExpr(fld *ir.Field) string {
	acc := "self." + rustIdent(fld.Name)
	switch fld.Kind {
	case ir.KindBlob:
		if raw, ok := g.blobBytes(fld); ok {
			return fmt.Sprintf("&%s[..] == &%s[..]", acc, byteSliceLit(raw))
		}
		return fmt.Sprintf("%s.is_empty()", acc)
	case ir.KindStruct, ir.KindUnion:
		// Lazily framed: the frame survives iff the nested serialize wrote a child,
		// which is exactly "the nested object is not default".
		return fmt.Sprintf("%s.is_default()", acc)
	case ir.KindArray:
		return g.arrayIsDefaultExpr(fld, acc)
	}
	return g.rustLeafCmp(acc, fld, "==")
}

// arrayIsDefaultExpr mirrors emitSerializeArray: a native array compares against
// its default through the same expression the write guard uses; a wrapper array
// is default iff it would write no child -- i.e. iff its narrowed run is empty.
//
// fld.HasCount must be threaded into elemTrimExpr unchanged: narrowing here but
// not in the serialize loop (or the reverse) is exactly the drift the shared
// helper exists to prevent -- it would call a dynamic [{}] "default" while the
// writer still frames its one empty element.
func (g *gen) arrayIsDefaultExpr(fld *ir.Field, acc string) string {
	if isNativeArrayElem(fld.Elem) {
		return g.nativeArrayCmp(fld, acc, false)
	}
	return fmt.Sprintf("%s.is_empty()", g.elemTrimExpr(acc, fld.Elem, fld.HasCount))
}

func (g *gen) emitSerialize(f *rfile, fld *ir.Field) {
	acc := "self." + rustIdent(fld.Name)
	var write string
	switch fld.Kind {
	case ir.KindU8, ir.KindU16, ir.KindU32, ir.KindU64, ir.KindBitfield:
		write = fmt.Sprintf("let _ = os.write_unsigned(%d, %s as Unsigned);", fld.ID, acc)
	case ir.KindI8, ir.KindI16, ir.KindI32, ir.KindI64, ir.KindEnum:
		write = fmt.Sprintf("let _ = os.write_signed(%d, %s as Signed);", fld.ID, acc)
	case ir.KindBool:
		write = fmt.Sprintf("let _ = os.write_boolean(%d, %s);", fld.ID, acc)
	case ir.KindFP32:
		write = fmt.Sprintf("let _ = os.write_fp32(%d, %s);", fld.ID, acc)
	case ir.KindFP64:
		write = fmt.Sprintf("let _ = os.write_fp64(%d, %s);", fld.ID, acc)
	case ir.KindString:
		write = fmt.Sprintf("let _ = os.write_str(%d, &%s);", fld.ID, acc)
	case ir.KindBlob:
		// blob is a leaf: omit when equal to its default. Compare as slices so the
		// same form works for std Vec and no_std heapless/alloc Vec alike.
		if raw, ok := g.blobBytes(fld); ok {
			f.line("        if &%s[..] != &%s[..] { let _ = os.write_blob(%d, &%s); }", acc, byteSliceLit(raw), fld.ID, acc)
		} else {
			f.line("        if !%s.is_empty() { let _ = os.write_blob(%d, &%s); }", acc, fld.ID, acc)
		}
		return
	case ir.KindStruct, ir.KindUnion:
		// MESSAGE_SPEC S2: the != default test is per field and a sequence is no
		// exception, so the frame is opened LAZILY -- the corelib writes the header
		// only once a child field appears. The nested serialize omits each child
		// that equals its default, so "no child was written" IS "the object equals
		// its declared default", evaluated per field and recursively. An all-default
		// nested object is therefore dropped, not emitted as an empty wrapper.
		f.line("        let _ = os.write_sequence_begin_lazy(%d); %s.serialize(os); let _ = os.write_sequence_end();", fld.ID, acc)
		return
	case ir.KindArray:
		g.emitSerializeArray(f, fld, acc)
		return
	}
	// Scalar/string/enum/bitfield leaf: always omit when equal to the default;
	// sparse encoding is canonical (MESSAGE_SPEC S2) and the decoder reconstructs
	// the omitted field from its default.
	f.line("        if %s { %s }", g.rustLeafNe(acc, fld), write)
}

func (g *gen) emitSerializeArray(f *rfile, fld *ir.Field, acc string) {
	// A native scalar array is a leaf field: omit it when equal to its default
	// (materialized in Default), else when empty. A composite/dynamic-element
	// array is a wrapper sequence: opened lazily, closed with the dropping end at
	// field level, so an all-default one is omitted (MESSAGE_SPEC §2).
	if isNativeArrayElem(fld.Elem) {
		f.line("        if %s {", g.nativeArrayCmp(fld, acc, true))
		g.serializeArray(f, "            ", fmt.Sprintf("%d", fld.ID), acc, fld.Elem, fld.ElemRef, fld.ElemItems, fld.Count, fld.HasCount, fld.HasCount, 0)
		f.line("        }")
		return
	}
	// The field-level wrapper frame is dropped when no element is written, and
	// absence then reconstructs the field's default. That is correct because a
	// wrapper array's declared `default` is not materialized today (the generated
	// Default is the empty collection), so absent and explicitly-empty denote the
	// same value. If that gap is ever closed, this call needs a guard --
	// `if value != default { ... write_sequence_end_keep() }` -- so that a value
	// differing from a non-empty default still reaches the wire as the empty
	// wrapper, the only encoding of "explicitly empty" (MESSAGE_SPEC S2, S3).
	g.serializeArray(f, "        ", fmt.Sprintf("%d", fld.ID), acc, fld.Elem, fld.ElemRef, fld.ElemItems, fld.Count, fld.HasCount, fld.HasCount, 0)
}

// nativeArrayCmp is the `!= default` write guard for a native scalar array field
// (MESSAGE_SPEC §2), or its exact negation when ne is false. One builder serves
// both emitSerializeArray and is_default; four inline branches in two places
// would let the write decision and the all-default predicate drift, and a field
// sitting on its default would then be written or dropped inconsistently.
func (g *gen) nativeArrayCmp(fld *ir.Field, acc string, ne bool) string {
	op, empty := "==", "%s.is_empty()"
	if ne {
		op, empty = "!=", "!%s.is_empty()"
	}
	if _, _, ok := g.fixedNativeArray(fld); ok {
		// Fixed `[elem; N]` is never "empty"; omit when equal to its default
		// (mirrors the C++ backend's `!= std::array{}`).
		return fmt.Sprintf("%s %s %s", acc, op, g.rustFieldDefault(fld))
	}
	if parts, ok := g.rustNativeArrayPartsN(fld); ok {
		// Native array with a default, held in a Vec: slice compare. The literal is
		// the N-element default — the same one the field is constructed with — or a
		// field sitting on its default would never compare equal and §2 would never
		// omit it.
		return fmt.Sprintf("&%s[..] %s &[%s][..]", acc, op, parts)
	}
	if fld.HasCount {
		return fmt.Sprintf("&%s[..] %s &[%s; %d][..]", acc, op, rustElemZeroLit(fld.Elem), fld.Count)
	}
	return fmt.Sprintf(empty, acc)
}

// elemTrimExpr narrows a wrapper array to the M its canonical wire carries: one
// past the last element differing from the element default (MESSAGE_SPEC §3/§5.1,
// which says "even for sequence-form elements"). Only a declared `count: N` array
// is fixed-length and may be narrowed -- a dynamic array has no N to refill from,
// so a trailing default ELEMENT is significant and stays framed. Interior
// all-default elements are never dropped by this: element presence carries the
// length, so only the trailing run goes. Both the serialize loop and is_default
// run off this one expression, so the writer and the predicate cannot disagree.
//
// A string/blob element is a leaf the writer already omits individually when it
// equals the element default, so narrowing that run does not change the bytes --
// it exists so the predicate is computed from the very expression the writer
// loops over. Nested-array ROWS are deliberately not narrowed here; see the
// KindArray arm of serializeArray.
func (g *gen) elemTrimExpr(val string, elem ir.Kind, fixed bool) string {
	if !fixed {
		return val
	}
	switch elem {
	case ir.KindString, ir.KindBlob:
		return fmt.Sprintf("_trim_seq(&%s, |_x| _x.is_empty())", val)
	case ir.KindStruct, ir.KindUnion:
		return fmt.Sprintf("_trim_seq(&%s, |_x| _x.is_default())", val)
	}
	return val
}

// lastElemGuard is the "|| this is the last element" disjunct that keeps a
// DYNAMIC wrapper array's final element on the wire whatever its value
// (MESSAGE_SPEC §2, "the last element of a dynamic array is always present").
// Such an array recovers its length as highest-present-id + 1 (§5.1), so the
// element at the highest index is the only one whose PRESENCE carries the
// length: dropping it would encode ["a", ""] exactly like ["a"] and decode one
// element short. Sequence-form elements never needed this -- they are framed
// unconditionally -- so this closes the gap on the leaf side and holds both
// element kinds to one standard. A fixed-count array needs none of it: its
// length is N whatever the wire carries, which is why it elides the entire
// trailing default run instead (§3/§5.1), so the guard is omitted there and the
// trailing run collapses as before -- the same `fixed` flag elemTrimExpr gates
// its narrowing on, so the writer and the all-default predicate stay in step.
//
// The loop body only runs when the array is non-empty, and the `+ 1 ==` form
// keeps the comparison off the underflowing `len() - 1` regardless.
func lastElemGuard(iv, val string, fixed bool) string {
	if fixed {
		return ""
	}
	return fmt.Sprintf(" || %s + 1 == %s.len()", iv, val)
}

// trimExpr renders the `&[T]` argument for a native array write, applying the
// trailing-default-run trim a fixed-count array's canonical encoding requires
// (MESSAGE_SPEC §3). Only a declared `count: N` array is fixed-length; a dynamic
// (count-less) array has no N to refill from, so a trailing default element is
// significant and stays. The `[..]` reborrow is what lets a `[T; N]` field and a
// `Vec<T>` field share one call shape.
func (g *gen) trimExpr(val string, elem ir.Kind, fixed bool) string {
	if !fixed {
		return "&" + val
	}
	switch elem {
	case ir.KindFP32:
		return fmt.Sprintf("sofab::trim_tail_f32(&%s[..])", val)
	case ir.KindFP64:
		return fmt.Sprintf("sofab::trim_tail_f64(&%s[..])", val)
	default:
		// Integer/enum/bitfield elements are ints (bool arrives here as its 0/1 u8
		// image), so the unsuffixed 0 infers to the element type.
		return fmt.Sprintf("sofab::trim_tail(&%s[..], 0)", val)
	}
}

// serializeArray writes the array val as field idExpr. Numeric/enum/bitfield
// elements use the native array wire type (numeric/enum by signedness, bitfield
// -> unsigned); boolean lowers to a 0/1 unsigned array; string/blob/struct/union/
// array elements lower to a wrapper sequence whose child ids are the 0-based
// index (per MESSAGE_SPEC). Recurses for nested arrays, depth-suffixing loop vars
// to avoid collisions.
// fixed marks val as a top-level `count: N` array field, whose native elements
// are trimmed of their trailing default run (MESSAGE_SPEC §3). It is distinct
// from hasCount, which only selects the storage shape: a nested array-of-array
// row is `count:`-shaped storage but is not a fixed-length field, so the
// recursion passes fixed=false.
func (g *gen) serializeArray(f *rfile, ind, idExpr, val string, elem ir.Kind, ref *ir.TypeRef, items *ir.ArrayElem, count int64, hasCount, fixed bool, depth int) {
	iv := fmt.Sprintf("_i%d", depth)
	ev := fmt.Sprintf("_e%d", depth)
	tv := fmt.Sprintf("_t%d", depth)
	// MESSAGE_SPEC S2: every sequence is opened lazily; the CLOSER decides whether a
	// contentless one survives. A wrapper array is a sequence-typed FIELD, so at
	// depth 0 it closes with the dropping end -- an all-default array is omitted and
	// absence reconstructs it. A nested row (depth > 0) is an array ELEMENT, and
	// element presence is what carries a dynamic array's length (S5.1), so it closes
	// with the keeping end.
	seqEnd := "write_sequence_end"
	if depth > 0 {
		seqEnd = "write_sequence_end_keep"
	}
	switch elem {
	case ir.KindU8, ir.KindU16, ir.KindU32, ir.KindU64, ir.KindBitfield:
		// bitfield backing is an unsigned int (UnsignedElem), so it writes directly.
		f.line("%slet _ = os.write_array_unsigned(%s, %s);", ind, idExpr, g.trimExpr(val, elem, fixed))
	case ir.KindI8, ir.KindI16, ir.KindI32, ir.KindI64, ir.KindEnum:
		// enum backing is a signed int (SignedElem), so it writes directly.
		f.line("%slet _ = os.write_array_signed(%s, %s);", ind, idExpr, g.trimExpr(val, elem, fixed))
	case ir.KindBool:
		// bool is not an array element type; lower to a 0/1 unsigned array. The
		// no_std profile avoids the heap collect: a fixed array maps in place via
		// core::array::from_fn, a dynamic (allow_dynamic) one collects into alloc.
		// Trimming the 0/1 image is equivalent to trimming the bools (false <-> 0).
		bt := g.trimExpr(tv, ir.KindU8, fixed)
		switch {
		case !g.noStd:
			f.line("%s{ let %s: Vec<u8> = %s.iter().map(|_v| *_v as u8).collect(); let _ = os.write_array_unsigned(%s, %s); }", ind, tv, val, idExpr, bt)
		case hasCount:
			f.line("%s{ let %s: [u8; %d] = core::array::from_fn(|_k| %s[_k] as u8); let _ = os.write_array_unsigned(%s, %s); }", ind, tv, count, val, idExpr, bt)
		default:
			f.line("%s{ let %s: alloc::vec::Vec<u8> = %s.iter().map(|_v| *_v as u8).collect(); let _ = os.write_array_unsigned(%s, %s); }", ind, tv, val, idExpr, bt)
		}
	case ir.KindFP32:
		f.line("%slet _ = os.write_array_fp32(%s, %s);", ind, idExpr, g.trimExpr(val, elem, fixed))
	case ir.KindFP64:
		f.line("%slet _ = os.write_array_fp64(%s, %s);", ind, idExpr, g.trimExpr(val, elem, fixed))
	case ir.KindString:
		// A string element is a leaf: omit it when equal to the element default
		// (empty), leaving an id gap the decoder restores (MESSAGE_SPEC S2) --
		// except at the one position whose presence carries the length, see
		// lastElemGuard.
		f.line("%slet _ = os.write_sequence_begin_lazy(%s);", ind, idExpr)
		f.line("%sfor (%s, %s) in %s.iter().enumerate() { if !%s.is_empty()%s { let _ = os.write_str(%s as Id, %s); } }", ind, iv, ev, g.elemTrimExpr(val, elem, fixed), ev, lastElemGuard(iv, val, fixed), iv, ev)
		f.line("%slet _ = os.%s();", ind, seqEnd)
	case ir.KindBlob:
		// A blob element is a leaf: omit it when equal to the element default
		// (empty), leaving an id gap the decoder restores (MESSAGE_SPEC S2) --
		// except at the one position whose presence carries the length, see
		// lastElemGuard.
		f.line("%slet _ = os.write_sequence_begin_lazy(%s);", ind, idExpr)
		f.line("%sfor (%s, %s) in %s.iter().enumerate() { if !%s.is_empty()%s { let _ = os.write_blob(%s as Id, %s); } }", ind, iv, ev, g.elemTrimExpr(val, elem, fixed), ev, lastElemGuard(iv, val, fixed), iv, ev)
		f.line("%slet _ = os.%s();", ind, seqEnd)
	case ir.KindStruct, ir.KindUnion:
		f.line("%slet _ = os.write_sequence_begin_lazy(%s);", ind, idExpr)
		f.line("%sfor (%s, %s) in %s.iter().enumerate() {", ind, iv, ev, g.elemTrimExpr(val, elem, fixed))
		// An INTERIOR element is framed unconditionally: dropping it would leave an
		// id gap and change the decoded length, not just the bytes (S5.1). The
		// TRAILING all-default run is already gone -- the loop runs to M, not to
		// len (S3/S5.1) -- and M == 0 writes no child at all, so the lazily-opened
		// wrapper is dropped and the field is omitted (S2).
		f.line("%s    let _ = os.write_sequence_begin_lazy(%s as Id); %s.serialize(os); let _ = os.write_sequence_end_keep();", ind, iv, ev)
		f.line("%s}", ind)
		f.line("%slet _ = os.%s();", ind, seqEnd)
	case ir.KindArray:
		f.line("%slet _ = os.write_sequence_begin_lazy(%s);", ind, idExpr)
		// Nested-array ROWS are not narrowed (elemTrimExpr is deliberately not
		// applied here). A row's writer emits an array header unconditionally, so a
		// row is never "not written" the way a default string element or an
		// all-default struct element is; dropping a trailing empty row would remove
		// a child that IS on the wire, and decode has no row refill to put it back,
		// so the outer array would shorten on every round trip. See docs/generator/
		// rust.md.
		f.line("%sfor (%s, %s) in %s.iter().enumerate() {", ind, iv, ev, val)
		// A nested row is not a fixed-length *field*, so it keeps every element.
		g.serializeArray(f, ind+"    ", fmt.Sprintf("%s as Id", iv), ev, items.Elem, items.ElemRef, items.ElemItems, items.Count, items.HasCount, false, depth+1)
		f.line("%s}", ind)
		f.line("%slet _ = os.%s();", ind, seqEnd)
	}
}
