// Package dart is the Dart throughput backend (PLAN §6.4): plain classes with a
// streaming `serialize` over the corelib Encoder and a push child-visitor decode
// against corelib-dart.
//
// corelib-dart's decode model is the push child-visitor (like Go): a
// `MessageVisitor` whose `onSequenceStart(id)` returns a child visitor for a
// nested scope, and whose native arrays arrive whole through a distinct
// `on*Array` callback. So the MESSAGE_SPEC §7.3/§7.4 wire-type dispatch is
// settled structurally — a contradictory header (including an integer array at a
// scalar id, or a fixlen subtype mismatch) lands in a different, unhandled
// callback and evaporates; a re-opened struct scope descends into the existing
// member (merge), while an array wrapper clears its list in `onSequenceStart`
// (replace). No `askip` guard is needed.
//
// The corelib's visitor callbacks return void, so a generated visitor cannot
// signal INVALID mid-decode. The over-count (#100), over-index (#142) and
// over-maxlen (S7.1) verdicts therefore ride a sticky `_inv` flag the visitor
// sets and the generated `decode`/`tryDecode` converts to a terminal INVALID
// after the corelib returns — the Rust/Zig "generated guard, sticky flag" model.
// The receiver-side decode limits (#102) are enforced by the corelib itself,
// passed in as a `DecoderLimits` (the Go/Python/TS family).
package dart

import (
	"fmt"
	"strings"

	"github.com/sofa-buffers/generator/internal/generator"
	"github.com/sofa-buffers/generator/internal/ir"
)

func init() { generator.Register(&Backend{}) }

// Backend implements generator.Backend for Dart.
type Backend struct{}

func (*Backend) Lang() string { return "dart" }

