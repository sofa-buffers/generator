package golang

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Behavioral wire tests for the generated Go encoder, run against the real
// corelib-go (gated on SOFAB_GO_CORELIB, same as the shared-vector conformance).
// Unlike the structural omit_test.go (which only checks that a conditional write
// is emitted), these actually serialize messages and inspect the bytes on the
// wire, pinning the MESSAGE_SPEC semantics:
//   - "the encoder emits a field iff its value != its default"
//   - enum/boolean/bitfield arrays reuse the signed/unsigned array wire types
//   - struct/union/nested arrays lower to wrapper sequences
//   - a sequence-typed FIELD whose value is all-default is omitted, while an
//     array ELEMENT is sparse in the interior and always written at the last
//     index (MESSAGE_SPEC S2).

func requireGoCorelib(t *testing.T) string {
	t.Helper()
	corelib := os.Getenv("SOFAB_GO_CORELIB")
	if corelib == "" {
		t.Skip("set SOFAB_GO_CORELIB to a corelib-go checkout to run the wire tests")
	}
	return corelib
}

// buildGoHarnessCfg is buildGoHarness with a caller-supplied config. Every
// message in def is reachable via the encode/decode CLI by name.
func buildGoHarnessCfg(t *testing.T, corelib, def string, extra map[string]any) string {
	t.Helper()
	s := schemaFromYAMLString(t, def)
	cfg := map[string]any{
		"emit": "project", "package": "message", "module_path": "example.com/wire", "go_version": "1.21",
	}
	for k, v := range extra {
		cfg[k] = v
	}
	files, err := (&Backend{}).Generate(s, cfg)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	dir := t.TempDir()
	for _, f := range files {
		full := filepath.Join(dir, f.Path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		content := f.Content
		if f.Path == "go.mod" {
			content = []byte(strings.ReplaceAll(string(content), "${SOFAB_GO_CORELIB}", corelib))
		}
		if err := os.WriteFile(full, content, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{{"mod", "tidy"}, {"build", "-o", "harness_bin", "./harness"}} {
		cmd := exec.Command("go", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GOFLAGS=-mod=mod")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("go %v: %v\n%s", args, err, out)
		}
	}
	return filepath.Join(dir, "harness_bin")
}

func encHex(t *testing.T, bin, msg, jsonIn string) string {
	t.Helper()
	cmd := exec.Command(bin, "encode", msg)
	cmd.Stdin = strings.NewReader(jsonIn)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("encode %s %q: %v", msg, jsonIn, err)
	}
	return hex.EncodeToString(out)
}

func decJSON(t *testing.T, bin, msg, hexBytes string) string {
	t.Helper()
	raw, err := hex.DecodeString(hexBytes)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "decode", msg)
	cmd.Stdin = bytes.NewReader(raw)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("decode %s: %v", msg, err)
	}
	return normJSON(t, strings.TrimSpace(string(out)))
}

// roundTrip encodes jsonIn then decodes the bytes, returning the normalized JSON.
func roundTrip(t *testing.T, bin, msg, jsonIn string) string {
	t.Helper()
	return decJSON(t, bin, msg, encHex(t, bin, msg, jsonIn))
}

