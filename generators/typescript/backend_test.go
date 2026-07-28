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
		// while each ELEMENT chooses its closer POSITIONALLY (§2) — the keeping one
		// at the last index, whose presence carries the array's length (§5.1), the
		// dropping one in the interior, where an all-default element leaves an id gap.
		"    os.writeSequenceBeginLazy(23);\n    this.somestructarray.forEach((_e0, _i0, _a0) => {\n      os.writeSequenceBeginLazy(_i0);\n      _e0.marshal(os);\n      if (_i0 === _a0.length - 1) {\n        os.writeSequenceEndKeep();\n      } else {\n        os.writeSequenceEnd();\n      }\n    });\n    os.writeSequenceEnd();\n",
		"    os.writeSequenceBeginLazy(25);\n    this.someunionarray.forEach((_e0, _i0, _a0) => {\n      os.writeSequenceBeginLazy(_i0);\n      _e0.marshal(os);\n      if (_i0 === _a0.length - 1) {\n        os.writeSequenceEndKeep();\n      } else {\n        os.writeSequenceEnd();\n      }\n    });\n    os.writeSequenceEnd();\n",
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
	if !strings.Contains(mod, "for (let _i0 = 0, _a0 = this.somestringarray; _i0 < _a0.length; _i0++) {") {
		t.Error("message.ts missing indexed-for string-list marshal (fast-encode)")
	}
}

// TestTSLazySequenceFraming pins the MESSAGE_SPEC §2 closer table. Every sequence
// opens with writeSequenceBeginLazy; the CLOSER decides whether a contentless one
// survives:
//
//	struct/union FIELD         -> writeSequenceEnd()      may vanish when all-default
//	array FIELD (the wrapper)  -> writeSequenceEnd()      may vanish when empty
//	wrapper-array ELEMENT      -> POSITIONAL: Keep at the last index, drop before it
//
// The element row is the one the schema cannot answer: it is decided from the
// position in the VALUE at run time, because only the last element's presence
// carries the array's length (§5.1).
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
		// The leaf runs are walked whole — nothing is narrowed away — and the last
		// element escapes the omit test (§2, see TestTSArrayElementSparsityIsPositional).
		"    os.writeSequenceBeginLazy(2);\n    for (let _i0 = 0, _a0 = this.strs; _i0 < _a0.length; _i0++) {",
		"    os.writeSequenceBeginLazy(4);\n    for (let _i0 = 0, _a0 = this.blobs; _i0 < _a0.length; _i0++) {",
		"    os.writeSequenceBeginLazy(1);\n    this.ss.forEach((_e0, _i0, _a0) => {\n      os.writeSequenceBeginLazy(_i0);\n      _e0.marshal(os);\n      if (_i0 === _a0.length - 1) {\n        os.writeSequenceEndKeep();\n      } else {\n        os.writeSequenceEnd();\n      }\n    });\n    os.writeSequenceEnd();\n",
		// The nested row is an ELEMENT of `rows`, so its own wrapper takes the
		// positional closer too; the outer `rows` wrapper is a FIELD and may vanish.
		"    os.writeSequenceBeginLazy(3);\n    this.rows.forEach((_e0, _i0, _a0) => {\n      os.writeSequenceBeginLazy(_i0);\n      for (let _i1 = 0, _a1 = _e0; _i1 < _a1.length; _i1++) {\n        if (_a1[_i1]! !== \"\" || _i1 === _a1.length - 1) {\n          os.writeString(_i1, _a1[_i1]!);\n        }\n      }\n      if (_i0 === _a0.length - 1) {\n        os.writeSequenceEndKeep();\n      } else {\n        os.writeSequenceEnd();\n      }\n    });\n    os.writeSequenceEnd();\n",
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
	// Exactly one keeping close per sequence-form ELEMENT site (the struct element
	// and the nested row), each guarded by its last-index test, and no keeping close
	// on any FIELD wrapper — a FIELD closes unconditionally with the dropping end.
	if got, want := strings.Count(mod, "os.writeSequenceEndKeep();"), 2; got != want {
		t.Errorf("writeSequenceEndKeep() count = %d, want %d (one per wrapper-array element site)", got, want)
	}
	if got, want := strings.Count(mod, "if (_i0 === _a0.length - 1) {"), 2; got != want {
		t.Errorf("positional closer guard count = %d, want %d (one per sequence-form element site)", got, want)
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
		// Long[] backing field + accessor pair; setter converts once. `count: 8` is a
		// CAPACITY, not a length (§3), so a fresh us is the EMPTY array — not 8
		// Long zeros.
		"private _us: Long[] = [];",
		"get us(): Long[] { return this._us; }",
		"set us(vals: readonly (Long | bigint | number)[]) { this._us = vals.map(Long.fromValue); }",
		// Nested array: Long[][] with a per-row setter conversion. `count: 2` adds no
		// rows either.
		"private _rows: Long[][] = [];",
		"set rows(vals: readonly (readonly (Long | bigint | number)[])[]) { this._rows = vals.map((_v0) => _v0.map(Long.fromValue)); }",
		// Marshal reads the backing field; 64-bit arrays use the Long writers, and
		// the value goes out whole — no trailing run is elided, because the wire
		// count IS the length (§3). With no declared default the omit test is the
		// emptiness of the value.
		"if (this._us.length !== 0) {",
		"os.writeUnsignedArrayLong(0, this._us);",
		"os.writeSignedArrayLong(1, this._is);",
		// Defaulted Long array: materialized Long default + longArrEq guard.
		`private _ud: Long[] = [Long.fromValue(1n), Long.fromValue(18446744073709551615n)];`,
		"if (!longArrEq(this._ud, [Long.fromValue(1n), Long.fromValue(18446744073709551615n)])) {",
		"function longArrEq(a: readonly Long[], b: readonly Long[]): boolean {",
		// Decode bypasses the setter (readers return canonical Long[]); a wire count
		// above the schema capacity rejects as INVALID (generator#100), and a wire
		// count below it is simply the array's length — nothing is filled in.
		`case 0: { if (c.wire !== WireType.ArrayUnsigned) { c.skip(c.wire); break; } const _a = c.readUnsignedArrayLong(8); if (_a.length > 8) throw new SofabError(SofabErrorCode.InvalidMsg, "us: array count above schema capacity 8"); o._us = _a; break; }`,
		`case 1: { if (c.wire !== WireType.ArraySigned) { c.skip(c.wire); break; } const _a = c.readSignedArrayLong(8); if (_a.length > 8) throw new SofabError(SofabErrorCode.InvalidMsg, "is: array count above schema capacity 8"); o._is = _a; break; }`,
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
	for _, gone := range []string{
		"bigint[]", "writeUnsignedArray(0", "readUnsignedArray()",
		// The trim/pad pair belonged to the superseded fixed-length reading of
		// `count` and is gone with it (MESSAGE_SPEC af536c4).
		"_trimTailLong", "_trimTail", "_padTo",
	} {
		if strings.Contains(mod, gone) {
			t.Errorf("int64: long message.ts should not emit %q", gone)
		}
	}
}