// Generate emits a single library file (message.dart) with every enum/bitfield
// constant class and object class, plus — when emit==project — a buildable
// package (pubspec.yaml + a JSON encode/decode harness) against corelib-dart.
func (*Backend) Generate(s *ir.Schema, cfg map[string]any) ([]generator.File, error) {
	g := &gen{
		schema:  s,
		banner:  cfgString(cfg, "tool_banner", "sofabgen"),
		license: generator.LicenseID(cfg),
		limits:  resolveLimits(s, cfg),
		size:    generator.NewSizePolicy(cfg),
	}
	project := cfgString(cfg, "emit", "sources") == "project"
	prefix := ""
	if project {
		prefix = "lib/"
	}
	files := []generator.File{{Path: prefix + "message.dart", Content: g.module(s)}}
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
// resolved against the schema like the Go/Python/TS backends: corelib-dart
// enforces the limits globally per decode (a DecoderLimits passed to the
// decoder), so each active cap is the configured value raised to the largest
// schema bound of its kind, and an entry is active only when its key is set AND
// the schema actually has an unbounded field of that kind.
type limitSet struct {
	arrayCount, stringLen, blobLen int64
	arrayHas, stringHas, blobHas   bool
}

func (l limitSet) any() bool { return l.arrayHas || l.stringHas || l.blobHas }

func resolveLimits(s *ir.Schema, cfg map[string]any) limitSet {
	var all []*ir.Field
	for _, m := range s.Messages {
		all = append(all, m.Fields...)
	}
	b := ir.Bounds(all)
	var l limitSet
	if v, ok := cfgLimit(cfg, "max_dyn_array_count"); ok && b.HasDynArray {
		l.arrayCount, l.arrayHas = max(v, b.MaxCount), true
	}
	if v, ok := cfgLimit(cfg, "max_dyn_string_len"); ok && b.HasDynString {
		l.stringLen, l.stringHas = max(v, b.MaxStringLen), true
	}
	if v, ok := cfgLimit(cfg, "max_dyn_blob_len"); ok && b.HasDynBlob {
		l.blobLen, l.blobHas = max(v, b.MaxBlobLen), true
	}
	return l
}

// ---- source file builder --------------------------------------------------

type dfile struct{ b strings.Builder }

func (f *dfile) line(format string, args ...any) {
	fmt.Fprintf(&f.b, format, args...)
	f.b.WriteByte('\n')
}
func (f *dfile) blank()        { f.b.WriteByte('\n') }
func (f *dfile) bytes() []byte { return []byte(f.b.String()) }

// ---- module ---------------------------------------------------------------

func (g *gen) module(s *ir.Schema) []byte {
	f := &dfile{}
	f.line("// Code generated by %s; DO NOT EDIT.", g.banner)
	if g.license != "" {
		f.line("// SPDX-License-Identifier: %s", g.license)
	}
	f.line("// ignore_for_file: unused_field, unused_element, deprecated_member_use_from_same_package")
	// dart:convert is for utf8.decode in the string destinations: the corelib
	// hands the visitor RAW wire bytes (onStringBytes) so the destination can be
	// resolved before anything is validated or transcoded, and transcoding is
	// therefore ours. Emitted only when the module actually decodes a string --
	// `dart analyze` treats an unused import as a warning, and the corpus sweep
	// builds definitions that have no string at all.
	if g.computeNeeds(s).str {
		f.line("import 'dart:convert';")
	}
	f.line("import 'dart:typed_data';")
	f.line("import 'package:sofabuffers/sofabuffers.dart' as sofab;")
	f.blank()

	g.emitLimits(f)
	g.emitPrelude(f, s)

	for _, key := range s.NamedOrder {
		nt := s.Named[key]
		switch nt.Category {
		case ir.CatEnum:
			g.emitEnum(f, nt)
		case ir.CatBitfield:
			g.emitBitfield(f, nt)
		}
	}
	for _, key := range s.NamedOrder {
		nt := s.Named[key]
		if nt.Category == ir.CatStruct || nt.Category == ir.CatUnion {
			g.emitClass(f, g.typeName(key), nt.Summary, nt.Fields, false)
		}
	}
	for _, m := range s.Messages {
		g.emitClass(f, exported(m.Name), m.Summary, m.Fields, true)
	}
	return f.bytes()
}

func (g *gen) emitLimits(f *dfile) {
	if !g.limits.any() {
		return
	}
	f.line("// Receiver-side decode limits baked from the sofabgen config")
	f.line("// (max_dyn_array_count / max_dyn_string_len / max_dyn_blob_len). They govern")
	f.line("// only fields the schema left unbounded; each cap is raised to the largest")
	f.line("// schema bound of its kind. Exceeding a cap fails decode with limitExceeded.")
	if g.limits.arrayHas {
		f.line("const int maxDynArrayCount = %d;", g.limits.arrayCount)
	}
	if g.limits.stringHas {
		f.line("const int maxDynStringLen = %d;", g.limits.stringLen)
	}
	if g.limits.blobHas {
		f.line("const int maxDynBlobLen = %d;", g.limits.blobLen)
	}
	var parts []string
	if g.limits.arrayHas {
		parts = append(parts, "maxArrayCount: maxDynArrayCount")
	}
	if g.limits.stringHas {
		parts = append(parts, "maxStringLen: maxDynStringLen")
	}
	if g.limits.blobHas {
		parts = append(parts, "maxBlobLen: maxDynBlobLen")
	}
	f.line("const sofab.DecoderLimits _limits = sofab.DecoderLimits(%s);", strings.Join(parts, ", "))
	f.blank()
}

// limitsArg is the trailing ", limits: _limits" appended to Decoder.decode when
// any receiver-side cap is active, else "".
func (g *gen) limitsArg() string {
	if g.limits.any() {
		return ", limits: _limits"
	}
	return ""
}

// ---- enum / bitfield ------------------------------------------------------

// emitEnum lowers an enum to a Dart namespace of `static const int` values (the
// field itself stays a raw `int`, signed on the wire). This matches the Zig
// backend's integer namespaces and admits negative values, which a plain Dart
// enum cannot.
func (g *gen) emitEnum(f *dfile, nt *ir.NamedType) {
	emitDoc(f, "", nt.Summary)
	f.line("abstract final class %s {", g.typeName(nt.Key))
	f.line("  %s._();", g.typeName(nt.Key))
	for _, c := range nt.Consts {
		emitDoc(f, "  ", c.Description)
		f.line("  static const int %s = %d;", dartIdent(c.Name), c.Value)
	}
	f.line("}")
	f.blank()
}

func (g *gen) emitBitfield(f *dfile, nt *ir.NamedType) {
	emitDoc(f, "", nt.Summary)
	f.line("abstract final class %s {", g.typeName(nt.Key))
	f.line("  %s._();", g.typeName(nt.Key))
	for _, fl := range nt.Flags {
		emitDoc(f, "  ", flagDoc(fl))
		f.line("  static const int %s = 1 << %d;", dartIdent(fl.Name), fl.Pos)
	}
	f.line("}")
	f.blank()
}

// ---- object class ---------------------------------------------------------

func (g *gen) emitClass(f *dfile, name, summary string, fields []*ir.Field, isMessage bool) {
	emitDoc(f, "", summary)
	f.line("class %s {", name)
	for _, fld := range fields {
		emitDoc(f, "  ", fieldDoc(fld, generator.BoundNote(fld, generator.StorageDynamic)))
		if fld.Deprecated {
			f.line("  @Deprecated('retained for backward compatibility only')")
		}
		f.line("  %s %s%s;", g.dartType(fld), dartIdent(fld.Name), g.dartInit(fld))
		if fld.Kind == ir.KindFP32 {
			// Companion raw-bits slot: a Dart `double` cannot carry an fp32 NaN's
			// payload/signaling bits (§4.6), so when decode delivers a NaN we keep the
			// exact 32 wire bits here and re-emit them via writeFp32Bits. null == "no
			// captured bits; derive the wire image from the double".
			f.line("  int? %s;", fp32BitsField(fld.Name))
		}
	}
	f.blank()

	// serialize: sparse-canonical field writes in ascending id order.
	f.line("  void serialize(sofab.Encoder e) {")
	for _, fld := range fields {
		g.emitMarshal(f, fld)
	}
	f.line("  }")

	f.blank()
	g.emitReset(f, fields)

	f.blank()
	g.emitIsDefault(f, fields)

	if isMessage {
		f.blank()
		f.line("  /// Worst-case serialized size (schema-bounded fields; a cap for")
		f.line("  /// unbounded ones).")
		ms := g.messageSize(name, fields)
		if !ms.Bounded {
			f.line("  // Configured ceiling (max_message_size): an unbounded field means this")
			f.line("  // size is imposed, not derived from the schema.")
			f.line("  static const int maxSizeLimit = %d;", ms.Size)
			f.line("  static const int maxSize = maxSizeLimit;")
		} else {
			f.line("  static const int maxSize = %d;", ms.Size)
		}
		f.line("  /// Serializes this message to a fresh byte buffer.")
		f.line("  Uint8List encode() => sofab.Encoder.encodeToBytes(serialize);")
		f.blank()
		// Streaming encode. [serialize] writes the fields and nothing else, so a
		// nested message can be written into an open frame; this is the entry
		// point for a caller who owns the encoder, and it flushes the tail the
		// last write left in the buffer. The Encoder's buffer may be smaller than
		// the message -- its flush callback drains it as it fills.
		f.line("  /// Encodes into an [sofab.Encoder] the caller owns, then flushes the tail.")
		f.line("  ///")
		f.line("  /// The encoder's buffer may be smaller than the message: it is drained")
		f.line("  /// through the flush callback as it fills, so what bounds memory is the")
		f.line("  /// buffer, not the message.")
		f.line("  void encodeTo(sofab.Encoder e) {")
		f.line("    serialize(e);")
		f.line("    e.flush();")
		f.line("  }")
		f.blank()
		f.line("  /// Status-surfacing one-shot decode: fills [out] and")
		f.line("  /// returns the terminal decode outcome. `invalid` covers both malformed")
		f.line("  /// bytes and a schema-bound violation (over-count/over-index/over-maxlen);")
		f.line("  /// `incomplete` means the bytes end inside a field or an open sequence.")
		f.line("  ///")
		f.line("  /// [out] is REUSABLE: it is [reset] to the declared defaults first. That")
		f.line("  /// reset is what makes a reused destination correct: a field equal to its")
		f.line("  /// default is not written to the wire at all, nested objects and arrays")
		f.line("  /// included, so nothing in the bytes can clear a value left over from an")
		f.line("  /// earlier decode.")
		f.line("  static sofab.DecodeStatus tryDecode(Uint8List data, %s out) {", name)
		f.line("    out.reset();")
		f.line("    return _decodeInto(data, out);")
		f.line("  }")
		f.blank()
		f.line("  /// Decodes into a destination the caller guarantees is already at its")
		f.line("  /// defaults, so [decode]'s fresh instance skips the redundant reset.")
		f.line("  static sofab.DecodeStatus _decodeInto(Uint8List data, %s out) {", name)
		f.line("    final e = _Dec();")
		f.line("    final st = sofab.Decoder.decode(data, %s(out, e)%s);", visitorName(name), g.limitsArg())
		f.line("    return e.inv ? sofab.DecodeStatus.invalid : st;")
		f.line("  }")
		f.blank()
		f.line("  /// Best-effort one-shot decode (the 90 %% case): returns the message with")
		f.line("  /// every field decoded so far, discarding the status. Prefer [tryDecode]")
		f.line("  /// when a truncated or malformed message must be distinguished.")
		f.line("  static %s decode(Uint8List data) {", name)
		f.line("    final m = %s();", name)
		f.line("    _decodeInto(data, m);")
		f.line("    return m;")
		f.line("  }")
		f.blank()
		// Streaming decode (PLAN S5.6). The corelib's Decoder is resumable and
		// reassembles a payload split across chunks itself, so nothing here has
		// to know about chunk boundaries; what was missing was a public handle,
		// since the generated visitor is library-private.
		f.line("  /// An incremental decoder filling [out]: hold it and feed chunks as they")
		f.line("  /// arrive, instead of buffering the whole message first.")
		f.line("  ///")
		f.line("  /// [out] is [reset] first, for the reason [tryDecode] resets: an absent")
		f.line("  /// field fires no callback, so a value left over from an earlier decode")
		f.line("  /// would survive.")
		f.line("  static %sDecoder decoder(%s out) {", name, name)
		f.line("    out.reset();")
		f.line("    return %sDecoder._(out);", name)
		f.line("  }")
	}
	f.line("}")
	f.blank()
	if isMessage {
		g.emitStreamDecoder(f, name)
	}

	g.emitVisitor(f, name, fields)
}

func visitorName(typeName string) string { return "_" + typeName + "Visitor" }

// ---- reset ----------------------------------------------------------------

// emitReset emits `reset()`, putting every field back to its declared default.
//
// MESSAGE_SPEC §2 omits a field whose value equals its default, and since the
// sequence carve-out is gone that now includes a struct/union member and a
// wrapper-array field. An omitted field fires NO decode callback, so the
// §7.4 "a later occurrence replaces the array whole" clear in onSequenceStart
// cannot run for it — decoding into a REUSED destination would keep the previous
// decode's elements. Clearing up front is the only place absence is still
// observable, so tryDecode calls this before handing the object to the corelib.
// (The §7.4 sequence-start clear stays exactly as it was: a re-opened wrapper
// must still replace, not append.)
//
// It works IN PLACE — a list is cleared and refilled rather than reallocated, so
// a reused destination keeps its backing storage, which is the point of the
// reuse entry point. Public: a caller driving the visitor itself needs the same
// ability (corelib-cpp exposes `IStreamImpl::reset()` for exactly this).
func (g *gen) emitReset(f *dfile, fields []*ir.Field) {
	f.line("  /// Restores every field to its declared default, in place.")
	f.line("  ///")
	f.line("  /// A field is only written during decode when the wire carries it, and a")
	f.line("  /// field equal to its default is not on the wire at all, so a destination")
	f.line("  /// must start from the defaults for an absent field to read back as its")
	f.line("  /// default. [tryDecode] does that for you; call this directly when driving")
	f.line("  /// the decode visitor yourself, or to recycle an instance.")
	f.line("  ///")
	f.line("  /// Lists are cleared and refilled rather than replaced, so a reused")
	f.line("  /// instance keeps its backing storage. (A list member assigned a")
	f.line("  /// fixed-length list by the caller is the one exception -- see the fp32")
	f.line("  /// array note in the generator docs.)")
	f.line("  void reset() {")
	for _, fld := range fields {
		g.emitResetField(f, fld)
	}
	f.line("  }")
}

func (g *gen) emitResetField(f *dfile, fld *ir.Field) {
	acc := dartIdent(fld.Name)
	switch fld.Kind {
	case ir.KindStruct, ir.KindUnion:
		// The member object survives; its own reset clears it recursively.
		f.line("    %s.reset();", acc)
	case ir.KindArray:
		if fld.Elem == ir.KindFP32 {
			// An fp32 array member holds a Float32List after decode (_f32copy, which
			// keeps a signaling NaN's raw bits). That is FIXED-LENGTH, so clear() would
			// throw — this one kind is reassigned instead of cleared.
			f.line("    %s = %s;", acc, g.dartDefaultValue(fld))
			return
		}
		// A `const` default literal is canonicalized once by the Dart compiler, so
		// refilling allocates nothing.
		if lit, ok := g.dartArrayLiteral(fld); ok {
			f.line("    %s..clear()..addAll(const %s);", acc, lit)
			return
		}
		// Every other array resets to EMPTY. A declared `count: N` adds nothing
		// here: N is a CAPACITY, never a length (MESSAGE_SPEC §3), so a fresh
		// count:N array holds no elements at all -- which is exactly what an absent
		// field decodes back to, keeping tryDecode's reused destination and
		// decode's fresh one in agreement.
		f.line("    %s.clear();", acc)
	case ir.KindFP32:
		// Drop any captured NaN wire bits with the value they belonged to (§4.6).
		f.line("    %s = %s;", acc, g.dartDefaultValue(fld))
		f.line("    %s = null;", fp32BitsField(fld.Name))
	default:
		// Scalars, strings and blobs are values: assignment IS the in-place reset
		// (a Uint8List is fixed-length and cannot be cleared).
		f.line("    %s = %s;", acc, g.dartDefaultValue(fld))
	}
}

// ---- all-default predicate -------------------------------------------------

// emitIsDefault emits the object's all-default predicate. It is the exact
// negation of what [serialize] writes: the object is default iff serialize would
// emit no child at all, evaluated per field and recursively (MESSAGE_SPEC §2).
//
// Keep this in lockstep with emitMarshal -- both are generated from
// fieldIsDefaultExpr's per-field expressions for exactly that reason. A
// predicate that disagrees with the writer omits a field that is on the wire, or
// keeps one that is not.
//
// Library-private (`_isDefault`) so it can never collide with a schema field
// name, and so the file-level `unused_element` ignore covers the classes that
// are never array elements.
func (g *gen) emitIsDefault(f *dfile, fields []*ir.Field) {
	f.line("  /// Whether every field equals its declared default, compared per field and")
	f.line("  /// recursively -- i.e. whether [serialize] would write no child at all.")
	f.line("  bool get _isDefault {")
	if len(fields) == 0 {
		f.line("    return true;")
		f.line("  }")
		return
	}
	for _, fld := range fields {
		f.line("    if (!(%s)) return false;", g.fieldIsDefaultExpr(fld))
	}
	f.line("    return true;")
	f.line("  }")
}

// fieldIsDefaultExpr is the boolean expression "this field equals its default",
// i.e. the negation of emitMarshal's write guard for the same field.
func (g *gen) fieldIsDefaultExpr(fld *ir.Field) string {
	acc := dartIdent(fld.Name)
	switch fld.Kind {
	case ir.KindBlob:
		if def, ok := g.blobDefaultLit(fld); ok {
			return fmt.Sprintf("_bytesEq(%s, %s)", acc, def)
		}
		return fmt.Sprintf("%s.isEmpty", acc)
	case ir.KindStruct, ir.KindUnion:
		// Lazily framed: the frame survives iff the nested serialize wrote a child,
		// which is exactly "the nested object is not default".
		return fmt.Sprintf("%s._isDefault", acc)
	case ir.KindArray:
		return g.arrayIsDefaultExpr(fld, acc)
	}
	// Scalars, strings, enums, bitfields, bools and fp32/fp64: serialize writes iff
	// `acc != default`. An fp32 NaN never equals the default, so the captured raw
	// bits ride along with a value that is already non-default.
	return fmt.Sprintf("%s == %s", acc, g.dartDefaultValue(fld))
}

// arrayIsDefaultExpr mirrors emitMarshalArray. An array's declared `count: N` is
// a CAPACITY, never a length (MESSAGE_SPEC §3), so it takes no part in this test:
// the value is compared against the declared default exactly as written, with no
// padding to N on either side, and against the empty collection when none is
// declared. A count:N array is therefore default only when it is EMPTY -- an
// all-zero N-element value is a length-N array, which differs from the empty one
// and stays on the wire.
func (g *gen) arrayIsDefaultExpr(fld *ir.Field, acc string) string {
	if nativeArrayElem(fld.Elem) {
		val := acc
		if fld.Elem == ir.KindBool {
			val = fmt.Sprintf("[for (final _b in %s) _b ? 1 : 0]", acc)
		}
		if def, ok := g.arrayDefaultLit(fld); ok {
			return fmt.Sprintf("_listEq(%s, %s)", val, def)
		}
		return fmt.Sprintf("%s.isEmpty", acc)
	}
	// Wrapper array: the writer emits a child for every element it holds, because
	// the LAST element is written whatever its value (§2) -- so "no child is
	// written" is exactly "the array is empty", and the two cannot drift apart.
	return fmt.Sprintf("%s.isEmpty", acc)
}

// ---- serialize --------------------------------------------------------------

func (g *gen) emitMarshal(f *dfile, fld *ir.Field) {
	acc := dartIdent(fld.Name)
	var write string
	switch fld.Kind {
	case ir.KindU8, ir.KindU16, ir.KindU32, ir.KindU64, ir.KindBitfield:
		write = fmt.Sprintf("e.writeUnsigned(%d, %s);", fld.ID, acc)
	case ir.KindI8, ir.KindI16, ir.KindI32, ir.KindI64, ir.KindEnum:
		write = fmt.Sprintf("e.writeSigned(%d, %s);", fld.ID, acc)
	case ir.KindBool:
		write = fmt.Sprintf("e.writeBool(%d, %s);", fld.ID, acc)
	case ir.KindFP32:
		// A NaN with captured bits re-emits bit-for-bit (writeFp32Bits); any other
		// value (incl. a user-set NaN with no captured bits) goes through writeFp32.
		// `acc != default` still gates omission: a NaN never equals the default.
		bits := fp32BitsField(fld.Name)
		f.line("    if (%s != %s) {", acc, g.dartDefaultValue(fld))
		f.line("      if (%s.isNaN && %s != null) { e.writeFp32Bits(%d, %s!); } else { e.writeFp32(%d, %s); }", acc, bits, fld.ID, bits, fld.ID, acc)
		f.line("    }")
		return
	case ir.KindFP64:
		write = fmt.Sprintf("e.writeFp64(%d, %s);", fld.ID, acc)
	case ir.KindString:
		write = fmt.Sprintf("e.writeString(%d, %s);", fld.ID, acc)
	case ir.KindBlob:
		// A blob is a leaf: omit when equal to its default (empty if none).
		if def, ok := g.blobDefaultLit(fld); ok {
			f.line("    if (!_bytesEq(%s, %s)) { e.writeBlob(%d, %s); }", acc, def, fld.ID, acc)
		} else {
			f.line("    if (%s.isNotEmpty) { e.writeBlob(%d, %s); }", acc, fld.ID, acc)
		}
		return
	case ir.KindStruct, ir.KindUnion:
		// MESSAGE_SPEC S2: the != default test is per field and a sequence is no
		// exception, so the frame is opened LAZILY -- the corelib holds the header
		// back until a child field appears. The nested serialize omits each child that
		// equals its default, so "no child was written" IS "the object equals its
		// declared default", evaluated per field and recursively. endSequence then
		// drops the frame, so an all-default nested object is omitted rather than
		// emitted as an empty wrapper.
		f.line("    e.beginSequenceLazy(%d); %s.serialize(e); e.endSequence();", fld.ID, acc)
		return
	case ir.KindArray:
		g.emitMarshalArray(f, fld, acc)
		return
	}
	// Scalar/string/enum/bitfield/bool leaf: omit when equal to the default.
	f.line("    if (%s != %s) { %s }", acc, g.dartDefaultValue(fld), write)
}

// blobDefaultLit is the Uint8List literal a blob field is compared against for
// omission when it has a non-empty schema default; ("", false) otherwise.
func (g *gen) blobDefaultLit(f *ir.Field) (string, bool) {
	init := g.dartInit(f)
	rhs := strings.TrimPrefix(init, " = ")
	if rhs == "Uint8List(0)" {
		return "", false
	}
	return rhs, true
}

func (g *gen) emitMarshalArray(f *dfile, fld *ir.Field, acc string) {
	// A native scalar array is a leaf field: omit when equal to its default, else
	// when empty. A composite/dynamic-element array is a wrapper sequence: opened
	// lazily and closed with the dropping end at field level, so an EMPTY one is
	// omitted rather than framed empty (MESSAGE_SPEC §2).
	//
	// A declared `count: N` takes no part in either test. `count` is a CAPACITY,
	// never a length (§3): it never reaches the wire, so the value is compared
	// against the declared default exactly as written -- neither side padded to N
	// -- and against the empty collection when no default is declared. Every
	// element the list holds is then written; nothing is elided from the tail,
	// because the wire count IS the array's length.
	if nativeArrayElem(fld.Elem) {
		val := acc
		if fld.Elem == ir.KindBool {
			val = fmt.Sprintf("[for (final _b in %s) _b ? 1 : 0]", acc)
		}
		if def, ok := g.arrayDefaultLit(fld); ok {
			f.line("    if (!_listEq(%s, %s)) { %s }", val, def, g.writeArrayStmt(fld, val))
		} else {
			f.line("    if (%s.isNotEmpty) { %s }", acc, g.writeArrayStmt(fld, val))
		}
		return
	}
	// Wrapper sequence (string/blob/struct/union/nested array). The field-level
	// wrapper frame is dropped when no element is written, and absence then
	// reconstructs the field's default. That is correct because a wrapper array's
	// declared `default` is not materialized today (the generated field starts as
	// the empty collection), so absent and explicitly-empty denote the same value.
	// If that gap is ever closed, this call needs a guard --
	// `if (value != default) { ...; e.endSequenceKeep(); }` -- so that a value
	// differing from a non-empty default still reaches the wire as the empty
	// wrapper, the only encoding of "explicitly empty" (MESSAGE_SPEC S2, S3).
	g.marshalWrapperArray(f, "    ", fmt.Sprintf("%d", fld.ID), acc, fld.Elem, fld.ElemRef, fld.ElemItems, 0, "")
}

// writeArrayStmt is the corelib call writing native-array expression `val` as
// field fld.ID (enum→signed, bool/bitfield→unsigned).
func (g *gen) writeArrayStmt(fld *ir.Field, val string) string {
	switch {
	case unsignedArrayElem(fld.Elem):
		return fmt.Sprintf("e.writeUnsignedArray(%d, %s);", fld.ID, val)
	case signedArrayElem(fld.Elem):
		return fmt.Sprintf("e.writeSignedArray(%d, %s);", fld.ID, val)
	case fld.Elem == ir.KindFP32:
		return fmt.Sprintf("e.writeFp32Array(%d, %s);", fld.ID, val)
	default: // fp64
		return fmt.Sprintf("e.writeFp64Array(%d, %s);", fld.ID, val)
	}
}

// marshalArrayElemType is the Dart element type of a native array's MARSHAL wire
// image, against which the omit-compare runs: a bool array is compared as its
// 0/1 integer image, so its compare list is <int>, not <bool>.
func (g *gen) marshalArrayElemType(elem ir.Kind) string {
	if elem == ir.KindBool {
		return "int"
	}
	return g.dartArrayElemType(elem, nil, nil)
}

// arrayDefaultLit is the full (untrimmed) default list literal a dynamic native
// array's value is compared against for omission; ("", false) when no default.
func (g *gen) arrayDefaultLit(f *ir.Field) (string, bool) {
	vals, ok := f.Default.([]any)
	if !ok {
		return "", false
	}
	parts := make([]string, len(vals))
	for i, v := range vals {
		parts[i] = g.marshalElemLit(f.Elem, v)
	}
	return fmt.Sprintf("<%s>[%s]", g.marshalArrayElemType(f.Elem), strings.Join(parts, ", ")), true
}

// marshalElemLit renders a native-array default element as it appears in the
// MARSHAL wire image (bool → 0/1 int), matching the trimmed/omit compare.
func (g *gen) marshalElemLit(elem ir.Kind, v any) string {
	if elem == ir.KindBool {
		if b, ok := v.(bool); ok && b {
			return "1"
		}
		return "0"
	}
	return g.elemLit(elem, v)
}

// lastElemExpr is the "this element is the array's last" test, at loop position
// iv over the value val.
//
// It is the whole of the positional half of MESSAGE_SPEC §2's element rule. A
// wrapper array carries no length field: its decoded length is *highest present
// id + 1* (§5.1), so the element at the highest index is the only one whose
// PRESENCE carries the length, and nothing that carries the length may be
// elided. Everything before it may be: an interior element equal to the element
// default is indistinguishable from an absent one, because the decoder restores
// an absent id from that same default. Hence: interior sparse, last always
// written.
//
// A declared `count: N` changes nothing here. N is a capacity, not a length
// (§3), so it can never restore an elided tail -- the same test applies with or
// without one.
func lastElemExpr(iv, val string) string {
	return fmt.Sprintf("%s == %s.length - 1", iv, val)
}

// emitSeqEnd closes the wrapper sequence opened at ind, choosing between the two
// closers corelib-dart offers. Every sequence is opened LAZILY (the corelib
// holds the header back until a child is written), so the closer alone decides
// whether a contentless one survives: endSequence drops it, endSequenceKeep
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
func emitSeqEnd(f *dfile, ind, keepIf string) {
	if keepIf == "" {
		f.line("%se.endSequence();", ind)
		return
	}
	f.line("%sif (%s) { e.endSequenceKeep(); } else { e.endSequence(); }", ind, keepIf)
}

// marshalWrapperArray writes a wrapper-sequence array (string/blob/struct/union
// or nested array). Elements are keyed by 0-based index.
//
// Every element the list holds is written -- no trailing run is elided, of
// either element kind, because the wire count IS the array's length (§3) and the
// highest wrapper id IS its last index (§5.1). What the interior may drop is a
// value that is indistinguishable from absence, and only that: an element equal
// to the element default leaves an id GAP the decoder restores from that same
// default, while the LAST element is always written -- as its value, or as an
// empty frame.
//
// keepIf is the closer this call's own wrapper takes (see emitSeqEnd).
func (g *gen) marshalWrapperArray(f *dfile, ind, idExpr, val string, elem ir.Kind, ref *ir.TypeRef, items *ir.ArrayElem, depth int, keepIf string) {
	iv := fmt.Sprintf("_i%d", depth)
	switch elem {
	case ir.KindString:
		// A string element is a leaf: in the array's INTERIOR it is omitted when it
		// equals the element default (empty), leaving an id gap the decoder restores
		// from that same default -- the ordinary sparse-field rule of MESSAGE_SPEC
		// §2, applied to an element. At the LAST index it is written whatever its
		// value: see lastElemExpr.
		f.line("%se.beginSequenceLazy(%s);", ind, idExpr)
		f.line("%sfor (var %s = 0; %s < %s.length; %s++) { if (%s[%s].isNotEmpty || %s) e.writeString(%s, %s[%s]); }", ind, iv, iv, val, iv, val, iv, lastElemExpr(iv, val), iv, val, iv)
		emitSeqEnd(f, ind, keepIf)
	case ir.KindBlob:
		// A blob element is a leaf, exactly like the string element above.
		f.line("%se.beginSequenceLazy(%s);", ind, idExpr)
		f.line("%sfor (var %s = 0; %s < %s.length; %s++) { if (%s[%s].isNotEmpty || %s) e.writeBlob(%s, %s[%s]); }", ind, iv, iv, val, iv, val, iv, lastElemExpr(iv, val), iv, val, iv)
		emitSeqEnd(f, ind, keepIf)
	case ir.KindStruct, ir.KindUnion:
		// A sequence-form element obeys the SAME rule as the leaf elements above --
		// one rule for both kinds -- and the lazily-held frame is where it is
		// applied. The nested serialize writes no child exactly when the element
		// equals its declared default, so the CLOSER alone decides: the dropping one
		// in the interior, where an all-default element vanishes into an id gap; the
		// keeping one at the last index, where it survives as an empty frame because
		// that presence is what fixes the array's length.
		f.line("%se.beginSequenceLazy(%s);", ind, idExpr)
		f.line("%sfor (var %s = 0; %s < %s.length; %s++) {", ind, iv, iv, val, iv)
		f.line("%s  e.beginSequenceLazy(%s); %s[%s].serialize(e);", ind, iv, val, iv)
		emitSeqEnd(f, ind+"  ", lastElemExpr(iv, val))
		f.line("%s}", ind)
		emitSeqEnd(f, ind, keepIf)
	case ir.KindArray:
		f.line("%se.beginSequenceLazy(%s);", ind, idExpr)
		f.line("%sfor (var %s = 0; %s < %s.length; %s++) {", ind, iv, iv, val, iv)
		if nativeArrayElem(items.Elem) {
			// A native row is a single count-prefixed value with no frame of its own,
			// so the rule lands on the WRITE rather than on a closer: an interior row
			// equal to the element default (the empty row) is not written at all, and
			// the last row always is.
			row := fmt.Sprintf("%s[%s]", val, iv)
			if items.Elem == ir.KindBool {
				row = fmt.Sprintf("[for (final _b in %s[%s]) _b ? 1 : 0]", val, iv)
			}
			f.line("%s  if (%s[%s].isNotEmpty || %s) %s", ind, val, iv, lastElemExpr(iv, val), g.writeRowStmt(items.Elem, iv, row))
		} else {
			// A wrapper row has its own frame, so it takes the closer instead -- the
			// same interior/last choice, expressed the same way as for a struct
			// element above.
			g.marshalWrapperArray(f, ind+"  ", iv, fmt.Sprintf("%s[%s]", val, iv), items.Elem, items.ElemRef, items.ElemItems, depth+1, lastElemExpr(iv, val))
		}
		f.line("%s}", ind)
		emitSeqEnd(f, ind, keepIf)
	}
}

// writeRowStmt writes one native-array row of a matrix as element `idExpr`.
func (g *gen) writeRowStmt(elem ir.Kind, idExpr, val string) string {
	switch {
	case unsignedArrayElem(elem):
		return fmt.Sprintf("e.writeUnsignedArray(%s, %s);", idExpr, val)
	case signedArrayElem(elem):
		return fmt.Sprintf("e.writeSignedArray(%s, %s);", idExpr, val)
	case elem == ir.KindFP32:
		return fmt.Sprintf("e.writeFp32Array(%s, %s);", idExpr, val)
	default:
		return fmt.Sprintf("e.writeFp64Array(%s, %s);", idExpr, val)
	}
}

// emitStreamDecoder writes the public incremental decoder: a handle on the
// corelib's resumable Decoder plus the destination it fills. The corelib
// suspends and resumes at any byte boundary AND reassembles a string/blob
// payload split across chunks into a fresh Uint8List of its own, so this class
// carries no parse state and borrows nothing from the fed chunks. What was
// missing was reach: the generated visitor is library-private (PLAN S5.6).
//
// Top-level rather than nested, because Dart has no nested classes; the private
// `._()` constructor keeps `decoder(out)` the only way to build one, so the
// destination is always reset first.
func (g *gen) emitStreamDecoder(f *dfile, name string) {
	f.line("/// Incremental decoder for [%s]: hold one and feed the message as", name)
	f.line("/// bytes arrive, instead of buffering it whole first.")
	f.line("///")
	f.line("/// The wire format has no end marker at the top level -- a message ends")
	f.line("/// where its bytes end -- so a feed cannot report that the MESSAGE is")
	f.line("/// complete, only that the bytes handed in ended on a field boundary")
	f.line("/// (`complete`) or mid-field (`incomplete`). Neither is a failure")
	f.line("/// mid-stream; the caller's own framing decides when the input is over, and")
	f.line("/// [finish] then gives the verdict for the message as a whole.")
	f.line("///")
	f.line("/// Nothing is borrowed from the chunks you feed: the corelib copies each")
	f.line("/// string/blob payload into storage of its own before it reaches the")
	f.line("/// destination, so a chunk may be reused as soon as [feed] returns.")
	f.line("class %sDecoder {", name)
	f.line("  %sDecoder._(this._out) : _e = _Dec() {", name)
	f.line("    _d = sofab.Decoder(%s(_out, _e)%s);", visitorName(name), g.limitsArg())
	f.line("  }")
	f.blank()
	f.line("  final %s _out;", name)
	f.line("  final _Dec _e;")
	f.line("  late final sofab.Decoder _d;")
	f.line("  sofab.DecodeStatus _st = sofab.DecodeStatus.complete;")
	f.blank()
	f.line("  /// Feeds the next chunk, of any size. `complete` means the bytes ended on")
	f.line("  /// a field boundary, `incomplete` mid-field -- neither answers whether the")
	f.line("  /// MESSAGE is done. `invalid` is terminal.")
	f.line("  sofab.DecodeStatus feed(List<int> chunk) {")
	f.line("    _st = _d.feed(chunk);")
	f.line("    return status;")
	f.line("  }")
	f.blank()
	f.line("  /// The outcome for everything fed so far, without feeding more.")
	f.line("  sofab.DecodeStatus get status =>")
	f.line("      _e.inv ? sofab.DecodeStatus.invalid : _st;")
	f.blank()
	f.line("  /// The destination, holding whatever has been decoded so far.")
	f.line("  %s get message => _out;", name)
	f.blank()
	f.line("  /// Takes the decoded message once the caller's framing says the input is")
	f.line("  /// over. Returns null if the stream ended mid-field or was rejected, so a")
	f.line("  /// half-filled value is never mistaken for a whole one; read [status] for")
	f.line("  /// which it was, or [message] to get it anyway.")
	f.line("  %s? finish() =>", name)
	f.line("      status == sofab.DecodeStatus.complete ? _out : null;")
	f.line("}")
	f.blank()
}
