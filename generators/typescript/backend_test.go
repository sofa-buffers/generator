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

func TestTSStructural(t *testing.T) {
	mod := genTS(t)
	for _, want := range []string{
		`import { OStream, Cursor, SofabError, SofabErrorCode } from "@sofa-buffers/corelib";`, // over-count reject needs the error type (generator#100)
		"export class Myfirstmessage {",
		"marshal(os: OStream): void {",
		"static decode(bytes: Uint8Array): Myfirstmessage {",
		"return Myfirstmessage.decodeFrom(new Cursor(bytes));",
		"static decodeFrom(c: Cursor): Myfirstmessage {",
		"while (c.readHeader()) {",                                      // monomorphic pull loop
		"switch (c.id) {",                                               // one switch per type
		"default: c.skip(c.wire); break;",                               // forward-compat skip
		"o.somestruct = MyfirstmessageSomestruct.decodeFrom(c); break;", // nested message recursion
		`while (c.readHeader()) { const _id = c.id; while (arr.length <= _id) arr.push(""); arr[_id] = c.readString(); }`, // id-aware string-list sequence (MESSAGE_SPEC S2)
		"o.someu64 = c.readUnsigned() as bigint; break;",                                                                  // u64 -> bigint, number-first
		"os.writeSequenceBegin(", // nested framing (marshal unchanged)
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
	} {
		if strings.Contains(mod, gone) {
			t.Errorf("message.ts should no longer emit %q (push/visitor decode removed)", gone)
		}
	}
	// Fast-encode marshal tidy-up: a leaf string list uses an indexed for (no
	// per-encode closure) rather than .forEach.
	if !strings.Contains(mod, "for (let _i0 = 0; _i0 < this.somestringarray.length; _i0++) {") {
		t.Error("message.ts missing indexed-for string-list marshal (fast-encode)")
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
		`import { OStream, Cursor, Long, SofabError, SofabErrorCode } from "@sofa-buffers/corelib";`,
		// Long[] backing field + accessor pair; setter converts once. us is
		// `count: 8`, so its implied default is 8 Long zeros (issue#136).
		"private _us: Long[] = [Long.fromValue(0n), Long.fromValue(0n), Long.fromValue(0n), Long.fromValue(0n), Long.fromValue(0n), Long.fromValue(0n), Long.fromValue(0n), Long.fromValue(0n)];",
		"get us(): Long[] { return this._us; }",
		"set us(vals: readonly (Long | bigint | number)[]) { this._us = vals.map(Long.fromValue); }",
		// Nested array: Long[][] with a per-row setter conversion.
		"private _rows: Long[][] = [];",
		"set rows(vals: readonly (readonly (Long | bigint | number)[])[]) { this._rows = vals.map((_v0) => _v0.map(Long.fromValue)); }",
		// Marshal reads the backing field; 64-bit arrays use the Long writers.
		// These are `count: N` fields, so the trailing default run is trimmed
		// (issue#136) by the Long flavour of the trim (word-pair compare). The
		// omission guard compares against the implied N-element default.
		"if (!longArrEq(this._us, [Long.fromValue(0n), Long.fromValue(0n), Long.fromValue(0n), Long.fromValue(0n), Long.fromValue(0n), Long.fromValue(0n), Long.fromValue(0n), Long.fromValue(0n)])) {",
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
		`case 0: { const _a = c.readUnsignedArrayLong(); if (_a.length > 8) throw new SofabError(SofabErrorCode.InvalidMsg, "us: array count above schema capacity 8"); o._us = _padTo(_a, 8, Long.fromValue(0)); break; }`,
		`case 1: { const _a = c.readSignedArrayLong(); if (_a.length > 8) throw new SofabError(SofabErrorCode.InvalidMsg, "is: array count above schema capacity 8"); o._is = _padTo(_a, 8, Long.fromValue(0)); break; }`,
		// toJSON prints via Long.toString with the schema signedness.
		`"us": this._us.map((_x0) => _x0.toString(false)),`,
		`"is": this._is.map((_x0) => _x0.toString(true)),`,
		// fromJSON keeps the bigint parse and lets the setter convert once.
		`if ("us" in d) o.us = (d["us"] as (string | number)[]).map((_x0) => BigInt(_x0));`,
		// Scalars stay bigint in long mode (no scalar Long codec in corelib yet).
		"u: bigint = 0n;",
		"i: bigint = -7n;",
		"case 4: o.u = c.readUnsigned() as bigint; break;",
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
		`case 0: { const _a = c.readUnsignedArrayLong(); if (_a.length > 8) throw new SofabError(SofabErrorCode.InvalidMsg, "us: array count above schema capacity 8"); o._us = _padTo(_a, 8, Long.fromValue(0)); break; }`,
		// Scalars are plain numbers: number default, !== 0 guard, Number() decode.
		"u: number = 0;",
		"i: number = -7;",
		"if (this.u !== 0) {",
		"os.writeUnsigned(4, this.u);",
		"case 4: o.u = Number(c.readUnsigned()); break;",
		"case 5: o.i = Number(c.readSigned()); break;",
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
		`case 0: { const _a = c.readUnsignedArray() as number[]; if (_a.length > 5) throw new SofabError(SofabErrorCode.InvalidMsg, "fu32: array count above schema capacity 5"); o.fu32 = _padTo(_a, 5, 0); break; }`,
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
	} {
		if !strings.Contains(mod, want) {
			t.Errorf("fixed-default message.ts missing %q", want)
		}
	}
	for _, gone := range []string{
		// Dynamic arrays are NOT fixed-length: no synthesized default, no padding.
		"dyn: number[] = [0",
		"dynd: number[] = [1, 2, 0",
		// A string-element array is a wrapper sequence: still starts empty.
		"strs: string[] = [\"\"",
	} {
		if strings.Contains(mod, gone) {
			t.Errorf("fixed-default message.ts should not emit %q", gone)
		}
	}
	for _, want := range []string{
		"dyn: number[] = [];",      // no default, dynamic -> empty
		"dynd: number[] = [1, 2];", // dynamic default kept verbatim (not padded)
		"strs: string[] = [];",
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
			"private _fu64: Long[] = [Long.fromValue(1n), Long.fromValue(0n), Long.fromValue(0n)];",
			"if (!longArrEq(this._fu64, [Long.fromValue(1n), Long.fromValue(0n), Long.fromValue(0n)])) {",
		} {
			if !strings.Contains(mod, want) {
				t.Errorf("int64: %s fixed-default message.ts missing %q", mode, want)
			}
		}
	}
}

