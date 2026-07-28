// Package dart is the Dart throughput backend (PLAN §6.4): plain classes with a
// streaming `marshal` over the corelib Encoder and a push child-visitor decode
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
		emitDoc(f, "  ", fieldDoc(fld))
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

	// marshal: sparse-canonical field writes in ascending id order.
	f.line("  void marshal(sofab.Encoder e) {")
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
		f.line("  Uint8List encode() => sofab.Encoder.encodeToBytes(marshal);")
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
	}
	f.line("}")
	f.blank()

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
		// A `count: N` wrapper array is N elements long even when the field never
		// reaches the wire (S5.1), so reset restores the same N element defaults the
		// field initializer materializes -- otherwise tryDecode's reused destination
		// and decode's fresh one would disagree about an absent field. Not `const`:
		// a struct/union element must be a FRESH instance per reset, never shared.
		if lit, ok := g.dartWrapperFillLit(fld); ok {
			f.line("    %s..clear()..addAll(%s);", acc, lit)
			return
		}
		// A count-less wrapper array has no N to refill from: it resets to empty.
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
// negation of what [marshal] writes: the object is default iff marshal would
// emit no child at all, evaluated per field and recursively (MESSAGE_SPEC §2).
// Every generated class carries it so that a `count: N` wrapper array can find
// M -- one past its last non-default element -- BEFORE opening the element loop
// (§3/§5.1), which the implicit "no child was written" test the lazy framing
// encodes for a FIELD cannot answer in time.
//
// Keep this in lockstep with emitMarshal: a predicate that disagrees with the
// writer either drops a non-default element or keeps a default one. The wrapper
// -array arm shares the element-count expression with the marshal loop
// (elemCountExpr) so the two cannot drift apart.
//
// Library-private (`_isDefault`) so it can never collide with a schema field
// name, and so the file-level `unused_element` ignore covers the classes that
// are never array elements.
func (g *gen) emitIsDefault(f *dfile, fields []*ir.Field) {
	f.line("  /// Whether every field equals its declared default, compared per field and")
	f.line("  /// recursively -- i.e. whether [marshal] would write no child at all. A")
	f.line("  /// `count: N` array of sequence-form elements uses this to find how many of")
	f.line("  /// its elements the canonical encoding carries.")
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
		// Lazily framed: the frame survives iff the nested marshal wrote a child,
		// which is exactly "the nested object is not default".
		return fmt.Sprintf("%s._isDefault", acc)
	case ir.KindArray:
		return g.arrayIsDefaultExpr(fld, acc)
	}
	// Scalars, strings, enums, bitfields, bools and fp32/fp64: marshal writes iff
	// `acc != default`. An fp32 NaN never equals the default, so the captured raw
	// bits ride along with a value that is already non-default.
	return fmt.Sprintf("%s == %s", acc, g.dartDefaultValue(fld))
}

// arrayIsDefaultExpr mirrors emitMarshalArray: a native array compares against
// its (trimmed, for `count: N`) default, a wrapper array is default iff it would
// write no child -- i.e. its canonical element count M is zero.
func (g *gen) arrayIsDefaultExpr(fld *ir.Field, acc string) string {
	if nativeArrayElem(fld.Elem) {
		val := acc
		if fld.Elem == ir.KindBool {
			val = fmt.Sprintf("[for (final _b in %s) _b ? 1 : 0]", acc)
		}
		if fld.HasCount {
			trimmed := g.trimExpr(val, fld.Elem, true)
			if def, ok := g.trimmedDefaultLit(fld); ok {
				return fmt.Sprintf("_listEq(%s, %s)", trimmed, def)
			}
			return fmt.Sprintf("%s.isEmpty", trimmed)
		}
		if def, ok := g.arrayDefaultLit(fld); ok {
			return fmt.Sprintf("_listEq(%s, %s)", val, def)
		}
		return fmt.Sprintf("%s.isEmpty", acc)
	}
	// Wrapper array: no child is written iff every element it would write equals
	// the element default. fld.HasCount must be threaded through UNCHANGED --
	// narrowing here but not in the marshal loop (or the reverse) is exactly the
	// drift the shared elemCountExpr exists to prevent: it would call a dynamic
	// [{}] "default" while the writer still frames its one empty element, omitting
	// a field that is on the wire.
	return fmt.Sprintf("%s == 0", g.elemCountExpr(acc, fld.Elem, fld.ElemRef, fld.ElemItems, fld.HasCount))
}

