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
		// Armed in ArrayBegin, per array kind.
		"askip = kind switch {",
		"            ArrayKind.Unsigned or ArrayKind.Signed => (cur, id) switch {",
		"            ArrayKind.Fixlen => (cur, id) switch {",
		// Declared integer arrays disarm under the int arm; so does the nested
		// native inner scope. The fp32 array disarms under the Fixlen arm (#193).
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
		"case (Root, 5): if (afill == 0) break; afill--; m.fb[ai++] = value != 0; break;",
		"case (Root, 6): if (afill == 0) break; afill--; m.fe[ai++] = (EnumColor)value; break;",
		"case (Root, 7): if (afill == 0) break; afill--; m.fp[ai++] = (BitfieldPerm)value; break;",
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
		"case (Root, 8): if (afill == 0) break; afill--; m.dyne.Add((EnumColor)value); break;",
		"case (Root, 8): m.dyne.Clear(); break;",
		"m.dyn = new uint[Math.Min(count, ArrayInitCap)]; break;",
		"case (Root, 1): if (afill == 0) break; afill--; m.dyn = EnsureCap(m.dyn, ai, acap); m.dyn[ai++] = (uint)value; break;",
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

// TestCsSeqTrimHelpersFollowNestedElements: the wrapper-array narrowings are
// emitted for the element kinds actually reached, at any nesting depth. A string
// array reachable only as the INNER row of an array-of-array still calls
// TrimStrs (marshalArray recurses into rows), so gating on top-level fields alone
// would emit a call to a helper that does not exist.
func TestCsSeqTrimHelpersFollowNestedElements(t *testing.T) {
	const src = `
version: 1
messages:
  m:
    payload:
      grid:
        id: 0
        type: array
        items:
          type: array
          items: { type: string, maxlen: 8 }
`
	m := buildModule(t, []byte(src), "grid.yaml", map[string]any{})
	if !strings.Contains(m, "internal static int TrimStrs(List<string> a) {") {
		t.Errorf("a nested string row must still get its narrowing helper:\n%s", m)
	}
	if !strings.Contains(m, "SofabFixedArray.TrimStrs(this.grid[_i0])") {
		t.Errorf("the inner row must narrow through the helper:\n%s", m)
	}
	// `grid` is dynamic, so no row narrowing is reachable and TrimRows stays out.
	if strings.Contains(m, "TrimRows") {
		t.Errorf("a dynamic array-of-array must not emit the row narrowing:\n%s", m)
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
		// A wrapper-sequence array carries no compare default: its whole-field
		// omission is the corelib's lazy frame (MESSAGE_SPEC §2), not a compare here.
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

// TestCsSequenceFramingClosers covers MESSAGE_SPEC §2 (documentation#29): every
// sequence is opened with the lazy begin, and the CLOSER — chosen statically from
// the position in the schema, never from the value — decides whether a contentless
// one survives. A sequence-typed FIELD (struct/union field, or the wrapper of a
// composite array) closes with the dropping `WriteSequenceEnd`, so an all-default
// nested object vanishes instead of costing an empty frame. A wrapper-array ELEMENT
// (a struct/union element, or a nested row of an array-of-array) closes with
// `WriteSequenceEndKeep`, because element presence is what carries a dynamic
// array's length (§5.1) — dropping one would change the decoded length, not just
// the bytes.
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
		// FIELD: the wrapper of a struct-element array (depth 0).
		"os.WriteSequenceBeginLazy(1);",
		// ELEMENT: the per-element struct frame is unconditional.
		"os.WriteSequenceBeginLazy(_i0); (this.structs[_i0] ?? new MStructsElem()).Marshal(os); os.WriteSequenceEndKeep();",
		// FIELD: the wrapper of a string array (depth 0) — leaf elements are omitted
		// individually, and an all-default array drops the wrapper too.
		"os.WriteSequenceBeginLazy(2);",
		// FIELD: the wrapper of an array-of-array (depth 0) ...
		"os.WriteSequenceBeginLazy(3);",
		// ... whose rows are ELEMENTS: an all-default row keeps its frame (§5.1).
		// `grid` is DYNAMIC, so its row loop runs to Count: with no schema N to
		// refill from, a trailing all-default row is significant (generator#248).
		"for (int _i0 = 0, _n0 = this.grid.Count; _i0 < _n0; _i0++) {\n            os.WriteSequenceBeginLazy(_i0);\n" +
			"            for (int _i1 = 0, _n1 = SofabFixedArray.TrimStrs(this.grid[_i0]); _i1 < _n1; _i1++) { if ((this.grid[_i0][_i1] ?? \"\") != \"\") os.WriteString(_i1, this.grid[_i0][_i1] ?? \"\"); }\n" +
			"            os.WriteSequenceEndKeep();\n        }\n        os.WriteSequenceEnd();",
	} {
		if !strings.Contains(m, want) {
			t.Errorf("Message.cs missing %q", want)
		}
	}
	// The eager begin is gone from the corelib; emitting it would not compile.
	if strings.Contains(m, "WriteSequenceBegin(") {
		t.Error("eager WriteSequenceBegin must not be emitted; the corelib only has WriteSequenceBeginLazy")
	}
	// Exactly one keeping closer per ELEMENT site (struct element, nested row) and
	// one dropping closer per FIELD site (4 wrappers + the struct field).
	if got := strings.Count(m, "os.WriteSequenceEndKeep();"); got != 2 {
		t.Errorf("WriteSequenceEndKeep count = %d, want 2 (struct element + nested row)", got)
	}
	if got := strings.Count(m, "os.WriteSequenceEnd();"); got != 4 {
		t.Errorf("WriteSequenceEnd count = %d, want 4 (struct field + 3 array wrappers)", got)
	}
}

