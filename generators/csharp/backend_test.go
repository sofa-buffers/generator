package csharp

import (
	"os"
	"strings"
	"testing"

	"github.com/sofa-buffers/generator/internal/analysis"
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
// (issue #142 / MESSAGE_SPEC §5.1/§7). A dynamic array keeps every index.
func TestCsOverIndexWrapperArray(t *testing.T) {
	src := []byte("version: 1\nmessages:\n  M:\n    payload:\n" +
		"      bs: { id: 0, type: array, items: { type: string, count: 4, maxlen: 16 } }\n" +
		"      bb: { id: 1, type: array, items: { type: blob,   count: 3, maxlen: 16 } }\n" +
		"      bp: { id: 2, type: array, items: { type: struct, count: 2, fields: { x: { id: 0, type: i32 } } } }\n" +
		"      ds: { id: 3, type: array, items: { type: string } }\n")
	m := buildModule(t, src, "in.yaml", map[string]any{"namespace": "S"})
	for _, want := range []string{
		`case (Root_bs, _): if (id >= 4) throw new SofabException(SofabError.InvalidMessage,`,
		`case (Root_bb, _): if (id >= 3) throw new SofabException(SofabError.InvalidMessage,`,
		`case (Root_bp, _): if (id >= 2) throw new SofabException(SofabError.InvalidMessage,`,
	} {
		if !strings.Contains(m, want) {
			t.Errorf("Message.cs missing over-index guard %q", want)
		}
	}
	// A DYNAMIC wrapper array is bounded too, and by the same test in the same
	// place -- what differs is the bound and the category. Its length is its
	// highest index, so the receiver cap binds the index; the bytes are well
	// formed and decode under a looser cap, so the verdict is LimitExceeded and
	// not InvalidMessage (generator#387, CORELIB_PLAN §6.2.1).
	if !strings.Contains(m, `case (Root_ds, _): if (id >= MaxDynArrayCount) throw new SofabException(SofabError.LimitExceeded, "Root_ds element: array index above configured limit 65536"); while (m.ds.Count <= id) m.ds.Add(""); m.ds[id] = _s; break;`) {
		t.Errorf("a dynamic wrapper array's element index must be capped:\n%s", m)
	}
}

// TestCsMaxlenReject: a bounded string/blob whose wire byte length exceeds its
// schema maxlen is malformed input, rejected as INVALID at the `total` length
// header (MESSAGE_SPEC §7.1) — for scalar fields and wrapper-array elements
// alike, never truncated. An unbounded field gets no maxlen arm.
func TestCsMaxlenReject(t *testing.T) {
	src := []byte("version: 1\nmessages:\n  M:\n    payload:\n" +
		"      s:  { id: 0, type: string, maxlen: 8 }\n" +
		"      b:  { id: 1, type: blob, maxlen: 8 }\n" +
		"      ws: { id: 2, type: array, items: { type: string, maxlen: 5 } }\n" +
		"      us: { id: 3, type: string }\n")
	m := buildModule(t, src, "in.yaml", map[string]any{"namespace": "S"})
	for _, want := range []string{
		// Bounded scalar string + blob: per-field maxlen check at `total`. The arm
		// closes by handing PayloadAcc that same maxlen as the cap -- the schema
		// bound governs a bounded field, so the receiver cap must not reach it and
		// the corelib's comparison can no longer fire (CORELIB_PLAN §6.2.1/§6.3).
		`case (Root, 0): if (total > 8) throw new SofabException(SofabError.InvalidMessage, "s: string length above schema maxlen 8"); _cap = 8; break;`,
		`case (Root, 1): if (total > 8) throw new SofabException(SofabError.InvalidMessage, "b: blob length above schema maxlen 8"); _cap = 8; break;`,
		// Bounded wrapper string element: keyed by the array location, element id agnostic.
		`case (Root_ws, _): if (total > 5) throw new SofabException(SofabError.InvalidMessage, "Root_ws element: string length above schema maxlen 5"); _cap = 5; break;`,
	} {
		if !strings.Contains(m, want) {
			t.Errorf("Message.cs missing maxlen guard %q\n%s", want, m)
		}
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
		// A boolean/enum array is a List: clearing it is decoding into it too, so
		// the kind test fronts the Clear() as well. boolean rides the Unsigned wire
		// type, enum the Signed one.
		`case (Root, 3): if (kind != ArrayKind.Unsigned) break; if (count > 2) throw new SofabException(SofabError.InvalidMessage, "ba: array count above schema capacity 2"); m.ba.Clear(); break;`,
		`case (Root, 4): if (kind != ArrayKind.Signed) break; if (count > 2) throw new SofabException(SofabError.InvalidMessage, "ea: array count above schema capacity 2"); m.ea.Clear(); break;`,
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
		"private readonly PayloadAcc pay = new PayloadAcc();",
		"string _s = pay.String(total, offset, data, chunkOffset, chunkLength, _cap);",
		"byte[] _b = pay.Blob(total, offset, data, chunkOffset, chunkLength, _cap);",
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
// wrapped in a CS0618 pragma so the output builds warning-clean), each enum
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
		// Internal access to the deprecated field is CS0618-suppressed.
		"    public void Serialize(OStream os) {\n#pragma warning disable 618 // internal access to a member marked [Obsolete]",
		"#pragma warning restore 618\n    }",
		"#pragma warning disable 618 // internal access to a member marked [Obsolete]\ninternal sealed class TelemetryVisitor : IVisitor {",
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
		"string _s = pay.String(total, offset, data, chunkOffset, chunkLength, _cap);",
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
		"os.WriteArraySigned(6, Array.ConvertAll(this.fe.ToArray(), _x => (sbyte)_x));",
		"os.WriteArrayUnsigned(7, Array.ConvertAll(this.fp.ToArray(), _x => (byte)_x));",

		// A fresh count:N array is EMPTY (nothing materialized to N), and a declared
		// default shorter than N stands exactly as written (never tail-padded).
		"public uint[] fx = Array.Empty<uint>();",
		"public double[] ff64 = Array.Empty<double>();",
		"public List<bool> fb = new();",
		"public List<BitfieldPerm> fp = new();",
		"public List<string> strs = new();",
		"public float[] ff32 = new float[]{1.5f};",
		"public List<EnumColor> fe = new List<EnumColor>{(EnumColor)(2)};",
		"public uint[] fxd = new uint[]{1, 2};",
		"private static readonly uint[] _arrdef_fxd = new uint[]{1, 2};",

		// Decode takes the M elements that arrived and nothing else: the primitive
		// arrays allocate the WIRE count (the #100 guard still bounds it by N), the
		// List<T> ones clear and append.
		`case (Root, 0): if (kind != ArrayKind.Unsigned) break; if (count > 5) throw new SofabException(SofabError.InvalidMessage, "fx: array count above schema capacity 5"); m.fx = new uint[count]; break;`,
		`case (Root, 3): if (kind != ArrayKind.Fp32) break; if (count > 4) throw new SofabException(SofabError.InvalidMessage, "ff32: array count above schema capacity 4"); m.ff32 = new float[count]; break;`,
		`case (Root, 5): if (kind != ArrayKind.Unsigned) break; if (count > 3) throw new SofabException(SofabError.InvalidMessage, "fb: array count above schema capacity 3"); m.fb.Clear(); break;`,
		"case (Root, 5): if (afill == 0) break; afill--; m.fb.Add(value != 0); break;",
		"case (Root, 6): if (afill == 0) break; afill--; m.fe.Add((EnumColor)value); break;",
		"case (Root, 7): if (afill == 0) break; afill--; m.fp.Add((BitfieldPerm)value); break;",

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
		// struct element: gap-fill, latch the id, descend — the element scope then
		// addresses the element the id named, not the last one.
		"case (Root_fixed, _): if (id >= 5) throw new SofabException(SofabError.InvalidMessage, " +
			"\"Root_fixed element: array index above schema capacity 5\"); " +
			"while (m.@fixed.Count <= id) m.@fixed.Add(new VecFixedElem()); _ixRoot_fixed = id; cur = Root_fixed_e; break;",
		"m.@fixed[_ixRoot_fixed].k = (uint)value; break;",
		// a count-less array is placed by id too: its length is highest id + 1.
		"while (m.dynamic.Count <= id) m.dynamic.Add(new VecDynamicElem()); _ixRoot_dynamic = id;",
		// string leaf element: placed, with the gap filled from the element default.
		"case (Root_fstrs, _): if (id >= 3) throw new SofabException(SofabError.InvalidMessage, " +
			"\"Root_fstrs element: array index above schema capacity 3\"); " +
			"while (m.fstrs.Count <= id) m.fstrs.Add(\"\"); m.fstrs[id] = _s; break;",
		// NATIVE row (the id-blind collector): placed at out[id], bounded by the outer
		// array's count, and the fill then addresses the latched row. The §7.3 kind
		// test fronts both (generator#254): a mis-typed row is skipped whole.
		// The ROW itself is count-less, so its element count also meets the
		// target's finite default cap (§9.5, generator#385) -- a bound on the
		// inner array, distinct from the outer index bound beside it.
		"case (Root_rows, _): if (kind != ArrayKind.Unsigned) break; " +
			"if (id >= 2) throw new SofabException(SofabError.InvalidMessage, " +
			"\"Root_rows element: array index above schema capacity 2\"); " +
			"if (count > MaxDynArrayCount) throw new SofabException(SofabError.LimitExceeded, " +
			"\"Root_rows element: array count above configured limit 65536\"); " +
			"while (m.rows.Count <= id) m.rows.Add(new List<uint>()); m.rows[id] = new List<uint>(); _ixRoot_rows = id; break;",
		// the §7.1 width guard for the u32 element follows afill-- and precedes the
		// store (see TestCsDeclaredWidthIsAValidityBound)
		"case (Root_rows, _): if (afill == 0) break; afill--; if (value > 4294967295) throw new SofabException(SofabError.InvalidMessage, " +
			"\"Root_rows element: value outside declared width u32\"); m.rows[_ixRoot_rows].Add((uint)value); break;",
		// WRAPPER row: same placement, then the descent.
		"case (Root_srows, _): if (id >= MaxDynArrayCount) throw new SofabException(SofabError.LimitExceeded, " +
			"\"Root_srows element: array index above configured limit 65536\"); " +
			"while (m.srows.Count <= id) m.srows.Add(new List<string>()); " +
			"m.srows[id] = new List<string>(); _ixRoot_srows = id; cur = Root_srows_e; break;",
		"case (Root_srows_e, _): if (id >= MaxDynArrayCount) throw new SofabException(SofabError.LimitExceeded, " +
			"\"Root_srows_e element: array index above configured limit 65536\"); " +
			"while (m.srows[_ixRoot_srows].Count <= id) m.srows[_ixRoot_srows].Add(\"\"); m.srows[_ixRoot_srows][id] = _s; break;",
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
	// The guard precedes the accumulator, which is where the buffering and the
	// UTF-8 verdict both happen, so a skipped payload is neither validated nor
	// able to leave bytes behind for a later declared field to inherit.
	for _, after := range []string{"pay.String("} {
		if i := strings.Index(fn, after); i < 0 || guardEnd > i {
			t.Errorf("String(): the destination guard must precede %q:\n%s", after, fn)
		}
	}
	// The maxlen reject is destination-scoped, and it is now scoped by BEING one
	// of the guard's arms: the same switch resolves the destination, applies the
	// schema bound and picks the cap, so an id with no arm never reaches either
	// test. It must therefore sit inside the guard and ahead of the accumulator.
	i := strings.Index(fn, "above schema maxlen")
	if i < 0 || i > guardEnd {
		t.Errorf("String(): the maxlen reject must be an arm of the destination guard:\n%s", fn)
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
	// The same arms carry the two bounds. A schema-bounded blob is rejected
	// against its own maxlen and then hands PayloadAcc that same maxlen as the
	// cap, under which the corelib's comparison can no longer fire (§6.2.1
	// forbids a receiver cap on a field the schema bounds; §6.3 gives the two
	// categories). An unbounded one hands over the configured constant.
	for _, want := range []string{
		`case (Root, 0): if (total > 16) throw new SofabException(SofabError.InvalidMessage, "b: blob length above schema maxlen 16"); _cap = 16; break;`,
		"case (Root, 1): _cap = MaxDynBlobLen; break;",
		"byte[] _b = pay.Blob(total, offset, data, chunkOffset, chunkLength, _cap);",
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
	if i := strings.Index(fn, "pay.Blob("); i < 0 || guardEnd > i {
		t.Errorf("Blob(): the destination guard must precede the accumulator:\n%s", fn)
	}
	// The maxlen reject is destination-scoped by BEING one of the guard's arms,
	// so it sits inside the guard and ahead of the accumulator.
	if i := strings.Index(fn, "above schema maxlen"); i < 0 || i > guardEnd {
		t.Errorf("Blob(): the maxlen reject must be an arm of the destination guard:\n%s", fn)
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
	if !strings.Contains(m, "case (Root_sa, _): if (id >= 3) throw") ||
		!strings.Contains(m, "if (total > 6) throw") {
		t.Error("a wrapper element must latch over-index then element maxlen")
	}
	if strings.Count(m, "total > 8") < 2 {
		t.Error("the payload-side maxlen guard must remain as defense")
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
		// The row's id, then the row's own element count.
		`if (id >= 3) throw new SofabException(SofabError.InvalidMessage, "Root_mat element: array index above schema capacity 3"); if (count > 4) throw new SofabException(SofabError.InvalidMessage, "Root_mat element: array count above schema capacity 4");`,
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
// the receiver cap, checked before the List grows (ARCHITECTURE §9.5,
// generator#387). See the Java twin for why the INDEX and not the element count:
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
		`if (id >= MaxDynArrayCount) throw new SofabException(SofabError.LimitExceeded, "Root_dstrs element: array index above configured limit 65536"); while (m.dstrs.Count <= id)`,
		`"Root_dblbs element: array index above configured limit 65536"); while (m.dblbs.Count <= id)`,
		`"Root_dobjs element: array index above configured limit 65536"); while (m.dobjs.Count <= id)`,
		`"Root_dmat element: array index above configured limit 65536");`,
	} {
		if !strings.Contains(m, want) {
			t.Errorf("Message.cs missing wrapper index cap %q:\n%s", want, m)
		}
	}
	if !strings.Contains(m, `if (id >= 4) throw new SofabException(SofabError.InvalidMessage, "Root_bstrs element: array index above schema capacity 4")`) {
		t.Errorf("a count:N wrapper array must keep its InvalidMessage schema bound:\n%s", m)
	}
	if strings.Contains(m, `"Root_bstrs element: array index above configured limit`) {
		t.Errorf("a schema-bounded array must not also carry the receiver cap:\n%s", m)
	}
}
