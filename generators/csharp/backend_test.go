package csharp

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/sofa-buffers/generator/internal/analysis"
	"github.com/sofa-buffers/generator/internal/ir"
	"github.com/sofa-buffers/generator/internal/model"
	"github.com/sofa-buffers/generator/internal/parser"
)

// buildModule parses a YAML definition, builds the IR, generates with cfg and
// returns the Message.cs content.
func buildModule(t *testing.T, data []byte, name string, cfg map[string]any) string {
	t.Helper()
	doc, err := parser.Parse(data, name)
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
	files, err := (&Backend{}).Generate(s, cfg)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	for _, f := range files {
		if f.Path == "Message.cs" {
			return string(f.Content)
		}
	}
	t.Fatal("no module")
	return ""
}

func exampleModule(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("../../examples/messages/example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return buildModule(t, b, "example.yaml", map[string]any{"namespace": "Sofabuffers"})
}

// TestCsOverIndexWrapperArray: a fixed-count wrapper array (string/blob/struct
// elements) throws InvalidMessage for an element id >= N before the List grows
// (issue #142 / MESSAGE_SPEC §5.1/§7). The comparison is corelib-cs's Seq
// (generator#587): generated code passes the schema count and the receiver cap
// and emits no index compare of its own.
func TestCsOverIndexWrapperArray(t *testing.T) {
	src := []byte("version: 1\nmessages:\n  M:\n    payload:\n" +
		"      bs: { id: 0, type: array, items: { type: string, count: 4, maxlen: 16 } }\n" +
		"      bb: { id: 1, type: array, items: { type: blob,   count: 3, maxlen: 16 } }\n" +
		"      bp: { id: 2, type: array, items: { type: struct, count: 2, fields: { x: { id: 0, type: i32 } } } }\n" +
		"      ds: { id: 3, type: array, items: { type: string } }\n")
	m := buildModule(t, src, "in.yaml", map[string]any{"namespace": "S"})
	for _, want := range []string{
		// at the length word, then at placement
		`case (Root_bs, _): global::sofab.Seq.CheckIndex(id, 4, MaxDynArrayCount);`,
		`case (Root_bs, _): global::sofab.Seq.PlaceElem(m.bs, id, "", _s, 4, MaxDynArrayCount); break;`,
		`case (Root_bb, _): global::sofab.Seq.CheckIndex(id, 3, MaxDynArrayCount);`,
		`case (Root_bb, _): global::sofab.Seq.PlaceElem(m.bb, id, Array.Empty<byte>(), _b, 3, MaxDynArrayCount); break;`,
		`case (Root_bp, _): global::sofab.Seq.ReserveElem(m.bp, id, static () => new `,
		`(), 2, MaxDynArrayCount); _ixRoot_bp = id; cur = Root_bp_e; break;`,
	} {
		if !strings.Contains(m, want) {
			t.Errorf("Message.cs missing over-index bound %q", want)
		}
	}
	if n := strings.Count(m, "if (id >= "); n != 0 {
		t.Errorf("generated code must not compare an element index itself (%d sites):\n%s", n, m)
	}
	// A DYNAMIC wrapper array is bounded too, and by the same call in the same
	// place -- what differs is the bound and the category. Its length is its
	// highest index, so the receiver cap binds the index; the bytes are well
	// formed and decode under a looser cap, so the verdict is LimitExceeded and
	// not InvalidMessage (generator#387, CORELIB_PLAN §6.2.1). The -1 tells the
	// corelib the schema declares no count, so it compares the cap instead.
	if !strings.Contains(m, `case (Root_ds, _): global::sofab.Seq.PlaceElem(m.ds, id, "", _s, -1, MaxDynArrayCount); break;`) {
		t.Errorf("a dynamic wrapper array's element index must be capped:\n%s", m)
	}
}

// TestCsMaxlenReject: a bounded string/blob whose wire byte length exceeds its
// schema maxlen is malformed input, rejected as INVALID at the `total` length
// header (MESSAGE_SPEC §7.1) — for scalar fields and wrapper-array elements
// alike, never truncated. The comparison happens ONCE, in FixlenBegin at the
// length word (#594); the payload callback only carries the number on to
// PayloadAcc as `_cap`. An unbounded field gets no maxlen arm.
func TestCsMaxlenReject(t *testing.T) {
	src := []byte("version: 1\nmessages:\n  M:\n    payload:\n" +
		"      s:  { id: 0, type: string, maxlen: 8 }\n" +
		"      b:  { id: 1, type: blob, maxlen: 8 }\n" +
		"      ws: { id: 2, type: array, items: { type: string, maxlen: 5 } }\n" +
		"      us: { id: 3, type: string }\n")
	m := buildModule(t, src, "in.yaml", map[string]any{"namespace": "S"})
	for _, want := range []string{
		// Bounded scalar string + blob: the per-field maxlen check sits at the
		// LENGTH WORD, in FixlenBegin, and nowhere else.
		`case (Root, 0): if (total > 8) throw new SofabException(SofabError.InvalidMessage, "s: string length above schema maxlen 8"); break;`,
		`case (Root, 1): if (total > 8) throw new SofabException(SofabError.InvalidMessage, "b: blob length above schema maxlen 8"); break;`,
		// Bounded wrapper string element: keyed by the array location, element id agnostic.
		`case (Root_ws, _): global::sofab.Seq.CheckIndex(id, -1, MaxDynArrayCount); if (total > 5) throw new SofabException(SofabError.InvalidMessage, "Root_ws element: string length above schema maxlen 5"); break;`,
	} {
		if !strings.Contains(m, want) {
			t.Errorf("Message.cs missing maxlen guard %q\n%s", want, m)
		}
	}
	// The payload callback states NO bound of its own: its dispatch resolves the
	// destination and hands PayloadAcc that same maxlen as `_cap` -- the schema
	// bound governs a bounded field, so the receiver cap must not reach it and
	// the corelib's comparison can no longer fire (CORELIB_PLAN §6.2.1/§6.3).
	for _, want := range []string{
		"case (Root, 0): _cap = 8; break;",
		"case (Root, 1): _cap = 8; break;",
		"case (Root_ws, _): _cap = 5; break;",
	} {
		if !strings.Contains(m, want) {
			t.Errorf("Message.cs missing payload cap arm %q\n%s", want, m)
		}
	}
	// Exactly one comparison per bounded destination (#594): the length word's.
	// Three bounded destinations here, so three -- the payload callbacks restate
	// none of them.
	if n := strings.Count(m, "above schema maxlen"); n != 3 {
		t.Errorf("expected 3 maxlen comparisons (one per bounded destination), got %d:\n%s", n, m)
	}
	if n := strings.Count(m, "total > 5"); n != 1 {
		t.Errorf("the element maxlen must be compared exactly once, got %d:\n%s", n, m)
	}
	// The unbounded string carries no maxlen reject (only its plain store arm).
	if strings.Contains(m, "us: string length above schema maxlen") {
		t.Errorf("unbounded string must not carry a maxlen guard:\n%s", m)
	}
}

// TestCsArrayAtScalarSkip: an integer ARRAY wire type at an id that does not
// declare a native array of the matching element kind is a wire-type
// contradiction and must be SKIPPED like an unknown id (MESSAGE_SPEC §7.3 /
// generator#183 for integers, #193 for fp). corelib-cs delivers array elements
// through the same Unsigned/Signed/Fp32/Fp64 callbacks a lone scalar uses, so
// the (cur, id) dispatch alone cannot see it: ArrayBegin arms an `askip` counter
// with the announced element count and the scalar callbacks discard exactly that
// many. Only ids that genuinely declare a native array of that element kind (and
// nested native inner-array scopes) disarm it — integer arrays under
// Unsigned/Signed, fp32 arrays under Fp32, fp64 arrays under Fp64.
func TestCsArrayAtScalarSkip(t *testing.T) {
	src := []byte("version: 1\nmessages:\n  M:\n    payload:\n" +
		"      u:  { id: 0, type: u8 }\n" +
		"      i:  { id: 1, type: i8 }\n" +
		"      ua: { id: 2, type: array, items: { type: u32, count: 4 } }\n" +
		"      ia: { id: 3, type: array, items: { type: i32 } }\n" +
		"      fa: { id: 4, type: array, items: { type: fp32, count: 2 } }\n" +
		"      na: { id: 5, type: array, items: { type: array, items: { type: u16, count: 2 }, count: 2 } }\n")
	m := buildModule(t, src, "in.yaml", map[string]any{"namespace": "S"})
	for _, want := range []string{
		// The discard clause heads every callback a scalar shares.
		"public void Unsigned(int id, ulong value) {\n        if (askip > 0) { askip--; return; }",
		"public void Signed(int id, long value) {\n        if (askip > 0) { askip--; return; }",
		"public void Fp32(int id, float value) {\n        if (askip > 0) { askip--; return; }",
		"public void Fp64(int id, double value) {\n        if (askip > 0) { askip--; return; }",
		// Visitor state.
		"private int askip = 0;",
		// Armed in ArrayBegin, one arm per array kind (#254).
		"askip = kind switch {",
		"            ArrayKind.Unsigned => (cur, id) switch {",
		"            ArrayKind.Signed => (cur, id) switch {",
		"            ArrayKind.Fp32 => (cur, id) switch {",
		"            ArrayKind.Fp64 => (cur, id) switch {",
		// Each declared array disarms under ITS OWN kind: the u32 array (id 2) and
		// the nested u16 inner scope under Unsigned, the i32 array (id 3) under
		// Signed, the fp32 array (id 4) under Fp32 (#193, re-keyed by subtype in #259).
		"                (Root, 2) => 0,",
		"                (Root, 3) => 0,",
		"                (Root_na, _) => 0,",
		"                (Root, 4) => 0,",
		// Everything else — scalar ids, unknown ids — discards `count`.
		"                _ => count,",
	} {
		if !strings.Contains(m, want) {
			t.Errorf("Message.cs missing §7.3 array-skip guard %q\n%s", want, m)
		}
	}
	// Scalar ids never disarm the counter.
	for _, bad := range []string{"                (Root, 0) => 0,", "                (Root, 1) => 0,"} {
		if strings.Contains(m, bad) {
			t.Errorf("scalar id must not disarm the array-skip counter (%q):\n%s", bad, m)
		}
	}
	// Unsigned and Signed are not one case (generator#254): the u32 array (id 2)
	// must not disarm the counter for an array-signed header, nor the i32 array
	// (id 3) for an array-unsigned one.
	if strings.Contains(m, "ArrayKind.Unsigned or ArrayKind.Signed") {
		t.Errorf("Unsigned and Signed must be separate arms (generator#254):\n%s", m)
	}
	for _, want := range []string{
		"            ArrayKind.Unsigned => (cur, id) switch {\n                (Root, 2) => 0,\n                (Root_na, _) => 0,\n                _ => count,\n            },",
		"            ArrayKind.Signed => (cur, id) switch {\n                (Root, 3) => 0,\n                _ => count,\n            },",
		// The fp32 array is the ONLY disarming id under Fp32, and no id at all
		// disarms under Fp64: this message declares no fp64 array, so every fp64
		// header it can receive is a §7.3 skip (generator#259).
		"            ArrayKind.Fp32 => (cur, id) switch {\n                (Root, 4) => 0,\n                _ => count,\n            },",
		"            ArrayKind.Fp64 => (cur, id) switch {\n                _ => count,\n            },",
	} {
		if !strings.Contains(m, want) {
			t.Errorf("Message.cs missing per-kind skip arm %q:\n%s", want, m)
		}
	}
	// The collapsed fixlen category is gone from the ABI (generator#259).
	if strings.Contains(m, "ArrayKind.Fixlen") {
		t.Errorf("ArrayKind.Fixlen no longer exists; fixlen arrays are keyed by subtype:\n%s", m)
	}
}

// TestCsMistypedArrayNotAllocated: MESSAGE_SPEC §7.3 — "A decoder ... MUST NOT
// decode its payload into the declared field." A native array field whose header
// carries the WRONG array kind (an array-signed header at a u8[]-declared id) is
// skipped like an unknown id, and skipping includes NOT RESIZING the declared
// field from the skipped header's count: the leak generator#254 pins is the
// LENGTH, not the element — csharp re-encoded `a6 06 04 01 06 07` as
// `a6 06 03 01 00 07`, a one-element unsigned array the wire never carried.
//
// Both halves are asserted: the skip counter is armed per array kind (above), and
// every ArrayBegin allocation arm is fronted by the kind test — which comes
// BEFORE the schema bound, so an over-count MIS-TYPED array is skipped rather
// than rejected as a false InvalidMessage (§7.3: "the schema bound applied only
// to a field that survives it").
func TestCsMistypedArrayNotAllocated(t *testing.T) {
	src := []byte(`
version: 1
$defs:
  enum:
    E: { A: 0, B: 1 }
messages:
  M:
    payload:
      ua: { id: 0, type: array, items: { type: u8, count: 5 } }
      ia: { id: 1, type: array, items: { type: i8, count: 5 } }
      fa: { id: 2, type: array, items: { type: fp32, count: 3 } }
      ba: { id: 3, type: array, items: { type: boolean, count: 2 } }
      ea: { id: 4, type: array, items: { type: enum, count: 2, enum: { $ref: "#/$defs/enum/E" } } }
      da: { id: 5, type: array, items: { type: u16 } }
`)
	m := buildModule(t, src, "in.yaml", map[string]any{"namespace": "S"})
	for _, want := range []string{
		// The kind test fronts the allocation AND precedes the schema bound.
		`case (Root, 0): if (kind != ArrayKind.Unsigned) break; if (count > 5) throw new SofabException(SofabError.InvalidMessage, "ua: array count above schema capacity 5"); m.ua = new byte[count]; break;`,
		`case (Root, 1): if (kind != ArrayKind.Signed) break; if (count > 5) throw new SofabException(SofabError.InvalidMessage, "ia: array count above schema capacity 5"); m.ia = new sbyte[count]; break;`,
		`case (Root, 2): if (kind != ArrayKind.Fp32) break; if (count > 3) throw new SofabException(SofabError.InvalidMessage, "fa: array count above schema capacity 3"); m.fa = new float[count]; break;`,
		// A boolean array is a List: clearing it is decoding into it too, so the
		// kind test fronts the Clear() as well. An ENUM array is a primitive array
		// at the width its declaration implies, so it allocates exactly as `ia`
		// does. boolean rides the Unsigned wire type, enum the Signed one.
		`case (Root, 3): if (kind != ArrayKind.Unsigned) break; if (count > 2) throw new SofabException(SofabError.InvalidMessage, "ba: array count above schema capacity 2"); m.ba.Clear(); break;`,
		`case (Root, 4): if (kind != ArrayKind.Signed) break; if (count > 2) throw new SofabException(SofabError.InvalidMessage, "ea: array count above schema capacity 2"); m.ea = new sbyte[count]; break;`,
		// A count-less array has no schema bound, so the target's finite default
		// cap governs it (§9.5, generator#385) -- like a schema bound, checked
		// BEHIND the kind test.
		`case (Root, 5): if (kind != ArrayKind.Unsigned) break; if (count > MaxDynArrayCount) throw new SofabException(SofabError.LimitExceeded, "da: array count above configured limit 65536"); m.da = new ushort[count]; break;`,
		// The skip counter is armed per kind; each id disarms under its own kind only.
		"            ArrayKind.Unsigned => (cur, id) switch {\n                (Root, 0) => 0,\n                (Root, 3) => 0,\n                (Root, 5) => 0,\n                _ => count,\n            },",
		"            ArrayKind.Signed => (cur, id) switch {\n                (Root, 1) => 0,\n                (Root, 4) => 0,\n                _ => count,\n            },",
		"            ArrayKind.Fp32 => (cur, id) switch {\n                (Root, 2) => 0,\n                _ => count,\n            },",
	} {
		if !strings.Contains(m, want) {
			t.Errorf("Message.cs missing §7.3 mis-typed-array guard %q:\n%s", want, m)
		}
	}
	// The bound must never precede the kind test: an over-count mis-typed array is
	// skipped, not a false InvalidMessage.
	if strings.Contains(m, "case (Root, 0): if (count > 5)") {
		t.Error("the schema bound must sit BEHIND the §7.3 kind test (generator#254)")
	}
}

// TestCsFixlenArrayKeyedBySubtype: a fixlen array header names its element
// SUBTYPE, so fp32 and fp64 are two distinct wire kinds, not one collapsed
// "fixlen" category (CORELIB_PLAN §4.8 / generator#259 / Crucible F-0042).
// corelib-cs now reads the fixlen_word BEFORE announcing the array, so
// ArrayBegin carries ArrayKind.Fp32 or ArrayKind.Fp64 — and generated code must
// key on it, exactly as it already keys Unsigned apart from Signed
// (generator#254).
//
// What that buys, on a schema declaring both an fp32[4] and an fp64[2]:
//
//   - an fp64 header at the fp32-declared id fails the arm's kind test, so the
//     declared float[] is never sized, cleared or allocated from it;
//   - the schema `count` bound sits BEHIND that kind test, so an OVER-COUNT
//     fp64 header at the fp32 slot is skipped (MESSAGE_SPEC §7.3) instead of
//     rejected as a false InvalidMessage — a skipped field's element count is
//     not this array's count and no schema bound may be applied to it;
//   - the skip counter is armed for the non-matching subtype, so the elements
//     that follow are discarded one by one like an unknown id's.
func TestCsFixlenArrayKeyedBySubtype(t *testing.T) {
	const src = `
version: 1
messages:
  M:
    payload:
      f32: { id: 0, type: array, items: { type: fp32, count: 4 } }
      f64: { id: 1, type: array, items: { type: fp64, count: 2 } }
      dyn: { id: 2, type: array, items: { type: fp32 } }
`
	m := buildModule(t, []byte(src), "fixlen.yaml", map[string]any{"namespace": "S"})
	for _, want := range []string{
		// Each declared fixlen array is fronted by ITS OWN subtype's kind test, with
		// the schema capacity bound behind it.
		`case (Root, 0): if (kind != ArrayKind.Fp32) break; if (count > 4) throw new SofabException(SofabError.InvalidMessage, "f32: array count above schema capacity 4"); m.f32 = new float[count]; break;`,
		`case (Root, 1): if (kind != ArrayKind.Fp64) break; if (count > 2) throw new SofabException(SofabError.InvalidMessage, "f64: array count above schema capacity 2"); m.f64 = new double[count]; break;`,
		// A count-less fixlen array has no schema bound, so the finite default cap
		// governs it (§9.5, generator#385), behind the kind test.
		`case (Root, 2): if (kind != ArrayKind.Fp32) break; if (count > MaxDynArrayCount) throw new SofabException(SofabError.LimitExceeded, "dyn: array count above configured limit 65536"); m.dyn = new float[count]; break;`,
		// The skip counter: the fp32 ids disarm only under Fp32, the fp64 id only
		// under Fp64. An fp64 header at id 0 therefore arms `count` discards.
		"            ArrayKind.Fp32 => (cur, id) switch {\n                (Root, 0) => 0,\n                (Root, 2) => 0,\n                _ => count,\n            },",
		"            ArrayKind.Fp64 => (cur, id) switch {\n                (Root, 1) => 0,\n                _ => count,\n            },",
		// The fill counter is the exact complement: armed only under the matching
		// subtype, so a mis-typed header never stores an element either.
		"        afill = kind switch {",
		"            ArrayKind.Fp32 => (cur, id) switch {\n                (Root, 0) => count,\n                (Root, 2) => count,\n                _ => 0,\n            },",
		"            ArrayKind.Fp64 => (cur, id) switch {\n                (Root, 1) => count,\n                _ => 0,\n            },",
	} {
		if !strings.Contains(m, want) {
			t.Errorf("Message.cs missing subtype-keyed fixlen arm %q:\n%s", want, m)
		}
	}
	for _, bad := range []string{
		// The collapsed category is gone from the corelib ABI entirely.
		"ArrayKind.Fixlen",
		// The bound must never precede the kind test, on either subtype: an
		// over-count MIS-TYPED fixlen header is a skip, not an InvalidMessage.
		"case (Root, 0): if (count > 4)",
		"case (Root, 1): if (count > 2)",
		// An fp64 id must never disarm the fp32 counter, nor the reverse: that is
		// exactly the fold that let a declared float[] be sized from an fp64 header.
		"            ArrayKind.Fp32 => (cur, id) switch {\n                (Root, 0) => 0,\n                (Root, 1) => 0,",
		"            ArrayKind.Fp64 => (cur, id) switch {\n                (Root, 0) => 0,",
	} {
		if strings.Contains(m, bad) {
			t.Errorf("Message.cs must not contain %q (generator#259):\n%s", bad, m)
		}
	}
}

func TestCsStructural(t *testing.T) {
	m := exampleModule(t)
	for _, want := range []string{
		"using sofab;",
		"namespace Sofabuffers;",
		"public sealed class Myfirstmessage {",
		"public void Serialize(OStream os)",
		"public byte[] Encode()",
		"public static Myfirstmessage Decode(byte[] data)",
		"public static DecodeStatus TryDecode(byte[] data, out Myfirstmessage msg)", // status-surfacing decode (#105)
		"internal sealed class MyfirstmessageVisitor : IVisitor {",
		"public void SequenceBegin(int id)", // flat-visitor nesting
		"public ulong someu64 = 18446744073709551615UL;",
		"public enum MyfirstmessageSomeenum : sbyte {",
		// Reassembly of a split payload and the strict UTF-8 verdict are the
		// corelib's (corelib-cs#92): the value comes back on the chunk that
		// completes it, invalid UTF-8 as INVALID (issue #85).
		// The accumulator is created only for a payload split across feeds; one
		// that arrives whole takes PayloadAcc's own one-chunk branch inline, cap
		// check first.
		"private PayloadAcc pay;",
		"if (offset == 0 && chunkLength >= total) { _s = global::sofab.Utf8.Decode(data, chunkOffset, total); }",
		"else _s = (pay ??= new PayloadAcc()).String(total, offset, data, chunkOffset, chunkLength, _cap);",
		"if (offset == 0 && chunkLength >= total) { _b = new byte[total]; Array.Copy(data, chunkOffset, _b, 0, total); }",
		"else _b = (pay ??= new PayloadAcc()).Blob(total, offset, data, chunkOffset, chunkLength, _cap);",
		// Encode() reuses a per-thread encoder; Reset drops whatever a previous,
		// failed Encode() left open.
		"[ThreadStatic] private static OStream _encStream;",
		"if (os == null) { _encStream = os = new OStream(buf); } else { os.Reset(buf, 0); }",
		// over-count scalar array rejected as INVALID before the (untrusted-count) allocation (#100)
		"if (count > 4) throw new SofabException(SofabError.InvalidMessage, \"someuintarray: array count above schema capacity 4\"); ",
	} {
		if !strings.Contains(m, want) {
			t.Errorf("Message.cs missing %q", want)
		}
	}
}

// TestCsMetadataDoc: field/enum/flag metadata renders as XML-doc comments and
// native annotations — a deprecated field carries [Obsolete] plus a
// "Deprecated." doc note (and the generated marshal/decode that reads it is
// wrapped in a CS0612 pragma so the output builds warning-clean), each enum
// constant carries its description, and each flag carries its description with
// the (default: true/false) note when the flag declares a default.
func TestCsMetadataDoc(t *testing.T) {
	const src = `
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
      legacyId: { id: 1, type: u32, description: "Old identifier retained for backward compatibility.", deprecated: true }
      mode:     { id: 2, type: enum, enum: { $ref: "#/$defs/enum/Mode" } }
      status:   { id: 3, type: bitfield, bits: { $ref: "#/$defs/bitfield/StatusFlags" } }
`
	m := buildModule(t, []byte(src), "meta.yaml", map[string]any{"namespace": "Demo.Messages"})
	for _, want := range []string{
		// Deprecated field: doc note + native [Obsolete] attribute.
		"/// Old identifier retained for backward compatibility.\n    /// Deprecated.\n    /// </summary>\n    [Obsolete]\n    public uint legacyId;",
		// Internal access to the deprecated field is CS0612-suppressed.
		"    public void Serialize(OStream os) {\n#pragma warning disable 612 // internal access to a member marked [Obsolete] (CS0612)",
		"#pragma warning restore 612\n    }",
		"#pragma warning disable 612 // internal access to a member marked [Obsolete] (CS0612)\ninternal sealed class TelemetryVisitor : IVisitor {",
		// Enum constant descriptions.
		"/// <summary>\n    /// Node is powered down.\n    /// </summary>\n    Off = 0,",
		"/// <summary>\n    /// Node is sampling and transmitting.\n    /// </summary>\n    Active = 1,",
		// Flag descriptions + default note.
		"/// <summary>\n    /// Node has completed initialization. (default: true)\n    /// </summary>\n    Ready = 1,",
		"/// <summary>\n    /// Core temperature exceeded the safe threshold.\n    /// </summary>\n    Overheated = 2,",
	} {
		if !strings.Contains(m, want) {
			t.Errorf("Message.cs missing %q", want)
		}
	}
	// No development/issue/spec citations leak into the generated comments.
	for _, junk := range []string{"generator#", "MESSAGE_SPEC", "cf. #96", "(generator#102)"} {
		if strings.Contains(m, junk) {
			t.Errorf("Message.cs leaks junk citation %q", junk)
		}
	}
}

func TestCsDeterministic(t *testing.T) {
	if exampleModule(t) != exampleModule(t) {
		t.Fatal("C# generation not deterministic")
	}
}

// TestCsDecodeLimits: the max_dyn_* config keys bake receiver-side decode
// limits (generator#102) into the generated visitor — constants next to the
// location constants plus per-field SofabError.LimitExceeded guards on
// schema-unbounded fields only, checked at the count/total header before any
// allocation. A schema-bounded field keeps only its generator#100
// schema-capacity guard, an unset key emits nothing, and a configured key
// whose kind has no unbounded field is inert. Independently of any config, the
// count-less primitive-array arm is hardened: a small bounded reservation
// grown on demand (Seq.EnsureCap) instead of an eager `new T[count]` from the
// untrusted wire count.
func TestCsDecodeLimits(t *testing.T) {
	const src = `
version: 1
messages:
  dyn:
    payload:
      s:    { id: 0, type: string }
      arr:  { id: 1, type: array, items: { type: u64 } }
      barr: { id: 2, type: array, items: { type: i32, count: 100000 } }
`
	m := buildModule(t, []byte(src), "dyn.yaml", map[string]any{
		"max_dyn_array_count": 65536,
		"max_dyn_string_len":  4096,
		"max_dyn_blob_len":    2048, // no unbounded blob in the schema -> inert
	})
	for _, want := range []string{
		"private const long MaxDynArrayCount = 65536;",
		"private const long MaxDynStringLen = 4096;",
		// Unbounded array: LimitExceeded at the count header, then a bounded
		// initial reservation grown on demand — never `new ulong[count]`.
		"case (Root, 1): if (kind != ArrayKind.Unsigned) break; if (count > MaxDynArrayCount) throw new SofabException(SofabError.LimitExceeded, \"arr: array count above configured limit 65536\"); m.arr = new ulong[count]; break;",
		"m.arr[ai++] = (ulong)value;",
		// Bounded array: only the #100 schema-capacity guard, and an alloc at the
		// WIRE count -- `count: N` is a capacity, so M is the length (§3) and the
		// guard is what still bounds the untrusted count.
		"case (Root, 2): if (kind != ArrayKind.Signed) break; if (count > 100000) throw new SofabException(SofabError.InvalidMessage, \"barr: array count above schema capacity 100000\"); m.barr = new int[count]; break;",
		// Unbounded string: the cap travels into PayloadAcc, which compares
		// `total` against it before it takes a byte (CORELIB_PLAN §6.2.1,
		// corelib-cs#101). The number is still this layer's -- passed per call,
		// never held by the corelib and with no omitted-argument "unlimited".
		"case (Root, 0): _cap = MaxDynStringLen; break;",
		"else _s = (pay ??= new PayloadAcc()).String(total, offset, data, chunkOffset, chunkLength, _cap);",
		"PayloadAcc.CheckStringLength(total, _cap); _s = global::sofab.Utf8.Decode(data, chunkOffset, total);",
	} {
		if !strings.Contains(m, want) {
			t.Errorf("Message.cs missing %q", want)
		}
	}
	if strings.Contains(m, "MaxDynBlobLen") {
		t.Error("inert blob limit must not be emitted (no unbounded blob)")
	}
	// The bounded array must not pick up a LimitExceeded guard.
	if strings.Contains(m, "barr: array count above configured limit") {
		t.Error("bounded field must keep only its #100 schema-capacity guard")
	}

	// No keys configured -> the target's finite DEFAULTS, not "unlimited"
	// (§9.5, generator#385). C# is on the server tier. The eager-allocation
	// hardening of the count-less arm is orthogonal and remains either way.
	plain := buildModule(t, []byte(src), "dyn.yaml", map[string]any{})
	for _, want := range []string{
		"const long MaxDynArrayCount = 65536;",
		"case (Root, 1): if (kind != ArrayKind.Unsigned) break; if (count > MaxDynArrayCount) throw new SofabException(SofabError.LimitExceeded, \"arr: array count above configured limit 65536\"); m.arr = new ulong[count]; break;",
		"m.arr[ai++] = (ulong)value;",
	} {
		if !strings.Contains(plain, want) {
			t.Errorf("no-config Message.cs missing hardened count-less arm %q", want)
		}
	}
	// Liveness is still a property of the schema, not of the configuration.
	if strings.Contains(plain, "MaxDynBlobLen") {
		t.Error("inert blob limit must not be emitted (no unbounded blob)")
	}
	// The bounded array allocates the wire count exactly (bounded by its schema
	// capacity guard), never lazy growth and never N.
	if !strings.Contains(plain, "m.barr = new int[count]; break;") {
		t.Error("bounded array must allocate the wire count")
	}
}

// `count: N` is a CAPACITY, not a length (MESSAGE_SPEC §3): it never reaches the
// wire, the wire count M IS the array's length, and nothing that carries that
// length may be elided. So the whole trim-on-encode / fill-on-decode pair is gone
// — from both array forms — and a count:N array is generated exactly like a
// count-less one except for the bound it still enforces.
func TestCsArrayCountIsCapacityNotLength(t *testing.T) {
	const src = `
version: 1
$defs:
  enum:
    Color: { none: 0, red: 1, blue: 2 }
  bitfield:
    Perm: { read: { pos: 0 }, write: { pos: 1 } }
messages:
  m:
    payload:
      fx:   { id: 0, type: array, items: { type: u32, count: 5 } }
      dyn:  { id: 1, type: array, items: { type: u32 } }
      ffs:  { id: 2, type: array, items: { type: i16, count: 3 } }
      ff32: { id: 3, type: array, items: { type: fp32, count: 4 }, default: [1.5] }
      ff64: { id: 4, type: array, items: { type: fp64, count: 2 } }
      fb:   { id: 5, type: array, items: { type: boolean, count: 3 } }
      fe:   { id: 6, type: array, items: { type: enum, count: 3, enum: { $ref: "#/$defs/enum/Color" } }, default: [2] }
      fp:   { id: 7, type: array, items: { type: bitfield, count: 2, bits: { $ref: "#/$defs/bitfield/Perm" } } }
      fxd:  { id: 8, type: array, items: { type: u32, count: 5 }, default: [1, 2] }
      strs: { id: 9, type: array, items: { type: string, count: 2, maxlen: 8 } }
      big:  { id: 10, type: array, items: { type: string, count: 65, maxlen: 8 } }
      edge: { id: 11, type: array, items: { type: string, count: 64, maxlen: 8 } }
      free: { id: 12, type: array, items: { type: string, maxlen: 8 } }
`
	m := buildModule(t, []byte(src), "capacity.yaml", map[string]any{})

	// Nothing narrows anything any more: the trim helper class is gone whole, and
	// with it every per-class TrimTail. Its presence would mean a call site that
	// still shortens a value.
	for _, gone := range []string{"SofabFixedArray", "TrimTail", "TrimTailF32", "TrimTailF64", "TrimStrs", "TrimBlobs", "TrimRows", "Filled<"} {
		if strings.Contains(m, gone) {
			t.Errorf("`count` is a capacity: %q must not be emitted:\n%s", gone, m)
		}
	}

	for _, want := range []string{
		// Encode writes every element the value holds, count:N and count-less alike.
		"os.WriteArrayUnsigned(0, this.fx);",
		"os.WriteArrayUnsigned(1, this.dyn);",
		"os.WriteArraySigned(2, this.ffs);",
		"os.WriteArrayFp32(3, this.ff32);",
		"os.WriteArrayFp64(4, this.ff64);",
		"os.WriteArrayUnsigned(5, Array.ConvertAll(this.fb.ToArray(), _x => _x ? (byte)1 : (byte)0));",
		"os.WriteArraySigned(6, this.fe);",
		"os.WriteArrayUnsigned(7, this.fp);",

		// A fresh count:N array is EMPTY (nothing materialized to N), and a declared
		// default shorter than N stands exactly as written (never tail-padded).
		"public uint[] fx = Array.Empty<uint>();",
		"public double[] ff64 = Array.Empty<double>();",
		// ...but a small bound sizes the List's CAPACITY, so decoding up to N
		// elements never regrows it. Count is still 0.
		"public List<bool> fb = new(3);",
		"public byte[] fp = Array.Empty<byte>();",
		"public List<string> strs = new(2);",
		"public List<string> edge = new(64);",
		// Above presizeMaxCount, and without a count, the default capacity.
		"public List<string> big = new();",
		"public List<string> free = new();",
		"public float[] ff32 = new float[]{1.5f};",
		"public sbyte[] fe = new sbyte[]{2};",
		"public uint[] fxd = new uint[]{1, 2};",
		"private static readonly uint[] _arrdef_fxd = new uint[]{1, 2};",

		// Decode takes the M elements that arrived and nothing else: the primitive
		// arrays allocate the WIRE count (the #100 guard still bounds it by N), the
		// List<T> ones clear and append.
		`case (Root, 0): if (kind != ArrayKind.Unsigned) break; if (count > 5) throw new SofabException(SofabError.InvalidMessage, "fx: array count above schema capacity 5"); m.fx = new uint[count]; break;`,
		`case (Root, 3): if (kind != ArrayKind.Fp32) break; if (count > 4) throw new SofabException(SofabError.InvalidMessage, "ff32: array count above schema capacity 4"); m.ff32 = new float[count]; break;`,
		`case (Root, 5): if (kind != ArrayKind.Unsigned) break; if (count > 3) throw new SofabException(SofabError.InvalidMessage, "fb: array count above schema capacity 3"); m.fb.Clear(); break;`,
		"case (Root, 5): if (afill == 0) break; afill--; m.fb.Add(value != 0); break;",
		// Both kinds carry their §1 width bound between fillGuard and the store
		// (generator#516): {0,1,2} implies an i8 and positions {0,1} imply a u8,
		// so an element outside those intervals is INVALID -- and one inside them
		// decodes whether or not the schema names it.
		`case (Root, 6): if (afill == 0) break; afill--; if (value < -128 || value > 127) throw new SofabException(SofabError.InvalidMessage, "fe element: value outside declared enum width"); m.fe[ai++] = (sbyte)value; break;`,
		`case (Root, 7): if (afill == 0) break; afill--; if (value > 255) throw new SofabException(SofabError.InvalidMessage, "fp element: value outside declared bitfield width"); m.fp[ai++] = (byte)value; break;`,

		// A count:N array with no declared default is default only when EMPTY: an
		// all-zero length-N value is a different value and stays on the wire.
		"if (this.fx != null && this.fx.Length != 0) {",
		"if (!(this.fx == null || this.fx.Length == 0)) return false;",
		"if (!(this.strs.Count == 0)) return false;",
	} {
		if !strings.Contains(m, want) {
			t.Errorf("Message.cs missing %q:\n%s", want, m)
		}
	}

	for _, bad := range []string{
		// The superseded shapes: an N-sized alloc, an N-element pre-fill, an
		// index-based List write, an N-padded literal, and the SequenceEnd refill.
		"m.fx = new uint[5];",
		"m.fb.Clear(); for (int _p",
		"m.fb[ai++]",
		"public uint[] fxd = new uint[]{1, 2, 0",
		"public float[] ff32 = new float[]{1.5f, 0f",
		"while (m.strs.Count < 2) m.strs.Add",
	} {
		if strings.Contains(m, bad) {
			t.Errorf("Message.cs must not contain the superseded fixed-length form %q:\n%s", bad, m)
		}
	}
	// SequenceEnd is a bare scope pop: there is no length to reconstruct.
	if !strings.Contains(m, "public void SequenceEnd() { cur = sp > 0 ? stk[--sp] : 0; }") {
		t.Errorf("SequenceEnd must be a bare scope pop:\n%s", m)
	}
}

// MESSAGE_SPEC §2 governs both element kinds with ONE rule, and the rule is
// positional: an element before the last one that equals its element default is
// omitted — a string/blob leaf simply not written, a sequence element not framed
// either — while the element at the LAST index is always written, as its value or
// as an empty frame. Only the last index carries the array's length (§5.1); an
// interior gap is restored from the element default and is therefore free.
//
// The choice is made from the position in the VALUE, at run time; the schema
// cannot answer it. This holds with or without a declared `count`: a capacity can
// never restore an elided tail.
func TestCsElementSparsityIsPositional(t *testing.T) {
	const src = `
version: 1
messages:
  vec:
    payload:
      dynstr:   { id: 0, type: array, items: { type: string, maxlen: 8 } }
      dynblob:  { id: 1, type: array, items: { type: blob, maxlen: 8 } }
      fixedstr: { id: 2, type: array, items: { type: string, count: 3, maxlen: 8 } }
      fixedobj: { id: 3, type: array, items: { type: struct, count: 5, fields: { k: { id: 0, type: u32 } } } }
      dynobj:   { id: 4, type: array, items: { type: struct, fields: { k: { id: 0, type: u32 } } } }
      rows:     { id: 5, type: array, items: { type: array, count: 2, items: { type: u32 } } }
      srows:    { id: 6, type: array, items: { type: array, items: { type: string, maxlen: 8 } } }
`
	m := buildModule(t, []byte(src), "vec.yaml", map[string]any{})

	for _, want := range []string{
		// LEAF elements: the omit test is `!= default || last`, unconditionally —
		// the count:N array is written exactly like the count-less one beside it.
		`for (int _i0 = 0, _n0 = this.dynstr.Count; _i0 < _n0; _i0++) { if ((this.dynstr[_i0] ?? "") != "" || _i0 == _n0 - 1) os.WriteString(_i0, this.dynstr[_i0] ?? ""); }`,
		`for (int _i0 = 0, _n0 = this.dynblob.Count; _i0 < _n0; _i0++) { if ((this.dynblob[_i0] ?? Array.Empty<byte>()).Length != 0 || _i0 == _n0 - 1) os.WriteBlob(_i0, this.dynblob[_i0] ?? Array.Empty<byte>()); }`,
		`for (int _i0 = 0, _n0 = this.fixedstr.Count; _i0 < _n0; _i0++) { if ((this.fixedstr[_i0] ?? "") != "" || _i0 == _n0 - 1) os.WriteString(_i0, this.fixedstr[_i0] ?? ""); }`,

		// SEQUENCE elements: the same rule, applied to the lazily-held frame. The
		// dropping closer in the interior (an all-default element writes no child, so
		// the frame vanishes and leaves an id gap), the keeping one at the last index.
		"os.WriteSequenceBeginLazy(_i0); (this.fixedobj[_i0] ?? new VecFixedobjElem()).Serialize(os);\n" +
			"            if (_i0 == _n0 - 1) os.WriteSequenceEndKeep(); else os.WriteSequenceEnd();",
		"os.WriteSequenceBeginLazy(_i0); (this.dynobj[_i0] ?? new VecDynobjElem()).Serialize(os);\n" +
			"            if (_i0 == _n0 - 1) os.WriteSequenceEndKeep(); else os.WriteSequenceEnd();",

		// A NATIVE nested row has no frame of its own, so the rule lands on the write.
		"            if (this.rows[_i0].Count != 0 || _i0 == _n0 - 1) {\n" +
			"                os.WriteArrayUnsigned(_i0, this.rows[_i0].ToArray());\n" +
			"            }",
		// A WRAPPER nested row does have one, so it takes the closer instead.
		"            for (int _i1 = 0, _n1 = this.srows[_i0].Count; _i1 < _n1; _i1++) { if ((this.srows[_i0][_i1] ?? \"\") != \"\" || _i1 == _n1 - 1) os.WriteString(_i1, this.srows[_i0][_i1] ?? \"\"); }\n" +
			"            if (_i0 == _n0 - 1) os.WriteSequenceEndKeep(); else os.WriteSequenceEnd();",
	} {
		if !strings.Contains(m, want) {
			t.Errorf("Message.cs missing %q:\n%s", want, m)
		}
	}

	// The superseded shapes: an unconditional keeping closer on every element
	// (sequence elements used to be framed whatever their value), and a leaf omit
	// test with no last-index escape on a count:N array.
	if strings.Contains(m, "Marshal(os); os.WriteSequenceEndKeep();") {
		t.Errorf("a sequence element must not be framed unconditionally:\n%s", m)
	}
	if strings.Contains(m, `if ((this.fixedstr[_i0] ?? "") != "") os.WriteString`) {
		t.Errorf("a count:N leaf element must still keep its last index:\n%s", m)
	}

	// IsDefault follows the writer exactly: the last element is always written, so
	// "no child is written" is "the array is empty" — for every element kind and
	// with or without a count. Anything narrower would omit a field that is on the
	// wire (a [""] or a [{}] is one element, not nothing).
	for _, want := range []string{
		"if (!(this.dynstr.Count == 0)) return false;",
		"if (!(this.dynblob.Count == 0)) return false;",
		"if (!(this.fixedstr.Count == 0)) return false;",
		"if (!(this.fixedobj.Count == 0)) return false;",
		"if (!(this.rows.Count == 0)) return false;",
	} {
		if !strings.Contains(m, want) {
			t.Errorf("IsDefault must test emptiness alone: missing %q:\n%s", want, m)
		}
	}
}

// MESSAGE_SPEC §2: every sequence opens with the lazy begin, so the CLOSER alone
// decides whether a contentless one survives — and where it is chosen from is the
// whole of the element rule. A sequence-typed FIELD (a struct/union field, an
// array wrapper) is decided by the SCHEMA: it always closes with the dropping
// WriteSequenceEnd, so an all-default one is omitted. A wrapper-array ELEMENT (a
// struct element, a nested row) is decided by its position in the VALUE, at run
// time.
func TestCsSequenceFramingClosers(t *testing.T) {
	const src = `
version: 1
messages:
  M:
    payload:
      nested:
        id: 0
        type: struct
        fields:
          a: { id: 0, type: u32 }
      structs:
        id: 1
        type: array
        items:
          type: struct
          fields:
            b: { id: 0, type: u32 }
      names: { id: 2, type: array, items: { type: string, maxlen: 8 } }
      grid:
        id: 3
        type: array
        items:
          type: array
          items: { type: string, maxlen: 8 }
`
	m := buildModule(t, []byte(src), "m.yaml", map[string]any{})
	for _, want := range []string{
		// FIELD: a struct field may vanish whole when every child is at its default.
		"os.WriteSequenceBeginLazy(0); (this.nested ?? new MNested()).Serialize(os); os.WriteSequenceEnd();",
		// FIELD: the wrapper of a struct-element array, closed by the dropping end.
		"        os.WriteSequenceBeginLazy(1);\n" +
			"        for (int _i0 = 0, _n0 = this.structs.Count; _i0 < _n0; _i0++) {\n" +
			"            os.WriteSequenceBeginLazy(_i0); (this.structs[_i0] ?? new MStructsElem()).Serialize(os);\n" +
			"            if (_i0 == _n0 - 1) os.WriteSequenceEndKeep(); else os.WriteSequenceEnd();\n" +
			"        }\n" +
			"        os.WriteSequenceEnd();",
		// FIELD: the wrapper of a string array — an EMPTY array drops the wrapper too
		// (the last element is always written, so a non-empty one never vanishes, §2).
		"        os.WriteSequenceBeginLazy(2);\n" +
			"        for (int _i0 = 0, _n0 = this.names.Count; _i0 < _n0; _i0++) { if ((this.names[_i0] ?? \"\") != \"\" || _i0 == _n0 - 1) os.WriteString(_i0, this.names[_i0] ?? \"\"); }\n" +
			"        os.WriteSequenceEnd();",
		// FIELD: the wrapper of an array-of-array, whose ROWS are elements and so take
		// the positional closer one level in.
		"        os.WriteSequenceBeginLazy(3);\n" +
			"        for (int _i0 = 0, _n0 = this.grid.Count; _i0 < _n0; _i0++) {\n" +
			"            os.WriteSequenceBeginLazy(_i0);\n" +
			"            for (int _i1 = 0, _n1 = this.grid[_i0].Count; _i1 < _n1; _i1++) { if ((this.grid[_i0][_i1] ?? \"\") != \"\" || _i1 == _n1 - 1) os.WriteString(_i1, this.grid[_i0][_i1] ?? \"\"); }\n" +
			"            if (_i0 == _n0 - 1) os.WriteSequenceEndKeep(); else os.WriteSequenceEnd();\n" +
			"        }\n" +
			"        os.WriteSequenceEnd();",
	} {
		if !strings.Contains(m, want) {
			t.Errorf("Message.cs missing %q:\n%s", want, m)
		}
	}
	// The eager begin is gone from the corelib; emitting it would not compile.
	if strings.Contains(m, "WriteSequenceBegin(") {
		t.Error("eager WriteSequenceBegin must not be emitted; the corelib only has WriteSequenceBeginLazy")
	}
	// Exactly two ELEMENT sites (the struct element, the nested row), each spelling
	// the run-time choice once; and no unconditional keeping closer anywhere.
	if got := strings.Count(m, "if (_i0 == _n0 - 1) os.WriteSequenceEndKeep(); else os.WriteSequenceEnd();"); got != 2 {
		t.Errorf("positional closer count = %d, want 2 (struct element + nested row)", got)
	}
	if got := strings.Count(m, "os.WriteSequenceEndKeep();"); got != 2 {
		t.Errorf("WriteSequenceEndKeep count = %d, want 2 (both inside a positional choice)", got)
	}
}

// A wrapper array's element id IS the array index (§5.1), so an element is PLACED
// at dest[id] after gap-filling from the element default — never appended. That is
// what restores an interior element the sparse rule omitted; appending would
// shorten the array by the size of every gap and would decode a REOPENED id as a
// second element instead of merging into the first (§7.4).
//
// The decoded length is highest present id + 1, exact because the last element is
// never elided. Nothing is filled in beyond it: a schema `count` is a capacity, so
// it bounds the id but never adds an element the wire did not carry.
//
// The matrix/nested-row collectors were the one place still appending id-blind.
// That was unreachable while every row was written; interior sparsity makes an
// interior gap reachable, and an appending collector then shifts every later row
// down by one.
func TestCsWrapperElementsArePlacedByID(t *testing.T) {
	const src = `
version: 1
messages:
  vec:
    payload:
      fixed:   { id: 0, type: array, items: { type: struct, count: 5, fields: { k: { id: 0, type: u32 } } } }
      dynamic: { id: 1, type: array, items: { type: struct, fields: { k: { id: 0, type: u32 } } } }
      fstrs:   { id: 2, type: array, items: { type: string, count: 3, maxlen: 8 } }
      rows:    { id: 3, type: array, items: { type: array, count: 2, items: { type: u32 } } }
      srows:   { id: 4, type: array, items: { type: array, items: { type: string, maxlen: 8 } } }
`
	m := buildModule(t, []byte(src), "m.yaml", map[string]any{})

	for _, want := range []string{
		// struct element: the corelib bounds the id and gap-fills, then generated
		// code latches the id and descends — the element scope then addresses the
		// element the id named, not the last one.
		"case (Root_fixed, _): global::sofab.Seq.ReserveElem(m.@fixed, id, static () => new VecFixedElem(), 5, MaxDynArrayCount); " +
			"_ixRoot_fixed = id; cur = Root_fixed_e; break;",
		"m.@fixed[_ixRoot_fixed].k = (uint)value; break;",
		// a count-less array is placed by id too: its length is highest id + 1.
		"global::sofab.Seq.ReserveElem(m.dynamic, id, static () => new VecDynamicElem(), -1, MaxDynArrayCount); _ixRoot_dynamic = id;",
		// string leaf element: placed, with the gap filled from the element default.
		"case (Root_fstrs, _): global::sofab.Seq.PlaceElem(m.fstrs, id, \"\", _s, 3, MaxDynArrayCount); break;",
		// NATIVE row (the id-blind collector): placed at out[id], bounded by the outer
		// array's count, and the fill then addresses the latched row. The §7.3 kind
		// test fronts both (generator#254): a mis-typed row is skipped whole.
		// The ROW itself is count-less, so its element count also meets the
		// target's finite default cap (§9.5, generator#385) -- a bound on the
		// inner array, distinct from the outer index bound beside it.
		"case (Root_rows, _): if (kind != ArrayKind.Unsigned) break; " +
			"global::sofab.Seq.ReserveRow(m.rows, id, 2, MaxDynArrayCount); " +
			"if (count > MaxDynArrayCount) throw new SofabException(SofabError.LimitExceeded, " +
			"\"Root_rows element: array count above configured limit 65536\"); " +
			"_ixRoot_rows = id; break;",
		// the §7.1 width guard for the u32 element follows afill-- and precedes the
		// store (see TestCsDeclaredWidthIsAValidityBound)
		"case (Root_rows, _): if (afill == 0) break; afill--; if (value > 4294967295) throw new SofabException(SofabError.InvalidMessage, " +
			"\"Root_rows element: value outside declared width u32\"); m.rows[_ixRoot_rows].Add((uint)value); break;",
		// WRAPPER row: same placement, then the descent.
		"case (Root_srows, _): global::sofab.Seq.ReserveRow(m.srows, id, -1, MaxDynArrayCount); " +
			"_ixRoot_srows = id; cur = Root_srows_e; break;",
		"case (Root_srows_e, _): global::sofab.Seq.PlaceElem(m.srows[_ixRoot_srows], id, \"\", _s, -1, MaxDynArrayCount); break;",
	} {
		if !strings.Contains(m, want) {
			t.Errorf("Message.cs missing %q:\n%s", want, m)
		}
	}

	// The defects this replaced: an id-blind append, and "the last element" as the
	// decode target.
	for _, bad := range []string{
		"m.rows.Add(new List<uint>()); break;",
		"m.srows.Add(new List<string>()); cur = Root_srows_e;",
		"m.rows[m.rows.Count - 1]",
		"m.srows[m.srows.Count - 1]",
		"m.@fixed[m.@fixed.Count - 1]",
	} {
		if strings.Contains(m, bad) {
			t.Errorf("a row must not be collected id-blind: %q:\n%s", bad, m)
		}
	}
	// And nothing is filled in past the highest id: `count` adds no elements.
	for _, bad := range []string{
		"while (m.@fixed.Count < 5)",
		"while (m.fstrs.Count < 3)",
		"while (m.rows.Count < 2)",
	} {
		if strings.Contains(m, bad) {
			t.Errorf("a capacity must not default-fill the array: %q:\n%s", bad, m)
		}
	}
}

// TestCsSkippedStringIsNotValidated: a `string` payload the visitor will not
// materialize must be skipped whole — its bytes jumped over, never inspected
// (CORELIB_PLAN §6.4, generator#257 / Crucible F-0038). corelib-cs hands EVERY
// fixlen-string field to the generated String() callback, unknown ids and §7.3
// wire-type contradictions included, so the callback itself is what decides
// whether a payload is read. It used to materialize the payload first and switch on
// (cur, id) second, so a lone continuation byte at an id the scope does not
// declare threw InvalidMessage out of an otherwise valid message.
//
// The fix is order: resolve the destination first and return when nothing
// matches, so no byte is buffered or handed to the shared PayloadAcc.
func TestCsSkippedStringIsNotValidated(t *testing.T) {
	m := buildModule(t, []byte(`
version: 1
messages:
  m:
    payload:
      s:  { id: 0, type: string, maxlen: 16 }
      n:
        id: 1
        type: struct
        fields:
          t: { id: 2, type: string, maxlen: 8 }
      sa: { id: 3, type: array, items: { type: string, count: 4, maxlen: 8 } }
`), "skip.yaml", map[string]any{})
	fn := csMethod(t, m, "    public void String(int id,")

	guardEnd := strings.Index(fn, "default: return;")
	if guardEnd < 0 {
		t.Fatalf("String() missing the §6.4 destination guard:\n%s", fn)
	}
	guard := fn[:guardEnd]
	for _, want := range []string{
		"case (Root, 0):",    // the scalar string
		"case (Root_n, 2):",  // the nested struct's string
		"case (Root_sa, _):", // every id of the string-array row
	} {
		if !strings.Contains(guard, want) {
			t.Errorf("String() missing destination arm %q:\n%s", want, fn)
		}
	}
	// The guard precedes both paths that take the payload -- the inline
	// whole-payload decode and the accumulator -- which is where the buffering
	// and the UTF-8 verdict happen, so a skipped payload is neither validated nor
	// able to leave bytes behind for a later declared field to inherit.
	for _, after := range []string{"Utf8.Decode(", "PayloadAcc()).String("} {
		if i := strings.Index(fn, after); i < 0 || guardEnd > i {
			t.Errorf("String(): the destination guard must precede %q:\n%s", after, fn)
		}
	}
	// No schema-maxlen comparison is emitted in the payload callback at all
	// (#594): the one comparison is FixlenBegin's, at the length word, and its
	// throw ends the decode before String() is entered.
	if strings.Contains(fn, "above schema maxlen") {
		t.Errorf("String(): the schema maxlen must be compared only at the length word:\n%s", fn)
	}
}

// The blob twin of the test above, and the correction of an earlier reading of
// it: the guard was called a string-only concern because UTF-8 is the only thing
// a blob has nothing of. Validation was never all it bought. Without it a blob at
// a (loc, id) this message does not bind still reached PayloadAcc.Blob, which
// sizes a byte[] from the wire `total` and copies the payload in -- and only the
// switch below found no arm and dropped it. A 1 MiB blob at an unknown id cost
// 1 MiB of heap for a field nobody reads: a payload MATERIALIZED where
// MESSAGE_SPEC §7.3 says the bytes are walked over, and storage sized from the
// wire for a value never delivered (CORELIB_PLAN §6.2.1, §6.6, §6.7.2).
//
// It is also what keeps the receiver cap off a skipped blob: the arm that binds
// an id is the arm that names its `_cap`, so an id with no arm is measured
// against nothing at all (CORELIB_PLAN §6.2.1, "a skipped field is never
// capped"). Both halves are asserted below.
func TestCsSkippedBlobIsNotMaterialized(t *testing.T) {
	m := buildModule(t, []byte(`
version: 1
messages:
  m:
    payload:
      b:  { id: 0, type: blob, maxlen: 16 }
      db: { id: 1, type: blob }
      s:  { id: 2, type: string }
      n:
        id: 3
        type: struct
        fields:
          t: { id: 4, type: blob, maxlen: 8 }
      ba: { id: 5, type: array, items: { type: blob, count: 4, maxlen: 8 } }
`), "skipblob.yaml", map[string]any{})
	fn := csMethod(t, m, "    public void Blob(int id,")

	guardEnd := strings.Index(fn, "default: return;")
	if guardEnd < 0 {
		t.Fatalf("Blob() missing the §6.2.1 destination guard:\n%s", fn)
	}
	guard := fn[:guardEnd]
	for _, want := range []string{
		"case (Root, 0):",    // the scalar blob
		"case (Root, 1):",    // the schema-unbounded blob
		"case (Root_n, 4):",  // the nested struct's blob
		"case (Root_ba, _):", // every id of the blob-array row
	} {
		if !strings.Contains(guard, want) {
			t.Errorf("Blob() missing destination arm %q:\n%s", want, fn)
		}
	}
	// The same arms carry whichever bound governs. A schema-bounded blob hands
	// PayloadAcc its own maxlen as the cap, under which the corelib's comparison
	// can no longer fire (§6.2.1 forbids a receiver cap on a field the schema
	// bounds; §6.3 gives the two categories) -- and which FixlenBegin already
	// enforced at the length word. An unbounded one hands over the configured
	// constant.
	for _, want := range []string{
		"case (Root, 0): _cap = 16; break;",
		"case (Root, 1): _cap = MaxDynBlobLen; break;",
		"_b = new byte[total]; Array.Copy(data, chunkOffset, _b, 0, total);",
		"else _b = (pay ??= new PayloadAcc()).Blob(total, offset, data, chunkOffset, chunkLength, _cap);",
	} {
		if !strings.Contains(fn, want) {
			t.Errorf("Blob() missing %q:\n%s", want, fn)
		}
	}
	// The string id is not a blob destination: it gets no arm, so a blob arriving
	// there is skipped by the `default` -- unbuffered, uncopied and uncapped
	// (MESSAGE_SPEC §7.3).
	if strings.Contains(guard, "(Root, 2)") {
		t.Errorf("a string id must not be a blob destination:\n%s", fn)
	}
	// The whole point: nothing is sized from the wire or copied before the gate.
	for _, after := range []string{"new byte[total]", "PayloadAcc()).Blob("} {
		if i := strings.Index(fn, after); i < 0 || guardEnd > i {
			t.Errorf("Blob(): the destination guard must precede %q:\n%s", after, fn)
		}
	}
	// No schema-maxlen comparison in the payload callback (#594) -- the one
	// comparison is FixlenBegin's, at the length word.
	if strings.Contains(fn, "above schema maxlen") {
		t.Errorf("Blob(): the schema maxlen must be compared only at the length word:\n%s", fn)
	}
}

// The blob twin of the string-free schema test: a message that declares NO blob
// still gets the callback (the Visitor interface declares it, and the corelib
// still routes blob fields at unknown ids to it), but every blob reaching it is
// skipped by definition -- so the body must be empty. A guard whose every arm
// returns is the same thing said longer.
func TestCsBlobFreeSchemaNeverCopiesABlob(t *testing.T) {
	m := buildModule(t, []byte(`
version: 1
messages:
  m:
    payload:
      a: { id: 0, type: u32 }
      s: { id: 1, type: string, maxlen: 8 }
`), "noblob.yaml", map[string]any{})
	fn := csMethod(t, m, "    public void Blob(int id,")
	for _, forbidden := range []string{"pay.Blob(", "switch ((cur, id))", "byte[] _b"} {
		if strings.Contains(fn, forbidden) {
			t.Errorf("a blob-free schema must not %q in Blob():\n%s", forbidden, fn)
		}
	}
	if !strings.Contains(m, "public void Blob(int id,") {
		t.Errorf("Blob() must still be declared -- Visitor requires it:\n%s", m)
	}
}

// A message that declares NO string still gets a String callback (the Visitor
// interface declares it, and the corelib still routes string fields at unknown
// ids to it), but every string reaching it is skipped by definition — so the
// body must be empty. Decoding one only to drop it is the same §6.4 violation,
// just with every string skipped instead of some.
func TestCsStringFreeSchemaNeverDecodesAString(t *testing.T) {
	m := buildModule(t, []byte(`
version: 1
messages:
  m:
    payload:
      a: { id: 0, type: u32 }
      b: { id: 1, type: blob, maxlen: 8 }
`), "nostr.yaml", map[string]any{})
	fn := csMethod(t, m, "    public void String(int id,")
	for _, forbidden := range []string{"pay.", "switch ((cur, id))", "string _s"} {
		if strings.Contains(fn, forbidden) {
			t.Errorf("a string-free schema must not %q in String():\n%s", forbidden, fn)
		}
	}
	// The callback is still declared -- the Visitor interface requires it.
	if !strings.Contains(m, "public void String(int id,") {
		t.Errorf("String() must still be declared:\n%s", m)
	}
}

// The same rule for every other (cur, id) dispatch: a callback whose kind the
// schema never declares has no arm, and `switch ((cur, id)) { }` is CS1522
// "Empty switch block". The primitive-array fill index `ai` and the fill counter
// `afill` are read only by a native-array fill, so a schema without one must not
// declare them either (CS0414 "assigned but its value is never used"). Both are
// warnings in a file the consumer must not edit, and fatal under
// TreatWarningsAsErrors. The u32-only schema is the reproduction of the defect;
// the string-only one covers Unsigned, the one callback it leaves empty.
func TestCsKindFreeCallbacksEmitNoEmptySwitch(t *testing.T) {
	cases := []struct {
		name, src string
		empty     []string // callbacks with no arm: no switch at all
		kept      []string // callbacks that do dispatch
	}{
		{"only u32", `
version: 1
messages:
  m:
    payload:
      a: { id: 0, type: u32 }
`, []string{"Signed(int id,", "Fp32(int id,", "Fp64(int id,", "ArrayBegin(int id,"}, []string{"Unsigned(int id,"}},
		{"only string", `
version: 1
messages:
  m:
    payload:
      s: { id: 0, type: string, maxlen: 8 }
`, []string{"Unsigned(int id,", "Signed(int id,", "Fp32(int id,", "Fp64(int id,", "ArrayBegin(int id,"}, []string{"String(int id,"}},
	}
	for _, c := range cases {
		m := buildModule(t, []byte(c.src), "kindfree.yaml", map[string]any{})
		for _, cb := range c.empty {
			fn := csMethod(t, m, "    public void "+cb)
			if strings.Contains(fn, "switch ((cur, id))") {
				t.Errorf("%s: %s declares no arm and must not open a (cur, id) switch (CS1522):\n%s", c.name, cb, fn)
			}
		}
		for _, cb := range c.kept {
			if fn := csMethod(t, m, "    public void "+cb); !strings.Contains(fn, "switch ((cur, id))") {
				t.Errorf("%s: %s binds a field and must dispatch on (cur, id):\n%s", c.name, cb, fn)
			}
		}
		for _, forbidden := range []string{"private int ai ", "ai = 0;", "[ai++]", "afill"} {
			if strings.Contains(m, forbidden) {
				t.Errorf("%s: a schema without a native array must not emit %q (CS0414):\n%s", c.name, forbidden, m)
			}
		}
	}
}

// The gate must not overshoot: a native array still gets its fill index, its
// fill counter and the ArrayBegin dispatch that allocates it. A native INNER row
// (array of fp32 arrays) needs afill but has no primitive-array field, so it
// must not get `ai`.
func TestCsNativeArrayKeepsFillState(t *testing.T) {
	m := buildModule(t, []byte(`
version: 1
messages:
  m:
    payload:
      a: { id: 0, type: array, items: { type: u32, count: 4 } }
`), "prim.yaml", map[string]any{})
	for _, want := range []string{"private int ai ", "        ai = 0;", "[ai++]", "private int afill ", "afill = kind switch"} {
		if !strings.Contains(m, want) {
			t.Errorf("a primitive array field needs %q:\n%s", want, m)
		}
	}
	if fn := csMethod(t, m, "    public void ArrayBegin(int id,"); !strings.Contains(fn, "switch ((cur, id))") {
		t.Errorf("ArrayBegin must dispatch to allocate the array:\n%s", fn)
	}

	m = buildModule(t, []byte(`
version: 1
messages:
  m:
    payload:
      mx: { id: 0, type: array, items: { type: array, count: 3, items: { type: fp32, count: 2 } } }
`), "rows.yaml", map[string]any{})
	for _, want := range []string{"private int afill ", "afill = kind switch", "if (afill == 0) break;"} {
		if !strings.Contains(m, want) {
			t.Errorf("a native inner row needs %q:\n%s", want, m)
		}
	}
	if strings.Contains(m, "private int ai ") || strings.Contains(m, "[ai++]") {
		t.Errorf("a native inner row is a List, not a primitive array: no `ai`:\n%s", m)
	}

	// A boolean array is a List<bool>, filled natively but not a primitive
	// array: it keeps afill (and its Unsigned fill arm) without ai.
	m = buildModule(t, []byte(`
version: 1
messages:
  m:
    payload:
      b: { id: 0, type: array, items: { type: boolean, count: 4 } }
`), "bools.yaml", map[string]any{})
	for _, want := range []string{"private int afill ", "afill = kind switch", "(Root, 0) => count,", "case (Root, 0): if (afill == 0) break; afill--; m.b.Add(value != 0);"} {
		if !strings.Contains(m, want) {
			t.Errorf("a boolean array field needs %q:\n%s", want, m)
		}
	}
	if strings.Contains(m, "private int ai ") || strings.Contains(m, "[ai++]") {
		t.Errorf("a boolean array is a List, not a primitive array: no `ai`:\n%s", m)
	}
}

// csMethod returns the generated method body starting at `head` up to the next
// top-level `    public ` line, so an ordering assertion inside one callback
// cannot accidentally match text from a neighbouring one.
func csMethod(t *testing.T, src, head string) string {
	t.Helper()
	i := strings.Index(src, head)
	if i < 0 {
		t.Fatalf("no %q in:\n%s", head, src)
	}
	rest := src[i+len(head):]
	if j := strings.Index(rest, "\n    public "); j >= 0 {
		return src[i : i+len(head)+j]
	}
	return src[i:]
}

// MESSAGE_SPEC §7.1 + documentation#32 (issue #266, Crucible F-0033 / G-0026):
// the declared integer width is a normative VALIDITY bound. An out-of-range value
// is InvalidMessage — never masked by the `(byte)value` cast, never kept.
//
// Unlike Java, C# needs no negative-value term: Unsigned delivers a ulong, so the
// comparison is already unsigned.
func TestCsDeclaredWidthIsAValidityBound(t *testing.T) {
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
	got := buildModule(t, []byte(src), "w.yaml", map[string]any{})
	for _, want := range []string{
		`case (Root, 0): if (value > 255) throw new SofabException(SofabError.InvalidMessage, "a_u8: value outside declared width u8"); m.a_u8 = (byte)value; break;`,
		`case (Root, 2): if (value > 4294967295) throw new SofabException(SofabError.InvalidMessage, "c_u32: value outside declared width u32"); m.c_u32 = (uint)value; break;`,
		`case (Root, 4): if (value < -128 || value > 127) throw new SofabException(SofabError.InvalidMessage, "e_i8: value outside declared width i8"); m.e_i8 = (sbyte)value; break;`,
		`case (Root, 6): if (value < -2147483648 || value > 2147483647) throw new SofabException(SofabError.InvalidMessage, "g_i32: value outside declared width i32"); m.g_i32 = (int)value; break;`,
		// Array elements: the guard follows the fill guard (§7.3 skip stays a skip).
		`case (Root, 8): if (afill == 0) break; afill--; if (value > 255) throw new SofabException(SofabError.InvalidMessage, "arr_u8 element: value outside declared width u8");`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("Message.cs missing width guard %q:\n%s", want, got)
		}
	}
	for _, want := range []string{
		"case (Root, 3): m.d_u64 = (ulong)value; break;",
		"case (Root, 7): m.h_i64 = (long)value; break;",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("Message.cs: a 64-bit destination must store unguarded (%q):\n%s", want, got)
		}
	}
}