// trimSchema is a message with one `count: N` struct array, one DYNAMIC struct
// array and one `count: N` string array — the three shapes generator#247/#248
// separate.
const trimSchema = `
version: 1
messages:
  vec:
    payload:
      fixed:   { id: 0, type: array, items: { type: struct, count: 5, fields: { k: { id: 0, type: u32 } } } }
      dynamic: { id: 1, type: array, items: { type: struct, fields: { k: { id: 0, type: u32 } } } }
      fstrs:   { id: 2, type: array, items: { type: string, count: 3, maxlen: 8 } }
`

// A count:N wrapper array's canonical wire stops at M — one past its last
// non-default element (MESSAGE_SPEC §3/§5.1, "even for sequence-form elements")
// — and M == 0 leaves the whole wrapper omitted (§2). generator#248: the element
// loop used to run to Count, framing every trailing all-default element, so a
// decoder that accepted the non-canonical form re-encoded it unchanged instead of
// normalising. A DYNAMIC array has no N to refill from, so its trailing default
// element is significant and must still be framed.
func TestCsFixedWrapperArrayTrimsTrailingDefaultRun(t *testing.T) {
	m := buildModule(t, []byte(trimSchema), "m.yaml", map[string]any{})

	// The fixed array narrows to M before framing anything...
	if !strings.Contains(m, "for (int _i0 = 0, _n0 = VecFixedElem.TrimTail(this.@fixed); _i0 < _n0; _i0++) {") {
		t.Errorf("count:N struct array must loop to M, not Count:\n%s", m)
	}
	// ...while the dynamic one keeps every element, trailing defaults included.
	if !strings.Contains(m, "for (int _i0 = 0, _n0 = this.dynamic.Count; _i0 < _n0; _i0++) {") {
		t.Errorf("dynamic struct array must not be narrowed:\n%s", m)
	}
	// An interior all-default element is still framed: only the TRAILING run goes.
	if !strings.Contains(m, "os.WriteSequenceBeginLazy(_i0); (this.@fixed[_i0] ?? new VecFixedElem()).Marshal(os); os.WriteSequenceEndKeep();") {
		t.Errorf("interior elements must keep the framing closer:\n%s", m)
	}
	// The predicate M is found with: the element's own all-default test.
	if !strings.Contains(m, "    internal static int TrimTail(List<VecFixedElem> a) {\n"+
		"        int n = a.Count;\n"+
		"        while (n > 0 && (a[n - 1] == null || a[n - 1].IsDefault())) n--;\n"+
		"        return n;\n    }") {
		t.Errorf("the element class must carry the narrowing helper:\n%s", m)
	}

	// IsDefault is the exact negation of what Marshal writes, so it must narrow a
	// field exactly when the marshal loop does — disagreeing would either omit a
	// field that is on the wire or keep one that is not.
	if !strings.Contains(m, "if (!(VecFixedElem.TrimTail(this.@fixed) == 0)) return false;") {
		t.Errorf("IsDefault must narrow the fixed array like Marshal does:\n%s", m)
	}
	if !strings.Contains(m, "if (!(this.dynamic.Count == 0)) return false;") {
		t.Errorf("IsDefault must NOT narrow the dynamic array:\n%s", m)
	}
	if !strings.Contains(m, "if (!(SofabFixedArray.TrimStrs(this.fstrs) == 0)) return false;") {
		t.Errorf("IsDefault for a string wrapper array must test the trimmed run:\n%s", m)
	}
	// A type only used as a DYNAMIC array's element never needs the helper.
	if strings.Contains(m, "TrimTail(List<VecDynamicElem> a)") {
		t.Errorf("a dynamic array's element type must not get a narrowing helper:\n%s", m)
	}
}

