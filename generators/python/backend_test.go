package python

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
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

func schemaFile(t *testing.T, path string) *ir.Schema {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return schema(t, string(b))
}

func genPy(t *testing.T, s *ir.Schema, cfg map[string]any) map[string][]byte {
	t.Helper()
	files, err := (&Backend{}).Generate(s, cfg)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	out := map[string][]byte{}
	for _, f := range files {
		out[f.Path] = f.Content
	}
	return out
}

func TestPythonStructural(t *testing.T) {
	mod := string(genPy(t, schemaFile(t, "../../examples/messages/example.yaml"), map[string]any{})["message.py"])
	for _, want := range []string{
		// example.yaml has count-bearing native arrays, so the over-count guard
		// (generator#100) pulls in SofaDecodeError.
		"from sofab import Encoder, Decoder, SofaDecodeError, WireType",
		"@dataclass",
		"class Myfirstmessage:",
		"def _marshal(self, e: Encoder)",
		"def _unmarshal(self, d: Decoder)",
		"class MyfirstmessageSomeenum(IntEnum):",
		"def to_jsonable(self)",
		"e.write_sequence_begin(",
		"if fld.count > 4:", // over-count scalar array rejected at the count header (generator#100/#216)
		`raise SofaDecodeError("someuintarray: array count above schema capacity 4")`,
	} {
		if !strings.Contains(mod, want) {
			t.Errorf("message.py missing %q", want)
		}
	}
}

// TestPythonDecodeLimits: the max_dyn_* config keys bake receiver-side decode
// limits (generator#102) into the generated module -- named constants at
// module level plus Decoder(max_array_count=..., ...) kwargs in every decode.
// The cap is raised to the largest schema bound of its kind (escape hatch:
// schema-bounded fields stay governed by their own bound), an unset key emits
// nothing, and a key whose kind has no unbounded field is inert.
func TestPythonDecodeLimits(t *testing.T) {
	const src = `
version: 1
messages:
  dyn:
    payload:
      s:    { id: 0, type: string }
      arr:  { id: 1, type: array, items: { type: u64 } }
      barr: { id: 2, type: array, items: { type: i32, count: 100000 } }
`
	s := schema(t, src)
	mod := string(genPy(t, s, map[string]any{
		"max_dyn_array_count": 65536,
		"max_dyn_string_len":  4096,
		"max_dyn_blob_len":    2048, // no unbounded blob in the schema -> inert
	})["message.py"])
	for _, want := range []string{
		"MAX_DYN_ARRAY_COUNT = 100000", // raised to the schema count of barr
		"MAX_DYN_STRING_LEN = 4096",
		"o._unmarshal(Decoder(io.BytesIO(data), max_array_count=MAX_DYN_ARRAY_COUNT, max_string_len=MAX_DYN_STRING_LEN))",
	} {
		if !strings.Contains(mod, want) {
			t.Errorf("message.py missing %q", want)
		}
	}
	if strings.Contains(mod, "MAX_DYN_BLOB_LEN") || strings.Contains(mod, "max_blob_len") {
		t.Error("inert blob limit must not be emitted (no unbounded blob)")
	}

	// No limits configured -> byte-identical plumbing-free output.
	plain := string(genPy(t, s, map[string]any{})["message.py"])
	if strings.Contains(plain, "MAX_DYN") || strings.Contains(plain, "max_array_count") {
		t.Error("unset limits must emit no limit plumbing")
	}
	if !strings.Contains(plain, "o._unmarshal(Decoder(io.BytesIO(data)))") {
		t.Error("unset limits must leave the plain Decoder call unchanged")
	}
}

