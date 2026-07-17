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
		"_s = Encoding.UTF8.GetString(data, chunkOffset, total);",
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
		"case (Root, 1): if (count > MaxDynArrayCount) throw new SofabException(SofabError.LimitExceeded, \"arr: array count above configured limit 65536\"); m.arr = new ulong[Math.Min(count, ArrayInitCap)]; break;",
		"m.arr = EnsureCap(m.arr, ai, acap); m.arr[ai++] = (ulong)value;",
		// Bounded array: only the #100 schema-capacity guard, and a fixed-length
		// alloc at the schema count (generator#136) — the guard still bounds it.
		"case (Root, 2): if (count > 100000) throw new SofabException(SofabError.InvalidMessage, \"barr: array count above schema capacity 100000\"); m.barr = new int[100000]; break;",
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
		"case (Root, 1): m.arr = new ulong[Math.Min(count, ArrayInitCap)]; break;",
		"m.arr = EnsureCap(m.arr, ai, acap); m.arr[ai++] = (ulong)value;",
		"private static T[] EnsureCap<T>(T[] a, int i, int cap) {",
	} {
		if !strings.Contains(plain, want) {
			t.Errorf("no-config Message.cs missing hardened count-less arm %q", want)
		}
	}
	// The bounded array allocates its fixed schema length, never lazy growth.
	if !strings.Contains(plain, "m.barr = new int[100000]; break;") {
		t.Error("bounded array must allocate its fixed schema count")
	}
}