func normJSON(t *testing.T, s string) string {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatalf("normJSON %q: %v", s, err)
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// --- sparse-canonical omission (MESSAGE_SPEC §2) --------------------------

// TestSparseWireOmitsDefaults: encoding is always sparse-canonical (no toggle).
// A field equal to its default is dropped, so an all-default message encodes to
// an EMPTY payload and reconstructs its defaults on decode; a field that
// overrides its default stays on the wire and round-trips.
func TestSparseWireOmitsDefaults(t *testing.T) {
	corelib := requireGoCorelib(t)
	def := "version: 1\nmessages:\n  vec:\n    payload:\n" +
		"      a: {id: 0, type: u32, default: 7}\n" +
		"      b: {id: 1, type: i32, default: 10}\n" +
		"      c: {id: 2, type: boolean, default: true}\n"
	bin := buildGoHarnessCfg(t, corelib, def, nil)

	// All fields at their default -> empty payload; decode reconstructs them.
	allDefault := `{"a":7,"b":10,"c":true}`
	if got := encHex(t, bin, "vec", allDefault); got != "" {
		t.Errorf("all-default message must encode to an empty payload (sparse), got %q", got)
	}
	if got := decJSON(t, bin, "vec", ""); got != normJSON(t, allDefault) {
		t.Errorf("decode of empty payload must reconstruct defaults: got %s want %s", got, normJSON(t, allDefault))
	}

	// A field overriding its default is on the wire and round-trips (the
	// untouched defaults are reconstructed).
	override := `{"a":99,"b":10,"c":true}`
	overHex := encHex(t, bin, "vec", override)
	if overHex == "" {
		t.Error("a field overriding its default must appear on the wire")
	}
	if got := decJSON(t, bin, "vec", overHex); got != normJSON(t, override) {
		t.Errorf("override round-trip: got %s want %s", got, normJSON(t, override))
	}
}

// --- new array element types (MESSAGE_SPEC wire forms) --------------------

// enum / boolean / bitfield arrays reuse the scalar array wire types, so their
// bytes must be identical to the equivalent numeric array carrying the same
// underlying integers (enum -> signed, boolean/bitfield -> unsigned).

func TestArrayEnumUsesSignedArrayWire(t *testing.T) {
	corelib := requireGoCorelib(t)
	enumDef := "version: 1\nmessages:\n  vec:\n    payload:\n" +
		"      arr: {id: 0, type: array, items: {type: enum, count: 3, enum: {NEG: -1, ZERO: 0, POS: 5}}}\n"
	sintDef := "version: 1\nmessages:\n  vec:\n    payload:\n" +
		"      arr: {id: 0, type: array, items: {type: i32, count: 3}}\n"
	enumBin := buildGoHarnessCfg(t, corelib, enumDef, nil)
	sintBin := buildGoHarnessCfg(t, corelib, sintDef, nil)
	vals := `{"arr":[-1,0,5]}`
	if e, s := encHex(t, enumBin, "vec", vals), encHex(t, sintBin, "vec", vals); e != s {
		t.Errorf("array-of-enum must use the signed-array wire form: enum=%s signed=%s", e, s)
	}
}

func TestArrayBooleanUsesUnsignedArrayWire(t *testing.T) {
	corelib := requireGoCorelib(t)
	boolDef := "version: 1\nmessages:\n  vec:\n    payload:\n" +
		"      arr: {id: 0, type: array, items: {type: boolean, count: 3}}\n"
	uintDef := "version: 1\nmessages:\n  vec:\n    payload:\n" +
		"      arr: {id: 0, type: array, items: {type: u8, count: 3}}\n"
	boolBin := buildGoHarnessCfg(t, corelib, boolDef, nil)
	uintBin := buildGoHarnessCfg(t, corelib, uintDef, nil)
	if b, u := encHex(t, boolBin, "vec", `{"arr":[true,false,true]}`), encHex(t, uintBin, "vec", `{"arr":[1,0,1]}`); b != u {
		t.Errorf("array-of-boolean must use the unsigned-array wire form (0/1): bool=%s uint=%s", b, u)
	}
}

func TestArrayBitfieldUsesUnsignedArrayWire(t *testing.T) {
	corelib := requireGoCorelib(t)
	bfDef := "version: 1\nmessages:\n  vec:\n    payload:\n" +
		"      arr: {id: 0, type: array, items: {type: bitfield, count: 2, bits: {X: {pos: 0}, Y: {pos: 1}}}}\n"
	uintDef := "version: 1\nmessages:\n  vec:\n    payload:\n" +
		"      arr: {id: 0, type: array, items: {type: u8, count: 2}}\n"
	bfBin := buildGoHarnessCfg(t, corelib, bfDef, nil)
	uintBin := buildGoHarnessCfg(t, corelib, uintDef, nil)
	vals := `{"arr":[1,3]}`
	if b, u := encHex(t, bfBin, "vec", vals), encHex(t, uintBin, "vec", vals); b != u {
		t.Errorf("array-of-bitfield must use the unsigned-array wire form: bitfield=%s uint=%s", b, u)
	}
}

// struct / union / nested arrays lower to wrapper sequences; assert they encode
// non-trivially and round-trip exactly.

func TestArrayOfStructWireRoundTrip(t *testing.T) {
	corelib := requireGoCorelib(t)
	def := "version: 1\nmessages:\n  vec:\n    payload:\n" +
		"      arr: {id: 0, type: array, items: {type: struct, count: 2, fields: {x: {id: 0, type: i32}, y: {id: 1, type: i32}}}}\n"
	bin := buildGoHarnessCfg(t, corelib, def, nil)
	in := `{"arr":[{"x":1,"y":-2},{"x":3,"y":4}]}`
	if got := roundTrip(t, bin, "vec", in); got != normJSON(t, in) {
		t.Errorf("array-of-struct round-trip: got %s want %s", got, normJSON(t, in))
	}
}

func TestNestedArrayWireRoundTrip(t *testing.T) {
	corelib := requireGoCorelib(t)
	def := "version: 1\nmessages:\n  vec:\n    payload:\n" +
		"      arr: {id: 0, type: array, items: {type: array, count: 2, items: {type: u32, count: 3}}}\n"
	bin := buildGoHarnessCfg(t, corelib, def, nil)
	in := `{"arr":[[1,2,3],[4,5,6]]}`
	if got := roundTrip(t, bin, "vec", in); got != normJSON(t, in) {
		t.Errorf("nested-array round-trip: got %s want %s", got, normJSON(t, in))
	}
}

// TestEmptyArrayFieldWireIsOmitted pins the MESSAGE_SPEC §2 rule for an array
// FIELD: the wrapper sequence is opened lazily and dropped when no element is
// written, so an EMPTY array is OMITTED rather than framed as an empty wrapper.
// That is the canonical encoding precisely because the field's declared default is
// the empty collection, so absence reconstructs the same value.
//
// A declared `count: 3` does not change this and does not change the decoded
// length either: `count` is a capacity, not a length (§3), so the omitted field
// and the legacy empty wrapper both decode to the EMPTY array -- length 0, not 3.
// A one-element wire decodes at length 1.
func TestEmptyArrayFieldWireIsOmitted(t *testing.T) {
	corelib := requireGoCorelib(t)
	def := "version: 1\nmessages:\n  vec:\n    payload:\n" +
		"      arr: {id: 0, type: array, items: {type: string, count: 3, maxlen: 8}}\n"
	bin := buildGoHarnessCfg(t, corelib, def, nil)

	empty := encHex(t, bin, "vec", `{"arr":[]}`)
	one := encHex(t, bin, "vec", `{"arr":["x"]}`)
	if empty != "" {
		t.Errorf("an empty array field must be omitted, got %s", empty)
	}
	if one == "" {
		t.Error("a populated array must be on the wire, got empty payload")
	}
	for _, tc := range []struct {
		what string
		hex  string
		want int
	}{
		{"the omitted field", empty, 0},
		{"a one-element wire", one, 1},
		// The pre-uniform encoding (an explicit empty wrapper, 06 07) stays readable
		// and denotes the very same value -- a decoder normalizes it away.
		{"a legacy empty wrapper", "0607", 0},
	} {
		if n := arrLen(t, bin, "vec", tc.hex); n != tc.want {
			t.Errorf("%s must decode at length %d, got %d", tc.what, tc.want, n)
		}
	}
}

// TestWireArrayElementSparsityIsPositional is the byte-level statement of
// MESSAGE_SPEC §2's one element rule, for both element kinds at once. Every hex
// below is a regenerated shared test vector (the serialized_sparse form), so these
// are cross-language byte targets, not this backend's opinion:
//
//	array_string_trailing_default          ["a",""]      06020a610a0207
//	array_string_all_default               ["",""]       060a0207
//	array_string_leading_default           ["","x",""]   060a0a78120207
//	array_string_gap                       ["a","","c"]  06020a61120a6307
//	array_struct_interior_default_element  [{1},{},{3}]  06060001071600030707
//	array_struct_all_default_elements      [{},{}]       060e0707
//
// The rule: an element before the last one that equals its element default is
// omitted (a leaf not written, a sequence element not framed either), leaving an id
// GAP the decoder restores from that default; the LAST element is always written,
// as its value or as an empty frame, because its presence is what fixes the decoded
// length. Round-tripping is therefore exact -- ["a",""], ["a"] and [] are three
// distinct values with three distinct encodings.
func TestWireArrayElementSparsityIsPositional(t *testing.T) {
	corelib := requireGoCorelib(t)
	// count: 4 deliberately -- a capacity must change none of this (§3).
	strDef := "version: 1\nmessages:\n  vec:\n    payload:\n" +
		"      arr: {id: 0, type: array, items: {type: string, count: 4, maxlen: 8}}\n"
	objDef := "version: 1\nmessages:\n  vec:\n    payload:\n" +
		"      arr: {id: 0, type: array, items: {type: struct, count: 4, fields: {k: {id: 0, type: u32}}}}\n"

	for _, probe := range []struct {
		what  string
		def   string
		cases []struct{ in, wantHex, wantJSON string }
	}{
		{"string elements", strDef, []struct{ in, wantHex, wantJSON string }{
			// interior gap at index 1, last element carries a value
			{`{"arr":["a","","c"]}`, "06020a61120a6307", `{"arr":["a","","c"]}`},
			// the trailing default is the LAST element: written, so length 2 survives
			{`{"arr":["a",""]}`, "06020a610a0207", `{"arr":["a",""]}`},
			// all-default: the interior one drops, the final one is written alone at
			// id 1 -- this is NOT the empty array
			{`{"arr":["",""]}`, "060a0207", `{"arr":["",""]}`},
			// leading default leaves a gap, trailing default is written
			{`{"arr":["","x",""]}`, "060a0a78120207", `{"arr":["","x",""]}`},
			// no element at all: the wrapper stays contentless and the FIELD is
			// omitted (§2), decoding to the empty array
			{`{"arr":[]}`, "", `{"arr":null}`},
		}},
		{"struct elements", objDef, []struct{ in, wantHex, wantJSON string }{
			// the interior all-default element is NOT framed: id 1 is a gap, and the
			// array still decodes at length 3 from id 2
			{`{"arr":[{"k":1},{"k":0},{"k":3}]}`, "06060001071600030707", `{"arr":[{"k":1},{"k":0},{"k":3}]}`},
			// all-default elements: interior drops, the last keeps its empty frame
			{`{"arr":[{"k":0},{"k":0}]}`, "060e0707", `{"arr":[{"k":0},{"k":0}]}`},
			// a single all-default element is the last one: an empty frame at id 0
			{`{"arr":[{"k":0}]}`, "06060707", `{"arr":[{"k":0}]}`},
			{`{"arr":[]}`, "", `{"arr":null}`},
		}},
	} {
		bin := buildGoHarnessCfg(t, corelib, probe.def, nil)
		for _, c := range probe.cases {
			if got := encHex(t, bin, "vec", c.in); got != c.wantHex {
				t.Errorf("%s: encode %s: got %s, want %s", probe.what, c.in, got, c.wantHex)
			}
			if got := roundTrip(t, bin, "vec", c.in); got != normJSON(t, c.wantJSON) {
				t.Errorf("%s: round-trip %s: got %s, want %s", probe.what, c.in, got, normJSON(t, c.wantJSON))
			}
		}
	}
}

// TestWireNestedRowSparsityIsPositional applies the same rule one level down, to
// an array whose elements are themselves arrays. A row equal to the element default
// (the empty row) is omitted in the interior and written at the last index -- as an
// empty count-prefixed array for a native row, as an empty frame for a wrapper row.
// The wrapper-row shape is the shared vector array_of_string_arrays
// ([["a"],[]] -> 0606020a61070e0707).
func TestWireNestedRowSparsityIsPositional(t *testing.T) {
	corelib := requireGoCorelib(t)
	rowsDef := "version: 1\nmessages:\n  vec:\n    payload:\n" +
		"      arr: {id: 0, type: array, items: {type: array, count: 3, items: {type: string, maxlen: 8}}}\n"
	matDef := "version: 1\nmessages:\n  vec:\n    payload:\n" +
		"      arr: {id: 0, type: array, items: {type: array, count: 3, items: {type: u32, count: 3}}}\n"

	rows := buildGoHarnessCfg(t, corelib, rowsDef, nil)
	// [["a"],[]]: the empty row is LAST, so it keeps its frame and the outer array
	// decodes at length 2.
	if got, want := encHex(t, rows, "vec", `{"arr":[["a"],[]]}`), "0606020a61070e0707"; got != want {
		t.Errorf("a trailing empty wrapper row must keep its frame: got %s, want %s", got, want)
	}
	// [[],["a"]]: now the empty row is INTERIOR -- not framed, an id gap, and the
	// decoder restores it as an empty row at index 0.
	if got, want := encHex(t, rows, "vec", `{"arr":[[],["a"]]}`), "060e020a610707"; got != want {
		t.Errorf("an interior empty wrapper row must leave a gap: got %s, want %s", got, want)
	}
	// (encoding/json renders an empty Go slice as null, so an empty row reads as
	// null on the way back -- the LENGTH is what these pin.)
	for _, c := range []struct{ in, want string }{
		{`{"arr":[["a"],[]]}`, `{"arr":[["a"],null]}`},
		{`{"arr":[[],["a"]]}`, `{"arr":[null,["a"]]}`},
		{`{"arr":[["a","b"],[],["c"]]}`, `{"arr":[["a","b"],null,["c"]]}`},
	} {
		if got := roundTrip(t, rows, "vec", c.in); got != normJSON(t, c.want) {
			t.Errorf("wrapper-row round-trip %s: got %s, want %s", c.in, got, normJSON(t, c.want))
		}
	}

	mat := buildGoHarnessCfg(t, corelib, matDef, nil)
	// [[1],[]]: id 0 unsigned array (03 01 01), then the LAST row as an empty
	// count-prefixed array at id 1 (0b 00).
	if got, want := encHex(t, mat, "vec", `{"arr":[[1],[]]}`), "060301010b0007"; got != want {
		t.Errorf("a trailing empty native row must be written: got %s, want %s", got, want)
	}
	// [[],[1]]: the interior empty row is dropped, so id 0 is a gap.
	if got, want := encHex(t, mat, "vec", `{"arr":[[],[1]]}`), "060b010107"; got != want {
		t.Errorf("an interior empty native row must leave a gap: got %s, want %s", got, want)
	}
	for _, c := range []struct{ in, want string }{
		// a row that was WRITTEN comes back as an empty slice; one restored from a
		// gap is nil, which encoding/json renders as null. Same value, same length.
		{`{"arr":[[1],[]]}`, `{"arr":[[1],[]]}`},
		{`{"arr":[[],[1]]}`, `{"arr":[null,[1]]}`},
		{`{"arr":[[1,2],[],[3]]}`, `{"arr":[[1,2],null,[3]]}`},
	} {
		if got := roundTrip(t, mat, "vec", c.in); got != normJSON(t, c.want) {
			t.Errorf("native-row round-trip %s: got %s, want %s", c.in, got, normJSON(t, c.want))
		}
	}
}

// TestWireCompactArrayKeepsItsTail is the compact-array half of the same
// principle: a count-prefixed scalar array carries EVERY element it holds, trailing
// defaults included, because the wire count M IS the length -- dropping a trailing
// zero would shorten the array. [1,2,0,0] and [1,2] are different values.
// 030401020000 is the regenerated shared vector array_unsigned_trailing_defaults,
// whose dense and sparse forms are now identical.
func TestWireCompactArrayKeepsItsTail(t *testing.T) {
	corelib := requireGoCorelib(t)
	def := "version: 1\nmessages:\n  vec:\n    payload:\n" +
		"      arr: {id: 0, type: array, items: {type: u32, count: 4}}\n"
	bin := buildGoHarnessCfg(t, corelib, def, nil)

	for _, c := range []struct{ in, wantHex string }{
		{`{"arr":[1,2,0,0]}`, "030401020000"},
		{`{"arr":[1,2]}`, "03020102"},
		// all-zero at length 4 is a length-4 array: it differs from the empty
		// default, so it stays on the wire (a capacity is not a minimum length).
		{`{"arr":[0,0,0,0]}`, "030400000000"},
		// only the EMPTY array is the field's default, and only it is omitted.
		{`{"arr":[]}`, ""},
	} {
		if got := encHex(t, bin, "vec", c.in); got != c.wantHex {
			t.Errorf("encode %s: got %s, want %s", c.in, got, c.wantHex)
		}
		want := c.in
		if c.wantHex == "" {
			want = `{"arr":null}` // an omitted field decodes to the empty slice
		}
		if got := roundTrip(t, bin, "vec", c.in); got != normJSON(t, want) {
			t.Errorf("round-trip %s: got %s, want %s", c.in, got, normJSON(t, want))
		}
	}
}

// arrLen decodes hexBytes and returns the length of the top-level "arr" field,
// treating a JSON null (Go's rendering of an empty/nil slice) as length 0.
func arrLen(t *testing.T, bin, msg, hexBytes string) int {
	t.Helper()
	raw, err := hex.DecodeString(hexBytes)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "decode", msg)
	cmd.Stdin = bytes.NewReader(raw)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("decode %s: %v", msg, err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("decode json %q: %v", out, err)
	}
	if m["arr"] == nil {
		return 0
	}
	return len(m["arr"].([]any))
}