func TestTSInt64Number(t *testing.T) {
	mod := genTSWith(t, int64Def, map[string]any{"int64": "number"})
	for _, want := range []string{
		// Arrays are Long-backed exactly as in long mode.
		"os.writeUnsignedArrayLong(0, this._us);",
		`case 0: { if (c.wire !== WireType.ArrayUnsigned) { c.skip(c.wire); break; } const _a = c.readUnsignedArrayLong(8); if (_a.length > 8) throw new SofabError(SofabErrorCode.InvalidMsg, "us: array count above schema capacity 8"); o._us = _a; break; }`,
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
// every native element kind, plus a nested array-of-array and a non-native
// (string) element array.
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

// TestTSCompactArrayKeepsItsTail: a `count: N` native array carries EVERY element
// it holds, trailing defaults included, because the wire count M IS the array's
// length (MESSAGE_SPEC §3, af536c4) — [1,2,0,0] and [1,2] are different values.
// A count:N array is written and read exactly like the dynamic one beside it: the
// count only bounds M, it never elides a tail on encode and never fills one back
// in on decode. The trim/pad pair that implemented the superseded fixed-length
// reading is gone from the module entirely.
func TestTSCompactArrayKeepsItsTail(t *testing.T) {
	mod := genTSWith(t, fixedCountDef, map[string]any{})
	for _, want := range []string{
		// Encode: the value goes out untouched, one form per element kind, and each
		// is byte-identical in shape to its dynamic sibling below.
		"os.writeUnsignedArray(0, this.fu32);",
		"os.writeSignedArray(2, this.fi16);",
		"os.writeFp32Array(3, this.ffp32);",
		"os.writeFp64Array(4, this.ffp64);",
		"os.writeUnsignedArray(6, this.fbool.map((_e0) => (_e0 ? 1 : 0)));",
		"os.writeSignedArray(8, this.fenum);",
		"os.writeUnsignedArray(9, this.fbits);",
		// Decode: the M elements that arrived ARE the value, taken as they come.
		// The over-count reject stays — `count` still bounds M (generator#100).
		`case 0: { if (c.wire !== WireType.ArrayUnsigned) { c.skip(c.wire); break; } const _a = c.readUnsignedArray(5) as number[]; if (_a.length > 5) throw new SofabError(SofabErrorCode.InvalidMsg, "fu32: array count above schema capacity 5"); o.fu32 = _a; break; }`,
		`o.ffp64 = _a; break; }`,
		`o.fbool = _a; break; }`,
		`o.fenum = _a; break; }`,
	} {
		if !strings.Contains(mod, want) {
			t.Errorf("fixed-count message.ts missing %q", want)
		}
	}
	for _, gone := range []string{
		// The whole trim-on-encode / fill-on-decode pair is gone: it was correct only
		// under the superseded reading of `count` as a length.
		"_trimTail", "_trimTailLong", "_padTo",
		"_trimStrs", "_trimBlobs", "_trimObjs", "_trimRows",
	} {
		if strings.Contains(mod, gone) {
			t.Errorf("fixed-count message.ts should not emit %q", gone)
		}
	}
	// Dynamic arrays keep their plain writer call unchanged — the point being that
	// the counted ones above now read identically.
	for _, want := range []string{
		"os.writeUnsignedArray(1, this.du32);",
		"os.writeFp64Array(5, this.dfp64);",
		"os.writeUnsignedArray(7, this.dbool.map((_e0) => (_e0 ? 1 : 0)));",
		// A nested native row is written under the positional guard: an interior
		// empty row is not written at all, the last one always is.
		"      if (_e0.length !== 0 || _i0 === _a0.length - 1) {\n        os.writeUnsignedArray(_i0, _e0);\n      }\n",
	} {
		if !strings.Contains(mod, want) {
			t.Errorf("fixed-count message.ts missing unchanged dynamic form %q", want)
		}
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

// TestTSCountIsACapacityNotADefaultLength: a `count: N` array's declared default
// is whatever the schema wrote and nothing more (MESSAGE_SPEC §3, af536c4). N is a
// CAPACITY: it never reaches the wire, so it can neither manufacture a default nor
// pad a short one out to N — a fresh count:N array is the EMPTY array, exactly like
// the dynamic one beside it, and both native and wrapper element kinds agree on
// that. The omit guard compares against that same unpadded default, which is what
// keeps an all-zero length-N value (a length-N array) distinct from the empty one.
func TestTSCountIsACapacityNotADefaultLength(t *testing.T) {
	mod := genTSWith(t, fixedDefaultDef, map[string]any{})
	for _, want := range []string{
		// No schema default -> the empty array, whatever the count.
		"none: number[] = [];",
		"ff: number[] = [];",
		"fe: EnumMode[] = [];",
		// A schema default stands exactly as written — not padded out to N.
		"short: number[] = [1, 2];",
		"fb: boolean[] = [true];",
		"fu64: bigint[] = [1n];",
		"exact: number[] = [1, 2, 3];",
		// The omit guard reads that same unpadded default; a count:N array with no
		// default is omitted only when it is EMPTY.
		"if (this.none.length !== 0) {",
		"if (!arrEq(this.short, [1, 2])) {",
		// A count:N WRAPPER array constructs empty for the same reason.
		`strs: string[] = [];`,
	} {
		if !strings.Contains(mod, want) {
			t.Errorf("fixed-default message.ts missing %q", want)
		}
	}
	for _, gone := range []string{
		// No count-derived materialization and no padding, of either element kind.
		"none: number[] = [0",
		"short: number[] = [1, 2, 0",
		"fb: boolean[] = [true, false",
		"fu64: bigint[] = [1n, 0n",
		"fe: EnumMode[] = [(0 as EnumMode)",
		`strs: string[] = [""`,
		"if (!arrEq(this.none,",
	} {
		if strings.Contains(mod, gone) {
			t.Errorf("fixed-default message.ts should not emit %q", gone)
		}
	}
	// The dynamic controls are unchanged — which is the point: the two kinds now
	// read identically.
	for _, want := range []string{
		"dyn: number[] = [];",      // no default -> empty
		"dynd: number[] = [1, 2];", // declared default kept verbatim
		"dstrs: string[] = [];",
	} {
		if !strings.Contains(mod, want) {
			t.Errorf("fixed-default message.ts missing unchanged dynamic form %q", want)
		}
	}
}

// TestTSCountIsACapacityNotADefaultLengthLong: the Long-backed 64-bit modes render
// the same unpadded default as Long values (and compare it with longArrEq).
func TestTSCountIsACapacityNotADefaultLengthLong(t *testing.T) {
	for _, mode := range []string{"long", "number"} {
		mod := genTSWith(t, fixedDefaultDef, map[string]any{"int64": mode})
		for _, want := range []string{
			"private _fu64: Long[] = [Long.fromValue(1n)];",
			"if (!longArrEq(this._fu64, [Long.fromValue(1n)])) {",
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

// TestTSNoTrimOrPadHelpers: the trim-on-encode / fill-on-decode helper family
// implemented the superseded fixed-length reading of `count` and is gone from
// every module (MESSAGE_SPEC af536c4), counted arrays included. The corelib still
// ships its equivalents; the generator simply never calls them.
func TestTSNoTrimOrPadHelpers(t *testing.T) {
	const src = `
version: 1
messages:
  dyn:
    payload:
      a: { id: 0, type: array, items: { type: u32 } }
      s: { id: 1, type: string }
      f: { id: 2, type: array, items: { type: u32, count: 4 } }
      w: { id: 3, type: array, items: { type: string, count: 4, maxlen: 8 } }
      o: { id: 4, type: array, items: { type: struct, count: 2, fields: { k: { id: 0, type: u32 } } } }
      r: { id: 5, type: array, items: { type: array, count: 2, items: { type: u32, count: 3 } } }
`
	for _, mode := range []string{"bigint", "long", "number"} {
		mod := genTSWith(t, src, map[string]any{"int64": mode})
		for _, gone := range []string{
			"_trimTail", "_trimTailLong", "_padTo",
			"_trimStrs", "_trimBlobs", "_trimObjs", "_trimRows",
		} {
			if strings.Contains(mod, gone) {
				t.Errorf("int64: %s module must not emit %q", mode, gone)
			}
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
			// count: 8 is a CAPACITY, so a fresh array is empty (§3, af536c4).
			"us: bigint[] = [];",
			// ...and the value goes out whole, the wire count being its length.
			"os.writeUnsignedArray(0, this.us);",
			`case 0: { if (c.wire !== WireType.ArrayUnsigned) { c.skip(c.wire); break; } const _a = c.readUnsignedArray(8) as bigint[]; if (_a.length > 8) throw new SofabError(SofabErrorCode.InvalidMsg, "us: array count above schema capacity 8"); o.us = _a; break; }`,
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

// TestTSArrayElementSparsityIsPositional is the codegen statement of MESSAGE_SPEC
// §2's ONE element rule (af536c4), the same for both element kinds and the same
// with or without a declared `count`: an element before the last one that equals
// its element default is omitted, leaving an id GAP — a string/blob leaf simply not
// written, a struct/union/nested-array element not framed either — while the LAST
// element is always written, as its value or as an empty frame.
//
// The choice is made from the position in the VALUE, at run time. That is what the
// superseded reading could not express: it picked the closer statically from the
// schema (sequence elements were framed unconditionally, the trailing all-default
// run was narrowed off before the loop), which is why `count: N` had to be a
// length. It is a capacity, so the run-time test is the only thing left.
//
// Byte targets, verified against corelib-ts through a built project — every hex is
// a regenerated shared test vector (serialized_sparse):
//
//	array_string_trailing_default          ["a",""]      06020a610a0207
//	array_string_all_default               ["",""]       060a0207
//	array_string_leading_default           ["","x",""]   060a0a78120207
//	array_string_gap                       ["a","","c"]  06020a61120a6307
//	array_struct_interior_default_element  [{1},{},{3}]  06060001071600030707
//	array_struct_all_default_elements      [{},{}]       060e0707
func TestTSArrayElementSparsityIsPositional(t *testing.T) {
	mod := genTSWith(t, `
version: 1
messages:
  vec:
    payload:
      fixed:   { id: 0, type: array, items: { type: struct, count: 5, fields: { k: { id: 0, type: u32 } } } }
      dynamic: { id: 1, type: array, items: { type: struct, fields: { k: { id: 0, type: u32 } } } }
      fstrs:   { id: 2, type: array, items: { type: string, count: 3, maxlen: 8 } }
      fblobs:  { id: 3, type: array, items: { type: blob, count: 2, maxlen: 4 } }
      rows:    { id: 4, type: array, items: { type: array, count: 2, items: { type: u32, count: 3 } } }
`, map[string]any{})

	for _, want := range []string{
		// A sequence-form ELEMENT takes the positional closer: keep the frame at the
		// last index (its presence fixes the length), drop it in the interior (an
		// all-default element vanishes into an id gap). Counted and dynamic alike —
		// `count: 5` buys the fixed array no exemption.
		"    os.writeSequenceBeginLazy(0);\n    this.fixed.forEach((_e0, _i0, _a0) => {\n      os.writeSequenceBeginLazy(_i0);\n      _e0.marshal(os);\n      if (_i0 === _a0.length - 1) {\n        os.writeSequenceEndKeep();\n      } else {\n        os.writeSequenceEnd();\n      }\n    });\n    os.writeSequenceEnd();\n",
		"    os.writeSequenceBeginLazy(1);\n    this.dynamic.forEach((_e0, _i0, _a0) => {\n      os.writeSequenceBeginLazy(_i0);\n      _e0.marshal(os);\n      if (_i0 === _a0.length - 1) {\n        os.writeSequenceEndKeep();\n      } else {\n        os.writeSequenceEnd();\n      }\n    });\n    os.writeSequenceEnd();\n",
		// A leaf element gets the same rule as an unconditional `|| last` disjunct,
		// walking the value whole — nothing is narrowed away first.
		"    for (let _i0 = 0, _a0 = this.fstrs; _i0 < _a0.length; _i0++) {\n      if (_a0[_i0]! !== \"\" || _i0 === _a0.length - 1) {\n        os.writeString(_i0, _a0[_i0]!);\n      }\n    }\n",
		"    for (let _i0 = 0, _a0 = this.fblobs; _i0 < _a0.length; _i0++) {\n      if (_a0[_i0]!.length !== 0 || _i0 === _a0.length - 1) {\n        os.writeBlob(_i0, _a0[_i0]!);\n      }\n    }\n",
		// A NATIVE nested row has no frame of its own, so the rule lands on the write.
		"    this.rows.forEach((_e0, _i0, _a0) => {\n      if (_e0.length !== 0 || _i0 === _a0.length - 1) {\n        os.writeUnsignedArray(_i0, _e0);\n      }\n    });\n",
	} {
		if !strings.Contains(mod, want) {
			t.Errorf("message.ts missing positional element rule %q:\n%s", want, mod)
		}
	}
	// A sequence-typed FIELD is NOT positional: it always takes the dropping closer,
	// so an empty array is omitted and absence reconstructs it (§2).
	if got, want := strings.Count(mod, "\n    os.writeSequenceEnd();\n"), 5; got != want {
		t.Errorf("field-level dropping closers = %d, want %d (one per array field)", got, want)
	}
	// Nothing narrows the run before the loop any more: the wire count IS the length
	// (§3) and the highest wrapper id IS the last index (§5.1).
	for _, gone := range []string{"_trimObjs", "_trimStrs", "_trimBlobs", "_trimRows", "_trimTail", "_padTo"} {
		if strings.Contains(mod, gone) {
			t.Errorf("message.ts must not narrow an array run with %q:\n%s", gone, mod)
		}
	}
	// isDefault is the exact negation of what marshal writes. The writer emits a
	// child for EVERY element the value holds (the last one whatever its value), so
	// "no child is written" is exactly "the array is empty" — for every kind, with
	// or without a count. Anything narrower would omit a field that is on the wire.
	for _, want := range []string{
		"if (!(this.fixed.length === 0)) return false;",
		"if (!(this.dynamic.length === 0)) return false;",
		"if (!(this.fstrs.length === 0)) return false;",
		"if (!(this.fblobs.length === 0)) return false;",
		"if (!(this.rows.length === 0)) return false;",
	} {
		if !strings.Contains(mod, want) {
			t.Errorf("isDefault must mirror the marshal loop, missing %q:\n%s", want, mod)
		}
	}
}

// generator#247: a wrapper array's element id IS the array index (§5.1), so an
// element is PLACED at arr[id] after gap-filling — never appended. Appending
// shortens the array by the size of any interior id gap and decodes a REOPENED id
// as a second element instead of merging into the first (§7.4).
//
// Under af536c4 an interior gap is no longer exotic: an interior element equal to
// its element default is omitted by every conformant encoder, so id-blind
// collection now silently shifts every later element down by one. What the count
// does NOT do is add elements — a decoded wrapper array is exactly as long as the
// highest present id + 1, never filled out to N.
func TestTSWrapperElementsArePlacedByID(t *testing.T) {
	mod := genTSWith(t, `
version: 1
messages:
  vec:
    payload:
      objs: { id: 0, type: array, items: { type: struct, count: 4, fields: { k: { id: 0, type: u32 } } } }
      dyn:  { id: 1, type: array, items: { type: struct, fields: { k: { id: 0, type: u32 } } } }
      strs: { id: 2, type: array, items: { type: string, count: 3, maxlen: 8 } }
      rows: { id: 3, type: array, items: { type: array, count: 3, items: { type: u32, count: 3 } } }
      wrows: { id: 4, type: array, items: { type: array, count: 3, items: { type: string, maxlen: 8 } } }
`, map[string]any{})

	for _, want := range []string{
		// placement, not append — and the gap-fill that precedes it
		"const _id = c.id; while (arr.length <= _id) arr.push(new VecObjsElem()); VecObjsElem.decodeInto(c, arr[_id]!);",
		"const _id = c.id; while (arr.length <= _id) arr.push(new VecDynElem()); VecDynElem.decodeInto(c, arr[_id]!);",
		`const _id = c.id; while (arr.length <= _id) arr.push(""); `,
		// A nested ROW is placed by id too. This was the id-blind append: it was
		// unreachable while every row was written, and an interior gap makes it
		// reachable, shifting every later row down one index.
		"const _id = c.id; while (arr.length <= _id) arr.push([]); arr[_id] = c.readUnsignedArray(3) as number[];",
		// ...including a wrapper row, whose own collector — typed with the ROW's type,
		// see TestTSNestedWrapperRowCollectorTypes — runs inside the placement.
		"const _id = c.id; while (arr.length <= _id) arr.push([]); arr[_id] = ((): string[] =>",
	} {
		if !strings.Contains(mod, want) {
			t.Errorf("message.ts missing %q:\n%s", want, mod)
		}
	}
	// The defects this replaced: appending ignored the id entirely.
	for _, bad := range []string{
		"arr.push(VecObjsElem.decodeFrom(c))",
		"arr.push(c.readUnsignedArray(3) as number[])",
		"arr.push(((): string[] =>",
	} {
		if strings.Contains(mod, bad) {
			t.Errorf("elements must not be appended id-blind (%q):\n%s", bad, mod)
		}
	}
	// `count: N` never ADDS an element: the decoded length is highest present id + 1,
	// exactly as for a count-less array, because the last element is always on the
	// wire. The fill-to-N that the superseded fixed-length reading needed is gone.
	for _, gone := range []string{
		"while (arr.length < 4) arr.push(new VecObjsElem());",
		`while (arr.length < 3) arr.push("");`,
		"while (arr.length < 3) arr.push([]);",
		"while (_r.length < ",
	} {
		if strings.Contains(mod, gone) {
			t.Errorf("a decoded wrapper array must not be filled to N (%q):\n%s", gone, mod)
		}
	}
	// The cap bound still rejects an out-of-range element id, which also bounds the
	// gap-fill above — a capacity is still a bound, just not a length.
	if !strings.Contains(mod, `if (c.id >= 4) throw new SofabError(SofabErrorCode.InvalidMsg, "arr: array index above schema capacity 4");`) {
		t.Errorf("the over-index guard must survive:\n%s", mod)
	}
}

// A `count: N` array constructs EMPTY, of every element kind — native and wrapper
// alike (MESSAGE_SPEC §3, af536c4). N is a capacity, so it never manufactures
// elements the value does not have; the field's declared default is the empty
// collection, an absent field decodes back to it, and a fresh message still
// encodes to zero bytes.
//
// The superseded reading materialized N element defaults here, which made an
// all-zero length-N array indistinguishable from the empty one and dropped both.
func TestTSCountNArrayConstructsEmpty(t *testing.T) {
	mod := genTSWith(t, `
version: 1
messages:
  vec:
    payload:
      strs:   { id: 0, type: array, items: { type: string, count: 3, maxlen: 8 } }
      nums:   { id: 1, type: array, items: { type: u32, count: 3 } }
      blobs:  { id: 2, type: array, items: { type: blob, count: 2, maxlen: 4 } }
      objs:   { id: 3, type: array, items: { type: struct, count: 2, fields: { k: { id: 0, type: u32 } } } }
      rows:   { id: 4, type: array, items: { type: array, count: 2, items: { type: u32, count: 3 } } }
      dstrs:  { id: 5, type: array, items: { type: string, maxlen: 8 } }
      dobjs:  { id: 6, type: array, items: { type: struct, fields: { k: { id: 0, type: u32 } } } }
`, map[string]any{})

	for _, want := range []string{
		"strs: string[] = [];",
		"nums: number[] = [];",
		"blobs: Uint8Array[] = [];",
		"objs: VecObjsElem[] = [];",
		"rows: number[][] = [];",
		// The count-less controls, unchanged — the point being that both kinds now
		// read identically.
		"dstrs: string[] = [];",
		"dobjs: VecDobjsElem[] = [];",
		// A fresh message still encodes to nothing: every array field is default.
		"if (!(this.nums.length === 0)) return false;",
		"if (!(this.strs.length === 0)) return false;",
		"if (!(this.objs.length === 0)) return false;",
	} {
		if !strings.Contains(mod, want) {
			t.Errorf("message.ts missing %q:\n%s", want, mod)
		}
	}
	for _, gone := range []string{
		`strs: string[] = ["`,
		"nums: number[] = [0",
		"blobs: Uint8Array[] = [new Uint8Array()",
		"objs: VecObjsElem[] = [new VecObjsElem()",
		"rows: number[][] = [[]",
	} {
		if strings.Contains(mod, gone) {
			t.Errorf("a count:N array must not materialize N elements (%q):\n%s", gone, mod)
		}
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

// The LAST element of a wrapper array is always written, whatever its value, and a
// declared `count` changes nothing about that (MESSAGE_SPEC §2, af536c4). Such an
// array recovers its length as highest-present-id + 1 (§5.1), so the element at
// the highest index is the only one whose PRESENCE carries the length: dropping a
// trailing default leaf would encode ["a", ""] exactly like ["a"] and decode one
// element short.
//
// The count:N row is the one af536c4 moved. It used to be exempt — its length was
// "N whatever the wire carries", so the whole trailing run was elided — and it now
// obeys the same rule as its count-less sibling, character for character.
func TestTSLastArrayElementIsAlwaysWritten(t *testing.T) {
	mod := genTSWith(t, `
version: 1
messages:
  vec:
    payload:
      dynstr:    { id: 0, type: array, items: { type: string, maxlen: 8 } }
      dynblob:   { id: 1, type: array, items: { type: blob, maxlen: 8 } }
      fixedstr:  { id: 2, type: array, items: { type: string, count: 3, maxlen: 8 } }
      fixedblob: { id: 3, type: array, items: { type: blob, count: 3, maxlen: 8 } }
`, map[string]any{})

	for _, want := range []string{
		// The run is walked whole and the last index escapes the omit test — the same
		// shape with and without a count.
		"for (let _i0 = 0, _a0 = this.dynstr; _i0 < _a0.length; _i0++) {\n      if (_a0[_i0]! !== \"\" || _i0 === _a0.length - 1) {",
		"for (let _i0 = 0, _a0 = this.fixedstr; _i0 < _a0.length; _i0++) {\n      if (_a0[_i0]! !== \"\" || _i0 === _a0.length - 1) {",
		"for (let _i0 = 0, _a0 = this.dynblob; _i0 < _a0.length; _i0++) {\n      if (_a0[_i0]!.length !== 0 || _i0 === _a0.length - 1) {",
		"for (let _i0 = 0, _a0 = this.fixedblob; _i0 < _a0.length; _i0++) {\n      if (_a0[_i0]!.length !== 0 || _i0 === _a0.length - 1) {",
	} {
		if !strings.Contains(mod, want) {
			t.Errorf("message.ts missing %q:\n%s", want, mod)
		}
	}
	// A bare omit test with no last-index escape would encode ["a",""] as ["a"].
	if strings.Contains(mod, "if (_a0[_i0]! !== \"\") {") || strings.Contains(mod, "if (_a0[_i0]!.length !== 0) {") {
		t.Errorf("no leaf element may be omitted without the last-index escape:\n%s", mod)
	}
	// The all-default predicate follows the writer: [""] now puts an element on the
	// wire, so the field is NOT default and must not be omitted — of either kind.
	for _, want := range []string{
		"if (!(this.dynstr.length === 0)) return false;",
		"if (!(this.dynblob.length === 0)) return false;",
		"if (!(this.fixedstr.length === 0)) return false;",
		"if (!(this.fixedblob.length === 0)) return false;",
	} {
		if !strings.Contains(mod, want) {
			t.Errorf("isDefault must mirror the marshal loop, missing %q:\n%s", want, mod)
		}
	}
	for _, gone := range []string{"_trimStrs", "_trimBlobs"} {
		if strings.Contains(mod, gone) {
			t.Errorf("no string/blob array may be trimmed, counted or not (%q):\n%s", gone, mod)
		}
	}
}

// TestTSNestedWrapperRowCollectorTypes: a nested array whose ROW is itself a
// wrapper sequence — array<array<string>>, array<array<blob>>,
// array<array<struct>> and the same one level deeper — must declare its inline
// row collector with the ROW's type, not the container type of the level above.
//
// tsArrayType already answers with the container type for the level it is handed,
// so appending another "[]" in elemDecode declared `const _r: string[][]` for a
// row that the very next statements fill with LEAF strings (`_r.push("")`,
// `_r[_id] = c.readString()`). The emitted module then failed tsc with TS2345
// "Argument of type 'string' is not assignable to parameter of type 'string[]'"
// plus TS2322 follow-ons on the same line — the TypeScript analogue of the C++
// defect fixed as generator#250. Native rows (array<array<u32>>) never went
// through this path and are kept here as the control.
func TestTSNestedWrapperRowCollectorTypes(t *testing.T) {
	mod := genTSWith(t, `
version: 1
$defs:
  struct:
    Point:
      x: { id: 0, type: i32 }
      y: { id: 1, type: i32 }
messages:
  NestedRows:
    payload:
      strrows:    { id: 0, type: array, items: { type: array, count: 2, items: { type: string, count: 3, maxlen: 8 } } }
      blobrows:   { id: 1, type: array, items: { type: array, count: 2, items: { type: blob, count: 2, maxlen: 4 } } }
      structrows: { id: 2, type: array, items: { type: array, count: 2, items: { type: struct, count: 2, fields: { $ref: '#/$defs/struct/Point' } } } }
      strcube:    { id: 3, type: array, items: { type: array, count: 2, items: { type: array, count: 2, items: { type: string, count: 2, maxlen: 4 } } } }
      numrows:    { id: 4, type: array, items: { type: array, count: 2, items: { type: u32, count: 3 } } }
`, map[string]any{})

	// The row collector is declared with the row's own type: one "[]" less than the
	// member it is placed into. Depth 3 wraps the depth-2 collector, so the outer
	// row is string[][] and the inner one string[].
	for _, tc := range []struct{ member, row string }{
		{"string[][]", "string[]"},
		{"Uint8Array[][]", "Uint8Array[]"},
		{"StructPoint[][]", "StructPoint[]"},
		{"string[][][]", "string[][]"},
	} {
		if !strings.Contains(mod, "const arr: "+tc.member+" = [];") {
			t.Fatalf("expected a member declared %q:\n%s", tc.member, mod)
		}
		want := "arr[_id] = ((): " + tc.row + " => { const _r: " + tc.row + " = [];"
		if !strings.Contains(mod, want) {
			// The pristine emission put tc.member here instead: the collector carried
			// the container type of the level above while its body collected that
			// container's leaf elements.
			t.Errorf("row collector for %s must be typed %s, missing %q:\n%s", tc.member, tc.row, want, mod)
		}
	}

	// Structural invariant, independent of the shapes above: in every emitted row
	// collector `((): T => { const _r: T = []; … _r.push(D)`, the gap-fill default D
	// must be the default of T's ELEMENT type. A collector typed one level too high
	// gets exactly that wrong, whatever the shape.
	elemDefault := map[string]string{"string": `""`, "Uint8Array": "new Uint8Array()"}
	collectors := 0
	for rest := mod; ; {
		i := strings.Index(rest, "((): ")
		if i < 0 {
			break
		}
		rest = rest[i+len("((): "):]
		j := strings.Index(rest, " => { const _r: ")
		if j < 0 {
			continue
		}
		collectors++
		rowT := rest[:j]
		if !strings.HasSuffix(rowT, "[]") {
			t.Errorf("row collector type %q is not an array type:\n%s", rowT, mod)
			continue
		}
		elem := strings.TrimSuffix(rowT, "[]")
		want, ok := elemDefault[elem]
		if !ok {
			if strings.HasSuffix(elem, "[]") {
				want = "[]" // a deeper row: the empty row is the element default
			} else {
				want = "new " + elem + "()" // struct/union element
			}
		}
		body := rest[j:]
		if e := strings.Index(body, "; })()"); e >= 0 {
			body = body[:e]
		}
		if !strings.Contains(body, "_r.push("+want+")") {
			t.Errorf("collector typed %s must gap-fill with the %s default %s:\n%s", rowT, elem, want, body)
		}
	}
	if collectors != 5 { // 3 depth-2 rows + the depth-3 row and its inner row
		t.Errorf("expected 5 row collectors, found %d:\n%s", collectors, mod)
	}

	// Control: a row of native scalars still reads in one corelib call, untouched.
	if !strings.Contains(mod, "arr[_id] = c.readUnsignedArray(3) as number[];") {
		t.Errorf("a native row must still read in one call:\n%s", mod)
	}
}