// TestCsFixedCountTrailingDefaultRun covers MESSAGE_SPEC §3 (generator#136): a
// `count: N` native array is FIXED-LENGTH. Encode emits only elements [0, M')
// (M' = one past the last non-default element); decode materializes exactly N,
// refilling the elided trailing default run. Dynamic (count-less) arrays are
// unaffected — a trailing default element is significant there.
func TestCsFixedCountTrailingDefaultRun(t *testing.T) {
	const src = `
version: 1
$defs:
  enum:
    Color: { none: 0, red: 1 }
  bitfield:
    Perm: { read: { pos: 0 }, write: { pos: 1 } }
messages:
  m:
    payload:
      fx:   { id: 0, type: array, items: { type: u32, count: 5 } }
      dyn:  { id: 1, type: array, items: { type: u32 } }
      ffs:  { id: 2, type: array, items: { type: i16, count: 3 } }
      ff32: { id: 3, type: array, items: { type: fp32, count: 4 } }
      ff64: { id: 4, type: array, items: { type: fp64, count: 2 } }
      fb:   { id: 5, type: array, items: { type: boolean, count: 3 } }
      fe:   { id: 6, type: array, items: { type: enum, count: 3, enum: { $ref: "#/$defs/enum/Color" } } }
      fp:   { id: 7, type: array, items: { type: bitfield, count: 2, bits: { $ref: "#/$defs/bitfield/Perm" } } }
      dyne: { id: 8, type: array, items: { type: enum, enum: { $ref: "#/$defs/enum/Color" } } }
      nest: { id: 9, type: array, items: { type: array, count: 2, items: { type: u32, count: 4 } } }
`
	m := buildModule(t, []byte(src), "fixed.yaml", map[string]any{})

	for _, want := range []string{
		// Helpers: bit-pattern comparison, incl. the float overloads.
		"internal static class SofabFixedArray {",
		"internal static T[] TrimTail<T>(T[] a) where T : struct {",
		"while (n > 0 && BitConverter.SingleToInt32Bits(a[n - 1]) == 0) n--;",
		"while (n > 0 && BitConverter.DoubleToInt64Bits(a[n - 1]) == 0) n--;",

		// Encode: every fixed native array trims its trailing default run.
		"os.WriteArrayUnsigned(0, SofabFixedArray.TrimTail(this.fx));",
		"os.WriteArraySigned(2, SofabFixedArray.TrimTail(this.ffs));",
		"os.WriteArrayFp32(3, SofabFixedArray.TrimTailF32(this.ff32));",
		"os.WriteArrayFp64(4, SofabFixedArray.TrimTailF64(this.ff64));",
		"os.WriteArrayUnsigned(5, SofabFixedArray.TrimTail(Array.ConvertAll(this.fb.ToArray(), _x => _x ? (byte)1 : (byte)0)));",
		"os.WriteArraySigned(6, SofabFixedArray.TrimTail(Array.ConvertAll(this.fe.ToArray(), _x => (sbyte)_x)));",
		"os.WriteArrayUnsigned(7, SofabFixedArray.TrimTail(Array.ConvertAll(this.fp.ToArray(), _x => (byte)_x)));",

		// Decode: materialize exactly the schema count N, not the wire count.
		"m.fx = new uint[5]; break;",
		"m.ffs = new short[3]; break;",
		"m.ff32 = new float[4]; break;",
		"m.ff64 = new double[2]; break;",
		// A fixed List<T> (bool/enum/bitfield) is pre-filled to N defaults and
		// then written by index, so [M, N) survives as the element default.
		"m.fb.Clear(); for (int _p = 0; _p < 3; _p++) m.fb.Add(default(bool)); break;",
		"m.fe.Clear(); for (int _p = 0; _p < 3; _p++) m.fe.Add(default(EnumColor)); break;",
		"m.fp.Clear(); for (int _p = 0; _p < 2; _p++) m.fp.Add(default(BitfieldPerm)); break;",
		"case (Root, 5): m.fb[ai++] = value != 0; break;",
		"case (Root, 6): m.fe[ai++] = (EnumColor)value; break;",
		"case (Root, 7): m.fp[ai++] = (BitfieldPerm)value; break;",
	} {
		if !strings.Contains(m, want) {
			t.Errorf("Message.cs missing %q", want)
		}
	}

	for _, bad := range []string{
		// Dynamic arrays: no trim on encode, no fixed alloc / pre-fill on decode.
		"SofabFixedArray.TrimTail(this.dyn)",
		"SofabFixedArray.TrimTail(Array.ConvertAll(this.dyne",
		"m.dyne.Clear(); for (int _p",
	} {
		if strings.Contains(m, bad) {
			t.Errorf("Message.cs must not contain %q (dynamic arrays are unchanged)", bad)
		}
	}
	// Dynamic arrays keep their untrimmed write and append-based decode.
	for _, want := range []string{
		"os.WriteArrayUnsigned(1, this.dyn);",
		"os.WriteArraySigned(8, Array.ConvertAll(this.dyne.ToArray(), _x => (sbyte)_x));",
		"case (Root, 8): m.dyne.Add((EnumColor)value); break;",
		"case (Root, 8): m.dyne.Clear(); break;",
		"m.dyn = new uint[Math.Min(count, ArrayInitCap)]; break;",
		"case (Root, 1): m.dyn = EnsureCap(m.dyn, ai, acap); m.dyn[ai++] = (uint)value; break;",
	} {
		if !strings.Contains(m, want) {
			t.Errorf("Message.cs missing untouched dynamic-array form %q", want)
		}
	}
	// Nested array-of-array rows are NOT fixed: only ir.Field carries the
	// fixed-length contract, so inner rows pass fixed=false and never trim.
	if !strings.Contains(m, "os.WriteArrayUnsigned(_i0, this.nest[_i0].ToArray());") {
		t.Error("nested inner array rows must keep their untrimmed write")
	}
	// The over-count guard still precedes (and thus bounds) the eager alloc.
	if !strings.Contains(m, `case (Root, 0): if (count > 5) throw new SofabException(SofabError.InvalidMessage, "fx: array count above schema capacity 5"); m.fx = new uint[5]; break;`) {
		t.Error("the #100 over-count guard must still precede the fixed-length alloc")
	}
}

// TestCsFixedHelpersOmitted: a schema with no fixed-count native array emits no
// trim helper class at all.
func TestCsFixedHelpersOmitted(t *testing.T) {
	const src = `
version: 1
messages:
  m: { payload: { dyn: { id: 0, type: array, items: { type: u32 } } } }
`
	if m := buildModule(t, []byte(src), "dynonly.yaml", map[string]any{}); strings.Contains(m, "SofabFixedArray") {
		t.Error("no fixed-count native array -> no trim helpers")
	}
}