// decExpectErr feeds hex bytes to `harness decode` and requires a non-zero exit
// (the generated decode surfaced an error).
func decExpectErr(t *testing.T, bin, msg, hexBytes string) {
	t.Helper()
	raw, err := hex.DecodeString(hexBytes)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "decode", msg)
	cmd.Stdin = bytes.NewReader(raw)
	if out, err := cmd.Output(); err == nil {
		t.Fatalf("decode %s %s: expected a decode error (INVALID per MESSAGE_SPEC §3+§7), got %s", msg, hexBytes, out)
	}
}

// --- over-count scalar arrays (MESSAGE_SPEC §3+§7, generator#100) ----------

// TestOverCountScalarArrayRejected: a count-prefixed scalar array whose wire
// element count exceeds the schema `count` capacity N must fail the whole
// decode (INVALID) — no clamp, no keep-all. `count == N` still decodes, and a
// count-less (dynamic) array keeps every element.
func TestOverCountScalarArrayRejected(t *testing.T) {
	corelib := requireGoCorelib(t)
	def := "version: 1\nmessages:\n  vec:\n    payload:\n" +
		"      arr: {id: 0, type: array, items: {type: u8, count: 5}}\n"
	bin := buildGoHarnessCfg(t, corelib, def, nil)

	// Control: exactly N elements decode (issue #100 reproducer, control).
	// A []uint8 field is []byte to encoding/json, so it renders as base64:
	// "AQIDBAU=" == 01 02 03 04 05.
	if got := decJSON(t, bin, "vec", "03050102030405"); got != normJSON(t, `{"arr":"AQIDBAU="}`) {
		t.Errorf("control (count == N) must decode: got %s", got)
	}
	// Over-count by one: 6 elements against count: 5 must reject.
	decExpectErr(t, bin, "vec", "0306010203040506")

	// A dynamic (count-less) array has no N: keep-all stays correct. u16 keeps
	// the same unsigned-array wire form but renders as a JSON array.
	dynDef := "version: 1\nmessages:\n  vec:\n    payload:\n" +
		"      arr: {id: 0, type: array, items: {type: u16}}\n"
	dynBin := buildGoHarnessCfg(t, corelib, dynDef, nil)
	if got := arrLen(t, dynBin, "vec", "0306010203040506"); got != 6 {
		t.Errorf("dynamic array must keep all 6 elements, got %d", got)
	}
}