// generator#268 (Crucible F-0044) and #272 (F-0047): SequenceBegin's (cur, id)
// switch had no default arm, so a sequence the schema does not declare at this
// position was ENTERED and its children bound into the ENCLOSING scope — an
// unknown sequence id carrying a child id 3 set the ROOT's field 3 (#268), and a
// sequence opened at a string-array element position bound its string as that
// element (#272). One missing default covers both: _DEAD matches no callback
// case, so the whole subtree is discarded and the stack restores the live scope.
func TestCsUnknownSequenceIsSkippedWhole(t *testing.T) {
	const src = `
version: 1
messages:
  Probe:
    payload:
      a:            { id: 3, type: i16 }
      known:        { id: 10, type: struct, fields: { k: { id: 0, type: u32 } } }
      string_array: { id: 200, type: array, items: { type: string, count: 5, maxlen: 64 } }
`
	got := buildModule(t, []byte(src), "probe.yaml", map[string]any{})
	for _, want := range []string{
		"private const int _DEAD = -1;",
		"case (Root, 10): cur = Root_known; break;",
		"default: cur = _DEAD; break;",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("Message.cs missing %q:\n%s", want, got)
		}
	}
}

// A schema bound that the fixlen LENGTH WORD already decides must be latched at
// that word, not once payload bytes arrive (CORELIB_PLAN §5.2, generator#267).
// The guards lived in the PAYLOAD callback, which never fires for a message
// truncated immediately after the length word -- so that reported INCOMPLETE
// where the same bytes read whole are INVALID.
func TestCsharpFixlenBeginLatchesBoundsAtTheLengthWord(t *testing.T) {
	m := buildModule(t, []byte(`version: 1
messages:
  m:
    payload:
      s:  { id: 0, type: string, maxlen: 8 }
      b:  { id: 1, type: blob, maxlen: 4 }
      sa: { id: 2, type: array, items: { type: string, count: 3, maxlen: 6 } }
`), "b.yaml", map[string]any{})

	if !strings.Contains(m, "public void FixlenBegin(int id, FixlenType subtype, int total)") {
		t.Fatal("no FixlenBegin implementation")
	}
	if !strings.Contains(m, "if (subtype == FixlenType.String) {") ||
		!strings.Contains(m, "case (Root, 0): if (total > 8) throw") {
		t.Error("a scalar string maxlen must be latched under FixlenType.String")
	}
	if !strings.Contains(m, "if (subtype == FixlenType.Blob) {") ||
		!strings.Contains(m, "case (Root, 1): if (total > 4) throw") {
		t.Error("a scalar blob maxlen must be latched under FixlenType.Blob")
	}
	// Over-index first, then the element maxlen -- an element that is not this
	// array's element must not be measured against its bound.
	if !strings.Contains(m, "case (Root_sa, _): global::sofab.Seq.CheckIndex(id, 3, MaxDynArrayCount); if (total > 6) throw") {
		t.Error("a wrapper element must latch over-index then element maxlen")
	}
	// Exactly one comparison per bound (#594): the length word's. The payload
	// callback restates none of them.
	if n := strings.Count(m, "total > 8"); n != 1 {
		t.Errorf("a scalar maxlen must be compared exactly once, got %d", n)
	}
	if n := strings.Count(m, "total > 6"); n != 1 {
		t.Errorf("an element maxlen must be compared exactly once, got %d", n)
	}
}

