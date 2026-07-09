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
//   - "empty != absent": an explicit [] overrides a non-empty default.

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

// TestEmptyArrayIsEmptySequence pins the spec rule that "an empty wrapper (a
// sequence with no children) is the explicit empty array": an explicit [] of a
// sequence-typed element (string) is written as a real, non-empty wire object (an
// empty wrapper sequence) -- shorter than a populated one and NOT dropped -- and
// round-trips as an empty array. (A string array lowers to a sequence; an empty
// numeric array has no legal native encoding, so string is the right probe.)
func TestEmptyArrayWireIsEmptySequence(t *testing.T) {
	corelib := requireGoCorelib(t)
	def := "version: 1\nmessages:\n  vec:\n    payload:\n" +
		"      arr: {id: 0, type: array, items: {type: string, count: 3, maxlen: 8}}\n"
	bin := buildGoHarnessCfg(t, corelib, def, nil)

	empty := encHex(t, bin, "vec", `{"arr":[]}`)
	one := encHex(t, bin, "vec", `{"arr":["x"]}`)
	if empty == "" {
		t.Error("an explicit empty array must be written as an empty wrapper sequence, got empty payload")
	}
	if len(empty) >= len(one) {
		t.Errorf("an empty array must be shorter on the wire than a populated one: empty=%s one=%s", empty, one)
	}
	// Round-trips: empty decodes to a zero-length array (Go renders it as null),
	// the populated one to length 1.
	if n := arrLen(t, bin, "vec", empty); n != 0 {
		t.Errorf("empty array must decode to length 0, got %d", n)
	}
	if n := arrLen(t, bin, "vec", one); n != 1 {
		t.Errorf("one-element array must decode to length 1, got %d", n)
	}
}

// TestWrapperArrayStringElementSparse pins MESSAGE_SPEC §2 element-level omission:
// inside a wrapper-sequence array a string element equal to its element default
// (empty) is dropped, leaving an id gap the decoder restores; trailing default
// elements collapse. The hex here is the cross-language canonical form (verified
// byte-identical against C, C++, Rust, Python, TypeScript, Java, C#).
func TestWrapperArrayStringElementSparse(t *testing.T) {
	corelib := requireGoCorelib(t)
	def := "version: 1\nmessages:\n  vec:\n    payload:\n" +
		"      arr: {id: 0, type: array, items: {type: string, count: 4, maxlen: 8}}\n"
	bin := buildGoHarnessCfg(t, corelib, def, nil)

	cases := []struct {
		in, wantHex, wantJSON string
	}{
		// gap at index 1: seq_begin(0), str(0)="a", str(2)="c", seq_end
		{`{"arr":["a","","c"]}`, "06020a61120a6307", `{"arr":["a","","c"]}`},
		// trailing default collapses: only str(0)="a" survives
		{`{"arr":["a",""]}`, "06020a6107", `{"arr":["a"]}`},
		// all-default array is an empty wrapper sequence (decodes to nil slice)
		{`{"arr":["",""]}`, "0607", `{"arr":null}`},
		// leading gap kept (str(1)="x"), trailing default collapses
		{`{"arr":["","x",""]}`, "060a0a7807", `{"arr":["","x"]}`},
	}
	for _, c := range cases {
		if got := encHex(t, bin, "vec", c.in); got != c.wantHex {
			t.Errorf("encode %s: got %s, want %s", c.in, got, c.wantHex)
		}
		if got := roundTrip(t, bin, "vec", c.in); got != normJSON(t, c.wantJSON) {
			t.Errorf("round-trip %s: got %s, want %s", c.in, got, normJSON(t, c.wantJSON))
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