// ---- marshal --------------------------------------------------------------

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
		// back until a child field appears. The nested marshal omits each child that
		// equals its default, so "no child was written" IS "the object equals its
		// declared default", evaluated per field and recursively. endSequence then
		// drops the frame, so an all-default nested object is omitted rather than
		// emitted as an empty wrapper.
		f.line("    e.beginSequenceLazy(%d); %s.marshal(e); e.endSequence();", fld.ID, acc)
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
	// A native scalar array is a leaf field: omit when equal to its (trimmed)
	// default, else when empty. A composite/dynamic-element array is a wrapper
	// sequence: opened lazily and closed with the dropping end at field level, so an
	// all-default one is omitted (MESSAGE_SPEC §2).
	if nativeArrayElem(fld.Elem) {
		val := acc
		if fld.Elem == ir.KindBool {
			val = fmt.Sprintf("[for (final _b in %s) _b ? 1 : 0]", acc)
		}
		trimmed := g.trimExpr(val, fld.Elem, fld.HasCount)
		if fld.HasCount {
			if def, ok := g.trimmedDefaultLit(fld); ok {
				f.line("    { final _t = %s; if (!_listEq(_t, %s)) { %s } }", trimmed, def, g.writeArrayStmt(fld, "_t"))
			} else {
				f.line("    { final _t = %s; if (_t.isNotEmpty) { %s } }", trimmed, g.writeArrayStmt(fld, "_t"))
			}
		} else {
			if def, ok := g.arrayDefaultLit(fld); ok {
				f.line("    if (!_listEq(%s, %s)) { %s }", val, def, g.writeArrayStmt(fld, val))
			} else {
				f.line("    if (%s.isNotEmpty) { %s }", acc, g.writeArrayStmt(fld, val))
			}
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
	g.marshalWrapperArray(f, "    ", fmt.Sprintf("%d", fld.ID), acc, fld.Elem, fld.ElemRef, fld.ElemItems, fld.HasCount, 0)
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

// trimExpr wraps a native-array expression in the trailing-default-run trim a
// fixed-count array's canonical encoding requires (MESSAGE_SPEC S3). Elements
// compare by BIT PATTERN for floats. A dynamic array has no N to refill from, so
// it is not trimmed.
func (g *gen) trimExpr(val string, elem ir.Kind, fixed bool) string {
	if !fixed {
		return val
	}
	switch elem {
	case ir.KindFP32:
		return fmt.Sprintf("_trimF32(%s)", val)
	case ir.KindFP64:
		return fmt.Sprintf("_trimF64(%s)", val)
	default:
		return fmt.Sprintf("_trimInt(%s)", val)
	}
}

// trimmedDefaultLit is the trimmed default list literal a fixed-count native
// array's value is compared against for whole-field omission; ("", false) when
// the field has no schema default (so the trimmed default is empty).
func (g *gen) trimmedDefaultLit(f *ir.Field) (string, bool) {
	vals, ok := f.Default.([]any)
	if !ok {
		return "", false
	}
	// Trim the trailing element-default run by bit pattern.
	n := len(vals)
	for n > 0 && g.isElemZero(f.Elem, vals[n-1]) {
		n--
	}
	if n == 0 {
		return "", false
	}
	parts := make([]string, n)
	for i := 0; i < n; i++ {
		parts[i] = g.marshalElemLit(f.Elem, vals[i])
	}
	return fmt.Sprintf("<%s>[%s]", g.marshalArrayElemType(f.Elem), strings.Join(parts, ", ")), true
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

func (g *gen) isElemZero(elem ir.Kind, v any) bool {
	switch elem {
	case ir.KindBool:
		b, _ := v.(bool)
		return !b
	case ir.KindFP32, ir.KindFP64:
		switch x := v.(type) {
		case float64:
			return x == 0 // note: +0.0 only; -0.0 has non-zero bits but == 0 here
		case int:
			return x == 0
		case int64:
			return x == 0
		}
		return false
	default:
		switch x := v.(type) {
		case int:
			return x == 0
		case int64:
			return x == 0
		case string:
			return x == "0"
		case float64:
			return x == 0
		}
		return false
	}
}

// elemCountExpr is the number of elements a wrapper array's canonical wire
// carries: M, one past its last element differing from the element default
// (MESSAGE_SPEC §3/§5.1, which says "even for sequence-form elements"). Only a
// declared `count: N` array is fixed-length and may be narrowed -- a DYNAMIC
// array has no N to refill from, so a trailing default ELEMENT is significant
// and must still be framed, and its count stays the plain length.
//
// Interior all-default elements are never dropped by this: element presence
// carries the length, so only the trailing run goes. Both the marshal loop and
// _isDefault run off this one expression, so the writer and the all-default
// predicate cannot disagree.
func (g *gen) elemCountExpr(val string, elem ir.Kind, ref *ir.TypeRef, items *ir.ArrayElem, fixed bool) string {
	if !fixed {
		return val + ".length"
	}
	switch elem {
	case ir.KindString, ir.KindBlob, ir.KindArray:
		// A string/blob leaf element and a nested row are default when empty. The
		// leaf writers already omit a default element individually, so narrowing
		// their loop does not change the bytes -- it exists so the all-default
		// predicate is computed from the very same expression the writer runs to.
		return fmt.Sprintf("_trimLen(%s, (x) => x.isEmpty)", val)
	case ir.KindStruct, ir.KindUnion:
		return fmt.Sprintf("_trimLen(%s, (x) => x._isDefault)", val)
	}
	return val + ".length"
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
// trailing run collapses as before -- the same `fixed` flag that keeps the leaf
// trim in elemCountExpr fixed-only, so writer and predicate cannot drift.
func lastElemGuard(iv, val string, fixed bool) string {
	if fixed {
		return ""
	}
	return fmt.Sprintf(" || %s == %s.length - 1", iv, val)
}

// marshalWrapperArray writes a wrapper-sequence array (string/blob/struct/union
// or nested array). Elements are keyed by 0-based index; string/blob leaves are
// omitted when equal to the element default (empty), except at the one position
// whose presence carries a dynamic array's length (see lastElemGuard). `fixed`
// marks a declared `count: N` array, whose canonical wire stops at M (see
// elemCountExpr).
func (g *gen) marshalWrapperArray(f *dfile, ind, idExpr, val string, elem ir.Kind, ref *ir.TypeRef, items *ir.ArrayElem, fixed bool, depth int) {
	iv := fmt.Sprintf("_i%d", depth)
	nv := fmt.Sprintf("_n%d", depth)
	// MESSAGE_SPEC S2: every sequence is opened lazily; the CLOSER decides whether a
	// contentless one survives, and it is chosen statically from the position in the
	// schema, never from the value. A wrapper array is a sequence-typed FIELD, so at
	// depth 0 it closes with the dropping endSequence -- an all-default array is
	// omitted and absence reconstructs it. A nested row (depth > 0) is an array
	// ELEMENT, and element presence is what carries a dynamic array's length (S5.1),
	// so it closes with endSequenceKeep: dropping an all-default row would change the
	// decoded length, not merely the bytes.
	seqEnd := "endSequence"
	if depth > 0 {
		seqEnd = "endSequenceKeep"
	}
	switch elem {
	case ir.KindString:
		f.line("%se.beginSequenceLazy(%s);", ind, idExpr)
		f.line("%sfor (var %s = 0; %s < %s.length; %s++) { if (%s[%s].isNotEmpty%s) e.writeString(%s, %s[%s]); }", ind, iv, iv, val, iv, val, iv, lastElemGuard(iv, val, fixed), iv, val, iv)
		f.line("%se.%s();", ind, seqEnd)
	case ir.KindBlob:
		f.line("%se.beginSequenceLazy(%s);", ind, idExpr)
		f.line("%sfor (var %s = 0; %s < %s.length; %s++) { if (%s[%s].isNotEmpty%s) e.writeBlob(%s, %s[%s]); }", ind, iv, iv, val, iv, val, iv, lastElemGuard(iv, val, fixed), iv, val, iv)
		f.line("%se.%s();", ind, seqEnd)
	case ir.KindStruct, ir.KindUnion:
		f.line("%se.beginSequenceLazy(%s);", ind, idExpr)
		f.line("%sfor (var %s = 0, %s = %s; %s < %s; %s++) {", ind, iv, nv, g.elemCountExpr(val, elem, ref, items, fixed), iv, nv, iv)
		// An INTERIOR element is framed unconditionally: dropping it would leave an
		// id gap and change the decoded length, not just the bytes (S5.1). The
		// TRAILING all-default run is already gone -- the loop runs to M, not to
		// length (S3/S5.1) -- and M == 0 writes no child at all, so the lazily-opened
		// wrapper is dropped by endSequence and the whole field is omitted (S2).
		f.line("%s  e.beginSequenceLazy(%s); %s[%s].marshal(e); e.endSequenceKeep();", ind, iv, val, iv)
		f.line("%s}", ind)
		f.line("%se.%s();", ind, seqEnd)
	case ir.KindArray:
		// The INNER row is an array ELEMENT, not a `count: N` field, so it is never
		// narrowed (MESSAGE_SPEC §3) -- the recursion passes fixed=false. The OUTER
		// container is a field and follows the same trailing-run rule as any other
		// wrapper array.
		f.line("%se.beginSequenceLazy(%s);", ind, idExpr)
		f.line("%sfor (var %s = 0, %s = %s; %s < %s; %s++) {", ind, iv, nv, g.elemCountExpr(val, elem, ref, items, fixed), iv, nv, iv)
		if nativeArrayElem(items.Elem) {
			row := fmt.Sprintf("%s[%s]", val, iv)
			if items.Elem == ir.KindBool {
				row = fmt.Sprintf("[for (final _b in %s[%s]) _b ? 1 : 0]", val, iv)
			}
			f.line("%s  %s", ind, g.writeRowStmt(items.Elem, iv, row))
		} else {
			g.marshalWrapperArray(f, ind+"  ", iv, fmt.Sprintf("%s[%s]", val, iv), items.Elem, items.ElemRef, items.ElemItems, false, depth+1)
		}
		f.line("%s}", ind)
		f.line("%se.%s();", ind, seqEnd)
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