// TestPythonMetadataDocs: enum-constant and bitfield-flag descriptions render as
// Sphinx "#:" attribute comments (flags append a "(default: true/false)" note),
// and a deprecated field carries a ".. deprecated::" directive in its doc.
// TestPythonOverIndexWrapperArray: a fixed-count wrapper array (string/blob/
// struct elements) raises SofaDecodeError for an element id >= N before the list
// grows (issue #142 / MESSAGE_SPEC §5.1/§7). A dynamic array keeps every index.
func TestPythonOverIndexWrapperArray(t *testing.T) {
	const src = `
version: 1
messages:
  M:
    payload:
      bs: { id: 0, type: array, items: { type: string, count: 4, maxlen: 16 } }
      bb: { id: 1, type: array, items: { type: blob,   count: 3, maxlen: 16 } }
      bp: { id: 2, type: array, items: { type: struct, count: 2, fields: { x: { id: 0, type: i32 } } } }
      ds: { id: 3, type: array, items: { type: string } }
`
	mod := string(genPy(t, schema(t, src), map[string]any{})["message.py"])
	// The over-index guard raises SofaDecodeError, so the on-demand import MUST be
	// emitted even when the schema has no scalar over-count array (the #100 case) —
	// a wrapper-only schema like this one. Missing it is a NameError at decode time.
	if !strings.Contains(mod, "from sofab import Encoder, Decoder, SofaDecodeError, WireType") {
		t.Error("message.py must import SofaDecodeError for the over-index guard (else NameError at decode)")
	}
	for _, want := range []string{
		`if _ef0.id >= 4:`,
		`raise SofaDecodeError("self.bs: array index above schema capacity 4")`,
		`raise SofaDecodeError("self.bb: array index above schema capacity 3")`,
		`raise SofaDecodeError("self.bp: array index above schema capacity 2")`,
	} {
		if !strings.Contains(mod, want) {
			t.Errorf("message.py missing over-index guard %q", want)
		}
	}
	// Dynamic string array keeps every index — no guard raised for it.
	if strings.Contains(mod, `raise SofaDecodeError("self.ds: array index above schema capacity`) {
		t.Errorf("dynamic string array must not carry an over-index guard")
	}
}

// TestPythonMaxlenReject: a bounded (maxlen) string/blob whose wire BYTE length
// exceeds its schema maxlen is malformed and MUST raise SofaDecodeError on
// decode — never silently truncated (MESSAGE_SPEC §7.1). Covers scalar fields
// and (dynamic) wrapper-array string elements. The schema below has NO counted
// native array and NO counted wrapper array, so the maxlen guard is the ONLY
// thing pulling in SofaDecodeError — a regression check on the on-demand import
// (a bounded-string-only schema that missed the import would NameError at
// decode).
func TestPythonMaxlenReject(t *testing.T) {
	const src = `
version: 1
messages:
  M:
    payload:
      s:   { id: 0, type: string, maxlen: 8 }
      b:   { id: 1, type: blob,   maxlen: 8 }
      arr: { id: 2, type: array, items: { type: string, maxlen: 5 } }
      us:  { id: 3, type: string }
`
	mod := string(genPy(t, schema(t, src), map[string]any{})["message.py"])

	// (a) The maxlen guard raises SofaDecodeError, so the on-demand import MUST be
	// present even though this schema has no counted native/wrapper array — the
	// import bug this test guards against.
	if !strings.Contains(mod, "from sofab import Encoder, Decoder, SofaDecodeError, WireType") {
		t.Error("message.py must import SofaDecodeError for the maxlen guard (else NameError at decode)")
	}

	for _, want := range []string{
		// (b) scalar string: bound the wire byte length (non-consuming peek), not a
		// re-encode of the decoded str (#155).
		`if d.fixlen_len() > 8:`,
		`raise SofaDecodeError("s: string byte length above schema maxlen 8")`,
		// (b) scalar blob: byte length of the bytes value.
		`if len(self.b) > 8:`,
		`raise SofaDecodeError("b: blob byte length above schema maxlen 8")`,
		// (c) bounded wrapper string element (maxlen 5): wire byte length peek.
		`if d.fixlen_len() > 5:`,
		`raise SofaDecodeError("self.arr: string element byte length above schema maxlen 5")`,
	} {
		if !strings.Contains(mod, want) {
			t.Errorf("message.py missing maxlen guard %q", want)
		}
	}

	// (d) the string maxlen check must never re-encode the decoded str (#155).
	if strings.Contains(mod, `.encode("utf-8")`) {
		t.Error(`string maxlen check must not re-encode via .encode("utf-8") (#155)`)
	}

	// (e) the unbounded string field carries no maxlen guard.
	if strings.Contains(mod, `raise SofaDecodeError("us:`) {
		t.Error("unbounded string must not raise a maxlen SofaDecodeError")
	}
	if strings.Contains(mod, `raise SofaDecodeError("us:`) {
		t.Error("unbounded string must not raise a maxlen SofaDecodeError")
	}
}

