package typescript

import (
	"os"
	"strings"
	"testing"

	"github.com/sofa-buffers/generator/internal/analysis"
	"github.com/sofa-buffers/generator/internal/ir"
	"github.com/sofa-buffers/generator/internal/model"
	"github.com/sofa-buffers/generator/internal/parser"
)

func schema(t *testing.T, src string) *ir.Schema {
	t.Helper()
	doc, err := parser.Parse([]byte(src), "t.yaml")
	if err != nil {
		t.Fatal(err)
	}
	resolved, _ := doc.Resolve()
	if errs := parser.Validate(resolved); errs != nil {
		t.Fatalf("invalid: %v", errs)
	}
	s, err := model.Build(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := analysis.Analyze(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func genTS(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("../../examples/messages/example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	files, err := (&Backend{}).Generate(schema(t, string(b)), map[string]any{})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	for _, f := range files {
		if f.Path == "message.ts" {
			return string(f.Content)
		}
	}
	t.Fatal("no message.ts")
	return ""
}

// TestTSWireTypeGuard: the pull decoder frames each known field by the header
// wire type before reading (issue #160). A header whose wire type differs from
// the field's schema type is routed through c.skip(c.wire) — like an unknown id
// — instead of calling the schema-typed reader and desynchronizing the stream.
// Every field kind must emit the guard matching its encoded wire type
// (emitMarshal / marshalArray): scalars -> Unsigned/Signed/Fixlen, native arrays
// -> Array{Unsigned,Signed,Fixlen}, nested messages and composite arrays ->
// SequenceStart.
func TestTSWireTypeGuard(t *testing.T) {
	src := "version: 1\nmessages:\n  M:\n    payload:\n" +
		"      a: { id: 0, type: u8 }\n" +
		"      b: { id: 1, type: i32 }\n" +
		"      c: { id: 2, type: string }\n" +
		"      d: { id: 3, type: fp32 }\n" +
		"      e: { id: 4, type: struct, fields: { x: { id: 0, type: i32 } } }\n" +
		"      f: { id: 5, type: array, items: { type: u32 } }\n" +
		"      g: { id: 6, type: array, items: { type: i16 } }\n" +
		"      h: { id: 7, type: array, items: { type: fp64 } }\n" +
		"      i: { id: 8, type: array, items: { type: string } }\n"
	files, err := (&Backend{}).Generate(schema(t, src), map[string]any{})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	var mod string
	for _, f := range files {
		if f.Path == "message.ts" {
			mod = string(f.Content)
		}
	}
	if !strings.Contains(mod, `import { OStream, Cursor, WireType, FixlenSubtype }`) {
		t.Errorf("message.ts missing WireType/FixlenSubtype import:\n%s", mod)
	}
	for _, want := range []string{
		"case 0: if (c.wire !== WireType.Unsigned) { c.skip(c.wire); break; } o.a = Number(c.readUnsigned()); break;",
		"case 1: if (c.wire !== WireType.Signed) { c.skip(c.wire); break; } o.b = Number(c.readSigned()); break;",
		// fp32/fp64/string/blob share WireType.Fixlen, so the guard also checks the
		// fixlen subtype (corelib-ts#58); the fp arrays share ArrayFixlen likewise.
		"case 2: if (c.wire !== WireType.Fixlen || c.fixSub !== FixlenSubtype.String) { c.skip(c.wire); break; } o.c = c.readString(); break;",
		"case 3: if (c.wire !== WireType.Fixlen || c.fixSub !== FixlenSubtype.Fp32) { c.skip(c.wire); break; } o.d = c.readFp32(); break;",
		"case 4: if (c.wire !== WireType.SequenceStart) { c.skip(c.wire); break; } ME.decodeInto(c, o.e); break;", // nested message, decoded into the existing member (§7.4)
		"case 5: if (c.wire !== WireType.ArrayUnsigned) { c.skip(c.wire); break; } o.f = c.readUnsignedArray() as number[]; break;",
		"case 6: if (c.wire !== WireType.ArraySigned) { c.skip(c.wire); break; } o.g = c.readSignedArray() as number[]; break;",
		"case 7: if (c.wire !== WireType.ArrayFixlen || c.fixSub !== FixlenSubtype.Fp64) { c.skip(c.wire); break; } o.h = c.readFp64Array(); break;",
		"case 8: {\n        if (c.wire !== WireType.SequenceStart) { c.skip(c.wire); break; }", // composite array wrapper sequence (SequenceStart, no subtype)
	} {
		if !strings.Contains(mod, want) {
			t.Errorf("message.ts missing wire-type guard %q\n%s", want, mod)
		}
	}
}

// TestTSOverIndexWrapperArray: a fixed-count wrapper array (string/blob/struct
// elements) throws InvalidMsg for an element id >= N before the array grows
// (issue #142 / MESSAGE_SPEC §5.1/§7). A dynamic array keeps every index.
func TestTSOverIndexWrapperArray(t *testing.T) {
	src := "version: 1\nmessages:\n  M:\n    payload:\n" +
		"      bs: { id: 0, type: array, items: { type: string, count: 4, maxlen: 16 } }\n" +
		"      bb: { id: 1, type: array, items: { type: blob,   count: 3, maxlen: 16 } }\n" +
		"      bp: { id: 2, type: array, items: { type: struct, count: 2, fields: { x: { id: 0, type: i32 } } } }\n" +
		"      ds: { id: 3, type: array, items: { type: string } }\n"
	files, err := (&Backend{}).Generate(schema(t, src), map[string]any{})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	var mod string
	for _, f := range files {
		if f.Path == "message.ts" {
			mod = string(f.Content)
		}
	}
	// The over-index guard throws SofabError, so the on-demand import MUST be
	// emitted even for a wrapper-only schema with no scalar over-count array (the
	// #100 case) — missing it is a ReferenceError / tsc failure at decode.
	if !strings.Contains(mod, "SofabError, SofabErrorCode") {
		t.Errorf("message.ts must import SofabError/SofabErrorCode for the over-index guard:\n%s", mod[:min(len(mod), 200)])
	}
	for _, want := range []string{
		`if (c.id >= 4) throw new SofabError(SofabErrorCode.InvalidMsg, "arr: array index above schema capacity 4"); const _id = c.id; while (arr.length <= _id) arr.push("");`,
		`if (c.id >= 3) throw new SofabError(SofabErrorCode.InvalidMsg, "arr: array index above schema capacity 3"); const _id = c.id; while (arr.length <= _id) arr.push(new Uint8Array());`,
		// The struct-element path now places by id like the leaf paths above; the
		// guard runs first and so also bounds the gap-fill (generator#247).
		`if (c.id >= 2) throw new SofabError(SofabErrorCode.InvalidMsg, "arr: array index above schema capacity 2"); const _id = c.id; while (arr.length <= _id) arr.push(new MBpElem()); MBpElem.decodeInto(c, arr[_id]!);`,
	} {
		if !strings.Contains(mod, want) {
			t.Errorf("message.ts missing over-index guard %q", want)
		}
	}
	// Dynamic string array keeps every index (no over-index guard), but still
	// carries the §7.3 wrapper-element wire guard (#189).
	if !strings.Contains(mod, `while (c.readHeader()) { if ((c.wire as WireType) !== WireType.Fixlen || c.fixSub !== FixlenSubtype.String) { c.skip(c.wire); continue; } const _id = c.id; while (arr.length <= _id) arr.push(""); arr[_id] = c.readString(); }`) {
		t.Errorf("dynamic string array must carry the wire guard but no over-index guard:\n%s", mod)
	}
	if strings.Contains(mod, `while (c.readHeader()) { const _id = c.id; while (arr.length <= _id) arr.push("");`) {
		t.Error("dynamic string array must not be missing the §7.3 wrapper-element wire guard (#189)")
	}
}

// TestTSMaxlenReject: a string/blob whose wire byte length exceeds its schema
// maxlen is malformed and MUST be rejected as INVALID, never truncated
// (MESSAGE_SPEC §7.1). Covers scalar fields and bounded wrapper-string elements;
// an unbounded field keeps the bare read.
// TestTSHeaderBoundReject verifies the generator#216 / F-0032 fix: the schema
// count/maxlen is passed into the corelib reader (readUnsignedArray(N),
// readString(N)…) so an over-bound field is rejected as INVALID at the header word
// — before the reader's own truncated-field INCOMPLETE — making INVALID dominate a
// subsequent truncation (MESSAGE_SPEC §5.2). A dynamic (count-less) array passes no
// bound, preserving today's behavior. The `status` harness mode is emitted so the
// INVALID-vs-INCOMPLETE distinction is assertable in conformance.
func TestTSHeaderBoundReject(t *testing.T) {
	src := "version: 1\nmessages:\n  M:\n    payload:\n" +
		"      ua: { id: 0, type: array, items: { type: u32,  count: 4 } }\n" +
		"      fa: { id: 1, type: array, items: { type: fp32, count: 3 } }\n" +
		"      da: { id: 2, type: array, items: { type: u32 } }\n" // dynamic: no bound
	files, err := (&Backend{}).Generate(schema(t, src), map[string]any{"emit": "project"})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	var mod, harness string
	for _, f := range files {
		switch f.Path {
		case "message.ts":
			mod = string(f.Content)
		case "harness.ts":
			harness = string(f.Content)
		}
	}
	// Bounded native arrays pass their schema count into the reader (header reject);
	// the dynamic array passes nothing (unbounded — no header arg).
	for _, want := range []string{
		"c.readUnsignedArray(4) as number[]", // ua, count 4 -> header reject
		"c.readFp32Array(3)",                 // fa, count 3 -> header reject
	} {
		if !strings.Contains(mod, want) {
			t.Errorf("message.ts missing header-bound reader call %q:\n%s", want, mod)
		}
	}
	// The dynamic array must NOT gain a schema count (would wrongly reject a valid
	// long array): its reader stays argument-free.
	if !strings.Contains(mod, "o.da = c.readUnsignedArray() as number[]") {
		t.Errorf("dynamic array must keep the unbounded reader call:\n%s", mod)
	}
	// The harness exposes a `status` mode surfacing the §7 outcome so INVALID vs
	// INCOMPLETE is assertable (the bare decode mode only yields a non-zero exit).
	for _, want := range []string{`mode === "status"`, `"INVALID\n"`, `"INCOMPLETE\n"`} {
		if !strings.Contains(harness, want) {
			t.Errorf("harness.ts missing status-mode surface %q:\n%s", want, harness)
		}
	}
}

func TestTSMaxlenReject(t *testing.T) {
	src := "version: 1\nmessages:\n  M:\n    payload:\n" +
		"      s:  { id: 0, type: string, maxlen: 8 }\n" +
		"      b:  { id: 1, type: blob,   maxlen: 8 }\n" +
		"      u:  { id: 2, type: string }\n" +
		"      es: { id: 3, type: array, items: { type: string, maxlen: 5 } }\n"
	files, err := (&Backend{}).Generate(schema(t, src), map[string]any{})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	var mod string
	for _, f := range files {
		if f.Path == "message.ts" {
			mod = string(f.Content)
		}
	}
	// (a) A bounded string is the ONLY reject here (no counted/over-index array),
	// so the on-demand SofabError import MUST still fire — missing it is a tsc /
	// ReferenceError at decode. This is the import-gating regression guard.
	if !strings.Contains(mod, "SofabError, SofabErrorCode") {
		t.Errorf("message.ts must import SofabError/SofabErrorCode for the maxlen guard:\n%s", mod[:min(len(mod), 200)])
	}
	for _, want := range []string{
		// (b) Scalar string + blob reject on an over-length byte check.
		`case 0: { if (c.wire !== WireType.Fixlen || c.fixSub !== FixlenSubtype.String) { c.skip(c.wire); break; } const _s = c.readString(8); if (_utf8Len(_s) > 8) throw new SofabError(SofabErrorCode.InvalidMsg, "s: string byte length above schema maxlen 8"); o.s = _s; break; }`,
		`case 1: { if (c.wire !== WireType.Fixlen || c.fixSub !== FixlenSubtype.Blob) { c.skip(c.wire); break; } const _b = c.readBlob(8); if (_b.length > 8) throw new SofabError(SofabErrorCode.InvalidMsg, "b: blob byte length above schema maxlen 8"); o.b = _b; break; }`,
		// (c) A bounded wrapper-string element rejects on its element maxlen.
		`const _s = c.readString(); if (_utf8Len(_s) > 5) throw new SofabError(SofabErrorCode.InvalidMsg, "arr element: string byte length above schema maxlen 5"); arr[_id] = _s;`,
		// (e) The allocation-free byte-length helper is emitted once for the bounded
		// strings above, replacing the per-decode TextEncoder allocation (issue #153).
		"function _utf8Len(s: string): number {",
	} {
		if !strings.Contains(mod, want) {
			t.Errorf("message.ts missing maxlen guard %q\n%s", want, mod)
		}
	}
	// (d) An unbounded string keeps the bare read (never truncated, no guard).
	if !strings.Contains(mod, "case 2: if (c.wire !== WireType.Fixlen || c.fixSub !== FixlenSubtype.String) { c.skip(c.wire); break; } o.u = c.readString(); break;") {
		t.Errorf("unbounded string must keep the bare read:\n%s", mod)
	}
	// (f) The per-decode TextEncoder allocation is gone from the hot path (issue #153).
	if strings.Contains(mod, "TextEncoder") {
		t.Errorf("decode maxlen check must not allocate a TextEncoder per string (issue #153):\n%s", mod)
	}
}

func TestTSStructural(t *testing.T) {
	mod := genTS(t)
	for _, want := range []string{
		`import { OStream, Cursor, WireType, FixlenSubtype, SofabError, SofabErrorCode } from "@sofa-buffers/corelib";`, // FixlenSubtype: fixlen §7.3 guard (corelib-ts#58); SofabError: over-count reject (generator#100)
		"export class Myfirstmessage {",
		"marshal(os: OStream): void {",
		"static decode(bytes: Uint8Array): Myfirstmessage {",
		"return Myfirstmessage.decodeFrom(new Cursor(bytes));",
		"static decodeFrom(c: Cursor): Myfirstmessage {",
		"while (c.readHeader()) {",        // monomorphic pull loop
		"switch (c.id) {",                 // one switch per type
		"default: c.skip(c.wire); break;", // forward-compat skip
		"static decodeInto(c: Cursor, o: Myfirstmessage): Myfirstmessage {",
		// Nested message recursion decodes INTO the existing member, so a repeated
		// field id continues that scope instead of replacing it (MESSAGE_SPEC §7.4,
		// generator#175).
		"MyfirstmessageSomestruct.decodeInto(c, o.somestruct); break;",
		`while (c.readHeader()) { if ((c.wire as WireType) !== WireType.Fixlen || c.fixSub !== FixlenSubtype.String) { c.skip(c.wire); continue; } if (c.id >= 5) throw new SofabError(SofabErrorCode.InvalidMsg, "arr: array index above schema capacity 5"); const _id = c.id; while (arr.length <= _id) arr.push(""); const _s = c.readString(); if (_utf8Len(_s) > 16) throw new SofabError(SofabErrorCode.InvalidMsg, "arr element: string byte length above schema maxlen 16"); arr[_id] = _s; }`, // wrapper-element §7.3 wire guard (#189) + id-aware string-list, over-index + over-maxlen rejected (S2/S5.1/S7/S7.1, #142)
		"o.someu64 = c.readUnsigned() as bigint; break;", // u64 -> bigint, number-first
		// MESSAGE_SPEC §2: a struct/union FIELD opens lazily and closes with the
		// dropping end, so an all-default nested object is omitted, not framed empty.
		"    os.writeSequenceBeginLazy(20);\n    this.somestruct.marshal(os);\n    os.writeSequenceEnd();\n",
		"    os.writeSequenceBeginLazy(21);\n    this.someunion.marshal(os);\n    os.writeSequenceEnd();\n",
		// A wrapper-array FIELD is a sequence too: lazy + dropping end at depth 0,
		// while each ELEMENT keeps its frame (element presence carries the array's
		// length, §5.1).
		"    os.writeSequenceBeginLazy(23);\n    _trimObjs(this.somestructarray).forEach((_e0, _i0) => {\n      os.writeSequenceBeginLazy(_i0);\n      _e0.marshal(os);\n      os.writeSequenceEndKeep();\n    });\n    os.writeSequenceEnd();\n",
		"    os.writeSequenceBeginLazy(25);\n    _trimObjs(this.someunionarray).forEach((_e0, _i0) => {\n      os.writeSequenceBeginLazy(_i0);\n      _e0.marshal(os);\n      os.writeSequenceEndKeep();\n    });\n    os.writeSequenceEnd();\n",
		// A leaf string/blob wrapper array is a FIELD as well.
		"    os.writeSequenceBeginLazy(18);\n",
		"    os.writeSequenceBeginLazy(19);\n",
		"export enum MyfirstmessageSomeenum {",
	} {
		if !strings.Contains(mod, want) {
			t.Errorf("message.ts missing %q", want)
		}
	}
	// The megamorphic push/visitor decode is gone: no _visitor()/ChunkAcc, no
	// per-field visitor callbacks, no `decode`/`Visitor` import.
	for _, gone := range []string{
		"_visitor()", "ChunkAcc", "type Visitor", "sequenceBegin(",
		"stringListVisitor", "unsigned(id: number, value: bigint)",
		// The eager begin no longer exists in corelib-ts: every sequence is opened
		// with writeSequenceBeginLazy (MESSAGE_SPEC §2).
		"os.writeSequenceBegin(",
	} {
		if strings.Contains(mod, gone) {
			t.Errorf("message.ts should no longer emit %q (push/visitor decode removed)", gone)
		}
	}
	// Fast-encode marshal tidy-up: a leaf string list uses an indexed for (no
	// per-encode closure) rather than .forEach.
	if !strings.Contains(mod, "for (let _i0 = 0, _a0 = _trimStrs(this.somestringarray); _i0 < _a0.length; _i0++) {") {
		t.Error("message.ts missing indexed-for string-list marshal (fast-encode)")
	}
}

// TestTSLazySequenceFraming pins the MESSAGE_SPEC §2 closer table. Every sequence
// opens with writeSequenceBeginLazy; the CLOSER is chosen statically from the
// position in the schema, never from the value:
//
//	struct/union FIELD        -> writeSequenceEnd()      may vanish when all-default
//	array FIELD (the wrapper)  -> writeSequenceEnd()      may vanish when all-default
//	wrapper-array ELEMENT      -> writeSequenceEndKeep()  presence carries the length
//
// example.yaml has no array of composite rows, so the depth > 0 nested-row case
// (an ELEMENT that is itself a wrapper sequence) is only covered here.
func TestTSLazySequenceFraming(t *testing.T) {
	src := "version: 1\nmessages:\n  M:\n    payload:\n" +
		"      s:    { id: 0, type: struct, fields: { x: { id: 0, type: i32 } } }\n" +
		"      ss:   { id: 1, type: array, items: { type: struct, fields: { x: { id: 0, type: i32 } } } }\n" +
		"      strs: { id: 2, type: array, items: { type: string } }\n" +
		"      rows: { id: 3, type: array, items: { type: array, items: { type: string } } }\n" +
		"      blobs: { id: 4, type: array, items: { type: blob } }\n"
	mod := genTSWith(t, src, map[string]any{})

	// FIELDs: struct field, leaf-string wrapper array, blob wrapper array, and the
	// composite/nested-array wrappers — all closed with the dropping end.
	for _, want := range []string{
		"    os.writeSequenceBeginLazy(0);\n    this.s.marshal(os);\n    os.writeSequenceEnd();\n",
		// Both are DYNAMIC, so the run is walked untrimmed and the last element
		// escapes the omit test (§2, see TestTSDynamicArrayAlwaysWritesLastElement).
		"    os.writeSequenceBeginLazy(2);\n    for (let _i0 = 0, _a0 = this.strs; _i0 < _a0.length; _i0++) {",
		"    os.writeSequenceBeginLazy(4);\n    for (let _i0 = 0, _a0 = this.blobs; _i0 < _a0.length; _i0++) {",
		"    os.writeSequenceBeginLazy(1);\n    this.ss.forEach((_e0, _i0) => {\n      os.writeSequenceBeginLazy(_i0);\n      _e0.marshal(os);\n      os.writeSequenceEndKeep();\n    });\n    os.writeSequenceEnd();\n",
		// The nested row is an ELEMENT of `rows`, so its own wrapper keeps its
		// frame; the outer `rows` wrapper is a FIELD and may vanish.
		"    os.writeSequenceBeginLazy(3);\n    this.rows.forEach((_e0, _i0) => {\n      os.writeSequenceBeginLazy(_i0);\n      for (let _i1 = 0, _a1 = _e0; _i1 < _a1.length; _i1++) {\n        if (_a1[_i1]! !== \"\" || _i1 === _a1.length - 1) {\n          os.writeString(_i1, _a1[_i1]!);\n        }\n      }\n      os.writeSequenceEndKeep();\n    });\n    os.writeSequenceEnd();\n",
	} {
		if !strings.Contains(mod, want) {
			t.Errorf("message.ts missing lazy-framing shape %q\n%s", want, mod)
		}
	}
	// The eager begin is gone from corelib-ts entirely: emitting it would not
	// compile.
	if strings.Contains(mod, "os.writeSequenceBegin(") {
		t.Error("message.ts must not emit the removed eager os.writeSequenceBegin()")
	}
	// Exactly one keeping close per ELEMENT site (two struct/row element frames,
	// one per array), and no keeping close on any FIELD wrapper.
	if got, want := strings.Count(mod, "os.writeSequenceEndKeep();"), 2; got != want {
		t.Errorf("writeSequenceEndKeep() count = %d, want %d (one per wrapper-array element site)", got, want)
	}
	if got, want := strings.Count(mod, "os.writeSequenceBeginLazy("), 7; got != want {
		t.Errorf("writeSequenceBeginLazy() count = %d, want %d (5 fields + 2 element frames)", got, want)
	}
}

func TestTSDeterministic(t *testing.T) {
	if genTS(t) != genTS(t) {
		t.Fatal("TS generation not deterministic")
	}
}

// int64Def exercises every 64-bit shape the `int64` config modes change:
// scalars, arrays (with and without a schema default), and a nested array.
const int64Def = `
version: 1
messages:
  m:
    payload:
      us:   { id: 0, type: array, items: { type: u64, count: 8 } }
      is:   { id: 1, type: array, items: { type: i64, count: 8 } }
      ud:   { id: 2, type: array, items: { type: u64, count: 2 }, default: [1, "18446744073709551615"] }
      rows: { id: 3, type: array, items: { type: array, count: 2, items: { type: i64, count: 2 } } }
      u:    { id: 4, type: u64 }
      i:    { id: 5, type: i64, default: -7 }
`

func genTSWith(t *testing.T, src string, cfg map[string]any) string {
	t.Helper()
	files, err := (&Backend{}).Generate(schema(t, src), cfg)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	for _, f := range files {
		if f.Path == "message.ts" {
			return string(f.Content)
		}
	}
	t.Fatal("no message.ts")
	return ""
}

func TestTSInt64Long(t *testing.T) {
	mod := genTSWith(t, int64Def, map[string]any{"int64": "long"})
	for _, want := range []string{
		`import { OStream, Cursor, WireType, Long, SofabError, SofabErrorCode } from "@sofa-buffers/corelib";`,
		// Long[] backing field + accessor pair; setter converts once. us is
		// `count: 8`, so its implied default is 8 Long zeros (issue#136).
		"private _us: Long[] = [Long.ZERO, Long.ZERO, Long.ZERO, Long.ZERO, Long.ZERO, Long.ZERO, Long.ZERO, Long.ZERO];",
		"get us(): Long[] { return this._us; }",
		"set us(vals: readonly (Long | bigint | number)[]) { this._us = vals.map(Long.fromValue); }",
		// Nested array: Long[][] with a per-row setter conversion. It is `count: 2`,
		// so it is materialized to two rows — the row default is the empty array,
		// the very value the outer sequence's N-fill grows it with.
		"private _rows: Long[][] = [[], []];",
		"set rows(vals: readonly (readonly (Long | bigint | number)[])[]) { this._rows = vals.map((_v0) => _v0.map(Long.fromValue)); }",
		// Marshal reads the backing field; 64-bit arrays use the Long writers.
		// These are `count: N` fields, so the trailing default run is trimmed
		// (issue#136) by the Long flavour of the trim (word-pair compare). The
		// omission guard compares against the implied N-element default.
		"if (!longArrEq(this._us, [Long.ZERO, Long.ZERO, Long.ZERO, Long.ZERO, Long.ZERO, Long.ZERO, Long.ZERO, Long.ZERO])) {",
		"os.writeUnsignedArrayLong(0, _trimTailLong(this._us));",
		"os.writeSignedArrayLong(1, _trimTailLong(this._is));",
		"function _trimTailLong(a: readonly Long[]): readonly Long[] {",
		// Defaulted Long array: materialized Long default + longArrEq guard.
		`private _ud: Long[] = [Long.fromValue(1n), Long.fromValue(18446744073709551615n)];`,
		"if (!longArrEq(this._ud, [Long.fromValue(1n), Long.fromValue(18446744073709551615n)])) {",
		"function longArrEq(a: readonly Long[], b: readonly Long[]): boolean {",
		// Decode bypasses the setter (readers return canonical Long[]); a wire
		// count above the schema capacity rejects as INVALID (generator#100), and a
		// wire count below it refills the elided trailing default run (issue#136).
		`case 0: { if (c.wire !== WireType.ArrayUnsigned) { c.skip(c.wire); break; } const _a = c.readUnsignedArrayLong(8); if (_a.length > 8) throw new SofabError(SofabErrorCode.InvalidMsg, "us: array count above schema capacity 8"); o._us = _padTo(_a, 8, Long.ZERO); break; }`,
		`case 1: { if (c.wire !== WireType.ArraySigned) { c.skip(c.wire); break; } const _a = c.readSignedArrayLong(8); if (_a.length > 8) throw new SofabError(SofabErrorCode.InvalidMsg, "is: array count above schema capacity 8"); o._is = _padTo(_a, 8, Long.ZERO); break; }`,
		// toJSON prints via Long.toString with the schema signedness.
		`"us": this._us.map((_x0) => _x0.toString(false)),`,
		`"is": this._is.map((_x0) => _x0.toString(true)),`,
		// fromJSON keeps the bigint parse and lets the setter convert once.
		`if ("us" in d) o.us = (d["us"] as (string | number)[]).map((_x0) => BigInt(_x0));`,
		// Scalars stay bigint in long mode (no scalar Long codec in corelib yet).
		"u: bigint = 0n;",
		"i: bigint = -7n;",
		"case 4: if (c.wire !== WireType.Unsigned) { c.skip(c.wire); break; } o.u = c.readUnsigned() as bigint; break;",
	} {
		if !strings.Contains(mod, want) {
			t.Errorf("int64: long message.ts missing %q", want)
		}
	}
	for _, gone := range []string{"bigint[]", "writeUnsignedArray(0", "readUnsignedArray()"} {
		if strings.Contains(mod, gone) {
			t.Errorf("int64: long message.ts should not emit %q", gone)
		}
	}
}

func TestTSInt64Number(t *testing.T) {
	mod := genTSWith(t, int64Def, map[string]any{"int64": "number"})
	for _, want := range []string{
		// Arrays are Long-backed exactly as in long mode.
		"os.writeUnsignedArrayLong(0, _trimTailLong(this._us));",
		`case 0: { if (c.wire !== WireType.ArrayUnsigned) { c.skip(c.wire); break; } const _a = c.readUnsignedArrayLong(8); if (_a.length > 8) throw new SofabError(SofabErrorCode.InvalidMsg, "us: array count above schema capacity 8"); o._us = _padTo(_a, 8, Long.ZERO); break; }`,
		// Scalars are plain numbers: number default, !== 0 guard, Number() decode.
		"u: number = 0;",
		"i: number = -7;",
		"if (this.u !== 0) {",
		"os.writeUnsigned(4, this.u);",
		"case 4: if (c.wire !== WireType.Unsigned) { c.skip(c.wire); break; } o.u = Number(c.readUnsigned()); break;",
		"case 5: if (c.wire !== WireType.Signed) { c.skip(c.wire); break; } o.i = Number(c.readSigned()); break;",
		`if ("u" in d) o.u = Number(d["u"] as string | number);`,
		// toJSON stays a string (number.toString()) for cross-mode JSON parity.
		`"u": this.u.toString(),`,
	} {
		if !strings.Contains(mod, want) {
			t.Errorf("int64: number message.ts missing %q", want)
		}
	}
}

// TestTSDecodeLimits: the max_dyn_* config keys bake receiver-side decode
// limits (generator#102) into the generated module — exported MAX_DYN_*
// constants referenced by the DecodeLimits object every static decode() passes
// to its Cursor. The cap is raised to the largest schema bound of its kind
// (escape hatch: schema-bounded fields stay governed by their own bound), an
// unset key emits nothing, a key whose kind has no unbounded field is inert,
// and the plumbing is identical across all three int64 modes.
func TestTSDecodeLimits(t *testing.T) {
	const src = `
version: 1
messages:
  dyn:
    payload:
      s:    { id: 0, type: string }
      arr:  { id: 1, type: array, items: { type: u64 } }
      barr: { id: 2, type: array, items: { type: i32, count: 100000 } }
`
	for _, mode := range []string{"bigint", "long", "number"} {
		mod := genTSWith(t, src, map[string]any{
			"int64":               mode,
			"max_dyn_array_count": 65536,
			"max_dyn_string_len":  4096,
			"max_dyn_blob_len":    2048, // no unbounded blob in the schema -> inert
		})
		for _, want := range []string{
			"export const MAX_DYN_ARRAY_COUNT = 100000;", // raised to the schema count of barr
			"export const MAX_DYN_STRING_LEN = 4096;",
			"return Dyn.decodeFrom(new Cursor(bytes, { maxArrayCount: MAX_DYN_ARRAY_COUNT, maxStringLen: MAX_DYN_STRING_LEN }));",
		} {
			if !strings.Contains(mod, want) {
				t.Errorf("int64: %s message.ts missing %q", mode, want)
			}
		}
		if strings.Contains(mod, "MAX_DYN_BLOB_LEN") {
			t.Errorf("int64: %s: inert blob limit must not be emitted (no unbounded blob)", mode)
		}
	}

	// No limits configured -> byte-identical plumbing-free output.
	plain := genTSWith(t, src, map[string]any{})
	if strings.Contains(plain, "MAX_DYN") || strings.Contains(plain, "maxArrayCount") {
		t.Error("unset limits must emit no limit plumbing")
	}
	if !strings.Contains(plain, "return Dyn.decodeFrom(new Cursor(bytes));") {
		t.Error("unset limits must keep the bare Cursor construction")
	}
}

// fixedCountDef pairs a `count: N` field with a dynamic (count-less) one for
// every native element kind the trailing-default-run rule touches, plus a
// nested array-of-array and a non-native (string) element array — neither of
// which is in scope.
const fixedCountDef = `
version: 1
$defs:
  enum:
    Mode: { Off: { value: 0 }, Active: { value: 1 } }
  bitfield:
    Flags: { ready: { pos: 0 } }
messages:
  m:
    payload:
      fu32:  { id: 0, type: array, items: { type: u32, count: 5 } }
      du32:  { id: 1, type: array, items: { type: u32 } }
      fi16:  { id: 2, type: array, items: { type: i16, count: 3 } }
      ffp32: { id: 3, type: array, items: { type: fp32, count: 3 } }
      ffp64: { id: 4, type: array, items: { type: fp64, count: 3 } }
      dfp64: { id: 5, type: array, items: { type: fp64 } }
      fbool: { id: 6, type: array, items: { type: boolean, count: 4 } }
      dbool: { id: 7, type: array, items: { type: boolean } }
      fenum: { id: 8, type: array, items: { type: enum, count: 2, enum: { $ref: "#/$defs/enum/Mode" } } }
      fbits: { id: 9, type: array, items: { type: bitfield, count: 2, bits: { $ref: "#/$defs/bitfield/Flags" } } }
      rows:  { id: 10, type: array, items: { type: array, count: 2, items: { type: u32, count: 3 } } }
      fstr:  { id: 11, type: array, items: { type: string, count: 2, maxlen: 8 } }
`

// TestTSFixedCountTrailingDefaultRun: a `count: N` native array is FIXED-LENGTH
// (MESSAGE_SPEC §3) — the encoder emits only through the last non-default
// element and the decoder refills [M, N) with the element default (issue#136).
// Dynamic arrays keep every element (a trailing default is significant there),
// and neither nested rows nor wrapper-sequence (string) element arrays are in
// scope.
func TestTSFixedCountTrailingDefaultRun(t *testing.T) {
	mod := genTSWith(t, fixedCountDef, map[string]any{})
	for _, want := range []string{
		// Encode: fixed-count native arrays trim, one form per element kind.
		"os.writeUnsignedArray(0, _trimTail(this.fu32, 0));",
		"os.writeSignedArray(2, _trimTail(this.fi16, 0));",
		"os.writeFp32Array(3, _trimTail(this.ffp32, 0));",
		"os.writeFp64Array(4, _trimTail(this.ffp64, 0));",
		"os.writeUnsignedArray(6, _trimTail(this.fbool.map((_e0) => (_e0 ? 1 : 0)), 0));",
		"os.writeSignedArray(8, _trimTail(this.fenum, 0 as EnumMode));",
		"os.writeUnsignedArray(9, _trimTail(this.fbits, 0));",
		// Decode: refill to exactly the schema count, after the over-count reject.
		`case 0: { if (c.wire !== WireType.ArrayUnsigned) { c.skip(c.wire); break; } const _a = c.readUnsignedArray(5) as number[]; if (_a.length > 5) throw new SofabError(SofabErrorCode.InvalidMsg, "fu32: array count above schema capacity 5"); o.fu32 = _padTo(_a, 5, 0); break; }`,
		`o.ffp64 = _padTo(_a, 3, 0); break; }`,
		`o.fbool = _padTo(_a, 4, false); break; }`,
		`o.fenum = _padTo(_a, 2, 0 as EnumMode); break; }`,
		// The default test is a BIT-PATTERN compare (Object.is), so a trailing
		// -0 / NaN is not a default and is never trimmed away.
		"while (n > 0 && Object.is(a[n - 1], zero)) n--;",
		"function _padTo<T>(a: T[], n: number, zero: T): T[] {",
	} {
		if !strings.Contains(mod, want) {
			t.Errorf("fixed-count message.ts missing %q", want)
		}
	}
	for _, gone := range []string{
		// Dynamic (count-less) arrays: no trim on encode, no refill on decode.
		"_trimTail(this.du32", "_trimTail(this.dfp64", "_trimTail(this.dbool",
		"os.writeUnsignedArray(1, _trimTail", "os.writeFp64Array(5, _trimTail",
		"o.du32 = _padTo", "o.dfp64 = _padTo", "o.dbool = _padTo",
		// A nested row is not a field: rows are never trimmed.
		"_trimTail(_e0", "_trimTail(_e1",
		// === would trim a trailing -0.0 (bit-pattern-distinct from +0.0).
		"=== 0) n--", "!== 0) n--",
	} {
		if strings.Contains(mod, gone) {
			t.Errorf("fixed-count message.ts should not emit %q", gone)
		}
	}
	// Dynamic arrays keep their plain writer call unchanged.
	for _, want := range []string{
		"os.writeUnsignedArray(1, this.du32);",
		"os.writeFp64Array(5, this.dfp64);",
		"os.writeUnsignedArray(7, this.dbool.map((_e0) => (_e0 ? 1 : 0)));",
		// Nested rows lower to the untrimmed inner writer.
		"os.writeUnsignedArray(_i0, _e0);",
	} {
		if !strings.Contains(mod, want) {
			t.Errorf("fixed-count message.ts missing unchanged dynamic form %q", want)
		}
	}
	// A wrapper-sequence (string) element array is out of scope even with count.
	if strings.Contains(mod, "_trimTail(this.fstr") || strings.Contains(mod, "o.fstr = _padTo") {
		t.Error("string-element arrays are wrapper sequences: must not trim/pad")
	}
}

// fixedDefaultDef pairs counted arrays that have no schema default, a SHORT
// schema default, and an exactly-N default, against a dynamic control.
const fixedDefaultDef = `
version: 1
$defs:
  enum:
    Mode: { Off: { value: 0 }, Active: { value: 1 } }
messages:
  m:
    payload:
      none:  { id: 0, type: array, items: { type: u32, count: 5 } }
      short: { id: 1, type: array, items: { type: u32, count: 5 }, default: [1, 2] }
      exact: { id: 2, type: array, items: { type: u32, count: 3 }, default: [1, 2, 3] }
      dyn:   { id: 3, type: array, items: { type: u32 } }
      dynd:  { id: 4, type: array, items: { type: u32 }, default: [1, 2] }
      fb:    { id: 5, type: array, items: { type: boolean, count: 3 }, default: [true] }
      ff:    { id: 6, type: array, items: { type: fp64, count: 2 } }
      fe:    { id: 7, type: array, items: { type: enum, count: 2, enum: { $ref: "#/$defs/enum/Mode" } } }
      fu64:  { id: 8, type: array, items: { type: u64, count: 3 }, default: [1] }
      strs:  { id: 9, type: array, items: { type: string, count: 2, maxlen: 8 } }
      dstrs: { id: 10, type: array, items: { type: string, maxlen: 8 } }
`

// TestTSFixedCountDefaultLength: a `count: N` array is FIXED-LENGTH, so its
// value is always exactly N elements (MESSAGE_SPEC §3) — with no schema default
// that is N element defaults, and a short schema default is tail-padded to N.
// This matches the fixed-storage backends' zero-filled `[T; N]`. Reached through
// the omission path: an all-default array never touches the wire, so without the
// materialized default it would decode back empty here and as N zeros there.
func TestTSFixedCountDefaultLength(t *testing.T) {
	mod := genTSWith(t, fixedDefaultDef, map[string]any{})
	for _, want := range []string{
		// No schema default -> N element defaults, per element kind.
		"none: number[] = [0, 0, 0, 0, 0];",
		"ff: number[] = [0, 0];",
		"fe: EnumMode[] = [(0 as EnumMode), (0 as EnumMode)];",
		// Short schema default -> tail-padded to N.
		"short: number[] = [1, 2, 0, 0, 0];",
		"fb: boolean[] = [true, false, false];",
		"fu64: bigint[] = [1n, 0n, 0n];",
		// Exactly-N default is unchanged.
		"exact: number[] = [1, 2, 3];",
		// The omission guard compares against that same materialized default, so
		// an all-default fixed array is omitted whole (no bytes at all).
		"if (!arrEq(this.none, [0, 0, 0, 0, 0])) {",
		"if (!arrEq(this.short, [1, 2, 0, 0, 0])) {",
		// A count:N WRAPPER array is fixed-length for exactly the same reason, so it
		// is materialized to N element defaults alongside the native ones. This line
		// used to read `strs: string[] = [];`, which pinned the pre-fill behaviour:
		// the field then constructed empty but decoded at N as soon as one element
		// was on the wire.
		`strs: string[] = ["", ""];`,
	} {
		if !strings.Contains(mod, want) {
			t.Errorf("fixed-default message.ts missing %q", want)
		}
	}
	for _, gone := range []string{
		// Dynamic arrays are NOT fixed-length: no synthesized default, no padding.
		"dyn: number[] = [0",
		"dynd: number[] = [1, 2, 0",
		// ...and that holds for a wrapper element kind too.
		`dstrs: string[] = [""`,
	} {
		if strings.Contains(mod, gone) {
			t.Errorf("fixed-default message.ts should not emit %q", gone)
		}
	}
	for _, want := range []string{
		"dyn: number[] = [];",      // no default, dynamic -> empty
		"dynd: number[] = [1, 2];", // dynamic default kept verbatim (not padded)
		"dstrs: string[] = [];",
	} {
		if !strings.Contains(mod, want) {
			t.Errorf("fixed-default message.ts missing unchanged dynamic form %q", want)
		}
	}
}

// TestTSFixedCountDefaultLong: the Long-backed 64-bit modes materialize the same
// N-element default as Long values (and compare it with longArrEq).
func TestTSFixedCountDefaultLong(t *testing.T) {
	for _, mode := range []string{"long", "number"} {
		mod := genTSWith(t, fixedDefaultDef, map[string]any{"int64": mode})
		for _, want := range []string{
			"private _fu64: Long[] = [Long.fromValue(1n), Long.ZERO, Long.ZERO];",
			"if (!longArrEq(this._fu64, [Long.fromValue(1n), Long.ZERO, Long.ZERO])) {",
		} {
			if !strings.Contains(mod, want) {
				t.Errorf("int64: %s fixed-default message.ts missing %q", mode, want)
			}
		}
	}
}

// TestTSNoFixedCountNoHelpers: a schema without any fixed-count native array
// must not carry the trim/pad helpers (they would be dead code).
// TestTSFixlenSubtypeImportMatchesUse is the durable form of generator#246:
// rather than pinning one import line per shape, it asserts the INVARIANT the
// gate exists for — the module imports FixlenSubtype exactly when its body
// references it. schemaHasFixlenGuard used to inspect field kinds plus one level
// of NATIVE array element only, so a schema whose sole fixlen use is a wrapper
// ELEMENT guard (array<string>, or a nested array<array<fp32>> row) emitted a
// module naming a symbol it never imported: ReferenceError at decode, and tsc
// fails on it.
func TestTSFixlenSubtypeImportMatchesUse(t *testing.T) {
	cases := []struct {
		name string
		want bool // FixlenSubtype expected in the import line
		src  string
	}{
		{"wrapper string array", true, `
      tags: { id: 0, type: array, items: { type: string } }
      n:    { id: 1, type: u32 }`},
		{"wrapper blob array", true, `
      parts: { id: 0, type: array, items: { type: blob } }
      n:     { id: 1, type: u32 }`},
		{"nested string rows", true, `
      rows: { id: 0, type: array, items: { type: array, items: { type: string } } }
      n:    { id: 1, type: u32 }`},
		{"nested fp32 rows", true, `
      grid: { id: 0, type: array, items: { type: array, items: { type: fp32 } } }
      n:    { id: 1, type: u32 }`},
		{"doubly nested blob rows", true, `
      cube: { id: 0, type: array, items: { type: array, items: { type: array, items: { type: blob } } } }
      n:    { id: 1, type: u32 }`},
		// Reached through a struct element: guarded inside that struct's own class,
		// which the named-type walk already covers.
		{"string inside a struct element", true, `
      items: { id: 0, type: array, items: { type: struct, fields: { s: { id: 0, type: string } } } }
      n:     { id: 1, type: u32 }`},
		// Negatives — the import must stay out, else every module carries a dead name.
		{"native integer array", false, `
      a: { id: 0, type: array, items: { type: u32 } }
      n: { id: 1, type: u32 }`},
		{"nested integer rows", false, `
      m: { id: 0, type: array, items: { type: array, items: { type: i32 } } }
      n: { id: 1, type: u32 }`},
		{"struct elements without fixlen", false, `
      items: { id: 0, type: array, items: { type: struct, fields: { x: { id: 0, type: i32 } } } }
      n:     { id: 1, type: u32 }`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mod := genTSWith(t, "version: 1\nmessages:\n  M:\n    payload:"+tc.src+"\n", map[string]any{})
			imp, body, ok := strings.Cut(mod, "\n")
			for ok && !strings.HasPrefix(imp, "import {") {
				imp, body, ok = strings.Cut(body, "\n")
			}
			if !ok {
				t.Fatalf("no corelib import line in:\n%s", mod)
			}
			imported := strings.Contains(imp, "FixlenSubtype")
			used := strings.Contains(body, "FixlenSubtype")
			if used != tc.want {
				t.Fatalf("expected the emitted guards to reference FixlenSubtype=%v; module:\n%s", tc.want, mod)
			}
			if imported != used {
				t.Errorf("import/use mismatch: imported=%v used=%v (imported without use = dead name; used without import = ReferenceError + tsc failure)\n%s",
					imported, used, mod)
			}
		})
	}
}

func TestTSNoFixedCountNoHelpers(t *testing.T) {
	const src = `
version: 1
messages:
  dyn:
    payload:
      a: { id: 0, type: array, items: { type: u32 } }
      s: { id: 1, type: string }
`
	mod := genTSWith(t, src, map[string]any{})
	for _, gone := range []string{"_trimTail", "_trimTailLong", "_padTo"} {
		if strings.Contains(mod, gone) {
			t.Errorf("schema with no fixed-count array must not emit %q", gone)
		}
	}
}

// metaDef exercises the metadata-comment surface: an enum with per-const
// descriptions, a bitfield with a defaulted and a non-defaulted flag, a
// deprecated field, and a field carrying a description + unit.
const metaDef = `
version: 1
$defs:
  enum:
    Mode:
      Off:    { value: 0, description: "Node is powered down." }
      Active: { value: 1, description: "Node is sampling and transmitting." }
  bitfield:
    StatusFlags:
      ready:      { pos: 0, default: true, description: "Node has completed initialization." }
      overheated: { pos: 1, description: "Core temperature exceeded the safe threshold." }
messages:
  Telemetry:
    payload:
      temp:     { id: 0, type: i16, description: "Ambient temperature.", unit: degC }
      legacyId: { id: 1, type: u32, description: "Old identifier retained for backward compatibility.", deprecated: true }
      mode:     { id: 2, type: enum, enum: { $ref: "#/$defs/enum/Mode" } }
      status:   { id: 3, type: bitfield, bits: { $ref: "#/$defs/bitfield/StatusFlags" } }
`

// TestTSMetadataComments checks that enum-const descriptions, bitfield-flag
// descriptions + default notes, and the deprecated field marker all render as
// TSDoc comments in the generated module.
func TestTSMetadataComments(t *testing.T) {
	mod := genTSWith(t, metaDef, map[string]any{})
	for _, want := range []string{
		// Enum-const descriptions.
		"  /** Node is powered down. */\n  Off = 0,",
		"  /** Node is sampling and transmitting. */\n  Active = 1,",
		// Bitfield-flag descriptions; the defaulted flag carries a (default: ...) note.
		"  /** Node has completed initialization. (default: true) */\n  Ready = 1,",
		"  /** Core temperature exceeded the safe threshold. */\n  Overheated = 2,",
		// Deprecated field: description kept, @deprecated tag appended (no runtime annotation in TS).
		"  /**\n   * Old identifier retained for backward compatibility.\n   * @deprecated\n   */\n  legacyId: number = 0;",
		// Field description + unit unchanged.
		"  /** Ambient temperature. (unit: degC) */",
	} {
		if !strings.Contains(mod, want) {
			t.Errorf("meta message.ts missing %q", want)
		}
	}
	// The junk citation must not leak into any emitted comment.
	if strings.Contains(mod, "generator#") || strings.Contains(mod, "MESSAGE_SPEC") {
		t.Error("generated module must not contain issue/spec citations")
	}
}

// TestTSInt64Default locks the default (and explicit bigint) mode to the
// bigint-everywhere shapes: no Long import, no accessor pairs.
func TestTSInt64Default(t *testing.T) {
	for _, cfg := range []map[string]any{{}, {"int64": "bigint"}} {
		mod := genTSWith(t, int64Def, cfg)
		for _, want := range []string{
			`import { OStream, Cursor, WireType, SofabError, SofabErrorCode } from "@sofa-buffers/corelib";`,
			// count: 8 with no schema default -> 8 element defaults (issue#136).
			"us: bigint[] = [0n, 0n, 0n, 0n, 0n, 0n, 0n, 0n];",
			// count: 8 -> the trailing default run is trimmed on encode and
			// refilled on decode; the bigint element default is 0n (issue#136).
			"os.writeUnsignedArray(0, _trimTail(this.us, 0n));",
			`case 0: { if (c.wire !== WireType.ArrayUnsigned) { c.skip(c.wire); break; } const _a = c.readUnsignedArray(8) as bigint[]; if (_a.length > 8) throw new SofabError(SofabErrorCode.InvalidMsg, "us: array count above schema capacity 8"); o.us = _padTo(_a, 8, 0n); break; }`,
			"u: bigint = 0n;",
		} {
			if !strings.Contains(mod, want) {
				t.Errorf("default message.ts missing %q", want)
			}
		}
		if strings.Contains(mod, "Long") {
			t.Error("default message.ts should not reference Long")
		}
	}
}

// TestTSDecodeIntoNestedScope pins the MESSAGE_SPEC §7.4 struct/union half
// (generator#175): a field id repeating within one scope RE-OPENS that scope
// rather than starting a new one, so children an earlier opening set whose ids do
// not recur must survive. The decode loop therefore lives in decodeInto(c, o) and
// nested members decode INTO the existing object; assigning decodeFrom(c)'s fresh
// object instead discarded the earlier opening.
func TestTSDecodeIntoNestedScope(t *testing.T) {
	mod := genTSWith(t, `
version: 1
messages:
  M:
    payload:
      s: { id: 0, type: struct, fields: { a: { id: 0, type: u8 }, b: { id: 1, type: u8 } } }
      u: { id: 1, type: union, default_id: 0, oneof: { o1: { id: 0, type: u8 }, o2: { id: 1, type: u8 } } }
      e: { id: 2, type: array, items: { type: struct, count: 2, fields: { x: { id: 0, type: u8 } } } }
`, map[string]any{})
	for _, want := range []string{
		// decodeFrom stays the fresh-object entry point, delegating to decodeInto.
		"static decodeFrom(c: Cursor): M {",
		"return M.decodeInto(c, new M());",
		"static decodeInto(c: Cursor, o: M): M {",
		// Nested struct and union both decode into the existing member.
		"MS.decodeInto(c, o.s); break;",
		"MU.decodeInto(c, o.u); break;",
	} {
		if !strings.Contains(mod, want) {
			t.Errorf("message.ts missing %q\n%s", want, mod)
		}
	}
	// A nested member must never be replaced by a fresh object.
	for _, bad := range []string{
		"o.s = MS.decodeFrom(c)",
		"o.u = MU.decodeFrom(c)",
	} {
		if strings.Contains(mod, bad) {
			t.Errorf("message.ts must not replace a nested member (%q):\n%s", bad, mod)
		}
	}
	// A wrapper-array ELEMENT is not new per arrival either: the element id IS the
	// array index (§5.1), so a REOPENED element id re-opens that element's scope and
	// must merge into it exactly like a re-opened field (§7.4, generator#247). It
	// therefore decodes INTO the element placed at that index, never into a fresh
	// object that would be appended alongside.
	if !strings.Contains(mod, "MEElem.decodeInto(c, arr[_id]!);") {
		t.Errorf("array elements must decode INTO the element at their id:\n%s", mod)
	}
	if strings.Contains(mod, "arr.push(MEElem.decodeFrom(c))") {
		t.Errorf("array elements must not be appended id-blind:\n%s", mod)
	}
}

// A count:N wrapper array's canonical wire stops at M — one past its last
// non-default element (MESSAGE_SPEC §3/§5.1, "even for sequence-form elements")
// — and M === 0 leaves the whole wrapper omitted (§2). generator#248: the element
// loop used to run to .length, framing every trailing all-default element, so a
// decoder that accepted the non-canonical form re-encoded it unchanged instead of
// normalising. A DYNAMIC array has no N to refill from, so its trailing default
// element is significant and must still be framed.
func TestTSFixedWrapperArrayTrimsTrailingDefaultRun(t *testing.T) {
	mod := genTSWith(t, `
version: 1
messages:
  vec:
    payload:
      fixed:   { id: 0, type: array, items: { type: struct, count: 5, fields: { k: { id: 0, type: u32 } } } }
      dynamic: { id: 1, type: array, items: { type: struct, fields: { k: { id: 0, type: u32 } } } }
      fstrs:   { id: 2, type: array, items: { type: string, count: 3, maxlen: 8 } }
      fblobs:  { id: 3, type: array, items: { type: blob, count: 2, maxlen: 4 } }
`, map[string]any{})

	// The fixed array narrows to M before framing anything...
	if !strings.Contains(mod, "_trimObjs(this.fixed).forEach((_e0, _i0) => {") {
		t.Errorf("count:N struct array must loop to M, not to .length:\n%s", mod)
	}
	// ...while the dynamic one keeps every element, trailing defaults included.
	if !strings.Contains(mod, "this.dynamic.forEach((_e0, _i0) => {") {
		t.Errorf("dynamic struct array must not be narrowed:\n%s", mod)
	}
	// An interior all-default element is still framed: only the TRAILING run goes.
	if !strings.Contains(mod, "      os.writeSequenceBeginLazy(_i0);\n      _e0.marshal(os);\n      os.writeSequenceEndKeep();\n") {
		t.Errorf("interior elements must keep the framing closer:\n%s", mod)
	}
	// The trim helpers are emitted only for the element kinds actually present.
	for _, want := range []string{
		"function _trimObjs<T extends { isDefault(): boolean }>(a: readonly T[]): readonly T[] {",
		"function _trimStrs(a: readonly string[]): readonly string[] {",
		"function _trimBlobs(a: readonly Uint8Array[]): readonly Uint8Array[] {",
	} {
		if !strings.Contains(mod, want) {
			t.Errorf("message.ts missing trim helper %q:\n%s", want, mod)
		}
	}
	if strings.Contains(mod, "function _trimRows") {
		t.Errorf("_trimRows must not be emitted for a schema with no nested rows:\n%s", mod)
	}

	// isDefault is the exact negation of what marshal writes, so it must narrow a
	// field exactly when the marshal loop does — disagreeing would either omit a
	// field that is on the wire or keep one that is not.
	for _, want := range []string{
		"if (!(_trimObjs(this.fixed).length === 0)) return false;",
		"if (!(this.dynamic.length === 0)) return false;",
		"if (!(_trimStrs(this.fstrs).length === 0)) return false;",
		"if (!(_trimBlobs(this.fblobs).length === 0)) return false;",
	} {
		if !strings.Contains(mod, want) {
			t.Errorf("isDefault must mirror the marshal loop, missing %q:\n%s", want, mod)
		}
	}
	// The leaf loops walk the very same narrowed run the predicate measures.
	if !strings.Contains(mod, "for (let _i0 = 0, _a0 = _trimStrs(this.fstrs); _i0 < _a0.length; _i0++) {") {
		t.Errorf("string wrapper loop must bind the trimmed run once:\n%s", mod)
	}
	if !strings.Contains(mod, "for (let _i0 = 0, _a0 = _trimBlobs(this.fblobs); _i0 < _a0.length; _i0++) {") {
		t.Errorf("blob wrapper loop must bind the trimmed run once:\n%s", mod)
	}
}

// generator#247: a wrapper array's element id IS the array index (§5.1), so an
// element is PLACED at arr[id] after gap-filling — never appended. Appending
// shortened the array by the size of any interior id gap and decoded a REOPENED
// id as a second element instead of merging into the first (§7.4). The leaf
// string/blob paths next to it always got this right.
//
// The N-fill when the sequence scope closes is what makes the §3/§5.1 trailing
// elision lossless: without it, re-encoding a decoded fixed array shortens it on
// every round trip.
func TestTSWrapperElementsArePlacedByIDAndFilledToN(t *testing.T) {
	mod := genTSWith(t, `
version: 1
messages:
  vec:
    payload:
      objs: { id: 0, type: array, items: { type: struct, count: 4, fields: { k: { id: 0, type: u32 } } } }
      dyn:  { id: 1, type: array, items: { type: struct, fields: { k: { id: 0, type: u32 } } } }
      strs: { id: 2, type: array, items: { type: string, count: 3, maxlen: 8 } }
`, map[string]any{})

	for _, want := range []string{
		// placement, not append — and the gap-fill that precedes it
		"const _id = c.id; while (arr.length <= _id) arr.push(new VecObjsElem()); VecObjsElem.decodeInto(c, arr[_id]!);",
		// N-fill when the sequence scope closes, per element kind
		"while (arr.length < 4) arr.push(new VecObjsElem());",
		"while (arr.length < 3) arr.push(\"\");",
	} {
		if !strings.Contains(mod, want) {
			t.Errorf("message.ts missing %q:\n%s", want, mod)
		}
	}
	// The defect this replaced: appending ignored the id entirely.
	if strings.Contains(mod, "arr.push(VecObjsElem.decodeFrom(c))") {
		t.Errorf("struct elements must not be appended id-blind:\n%s", mod)
	}
	// A dynamic array (no schema count) has no N to refill from: its length is
	// highest-present-id + 1, so it is placed by id but never filled.
	if !strings.Contains(mod, "const _id = c.id; while (arr.length <= _id) arr.push(new VecDynElem()); VecDynElem.decodeInto(c, arr[_id]!);") {
		t.Errorf("dynamic struct elements must still be placed by id:\n%s", mod)
	}
	if strings.Contains(mod, "arr.push(new VecDynElem());\n") {
		t.Errorf("a dynamic array must not be filled to any N:\n%s", mod)
	}
	// The cap bound still rejects an out-of-range element id, which also bounds
	// the gap-fill above.
	if !strings.Contains(mod, `if (c.id >= 4) throw new SofabError(SofabErrorCode.InvalidMsg, "arr: array index above schema capacity 4");`) {
		t.Errorf("the over-index guard must survive:\n%s", mod)
	}
}

// A count:N array's value is N elements long whether or not the field ever
// reaches the wire (MESSAGE_SPEC §5.1: the length "is N for every target"). The
// seqFillTo refill above can only fill a sequence that was actually OPENED, so
// with the field materialized empty the same schema decoded three different
// lengths:
//
//	absent field            -> 0   (wrong)
//	one element on the wire  -> N   (right)
//	explicitly-empty wrapper -> N   (right)
//
// A native count:N array never had that split — nativeArrayDefault has always
// materialized it. This pins the wrapper kinds getting the same treatment, in the
// same place, and the three things that must NOT change with it: a dynamic
// wrapper array stays empty, each composite slot is its own instance, and the N
// defaults never reach the wire (the trims narrow them away, so marshal writes no
// child and isDefault stays true).
//
// Verified against a real build (corelib-ts): fresh / empty input / one string
// element / explicit empty wrapper all decode `strs` at length 3, matching the
// count:3 u32 array beside it, and the fresh message still encodes to zero bytes.
func TestTSFixedCountWrapperArrayMaterializedToN(t *testing.T) {
	mod := genTSWith(t, `
version: 1
messages:
  vec:
    payload:
      strs:   { id: 0, type: array, items: { type: string, count: 3, maxlen: 8 } }
      nums:   { id: 1, type: array, items: { type: u32, count: 3 } }
      blobs:  { id: 2, type: array, items: { type: blob, count: 2, maxlen: 4 } }
      objs:   { id: 3, type: array, items: { type: struct, count: 2, fields: { k: { id: 0, type: u32 } } } }
      dstrs:  { id: 4, type: array, items: { type: string, maxlen: 8 } }
      dobjs:  { id: 5, type: array, items: { type: struct, fields: { k: { id: 0, type: u32 } } } }
`, map[string]any{})

	for _, want := range []string{
		// The wrapper kinds now materialize to N, exactly like the native one below
		// them. Element defaults are the values seqFillTo grows the array with.
		`strs: string[] = ["", "", ""];`,
		"blobs: Uint8Array[] = [new Uint8Array(), new Uint8Array()];",
		"objs: VecObjsElem[] = [new VecObjsElem(), new VecObjsElem()];",
		// The native count:N array is unchanged — it is the behaviour being matched.
		"nums: number[] = [0, 0, 0];",
		// A dynamic wrapper array has no N and must stay empty: its length is
		// highest-present-id + 1, so pre-filling would invent elements.
		"dstrs: string[] = [];",
		"dobjs: VecDobjsElem[] = [];",
		// The N defaults are trimmed away before framing, so an untouched message
		// still encodes to nothing (MESSAGE_SPEC §2) and isDefault agrees.
		"if (!(_trimStrs(this.strs).length === 0)) return false;",
		"if (!(_trimObjs(this.objs).length === 0)) return false;",
	} {
		if !strings.Contains(mod, want) {
			t.Errorf("message.ts missing %q:\n%s", want, mod)
		}
	}
	// Each composite slot must be a FRESH instance: `[x, x]` from one shared value
	// would let a decode into slot 0 show up in slot 1.
	if strings.Contains(mod, "objs: VecObjsElem[] = new Array") || strings.Contains(mod, ".fill(new VecObjsElem())") {
		t.Errorf("composite slots must not share one instance:\n%s", mod)
	}
	// The decode-side refill is what the construction default now agrees with; it
	// must survive, since a partially-transmitted array still arrives short.
	if !strings.Contains(mod, `while (arr.length < 3) arr.push("");`) {
		t.Errorf("the sequence-close N-fill must survive:\n%s", mod)
	}
}

// isDefault must be emitted for every generated class and must be the exact
// negation of marshal's per-field write guards (MESSAGE_SPEC §2) — including a
// nested object, which is default iff ITS marshal writes no child.
func TestTSIsDefaultMirrorsMarshalGuards(t *testing.T) {
	mod := genTSWith(t, `
version: 1
messages:
  m:
    payload:
      n:    { id: 0, type: u8, default: 7 }
      s:    { id: 1, type: string, default: "hi" }
      b:    { id: 2, type: blob, maxlen: 4 }
      sub:  { id: 3, type: struct, fields: { x: { id: 0, type: i32 } } }
      nat:  { id: 4, type: array, items: { type: i32, count: 3 }, default: [1, 2, 3] }
      dynn: { id: 5, type: array, items: { type: u8 } }
`, map[string]any{})
	for _, want := range []string{
		"if (!(this.n === 7)) return false;",
		`if (!(this.s === "hi")) return false;`,
		"if (!(this.b.length === 0)) return false;",
		"if (!(this.sub.isDefault())) return false;",
		"if (!(arrEq(this.nat, [1, 2, 3]))) return false;",
		"if (!(this.dynn.length === 0)) return false;",
	} {
		if !strings.Contains(mod, want) {
			t.Errorf("isDefault missing %q:\n%s", want, mod)
		}
	}
	// Every class carries the predicate, nested types included.
	if strings.Count(mod, "isDefault(): boolean {") != 2 {
		t.Errorf("every generated class must carry isDefault:\n%s", mod)
	}
	// A field-less class is trivially default.
	empty := genTSWith(t, "version: 1\nmessages:\n  e:\n    payload: {}\n", map[string]any{})
	if !strings.Contains(empty, "isDefault(): boolean {\n    return true;\n  }") {
		t.Errorf("a field-less class must be trivially default:\n%s", empty)
	}
}

// The last element of a DYNAMIC wrapper array is always written, whatever its
// value (MESSAGE_SPEC §2). Such an array recovers its length as highest-present-
// id + 1 (§5.1), so the element at the highest index is the only one whose
// PRESENCE carries the length: dropping a trailing default leaf encoded ["a", ""]
// exactly like ["a"] and decoded one element short. Sequence-form elements never
// had the problem — they are framed unconditionally — so this holds both element
// kinds to one standard. A fixed-count array is exempt: its length is N whatever
// the wire carries, so it still elides the whole trailing run.
func TestTSDynamicArrayAlwaysWritesLastElement(t *testing.T) {
	mod := genTSWith(t, `
version: 1
messages:
  vec:
    payload:
      dynstr:   { id: 0, type: array, items: { type: string, maxlen: 8 } }
      dynblob:  { id: 1, type: array, items: { type: blob, maxlen: 8 } }
      fixedstr: { id: 2, type: array, items: { type: string, count: 3, maxlen: 8 } }
`, map[string]any{})

	for _, want := range []string{
		// dynamic: the run is walked untrimmed and the last index escapes the omit test
		"for (let _i0 = 0, _a0 = this.dynstr; _i0 < _a0.length; _i0++) {\n      if (_a0[_i0]! !== \"\" || _i0 === _a0.length - 1) {",
		"for (let _i0 = 0, _a0 = this.dynblob; _i0 < _a0.length; _i0++) {\n      if (_a0[_i0]!.length !== 0 || _i0 === _a0.length - 1) {",
		// fixed: no guard — the trailing run still collapses, the decoder refills to N
		"for (let _i0 = 0, _a0 = _trimStrs(this.fixedstr); _i0 < _a0.length; _i0++) {\n      if (_a0[_i0]! !== \"\") {",
	} {
		if !strings.Contains(mod, want) {
			t.Errorf("message.ts missing %q:\n%s", want, mod)
		}
	}
	// The all-default predicate has to follow the writer: a dynamic [""] now puts
	// an element on the wire, so the field is NOT default and must not be omitted.
	// Trimming it here would drop a field the marshal loop writes.
	for _, want := range []string{
		"if (!(this.dynstr.length === 0)) return false;",
		"if (!(this.dynblob.length === 0)) return false;",
		// The fixed one keeps its trim on both sides.
		"if (!(_trimStrs(this.fixedstr).length === 0)) return false;",
	} {
		if !strings.Contains(mod, want) {
			t.Errorf("isDefault must mirror the marshal loop, missing %q:\n%s", want, mod)
		}
	}
	if strings.Contains(mod, "_trimStrs(this.dynstr)") || strings.Contains(mod, "_trimBlobs(this.dynblob)") {
		t.Errorf("a dynamic string/blob array must not be trimmed:\n%s", mod)
	}
	// Helper emission mirrors elemTrimExpr: _trimStrs is still referenced by the
	// fixed field, _trimBlobs by nothing at all — emitting it would be dead code.
	if !strings.Contains(mod, "function _trimStrs(") {
		t.Errorf("_trimStrs must still be emitted for the count:N string array:\n%s", mod)
	}
	if strings.Contains(mod, "function _trimBlobs(") {
		t.Errorf("_trimBlobs must not be emitted when only a dynamic blob array exists:\n%s", mod)
	}
}