// TestCsAShapeCheckThenAllocate: the C# half of ARCHITECTURE §9.5's shape A
// (generator#386) -- an array whose size arrives before its payload is bounded at
// the count header and then allocated at exactly that count, once, replacing the
// #96/#98 reserve-at-Seq.ArrayInitCap-and-grow shape the config caps made
// unnecessary.
//
// A native matrix ROW is a List<T> here rather than a T[], so nothing is
// allocated from its count either way -- but its count still needs a VERDICT,
// which it did not have: fr.cap bounds the row's id, never how many elements the
// row claims (§7.1).
func TestCsAShapeCheckThenAllocate(t *testing.T) {
	const src = `
version: 1
messages:
  M:
    payload:
      dyn: { id: 0, type: array, items: { type: u32 } }
      bnd: { id: 1, type: array, items: { type: u32, count: 8 } }
      fps: { id: 2, type: array, items: { type: fp32 } }
      mat: { id: 3, type: array, items: { type: array, count: 3, items: { type: u32, count: 4 } } }
`
	m := buildModule(t, []byte(src), "m.yaml", map[string]any{})

	for _, want := range []string{
		`if (count > MaxDynArrayCount) throw new SofabException(SofabError.LimitExceeded, "dyn: array count above configured limit 65536"); m.dyn = new uint[count];`,
		`if (count > 8) throw new SofabException(SofabError.InvalidMessage, "bnd: array count above schema capacity 8"); m.bnd = new uint[count];`,
		`m.fps = new float[count];`,
		// The row's id (the corelib's compare), then the row's own element count.
		`global::sofab.Seq.ReserveRow(m.mat, id, 3, MaxDynArrayCount); if (count > 4) throw new SofabException(SofabError.InvalidMessage, "Root_mat element: array count above schema capacity 4");`,
		// A plain indexed store: the destination is already exactly `count` long.
		`m.dyn[ai++] = (uint)value;`,
	} {
		if !strings.Contains(m, want) {
			t.Errorf("Message.cs missing %q:\n%s", want, m)
		}
	}
	for _, gone := range []string{"Seq.EnsureCap", "Seq.ArrayInitCap", "Math.Min(count", "acap"} {
		if strings.Contains(m, gone) {
			t.Errorf("Message.cs must not still grow into an array (%q):\n%s", gone, m)
		}
	}
}

