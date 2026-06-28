package c

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/sofa-buffers/generator/internal/analysis"
	"github.com/sofa-buffers/generator/internal/ir"
	"github.com/sofa-buffers/generator/internal/model"
	"github.com/sofa-buffers/generator/internal/parser"
)

// M4 conformance: drive the generated C encoder against the corelib's
// language-agnostic shared test vectors (assets/test_vectors.json) and assert
// byte-exact output.
//
// Scope note: the C object.h encoder is *sparse* — it omits zero/default fields
// (for forward-compat + footprint) and treats blobs as fixed-size storage. So
// byte-exact vector matching applies to NON-ZERO scalar/string vectors at id 0;
// blob/array/zero cases are covered by the round-trip harness (M2/M3) instead.

type vectorFile struct {
	Vectors []vector `json:"vectors"`
}
type vector struct {
	Name       string        `json:"name"`
	Group      string        `json:"group"`
	Offset     int           `json:"offset"`
	Fields     []vectorField `json:"fields"`
	Serialized struct {
		Hex string `json:"hex"`
	} `json:"serialized"`
}
type vectorField struct {
	Op    string          `json:"op"`
	ID    int64           `json:"id"`
	Value json.RawMessage `json:"value"`
}

func TestSharedVectorConformance(t *testing.T) {
	corelib := os.Getenv("SOFAB_C_CORELIB")
	if corelib == "" {
		t.Skip("set SOFAB_C_CORELIB to run shared-vector conformance")
	}
	if _, err := exec.LookPath("make"); err != nil {
		t.Skip("make not found")
	}
	raw, err := os.ReadFile(filepath.Join(corelib, "assets", "test_vectors.json"))
	if err != nil {
		t.Skipf("no shared vectors: %v", err)
	}
	var vf vectorFile
	if err := json.Unmarshal(raw, &vf); err != nil {
		t.Fatalf("parse vectors: %v", err)
	}

	// One generated harness per scalar op type; reuse it across matching vectors.
	groups := map[string]string{
		"unsigned": "u64",
		"signed":   "i64",
		"fp32":     "fp32",
		"fp64":     "fp64",
		"string":   "string",
	}
	harnesses := map[string]string{}
	for op, typ := range groups {
		def := scalarDef(typ)
		h, err := buildHarness(t, corelib, def)
		if err != nil {
			t.Fatalf("build harness for %s: %v", op, err)
		}
		harnesses[op] = h
	}

	checked, skipped := 0, 0
	for _, v := range vf.Vectors {
		if len(v.Fields) != 1 || v.Offset != 0 {
			skipped++
			continue
		}
		f := v.Fields[0]
		harness, ok := harnesses[f.Op]
		if !ok || f.ID != 0 {
			skipped++
			continue
		}
		jsonIn, ok := scalarJSON(f)
		if !ok {
			skipped++
			continue
		}
		got := runEncode(t, harness, jsonIn)
		if got == "" { // zero-valued -> sparse encoder omits it; not a vector match case
			skipped++
			continue
		}
		if got != v.Serialized.Hex {
			t.Errorf("vector %q (%s): got %s, want %s", v.Name, v.Group, got, v.Serialized.Hex)
			continue
		}
		checked++
	}
	t.Logf("shared-vector conformance: %d byte-exact, %d skipped (zero/blob/array/multi-field/non-id0)", checked, skipped)
	if checked == 0 {
		t.Fatal("no vectors were checked — conformance proved nothing")
	}
}

func scalarDef(typ string) string {
	extra := ""
	if typ == "string" {
		extra = ", maxlen: 4096"
	}
	return fmt.Sprintf("version: 1\nmessages:\n  vec:\n    payload:\n      a: {id: 0, type: %s%s}\n", typ, extra)
}

// scalarJSON converts a vector field's value into the harness's canonical JSON
// input {"a": value}; returns false if the value should be skipped (zero/inf).
func scalarJSON(f vectorField) (string, bool) {
	s := strings.TrimSpace(string(f.Value))
	switch f.Op {
	case "unsigned", "signed":
		n, err := strconv.ParseFloat(strings.Trim(s, `"`), 64)
		if err == nil && n == 0 {
			return "", false
		}
		return `{"a":` + strings.Trim(s, `"`) + `}`, true
	case "fp32", "fp64":
		if strings.Contains(s, "inf") || s == "0" || s == "0.0" {
			return "", false
		}
		return `{"a":` + s + `}`, true
	case "string":
		if s == `""` {
			return "", false
		}
		return `{"a":` + s + `}`, true
	}
	return "", false
}

// ---- harness build/run helpers -----------------------------------------

func schemaFromYAML(t *testing.T, src string) *ir.Schema {
	t.Helper()
	doc, err := parser.Parse([]byte(src), "vec.yaml")
	if err != nil {
		t.Fatal(err)
	}
	resolved, _ := doc.Resolve()
	if errs := parser.Validate(resolved); errs != nil {
		t.Fatalf("def invalid: %v", errs)
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

func buildHarness(t *testing.T, corelib, def string) (string, error) {
	t.Helper()
	s := schemaFromYAML(t, def)
	files, err := (&Backend{}).Generate(s, map[string]any{"emit": "project", "symbol_prefix": "sofab_"})
	if err != nil {
		return "", err
	}
	dir := t.TempDir()
	for _, f := range files {
		full := filepath.Join(dir, f.Path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return "", err
		}
		if err := os.WriteFile(full, f.Content, 0o644); err != nil {
			return "", err
		}
	}
	out, err := exec.Command("make", "-C", dir, "SOFAB_C_CORELIB="+corelib).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("build: %v\n%s", err, out)
	}
	return filepath.Join(dir, "harness", "harness"), nil
}

func runEncode(t *testing.T, harness, jsonIn string) string {
	t.Helper()
	cmd := exec.Command(harness, "encode", "vec")
	cmd.Stdin = strings.NewReader(jsonIn)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("encode %q: %v", jsonIn, err)
	}
	return hex.EncodeToString(out)
}
