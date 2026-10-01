package c

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/sofa-buffers/generator/internal/analysis"
	"github.com/sofa-buffers/generator/internal/model"
	"github.com/sofa-buffers/generator/internal/parser"
)

// corelibMacroFields are fields in the corelib's macro namespace: its include
// guards, its object-like macros, and the switches a build defines on the
// command line (-DSOFAB_DISABLE_FIXLEN_SUPPORT makes that name a macro in every
// translation unit). cIdent escapes the whole SOFAB_ prefix, so these stand for
// the rule rather than a list.
var corelibMacroFields = []string{
	"SOFAB_H", "SOFAB_OBJECT_H", "SOFAB_MAX_DEPTH", "SOFAB_ID_MAX", "SOFAB_API_VERSION",
	"SOFAB_DISABLE_FIXLEN_SUPPORT", "SOFAB_STRICT_UTF8", "SOFAB_OBJECT_DESCR_PROFILE",
	"SOFAB_TEST_JSON_H",
}

// reservedNames is every member name on the C list, sorted.
func reservedNames() []string {
	names := append([]string{}, corelibMacroFields...)
	for _, set := range []map[string]bool{cKeywords, cHeaderMacros} {
		for n := range set {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	return names
}

// reservedYAML is a schema that uses every reserved name as a field of a message,
// of a nested struct, and as an option of a union. A blob gives the message a
// length companion too. Names are quoted: `true`/`false` are YAML booleans.
func reservedYAML(names []string) string {
	var fields strings.Builder
	opts := make([]string, len(names))
	for i, n := range names {
		fmt.Fprintf(&fields, "      %q: { id: %d, type: u8 }\n", n, i)
		opts[i] = fmt.Sprintf("%q: { id: %d, type: u8 }", n, i)
	}
	k := len(names)
	return fmt.Sprintf(`version: 1
$defs:
  struct:
    Inner:
%smessages:
  m:
    payload:
%s      b:     { id: %d, type: blob, maxlen: 4 }
      inner: { id: %d, type: struct, fields: { $ref: '#/$defs/struct/Inner' } }
      u:     { id: %d, type: union, oneof: { %s } }
`, fields.String(), fields.String(), k, k+1, k+2, strings.Join(opts, ", "))
}

// TestReservedNamesAreMangled: every name on the list is a member with a
// trailing underscore, in the message and in the nested struct.
func TestReservedNamesAreMangled(t *testing.T) {
	names := reservedNames()
	h := genCFromYAMLCfg(t, reservedYAML(names), map[string]any{})["m_sofab.h"]
	for _, n := range names {
		// Anchored on the 4-space indent: the union's options sit at 8.
		if got := strings.Count(h, "\n    uint8_t "+n+"_;\n"); got != 2 {
			t.Errorf("field %q is the member %s_ in %d of the 2 structs", n, n, got)
		}
	}
}

// TestDerivedMembersNeverCollide: what the backend adds beside a field is a role
// of it (`<member>__len`), and every macro it defines has a "__" in it, so the
// schemas that once had to be refused -- a field named after a sibling's length
// companion, a field spelled like the message's MAX_SIZE, include guard or an
// option id -- now generate, each name reaching its own member.
func TestDerivedMembersNeverCollide(t *testing.T) {
	h := genCFromYAML(t, `
version: 1
messages:
  m:
    payload:
      b:     { id: 0, type: blob, maxlen: 4 }
      b_len: { id: 1, type: u8 }
      a:     { id: 2, type: array, items: { type: u8, count: 2 } }
      a_len: { id: 3, type: u8 }
      MESSAGE_M_MAX_SIZE: { id: 4, type: u8 }
      MESSAGE_M_H:        { id: 5, type: u8 }
      MESSAGE_M_U_P_ID:   { id: 6, type: u8 }
      u: { id: 7, type: union, oneof: { p: { id: 0, type: u8 }, MESSAGE_M_H: { id: 1, type: u8 } } }
`)["m_sofab.h"]
	for _, want := range []string{
		"uint8_t b__len; uint8_t b[4];", "uint8_t b_len;",
		"uint8_t a__len; uint8_t a[2];", "uint8_t a_len;",
		"uint8_t MESSAGE_M_MAX_SIZE;", "#define MESSAGE_M__MAX_SIZE ",
		"uint8_t MESSAGE_M_H;", "#define MESSAGE_M__H\n",
		"uint8_t MESSAGE_M_U_P_ID;", "#define MESSAGE_M___U___P__ID 0",
		"#define MESSAGE_M___U___MESSAGE_M_H__ID 1",
	} {
		if !strings.Contains(h, want) {
			t.Errorf("header missing %q:\n%s", want, h)
		}
	}
}

// TestSymbolPrefixMustStartWithALetter: the naming guarantee assumes a
// symbol_prefix that starts with a letter, so any other prefix is a config
// error — named, and raised before anything is generated.
func TestSymbolPrefixMustStartWithALetter(t *testing.T) {
	doc, err := parser.Parse([]byte("version: 1\nmessages:\n  m: { payload: { x: { id: 0, type: u8 } } }\n"), "t.yaml")
	if err != nil {
		t.Fatal(err)
	}
	s, err := model.Build(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := analysis.Analyze(s); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"_x", "1x", "a-b", "a b"} {
		_, err := (&Backend{}).Generate(s, map[string]any{"symbol_prefix": p})
		if err == nil || !strings.Contains(err.Error(), "symbol_prefix") {
			t.Errorf("prefix %q: want a symbol_prefix config error, got %v", p, err)
		}
	}
	for _, p := range []string{"u", "message_", "sofab_", "My_Proj1_"} {
		if _, err := (&Backend{}).Generate(s, map[string]any{"symbol_prefix": p}); err != nil {
			t.Errorf("prefix %q: %v", p, err)
		}
	}
}

// TestTypedefEscape: a type's typedef is `<prefix><path>_t`, the one generated
// name without a "__", so a symbol_prefix can spell it like a typedef of the C
// library or the corelib; it then takes the escape, a trailing underscore, and
// the roles keep the unescaped base.
func TestTypedefEscape(t *testing.T) {
	for _, c := range []struct {
		prefix, message string
		want            []string
	}{
		{"u", "int8", []string{"} uint8_t_;", "void uint8__init(uint8_t_ *msg);", "} uint8__decoder_t;"}},
		{"sofab_", "ostream", []string{"} sofab_ostream_t_;", "void sofab_ostream__init(sofab_ostream_t_ *msg);", "#ifndef SOFAB_OSTREAM__H"}},
		{"sofab_", "json", []string{"} sofab_json_t_;"}},
		{"run_encode_", "x", []string{"} run_encode_x_t;"}},
	} {
		src := fmt.Sprintf("version: 1\nmessages:\n  %s: { payload: { x: { id: 0, type: u8 } } }\n", c.message)
		h := genCFromYAMLCfg(t, src, map[string]any{"symbol_prefix": c.prefix})[c.message+"_sofab.h"]
		for _, want := range c.want {
			if !strings.Contains(h, want) {
				t.Errorf("prefix %q, message %q: header missing %q:\n%s", c.prefix, c.message, want, h)
			}
		}
	}
	// The harness's bench entry point run_encode_<name> is in the typedefs'
	// namespace too: a message `a_t` with the prefix `run_encode_` would spell it.
	src := "version: 1\nmessages:\n  a: { payload: { x: { id: 0, type: u8 } } }\n  a_t: { payload: { x: { id: 0, type: u8 } } }\n"
	h := genCFromYAMLCfg(t, src, map[string]any{"symbol_prefix": "run_encode_"})["a_sofab.h"]
	if !strings.Contains(h, "} run_encode_a_t_;") {
		t.Errorf("a typedef spelled like the bench entry point run_encode_a_t must be escaped:\n%s", h)
	}
}

// TestReservedNamesBuild: the collision test. Every name on the list, used as a
// field, must give a project that builds under the strict warning set, compiles
// as C23 and as gcc's default gnu17 too, and round-trips every value under its
// schema name.
// Gated on SOFAB_C_CORELIB + make + gcc.
func TestReservedNamesBuild(t *testing.T) {
	corelib := os.Getenv("SOFAB_C_CORELIB")
	if corelib == "" {
		t.Skip("set SOFAB_C_CORELIB to run the reserved-name build gate")
	}
	for _, tool := range []string{"make", "gcc"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not found", tool)
		}
	}
	names := reservedNames()
	dir := t.TempDir()
	for path, content := range genCFromYAMLCfg(t, reservedYAML(names), map[string]any{"emit": "project"}) {
		full := filepath.Join(dir, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if out, err := exec.Command("make", "-C", dir, "SOFAB_C_CORELIB="+corelib, strictMakeVar).CombinedOutput(); err != nil {
		t.Fatalf("every reserved name as a field does not build:\n%s", out)
	}
	// C23 makes `true`, `nullptr`, `typeof` keywords and adds the *_WIDTH
	// macros; gnu17 (gcc's default) predefines `linux`/`unix` and lets glibc
	// add its own. The project's Makefile builds as C99, which has neither.
	for _, std := range []string{"c2x", "gnu17"} {
		cc := exec.Command("gcc", "-std="+std, "-fsyntax-only", "-Wall", "-Wextra", "-Werror",
			"-I"+filepath.Join(corelib, "src", "include"), "-I"+filepath.Join(dir, "generated"), filepath.Join(dir, "generated", "m_sofab.c"))
		if out, err := cc.CombinedOutput(); err != nil {
			t.Fatalf("every reserved name as a field does not compile as -std=%s:\n%s", std, out)
		}
	}

	top := map[string]any{"b": []int{1, 2}}
	inner := map[string]any{}
	for i, n := range names {
		top[n] = i + 1
		inner[n] = i + 2
	}
	top["inner"] = inner
	last := names[len(names)-1]
	top["u"] = map[string]any{last: 7}
	in, _ := json.Marshal(top)
	harness := filepath.Join(dir, "harness", "harness")
	enc := exec.Command(harness, "encode")
	enc.Stdin = strings.NewReader(string(in))
	wire, err := enc.Output()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	dec := exec.Command(harness, "decode")
	dec.Stdin = strings.NewReader(string(wire))
	out, err := dec.Output()
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("decode output is not JSON: %v\n%s", err, out)
	}
	gotInner, _ := got["inner"].(map[string]any)
	for i, n := range names {
		if got[n] != float64(i+1) || gotInner[n] != float64(i+2) {
			t.Errorf("field %q did not round-trip under its schema name: top %v, inner %v", n, got[n], gotInner[n])
		}
	}
	if u, _ := got["u"].(map[string]any); u[last] != float64(7) {
		t.Errorf("union option %q did not round-trip under its schema name: %v", last, got["u"])
	}
	if b, _ := json.Marshal(got["b"]); string(b) != "[1,2]" {
		t.Errorf("blob b did not round-trip: %s", b)
	}
}

// TestReservedTypesBuild: the typedef half of the list, against the real
// corelib. Every typedef the corelib and <stdint.h> declare, spelled by a
// symbol_prefix plus a message name, must give a project that builds under the
// strict warning set and round-trips -- the escaped typedef sits beside the
// corelib's own in the same translation units, the harness's included.
// Gated on SOFAB_C_CORELIB + make + gcc.
func TestReservedTypesBuild(t *testing.T) {
	corelib := os.Getenv("SOFAB_C_CORELIB")
	if corelib == "" {
		t.Skip("set SOFAB_C_CORELIB to run the reserved-typedef build gate")
	}
	for _, tool := range []string{"make", "gcc"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not found", tool)
		}
	}
	for _, prefix := range []string{"sofab_", "u"} {
		var msgs []string
		for name := range cReservedTypes {
			if strings.HasPrefix(name, prefix) && strings.HasSuffix(name, "_t") {
				stem := strings.TrimSuffix(strings.TrimPrefix(name, prefix), "_t")
				if stem != "" && stem[0] >= 'a' && stem[0] <= 'z' {
					msgs = append(msgs, stem)
				}
			}
		}
		sort.Strings(msgs)
		if len(msgs) < 8 {
			t.Fatalf("prefix %s reaches only %d reserved typedefs", prefix, len(msgs))
		}
		var src strings.Builder
		src.WriteString("version: 1\nmessages:\n")
		for _, m := range msgs {
			fmt.Fprintf(&src, "  %s: { payload: { x: { id: 0, type: u8 } } }\n", m)
		}
		dir := t.TempDir()
		for path, content := range genCFromYAMLCfg(t, src.String(), map[string]any{"emit": "project", "symbol_prefix": prefix}) {
			full := filepath.Join(dir, path)
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if out, err := exec.Command("make", "-C", dir, "SOFAB_C_CORELIB="+corelib, strictMakeVar).CombinedOutput(); err != nil {
			t.Fatalf("prefix %s: every reserved typedef as a message does not build:\n%s", prefix, out)
		}
		harness := filepath.Join(dir, "harness", "harness")
		for _, m := range msgs {
			enc := exec.Command(harness, "encode", m)
			enc.Stdin = strings.NewReader(`{"x":7}`)
			wire, err := enc.Output()
			if err != nil {
				t.Fatalf("prefix %s, message %s: encode: %v", prefix, m, err)
			}
			dec := exec.Command(harness, "decode", m)
			dec.Stdin = strings.NewReader(string(wire))
			out, err := dec.Output()
			if err != nil || strings.TrimSpace(string(out)) != `{"x":7}` {
				t.Errorf("prefix %s, message %s: round trip gave %q (%v)", prefix, m, out, err)
			}
		}
	}
}
