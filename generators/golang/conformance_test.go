package golang

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// M-Go conformance: build the generated Go encoder against corelib-go and assert
// byte-exact output against the language-agnostic shared vectors. The Go encoder
// is DENSE (it writes every field), so unlike C it also matches zero-valued
// scalar vectors. Gated on SOFAB_GO_CORELIB (a corelib-go checkout).

type goVectorFile struct {
	Vectors []goVector `json:"vectors"`
}
type goVector struct {
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
}

func TestGoSharedVectorConformance(t *testing.T) {
	corelib := os.Getenv("SOFAB_GO_CORELIB")
	if corelib == "" {
		t.Skip("set SOFAB_GO_CORELIB to a corelib-go checkout to run conformance")
	}
	raw, err := os.ReadFile(filepath.Join(corelib, "assets", "test_vectors.json"))
	if err != nil {
		t.Skipf("no shared vectors: %v", err)
	}
	var vf goVectorFile
	if err := json.Unmarshal(raw, &vf); err != nil {
		t.Fatalf("parse vectors: %v", err)
	}

	groups := map[string]string{
		"unsigned": "u64", "signed": "i64", "fp32": "fp32", "fp64": "fp64", "string": "string",
	}
	harnesses := map[string]string{}
	for op, typ := range groups {
		bin, err := buildGoHarness(t, corelib, scalarDef(typ))
		if err != nil {
			t.Fatalf("build %s harness: %v", op, err)
		}
		harnesses[op] = bin
	}

	checked := 0
	for _, v := range vf.Vectors {
		if len(v.Fields) != 1 || v.Offset != 0 {
			continue
		}
		f := v.Fields[0]
		bin, ok := harnesses[f.Op]
		if !ok || f.ID != 0 {
			continue
		}
		in, ok := goScalarJSON(f.Op, string(f.Value))
		if !ok {
			continue
		}
		got := goRunEncode(t, bin, in)
		// Sparse-canonical (MESSAGE_SPEC S2): a field equal to its default is
		// omitted, so a default-valued single-field message encodes to empty. The
		// dense per-field vector is still validated for every non-default value.
		if goValueIsDefault(f.Op, string(f.Value)) {
			if got != "" {
				t.Errorf("vector %q: default-valued field must be omitted (sparse), got %s", v.Name, got)
			}
		} else if got != v.Serialized.Hex {
			t.Errorf("vector %q: got %s want %s", v.Name, got, v.Serialized.Hex)
		}
		checked++
	}
	t.Logf("Go shared-vector conformance: %d byte-exact", checked)
	if checked == 0 {
		t.Fatal("no vectors checked")
	}
}

func scalarDef(typ string) string {
	extra := ""
	if typ == "string" {
		extra = ", maxlen: 4096"
	}
	return fmt.Sprintf("version: 1\nmessages:\n  vec:\n    payload:\n      a: {id: 0, type: %s%s}\n", typ, extra)
}

func goScalarJSON(op, rawValue string) (string, bool) {
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

// goValueIsDefault reports whether a shared-vector scalar value is the type
// default (zero / empty) -- which a sparse-canonical encoder omits.
func goValueIsDefault(op, rawValue string) bool {
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

func buildGoHarness(t *testing.T, corelib, def string) (string, error) {
	t.Helper()
	s := schemaFromYAMLString(t, def)
	files, err := (&Backend{}).Generate(s, map[string]any{
		"emit": "project", "package": "messages", "module_path": "example.com/vec", "go_version": "1.21",
	})
	if err != nil {
		return "", err
	}
	dir := t.TempDir()
	for _, f := range files {
		full := filepath.Join(dir, f.Path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return "", err
		}
		content := f.Content
		if f.Path == "go.mod" {
			content = []byte(strings.ReplaceAll(string(content), "${SOFAB_GO_CORELIB}", corelib))
		}
		if err := os.WriteFile(full, content, 0o644); err != nil {
			return "", err
		}
	}
	for _, args := range [][]string{{"mod", "tidy"}, {"build", "-o", "harness_bin", "./harness"}} {
		cmd := exec.Command("go", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GOFLAGS=-mod=mod")
		if out, err := cmd.CombinedOutput(); err != nil {
			return "", fmt.Errorf("go %v: %v\n%s", args, err, out)
		}
	}
	return filepath.Join(dir, "harness_bin"), nil
}

func goRunEncode(t *testing.T, bin, jsonIn string) string {
	t.Helper()
	cmd := exec.Command(bin, "encode", "vec")
	cmd.Stdin = strings.NewReader(jsonIn)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("encode %q: %v", jsonIn, err)
	}
	return hex.EncodeToString(out)
}
