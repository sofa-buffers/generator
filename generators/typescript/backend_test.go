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
		`import { OStream, Cursor } from "@sofa-buffers/corelib";`,
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
		`import { OStream, Cursor, Long } from "@sofa-buffers/corelib";`,
		// Long[] backing field + accessor pair; setter converts once.
		"private _us: Long[] = [];",
		"get us(): Long[] { return this._us; }",
		"set us(vals: readonly (Long | bigint | number)[]) { this._us = vals.map(Long.fromValue); }",
		// Nested array: Long[][] with a per-row setter conversion.
		"private _rows: Long[][] = [];",
		"set rows(vals: readonly (readonly (Long | bigint | number)[])[]) { this._rows = vals.map((_v0) => _v0.map(Long.fromValue)); }",
		// Marshal reads the backing field; 64-bit arrays use the Long writers.
		"if (this._us.length !== 0) {",
		"os.writeUnsignedArrayLong(0, this._us);",
		"os.writeSignedArrayLong(1, this._is);",
		// Defaulted Long array: materialized Long default + longArrEq guard.
		`private _ud: Long[] = [Long.fromValue(1n), Long.fromValue(18446744073709551615n)];`,
		"if (!longArrEq(this._ud, [Long.fromValue(1n), Long.fromValue(18446744073709551615n)])) {",
		"function longArrEq(a: readonly Long[], b: readonly Long[]): boolean {",
		// Decode bypasses the setter (readers return canonical Long[]).
		"case 0: o._us = c.readUnsignedArrayLong(); break;",
		"case 1: o._is = c.readSignedArrayLong(); break;",
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
		"os.writeUnsignedArrayLong(0, this._us);",
		"case 0: o._us = c.readUnsignedArrayLong(); break;",
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

// TestTSInt64Default locks the default (and explicit bigint) mode to the
// bigint-everywhere shapes: no Long import, no accessor pairs.
func TestTSInt64Default(t *testing.T) {
	for _, cfg := range []map[string]any{{}, {"int64": "bigint"}} {
		mod := genTSWith(t, int64Def, cfg)
		for _, want := range []string{
			`import { OStream, Cursor } from "@sofa-buffers/corelib";`,
			"us: bigint[] = [];",
			"os.writeUnsignedArray(0, this.us);",
			"case 0: o.us = c.readUnsignedArray() as bigint[]; break;",
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