func TestPythonMetadataDocs(t *testing.T) {
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
      legacyId: { id: 0, type: u32, description: "Old identifier retained for backward compatibility.", deprecated: true }
      mode:     { id: 1, type: enum, enum: { $ref: "#/$defs/enum/Mode" } }
      status:   { id: 2, type: bitfield, bits: { $ref: "#/$defs/bitfield/StatusFlags" } }
`
	mod := string(genPy(t, schema(t, src), map[string]any{})["message.py"])
	for _, want := range []string{
		// enum-constant descriptions
		"    #: Node is powered down.\n    OFF = 0",
		"    #: Node is sampling and transmitting.\n    ACTIVE = 1",
		// bitfield flag description + default note (and no-default flag)
		"    #: Node has completed initialization. (default: true)\n    READY = 1 << 0",
		"    #: Core temperature exceeded the safe threshold.\n    OVERHEATED = 1 << 1",
		// deprecated field doc: description then a Sphinx deprecated directive
		"    #: Old identifier retained for backward compatibility.\n    #: .. deprecated::",
	} {
		if !strings.Contains(mod, want) {
			t.Errorf("message.py missing %q", want)
		}
	}
	// A flag without a default must NOT carry a "(default:" note.
	if strings.Contains(mod, "safe threshold. (default:") {
		t.Error("no-default flag must not get a (default:) note")
	}
}

// TestPythonFixedCountTrailingDefaultRun: a `count: N` native array is
// FIXED-LENGTH (MESSAGE_SPEC §3) — encode elides the trailing element-default
// run, decode rebuilds it out to N. A dynamic (count-less) array has no N to
// refill from, so its trailing default is significant and must survive
// untouched (generator#136 / F-0010).
func TestPythonFixedCountTrailingDefaultRun(t *testing.T) {
	const src = `
version: 1
$defs:
  enum:
    Mode: { Off: { value: 0 }, Active: { value: 1 } }
messages:
  T:
    payload:
      fixedU32:  { id: 0, type: array, items: { type: u32, count: 5 } }
      fixedI16:  { id: 1, type: array, items: { type: i16, count: 3 } }
      fixedF32:  { id: 2, type: array, items: { type: fp32, count: 2 } }
      fixedF64:  { id: 3, type: array, items: { type: fp64, count: 2 } }
      fixedBool: { id: 4, type: array, items: { type: boolean, count: 4 } }
      fixedEnum: { id: 5, type: array, items: { type: enum, enum: { $ref: "#/$defs/enum/Mode" }, count: 2 } }
      dynU32:    { id: 6, type: array, items: { type: u32 } }
      dynStrs:   { id: 7, type: array, items: { type: string } }
`
	mod := string(genPy(t, schema(t, src), map[string]any{})["message.py"])
	for _, want := range []string{
		// helpers + the math import the float trim's bit-pattern compare needs
		"import math",
		"def _trim_tail(a: list, zero) -> list:",
		"def _trim_tail_float(a: list) -> list:",
		"def _pad_to(a: list, n: int, zero) -> list:",
		// encode: fixed native arrays trim their trailing default run
		"e.write_unsigned_array(0, _trim_tail(self.fixedU32, 0))",
		"e.write_signed_array(1, _trim_tail(self.fixedI16, 0))",
		"e.write_float32_array(2, _trim_tail_float(self.fixedF32))",
		"e.write_float64_array(3, _trim_tail_float(self.fixedF64))",
		"e.write_unsigned_array(4, _trim_tail([1 if _v else 0 for _v in self.fixedBool], 0))",
		"e.write_signed_array(5, _trim_tail([int(_v) for _v in self.fixedEnum], 0))",
		// decode: fixed native arrays refill to exactly the schema count
		"self.fixedU32 = _pad_to(self.fixedU32, 5, 0)",
		"self.fixedI16 = _pad_to(self.fixedI16, 3, 0)",
		"self.fixedF32 = _pad_to(self.fixedF32, 2, 0.0)",
		"self.fixedF64 = _pad_to(self.fixedF64, 2, 0.0)",
		"self.fixedBool = _pad_to(self.fixedBool, 4, False)",
		"self.fixedEnum = _pad_to(self.fixedEnum, 2, 0)",
	} {
		if !strings.Contains(mod, want) {
			t.Errorf("message.py missing %q", want)
		}
	}
	for _, bad := range []string{
		// dynamic arrays: no trim on encode, no fill on decode
		"e.write_unsigned_array(6, _trim_tail(self.dynU32, 0))",
		"_pad_to(self.dynU32",
		"_pad_to(self.dynStrs",
	} {
		if strings.Contains(mod, bad) {
			t.Errorf("message.py must not contain %q (dynamic arrays are unchanged)", bad)
		}
	}
	if !strings.Contains(mod, "e.write_unsigned_array(6, self.dynU32)") {
		t.Error("dynamic u32 array must encode untrimmed")
	}
	// The over-count guard must still reject a wire count > N, now decided at the
	// count header (fld.count) so INVALID dominates a truncated tail (generator#216).
	if !strings.Contains(mod, "if fld.count > 5:") {
		t.Error("over-count SofaDecodeError guard regressed")
	}
}

// TestPythonFixedCountDefaultIsNElements: a `count: N` native array is
// fixed-length, so its VALUE is always N elements — with no schema default that
// is N element defaults, and a short schema default leaves the unlisted trailing
// elements at the element default. Without this a fresh (or all-default, hence
// omitted) array would be an empty list here while the fixed-storage camp
// (`[T; N]` / `std::array<T, N>`) yields N zeros — the same MESSAGE_SPEC §3
// divergence as the trailing default run, reached through the omission path
// (generator#136 / F-0010).
func TestPythonFixedCountDefaultIsNElements(t *testing.T) {
	const src = `
version: 1
$defs:
  enum:
    Mode: { Off: { value: 0 }, Active: { value: 1 } }
messages:
  T:
    payload:
      noDflt:    { id: 0, type: array, items: { type: u32, count: 5 } }
      shortDflt: { id: 1, type: array, items: { type: u32, count: 5 }, default: [1, 2] }
      fullDflt:  { id: 2, type: array, items: { type: i32, count: 3 }, default: [1, 2, 3] }
      fixedF64:  { id: 3, type: array, items: { type: fp64, count: 3 } }
      fixedBool: { id: 4, type: array, items: { type: boolean, count: 4 } }
      fixedEnum: { id: 5, type: array, items: { type: enum, enum: { $ref: "#/$defs/enum/Mode" }, count: 2 } }
      dynU32:    { id: 6, type: array, items: { type: u32 } }
      dynStrs:   { id: 7, type: array, items: { type: string } }
      fixedStrs: { id: 8, type: array, items: { type: string, count: 3 } }
`
	mod := string(genPy(t, schema(t, src), map[string]any{})["message.py"])
	for _, want := range []string{
		// no schema default -> N element defaults, per element kind
		"noDflt: list[int] = field(default_factory=lambda: [0, 0, 0, 0, 0])",
		"fixedF64: list[float] = field(default_factory=lambda: [0.0, 0.0, 0.0])",
		"fixedBool: list[bool] = field(default_factory=lambda: [False, False, False, False])",
		"fixedEnum: list[EnumMode] = field(default_factory=lambda: [0, 0])",
		// a short schema default is tail-padded to N
		"shortDflt: list[int] = field(default_factory=lambda: [1, 2, 0, 0, 0])",
		// an already-N-long default is unchanged
		"fullDflt: list[int] = field(default_factory=lambda: [1, 2, 3])",
		// marshal's omit-when-default compare must use the SAME padded literal,
		// so an all-default (fresh) object still omits the field entirely.
		"if self.noDflt != [0, 0, 0, 0, 0]:",
		"if self.shortDflt != [1, 2, 0, 0, 0]:",
		"if self.fixedBool != [False, False, False, False]:",
		// dynamic + wrapper-sequence arrays keep starting empty
		"dynU32: list[int] = field(default_factory=list)",
		"dynStrs: list[str] = field(default_factory=list)",
		"fixedStrs: list[str] = field(default_factory=list)",
		"if len(self.dynU32) != 0:",
	} {
		if !strings.Contains(mod, want) {
			t.Errorf("message.py missing %q", want)
		}
	}
	// A dynamic native array has no N to pad to, so it must not gain a literal
	// default (that would also flip its marshal gate).
	if strings.Contains(mod, "dynU32: list[int] = field(default_factory=lambda:") {
		t.Error("a count-less array must not get a materialized default")
	}
}

// A schema whose only fixed-count native arrays are integral must not emit the
// float trim helper or import math (they would be dead code).
func TestPythonFixedCountNoFloatHelperWhenUnused(t *testing.T) {
	const src = `
version: 1
messages:
  T:
    payload:
      fixedU32: { id: 0, type: array, items: { type: u32, count: 3 } }
`
	mod := string(genPy(t, schema(t, src), map[string]any{})["message.py"])
	for _, bad := range []string{"import math", "_trim_tail_float"} {
		if strings.Contains(mod, bad) {
			t.Errorf("message.py must not contain %q for an all-integral schema", bad)
		}
	}
}

// A schema with no fixed-count native array must not emit the helpers at all.
func TestPythonNoFixedArrayHelpersWhenUnused(t *testing.T) {
	const src = `
version: 1
messages:
  T:
    payload:
      dynU32: { id: 0, type: array, items: { type: u32 } }
`
	mod := string(genPy(t, schema(t, src), map[string]any{})["message.py"])
	for _, bad := range []string{"_trim_tail", "_pad_to", "import math"} {
		if strings.Contains(mod, bad) {
			t.Errorf("message.py must not contain %q when no fixed-count array exists", bad)
		}
	}
}

func TestPythonSyntaxValid(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not found")
	}
	dir := t.TempDir()
	for path, content := range genPy(t, schemaFile(t, "../../examples/messages/example.yaml"), map[string]any{"emit": "project"}) {
		if err := os.WriteFile(filepath.Join(dir, path), content, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command(py, "-m", "py_compile", filepath.Join(dir, "message.py"), filepath.Join(dir, "harness.py"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generated Python is invalid:\n%s", out)
	}
}

// TestPythonConformance: byte-exact shared-vector conformance against corelib-py
// (no build step — Python just needs the corelib on PYTHONPATH). Gated on
// SOFAB_PY_CORELIB (a corelib-py checkout with src/ + assets/test_vectors.json).
func TestPythonConformance(t *testing.T) {
	corelib := os.Getenv("SOFAB_PY_CORELIB")
	if corelib == "" {
		t.Skip("set SOFAB_PY_CORELIB to a corelib-py checkout")
	}
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not found")
	}
	raw, err := os.ReadFile(filepath.Join(corelib, "assets", "test_vectors.json"))
	if err != nil {
		t.Skipf("no vectors: %v", err)
	}
	var vf struct {
		Vectors []struct {
			Name   string `json:"name"`
			Offset int    `json:"offset"`
			Fields []struct {
				Op    string          `json:"op"`
				ID    int64           `json:"id"`
				Value json.RawMessage `json:"value"`
			} `json:"fields"`
			Serialized struct {
				Hex string `json:"hex"`
			} `json:"serialized"`
		} `json:"vectors"`
	}
	if err := json.Unmarshal(raw, &vf); err != nil {
		t.Fatal(err)
	}

	groups := map[string]string{"unsigned": "u64", "signed": "i64", "fp32": "fp32", "fp64": "fp64", "string": "string"}
	dirs := map[string]string{}
	for op, typ := range groups {
		dirs[op] = g(t, typ)
	}
	pyEnv := append(os.Environ(), "PYTHONPATH="+filepath.Join(corelib, "src"))

	checked := 0
	for _, v := range vf.Vectors {
		if len(v.Fields) != 1 || v.Offset != 0 {
			continue
		}
		f := v.Fields[0]
		dir, ok := dirs[f.Op]
		if !ok || f.ID != 0 {
			continue
		}
		in, ok := scalarJSON(f.Op, string(f.Value))
		if !ok {
			continue
		}
		cmd := exec.Command(py, filepath.Join(dir, "harness.py"), "encode", "vec")
		cmd.Stdin = strings.NewReader(in)
		cmd.Env = pyEnv
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("encode %q: %v", in, err)
		}
		// Sparse-canonical (MESSAGE_SPEC S2): a field equal to its default is
		// omitted, so a default-valued single-field message encodes to empty. The
		// dense per-field vector is still validated for every non-default value.
		got := hex.EncodeToString(out)
		if pyValueIsDefault(f.Op, string(f.Value)) {
			if got != "" {
				t.Errorf("vector %q: default-valued field must be omitted (sparse), got %s", v.Name, got)
			} else {
				checked++
			}
		} else if got != v.Serialized.Hex {
			t.Errorf("vector %q: got %s want %s", v.Name, got, v.Serialized.Hex)
		} else {
			checked++
		}
	}
	t.Logf("Python shared-vector conformance: %d byte-exact", checked)
	if checked == 0 {
		t.Fatal("no vectors checked")
	}
}

// g generates a one-field project into a temp dir and returns it.
func g(t *testing.T, typ string) string {
	t.Helper()
	extra := ""
	if typ == "string" {
		extra = ", maxlen: 4096"
	}
	src := "version: 1\nmessages:\n  vec:\n    payload:\n      a: {id: 0, type: " + typ + extra + "}\n"
	dir := t.TempDir()
	for path, content := range genPy(t, schema(t, src), map[string]any{"emit": "project"}) {
		if err := os.WriteFile(filepath.Join(dir, path), content, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// pyValueIsDefault reports whether a shared-vector scalar value is the type
// default (zero / empty) -- which a sparse-canonical encoder omits.
func pyValueIsDefault(op, rawValue string) bool {
	s := strings.Trim(strings.TrimSpace(rawValue), `"`)
	switch op {
	case "unsigned", "signed":
		return s == "0"
	case "fp32", "fp64":
		return s == "0" || s == "0.0"
	case "string":
		return s == ""
	}
	return false
}