// TestCsWrapperIndexCap: a DYNAMIC wrapper array's element index is bounded by
// the receiver cap, checked by the corelib before the List grows (ARCHITECTURE
// §9.5, generator#387, #587). See the Java twin for why the INDEX and not the element count:
// gap filling makes the array's length its highest present id, so two delivered
// elements can be an arbitrarily large List.
func TestCsWrapperIndexCap(t *testing.T) {
	const src = `
version: 1
messages:
  M:
    payload:
      dstrs: { id: 0, type: array, items: { type: string } }
      dblbs: { id: 1, type: array, items: { type: blob } }
      dobjs: { id: 2, type: array, items: { type: struct, fields: { x: { id: 0, type: u32 } } } }
      dmat:  { id: 3, type: array, items: { type: array, items: { type: u32 } } }
      bstrs: { id: 4, type: array, items: { type: string, count: 4 } }
`
	m := buildModule(t, []byte(src), "m.yaml", map[string]any{})

	for _, want := range []string{
		"private const long MaxDynArrayCount = 65536;",
		`global::sofab.Seq.PlaceElem(m.dstrs, id, "", _s, -1, MaxDynArrayCount);`,
		`global::sofab.Seq.PlaceElem(m.dblbs, id, Array.Empty<byte>(), _b, -1, MaxDynArrayCount);`,
		`global::sofab.Seq.ReserveElem(m.dobjs, id, static () => new `,
		`global::sofab.Seq.ReserveRow(m.dmat, id, -1, MaxDynArrayCount);`,
	} {
		if !strings.Contains(m, want) {
			t.Errorf("Message.cs missing wrapper index cap %q:\n%s", want, m)
		}
	}
	// A count:N array hands the corelib its schema count; the corelib then
	// compares that and never the receiver cap passed beside it (§6.2.1).
	if !strings.Contains(m, `global::sofab.Seq.PlaceElem(m.bstrs, id, "", _s, 4, MaxDynArrayCount);`) {
		t.Errorf("a count:N wrapper array must hand its schema count to the corelib:\n%s", m)
	}
	if n := strings.Count(m, "if (id >= "); n != 0 {
		t.Errorf("generated code must not compare an element index itself (%d sites):\n%s", n, m)
	}
}