// generator#247: a wrapper array's element id IS the array index (§5.1), so an
// element is PLACED at dest[id] after gap-filling — never appended. Appending
// shortened the array by the size of any interior id gap and decoded a REOPENED
// id as a second element instead of merging into the first (§7.4). The leaf
// string/blob element arms next to it always got this right.
//
// The N-fill when the sequence scope closes is what makes the §3/§5.1 trailing
// elision lossless: without it, re-encoding a decoded fixed array shortens it on
// every round trip.
func TestCsWrapperElementsArePlacedByIDAndFilledToN(t *testing.T) {
	m := buildModule(t, []byte(trimSchema), "m.yaml", map[string]any{})

	for _, want := range []string{
		// placement, not append — the gap-fill, the id latch, then the descent
		"case (Root_fixed, _): if (id >= 5) throw new SofabException(SofabError.InvalidMessage, " +
			"\"Root_fixed element: array index above schema capacity 5\"); " +
			"while (m.@fixed.Count <= id) m.@fixed.Add(new VecFixedElem()); _ixRoot_fixed = id; cur = Root_fixed_e; break;",
		// the element scope decodes into the element the id named, not the last one
		"case (Root_fixed_e, 0): m.@fixed[_ixRoot_fixed].k = (uint)value; break;",
		// N-fill when the sequence scope closes, per element kind
		"case Root_fixed: while (m.@fixed.Count < 5) m.@fixed.Add(new VecFixedElem()); break;",
		"case Root_fstrs: while (m.fstrs.Count < 3) m.fstrs.Add(\"\"); break;",
	} {
		if !strings.Contains(m, want) {
			t.Errorf("Message.cs missing %q:\n%s", want, m)
		}
	}
	// The defect this replaced: appending ignored the id entirely.
	if strings.Contains(m, "m.@fixed.Add(new VecFixedElem()); cur = Root_fixed_e;") {
		t.Errorf("a struct element must not be appended id-blind:\n%s", m)
	}
	if strings.Contains(m, "m.@fixed[m.@fixed.Count - 1]") {
		t.Errorf("the element scope must address the id, not the last element:\n%s", m)
	}
	// A DYNAMIC array is gap-filled like any other (its length is highest id + 1)
	// but never filled out to an N it does not have.
	if !strings.Contains(m, "while (m.dynamic.Count <= id) m.dynamic.Add(new VecDynamicElem()); _ixRoot_dynamic = id;") {
		t.Errorf("a dynamic array's elements must still be placed by id:\n%s", m)
	}
	if strings.Contains(m, "case Root_dynamic:") {
		t.Errorf("a dynamic array must not be default-filled at SequenceEnd:\n%s", m)
	}
}