func scalarJSON(op, rawValue string) (string, bool) {
	s := strings.TrimSpace(rawValue)
	switch op {
	case "unsigned", "signed":
		return `{"a":` + strings.Trim(s, `"`) + `}`, true
	case "fp32", "fp64":
		if strings.Contains(s, "inf") {
			return "", false
		}
		return `{"a":` + s + `}`, true
	case "string":
		return `{"a":` + s + `}`, true
	}
	return "", false
}

// TestPythonWireTypeGuard pins the MESSAGE_SPEC §7.3 guard (generator#174): the
// generated dispatch compares each field header's wire type — plus the fixlen
// subtype where the wire type alone is ambiguous — against the type the schema
// declares, and skips the field like an unknown id on a mismatch. Without it the
// schema-typed reader is called for a type the field does not carry, which
// corelib-py correctly rejects with SofaStateError, failing the whole decode.
func TestPythonWireTypeGuard(t *testing.T) {
	s := schema(t, `
version: 1
messages:
  M:
    payload:
      a: { id: 0, type: u8 }
      b: { id: 1, type: i32 }
      c: { id: 2, type: boolean }
      d: { id: 3, type: fp32 }
      e: { id: 4, type: fp64 }
      f: { id: 5, type: string, maxlen: 8 }
      g: { id: 6, type: blob, maxlen: 8 }
      h: { id: 7, type: struct, fields: { x: { id: 0, type: u8 } } }
      i: { id: 8, type: array, items: { type: u32, count: 2 } }
      j: { id: 9, type: array, items: { type: i32, count: 2 } }
      k: { id: 10, type: array, items: { type: fp32, count: 2 } }
      l: { id: 11, type: array, items: { type: string, count: 2, maxlen: 4 } }
`)
	mod := string(genPy(t, s, map[string]any{})["message.py"])
	// FixlenSubtype is referenced by the fixlen guards, so it must be imported.
	if !strings.Contains(mod, "from sofab import Encoder, Decoder, SofaDecodeError, WireType, FixlenSubtype") {
		t.Errorf("message.py missing FixlenSubtype import:\n%s", mod)
	}
	for _, want := range []string{
		// Wire type alone settles the integer/bool kinds...
		"if fld.type != WireType.UNSIGNED:",
		"if fld.type != WireType.SIGNED:",
		// ...but fp32/fp64/string/blob all share FIXLEN, so the subtype decides.
		"if fld.type != WireType.FIXLEN or fld.subtype != FixlenSubtype.FP32:",
		"if fld.type != WireType.FIXLEN or fld.subtype != FixlenSubtype.FP64:",
		"if fld.type != WireType.FIXLEN or fld.subtype != FixlenSubtype.STRING:",
		"if fld.type != WireType.FIXLEN or fld.subtype != FixlenSubtype.BLOB:",
		// Nested messages and composite (wrapper) arrays open a sequence.
		"if fld.type != WireType.SEQUENCE_START:",
		// Native scalar arrays carry the matching ARRAY_* wire type; the fp array
		// shares ARRAY_FIXLEN with the other fixlen arrays, so it too needs the
		// subtype.
		"if fld.type != WireType.ARRAY_UNSIGNED:",
		"if fld.type != WireType.ARRAY_SIGNED:",
		"if fld.type != WireType.ARRAY_FIXLEN or fld.subtype != FixlenSubtype.FP32:",
		// A mismatch skips the field and resumes the loop — never falls through
		// into the reader below it.
		"                    d.skip()\n                    continue",
	} {
		if !strings.Contains(mod, want) {
			t.Errorf("message.py missing wire-type guard %q\n%s", want, mod)
		}
	}
}

// TestPythonNoFixlenSubtypeImportWhenUnused keeps the import line honest: a
// schema with no fixlen-framed field never references FixlenSubtype, so
// importing it would leave an unused name in every generated module.
func TestPythonNoFixlenSubtypeImportWhenUnused(t *testing.T) {
	s := schema(t, `
version: 1
messages:
  M:
    payload:
      a: { id: 0, type: u8 }
      b: { id: 1, type: i32 }
`)
	mod := string(genPy(t, s, map[string]any{})["message.py"])
	if strings.Contains(mod, "FixlenSubtype") {
		t.Errorf("message.py must not import FixlenSubtype when no fixlen field exists:\n%s", mod)
	}
	if !strings.Contains(mod, "from sofab import Encoder, Decoder, WireType") {
		t.Errorf("message.py missing plain import line:\n%s", mod)
	}
}