// TestCountedArrayOmittedOnlyWhenItEqualsItsDefault: the field-level omit test for
// a `count: N` native array is the ordinary != default test of MESSAGE_SPEC §2,
// applied to the value exactly as it stands. `count` is a capacity, so nothing is
// padded to N on either side of that comparison (§3):
//
//   - the EMPTY array is the default when none is declared -> omitted;
//   - an all-ZERO array of length N is a length-N value, which differs from the
//     empty one -> on the wire, every element of it (this is the behaviour the
//     superseded fixed-length reading got wrong in the other direction: it called
//     [0,0,0,0,0] "all-default" and dropped the field);
//   - a declared `default` shorter than N stands for itself -> the field is
//     omitted only when the value equals it exactly.
func TestCountedArrayOmittedOnlyWhenItEqualsItsDefault(t *testing.T) {
	corelib := requireGoCorelib(t)
	def := "version: 1\nmessages:\n  probe:\n    payload:\n" +
		"      u16s: {id: 0, type: array, items: {type: u16, count: 5}}\n" +
		"      i8s:  {id: 1, type: array, items: {type: i8, count: 5}}\n" +
		"      f32s: {id: 2, type: array, items: {type: fp32, count: 3}}\n" +
		"      bls:  {id: 3, type: array, items: {type: boolean, count: 4}}\n" +
		"      u32s: {id: 4, type: array, items: {type: u32, count: 5}}\n" +
		"      withdef: {id: 5, type: array, items: {type: u32, count: 3}, default: [1, 2]}\n"
	bin := buildGoHarnessCfg(t, corelib, def, nil)

	// (a) Every array at its default -> empty payload. The default is the EMPTY
	//     array everywhere but withdef, which equals its own declared [1,2].
	allDefault := `{"u16s":[],"i8s":[],"f32s":[],"bls":[],"u32s":[],"withdef":[1,2]}`
	if got := encHex(t, bin, "probe", allDefault); got != "" {
		t.Errorf("arrays equal to their default must be omitted: got %q", got)
	}

	// (b) An all-ZERO array is NOT the empty array: it is a length-N value and
	//     stays on the wire, tail included. id 0 unsigned array (03) count 5 + five 00;
	//     id 3 boolean array lowers to unsigned (0x1b) count 4 + four 00.
	allZero := `{"u16s":[0,0,0,0,0],"i8s":[],"f32s":[],"bls":[false,false,false,false],"u32s":[],"withdef":[1,2]}`
	if got, want := encHex(t, bin, "probe", allZero), "030500000000001b0400000000"; got != want {
		t.Errorf("an all-zero count:N array must stay on the wire: got %q, want %q", got, want)
	}

	// (c) A populated array is written whole -- the trailing default run is part of
	//     the value. id 4 ARRAY_UNSIGNED: header (4<<3)|3 = 0x23, count 5.
	oneSet := `{"u16s":[],"i8s":[],"f32s":[],"bls":[],"u32s":[7,8,9,0,0],"withdef":[1,2]}`
	if got, want := encHex(t, bin, "probe", oneSet), "23050708090000"; got != want {
		t.Errorf("a populated count:N array must keep its tail: got %q, want %q", got, want)
	}

	// (d) A count:N array overriding its non-empty schema default is on the wire.
	overDef := `{"u16s":[],"i8s":[],"f32s":[],"bls":[],"u32s":[],"withdef":[1,2,9]}`
	if got := encHex(t, bin, "probe", overDef); got == "" {
		t.Error("a count:N array overriding its schema default must be on the wire")
	}
	// ...and every probe round-trips at exactly the length it was given -- no
	// element added, none dropped. (encoding/json renders an empty Go slice as
	// null, which is how an omitted field comes back.)
	for _, c := range []struct{ in, want string }{
		{allDefault, `{"u16s":null,"i8s":null,"f32s":null,"bls":null,"u32s":null,"withdef":[1,2]}`},
		{allZero, `{"u16s":[0,0,0,0,0],"i8s":null,"f32s":null,"bls":[false,false,false,false],"u32s":null,"withdef":[1,2]}`},
		{oneSet, `{"u16s":null,"i8s":null,"f32s":null,"bls":null,"u32s":[7,8,9,0,0],"withdef":[1,2]}`},
		{overDef, `{"u16s":null,"i8s":null,"f32s":null,"bls":null,"u32s":null,"withdef":[1,2,9]}`},
	} {
		if got := roundTrip(t, bin, "probe", c.in); got != normJSON(t, c.want) {
			t.Errorf("round-trip %s: got %s, want %s", c.in, got, normJSON(t, c.want))
		}
	}
}

