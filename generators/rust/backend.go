// Package rust is the Rust backend (PLAN §6.2, embedded/no_std-capable): structs
// with marshal() over OStream and a flat-visitor decode. The corelib's Visitor
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
	files := []generator.File{{Path: "src/message.rs", Content: g.module(s)}}
	if cfgString(cfg, "emit", "sources") == "project" {
		files = append(files, g.projectFiles(s, cfg)...)
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

// dynString / dynBlob / dynArray report whether a given unbounded field falls
// back to an alloc heap container (allow_dynamic) rather than heapless storage.
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
			case ir.KindString, ir.KindBlob:
				if !f.HasMaxlen {
					found = true
				}
			case ir.KindArray:
				if !f.HasCount {
					found = true
				}
				if (f.Elem == ir.KindString || f.Elem == ir.KindBlob) && !f.ElemMaxHas {
					found = true
				}
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

	g.emitTrimHelpers(f, s)

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

// emitTrimHelpers emits the trailing-default-run trim helpers a fixed-count
// native array needs on encode (MESSAGE_SPEC §3). They are `core`-only (slice
// len/index plus f32/f64::to_bits) and allocate nothing — `&a[..n]` reborrows —
// so the same code serves the std and the #![no_std] profile.
func (g *gen) emitTrimHelpers(f *rfile, s *ir.Schema) {
	anyInt, anyF32, anyF64 := g.trimKinds(s)
	if !anyInt && !anyF32 && !anyF64 {
		return
	}
	f.line("// _trim_tail / _trim_tail_f32 / _trim_tail_f64 return &a[..M'], where M' is one")
	f.line("// past the last element that differs from the element default (0 when every")
	f.line("// element is the default). A `count: N` array is fixed-length: its canonical wire")
	f.line("// carries exactly those M' elements and the decoder rebuilds the trailing default")
	f.line("// run from the schema count (MESSAGE_SPEC S3). A dynamic (count-less) array has")
	f.line("// no N to refill from, so it is never trimmed. Floats compare by BIT PATTERN, not")
	f.line("// by ==, so a trailing -0.0 (which == 0.0) survives the round-trip instead of")
	f.line("// being silently trimmed to +0.0, and a NaN is never taken for the default.")
	if anyInt {
		f.line("fn _trim_tail<T: PartialEq + Copy>(a: &[T], zero: T) -> &[T] {")
		f.line("    let mut n = a.len();")
		f.line("    while n > 0 && a[n - 1] == zero { n -= 1; }")
		f.line("    &a[..n]")
		f.line("}")
	}
	if anyF32 {
		f.line("fn _trim_tail_f32(a: &[f32]) -> &[f32] {")
		f.line("    let mut n = a.len();")
		f.line("    while n > 0 && f32::to_bits(a[n - 1]) == 0 { n -= 1; }")
		f.line("    &a[..n]")
		f.line("}")
	}
	if anyF64 {
		f.line("fn _trim_tail_f64(a: &[f64]) -> &[f64] {")
		f.line("    let mut n = a.len();")
		f.line("    while n > 0 && f64::to_bits(a[n - 1]) == 0 { n -= 1; }")
		f.line("    &a[..n]")
		f.line("}")
	}
	f.blank()
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
	// The generated Default, marshal, and decode read deprecated fields directly,
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
	if isMessage {
		size, _ := g.maxSize(fields)
		f.line("    pub const MAX_SIZE: usize = %d;", size)
	}
	// marshal
	f.line("    pub fn marshal(&self, os: &mut OStream) {")
	for _, fld := range fields {
		g.emitMarshal(f, fld)
	}
	f.line("    }")

	if isMessage {
		if g.noStd {
			// Heap-free encode into a fixed-capacity heapless::Vec sized by MAX_SIZE.
			size, _ := g.maxSize(fields)
			f.line("    pub fn encode(&self) -> heapless::Vec<u8, %d> {", size)
			f.line("        let mut buf: heapless::Vec<u8, %d> = heapless::Vec::new();", size)
			f.line("        let _ = buf.resize_default(%d);", size)
			f.line("        let used = { let mut os = OStream::new(&mut buf); self.marshal(&mut os); os.bytes_used() };")
			f.line("        buf.truncate(used);")
			f.line("        buf")
			f.line("    }")
		} else {
			f.line("    pub fn encode(&self) -> Vec<u8> {")
			f.line("        let mut buf = vec![0u8; Self::MAX_SIZE];")
			f.line("        let used = { let mut os = OStream::new(&mut buf); self.marshal(&mut os); os.bytes_used() };")
			f.line("        buf.truncate(used);")
			f.line("        buf")
			f.line("    }")
		}
		f.line("    pub fn decode(data: &[u8]) -> Self {")
		f.line("        %s_dec::decode(data)", strings.ToLower(name))
		f.line("    }")
		f.line("    pub fn try_decode(data: &[u8]) -> Result<Self, sofab::Error> {")
		f.line("        %s_dec::try_decode(data)", strings.ToLower(name))
		f.line("    }")
	}
	f.line("}")
	f.blank()

	if isMessage {
		g.emitVisitor(f, name, fields)
	}
}

func (g *gen) emitMarshal(f *rfile, fld *ir.Field) {
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
		// A sequence is always framed; its child fields are omitted per-field by
		// the nested marshal (MESSAGE_SPEC S2). An all-default nested object thus
		// becomes an empty wrapper sequence, not a dropped field.
		f.line("        let _ = os.write_sequence_begin(%d); %s.marshal(os); let _ = os.write_sequence_end();", fld.ID, acc)
		return
	case ir.KindArray:
		g.emitMarshalArray(f, fld, acc)
		return
	}
	// Scalar/string/enum/bitfield leaf: always omit when equal to the default;
	// sparse encoding is canonical (MESSAGE_SPEC S2) and the decoder reconstructs
	// the omitted field from its default.
	f.line("        if %s { %s }", g.rustLeafNe(acc, fld), write)
}