// The RECEIVER CAP is latched at that same length word, and with its own
// category (CORELIB_PLAN §6.2.1 "Enforcement point", ARCHITECTURE §9.5).
//
// The schema-maxlen half above was already latched there. The cap half was not:
// it travelled only as PayloadAcc.String/.Blob's `cap` argument, and those fire
// once a payload byte exists. So `02 a2 06` -- a length word declaring 100 bytes
// on a field capped at 8, then end of input -- reached no callback and answered
// Incomplete, which §6.3 makes the wrong category (the refusal is terminal) and
// which invites a streaming caller to feed more of a stream this receiver has
// already refused.
//
// The comparison is still the corelib's -- PayloadAcc.CheckStringLength is what
// PayloadAcc.String runs at the top of itself -- so this is one implementation of
// the rule applied at two points, not a second copy in the generated layer.
func TestCsharpFixlenBeginLatchesTheReceiverCapAtTheLengthWord(t *testing.T) {
	m := buildModule(t, []byte(`version: 1
messages:
  m:
    payload:
      ds: { id: 0, type: string }
      bs: { id: 1, type: string, maxlen: 32 }
      db: { id: 2, type: blob }
      sa: { id: 3, type: array, items: { type: string } }
`), "b.yaml", map[string]any{"max_dyn_string_len": 8, "max_dyn_blob_len": 8, "max_dyn_array_count": 4})

	fx := m[strings.Index(m, "public void FixlenBegin("):strings.Index(m, "public void String(")]
	if fx == "" {
		t.Fatal("no FixlenBegin implementation")
	}
	for _, want := range []string{
		// A schema-unbounded string and blob: the corelib's own check, at the header.
		"case (Root, 0): PayloadAcc.CheckStringLength(total, MaxDynStringLen); break;",
		"case (Root, 2): PayloadAcc.CheckBlobLength(total, MaxDynBlobLen); break;",
		// A schema-bounded one keeps InvalidMessage and its own number: §6.2.1
		// forbids the cap on a field the schema bounds, even a maxlen above the cap.
		`case (Root, 1): if (total > 32) throw new SofabException(SofabError.InvalidMessage, "bs: string length above schema maxlen 32"); break;`,
		// A schema-unbounded wrapper element carries both its array's index cap
		// and its own length cap, over-index first: an element that is not this
		// array's element at all must not have its length measured here.
		`case (Root_sa, _): global::sofab.Seq.CheckIndex(id, -1, MaxDynArrayCount); PayloadAcc.CheckStringLength(total, MaxDynStringLen); break;`,
		// ...all of it behind the §7.3 declared-subtype gate.
		"if (subtype == FixlenType.String) {",
		"if (subtype == FixlenType.Blob) {",
	} {
		if !strings.Contains(fx, want) {
			t.Errorf("FixlenBegin missing %q\ngot:\n%s", want, fx)
		}
	}
	// The cap is never re-implemented here: a bare comparison against the constant
	// would be the second implementation §6.2.1 forbids.
	if strings.Contains(fx, "total > MaxDynStringLen") || strings.Contains(fx, "total > MaxDynBlobLen") {
		t.Error("the cap comparison belongs to the corelib call, not to a guard emitted beside it (§6.2.1)")
	}
	// A message whose every string the schema bounds meets no cap anywhere: the
	// exclusivity rule leaves nothing for the cap to govern, and the constant is
	// not even declared -- naming it would not compile.
	bounded := buildModule(t, []byte(`version: 1
messages:
  m:
    payload:
      s: { id: 0, type: string, maxlen: 8 }
      b: { id: 1, type: blob, maxlen: 8 }
`), "b.yaml", map[string]any{"max_dyn_string_len": 8, "max_dyn_blob_len": 8})
	if strings.Contains(bounded, "CheckStringLength") || strings.Contains(bounded, "CheckBlobLength") {
		t.Error("a schema-bounded field must not meet the receiver cap (§6.2.1)")
	}
}

