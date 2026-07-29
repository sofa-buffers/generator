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
	// Dynamic string array keeps every index (bare grow, no throw).
	if !strings.Contains(m, `case (Root_ds, _): while (m.ds.Count <= id) m.ds.Add(""); m.ds[id] = _s; break;`) {
		t.Errorf("dynamic string array must not carry an over-index guard:\n%s", m)
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
		// Bounded scalar string + blob: per-field maxlen check at `total`.
		`case (Root, 0): if (total > 8) throw new SofabException(SofabError.InvalidMessage, "s: string length above schema maxlen 8"); break;`,
		`case (Root, 1): if (total > 8) throw new SofabException(SofabError.InvalidMessage, "b: blob length above schema maxlen 8"); break;`,
		// Bounded wrapper string element: keyed by the array location, element id agnostic.
		`case (Root_ws, _): if (total > 5) throw new SofabException(SofabError.InvalidMessage, "Root_ws element: string length above schema maxlen 5"); break;`,
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
// Unsigned/Signed, fp arrays under Fixlen.
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
		"            ArrayKind.Fixlen => (cur, id) switch {",
		// Each declared array disarms under ITS OWN kind: the u32 array (id 2) and
		// the nested u16 inner scope under Unsigned, the i32 array (id 3) under
		// Signed, the fp32 array (id 4) under Fixlen (#193).
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
	} {
		if !strings.Contains(m, want) {
			t.Errorf("Message.cs missing per-kind skip arm %q:\n%s", want, m)
		}
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
		`case (Root, 2): if (kind != ArrayKind.Fixlen) break; if (count > 3) throw new SofabException(SofabError.InvalidMessage, "fa: array count above schema capacity 3"); m.fa = new float[count]; break;`,
		// A boolean/enum array is a List: clearing it is decoding into it too, so
		// the kind test fronts the Clear() as well. boolean rides the Unsigned wire
		// type, enum the Signed one.
		`case (Root, 3): if (kind != ArrayKind.Unsigned) break; if (count > 2) throw new SofabException(SofabError.InvalidMessage, "ba: array count above schema capacity 2"); m.ba.Clear(); break;`,
		`case (Root, 4): if (kind != ArrayKind.Signed) break; if (count > 2) throw new SofabException(SofabError.InvalidMessage, "ea: array count above schema capacity 2"); m.ea.Clear(); break;`,
		// A count-less array has no schema bound, but still gets the kind test.
		`case (Root, 5): if (kind != ArrayKind.Unsigned) break; m.da = new ushort[Math.Min(count, ArrayInitCap)]; break;`,
		// The skip counter is armed per kind; each id disarms under its own kind only.
		"            ArrayKind.Unsigned => (cur, id) switch {\n                (Root, 0) => 0,\n                (Root, 3) => 0,\n                (Root, 5) => 0,\n                _ => count,\n            },",
		"            ArrayKind.Signed => (cur, id) switch {\n                (Root, 1) => 0,\n                (Root, 4) => 0,\n                _ => count,\n            },",
		"            ArrayKind.Fixlen => (cur, id) switch {\n                (Root, 2) => 0,\n                _ => count,\n            },",
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

func TestCsStructural(t *testing.T) {
	m := exampleModule(t)
	for _, want := range []string{
		"using sofab;",
		"namespace Sofabuffers;",
		"public sealed class Myfirstmessage {",
		"public void Marshal(OStream os)",
		"public byte[] Encode()",
		"public static Myfirstmessage Decode(byte[] data)",
		"public static DecodeStatus TryDecode(byte[] data, out Myfirstmessage msg)", // status-surfacing decode (#105)
		"internal sealed class MyfirstmessageVisitor : IVisitor {",
		"public void SequenceBegin(int id)", // flat-visitor nesting
		"public ulong someu64 = 18446744073709551615UL;",
		"public enum MyfirstmessageSomeenum : sbyte {",
		"if (offset == 0 && chunkLength >= total) {", // string/blob single-shot fast path
		"_s = _Utf8(data, chunkOffset, total);",      // strict UTF-8: invalid -> INVALID (issue #85)
		"new System.Text.UTF8Encoding(false, true)",  // throwOnInvalidBytes, never lossy U+FFFD
		"System.Array.Copy(data, chunkOffset, _b, 0, total);",
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
		"    public void Marshal(OStream os) {\n#pragma warning disable 618 // internal access to a member marked [Obsolete]",
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
// grown on demand (EnsureCap) instead of an eager `new T[count]` from the
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
		"case (Root, 1): if (kind != ArrayKind.Unsigned) break; if (count > MaxDynArrayCount) throw new SofabException(SofabError.LimitExceeded, \"arr: array count above configured limit 65536\"); m.arr = new ulong[Math.Min(count, ArrayInitCap)]; break;",
		"m.arr = EnsureCap(m.arr, ai, acap); m.arr[ai++] = (ulong)value;",
		// Bounded array: only the #100 schema-capacity guard, and an alloc at the
		// WIRE count -- `count: N` is a capacity, so M is the length (§3) and the
		// guard is what still bounds the untrusted count.
		"case (Root, 2): if (kind != ArrayKind.Signed) break; if (count > 100000) throw new SofabException(SofabError.InvalidMessage, \"barr: array count above schema capacity 100000\"); m.barr = new int[count]; break;",
		// Unbounded string: `total` checked before any accumulation.
		"if (total > MaxDynStringLen) {",
		"case (Root, 0): throw new SofabException(SofabError.LimitExceeded, \"s: string length above configured limit 4096\");",
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

	// No limits configured -> no limit plumbing at all; only the unconditional
	// eager-allocation hardening of the count-less arm remains.
	plain := buildModule(t, []byte(src), "dyn.yaml", map[string]any{})
	if strings.Contains(plain, "MaxDyn") || strings.Contains(plain, "LimitExceeded") {
		t.Error("unset limits must emit no limit plumbing")
	}
	for _, want := range []string{
		"case (Root, 1): if (kind != ArrayKind.Unsigned) break; m.arr = new ulong[Math.Min(count, ArrayInitCap)]; break;",
		"m.arr = EnsureCap(m.arr, ai, acap); m.arr[ai++] = (ulong)value;",
		"private static T[] EnsureCap<T>(T[] a, int i, int cap) {",
	} {
		if !strings.Contains(plain, want) {
			t.Errorf("no-config Message.cs missing hardened count-less arm %q", want)
		}
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
		`case (Root, 3): if (kind != ArrayKind.Fixlen) break; if (count > 4) throw new SofabException(SofabError.InvalidMessage, "ff32: array count above schema capacity 4"); m.ff32 = new float[count]; break;`,
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
		"os.WriteSequenceBeginLazy(_i0); (this.fixedobj[_i0] ?? new VecFixedobjElem()).Marshal(os);\n" +
			"            if (_i0 == _n0 - 1) os.WriteSequenceEndKeep(); else os.WriteSequenceEnd();",
		"os.WriteSequenceBeginLazy(_i0); (this.dynobj[_i0] ?? new VecDynobjElem()).Marshal(os);\n" +
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
		"os.WriteSequenceBeginLazy(0); (this.nested ?? new MNested()).Marshal(os); os.WriteSequenceEnd();",
		// FIELD: the wrapper of a struct-element array, closed by the dropping end.
		"        os.WriteSequenceBeginLazy(1);\n" +
			"        for (int _i0 = 0, _n0 = this.structs.Count; _i0 < _n0; _i0++) {\n" +
			"            os.WriteSequenceBeginLazy(_i0); (this.structs[_i0] ?? new MStructsElem()).Marshal(os);\n" +
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
		"case (Root_fixed_e, 0): m.@fixed[_ixRoot_fixed].k = (uint)value; break;",
		// a count-less array is placed by id too: its length is highest id + 1.
		"while (m.dynamic.Count <= id) m.dynamic.Add(new VecDynamicElem()); _ixRoot_dynamic = id;",
		// string leaf element: placed, with the gap filled from the element default.
		"case (Root_fstrs, _): if (id >= 3) throw new SofabException(SofabError.InvalidMessage, " +
			"\"Root_fstrs element: array index above schema capacity 3\"); " +
			"while (m.fstrs.Count <= id) m.fstrs.Add(\"\"); m.fstrs[id] = _s; break;",
		// NATIVE row (the id-blind collector): placed at out[id], bounded by the outer
		// array's count, and the fill then addresses the latched row. The §7.3 kind
		// test fronts both (generator#254): a mis-typed row is skipped whole.
		"case (Root_rows, _): if (kind != ArrayKind.Unsigned) break; if (id >= 2) throw new SofabException(SofabError.InvalidMessage, " +
			"\"Root_rows element: array index above schema capacity 2\"); " +
			"while (m.rows.Count <= id) m.rows.Add(new List<uint>()); m.rows[id] = new List<uint>(); _ixRoot_rows = id; break;",
		"case (Root_rows, _): if (afill == 0) break; afill--; m.rows[_ixRoot_rows].Add((uint)value); break;",
		// WRAPPER row: same placement, then the descent.
		"case (Root_srows, _): while (m.srows.Count <= id) m.srows.Add(new List<string>()); " +
			"m.srows[id] = new List<string>(); _ixRoot_srows = id; cur = Root_srows_e; break;",
		"case (Root_srows_e, _): while (m.srows[_ixRoot_srows].Count <= id) m.srows[_ixRoot_srows].Add(\"\"); m.srows[_ixRoot_srows][id] = _s; break;",
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