func (g *gen) emitMarshalArray(f *rfile, fld *ir.Field, acc string) {
	// A native scalar array is a leaf field: omit it when equal to its default
	// (materialized in Default), else when empty. A composite/dynamic-element
	// array is a wrapper sequence and is always framed (never whole-omitted).
	if isNativeArrayElem(fld.Elem) {
		if _, _, ok := g.fixedNativeArray(fld); ok {
			// Fixed `[elem; N]` is never "empty"; omit when equal to its default
			// (mirrors the C++ backend's `!= std::array{}`).
			f.line("        if %s != %s {", acc, g.rustFieldDefault(fld))
		} else if parts, ok := g.rustNativeArrayParts(fld); ok {
			// Dynamic native array with a default: slice compare (std Vec / no_std alloc).
			f.line("        if &%s[..] != &[%s][..] {", acc, parts)
		} else {
			f.line("        if !%s.is_empty() {", acc)
		}
		g.marshalArray(f, "            ", fmt.Sprintf("%d", fld.ID), acc, fld.Elem, fld.ElemRef, fld.ElemItems, fld.Count, fld.HasCount, fld.HasCount, 0)
		f.line("        }")
		return
	}
	g.marshalArray(f, "        ", fmt.Sprintf("%d", fld.ID), acc, fld.Elem, fld.ElemRef, fld.ElemItems, fld.Count, fld.HasCount, fld.HasCount, 0)
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
		return fmt.Sprintf("_trim_tail_f32(&%s[..])", val)
	case ir.KindFP64:
		return fmt.Sprintf("_trim_tail_f64(&%s[..])", val)
	default:
		// Integer/enum/bitfield elements are ints (bool arrives here as its 0/1 u8
		// image), so the unsuffixed 0 infers to the element type.
		return fmt.Sprintf("_trim_tail(&%s[..], 0)", val)
	}
}

// marshalArray writes the array val as field idExpr. Numeric/enum/bitfield
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
func (g *gen) marshalArray(f *rfile, ind, idExpr, val string, elem ir.Kind, ref *ir.TypeRef, items *ir.ArrayElem, count int64, hasCount, fixed bool, depth int) {
	iv := fmt.Sprintf("_i%d", depth)
	ev := fmt.Sprintf("_e%d", depth)
	tv := fmt.Sprintf("_t%d", depth)
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
		// (empty), leaving an id gap the decoder restores (MESSAGE_SPEC S2).
		f.line("%slet _ = os.write_sequence_begin(%s);", ind, idExpr)
		f.line("%sfor (%s, %s) in %s.iter().enumerate() { if !%s.is_empty() { let _ = os.write_str(%s as Id, %s); } }", ind, iv, ev, val, ev, iv, ev)
		f.line("%slet _ = os.write_sequence_end();", ind)
	case ir.KindBlob:
		// A blob element is a leaf: omit it when equal to the element default
		// (empty), leaving an id gap the decoder restores (MESSAGE_SPEC S2).
		f.line("%slet _ = os.write_sequence_begin(%s);", ind, idExpr)
		f.line("%sfor (%s, %s) in %s.iter().enumerate() { if !%s.is_empty() { let _ = os.write_blob(%s as Id, %s); } }", ind, iv, ev, val, ev, iv, ev)
		f.line("%slet _ = os.write_sequence_end();", ind)
	case ir.KindStruct, ir.KindUnion:
		f.line("%slet _ = os.write_sequence_begin(%s);", ind, idExpr)
		f.line("%sfor (%s, %s) in %s.iter().enumerate() {", ind, iv, ev, val)
		f.line("%s    let _ = os.write_sequence_begin(%s as Id); %s.marshal(os); let _ = os.write_sequence_end();", ind, iv, ev)
		f.line("%s}", ind)
		f.line("%slet _ = os.write_sequence_end();", ind)
	case ir.KindArray:
		f.line("%slet _ = os.write_sequence_begin(%s);", ind, idExpr)
		f.line("%sfor (%s, %s) in %s.iter().enumerate() {", ind, iv, ev, val)
		// A nested row is not a fixed-length *field*, so it keeps every element.
		g.marshalArray(f, ind+"    ", fmt.Sprintf("%s as Id", iv), ev, items.Elem, items.ElemRef, items.ElemItems, items.Count, items.HasCount, false, depth+1)
		f.line("%s}", ind)
		f.line("%slet _ = os.write_sequence_end();", ind)
	}
}
