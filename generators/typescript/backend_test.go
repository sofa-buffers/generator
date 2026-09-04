package typescript

import (
	"os"
	"regexp"
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
	if !strings.Contains(mod, `import { OStream, FixlenSubtype, ArrayKind`) {
		t.Errorf("message.ts missing FixlenSubtype/ArrayKind import:\n%s", mod)
	}
	// A field whose WIRE TYPE contradicts the declared one needs no guard in the
	// generated code at all: the corelib routes by wire type, so such a field
	// arrives at a callback this id has no arm in — or at none — and is skipped
	// (MESSAGE_SPEC §7.3). A `string` at id 0 reaches `string()`, where only id 2
	// has an arm; it cannot reach the `unsigned()` arm that would store it.
	//
	// The two cases the wire type does NOT settle keep an explicit test, because
	// several declared kinds share one wire type:
	for _, want := range []string{
		// fp32/fp64/string/blob all ride WireType.Fixlen, so an ARRAY of them is
		// separated only by the announced element kind.
		`case 5: { if (kind !== ArrayKind.Unsigned) break; if (count > MAX_DYN_ARRAY_COUNT) throw new SofabError(SofabErrorCode.LimitExceeded, "f: array count above configured limit " + MAX_DYN_ARRAY_COUNT); const _d: number[] = []; this.o.f = _d; this._a0F = _d; break; }`,
		`case 6: { if (kind !== ArrayKind.Signed) break; if (count > MAX_DYN_ARRAY_COUNT) throw new SofabError(SofabErrorCode.LimitExceeded, "g: array count above configured limit " + MAX_DYN_ARRAY_COUNT); const _d: number[] = []; this.o.g = _d; this._a0G = _d; break; }`,
		`case 7: { if (kind !== ArrayKind.Fp64) break; if (count > MAX_DYN_ARRAY_COUNT) throw new SofabError(SofabErrorCode.LimitExceeded, "h: array count above configured limit " + MAX_DYN_ARRAY_COUNT); const _d: number[] = []; this.o.h = _d; this._a0H = _d; break; }`,
		// ...and a fixlen SCALAR is separated by the announced subtype, which is
		// what the collector of a string wrapper array tests for its elements.
		"  fixlenBegin(id: number, sub: FixlenSubtype, total: number): void {",
		"    this._q2?.begin(id, sub, total);",
		// A nested struct is a sequence: entering its scope is the whole guard.
		"    case 4: { this._c = _L_M_e; return true; }",
		// ...and its i32 field dispatches on the scope, not on the id alone: id 0
		// means something different in every scope of the tree.
		`      case _L_M_e: {`,
		`        case 0: { const _v = v as number; if (_v < -2147483648 || _v > 2147483647) throw new SofabError(SofabErrorCode.InvalidMsg, "x: value outside declared width i32"); this.o.e.x = _v; break; }`,
	} {
		if !strings.Contains(mod, want) {
			t.Errorf("message.ts missing wire-type guard %q\n%s", want, mod)
		}
	}
	// The header hook must not ignore what it was told.
	for _, bad := range []string{"arrayBegin(id: number, _kind:", "fixlenBegin(id: number, _sub:"} {
		if strings.Contains(mod, bad) {
			t.Errorf("%q ignores the announced kind/subtype (generator#300)", bad)
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
		// A leaf element's index bound is the corelib collector's: the capacity and
		// the element maxlen are constructor arguments, and it judges the index at
		// the length word — before the payload, and before the destination grows.
		`this._q1 = new StringSeq(_t, this.a, 4, 16, "bs", MAX_DYN_ARRAY_COUNT, MAX_DYN_STRING_LEN);`,
		`this._q2 = new BlobSeq(_t, this.a, 3, 16, "bb", MAX_DYN_ARRAY_COUNT, 1048576);`,
		// A FRAMED element opens a scope, so generated code places it — and the
		// guard runs before the gap-fill, so an over-index id extends nothing
		// (generator#247, CORELIB_PLAN §7.2 item 8).
		"        const _t = this.o.bp;\n" +
			"        if (id >= 2) throw new SofabError(SofabErrorCode.InvalidMsg, \"bp: array index above schema capacity 2\");\n" +
			"        while (_t.length <= id) _t.push(new MBpElem());\n",
		// A dynamic array has no schema capacity, so the receiver cap governs
		// instead (§6.2.1) — never both.
		`this._q5 = new StringSeq(_t, this.a, -1, -1, "ds", MAX_DYN_ARRAY_COUNT, MAX_DYN_STRING_LEN);`,
	} {
		if !strings.Contains(mod, want) {
			t.Errorf("message.ts missing over-index guard %q:\n%s", want, mod)
		}
	}
	// A schema-bounded array must NOT also carry the receiver cap as a rejection of
	// its own: §6.2.1 keeps a cap off a field the schema already bounds.
	if strings.Contains(mod, `if (id >= MAX_DYN_ARRAY_COUNT) throw new SofabError(SofabErrorCode.LimitExceeded, "bp:`) {
		t.Error("a schema-bounded wrapper array must not also apply the receiver cap (§6.2.1)")
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
	// A bounded native array takes its over-count verdict in arrayBegin, at the
	// COUNT WORD, before a single element arrives: that is what keeps an
	// over-count array INVALID rather than INCOMPLETE when the message is
	// truncated inside it (§5.2.3, F-0032).
	for _, want := range []string{
		`case 0: { if (kind !== ArrayKind.Unsigned) break; if (count > 4) throw new SofabError(SofabErrorCode.InvalidMsg, "ua: array count above schema capacity 4");`,
		// fp32 arrays keep the raw wire bits beside the value (§6.5,
		// generator#235), but the count verdict is the same one and stays at the
		// header — it must not be lost to the raw path.
		`case 1: { if (kind !== ArrayKind.Fp32) break; if (count > 3) throw new SofabError(SofabErrorCode.InvalidMsg, "fa: array count above schema capacity 3");`,
	} {
		if !strings.Contains(mod, want) {
			t.Errorf("message.ts missing header-bound reject %q:\n%s", want, mod)
		}
	}
	// The dynamic array must NOT gain a SCHEMA count (that would wrongly reject a
	// valid long array as INVALID). What it does carry is the receiver CAP, in the
	// same place and with the other category -- LimitExceeded, a policy rejection
	// of well-formed bytes (generator#388, CORELIB_PLAN §6.2.1). It also carries
	// the element width bound: a property of the element TYPE, not of the array
	// length, taken as each element arrives so a truncation behind an out-of-range
	// element cannot downgrade the verdict (§7.1).
	if !strings.Contains(mod, `case 2: { if (kind !== ArrayKind.Unsigned) break; if (count > MAX_DYN_ARRAY_COUNT) throw new SofabError(SofabErrorCode.LimitExceeded, "da: array count above configured limit " + MAX_DYN_ARRAY_COUNT); const _d: number[] = []; this.o.da = _d; this._a0Da = _d; break; }`) {
		t.Errorf("a dynamic array must carry the cap, never a schema count:\n%s", mod)
	}
	if strings.Contains(mod, `"da: array count above schema capacity`) {
		t.Errorf("a dynamic array must not gain a schema count bound:\n%s", mod)
	}
	if !strings.Contains(mod, `case 2: { const _e = v as number; if (_e > 4294967295) throw new SofabError(SofabErrorCode.InvalidMsg, "da: value outside declared width u32"); this._a0Da[i] = _e; break; }`) {
		t.Errorf("a dynamic array must still bound each element's declared width:\n%s", mod)
	}
	// The harness exposes a `status` mode surfacing the §7 outcome so INVALID vs
	// INCOMPLETE is assertable (the bare decode mode only yields a non-zero exit).
	for _, want := range []string{`mode === "status"`, `"INVALID\n"`, `"INCOMPLETE\n"`} {
		if !strings.Contains(harness, want) {
			t.Errorf("harness.ts missing status-mode surface %q:\n%s", want, harness)
		}
	}
	// ...and the CHUNKED decode surface (generator#456): the same raw wire bytes
	// `decode` takes, fed ONE BYTE PER feed, so every position inside every
	// skipped payload becomes a suspend/resume boundary. The Decoder classes sit
	// beside the message classes rather than on them, hence a map of their own.
	for _, want := range []string{
		`mode === "streamdecode"`,
		`const dec = new DECODERS[name]();`,
		`const fed = dec.feed(one);`,
		// The stream answers once, on feed's return, so the Decoder's `status` is
		// the wrapper's memory of it -- and nothing else in the suite reads that
		// memory, so a stale one would let every vector pass. Check it per byte
		// (generator#461).
		`if (dec.status !== fed) {`,
		// ...and on the refusal path NAME what the latch recorded. A reject
		// vector exits non-zero whatever the latch did, so printing the
		// remembered status is the only thing that makes an inverted -- or
		// absent -- mapping visible to the suite, which greps this line.
		"`decode error: ${String(e)} [status=${dec.status}]\\n`);",
		`obj = dec.finish();`,
		`"M": M.MDecoder,`,
	} {
		if !strings.Contains(harness, want) {
			t.Errorf("harness.ts missing streamdecode-mode surface %q:\n%s", want, harness)
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
	// (a2) The maxlen verdict is taken at the LENGTH WORD, via fixlenBegin — and it
	// tests the announced SUBTYPE. In the payload callback it could not fire at all
	// for a message ending right after an over-maxlen word, so the same bytes were
	// INVALID one-shot and INCOMPLETE chunked (generator#300). Asserting the exact
	// arms, because a bare "has a fixlenBegin" also matches the wrapper-element
	// collectors and would not notice the message-level one going missing.
	for _, want := range []string{
		"case 0: if (sub === FixlenSubtype.String && total > 8) throw new SofabError(SofabErrorCode.InvalidMsg, \"s: string byte length above schema maxlen 8\"); break;",
		"case 1: if (sub === FixlenSubtype.Blob && total > 8) throw new SofabError(SofabErrorCode.InvalidMsg, \"b: blob byte length above schema maxlen 8\"); break;",
	} {
		if !strings.Contains(mod, want) {
			t.Errorf("message.ts must take the maxlen verdict at the length word: missing %q", want)
		}
	}
	for _, want := range []string{
		// (b) The payload callback re-takes the same verdict against `total`, the
		// word that established the violation -- not against the assembled payload
		// -- so an over-maxlen field stays INVALID even when the message is
		// truncated inside it, and an over-long payload is never buffered (#267).
		`case 0: { if (total > 8) throw new SofabError(SofabErrorCode.InvalidMsg, "s: string byte length above schema maxlen 8");`,
		`case 1: { if (total > 8) throw new SofabError(SofabErrorCode.InvalidMsg, "b: blob byte length above schema maxlen 8");`,
		// (c) A bounded wrapper-string ELEMENT carries its maxlen into the corelib
		// collector, which takes the verdict at the element's length word rather
		// than after its payload -- generator#300's larger half.
		`new StringSeq(_t, this.a, -1, 5, "es", MAX_DYN_ARRAY_COUNT, MAX_DYN_STRING_LEN)`,
	} {
		if !strings.Contains(mod, want) {
			t.Errorf("message.ts missing maxlen guard %q\n%s", want, mod)
		}
	}
	// (d) An unbounded string keeps the bare store: never truncated, no guard.
	if !strings.Contains(mod, `case 2: { if (offset === 0 && end - start === total) { this.o.u = decodeUtf8(src, start, end); }`) {
		t.Errorf("unbounded string must keep the bare store:\n%s", mod)
	}
	// (e) A decoded blob is storage of its own -- what PayloadAcc hands back is a
	// buffer it allocated, never a view into the fed chunk (§6.7).
	if !strings.Contains(mod, "const _p = this.a.take(total, offset, src, start, end); if (_p !== null) this.o.b = _p;") {
		t.Errorf("a decoded blob must take the accumulator's own buffer:\n%s", mod)
	}
	// (f) The per-decode TextEncoder allocation is gone from the hot path (issue #153).
	if strings.Contains(mod, "TextEncoder") {
		t.Errorf("decode maxlen check must not allocate a TextEncoder per string (issue #153):\n%s", mod)
	}
}

func TestTSStructural(t *testing.T) {
	mod := genTS(t)
	for _, want := range []string{
		`import { OStream, FixlenSubtype, ArrayKind, DecodeStatus, SofabError, SofabErrorCode, elementsEqual, Visitor, IStream, PayloadAcc, decodeUtf8, StringSeq, BlobSeq, decode as _decode } from "@sofa-buffers/corelib";`, // FixlenSubtype: fixlen §7.3 guard; SofabError: over-count reject (generator#100); the rest is the generated layer's support, owned by the corelib (corelib-ts#151/#161)
		"export class Myfirstmessage {",
		"serialize(os: OStream): void {",
		// decode(bytes) is the corelib's one-shot decode driving THIS type's flat
		// visitor: one decode surface, because CORELIB_PLAN §5.3.1 permits no second.
		// It takes the bytes and nothing else: every receiver cap is a per-field
		// guard in generated code or an argument to a collector, so the corelib is
		// handed no DecodeLimits at all (generator#405).
		"  static decode(bytes: Uint8Array): Myfirstmessage {\n    const o = new Myfirstmessage();\n    _decode(bytes, new _MyfirstmessageVis(o, new PayloadAcc()));\n    return o;\n  }",
		// Dispatch is keyed on (location, id): a field id is unique only WITHIN a
		// scope, and corelib-ts's visitor is flat.
		"const _L_Myfirstmessage = 0;",
		"const _L_Myfirstmessage_somestruct_nestedstruct = 4;",
		"class _MyfirstmessageVis implements Visitor {",
		"  private _c = _L_Myfirstmessage;",
		// A nested struct/union enters its own scope and MERGES on re-open (§7.4):
		// nothing is cleared, so a second opening continues what the first set.
		"    case 20: { this._c = _L_Myfirstmessage_somestruct; return true; }",
		// A wrapper array REPLACES on re-open (§7.4), so its destination is rebuilt
		// and the corelib collector that owns the element rules is bound to it.
		`    case 18: { const _t: string[] = []; this.o.somestringarray = _t; this._q1 = new StringSeq(_t, this.a, 5, 16, "somestringarray", MAX_DYN_ARRAY_COUNT, 262144); this._c = _L_Myfirstmessage_somestringarray; return true; }`,
		// u64 -> bigint, off the number-first value the hook already carries.
		`    case 3: this.o.someu64 = typeof v === "bigint" ? v : BigInt(v); break;`,
		// MESSAGE_SPEC §2: a struct/union FIELD opens lazily and closes with the
		// dropping end, so an all-default nested object is omitted, not framed empty.
		"    os.writeSequenceBeginLazy(20);\n    this.somestruct.serialize(os);\n    os.writeSequenceEnd();\n",
		"    os.writeSequenceBeginLazy(21);\n    this.someunion.serialize(os);\n    os.writeSequenceEnd();\n",
		// A wrapper-array FIELD is a sequence too: lazy + dropping end at depth 0,
		// while each ELEMENT chooses its closer POSITIONALLY (§2) — the keeping one
		// at the last index, whose presence carries the array's length (§5.1), the
		// dropping one in the interior, where an all-default element leaves an id gap.
		"    os.writeSequenceBeginLazy(23);\n    this.somestructarray.forEach((_e0, _i0, _a0) => {\n      os.writeSequenceBeginLazy(_i0);\n      _e0.serialize(os);\n      if (_i0 === _a0.length - 1) {\n        os.writeSequenceEndKeep();\n      } else {\n        os.writeSequenceEnd();\n      }\n    });\n    os.writeSequenceEnd();\n",
		"    os.writeSequenceBeginLazy(25);\n    this.someunionarray.forEach((_e0, _i0, _a0) => {\n      os.writeSequenceBeginLazy(_i0);\n      _e0.serialize(os);\n      if (_i0 === _a0.length - 1) {\n        os.writeSequenceEndKeep();\n      } else {\n        os.writeSequenceEnd();\n      }\n    });\n    os.writeSequenceEnd();\n",
		// A leaf string/blob wrapper array is a FIELD as well.
		"    os.writeSequenceBeginLazy(18);\n",
		"    os.writeSequenceBeginLazy(19);\n",
		"export enum MyfirstmessageSomeenum {",
	} {
		if !strings.Contains(mod, want) {
			t.Errorf("message.ts missing %q", want)
		}
	}
	// There is exactly ONE decode surface (CORELIB_PLAN §5.3.1). The Cursor pull
	// decoder this backend used to emit beside the visitor is gone from
	// corelib-ts, and nothing may reintroduce a second route to the same bytes:
	// a second surface is a second implementation of every rule in the spec, and
	// the divergences it produces are invisible to the shared vectors.
	for _, gone := range []string{
		"Cursor", "readHeader()", "c.skip(", "_decodeFrom", "_decodeInto",
		// generator#345: the schema-free support the corelib owns (corelib-ts#151)
		// must be CALLED, not re-emitted — a leftover copy is dead weight in every
		// generated package and drifts from the library it duplicates.
		"class _Acc", "class _StrSeq", "class _BlobSeq",
		// ...and the collectors that were re-emitted because the CHILD-visitor
		// shape needed one visitor per element scope. A flat visitor routes the
		// element scope itself, so they have no reason to exist.
		"class _ObjSeq", "class _MatSeq", "class _RowSeq",
		"function arrEq", "new TextDecoder", "function _str(",
		"stringListVisitor",
		// corelib-ts#161 withdrew the opt-in Long channel: `lo`/`hi` are on every
		// integer hook now, so a 64-bit field takes the wire halves without a flag
		// and a narrow one no longer pays for the choice.
		"LongVisitor", "AnyVisitor", "readonly longs: true",
		// The eager begin no longer exists in corelib-ts: every sequence is opened
		// with writeSequenceBeginLazy (MESSAGE_SPEC §2).
		"os.writeSequenceBegin(",
		// corelib-ts#170 removed IStream's status accessor: `feed`'s return is the
		// only channel the outcome travels on (§5.2.4). Asking the stream a second
		// time no longer compiles, and must not come back (generator#461).
		"this.is.status()",
	} {
		if strings.Contains(mod, gone) {
			t.Errorf("message.ts should no longer emit %q (one visitor surface, §5.3.1)", gone)
		}
	}
	// The streaming surface is the same visitor, fed incrementally.
	for _, want := range []string{
		"export class MyfirstmessageDecoder {",
		// corelib-ts#170 left `feed`'s return as the only place the stream
		// publishes its outcome. The wrapper is the caller, and a caller may
		// remember: the public surface is unchanged and backed by a field.
		// Complete before the first feed -- an all-default message is zero bytes,
		// so a decoder that was never fed still finishes (generator#461).
		"  private st: DecodeStatus = DecodeStatus.Complete;",
		"      return (this.st = this.is.feed(chunk));",
		// A refusal is terminal and never comes back as a status, so it is
		// latched: InvalidMsg is the wire verdict, a receiver cap is this
		// side's own stop and leaves the message unfinished, not wrong (S6.3).
		// A throw that is not the corelib's -- a TypeError out of a callback, an
		// Argument fault -- is not a wire event at all, so it leaves the memory
		// alone rather than claiming the stream ended mid-field.
		"      if (e instanceof SofabError) {",
		"        if (e.code === SofabErrorCode.InvalidMsg) {",
		"          this.st = DecodeStatus.Invalid;",
		"          e.code === SofabErrorCode.LimitExceeded ||",
		"          this.st = DecodeStatus.Incomplete;",
		"  get status(): DecodeStatus { return this.st; }",
		"    if (this.st !== DecodeStatus.Complete) {",
		"  sequenceBegin(id: number): boolean {",
		// The scope graph is a tree, so the parent is static and sequenceEnd needs
		// no stack to restore it.
		"  sequenceEnd(): void {",
		"    case _L_Myfirstmessage_somestruct_nestedstruct: this._c = _L_Myfirstmessage_somestruct; break;",
		// Malformed UTF-8 leaves as SofabError, because the store site transcodes
		// through the corelib's decodeUtf8 rather than a fatal TextDecoder of its
		// own: that one raises a platform TypeError, which walks past a caller's
		// `instanceof SofabError` guard (generator#297).
		"this.o.somestring = decodeUtf8(src, start, end);",
		// An array header is routed by id alone, so the arm also receives one whose
		// element kind CONTRADICTS the declared field. Such a field is skipped whole
		// (S7.3): its count is not this field's count, so the kind test has to come
		// before the capacity bound -- and before the destination is cleared
		// (generator#300).
		"case 15: { if (kind !== ArrayKind.Unsigned) break; if (count > 4) throw new SofabError(SofabErrorCode.InvalidMsg, \"someuintarray: array count above schema capacity 4\");",
	} {
		if !strings.Contains(mod, want) {
			t.Errorf("message.ts missing decode surface %q", want)
		}
	}
	// The header hook must not ignore the announced kind: an `_kind` parameter is
	// how generator#300 happened, and it reads as a deliberate "not needed".
	if strings.Contains(mod, "arrayBegin(id: number, _kind: ArrayKind") {
		t.Error("arrayBegin must test the announced element kind, not ignore it (generator#300)")
	}
	// The maxlen verdict must be taken at the LENGTH WORD, via fixlenBegin. In the
	// payload callback it cannot fire at all for a message that ends right after an
	// over-maxlen word, so a truncated message would report INCOMPLETE where §5.2.3
	// requires INVALID. Like arrayBegin, it has to test the announced subtype
	// rather than trust the id: a string arriving at a blob field's id is a §7.3
	// mismatch to skip, not something to bound.
	if strings.Contains(mod, "fixlenBegin(id: number, _sub:") {
		t.Error("fixlenBegin must test the announced subtype, not ignore it (generator#300)")
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
		"    os.writeSequenceBeginLazy(0);\n    this.s.serialize(os);\n    os.writeSequenceEnd();\n",
		// The leaf runs are walked whole — nothing is narrowed away — and the last
		// element escapes the omit test (§2, see TestTSArrayElementSparsityIsPositional).
		"    os.writeSequenceBeginLazy(2);\n    for (let _i0 = 0, _a0 = this.strs; _i0 < _a0.length; _i0++) {",
		"    os.writeSequenceBeginLazy(4);\n    for (let _i0 = 0, _a0 = this.blobs; _i0 < _a0.length; _i0++) {",
		"    os.writeSequenceBeginLazy(1);\n    this.ss.forEach((_e0, _i0, _a0) => {\n      os.writeSequenceBeginLazy(_i0);\n      _e0.serialize(os);\n      if (_i0 === _a0.length - 1) {\n        os.writeSequenceEndKeep();\n      } else {\n        os.writeSequenceEnd();\n      }\n    });\n    os.writeSequenceEnd();\n",
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
		`import { OStream, ArrayKind, DecodeStatus, Long, SofabError, SofabErrorCode, elementsEqual, Visitor, IStream, PayloadAcc, decode as _decode } from "@sofa-buffers/corelib";`,
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
		// Decode bypasses the setter (the hot path writes the canonical Long[]
		// directly); a wire count above the schema capacity rejects as INVALID at
		// the count word (generator#100), and a wire count below it is simply the
		// array's length — nothing is filled in.
		`case 0: { if (kind !== ArrayKind.Unsigned) break; if (count > 8) throw new SofabError(SofabErrorCode.InvalidMsg, "us: array count above schema capacity 8"); const _d: Long[] = []; this.o["_us"] = _d; this._a0Us = _d; break; }`,
		`case 0: this._a0Us[i] = Long.fromBits(lo, hi); break;`,
		`case 1: this._a0Is[i] = Long.fromBits(lo, hi); break;`,
		// toJSON prints via Long.toString with the schema signedness.
		`"us": this._us.map((_x0) => _x0.toString(false)),`,
		`"is": this._is.map((_x0) => _x0.toString(true)),`,
		// fromJSON keeps the bigint parse and lets the setter convert once.
		`if ("us" in d) o.us = (d["us"] as (string | number)[]).map((_x0) => BigInt(_x0));`,
		// Scalars are Long-backed too (generator#339, corelib-ts#143): the same
		// private-field-plus-accessor shape as the arrays, one level down. The
		// zero default is the shared immutable Long.ZERO, never Long.fromValue(0n).
		"private _u: Long = Long.ZERO;",
		"get u(): Long { return this._u; }",
		"set u(v: Long | bigint | number) { this._u = Long.fromValue(v); }",
		"private _i: Long = Long.fromValue(-7n);",
		// The omit test is a (low, high) compare against halves computed at
		// generation time — `===` on a Long would compare object identity, and
		// nothing is allocated per call. -7 is 0xFFFFFFFF_FFFFFFF9.
		"if (!(this._u.low === 0 && this._u.high === 0)) {",
		"os.writeUnsignedLong(4, this._u);",
		"if (!(this._i.low === 4294967289 && this._i.high === 4294967295)) {",
		"os.writeSignedLong(5, this._i);",
		"if (!(this._i.low === 4294967289 && this._i.high === 4294967295)) return false;",
		// Decode bypasses the accessor and takes the corelib's scalar Long readers,
		// so no bigint is materialised on the hot path in either direction.
		"case 4: this.o[\"_u\"] = Long.fromBits(lo, hi); break;",
		"case 5: this.o[\"_i\"] = Long.fromBits(lo, hi); break;",
		// corelib-ts#161 withdrew the opt-in `Visitor.longs` channel and put `lo`/`hi`
		// -- the exact wire halves the varint reader already holds -- on every
		// integer hook instead. A Long-backed destination therefore takes the value
		// with no conversion at all and no flag to opt into, and a NARROW field in
		// the same message no longer pays for that choice: the channel was read once
		// from the root and covered every field alike (#344, #335).
		"class _MVis implements Visitor {",
		"  unsigned(id: number, v: number | bigint, lo: number, hi: number): void {",
		"  arrayUnsigned(id: number, i: number, v: number | bigint, lo: number, hi: number): void {",
		"  sequenceBegin(id: number): boolean {",
		// JSON keeps the decimal-string form, with the schema's signedness.
		`"u": this._u.toString(false),`,
		`"i": this._i.toString(true),`,
		`if ("u" in d) o.u = Long.fromValue(BigInt(d["u"] as string | number));`,
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
		// No bigint scalar survives anywhere in this mode: not as a declared type,
		// not as a default, not on either decode path (generator#339).
		"u: bigint", "i: bigint", "= 0n;", "= -7n;",
		"BigInt(c.readUnsigned())", "BigInt(c.readSigned())",
		"os.writeUnsigned(4,", "os.writeSigned(5,",
	} {
		if strings.Contains(mod, gone) {
			t.Errorf("int64: long message.ts should not emit %q", gone)
		}
	}
}

// The Long channel is a TRADE, so the backend takes it only where the schema says
// it pays: every integer value on the push path becomes a Long, which a 64-bit
// destination wants and a narrow one does not (generator#344, corelib-ts#146).
// Measured break-even and the threshold: see longsThreshold.
// TestTSArrayBulkIsOfferedOnlyWhereItPays pins both halves of the bulk
// destination hand-off (corelib-ts BULK_MIN): which arrays get an `arrayBulk` arm,
// and which deliberately do not.
//
// The offer is a call out to the visitor and costs ~1300 Ir per array; the fill it
// enables saves ~435-730 Ir per element. So it pays from a handful of elements up
// and loses below that — measured on a message whose arrays are all four elements
// long, offering unconditionally cost +7572 Ir and returned 1740. A declared
// `count` is a CAPACITY, so an array declared under the threshold can never reach
// it on the wire and the arm is left out statically.
func TestTSArrayBulkIsOfferedOnlyWhereItPays(t *testing.T) {
	mod := genTSWith(t, `
version: 1
$defs:
  enum:
    Mode: { Off: { value: 0 }, On: { value: 1 } }
messages:
  M:
    payload:
      big:   { id: 0, type: array, items: { type: u32, count: 64 } }
      open:  { id: 1, type: array, items: { type: i16 } }
      small: { id: 2, type: array, items: { type: u16, count: 4 } }
      wide:  { id: 3, type: array, items: { type: u64, count: 64 } }
      flags: { id: 4, type: array, items: { type: boolean, count: 64 } }
      mode:  { id: 5, type: array, items: { type: enum, count: 64, enum: { $ref: "#/$defs/enum/Mode" } } }
      fp:    { id: 6, type: array, items: { type: fp32, count: 64 } }
`, map[string]any{})

	for _, want := range []string{
		// A schema-bounded array at or above the threshold, and an array the schema
		// left open (only the wire knows its length, so the corelib's own gate
		// decides per message).
		`case 0: { if (kind !== ArrayKind.Unsigned) break; const _t = this._bt; _t.out = this._a0Big; _t.min = 0; _t.max = 4294967295; return _t; }`,
		`case 1: { if (kind !== ArrayKind.Signed) break; const _t = this._bt; _t.out = this._a0Open; _t.min = -32768; _t.max = 32767; return _t; }`,
		// ONE target for the whole visitor, re-pointed per array.
		"  private readonly _bt: ArrayTarget = { out: [], min: 0, max: 0 };",
		// The element arms stay, for the arrays that decline and for a corelib that
		// predates the hook. That is what makes taking it additive.
		`case 2: { const _e = v as number; if (_e > 65535) throw new SofabError(SofabErrorCode.InvalidMsg, "small: value outside declared width u16"); this._a0Small[i] = _e; break; }`,
	} {
		if !strings.Contains(mod, want) {
			t.Errorf("message.ts missing %q:\n%s", want, mod)
		}
	}
	// No arm for: an array declared too short to pay; a destination that is not a
	// plain JS number (u64 -> bigint, boolean); or a kind with no declared width to
	// state (enum, bitfield) — inventing bounds there would reject legal values.
	// fp32/fp64 are never offered one by the corelib at all.
	for _, id := range []string{"case 2: { if (kind", "case 3: { if (kind", "case 4: { if (kind", "case 5: { if (kind", "case 6: { if (kind"} {
		if strings.Contains(mod, id+" !== ArrayKind.Unsigned) break; const _t") ||
			strings.Contains(mod, id+" !== ArrayKind.Signed) break; const _t") ||
			strings.Contains(mod, id+" !== ArrayKind.Fp32) break; const _t") {
			t.Errorf("%s must not be offered a bulk destination:\n%s", id, mod)
		}
	}
	// ...and a schema whose arrays ALL fall below the threshold emits no hook at
	// all, so nothing pays the call.
	short := genTSWith(t, `
version: 1
messages:
  S:
    payload:
      a: { id: 0, type: array, items: { type: u16, count: 4 } }
      b: { id: 1, type: array, items: { type: u32, count: 8 } }
`, map[string]any{})
	if strings.Contains(short, "arrayBulk") || strings.Contains(short, "ArrayTarget") {
		t.Errorf("a schema of short arrays must emit no bulk hook:\n%s", short)
	}
}

func TestTSLongChannelIsWithdrawn(t *testing.T) {
	// A narrow-heavy schema and a 64-bit-heavy one used to generate DIFFERENTLY:
	// the opt-in `Visitor.longs` channel was read once from the root visitor and
	// covered every integer field alike, so taking it made 64-bit fields free and
	// narrow ones dearer, and the backend had to guess from the schema which way
	// that traded (#344). corelib-ts#161 put `lo` / `hi` on every integer hook
	// instead, so there is no trade left and both schemas emit the same shape.
	narrow := `
version: 1
messages:
  m:
    payload:
      u:  { id: 0, type: u64 }
      a:  { id: 1, type: array, items: { type: u64, count: 4 } }
      n0: { id: 2, type: u8 }
      n1: { id: 3, type: i8 }
      n2: { id: 4, type: boolean }
      n3: { id: 5, type: u16 }
      n4: { id: 6, type: i16 }
      n5: { id: 7, type: i32 }
`
	wide := `
version: 1
messages:
  m:
    payload:
      u:  { id: 0, type: u64 }
      a:  { id: 1, type: array, items: { type: u64, count: 4 } }
      n0: { id: 2, type: u8 }
      n1: { id: 3, type: i8 }
      n2: { id: 4, type: boolean }
`
	for _, src := range []string{narrow, wide} {
		mod := genTSWith(t, src, map[string]any{"int64": "long"})
		for _, gone := range []string{"longs: true", "LongVisitor", "AnyVisitor", "function _u(", "function _i("} {
			if strings.Contains(mod, gone) {
				t.Errorf("the Long channel is withdrawn, but the module still emits %q", gone)
			}
		}
		for _, want := range []string{
			"  unsigned(id: number, v: number | bigint, lo: number, hi: number): void {",
			// A 64-bit destination is built from the halves: no bigint on the path
			// the Long mode exists to keep bigint-free, and no flag to reach it.
			`case 0: this.o["_u"] = Long.fromBits(lo, hi); break;`,
			`case 1: this._a0A[i] = Long.fromBits(lo, hi); break;`,
			// ...and a narrow one in the SAME message reads the number-first value
			// directly, which is what it costs now that it is not paying for the
			// 64-bit fields' channel.
			`case 2: { const _v = v as number; if (_v > 255) throw new SofabError(SofabErrorCode.InvalidMsg, "n0: value outside declared width u8"); this.o.n0 = _v; break; }`,
			`case 3: { const _v = v as number; if (_v < -128 || _v > 127) throw new SofabError(SofabErrorCode.InvalidMsg, "n1: value outside declared width i8"); this.o.n1 = _v; break; }`,
			"case 4: this.o.n2 = Boolean(v); break;",
		} {
			if !strings.Contains(mod, want) {
				t.Errorf("message.ts missing %q", want)
			}
		}
	}
}

// The chunked decode surface needs its own bench workload: `decode_*` is the
// whole-buffer pull cursor, so a change confined to the push path — the Long
// channel is exactly one — does not show up there at all (generator#344).
func TestTSBenchStreamWorkload(t *testing.T) {
	files, err := (&Backend{}).Generate(schema(t, int64Def), map[string]any{"int64": "long", "emit": "project"})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	var harness string
	for _, f := range files {
		if f.Path == "harness.ts" {
			harness = string(f.Content)
		}
	}
	if harness == "" {
		t.Fatal("no harness.ts")
	}
	for _, want := range []string{
		`if (w === "stream_m") {`,
		"const _d = new M.MDecoder(); _d.feed(wire);",
	} {
		if !strings.Contains(harness, want) {
			t.Errorf("harness.ts missing %q", want)
		}
	}
}

func TestTSInt64Number(t *testing.T) {
	mod := genTSWith(t, int64Def, map[string]any{"int64": "number"})
	for _, want := range []string{
		// Arrays are Long-backed exactly as in long mode, and take the wire halves.
		"os.writeUnsignedArrayLong(0, this._us);",
		`case 0: this._a0Us[i] = Long.fromBits(lo, hi); break;`,
		// Scalars are plain numbers: number default, !== 0 guard, Number() decode.
		"u: number = 0;",
		"i: number = -7;",
		"if (this.u !== 0) {",
		"os.writeUnsigned(4, this.u);",
		"case 4: this.o.u = Number(v); break;",
		"case 5: this.o.i = Number(v); break;",
		`if ("u" in d) o.u = Number(d["u"] as string | number);`,
		// toJSON stays a string (number.toString()) for cross-mode JSON parity.
		`"u": this.u.toString(),`,
	} {
		if !strings.Contains(mod, want) {
			t.Errorf("int64: number message.ts missing %q", want)
		}
	}
}

// TestTSDecodeLimits: the max_dyn_* config keys bake receiver-side decode limits
// (generator#102) into the generated module as exported MAX_DYN_* constants, and
// generator#388 moved their ENFORCEMENT into the visitor -- per field, at the
// count/length header, as the else of the schema bound.
//
// Two consequences this pins. The values are emitted AS CONFIGURED: the raise to
// the largest schema bound existed only because the caps rode into the corelib as
// a DecodeLimits applied globally per decode, which would otherwise have rejected
// a schema-bounded sibling. And no DecodeLimits is passed at all for this schema
// -- there is no wrapper string/blob element here, so nothing is left that
// generated code cannot bound itself.
//
// A key whose kind has no unbounded field stays inert, and the plumbing is
// identical across all three int64 modes.
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
			"export const MAX_DYN_ARRAY_COUNT = 65536;", // as configured, NOT raised to barr's 100000
			"export const MAX_DYN_STRING_LEN = 4096;",
			// Enforced per field, in the visitor, at the header.
			`case 0: if (sub === FixlenSubtype.String && total > MAX_DYN_STRING_LEN) throw new SofabError(SofabErrorCode.LimitExceeded, "s: string byte length above configured limit " + MAX_DYN_STRING_LEN); break;`,
			`if (count > MAX_DYN_ARRAY_COUNT) throw new SofabError(SofabErrorCode.LimitExceeded, "arr: array count above configured limit " + MAX_DYN_ARRAY_COUNT);`,
			// ...and `barr`, which the schema bounds, keeps its own bound and its own
			// category. At a cap of 65536 the old global DecodeLimits would have
			// rejected it outright -- which is exactly why the raise had to exist.
			`if (count > 100000) throw new SofabError(SofabErrorCode.InvalidMsg, "barr: array count above schema capacity 100000");`,
			// No DecodeLimits: nothing here is beyond the visitor's reach.
			"_decode(bytes, new _DynVis(o, new PayloadAcc()));",
		} {
			if !strings.Contains(mod, want) {
				t.Errorf("int64: %s message.ts missing %q", mode, want)
			}
		}
		if strings.Contains(mod, "MAX_DYN_BLOB_LEN") {
			t.Errorf("int64: %s: inert blob limit must not be emitted (no unbounded blob)", mode)
		}
		if strings.Contains(mod, "_LIMITS") {
			t.Errorf("int64: %s: the corelib must take no cap for a schema the visitor fully covers:\n%s", mode, mod)
		}
	}

	// No keys configured -> the target's finite DEFAULTS, not "unlimited"
	// (§9.5, generator#385). TypeScript is on the client tier: 16384 elements and
	// 256 KiB of string, and neither is raised.
	plain := genTSWith(t, src, map[string]any{})
	for _, want := range []string{
		"export const MAX_DYN_ARRAY_COUNT = 16384;",
		"export const MAX_DYN_STRING_LEN = 262144;",
		"_decode(bytes, new _DynVis(o, new PayloadAcc()));",
	} {
		if !strings.Contains(plain, want) {
			t.Errorf("default limits missing %q", want)
		}
	}
	// Liveness is still a property of the schema, not of the configuration.
	if strings.Contains(plain, "MAX_DYN_BLOB_LEN") {
		t.Error("inert blob limit must not be emitted (no unbounded blob)")
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
		// Decode: the M elements that arrived ARE the value, taken as they come --
		// no fill-to-count on arrayEnd. The over-count reject stays and is taken at
		// the count word, where `count` still bounds M (generator#100).
		`case 0: { if (kind !== ArrayKind.Unsigned) break; if (count > 5) throw new SofabError(SofabErrorCode.InvalidMsg, "fu32: array count above schema capacity 5"); const _d: number[] = []; this.o.fu32 = _d; this._a0Fu32 = _d; break; }`,
		`case 4: { if (kind !== ArrayKind.Fp64) break; if (count > 3) throw new SofabError(SofabErrorCode.InvalidMsg, "ffp64: array count above schema capacity 3"); const _d: number[] = []; this.o.ffp64 = _d; this._a0Ffp64 = _d; break; }`,
		`case 6: this._a0Fbool[i] = Boolean(v); break;`,
		// An ENUM array is SIGNED on the wire (serialize writes writeSignedArray),
		// so its header must be recognised as such -- classifying it as unsigned
		// made arrayBegin skip every enum array as a §7.3 contradiction, losing the
		// count bound and the §7.4 replace while the elements still arrived.
		`case 8: { if (kind !== ArrayKind.Signed) break; if (count > 2) throw new SofabError(SofabErrorCode.InvalidMsg, "fenum: array count above schema capacity 2"); const _d: EnumMode[] = []; this.o.fenum = _d; this._a0Fenum = _d; break; }`,
		`case 8: this._a0Fenum[i] = Number(v) as EnumMode; break;`,
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
		"if (!elementsEqual(this.short, [1, 2])) {",
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
		"if (!elementsEqual(this.none,",
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
		// An fp32/fp64 ROW is separated by the corelib's ArrayKind, which names the
		// element subtype directly (Fp32 / Fp64) -- so the row guard needs no
		// FixlenSubtype, where the withdrawn Cursor path had only WireType.Fixlen
		// to work with and had to reach for the subtype.
		{"nested fp32 rows", false, `
      grid: { id: 0, type: array, items: { type: array, items: { type: fp32 } } }
      n:    { id: 1, type: u32 }`},
		{"doubly nested blob rows", true, `
      cube: { id: 0, type: array, items: { type: array, items: { type: array, items: { type: blob } } } }
      n:    { id: 1, type: u32 }`},
		// An unbounded string carries a length verdict too since generator#388 -- the
		// receiver cap, taken at the same length word as a schema maxlen and keyed on
		// the same subtype (§7.3: a field of another type at this id was never this
		// field's value and must not be measured against its bound).
		{"string inside a struct element", true, `
      items: { id: 0, type: array, items: { type: struct, fields: { s: { id: 0, type: string } } } }
      n:     { id: 1, type: u32 }`},
		{"bounded string inside a struct element", true, `
      items: { id: 0, type: array, items: { type: struct, fields: { s: { id: 0, type: string, maxlen: 8 } } } }
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
			`import { OStream, ArrayKind, DecodeStatus, SofabError, SofabErrorCode, elementsEqual, Visitor, IStream, PayloadAcc, decode as _decode } from "@sofa-buffers/corelib";`,
			// count: 8 is a CAPACITY, so a fresh array is empty (§3, af536c4).
			"us: bigint[] = [];",
			// ...and the value goes out whole, the wire count being its length.
			"os.writeUnsignedArray(0, this.us);",
			`case 0: this._a0Us[i] = typeof v === "bigint" ? v : BigInt(v); break;`,
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
// not recur must survive. The decode loop therefore lives in _decodeInto<T>(c, o)
// and nested members decode INTO the existing object; assigning the from-decoder's fresh
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
		// Entering a nested struct/union scope CLEARS NOTHING: the fields decode
		// into the member that is already there, so a re-opened scope continues
		// what the first opening set.
		"    case 0: { this._c = _L_M_s; return true; }",
		"    case 1: { this._c = _L_M_u; return true; }",
		"        case 0: { const _v = v as number; if (_v > 255) throw new SofabError(SofabErrorCode.InvalidMsg, \"a: value outside declared width u8\"); this.o.s.a = _v; break; }",
		"        case 0: { const _v = v as number; if (_v > 255) throw new SofabError(SofabErrorCode.InvalidMsg, \"o1: value outside declared width u8\"); this.o.u.o1 = _v; break; }",
	} {
		if !strings.Contains(mod, want) {
			t.Errorf("message.ts missing %q\n%s", want, mod)
		}
	}
	// A nested member must never be replaced by a fresh object on re-open. Only a
	// wrapper ARRAY replaces (§7.4), and that is the `_t = []` in its own arm.
	for _, bad := range []string{"this.o.s = new MS()", "this.o.u = new MU()"} {
		if strings.Contains(mod, bad) {
			t.Errorf("message.ts must not replace a nested member (%q):\n%s", bad, mod)
		}
	}
	// A wrapper-array ELEMENT is not new per arrival either: the element id IS the
	// array index (§5.1), so a REOPENED element id re-opens that element's scope
	// and must merge into it exactly like a re-opened field (§7.4, generator#247).
	// The gap-fill places an element at the index and the element scope decodes
	// into whatever is there -- so a repeat lands on the same object.
	if !strings.Contains(mod, "        while (_t.length <= id) _t.push(new MEElem());\n        this._ix3 = id;") {
		t.Errorf("array elements must decode INTO the element at their id:\n%s", mod)
	}
	if !strings.Contains(mod, "this.o.e[this._ix3]!.x = _v;") {
		t.Errorf("an element scope must write through the index register:\n%s", mod)
	}
	if strings.Contains(mod, "this._ix3 = id; _t[id] = new MEElem()") {
		t.Errorf("a re-opened element id must not be replaced by a fresh object:\n%s", mod)
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
		"    os.writeSequenceBeginLazy(0);\n    this.fixed.forEach((_e0, _i0, _a0) => {\n      os.writeSequenceBeginLazy(_i0);\n      _e0.serialize(os);\n      if (_i0 === _a0.length - 1) {\n        os.writeSequenceEndKeep();\n      } else {\n        os.writeSequenceEnd();\n      }\n    });\n    os.writeSequenceEnd();\n",
		"    os.writeSequenceBeginLazy(1);\n    this.dynamic.forEach((_e0, _i0, _a0) => {\n      os.writeSequenceBeginLazy(_i0);\n      _e0.serialize(os);\n      if (_i0 === _a0.length - 1) {\n        os.writeSequenceEndKeep();\n      } else {\n        os.writeSequenceEnd();\n      }\n    });\n    os.writeSequenceEnd();\n",
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
		// Placement, not append — the gap-fill precedes it, and the index bound
		// precedes the gap-fill so a rejected id extends nothing (§7.2 item 8).
		"        if (id >= 4) throw new SofabError(SofabErrorCode.InvalidMsg, \"objs: array index above schema capacity 4\");\n" +
			"        while (_t.length <= id) _t.push(new VecObjsElem());\n",
		// A count-less array has no schema capacity, so the receiver cap governs
		// instead — a policy rejection, not INVALID (§6.2.1).
		"        if (id >= MAX_DYN_ARRAY_COUNT) throw new SofabError(SofabErrorCode.LimitExceeded, \"dyn: array index \" + id + \" exceeds the receiver cap \" + MAX_DYN_ARRAY_COUNT);\n" +
			"        while (_t.length <= id) _t.push(new VecDynElem());\n",
		// A leaf element's placement is the corelib collector's, which does the
		// same thing with the same ordering.
		`this._q5 = new StringSeq(_t, this.a, 3, 8, "strs", MAX_DYN_ARRAY_COUNT, 262144);`,
		// A native ROW is placed by id too. The id-blind append was unreachable
		// while every row was written, and an interior gap makes it reachable,
		// shifting every later row down one index.
		"    while (_t.length <= id) _t.push([]);\n" +
			"    const _r: number[] = []; _t[id] = _r; this._row6 = _r;",
		// ...including a WRAPPER row, whose own collector is bound to the row the
		// placement just made — a re-opened row index replaces (§7.4).
		"        const _e: string[] = []; _t[id] = _e;\n" +
			"        this._q8 = new StringSeq(_e, this.a, -1, 8, \"wrows row\", MAX_DYN_ARRAY_COUNT, 262144);",
	} {
		if !strings.Contains(mod, want) {
			t.Errorf("message.ts missing %q:\n%s", want, mod)
		}
	}
	// The defects this replaced: appending ignored the id entirely.
	for _, bad := range []string{"_t.push(new VecObjsElem()); this._c", ".push(_r);", "_t.push(_e);"} {
		if strings.Contains(mod, bad) {
			t.Errorf("elements must not be appended id-blind (%q):\n%s", bad, mod)
		}
	}
	// `count: N` never ADDS an element: the decoded length is highest present id + 1,
	// exactly as for a count-less array, because the last element is always on the
	// wire. The fill-to-N that the superseded fixed-length reading needed is gone.
	for _, gone := range []string{
		"while (_t.length < 4)", "while (_t.length < 3)", "while (_r.length < ",
	} {
		if strings.Contains(mod, gone) {
			t.Errorf("a decoded wrapper array must not be filled to N (%q):\n%s", gone, mod)
		}
	}
	// A scope arm leaves the callback with `return`, never a bare `break`: the arm
	// is emitted inside a `case` only when the hook has more than one scope, and
	// bare when it has one — where a `break` has no enclosing switch and is a
	// SyntaxError. `rows` is this schema's only arrayBegin scope.
	if strings.Contains(mod, "if (kind !== ArrayKind.Unsigned) break;") {
		t.Errorf("a single-scope arm must not `break` — there is no switch to break out of:\n%s", mod)
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
		"if (!(elementsEqual(this.nat, [1, 2, 3]))) return false;",
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

	// The FIELD is declared with the member's type; the ROW built inside its scope
	// is one "[]" less. Depth 3 has two levels of row, so the outer one is
	// string[][] and the inner one string[] — a row typed one level too high was
	// the defect this pins.
	for _, tc := range []struct{ member, row string }{
		{"string[][]", "string[]"},
		{"Uint8Array[][]", "Uint8Array[]"},
		{"StructPoint[][]", "StructPoint[]"},
		{"string[][][]", "string[][]"},
	} {
		if !strings.Contains(mod, "const _t: "+tc.member+" = [];") {
			t.Fatalf("expected a member declared %q:\n%s", tc.member, mod)
		}
		if !strings.Contains(mod, "const _e: "+tc.row+" = []; _t[id] = _e;") {
			t.Errorf("row of %s must be typed %s:\n%s", tc.member, tc.row, mod)
		}
	}
	// A LEAF row hands its elements to the corelib collector, carrying the row's
	// own capacity and element maxlen — never the enclosing array's.
	for _, want := range []string{
		`this._q2 = new StringSeq(_e, this.a, 3, 8, "strrows row", 16384, 262144);`,
		`this._q4 = new BlobSeq(_e, this.a, 2, 4, "blobrows row", 16384, 1048576);`,
		`this._q10 = new StringSeq(_e, this.a, 2, 4, "strcube row row", 16384, 262144);`,
	} {
		if !strings.Contains(mod, want) {
			t.Errorf("message.ts missing row collector %q:\n%s", want, mod)
		}
	}
	// A FRAMED row opens a scope of its own instead, one level down, and its
	// elements are placed there.
	if !strings.Contains(mod, "      case _L_NestedRows_structrows_r: {\n        const _t = this.o.structrows[this._ix5]!;") {
		t.Errorf("a struct row must open its own element scope:\n%s", mod)
	}
	// A NATIVE row needs no collector at all: its elements arrive on the array
	// hooks in the row scope, and the row register carries the destination.
	if !strings.Contains(mod, "const _r: number[] = []; _t[id] = _r; this._row11 = _r;") {
		t.Errorf("a native row must be held in a row register:\n%s", mod)
	}
	// The generated collectors this replaced are gone: the corelib owns the leaf
	// ones, and a flat visitor routes the framed and native rows itself.
	for _, gone := range []string{"class _ObjSeq", "class _MatSeq", "class _RowSeq"} {
		if strings.Contains(mod, gone) {
			t.Errorf("message.ts must no longer emit %q (ARCHITECTURE §8):\n%s", gone, mod)
		}
	}
}

const fp32RawDef = `version: 1
messages:
  m:
    payload:
      f32:  { id: 0, type: fp32 }
      f32d: { id: 1, type: fp32, default: 1.5 }
      fa:   { id: 2, type: array, items: { type: fp32, count: 3 } }
      da:   { id: 3, type: array, items: { type: fp32 } }
      f64:  { id: 4, type: fp64 }
      d64:  { id: 5, type: array, items: { type: fp64, count: 3 } }
      st:   { id: 6, type: struct, fields: { inner: { id: 0, type: fp32 }, innera: { id: 1, type: array, items: { type: fp32, count: 2 } } } }
`

// TestTSFp32SignalingNaNRawChannel pins the fix for generator#235: a JS number is
// a 64-bit double, so widening an fp32 SIGNALING NaN into one quiets it
// (0x7F800001 -> 0x7FC00001) and the field can never be re-encoded bit-for-bit
// (MESSAGE_SPEC §4.6). The generated code must therefore drive corelib-ts's raw
// channel — Cursor.readFp32Raw / readFp32ArrayRaw on the way in, and
// OStream.writeFixlen(subtype fp32) / writeFp32ArrayRaw on the way out — at BOTH
// fp32 positions: the scalar field and the native fp32 array's elements.
//
// Measured before the fix, scalar and array alike: in 02 20 0100807f -> out
// 02 20 0100c07f. TypeScript was the last of the 13 drivers to quiet it.
func TestTSFp32SignalingNaNRawChannel(t *testing.T) {
	mod := genTSWith(t, fp32RawDef, map[string]any{})
	for _, want := range []string{
		// The raw-bits companion sits beside the value, per fp32 position, in
		// messages and in named types alike.
		"f32Fp32Raw: Uint8Array | null = null;",
		"f32dFp32Raw: Uint8Array | null = null;",
		"faFp32Raw: Uint8Array | null = null;",
		"daFp32Raw: Uint8Array | null = null;",
		"innerFp32Raw: Uint8Array | null = null;",
		"inneraFp32Raw: Uint8Array | null = null;",

		// Scalar decode: the four wire bytes, widened for the value consumer, and
		// COPIED — readFp32Raw hands back a view aliasing the decoder's buffer,
		// valid only until it is reused (readBlob's contract), and the object
		// outlives one feed. The bytes are kept only for a NaN, and the assignment
		// is unconditional so a re-opened id (§7.4) drops an earlier capture.
		`case 0: { this.o.f32 = v; this.o.f32Fp32Raw = Number.isNaN(v) ? _fp32Raw(bits) : null; break; }`,
		`case 0: { this.o.st.inner = v; this.o.st.innerFp32Raw = Number.isNaN(v) ? _fp32Raw(bits) : null; break; }`,
		// Scalar encode: the captured bytes go out verbatim. corelib-ts 0.9.0 has no
		// writeFp32Raw by design — writeFixlen with subtype fp32 emits the identical
		// fixlenHead(id, 4, Fp32) + 4 bytes.
		"if (Number.isNaN(this.f32) && this.f32Fp32Raw !== null && this.f32Fp32Raw.length === 4) {",
		"os.writeFixlen(0, this.f32Fp32Raw, FixlenSubtype.Fp32);",
		"os.writeFp32(0, this.f32);", // the number path survives for every non-NaN

		// Array decode: the companion is sized at the COUNT WORD, from a count the
		// over-count reject has already bounded, then filled element by element and
		// kept at arrayEnd only when some element was a NaN.
		`case 2: { if (kind !== ArrayKind.Fp32) break; if (count > 3) throw new SofabError(SofabErrorCode.InvalidMsg, "fa: array count above schema capacity 3"); const _d: number[] = []; this.o.fa = _d; this._a0Fa = _d; this.o.faFp32Raw = null; this._raw0Fa = new Uint8Array(count * 4); this._rawNaN0Fa = false; break; }`,
		"case 2: { this._a0Fa[i] = v; const _r = this._raw0Fa; if (_r !== null && (i + 1) * 4 <= _r.length) _fp32RawInto(_r, i * 4, bits); if (Number.isNaN(v)) this._rawNaN0Fa = true; break; }",
		"case 2: this.o.faFp32Raw = this._rawNaN0Fa ? this._raw0Fa : null; this._raw0Fa = null; break;",
		// dynamic array: no schema count, but the same companion machinery
		`case 3: { if (kind !== ArrayKind.Fp32) break; if (count > MAX_DYN_ARRAY_COUNT) throw new SofabError(SofabErrorCode.LimitExceeded, "da: array count above configured limit " + MAX_DYN_ARRAY_COUNT); const _d: number[] = []; this.o.da = _d; this._a0Da = _d; this.o.daFp32Raw = null;`,
		// Array encode: the payload is re-rendered from the value, taking captured
		// bits only for an element that is still the NaN it decoded as.
		"os.writeFp32ArrayRaw(2, _fp32ArrayRaw(this.fa, this.faFp32Raw));",
		"os.writeFp32Array(2, this.fa);", // no capture -> the plain writer, unchanged

		// Both helpers, emitted because both positions occur.
		"function _fp32FromRaw(raw: Uint8Array, off: number): number {",
		"function _fp32ArrayRaw(vals: readonly number[], raw: Uint8Array): Uint8Array {",
	} {
		if !strings.Contains(mod, want) {
			t.Errorf("fp32 raw channel missing %q:\n%s", want, mod)
		}
	}

	// The quieting readers must be gone from the fp32 paths entirely — this is the
	// defect itself, not a style point. (fp64 keeps its own readers; they are
	// spelled differently and are asserted below.)
	// The value alone must never be the only thing kept: `bits` is the 32-bit wire
	// word the hook carries, and the companion is built from it.
	if !strings.Contains(mod, "function _fp32Raw(bits: number): Uint8Array {") {
		t.Errorf("the fp32 companion must be built from the hook's wire word:\n%s", mod)
	}

	// fp64 is untouched: a JS number IS an fp64, so its NaN payload round-trips
	// through the plain readers. Widening the fix to fp64 would be pure cost.
	for _, want := range []string{"case 4: this.o.f64 = v; break;", "case 5: this._a0D64[i] = v; break;", "os.writeFp64(4, this.f64);", "os.writeFp64Array(5, this.d64);"} {
		if !strings.Contains(mod, want) {
			t.Errorf("fp64 must keep the plain number path, missing %q:\n%s", want, mod)
		}
	}
	if strings.Contains(mod, "f64Fp32Raw") || strings.Contains(mod, "d64Fp32Raw") {
		t.Errorf("fp64 must not grow an fp32 raw-bits companion:\n%s", mod)
	}
}

// TestTSFp32RawDoesNotMoveTheOmissionTest is the other half of generator#235, and
// the mistake it guards is the one that reproduces most easily: reading "the field
// carried raw bytes" as "the field was present". It is not. MESSAGE_SPEC §2 decides
// presence from the VALUE alone — emit iff it differs from its default — and a
// signaling NaN is ≠ 0 as a value anyway, so it goes out on the value test alone.
// Widening the guard to `this.f32 !== 0 || this.f32Fp32Raw !== null` re-emits an
// input that carried an explicit +0.0 instead of normalizing it away, which is a
// divergence from all 12 other drivers.
func TestTSFp32RawDoesNotMoveTheOmissionTest(t *testing.T) {
	mod := genTSWith(t, fp32RawDef, map[string]any{})
	for _, want := range []string{
		// marshal: the value test, byte for byte what it was before the raw channel.
		"    if (this.f32 !== 0) {",
		"    if (this.f32d !== 1.5) {",
		"    if (this.fa.length !== 0) {",
		// isDefault: the exact negation of the same test, likewise untouched.
		"if (!(this.f32 === 0)) return false;",
		"if (!(this.f32d === 1.5)) return false;",
		"if (!(this.fa.length === 0)) return false;",
	} {
		if !strings.Contains(mod, want) {
			t.Errorf("the §2 omission test must not move, missing %q:\n%s", want, mod)
		}
	}
	// No presence test anywhere may consult the raw slot.
	for _, gone := range []string{
		"this.f32 !== 0 || this.f32Fp32Raw",
		"this.f32Fp32Raw !== null || ",
		"if (!(this.f32 === 0 && this.f32Fp32Raw",
	} {
		if strings.Contains(mod, gone) {
			t.Errorf("presence must not be decided from the raw slot (%q):\n%s", gone, mod)
		}
	}
	// The companion is wire state, not value state: it stays out of the JSON
	// surface, so a JSON round-trip (and the generated harness) is unchanged.
	for _, gone := range []string{`"f32Fp32Raw":`, `"faFp32Raw":`, `"f32Fp32Raw" in d`} {
		if strings.Contains(mod, gone) {
			t.Errorf("the raw companion must stay out of the JSON surface (%q):\n%s", gone, mod)
		}
	}
}

// TestTSFp32RawHelpersOnlyWhereNeeded: the two helpers are module-level, so an
// unconditional emit would put dead code in every module. Each is emitted only
// where its position actually occurs.
func TestTSFp32RawHelpersOnlyWhereNeeded(t *testing.T) {
	scalarOnly := genTSWith(t, "version: 1\nmessages:\n  m:\n    payload:\n      a: { id: 0, type: fp32 }\n", map[string]any{})
	if !strings.Contains(scalarOnly, "function _fp32FromRaw(") {
		t.Errorf("an fp32 scalar needs _fp32FromRaw:\n%s", scalarOnly)
	}
	if strings.Contains(scalarOnly, "function _fp32ArrayRaw(") {
		t.Errorf("no fp32 array -> no _fp32ArrayRaw:\n%s", scalarOnly)
	}
	arrayOnly := genTSWith(t, "version: 1\nmessages:\n  m:\n    payload:\n      a: { id: 0, type: array, items: { type: fp32 } }\n", map[string]any{})
	if !strings.Contains(arrayOnly, "function _fp32ArrayRaw(") {
		t.Errorf("an fp32 array needs _fp32ArrayRaw:\n%s", arrayOnly)
	}
	// The array half needs the scalar half too: decode widens each element through
	// the shared 4-byte scratch rather than a DataView built per read (#339).
	if !strings.Contains(arrayOnly, "function _fp32FromRaw(") {
		t.Errorf("an fp32 array widens through _fp32FromRaw:\n%s", arrayOnly)
	}
	none := genTSWith(t, "version: 1\nmessages:\n  m:\n    payload:\n      a: { id: 0, type: fp64 }\n      b: { id: 1, type: array, items: { type: fp64 } }\n", map[string]any{})
	if strings.Contains(none, "_fp32") {
		t.Errorf("an fp32-free schema must not name the fp32 raw channel at all:\n%s", none)
	}
}

// MESSAGE_SPEC §7.1 + documentation#32 (issue #266, Crucible F-0033 / G-0026):
// the declared integer width is a normative VALIDITY bound. A decoded integer
// lives in a JS `number`, so nothing masked it — the defect was that it was KEPT
// — and the throw uses the same InvalidMsg channel as the maxlen/count rejects.
func TestTSDeclaredWidthIsAValidityBound(t *testing.T) {
	const src = `
version: 1
messages:
  W:
    payload:
      a_u8:   { id: 0, type: u8 }
      c_u32:  { id: 2, type: u32 }
      d_u64:  { id: 3, type: u64 }
      e_i8:   { id: 4, type: i8 }
      g_i32:  { id: 6, type: i32 }
      h_i64:  { id: 7, type: i64 }
      arr_u8: { id: 8, type: array, items: { type: u8, count: 4 } }
`
	got := genTSWith(t, src, map[string]any{})
	for _, want := range []string{
		// The value is read as the `number` a narrow destination holds, WITHOUT a
		// conversion call -- and the guard on the next line is what makes that
		// assertion true. corelib-ts hands over a bigint in exactly one case, a
		// magnitude above 2^53-1, and every narrow width tops out at 2^32-1, so
		// such a value throws before it can be stored.
		`case 0: { const _v = v as number; if (_v > 255) throw new SofabError(SofabErrorCode.InvalidMsg, "a_u8: value outside declared width u8"); this.o.a_u8 = _v; break; }`,
		`case 2: { const _v = v as number; if (_v > 4294967295) throw new SofabError(SofabErrorCode.InvalidMsg, "c_u32: value outside declared width u32"); this.o.c_u32 = _v; break; }`,
		`case 4: { const _v = v as number; if (_v < -128 || _v > 127) throw new SofabError(SofabErrorCode.InvalidMsg, "e_i8: value outside declared width i8"); this.o.e_i8 = _v; break; }`,
		`case 6: { const _v = v as number; if (_v < -2147483648 || _v > 2147483647) throw new SofabError(SofabErrorCode.InvalidMsg, "g_i32: value outside declared width i32"); this.o.g_i32 = _v; break; }`,
		// The array element bound lands on the element that carries the value, as
		// it arrives, which is what keeps INVALID ahead of a truncation right
		// behind it (#267, #339).
		`case 8: { const _e = v as number; if (_e > 255) throw new SofabError(SofabErrorCode.InvalidMsg, "arr_u8: value outside declared width u8"); this._a0ArrU8[i] = _e; break; }`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("message.ts missing width guard %q:\n%s", want, got)
		}
	}
	// 64-bit destinations store unguarded (bigint-backed under the default int64
	// mode): their range is the wire's own, so there is nothing to bound.
	for _, want := range []string{
		`case 3: this.o.d_u64 = typeof v === "bigint" ? v : BigInt(v); break;`,
		`case 7: this.o.h_i64 = typeof v === "bigint" ? v : BigInt(v); break;`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("message.ts: a 64-bit destination must store unguarded (%q):\n%s", want, got)
		}
	}
}

// An fp32 ARRAY's raw-bits companion has to be assembled on the visitor path too
// (generator#300 / #235). A JS number is a 64-bit double and widening an fp32
// SIGNALING NaN into one sets the quiet bit, so the value alone cannot round-trip
// the element -- the cursor path keeps the whole payload via readFp32ArrayRaw,
// and before this the visitor kept nothing at all: it took `arrayFp32(id, i, v)`
// without the `raw` parameter its own class enables with `fp32Raw = true`.
//
// Measured with Crucible's chunk-invariance oracle over the checked-in corpora:
// 60 mismatches across corpus/structured + corpus/regression went to 0, all of
// them fp32 NaN bit patterns quieted by the chunked path.
//
// The buffer is the WHOLE payload, not just the NaN slots. A bit-exact consumer
// reads the companion for every element once it exists (Crucible's materialized
// walk does exactly that), so a partially filled buffer reports zeros as values.
func TestTypescriptStreamFp32ArrayKeepsRawBits(t *testing.T) {
	out := genTSWith(t, `
version: 1
messages:
  vec:
    payload:
      a: { id: 0, type: array, items: { type: fp32, count: 4 } }
      b: { id: 1, type: array, items: { type: fp64, count: 4 } }
`, map[string]any{})

	// The callback must ACCEPT the wire word -- without the parameter the corelib's
	// 4th argument is silently dropped and everything below is unreachable. It is a
	// 32-bit NUMBER, not a byte view: a number costs nothing to pass, where the
	// view it replaced was an allocation per element and a borrowed slice §6.7
	// forbids.
	if !strings.Contains(out, "arrayFp32(id: number, i: number, v: number, bits: number): void") {
		t.Error("arrayFp32 must take the element's wire word")
	}
	// ...store it at the element's own offset...
	if !strings.Contains(out, "_fp32RawInto(_r, i * 4, bits)") {
		t.Error("the element's wire bytes must land at i*4 in the companion")
	}
	// ...and the companion must be decided at arrayEnd, kept only when some
	// element was a NaN -- which is what the cursor path does after its read.
	if !strings.Contains(out, "arrayEnd(id: number): void") {
		t.Error("missing arrayEnd, where the companion is decided")
	}
	if !strings.Contains(out, "this.o.aFp32Raw = this._rawNaN0A ? this._raw0A : null;") {
		t.Error("arrayEnd must keep the payload only when an element was NaN")
	}
	// A re-opened array id REPLACES (§7.4), so arrayBegin resets the companion;
	// otherwise a second occurrence inherits the first's bytes.
	if !strings.Contains(out, "this.o.aFp32Raw = null; this._raw0A = new Uint8Array(count * 4);") {
		t.Error("arrayBegin must reset the companion and size the scratch from the count")
	}
	// fp64 needs none of this -- a double holds all 64 bits verbatim -- so it must
	// not grow a companion, a scratch slot, or an arrayEnd arm.
	if strings.Contains(out, "_raw0B") || strings.Contains(out, "bFp32Raw") {
		t.Error("an fp64 array must not get an fp32 raw companion")
	}
	if strings.Contains(out, "case 1: this.o.bFp32Raw") {
		t.Error("fp64 must not appear in arrayEnd")
	}
}

// A bounded wrapper-array ELEMENT must carry its maxlen into the reader, exactly
// as a scalar string/blob already did (generator#300 / #267).
//
// readString()/readBlob() read the payload before returning, so a post-read
// length check cannot fire for an element that never fully arrives: the reader
// raises INCOMPLETE first and the verdict is lost, while §5.2 makes INVALID
// dominate because the violation is established by the length word alone.
//
// Measured with Crucible's chunk-invariance oracle over 5637 truncations of the
// checked-in corpora (every proper prefix -- a whole-message INCOMPLETE is by
// definition a truncation, which is the shape this defect needs): 3290 mismatches
// went to 122, and every one of the 3168 that disappeared was a string-array
// element over its maxlen.
func TestTypescriptWrapperElementMaxlenGoesIntoTheReader(t *testing.T) {
	out := genTSWith(t, `
version: 1
messages:
  vec:
    payload:
      sa: { id: 0, type: array, items: { type: string, count: 4, maxlen: 6 } }
      ba: { id: 1, type: array, items: { type: blob, count: 4, maxlen: 5 } }
      s:  { id: 2, type: string, maxlen: 8 }
`, map[string]any{})

	// The element collectors must carry the bound, so the verdict is taken at the
	// element's LENGTH WORD rather than after its payload.
	if !strings.Contains(out, `this._q1 = new StringSeq(_t, this.a, 4, 6, "sa", 16384, 262144);`) {
		t.Error("a bounded string element must pass its maxlen into StringSeq")
	}
	if !strings.Contains(out, `this._q2 = new BlobSeq(_t, this.a, 4, 5, "ba", 16384, 1048576);`) {
		t.Error("a bounded blob element must pass its maxlen into BlobSeq")
	}
	// ...and the element's length word must actually reach the collector, not only
	// its payload: the whole point is a verdict that survives a truncation inside
	// the element.
	if !strings.Contains(out, "this._q1?.begin(id, sub, total);") {
		t.Error("the element's length word must reach the collector")
	}
	// ...and the scalar arm keeps its own bound, at the same word.
	if !strings.Contains(out, `case 2: if (sub === FixlenSubtype.String && total > 8) throw new SofabError(SofabErrorCode.InvalidMsg, "s: string byte length above schema maxlen 8"); break;`) {
		t.Error("the scalar string bound must still be taken at the length word")
	}
	// The collector's bound is the whole bound: nothing may re-walk a decoded
	// string on the hot path to reach a verdict that was already taken, and taken
	// earlier.
	if strings.Contains(out, "_utf8Len") {
		t.Error("the string element must not re-scan what the collector already bounded")
	}
}

// TestTSCallerOwnsTheEncodeBuffer: the output buffer belongs to the caller, and
// generated code IS that caller — it allocates the storage and hands it to the
// corelib, which allocates and grows nothing (CORELIB_PLAN §5.1).
//
// corelib-ts's no-argument `new OStream()` is the shape that breaks this: it is
// deprecated as an alias for growingOStream(), which allocates a slab and doubles
// it as the message grows. Nothing this backend emits may use it.
//
// Which of the two conformant shapes applies is a property of the SCHEMA, so both
// arms are asserted from the same test: a fully bounded message gets one
// exactly-sized buffer, and an unbounded one gets a fixed scratch draining into a
// caller-owned list — sizing a buffer from the CEILING would silently refuse a
// larger message the caller legitimately built.
func TestTSCallerOwnsTheEncodeBuffer(t *testing.T) {
	bounded := genTSWith(t, "version: 1\nmessages:\n  M:\n    payload:\n"+
		"      a: { id: 0, type: u32 }\n"+
		"      s: { id: 1, type: string, maxlen: 4 }\n", map[string]any{})
	for _, want := range []string{
		"  static readonly MAX_SIZE = 12;",
		"    const _buf = new Uint8Array(M.MAX_SIZE);",
		"    const _os = new OStream(_buf);",
		"    return _buf.slice(0, _os.bytesUsed);",
	} {
		if !strings.Contains(bounded, want) {
			t.Errorf("a bounded message must encode through one exactly-sized caller buffer: missing %q\n%s", want, bounded)
		}
	}
	// The derived size must not be dressed up as a ceiling: MAX_SIZE_LIMIT is how a
	// reader tells an IMPOSED number from one the schema supplies.
	if strings.Contains(bounded, "MAX_SIZE_LIMIT") {
		t.Errorf("a bounded message must emit the derived MAX_SIZE alone, not a ceiling:\n%s", bounded)
	}

	unbounded := genTSWith(t, "version: 1\nmessages:\n  M:\n    payload:\n"+
		"      s: { id: 0, type: string }\n", map[string]any{"max_message_size": 2048})
	for _, want := range []string{
		"  static readonly MAX_SIZE_LIMIT = 2048;",
		"  static readonly MAX_SIZE = M.MAX_SIZE_LIMIT;",
		// The sink is handed the INSTALLED buffer plus the region's coordinates,
		// never a view the encoder built: a subarray would be an allocation per
		// flush, and the encoder allocates nothing after construction (§6.6/§5.1.6).
		"    const _os = new OStream(new Uint8Array(512), 0, (_b, _s, _e) => { const _k = _b.slice(_s, _e); _out.push(_k); _n += _k.length; });",
		"    _os.flush();",
	} {
		if !strings.Contains(unbounded, want) {
			t.Errorf("an unbounded message must drain a fixed scratch into caller storage: missing %q\n%s", want, unbounded)
		}
	}
	// The ceiling may never size the buffer: a message above it is legal.
	if strings.Contains(unbounded, "new Uint8Array(M.MAX_SIZE)") || strings.Contains(unbounded, "new Uint8Array(2048)") {
		t.Errorf("the configured ceiling must not size an encode buffer:\n%s", unbounded)
	}

	// The corelib-allocating form must appear nowhere, in any emitted file.
	files, err := (&Backend{}).Generate(schema(t, "version: 1\nmessages:\n  M:\n    payload:\n"+
		"      s: { id: 0, type: string }\n"), map[string]any{"emit": "project"})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	for _, f := range files {
		if strings.Contains(string(f.Content), "new OStream()") {
			t.Errorf("%s constructs the corelib-allocating OStream():\n%s", f.Path, f.Content)
		}
	}
}

// TestTSStructsGetNoEncodeEntryPoint: a struct/union serializes a headerless
// field RUN, not a message. Bytes handed back from an encode() on one would not
// be a message any decoder could read on its own, so only a message gets the
// entry point and the size constant that sizes its buffer.
func TestTSStructsGetNoEncodeEntryPoint(t *testing.T) {
	mod := genTSWith(t, "version: 1\nmessages:\n  M:\n    payload:\n"+
		"      p: { id: 0, type: struct, fields: { x: { id: 0, type: i32 } } }\n", map[string]any{})
	cls := mod[strings.Index(mod, "export class MP {"):strings.Index(mod, "export class M {")]
	if strings.Contains(cls, "encode(): Uint8Array") || strings.Contains(cls, "MAX_SIZE") {
		t.Errorf("a struct must not carry a message encode entry point:\n%s", cls)
	}
	if !strings.Contains(mod, "export class M {") || !strings.Contains(mod[strings.Index(mod, "export class M {"):], "encode(): Uint8Array") {
		t.Errorf("the message must carry one:\n%s", mod)
	}
}

// TestTSDecodedBlobOwnsItsBytes: the same ownership rule read on the decode side.
// A blob payload reaches the visitor as a range of the CALLER's own fed chunk
// (§6.6.3): the corelib builds no view over it and holds no storage of its own,
// so a destination that kept the range would make the message's lifetime the
// caller's buffer's, silently — and §6.7 forbids the decoder handing out a
// borrowed value in the first place.
//
// PayloadAcc is the way across: what `take` hands back is a buffer it allocated
// and gave away, so the destination owns its bytes at every position, bounded or
// not, and the caller may reuse the chunk the moment `feed` returns.
func TestTSDecodedBlobOwnsItsBytes(t *testing.T) {
	mod := genTSWith(t, "version: 1\nmessages:\n  M:\n    payload:\n"+
		"      bb: { id: 0, type: blob, maxlen: 8 }\n"+
		"      bu: { id: 1, type: blob }\n"+
		"      ab: { id: 2, type: array, items: { type: blob, count: 3, maxlen: 4 } }\n"+
		"      au: { id: 3, type: array, items: { type: blob } }\n", map[string]any{})
	for _, want := range []string{
		// bounded + unbounded scalar: the accumulator's own buffer, taken whole
		"const _p = this.a.take(total, offset, src, start, end); if (_p !== null) this.o.bb = _p;",
		"const _p = this.a.take(total, offset, src, start, end); if (_p !== null) this.o.bu = _p;",
		// bounded + unbounded wrapper element: the corelib's BlobSeq, which stores
		// what the same accumulator hands it, never a range of the chunk
		`this._q1 = new BlobSeq(_t, this.a, 3, 4, "ab", MAX_DYN_ARRAY_COUNT, MAX_DYN_BLOB_LEN);`,
		`this._q2 = new BlobSeq(_t, this.a, -1, -1, "au", MAX_DYN_ARRAY_COUNT, MAX_DYN_BLOB_LEN);`,
		"this._q1?.element(id, total, offset, src, start, end);",
	} {
		if !strings.Contains(mod, want) {
			t.Errorf("a decoded blob destination must own its bytes: missing %q\n%s", want, mod)
		}
	}
	// ...and nothing may keep a range of the fed chunk (§6.7).
	for _, bad := range []string{"= src;", "= src.subarray(", "src.slice(start, end)"} {
		if strings.Contains(mod, bad) {
			t.Errorf("a decoded blob destination still aliases the fed chunk: %q", bad)
		}
	}
}

// MESSAGE_SPEC §7.1 one level down: a NESTED row's element carries the same
// declared width as a flat array's, and the same verdict is owed for it.
//
// #330 closed this for go and dart and recorded TypeScript as already guarding
// — true of the pull path, which hands the bound to `readUnsignedArray`, and
// false of the push path, where `_MatSeq` stored whatever arrived. The two
// surfaces of one generated module therefore disagreed: `array<array<u32>>`
// carrying 2^32 was INVALID through `Cursor` and COMPLETE through the visitor,
// which left a `number[][]` holding a value one past its declared type.
func TestTSNestedRowElementWidthIsGuarded(t *testing.T) {
	const src = `
version: 1
messages:
  M:
    payload:
      urows: { id: 0, type: array, items: { type: array, count: 2, items: { type: u8, count: 3 } } }
      irows: { id: 1, type: array, items: { type: array, count: 2, items: { type: i8, count: 3 } } }
      wrows: { id: 2, type: array, items: { type: array, count: 2, items: { type: u64, count: 3 } } }
`
	got := genTSWith(t, src, map[string]any{})
	for _, want := range []string{
		// The guard fires on the element that carries the value, as it arrives —
		// not on the finished row. A scan of the row could not reject an element a
		// truncation stops the row from ever completing (§5.2.3).
		"        const _e = v as number; if (_e > 255) throw new SofabError(SofabErrorCode.InvalidMsg, \"urows element: value outside declared width u8\");\n" +
			"        this._row1[i] = _e;",
		"    const _e = v as number; if (_e < -128 || _e > 127) throw new SofabError(SofabErrorCode.InvalidMsg, \"irows element: value outside declared width i8\");\n" +
			"    this._row2[i] = _e;",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("message.ts missing nested-row width guard %q:\n%s", want, got)
		}
	}
	// u64 has nothing narrower than the wire to check, so the store stays a plain
	// conversion — a guard here would be dead code on every element.
	if !strings.Contains(got, `this._row3[i] = typeof v === "bigint" ? v : BigInt(v);`) {
		t.Errorf("a u64 row must keep the bare conversion:\n%s", got)
	}
}

// TestTSClosedNameSet pins CORELIB_PLAN §6.1.1 on the generated class surface
// (issue #384, Crucible G-0040). The set of names a generated object may carry
// for the wire operations is CLOSED — encode, decode, try_decode, serialize,
// deserialize, decoder — and the clause names `decode_from` and `decode_into`
// among the spellings a port must not invent beside them. Only the casing/idiom
// may be adapted, so `decodeFrom` is not a different name from `decode_from`: it
// is that name spelled in TypeScript, and a generated class carrying it makes a
// developer learn a per-language second entry point into one operation.
//
// The cursor-level steps still exist — §7.4 needs the loop separable from the
// fresh-object entry — but as MODULE-LEVEL functions of the generated file:
// reachable from the sibling classes that decode into one another (that is why a
// `#private` static cannot do the job), exported to nobody.
func TestTSClosedNameSet(t *testing.T) {
	// example.yaml reaches every class shape the backend emits: message, nested
	// struct, union, and the per-element wrapper classes.
	mod := genTS(t)
	for _, gone := range []string{
		"static decodeFrom", "static decodeInto", // on the class itself
		".decodeFrom(", ".decodeInto(", // and as a call on any generated type
		"export function _decodeFrom", "export function _decodeInto", // module-level, but exported
	} {
		if strings.Contains(mod, gone) {
			t.Errorf("§6.1.1 closes the generated object's name set: message.ts must not emit %q", gone)
		}
	}
	// What replaces them: one public entry, running the corelib's one-shot decode
	// against this type's flat visitor. The visitor is a module internal -- not
	// exported, and not a member of the class -- so it adds no name to the closed
	// set §6.1.1 defines.
	for _, want := range []string{
		// The example schema HAS an unbounded string element inside a wrapper array
		// (somestringarray), the one shape the visitor never sees a header for, so
		// the residual DecodeLimits survives here (generator#388).
		"  static decode(bytes: Uint8Array): Myfirstmessage {\n    const o = new Myfirstmessage();\n    _decode(bytes, new _MyfirstmessageVis(o, new PayloadAcc()));\n    return o;\n  }",
		"class _MyfirstmessageVis implements Visitor {",
	} {
		if !strings.Contains(mod, want) {
			t.Errorf("message.ts missing %q", want)
		}
	}
	// `decode` is the only decode-side name the CLASS carries. Any other static
	// on a generated class must come from the language-mandated-extra escape
	// hatch, not from a second spelling of a wire operation.
	statics := regexp.MustCompile(`(?m)^  static (?:readonly )?(\w+)`).FindAllStringSubmatch(mod, -1)
	if len(statics) == 0 {
		t.Fatal("no statics found — the scan is not seeing the class bodies")
	}
	allowed := map[string]bool{
		"decode":         true, // §6.1.1
		"fromJSON":       true, // JSON bridge, not a wire entry point
		"MAX_SIZE":       true, // the schema's worst-case size constant, not an entry point
		"MAX_SIZE_LIMIT": true, // its unbounded-schema companion, likewise a constant
	}
	for _, m := range statics {
		if !allowed[m[1]] {
			t.Errorf("generated class carries static %q; §6.1.1 closes the set (add it to the allowlist only if it is a language-mandated extra, never a second wire entry point)", m[1])
		}
	}
}

// TestTSCapsAreEnforcedPerField: the receiver caps are applied by the generated
// visitor, at each field's own count/length header, rather than handed to the
// corelib as a DecodeLimits (ARCHITECTURE §9.5, generator#388).
//
// The corelib has no schema. A cap it is given applies to every field it sees, so
// it cannot honour §6.2.1's "MUST NOT be applied to a field the schema already
// bounds" -- which is why the emitted value had to be RAISED to the largest schema
// bound in the message, and why every unbounded field lost that much tightness.
// The visitor knows the schema, so it needs neither.
func TestTSCapsAreEnforcedPerField(t *testing.T) {
	const src = `
version: 1
messages:
  M:
    payload:
      s:    { id: 0, type: string }
      bs:   { id: 1, type: string, maxlen: 8 }
      b:    { id: 2, type: blob }
      arr:  { id: 3, type: array, items: { type: u32 } }
      barr: { id: 4, type: array, items: { type: u32, count: 100000 } }
      mat:  { id: 5, type: array, items: { type: array, items: { type: u32 } } }
`
	mod := genTSWith(t, src, map[string]any{"max_dyn_array_count": 64, "max_dyn_string_len": 32})

	for _, want := range []string{
		// Emitted as configured. 64 is far below barr's schema count of 100000 --
		// under the old global DecodeLimits that combination was impossible, because
		// the corelib would have rejected barr at 64 and the value had to be lifted.
		"export const MAX_DYN_ARRAY_COUNT = 64;",
		"export const MAX_DYN_STRING_LEN = 32;",
		// Unbounded scalar string/blob: the cap, at the length word.
		`case 0: if (sub === FixlenSubtype.String && total > MAX_DYN_STRING_LEN) throw new SofabError(SofabErrorCode.LimitExceeded, "s: string byte length above configured limit " + MAX_DYN_STRING_LEN); break;`,
		// Schema-bounded string: its own bound, its own category, no cap.
		`case 1: if (sub === FixlenSubtype.String && total > 8) throw new SofabError(SofabErrorCode.InvalidMsg, "bs: string byte length above schema maxlen 8"); break;`,
		// Unbounded native array: the cap, at the count word, behind the §7.3 kind test.
		`case 3: { if (kind !== ArrayKind.Unsigned) break; if (count > MAX_DYN_ARRAY_COUNT) throw new SofabError(SofabErrorCode.LimitExceeded, "arr: array count above configured limit " + MAX_DYN_ARRAY_COUNT);`,
		// Schema-bounded native array: its own bound at 100000, NOT the cap at 64.
		`case 4: { if (kind !== ArrayKind.Unsigned) break; if (count > 100000) throw new SofabError(SofabErrorCode.InvalidMsg, "barr: array count above schema capacity 100000");`,
		// A matrix ROW's own element count takes the cap the same way.
		`if (count > MAX_DYN_ARRAY_COUNT) throw new SofabError(SofabErrorCode.LimitExceeded, "mat element: array count above configured limit " + MAX_DYN_ARRAY_COUNT);`,
	} {
		if !strings.Contains(mod, want) {
			t.Errorf("message.ts missing per-field cap %q:\n%s", want, mod)
		}
	}
	// Nothing in this schema is beyond the visitor's reach, so the corelib is
	// handed no cap at all and there is no DecodeLimits object to build.
	for _, gone := range []string{"_LIMITS", "maxArrayCount", "maxStringLen", "maxBlobLen"} {
		if strings.Contains(mod, gone) {
			t.Errorf("the corelib must take no cap for a fully covered schema (%q):\n%s", gone, mod)
		}
	}
	// The blob cap stays inert on liveness, not on enforcement: `b` is unbounded,
	// so MAX_DYN_BLOB_LEN IS live here and is applied at id 2.
	if !strings.Contains(mod, `case 2: if (sub === FixlenSubtype.Blob && total > MAX_DYN_BLOB_LEN) throw new SofabError(SofabErrorCode.LimitExceeded, "b: blob byte length above configured limit " + MAX_DYN_BLOB_LEN); break;`) {
		t.Errorf("an unbounded blob must be capped at its length word:\n%s", mod)
	}
}

// TestTSWrapperElementCapsRideOnTheCollector: a wrapper array's elements never
// reach the generated visitor -- neither their index nor their length word -- so
// BOTH of their receiver caps are arguments to the corelib collector, and the
// module hands the corelib no DecodeLimits at all.
//
// This replaces the residual DecodeLimits that used to carry the element LENGTH
// cap. That object had to be RAISED past every schema `maxlen` in the module,
// because corelib-ts measured every fixlen length against it with no schema
// exemption and at the length word -- before the visitor's fixlenBegin -- so a
// tight cap rejected a schema-bounded field on the way in. The collector's
// `receiverElemMax` (corelib-ts#164) is per field and exclusive with the `elemMax`
// beside it, so the cap travels tight and the raise is gone with the object.
func TestTSWrapperElementCapsRideOnTheCollector(t *testing.T) {
	const src = `
version: 1
messages:
  M:
    payload:
      s:    { id: 0, type: string }
      wdyn: { id: 1, type: array, items: { type: string } }
      wbnd: { id: 2, type: array, items: { type: string, maxlen: 4096 } }
`
	mod := genTSWith(t, src, map[string]any{"max_dyn_string_len": 1024})

	// As configured, and read by every guard alike -- the scalar's, and the
	// collector's. 1024 coexists with wbnd's `maxlen: 4096`, which the raised
	// residual could not: it had to carry 4096 or reject that field on the way in.
	if !strings.Contains(mod, "export const MAX_DYN_STRING_LEN = 1024;") {
		t.Errorf("the exported cap must stay as configured:\n%s", mod)
	}
	for _, want := range []string{
		// The unbounded element: schema bounds -1/-1, then both receiver caps.
		`this._q1 = new StringSeq(_t, this.a, -1, -1, "wdyn", MAX_DYN_ARRAY_COUNT, MAX_DYN_STRING_LEN);`,
		// The bounded one: its own maxlen governs and the cap beside it is inert,
		// but it is passed all the same -- an omitted argument is the format
		// ceiling, i.e. no receiver bound, never "the corelib's default".
		`this._q2 = new StringSeq(_t, this.a, -1, 4096, "wbnd", MAX_DYN_ARRAY_COUNT, MAX_DYN_STRING_LEN);`,
		// The scalar keeps its own guard, at the same word, on the same constant.
		`case 0: if (sub === FixlenSubtype.String && total > MAX_DYN_STRING_LEN) throw new SofabError(SofabErrorCode.LimitExceeded, "s: string byte length above configured limit " + MAX_DYN_STRING_LEN); break;`,
	} {
		if !strings.Contains(mod, want) {
			t.Errorf("message.ts missing %q:\n%s", want, mod)
		}
	}
	// Nothing is left for a decoder-level cap: no object, and no argument for it.
	for _, gone := range []string{"_LIMITS", "maxStringLen", "maxBlobLen", "maxArrayCount"} {
		if strings.Contains(mod, gone) {
			t.Errorf("the corelib must be handed no DecodeLimits (%q):\n%s", gone, mod)
		}
	}
	if !strings.Contains(mod, "_decode(bytes, new _MVis(o, new PayloadAcc()));") {
		t.Errorf("decode() must pass the bytes and the visitor, nothing else:\n%s", mod)
	}
	if !strings.Contains(mod, "this.is = new IStream(new _MVis(this.out, new PayloadAcc()));") {
		t.Errorf("the streaming decoder must pass the visitor, nothing else:\n%s", mod)
	}
}

// TestTSCollectorCapsAreNeverOmitted: where the schema bounds every field of a
// kind, its exported constant is not emitted -- the cap is inert and would be dead
// code -- but the collector's argument is still filled, with the target's own
// resolved default as a literal.
//
// The argument cannot simply be dropped. corelib-ts falls back to the FORMAT
// CEILING for an omitted receiver cap (ARRAY_MAX / FIXLEN_MAX, corelib-ts#165), so
// leaving one out is not "the corelib's default" but no receiver bound at all --
// and §6.2.1 puts the number in generated code either way.
func TestTSCollectorCapsAreNeverOmitted(t *testing.T) {
	mod := genTSWith(t, `
version: 1
messages:
  M:
    payload:
      w: { id: 0, type: array, items: { type: string, count: 4, maxlen: 8 } }
      b: { id: 1, type: array, items: { type: blob, count: 2, maxlen: 9 } }
`, map[string]any{})

	for _, want := range []string{
		`this._q1 = new StringSeq(_t, this.a, 4, 8, "w", 16384, 262144);`,
		`this._q2 = new BlobSeq(_t, this.a, 2, 9, "b", 16384, 1048576);`,
	} {
		if !strings.Contains(mod, want) {
			t.Errorf("message.ts missing %q:\n%s", want, mod)
		}
	}
	// Inert, so no constant: nothing in this schema can reach one.
	for _, gone := range []string{"MAX_DYN_ARRAY_COUNT", "MAX_DYN_STRING_LEN", "MAX_DYN_BLOB_LEN"} {
		if strings.Contains(mod, gone) {
			t.Errorf("a schema that bounds everything must export no cap (%q):\n%s", gone, mod)
		}
	}
}

// wideBitfieldDef puts a bitfield whose highest flag sits at 63 beside one whose
// flags all fit below the 32-bit sign bit, as scalars and as array elements, and
// a third sitting exactly ON the boundary at position 31.
const wideBitfieldDef = `
version: 1
$defs:
  bitfield:
    Wide:   { low: { pos: 0 }, high: { pos: 63, default: true } }
    Edge:   { a: { pos: 30 }, b: { pos: 31 } }
    Narrow: { a: { pos: 0 }, b: { pos: 30 } }
messages:
  m:
    payload:
      w:  { id: 0, type: bitfield, bits: { $ref: "#/$defs/bitfield/Wide" } }
      n:  { id: 1, type: bitfield, bits: { $ref: "#/$defs/bitfield/Narrow" } }
      wa: { id: 2, type: array, items: { type: bitfield, count: 2, bits: { $ref: "#/$defs/bitfield/Wide" } } }
      na: { id: 3, type: array, items: { type: bitfield, count: 2, bits: { $ref: "#/$defs/bitfield/Narrow" } } }
      e:  { id: 4, type: bitfield, bits: { $ref: "#/$defs/bitfield/Edge" } }
`

// TestTSWideBitfieldIsBigint: a bitfield with a flag at position 31 or above is
// carried as a `bigint`, not a `number`.
//
// A `number` is a double. It holds a mask exactly only to bit 52, and the size
// walk charges EVERY bitfield the full ten-byte varint (internal/ir/wiresize.go),
// so a `number`-carried 64-bit bitfield could not reach its own MAX_SIZE: an
// all-flags-set value arrived rounded and the encode was refused outright
// (generator#470).
//
// The boundary sits at 31, not 32, and `e` is here to pin it. Storage is not the
// only property a mask needs. JavaScript narrows both operands of `|` and `&` to
// 32-bit SIGNED, so a mask with bit 31 set comes back NEGATIVE -- `Edge.A |
// Edge.B` as numbers is -1073741824, which corelib-ts refuses as an unsigned
// value out of 64-bit range. Position 31 IS storable in a double and is NOT
// combinable, and combinable is the property the carrier choice turns on.
// Positions 0..30 have neither problem (`|` over them tops out at 0x7FFFFFFF) and
// keep the numeric enum they have always had -- this is the same "smallest type
// that holds the highest flag position" choice C, C++, C#, Rust and Zig already
// make, with the two carriers TypeScript has.
func TestTSWideBitfieldIsBigint(t *testing.T) {
	mod := genTSWith(t, wideBitfieldDef, map[string]any{})
	for _, want := range []string{
		// A TS enum member can only be a number, so the wide masks become a
		// literal-typed `const` object; the narrow ones stay an enum.
		"export const BitfieldWide = {",
		"  High: 9223372036854775808n,",
		"export enum BitfieldNarrow {",
		"  B = 1073741824,",
		// The boundary itself: a highest flag at 31 is bigint-carried too.
		"export const BitfieldEdge = {",
		"  A: 1073741824n,",
		"  B: 2147483648n,",
		"e: bigint = 0n;",
		"case 4: this.o.e = BigInt(v); break;",
		// Storage, default and the default comparison that reads it.
		"w: bigint = 9223372036854775808n;",
		"n: number = 0;",
		"wa: bigint[] = [];",
		"na: number[] = [];",
		"if (!(this.w === 9223372036854775808n)) return false;",
		// JSON: a bigint is not JSON-able, so it prints as a decimal string and
		// reads back through BigInt() -- exactly what u64 does.
		`"w": this.w.toString(),`,
		`"n": this.n,`,
		`if ("w" in d) o.w = BigInt(d["w"] as string | number);`,
		`if ("n" in d) o.n = d["n"] as number;`,
		`o.wa = (d["wa"] as (string | number)[]).map((_x0) => BigInt(_x0));`,
		`o.na = d["na"] as number[];`,
		// Decode: the unsigned callback delivers a number below 2^53 and a bigint
		// above, so the wide store normalises instead of rounding through Number().
		"case 0: this.o.w = BigInt(v); break;",
		"case 1: this.o.n = Number(v); break;",
	} {
		if !strings.Contains(mod, want) {
			t.Errorf("missing %q", want)
		}
	}
	for _, bad := range []string{
		"w: number =",
		"case 0: this.o.w = Number(v); break;",
		"export enum BitfieldWide {",
		// A mask with bit 31 set is not `|`-combinable as a number.
		"e: number =",
		"export enum BitfieldEdge {",
	} {
		if strings.Contains(mod, bad) {
			t.Errorf("a 64-bit-backed bitfield must not be carried as a number: found %q", bad)
		}
	}
}