// TestCsCapGuardsSitBehindTheKindTest: a §7.3-skipped field must never be capped
// (CORELIB_PLAN §6.2.1, generator#410). A limit bounds an ALLOCATION, and a field
// the visitor skips allocates nothing, so a decode that steps over an over-cap
// field it was never going to read stays Complete.
//
// §7.3 skips two shapes and this backend defeats both structurally rather than by
// testing for them: ArrayBegin switches on `(location, id)`, so an unknown id
// reaches no arm, and every arm opens with `if (kind != ArrayKind.X) break;`, so
// an array whose wire kind contradicts the declared element type leaves the arm
// with the skip counter still armed. The cap sits inside that arm, behind both.
//
// TestCsDecodeLimits pins the same ordering, but by spelling out the two arms its
// own schema happens to produce. This states it as a PROPERTY instead: every count
// guard the backend emits, for every schema, sits inside a keyed arm and behind
// that arm's kind test. That is what survives a new field shape, and it is what
// catches the cap being hoisted OUT of the switch -- where there is no enumerated
// line left to miss it, and where the cap would reach both shapes §7.3 requires to
// be skipped while still firing correctly on a well-typed over-cap array.
// tests/conformance/csharp/run.sh decodes the bytes end to end; this pins the
// shape that keeps them decoding.
func TestCsCapGuardsSitBehindTheKindTest(t *testing.T) {
	const src = `
version: 1
messages:
  M:
    payload:
      da:  { id: 0, type: array, items: { type: u32 } }
      ba:  { id: 1, type: array, items: { type: u32, count: 4 } }
      mat: { id: 2, type: array, items: { type: array, items: { type: u32 } } }
`
	m := buildModule(t, []byte(src), "m.yaml", map[string]any{"max_dyn_array_count": 8})

	// The skip counter is armed BEFORE the guarded switch, and its DEFAULT is the
	// wire count: every (kind, location, id) triple this message does not declare
	// drops exactly its own elements and leaves every declared field untouched.
	arm := strings.Index(m, "askip = kind switch {")
	if arm < 0 {
		t.Fatalf("ArrayBegin must arm a skip counter keyed by (kind, cur, id):\n%s", m)
	}
	if !strings.Contains(m[arm:], "_ => count,") {
		t.Errorf("the skip counter must default to the wire count (§7.3):\n%s", m)
	}
	if g := strings.Index(m, "if (count > "); g >= 0 && g < arm {
		t.Errorf("a count guard ahead of the skip arming -- a skipped field would be capped:\n%s", m)
	}

	// Every count comparison -- the cap and the schema bound alike -- is the body
	// of a `case (<location>, <id>):` arm and sits behind that arm's ArrayKind
	// test. A guard that is not one caps whatever the callback was handed, skipped
	// or not.
	seen := 0
	for _, line := range strings.Split(m, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.Contains(trimmed, "if (count > ") || strings.HasPrefix(trimmed, "//") {
			continue
		}
		seen++
		if !strings.HasPrefix(trimmed, "case (") {
			t.Errorf("an array count guard outside a keyed ArrayBegin arm:\n%s", trimmed)
			continue
		}
		kind := strings.Index(trimmed, "if (kind != ArrayKind.")
		if kind < 0 || kind > strings.Index(trimmed, "if (count > ") {
			t.Errorf("an array count guard ahead of (or without) the §7.3 kind test:\n%s", trimmed)
		}
	}
	// da's cap, ba's schema bound, and the matrix row's own element count.
	if seen < 3 {
		t.Fatalf("expected a count guard on each of da, ba and the mat row, found %d:\n%s", seen, m)
	}
}

