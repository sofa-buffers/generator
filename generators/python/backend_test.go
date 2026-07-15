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
		"if len(self.someuintarray) > 4:", // over-count scalar array rejected (generator#100)
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