// --- fixlen-array subtype before the count bound (generator#259) -----------

// TestFixlenArrayBoundAppliesOnlyToItsOwnSubtype is the behavioral half of
// generator#259 / Crucible F-0042, run against the real corelib: a fixlen array
// header carries its element subtype in a fixlen_word AFTER the count, and that
// subtype -- not the count -- decides whether the header is the declared field's
// value at all. An array whose subtype contradicts the declaration is skipped
// (MESSAGE_SPEC §7.3), so the schema `count` capacity must NOT be applied to its
// count: the field was never that array's value.
//
// The wire bytes below are hand-built. `05`/`0d` are the field headers for id 0
// and id 1 with the fixlen-array wire type (0b101); `03` is id 0 with the
// unsigned-array type (0b011). `20` is the fixlen_word for fp32 (subtype 0,
// elem_len 4) and `41` the one for fp64 (subtype 1, elem_len 8).
func TestFixlenArrayBoundAppliesOnlyToItsOwnSubtype(t *testing.T) {
	corelib := requireGoCorelib(t)
	def := "version: 1\nmessages:\n  probe:\n    payload:\n" +
		"      f32s: {id: 0, type: array, items: {type: fp32, count: 5}}\n" +
		"      f64s: {id: 1, type: array, items: {type: fp64, count: 5}}\n"
	bin := buildGoHarnessCfg(t, corelib, def, nil)

	const (
		z32 = "0000000000000000000000000000000000000000000000000000000000000000" // 32 zero bytes
		z64 = z32 + z32                                                          // 64 zero bytes
		z24 = "000000000000000000000000000000000000000000000000"                 // 24 zero bytes
		z12 = "000000000000000000000000"                                         // 12 zero bytes
		z8  = "0000000000000000"                                                 // 8 zero bytes
	)
	// Everything below must ACCEPT: the header is not this field's value, so it
	// is consumed and dropped, the declared field keeps its default (null), and
	// no capacity is judged -- not even when the mis-typed count is over it.
	for _, c := range []struct{ name, in string }{
		// THE PRIMARY VECTOR: 8 fp64 elements at a declared array<fp32, count 5>.
		{"over-count fp64 at the fp32 slot", "05" + "08" + "41" + z64},
		{"in-count fp64 at the fp32 slot", "05" + "03" + "41" + z24},
		// The mirror: 8 fp32 elements at a declared array<fp64, count 5>.
		{"over-count fp32 at the fp64 slot", "0d" + "08" + "20" + z32},
		// One step earlier on the wire: an integer-array header at a fixlen slot
		// is the same §7.3 skip, and its count is likewise not this field's.
		{"over-count unsigned array at the fp32 slot", "03" + "08" + z8},
		// A zero-count fixlen array still carries its fixlen_word, so the header
		// still resolves to the contradicting subtype and is still skipped.
		{"zero-count fp64 at the fp32 slot", "05" + "00" + "41"},
	} {
		if got := decJSON(t, bin, "probe", c.in); got != normJSON(t, `{"f32s":null,"f64s":null}`) {
			t.Errorf("%s: must be skipped with the declared fields untouched, got %s", c.name, got)
		}
	}
	// THE CONTROL that proves the bound was re-keyed and not removed: the same
	// count of 8 under the MATCHING subtype is over the capacity of 5 -> INVALID.
	decExpectErr(t, bin, "probe", "05"+"08"+"20"+z32)
	decExpectErr(t, bin, "probe", "0d"+"08"+"41"+z64)
	// ...and the matching subtype within the capacity still delivers its value,
	// including the empty array an in-bound zero count stands for.
	for _, c := range []struct{ in, want string }{
		{"05" + "03" + "20" + z12, `{"f32s":[0,0,0],"f64s":null}`},
		{"0d" + "03" + "41" + z24, `{"f32s":null,"f64s":[0,0,0]}`},
		{"05" + "00" + "20", `{"f32s":[],"f64s":null}`},
	} {
		if got := decJSON(t, bin, "probe", c.in); got != normJSON(t, c.want) {
			t.Errorf("matching subtype %s: got %s, want %s", c.in, got, normJSON(t, c.want))
		}
	}
	// §7.4: an occurrence skipped under §7.3 is not an occurrence, so a correctly
	// typed earlier array survives a mis-typed later one at the same id.
	if got := decJSON(t, bin, "probe", "05"+"03"+"20"+z12+"05"+"02"+"41"+"0000000000000000"+z8); got != normJSON(t, `{"f32s":[0,0,0],"f64s":null}`) {
		t.Errorf("a skipped later occurrence must not clobber the earlier value, got %s", got)
	}
}