// TestCsDecoderAsksTheStreamForItsVerdict pins issue #541: the generated Decoder
// remembers NOTHING about the outcome. The stream already holds it — a refusal is
// terminal (CORELIB_PLAN §5.2 for malformed bytes, §6.3 for a receiver limit) and
// the corelib latches it, re-throwing the very code it was refused with from every
// later call. #461 had this layer keep a second copy in `_st`, with a catch that
// mapped a refusal onto it; that copy could only restate what the stream held, and
// it flattened LimitExceeded into an Incomplete that says something untrue about
// the wire.
//
// Finish therefore ASKS, with a zero-length feed: refused streams re-throw before
// looking at a byte, and an unrefused one answers with the outcome for everything
// fed so far.
func TestCsDecoderAsksTheStreamForItsVerdict(t *testing.T) {
	m := exampleModule(t)
	for _, want := range []string{
		// Feed forwards and nothing more: no assignment, no catch.
		"public DecodeStatus Feed(byte[] chunk) => Feed(chunk, 0, chunk.Length);",
		"        public DecodeStatus Feed(byte[] chunk, int off, int len) =>",
		"            _is.Feed(chunk, off, len, _v);",
		// Finish asks the stream and judges what it answers.
		"            var st = _is.Feed(System.Array.Empty<byte>(), 0, 0, _v);",
		"            if (st != DecodeStatus.Complete) {",
		"                    $\"Myfirstmessage: stream ended mid-field ({st})\");",
	} {
		if !strings.Contains(m, want) {
			t.Errorf("Message.cs missing %q (generator#541):\n%s", want, m)
		}
	}
	// Nothing remembers a status, and nothing maps a refusal onto one. Each of
	// these is a way the deleted latch could come back: the field itself, the
	// catch that wrote it, either mapping arm, or the accessor that read it.
	for _, gone := range []string{
		"_st",
		"catch (SofabException e) {",
		"_st = DecodeStatus.Invalid;",
		"_st = DecodeStatus.Incomplete;",
		"public DecodeStatus Status",
	} {
		if strings.Contains(m, gone) {
			t.Errorf("Message.cs still carries the removed status latch %q (generator#541):\n%s", gone, m)
		}
	}
	// The accessor is gone from the corelib; asking the stream a second time
	// must not come back in any form (generator#461).
	if strings.Contains(m, "_is.Status") {
		t.Errorf("Message.cs still reads the removed IStream.Status (generator#461):\n%s", m)
	}
}

// widthSixSrc declares an `enum` and a `bitfield` at all SIX positions a value
// can land in, and both definitions are GAPPED on purpose: the enum declares
// {0, 1, 2, 10}, so 5 sits inside the implied width and is not a constant, and
// the bitfield declares positions 0, 1 and 3, so 4 sets a bit no flag declares.
// Both of those values are VALID under the width rule and were INVALID under the
// withdrawn closed-set one, which is what makes a gapped definition the shape
// that tells the two rules apart.
const widthSixSrc = `
version: 1
messages:
  Closed:
    payload:
      en:  { id: 0, type: enum, enum: { A: 0, B: 1, C: 2, Z: 10 } }
      bf:  { id: 1, type: bitfield, bits: { A: { pos: 0 }, B: { pos: 1 }, D: { pos: 3 } } }
      ea:  { id: 2, type: array, items: { type: enum, count: 4, enum: { A: 0, B: 1, C: 2, Z: 10 } } }
      bfa: { id: 3, type: array, items: { type: bitfield, count: 4, bits: { A: { pos: 0 }, B: { pos: 1 }, D: { pos: 3 } } } }
      st:
        id: 4
        type: struct
        fields:
          se:  { id: 0, type: enum, enum: { A: 0, B: 1, C: 2, Z: 10 } }
          sbf: { id: 1, type: bitfield, bits: { A: { pos: 0 }, B: { pos: 1 }, D: { pos: 3 } } }
      sa:
        id: 5
        type: array
        items:
          type: struct
          count: 2
          fields:
            se:  { id: 0, type: enum, enum: { A: 0, B: 1, C: 2, Z: 10 } }
            sbf: { id: 1, type: bitfield, bits: { A: { pos: 0 }, B: { pos: 1 }, D: { pos: 3 } } }
      un:
        id: 6
        type: union
        default_id: 0
        oneof:
          ue:  { id: 0, type: enum, enum: { A: 0, B: 1, C: 2, Z: 10 } }
          ubf: { id: 1, type: bitfield, bits: { A: { pos: 0 }, B: { pos: 1 }, D: { pos: 3 } } }
      mat: { id: 7, type: array, items: { type: array, count: 2, items: { type: enum, count: 3, enum: { A: 0, B: 1, C: 2, Z: 10 } } } }
      mbf: { id: 8, type: array, items: { type: array, count: 2, items: { type: bitfield, count: 3, bits: { A: { pos: 0 }, B: { pos: 1 }, D: { pos: 3 } } } } }
`

// MESSAGE_SPEC §1 binds an `enum` to the width of the smallest SIGNED type
// holding every declared constant and a `bitfield` to the width of the smallest
// UNSIGNED type holding its highest declared `pos`. Here that is i8 (−128..127)
// for {0, 1, 2, 10} and u8 (0..255) for positions 0, 1 and 3.
//
// C# stores both at exactly that width — `sbyte` and `byte` here — which is why
// the comparison MUST run on the raw accumulator, ahead of the narrowing cast:
// before #516 there was no comparison at all, so 5 into the gapped enum and 4
// into the three-flag bitfield were kept and 256 came back 0, the decode
// reporting success in every case.
//
// All six positions are pinned by name — scalar, native array element, struct
// member, struct-array element member, union member, matrix row element — for
// both kinds. Four of them share one emitted arm per kind, which is exactly why
// "the arm is shared" is not worth trusting after the next refactor.
func TestCsEnumAndBitfieldWidthBoundAtEverySixPositions(t *testing.T) {
	m := buildModule(t, []byte(widthSixSrc), "width.yaml", map[string]any{})
	const enRej = `if (value < -128 || value > 127) throw new SofabException(SofabError.InvalidMessage, `
	const bfRej = `if (value > 255) throw new SofabException(SofabError.InvalidMessage, `
	const fill = "if (afill == 0) break; afill--; "
	for _, want := range []string{
		// 1. scalar
		`case (Root, 0): ` + enRej + `"en: value outside declared enum width"); m.en = (ClosedEn)value; break;`,
		`case (Root, 1): ` + bfRej + `"bf: value outside declared bitfield width"); m.bf = (ClosedBf)value; break;`,
		// 2. native array element — the guard sits BEHIND fillGuard, never in
		// front of it: a bare scalar delivered at an array id is a §7.3 skip, and
		// rejecting it ahead of the fill check would turn that skip into a
		// spurious INVALID.
		`case (Root, 2): ` + fill + enRej + `"ea element: value outside declared enum width"); m.ea[ai++] = (sbyte)value; break;`,
		`case (Root, 3): ` + fill + bfRej + `"bfa element: value outside declared bitfield width"); m.bfa[ai++] = (byte)value; break;`,
		// 3. struct member
		`case (Root_st, 0): ` + enRej + `"se: value outside declared enum width"); m.st.se = (ClosedStSe)value; break;`,
		`case (Root_st, 1): ` + bfRej + `"sbf: value outside declared bitfield width"); m.st.sbf = (ClosedStSbf)value; break;`,
		// 4. struct-array element member
		`case (Root_sa_e, 0): ` + enRej + `"se: value outside declared enum width"); m.sa[_ixRoot_sa].se = (ClosedSaElemSe)value; break;`,
		`case (Root_sa_e, 1): ` + bfRej + `"sbf: value outside declared bitfield width"); m.sa[_ixRoot_sa].sbf = (ClosedSaElemSbf)value; break;`,
		// 5. union member
		`case (Root_un, 0): ` + enRej + `"ue: value outside declared enum width"); m.un.ue = (ClosedUnUe)value; break;`,
		`case (Root_un, 1): ` + bfRej + `"ubf: value outside declared bitfield width"); m.un.ubf = (ClosedUnUbf)value; break;`,
		// 6. matrix row element
		`case (Root_mat, _): ` + fill + enRej,
		`case (Root_mbf, _): ` + fill + bfRej,
	} {
		if !strings.Contains(m, want) {
			t.Errorf("Message.cs: an enum/bitfield position stores without its §1 width bound, missing %q:\n%s", want, m)
		}
	}
	// Storage is the declared width itself, so guard and member agree: every
	// value the guard admits is one the member holds verbatim, and every value it
	// refuses is one the cast would otherwise have folded into a representable
	// one.
	// The two ARRAY cells store into `sbyte[]`/`byte[]` — the same width, one
	// level in — so the generated guard is what enforces the bound there too.
	// corelib-cs's IVisitor is flat: every element arrives through
	// Signed/Unsigned in the 64-bit accumulator, with no bulk element offer that
	// could carry the destination's width, so unlike Java there is no corelib
	// half to hand the check to and the guard may never be dropped.
	for _, want := range []string{
		"public enum ClosedEn : sbyte {", "public enum ClosedBf : byte {",
		"public sbyte[] ea = Array.Empty<sbyte>();", "public byte[] bfa = Array.Empty<byte>();",
	} {
		if !strings.Contains(m, want) {
			t.Errorf("Message.cs: the width bound must not widen storage, missing %q:\n%s", want, m)
		}
	}
	// No bare cast may remain on any of the twelve paths. Spelled as the exact
	// pre-#516 stores so an arm that loses its guard in a later refactor fails
	// here rather than only in conformance.
	for _, bad := range []string{
		`case (Root, 0): m.en = (ClosedEn)value;`,
		`case (Root, 1): m.bf = (ClosedBf)value;`,
		`case (Root_st, 0): m.st.se = (ClosedStSe)value;`,
		`case (Root_un, 1): m.un.ubf = (ClosedUnUbf)value;`,
	} {
		if strings.Contains(m, bad) {
			t.Errorf("Message.cs still stores an enum/bitfield through a bare cast (%q):\n%s", bad, m)
		}
	}
}