// TestTSNoFixedCountNoHelpers: a schema without any fixed-count native array
// must not carry the trim/pad helpers (they would be dead code).
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
			`import { OStream, Cursor, SofabError, SofabErrorCode } from "@sofa-buffers/corelib";`,
			// count: 8 with no schema default -> 8 element defaults (issue#136).
			"us: bigint[] = [0n, 0n, 0n, 0n, 0n, 0n, 0n, 0n];",
			// count: 8 -> the trailing default run is trimmed on encode and
			// refilled on decode; the bigint element default is 0n (issue#136).
			"os.writeUnsignedArray(0, _trimTail(this.us, 0n));",
			`case 0: { const _a = c.readUnsignedArray() as bigint[]; if (_a.length > 8) throw new SofabError(SofabErrorCode.InvalidMsg, "us: array count above schema capacity 8"); o.us = _padTo(_a, 8, 0n); break; }`,
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

func TestTSMapField(t *testing.T) {
	src := `
version: 1
messages:
  M:
    payload:
      counts: { type: map, id: 1, key: { type: string, maxlen: 32 }, value: { type: u32 }, count: 128 }
      nested:
        type: map
        id: 2
        key: { type: u32 }
        value: { type: map, key: { type: u32 }, value: { type: u8 } }
`
	m := genTSWith(t, src, map[string]any{})
	for _, want := range []string{
		"counts: Map<string, number> = new Map();",           // surface container
		"nested: Map<number, Map<number, number>> = new Map();", // nested map value
		"Array.from(this.counts.keys()).sort(",               // canonical-order encode
		"const _e = new MCountsEntry();",                     // entry-class reuse on marshal
		"const _m: Map<string, number> = new Map();",         // decode collector
		"_m.set(_e.key, _e.value);",                          // build map on decode
		"MCountsEntry.decodeFrom(c)",                         // entry decode via decodeFrom
	} {
		if !strings.Contains(m, want) {
			t.Errorf("message.ts missing %q", want)
		}
	}
}