// TestCsFixedCountDefaultIsNElements covers the second F-0010 route (#136): the
// OMISSION path. A `count: N` native array is fixed-length, so its value is
// ALWAYS exactly N elements — with no schema default that is N element
// defaults, and a short schema default leaves the unlisted trailing elements at
// the element default. An all-default array is omitted by the sparse rule and so
// never reaches ArrayBegin; without an N-element initializer it would decode
// back as length 0 here while the fixed-storage backends yield N zeros.
func TestCsFixedCountDefaultIsNElements(t *testing.T) {
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
      fx:    { id: 0, type: array, items: { type: u32, count: 5 } }
      fxd:   { id: 1, type: array, items: { type: u32, count: 5 }, default: [1, 2] }
      full:  { id: 2, type: array, items: { type: u32, count: 3 }, default: [1, 2, 3] }
      dyn:   { id: 3, type: array, items: { type: u32 } }
      dynd:  { id: 4, type: array, items: { type: u32 }, default: [1, 2] }
      f32s:  { id: 5, type: array, items: { type: fp32, count: 4 }, default: [1.5] }
      f64s:  { id: 6, type: array, items: { type: fp64, count: 2 } }
      bools: { id: 7, type: array, items: { type: boolean, count: 3 } }
      enums: { id: 8, type: array, items: { type: enum, count: 3, enum: { $ref: "#/$defs/enum/Color" } }, default: [2] }
      perms: { id: 9, type: array, items: { type: bitfield, count: 2, bits: { $ref: "#/$defs/bitfield/Perm" } } }
      strs:  { id: 10, type: array, items: { type: string, count: 2, maxlen: 8 } }
`
	m := buildModule(t, []byte(src), "fixeddef.yaml", map[string]any{})

	for _, want := range []string{
		// No schema default: N element defaults. `new T[N]` keeps the emitted
		// source O(1) in N rather than spelling out N zero literals.
		"public uint[] fx = new uint[5];",
		"public double[] f64s = new double[2];",
		"public List<bool> bools = new List<bool>(new bool[3]);",
		"public List<BitfieldPerm> perms = new List<BitfieldPerm>(new BitfieldPerm[2]);",
		// Short schema default: tail-padded to exactly N.
		"public uint[] fxd = new uint[]{1, 2, 0, 0, 0};",
		"public float[] f32s = new float[]{1.5f, 0f, 0f, 0f};",
		"public List<EnumColor> enums = new List<EnumColor>{(EnumColor)(2), (EnumColor)(0), (EnumColor)(0)};",
		// An already-N-long default is untouched.
		"public uint[] full = new uint[]{1, 2, 3};",
		// The omit-compare default is hoisted into a static (Marshal only reads
		// it), so encode never allocates a fresh N-element literal per call.
		"private static readonly uint[] _arrdef_fx = new uint[5];",
		"private static readonly uint[] _arrdef_fxd = new uint[]{1, 2, 0, 0, 0};",
		"if (!System.Linq.Enumerable.SequenceEqual(this.fx, _arrdef_fx)) {",
		"if (!System.Linq.Enumerable.SequenceEqual(this.bools, _arrdef_bools)) {",
	} {
		if !strings.Contains(m, want) {
			t.Errorf("Message.cs missing %q", want)
		}
	}

	for _, bad := range []string{
		// Dynamic arrays are NOT fixed-length: no synthesized default, no
		// tail-pad, and no whole-field omit-compare when they have no default.
		"public uint[] dyn = new uint[",
		"_arrdef_dyn ",
		"public uint[] dynd = new uint[]{1, 2, 0",
		// A wrapper-sequence array is always framed: never whole-omitted, so it
		// gets no compare default.
		"_arrdef_strs",
	} {
		if strings.Contains(m, bad) {
			t.Errorf("Message.cs must not contain %q", bad)
		}
	}
	for _, want := range []string{
		"public uint[] dyn = Array.Empty<uint>();",
		"public uint[] dynd = new uint[]{1, 2};",
		"public List<string> strs = new();",
		// A dynamic array with no default keeps the allocation-free emptiness test.
		"if (this.dyn != null && this.dyn.Length != 0) {",
	} {
		if !strings.Contains(m, want) {
			t.Errorf("Message.cs missing untouched dynamic form %q", want)
		}
	}
}