// An enum/bitfield ARRAY is backed by a primitive array of the width its
// declaration implies (MESSAGE_SPEC §1, generator#516) — `sbyte[]`/`byte[]` for
// the narrow declarations, widening step by step with the constants and the
// positions, and never the named type.
//
// The width is the same one enumBacking/bitfieldBacking already hold the SCALAR
// in, which is what makes the container swap a drop-in: `public enum X : sbyte`
// beside `public sbyte[] xs`, one width per declaration, derived once through
// ir.EnumWidthRange / ir.BitfieldWidthMax. It is also what the List<T> shape
// cost — `Array.ConvertAll(this.xs.ToArray(), _x => (sbyte)_x)` allocated two
// throwaway arrays per field per encode, and the decode appended element by
// element into a list with no capacity reserve where the wire count was already
// in hand.
//
// Every step of both ladders is pinned, because a single narrow case would pass
// on `byte[]` hard-coded. The boolean array is pinned too, from the other side:
// its wire element is a u8 and its member is a bool, so there is no shared width
// and it stays a List<bool>.
func TestCsEnumBitfieldArrayIsPrimitiveAtDeclaredWidth(t *testing.T) {
	src := `
version: 1
messages:
  m:
    payload:
      e8:   { id: 0, type: array, items: { type: enum, count: 4, enum: { A: 0, Z: 127 } } }
      e8n:  { id: 1, type: array, items: { type: enum, count: 4, enum: { A: 0, N: -128 } } }
      e16:  { id: 2, type: array, items: { type: enum, count: 4, enum: { A: 0, Z: 128 } } }
      e32:  { id: 3, type: array, items: { type: enum, count: 4, enum: { A: 0, Z: 32768 } } }
      b8:   { id: 4, type: array, items: { type: bitfield, count: 4, bits: { A: { pos: 0 }, D: { pos: 7 } } } }
      b16:  { id: 5, type: array, items: { type: bitfield, count: 4, bits: { A: { pos: 0 }, D: { pos: 8 } } } }
      b32:  { id: 6, type: array, items: { type: bitfield, count: 4, bits: { A: { pos: 0 }, D: { pos: 16 } } } }
      b64:  { id: 7, type: array, items: { type: bitfield, count: 4, bits: { A: { pos: 0 }, D: { pos: 32 } } } }
      bl:   { id: 8, type: array, items: { type: boolean, count: 4 } }
      ed:   { id: 9, type: array, items: { type: enum, count: 4, enum: { A: 0, N: -200, Z: 200 } }, default: [-200, 200] }
      mat:  { id: 10, type: array, items: { type: array, count: 2, items: { type: enum, count: 3, enum: { A: 0, Z: 10 } } } }
`
	m := buildModule(t, []byte(src), "primwidth.yaml", map[string]any{})
	for _, want := range []string{
		// The element type is the declared width, and the named type's storage is
		// the same width: the two are one derivation, so they move together.
		"public sbyte[] e8 = Array.Empty<sbyte>();", "public enum ME8Elem : sbyte {",
		"public sbyte[] e8n = Array.Empty<sbyte>();", "public enum ME8nElem : sbyte {",
		"public short[] e16 = Array.Empty<short>();", "public enum ME16Elem : short {",
		"public int[] e32 = Array.Empty<int>();", "public enum ME32Elem : int {",
		"public byte[] b8 = Array.Empty<byte>();", "public enum MB8Elem : byte {",
		"public ushort[] b16 = Array.Empty<ushort>();", "public enum MB16Elem : ushort {",
		"public uint[] b32 = Array.Empty<uint>();", "public enum MB32Elem : uint {",
		"public ulong[] b64 = Array.Empty<ulong>();", "public enum MB64Elem : ulong {",
		// The boolean array keeps the List: no width is shared with its member.
		"public List<bool> bl = new(4);",
		// A declared default is the bare number at that width — no `(MEdElem)`
		// cast in front of it — and the omit-compare static matches it exactly.
		"public short[] ed = new short[]{-200, 200};",
		"private static readonly short[] _arrdef_ed = new short[]{-200, 200};",
		// Encode passes the field straight to the OStream overload.
		"os.WriteArraySigned(0, this.e8);",
		"os.WriteArrayUnsigned(7, this.b64);",
		// Decode allocates once, at the wire count the schema bound just admitted,
		// and fills by index behind the §1 guard.
		`case (Root, 0): if (kind != ArrayKind.Signed) break; if (count > 4) throw new SofabException(SofabError.InvalidMessage, "e8: array count above schema capacity 4"); m.e8 = new sbyte[count]; break;`,
		`case (Root, 5): if (kind != ArrayKind.Unsigned) break; if (count > 4) throw new SofabException(SofabError.InvalidMessage, "b16: array count above schema capacity 4"); m.b16 = new ushort[count]; break;`,
		`case (Root, 2): if (afill == 0) break; afill--; if (value < -32768 || value > 32767) throw new SofabException(SofabError.InvalidMessage, "e16 element: value outside declared enum width"); m.e16[ai++] = (short)value; break;`,
		`case (Root, 6): if (afill == 0) break; afill--; if (value > 4294967295) throw new SofabException(SofabError.InvalidMessage, "b32 element: value outside declared bitfield width"); m.b32[ai++] = (uint)value; break;`,
		// A matrix ROW is unchanged: the corelib hands its elements over one at a
		// time into a List that grows, so it keeps the named type and the bridge.
		"public List<List<MMatElemElem>> mat = new(2);",
		"os.WriteArraySigned(_i0, Array.ConvertAll(this.mat[_i0].ToArray(), _x => (sbyte)_x));",
	} {
		if !strings.Contains(m, want) {
			t.Errorf("Message.cs missing %q:\n%s", want, m)
		}
	}
	for _, bad := range []string{
		// The List shape and both of its allocations, gone from the direct fields.
		"public List<ME8Elem>", "public List<MB8Elem>", "public List<MEdElem>",
		"Array.ConvertAll(this.e8", "Array.ConvertAll(this.b8", "Array.ConvertAll(this.ed",
		"m.e8.Add(", "m.b8.Clear();", "m.b64.Add(",
	} {
		if strings.Contains(m, bad) {
			t.Errorf("Message.cs still lowers an enum/bitfield array through List<T> (%q):\n%s", bad, m)
		}
	}
}

// The elisions under the width rule: a guard is emitted only where the implied
// width is NARROWER than the 64-bit accumulator the value arrives in. A bitfield
// whose highest declared position is 63 implies u64, and an enum needing more
// than i32 implies i64 — in both cases nothing reachable can breach the bound
// and the clause would be dead code.
//
// Note what is NOT an elision any more: whether an enum's constants are
// contiguous no longer matters at all. The width is derived from the extremes, so
// {0,1,2} and {0,1,2,10} produce the identical i8 guard, and the membership chain
// a gapped set used to need is gone.
func TestCsEnumBitfieldWidthElisions(t *testing.T) {
	var bits []string
	for i := 0; i < 64; i++ {
		bits = append(bits, fmt.Sprintf("F%d: { pos: %d }", i, i))
	}
	m := buildModule(t, []byte("version: 1\nmessages:\n  W:\n    payload:\n"+
		"      f: { id: 0, type: bitfield, bits: { "+strings.Join(bits, ", ")+" } }\n"+
		"      e: { id: 1, type: enum, enum: { R: 0, G: 1, B: 2 }, default: 0 }\n"),
		"elide.yaml", map[string]any{})
	if !strings.Contains(m, "case (Root, 0): m.f = (WF)value; break;") {
		t.Errorf("a bitfield implying the full u64 width must store unguarded:\n%s", m)
	}
	if strings.Contains(m, "0xffffffffffffffff") {
		t.Errorf("a tautological mask guard was emitted:\n%s", m)
	}
	// {R:0, G:1, B:2} implies i8, NOT the 0..2 hull of its constants: 5 is a
	// valid wire value for this field and must decode.
	if !strings.Contains(m, `case (Root, 1): if (value < -128 || value > 127) throw new SofabException(SofabError.InvalidMessage, "e: value outside declared enum width"); m.e = (WE)value; break;`) {
		t.Errorf("a contiguous enum must take the implied i8 width, not its constant hull:\n%s", m)
	}
}

// A bitfield declaring position 63 implies u64 — the accumulator's own width —
// so under the width rule it carries NO guard at all. Under the withdrawn
// closed-set rule the same declaration emitted a `~0x8000000000000001UL` mask,
// the literal-rendering trap generator#470 hit once; deriving the bound from the
// highest position instead removes both the trap and the comparison.
//
// One position below it is the widest literal this backend still renders, and
// `value` is a `ulong` there — pinned because a mistyped constant is a C#
// compile error, so otherwise only conformance would catch it.
func TestCsBitfieldWidthLiterals(t *testing.T) {
	m := buildModule(t, []byte("version: 1\nmessages:\n  W:\n    payload:\n"+
		"      g: { id: 0, type: bitfield, bits: { LOW: { pos: 0 }, HIGH: { pos: 63 } } }\n"+
		"      h: { id: 1, type: bitfield, bits: { LOW: { pos: 0 }, TOP: { pos: 31 } } }\n"),
		"bit63.yaml", map[string]any{})
	if !strings.Contains(m, "case (Root, 0): m.g = (WG)value; break;") {
		t.Errorf("a bitfield implying the full u64 width must store unguarded:\n%s", m)
	}
	if strings.Contains(m, "0x8000000000000001") {
		t.Errorf("the withdrawn flag-mask guard was emitted:\n%s", m)
	}
	if !strings.Contains(m, `case (Root, 1): if (value > 4294967295) throw new SofabException(SofabError.InvalidMessage, "h: value outside declared bitfield width"); m.h = (WH)value; break;`) {
		t.Errorf("a pos-31 bitfield must take the u32 width bound:\n%s", m)
	}
}

// The behavioural difference the width rule makes, stated as the values
// themselves: a gapped enum admits a value between its constants, and a bitfield
// admits an undeclared bit — both INVALID under the withdrawn closed-set rule.
// Pinned on the emitted bound so a silent reversion is loud.
func TestCsWidthAdmitsUndeclaredValues(t *testing.T) {
	m := buildModule(t, []byte(widthSixSrc), "width.yaml", map[string]any{})
	// enum {0,1,2,10}: the guard must admit 5 — i.e. be the i8 interval, never a
	// membership chain over the constants.
	if strings.Contains(m, "value != 10") {
		t.Errorf("the withdrawn membership chain over enum constants was emitted:\n%s", m)
	}
	// bitfield pos{0,1,3}: the guard must admit 4 — i.e. bound the WIDTH (255),
	// never mask the declared flags (0xb).
	if strings.Contains(m, "~0xbUL") {
		t.Errorf("the withdrawn flag-mask guard was emitted:\n%s", m)
	}
	if !strings.Contains(m, "value > 255") {
		t.Errorf("the bitfield width bound is missing:\n%s", m)
	}
}

// The bench harness folds one integer field into its sink after every decode.
// A deprecated field carries [Obsolete], and reading it there is CS0612 in the
// generated Program.cs, so the sink prefers the next non-deprecated integer; a
// deprecated integer that is the only one is still the sink, and the caller
// wraps that one read in a narrow CS0612 pragma.
func TestCsBenchSinkPrefersNonDeprecatedField(t *testing.T) {
	m := &ir.Message{Name: "m", Fields: []*ir.Field{
		{Name: "old", Kind: ir.KindU32, Deprecated: true},
		{Name: "cur", Kind: ir.KindU32},
	}}
	if got, dep := benchSinkField(m); got != "cur" || dep {
		t.Errorf("benchSinkField = (%q, %v), want the first non-deprecated integer (%q, false)", got, dep, "cur")
	}
	m.Fields = m.Fields[:1]
	if got, dep := benchSinkField(m); got != "old" || !dep {
		t.Errorf("benchSinkField = (%q, %v), want the only (deprecated) integer (%q, true)", got, dep, "old")
	}
}

// csProgram generates src as a project and returns its Program.cs.
func csProgram(t *testing.T, src string) string {
	t.Helper()
	doc, err := parser.Parse([]byte(src), "p.yaml")
	if err != nil {
		t.Fatal(err)
	}
	s, err := model.Build(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := analysis.Analyze(s); err != nil {
		t.Fatal(err)
	}
	files, err := (&Backend{}).Generate(s, map[string]any{"emit": "project"})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	for _, f := range files {
		if f.Path == "Program.cs" {
			return string(f.Content)
		}
	}
	t.Fatal("no Program.cs")
	return ""
}

// A file with no message ($defs only, like examples/messages/realworld/common.yaml)
// still gets a Program.cs. The bench sink would be assigned and never read
// (CS0414), and the `return 0;` after a switch whose only arm returns would be
// unreachable (CS0162) -- both errors under TreatWarningsAsErrors. With a
// message, both are read and reachable and must stay.
func TestCsHarnessWithoutMessagesLeavesNothingUnread(t *testing.T) {
	none := csProgram(t, "version: 1\n$defs:\n  struct:\n    P: { x: { id: 0, type: u8 } }\n")
	for _, bad := range []string{"benchSink", "Warmup", "return 0;"} {
		if strings.Contains(none, bad) {
			t.Errorf("message-less Program.cs contains %q", bad)
		}
	}
	one := csProgram(t, "version: 1\nmessages:\n  m: { payload: { a: { id: 0, type: u32 } } }\n")
	for _, want := range []string{"static long benchSink = 0;", "static readonly int Warmup", "        return 0;\n    }\n}"} {
		if !strings.Contains(one, want) {
			t.Errorf("Program.cs with a message lacks %q", want)
		}
	}
}

// TestCsNoLiteralIndexCheck: every wrapper-array element index -- string/blob
// leaf, struct/union element, wrapper row, native matrix row -- is bounded by
// corelib-cs's Seq, which takes the schema count and the receiver cap as
// arguments (ARCHITECTURE §8, generator#587). Generated code therefore compares
// no element index itself, in any shipped schema or in an uncounted-row probe.
// (vehicle_telemetry pulls cross-file $refs this in-memory build cannot resolve;
// the bench and the conformance suite generate it.)
func TestCsNoLiteralIndexCheck(t *testing.T) {
	for _, path := range []string{
		"../../examples/messages/example.yaml",
		"../../tests/matrix/corpus/defs/nested_rows.yaml",
		"../../tests/matrix/corpus/defs/seq_elements_dyn.yaml",
	} {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		m := buildModule(t, b, "in.yaml", map[string]any{})
		if n := strings.Count(m, "if (id >= "); n != 0 {
			t.Errorf("%s: %d literal index checks left in generated code", path, n)
		}
		if !strings.Contains(m, "global::sofab.Seq.") {
			t.Errorf("%s: no Seq call -- the probe no longer exercises a wrapper array", path)
		}
	}
	probe := buildModule(t, []byte(`version: 1
messages:
  M:
    payload:
      nrows: { id: 0, type: array, items: { type: array, items: { type: u32 } } }
      srows: { id: 1, type: array, items: { type: array, items: { type: string } } }
      orows: { id: 2, type: array, items: { type: array, items: { type: struct, fields: { x: { id: 0, type: u8 } } } } }
`), "p.yaml", map[string]any{})
	if n := strings.Count(probe, "if (id >= "); n != 0 {
		t.Errorf("uncounted rows: %d literal index checks left:\n%s", n, probe)
	}
	if n := strings.Count(probe, ".Count <= id)"); n != 0 {
		t.Errorf("uncounted rows: %d generated gap-fill loops left; growth is Seq.ReserveElem's:\n%s", n, probe)
	}
	for _, want := range []string{
		"global::sofab.Seq.ReserveRow(m.nrows, id, -1, MaxDynArrayCount);",
		"global::sofab.Seq.ReserveRow(m.srows, id, -1, MaxDynArrayCount);",
		"global::sofab.Seq.ReserveRow(m.orows, id, -1, MaxDynArrayCount);",
		"global::sofab.Seq.ReserveElem(m.orows[_ixRoot_orows], id, static () => new ",
	} {
		if !strings.Contains(probe, want) {
			t.Errorf("uncounted rows: missing %q:\n%s", want, probe)
		}
	}
}
