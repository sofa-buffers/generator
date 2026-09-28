package c

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// unionShapeYAML covers every option kind a union can hold, with default_id on
// a STRUCT option that is not the first (a sequence default option at id != 0).
const unionShapeYAML = `
version: 1
messages:
  m:
    payload:
      u:
        id: 0
        type: union
        default_id: 2
        oneof:
          num:  { id: 0, type: u16, default: 5 }
          s:    { id: 1, type: string, maxlen: 8 }
          pt:   { id: 2, type: struct, fields: { x: { id: 0, type: i32, default: 7 } } }
          arr:  { id: 3, type: array, items: { type: u16, count: 4 } }
          strs: { id: 4, type: array, items: { type: string, count: 3, maxlen: 4 } }
          bl:   { id: 5, type: blob, maxlen: 4 }
      w: { id: 1, type: u8 }
`

func TestUnionTypeIsTagPlusOverlay(t *testing.T) {
	files := genCFromYAML(t, unionShapeYAML)
	h, c := files["m.h"], files["m.c"]

	// The tag leads (SOFAB_OBJECT_DESCR_UNION asserts offset 0), the options
	// overlay each other in a C union `u`, in schema order.
	for _, want := range []string{
		"typedef struct {\n    sofab_object_descr_id_t which;",
		"    union {\n        uint16_t num;",
		"        char s[9];",
		"        message_m_u_pt_t pt;",
		// sized options keep their length INSIDE the option: a sibling length
		// member would overlay the other options too
		"        struct { uint16_t len; uint16_t items[4]; } arr;",
		"        message_m_u_strs_elems_t strs;",
		"        struct { uint8_t len; uint8_t data[4]; } bl;",
		"    } u;\n} message_m_u_t;",
	} {
		if !strings.Contains(h, want) {
			t.Errorf("header missing %q:\n%s", want, h)
		}
	}
	// No product type: no option is a member of its own beside the others.
	if strings.Contains(h, "arr_len") || strings.Contains(h, "bl_len") {
		t.Errorf("a union option must not carry a sibling length member:\n%s", h)
	}

	for _, want := range []string{
		"SOFAB_OBJECT_FIELD(0, message_m_u_t, u.num, SOFAB_OBJECT_FIELDTYPE_UNSIGNED),",
		"SOFAB_OBJECT_FIELD(1, message_m_u_t, u.s, SOFAB_OBJECT_FIELDTYPE_STRING),",
		"SOFAB_OBJECT_FIELD_SEQUENCE(2, message_m_u_t, u.pt, SOFAB_OBJECT_FIELDTYPE_SEQUENCE, 0),",
		"SOFAB_OBJECT_FIELD_ARRAY_SIZED(3, message_m_u_t, u.arr.items, u.arr.len, SOFAB_OBJECT_FIELDTYPE_ARRAY_UNSIGNED),",
		"SOFAB_OBJECT_FIELD_SEQUENCE(4, message_m_u_t, u.strs, SOFAB_OBJECT_FIELDTYPE_SEQUENCE, 1),",
		"SOFAB_OBJECT_FIELD_BLOB_SIZED(5, message_m_u_t, u.bl.data, u.bl.len),",
		"= SOFAB_OBJECT_DESCR_UNION(_message_fields_named_m_u, 6, _message_nested_named_m_u, 2, &_message_defaults_named_m_u, message_m_u_t, which);",
	} {
		if !strings.Contains(c, want) {
			t.Errorf("source missing %q:\n%s", want, c)
		}
	}
	// A plain struct descriptor for the union would walk every option as if it
	// were present (a product type): the union must never get one.
	if strings.Contains(c, "_message_descr_named_m_u = SOFAB_OBJECT_DESCR(") ||
		strings.Contains(c, "_message_descr_named_m_u = SOFAB_OBJECT_DESCR_WITH_DEFAULTS(") {
		t.Errorf("a union must be described by SOFAB_OBJECT_DESCR_UNION:\n%s", c)
	}
}

func TestUnionOptionIDMacros(t *testing.T) {
	files := genCFromYAML(t, unionShapeYAML)
	h := files["m.h"]
	for _, want := range []string{
		"#define MESSAGE_M_U_NUM_ID 0",
		"#define MESSAGE_M_U_S_ID 1",
		"#define MESSAGE_M_U_PT_ID 2",
		"#define MESSAGE_M_U_ARR_ID 3",
		"#define MESSAGE_M_U_STRS_ID 4",
		"#define MESSAGE_M_U_BL_ID 5",
		"a fresh value holds pt.",
	} {
		if !strings.Contains(h, want) {
			t.Errorf("header missing %q:\n%s", want, h)
		}
	}
}

// TestUnionSequenceOptionDescriptorsDeclared: selecting a struct/union/wrapper
// option at its default is `sofab_object_init(&<descr>, &x.u.<opt>)`, so the
// header must declare exactly those option descriptors -- and no leaf's.
func TestUnionSequenceOptionDescriptorsDeclared(t *testing.T) {
	h := genCFromYAML(t, unionShapeYAML)["m.h"]
	for _, want := range []string{
		"extern const sofab_object_descr_t _message_descr_named_m_u_pt;",
		"extern const sofab_object_descr_t _message_descr_named_m_u_strs_elems;",
	} {
		if !strings.Contains(h, want) {
			t.Errorf("header missing %q:\n%s", want, h)
		}
	}
	if n := strings.Count(h, "extern const sofab_object_descr_t _message_descr_named_"); n != 2 {
		t.Errorf("want exactly the 2 sequence-option descriptors declared, got %d:\n%s", n, h)
	}
}

// TestUnionDefaultImageRule pins the one "NULL iff" rule and both prefix forms.
// A union's image is read for the tag and for a LEAF default option's own bytes
// only, so it is never a full sizeof(T) image.
func TestUnionDefaultImageRule(t *testing.T) {
	cases := []struct {
		name, yaml string
		want       []string
		notWant    []string
	}{
		{"id 0, sequence D with non-zero nested defaults -> NULL", `
version: 1
messages:
  m:
    payload:
      u: { id: 0, type: union, default_id: 0, oneof: { p: { id: 0, type: struct, fields: { x: { id: 0, type: u8, default: 9 } } }, n: { id: 1, type: u8, default: 3 } } }
`, []string{"SOFAB_OBJECT_DESCR_UNION(_message_fields_named_m_u, 2, _message_nested_named_m_u, 1, NULL, message_m_u_t, which);"},
			[]string{"_message_defaults_named_m_u ", "defimg_named_m_u"}},
		{"id 0, all-zero leaf D -> NULL", `
version: 1
messages:
  m:
    payload:
      u: { id: 0, type: union, oneof: { n: { id: 0, type: u8 }, s: { id: 1, type: string, maxlen: 4 } } }
`, []string{"SOFAB_OBJECT_DESCR_UNION(_message_fields_named_m_u, 2, NULL, 0, NULL, message_m_u_t, which);"},
			[]string{"_message_defaults_named_m_u ", "defimg_named_m_u"}},
		{"id != 0, sequence D -> tag-only image", `
version: 1
messages:
  m:
    payload:
      u: { id: 0, type: union, default_id: 1, oneof: { n: { id: 0, type: u8 }, p: { id: 1, type: struct, fields: { x: { id: 0, type: u8 } } } } }
`, []string{
			"static const struct { sofab_object_descr_id_t which; } _message_defaults_named_m_u = { 1 };",
			", &_message_defaults_named_m_u, message_m_u_t, which);",
		}, []string{"defimg_named_m_u"}},
		{"id 0, leaf D at a non-zero default -> tag + D prefix", `
version: 1
messages:
  m:
    payload:
      u: { id: 0, type: union, default_id: 0, oneof: { n: { id: 0, type: u16, default: 5 }, big: { id: 1, type: string, maxlen: 64 } } }
`, []string{
			"typedef struct { sofab_object_descr_id_t which; union { uint16_t n; uint16_t _align; } u; } _message_defimg_named_m_u_t;",
			"typedef char _message_defimg_named_m_u_at_u[(offsetof(_message_defimg_named_m_u_t, u) == offsetof(message_m_u_t, u)) ? 1 : -1];",
			"static const _message_defimg_named_m_u_t _message_defaults_named_m_u = { .which = 0, .u.n = 5 };",
		}, []string{"char big[65]; uint"}},
		{"id != 0, leaf D (zero default) -> tag + D prefix aligned like T's union", `
version: 1
messages:
  m:
    payload:
      u: { id: 0, type: union, default_id: 1, oneof: { big: { id: 0, type: u64 }, sig: { id: 1, type: i64 } } }
`, []string{
			"typedef struct { sofab_object_descr_id_t which; union { int64_t sig; uint64_t _align; } u; } _message_defimg_named_m_u_t;",
			"static const _message_defimg_named_m_u_t _message_defaults_named_m_u = { .which = 1 };",
		}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := genCFromYAML(t, tc.yaml)["m.c"]
			for _, w := range tc.want {
				if !strings.Contains(c, w) {
					t.Errorf("missing %q:\n%s", w, c)
				}
			}
			for _, nw := range tc.notWant {
				if strings.Contains(c, nw) {
					t.Errorf("must not contain %q:\n%s", nw, c)
				}
			}
			// never a full image of the union type
			if strings.Contains(c, "static const message_m_u_t ") {
				t.Errorf("a union's image is a prefix, never a full message_m_u_t:\n%s", c)
			}
		})
	}
}

// TestUnionSplitVariantsGetTheirOwnDescriptors: a $defs union used with two
// default_ids is two IR types (Core), so two C types, two descriptors and two
// images, each with its own default option.
func TestUnionSplitVariantsGetTheirOwnDescriptors(t *testing.T) {
	files := genCFromYAML(t, `
version: 1
$defs:
  union:
    Pick:
      n: { id: 0, type: u16, default: 6 }
      t: { id: 1, type: struct, fields: { k: { id: 0, type: u8, default: 2 } } }
messages:
  m:
    payload:
      a: { id: 0, type: union, default_id: 1, oneof: { $ref: "#/$defs/union/Pick" } }
      b: { id: 1, type: union, oneof: { $ref: "#/$defs/union/Pick" } }
`)
	h, c := files["m.h"], files["m.c"]
	for _, want := range []string{
		"} message_union_Pick_default_t_t;",
		"} message_union_Pick_default_n_t;",
		"#define MESSAGE_UNION_PICK_DEFAULT_T_N_ID 0",
		"#define MESSAGE_UNION_PICK_DEFAULT_N_T_ID 1",
	} {
		if !strings.Contains(h, want) {
			t.Errorf("header missing %q:\n%s", want, h)
		}
	}
	for _, want := range []string{
		"static const struct { sofab_object_descr_id_t which; } _message_defaults_named_union_Pick_default_t = { 1 };",
		"static const _message_defimg_named_union_Pick_default_n_t _message_defaults_named_union_Pick_default_n = { .which = 0, .u.n = 6 };",
		"const sofab_object_descr_t _message_descr_named_union_Pick_default_t = SOFAB_OBJECT_DESCR_UNION(",
		"const sofab_object_descr_t _message_descr_named_union_Pick_default_n = SOFAB_OBJECT_DESCR_UNION(",
	} {
		if !strings.Contains(c, want) {
			t.Errorf("source missing %q:\n%s", want, c)
		}
	}
}

// TestUnionOptionMacroCollisions: the option ids join the one flat macro
// namespace, so checkMacroNames must see them -- against a bitfield flag of an
// option, and against another union's option.
func TestUnionOptionMacroCollisions(t *testing.T) {
	cases := []struct{ name, yaml, want string }{
		{"option id vs a flag of a bitfield option", `
version: 1
messages:
  m:
    payload:
      u: { id: 0, type: union, oneof: { fl: { id: 0, type: bitfield, bits: { id: { pos: 0 } } }, x: { id: 1, type: u8 } } }
`, "MESSAGE_M_U_FL_ID"},
		{"option ids of two unions", `
version: 1
messages:
  m:
    payload:
      u:   { id: 0, type: union, oneof: { a_b: { id: 0, type: u8 } } }
      u_a: { id: 1, type: union, oneof: { b: { id: 0, type: u8 } } }
`, "MESSAGE_M_U_A_B_ID"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := genCErr(t, tc.yaml)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want a macro collision naming %s, got %v", tc.want, err)
			}
		})
	}
}

// TestUnionCapabilityGuard: a union is a sequence on the wire, so a message
// that reaches one -- directly or only through an array element -- refuses a
// corelib built without sequence support. The corelib has no union switch of
// its own, so there is none to refuse.
func TestUnionCapabilityGuard(t *testing.T) {
	guard := "#if defined(SOFAB_DISABLE_SEQUENCE_SUPPORT)"
	h := genCFromYAML(t, unionShapeYAML)["m.h"]
	if !strings.Contains(h, guard) {
		t.Errorf("a message with a union must refuse SOFAB_DISABLE_SEQUENCE_SUPPORT:\n%s", h)
	}
	// reached only through an array element
	elem := genCFromYAML(t, `
version: 1
messages:
  m:
    payload:
      v: { id: 0, type: array, items: { type: union, count: 2, oneof: { a: { id: 0, type: u8 } } } }
`)["m.h"]
	if !strings.Contains(elem, guard) {
		t.Errorf("an array of unions must refuse SOFAB_DISABLE_SEQUENCE_SUPPORT:\n%s", elem)
	}
	for name, src := range map[string]string{"union": h, "array of unions": elem} {
		if strings.Contains(src, "_UNION_SUPPORT") {
			t.Errorf("%s: the corelib has no union switch to refuse:\n%s", name, src)
		}
	}
}

// TestUnionHarnessJSONHoldsOneOption: the harness prints exactly the held
// option ({"<option>": value}) and parses one member into the tag + value,
// putting a sequence option at its own default first.
func TestUnionHarnessJSONHoldsOneOption(t *testing.T) {
	main := genCProject(t, unionShapeYAML)["harness/main.c"]
	for _, want := range []string{
		"    switch (o->which) {\n    case MESSAGE_M_U_NUM_ID:\n        fprintf(out, \"\\\"num\\\":\");",
		"        json_bytes(out, o->u.bl.data, o->u.bl.len);",
		"        o->which = MESSAGE_M_U_PT_ID;\n        sofab_object_init(&_message_descr_named_m_u_pt, &o->u.pt);",
		"        o->which = MESSAGE_M_U_STRS_ID;\n        sofab_object_init(&_message_descr_named_m_u_strs_elems, &o->u.strs);",
		"        o->u.bl.len = (uint8_t)json_to_bytes(c, o->u.bl.data, sizeof(o->u.bl.data));",
		"        o->u.arr.len = (uint16_t)_n0;",
	} {
		if !strings.Contains(main, want) {
			t.Errorf("harness missing %q:\n%s", want, main)
		}
	}
	// a leaf option is selected by its value alone, no init
	if strings.Contains(main, "&o->u.num)") {
		t.Errorf("a leaf option needs no sofab_object_init:\n%s", main)
	}
}

// TestUnionCompilesOnEveryProfile builds the generated union code against the
// corelib under all three descriptor profiles (the tag is 1, 2 or 4 bytes wide),
// which is where the prefix image's offset assertion has to hold.
func TestUnionCompilesOnEveryProfile(t *testing.T) {
	corelib := os.Getenv("SOFAB_C_CORELIB")
	if corelib == "" {
		t.Skip("set SOFAB_C_CORELIB to a corelib-c-cpp checkout to run the compile gate")
	}
	gcc, err := exec.LookPath("gcc")
	if err != nil {
		t.Skip("gcc not found")
	}
	dir := t.TempDir()
	src := unionShapeYAML + `
      q: { id: 2, type: union, default_id: 1, oneof: { big: { id: 0, type: u64 }, sig: { id: 1, type: i64, default: -3 } } }
      f: { id: 3, type: union, oneof: { d: { id: 0, type: fp64, default: 1.5 }, b: { id: 1, type: u8 } } }
      v: { id: 4, type: array, items: { type: union, count: 2, default_id: 1, oneof: { i: { id: 0, type: i32 }, s: { id: 1, type: string, maxlen: 8 } } } }
`
	for path, content := range genCFromYAML(t, src) {
		if err := os.WriteFile(filepath.Join(dir, path), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, prof := range []string{"1", "2", "3"} {
		args := append([]string{"-std=c99", "-pedantic"}, strictWarnings...)
		args = append(args, "-DSOFAB_OBJECT_DESCR_PROFILE="+prof, "-I"+filepath.Join(corelib, "src", "include"), "-I"+dir,
			"-c", filepath.Join(dir, "m.c"), "-o", filepath.Join(dir, "m.o"))
		if out, err := exec.Command(gcc, args...).CombinedOutput(); err != nil {
			t.Fatalf("profile %s: generated union C failed to compile:\n%s", prof, out)
		}
	}
}

// plainYAML is a union-free schema (a nested struct keeps the object walk busy).
const plainYAML = `
version: 1
messages:
  m:
    payload:
      s: { id: 0, type: struct, fields: { a: { id: 0, type: u8, default: 3 } } }
      w: { id: 1, type: u8 }
`

// TestUnionCorelibCapabilityGuard: a header that uses unions refuses a corelib
// without SOFAB_OBJECT_DESCR_UNION by name; a union-free one does not ask. The
// build half stands in for an old corelib by dropping the macro after object.h.
func TestUnionCorelibCapabilityGuard(t *testing.T) {
	guard := "#if !defined(SOFAB_OBJECT_DESCR_UNION)"
	h := genCFromYAML(t, unionShapeYAML)["m.h"]
	if !strings.Contains(h, guard) || !strings.Contains(h, "which this corelib-c-cpp predates") {
		t.Errorf("a message with a union must refuse a corelib without SOFAB_OBJECT_DESCR_UNION:\n%s", h)
	}
	if plain := genCFromYAML(t, plainYAML)["m.h"]; strings.Contains(plain, guard) {
		t.Errorf("a message without a union must not ask for SOFAB_OBJECT_DESCR_UNION:\n%s", plain)
	}

	corelib := os.Getenv("SOFAB_C_CORELIB")
	if corelib == "" {
		t.Skip("set SOFAB_C_CORELIB to a corelib-c-cpp checkout to run the build half")
	}
	gcc, err := exec.LookPath("gcc")
	if err != nil {
		t.Skip("gcc not found")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "m.h"), []byte(h), 0o644); err != nil {
		t.Fatal(err)
	}
	tu := "#include \"sofab/object.h\"\n#undef SOFAB_OBJECT_DESCR_UNION\n#include \"m.h\"\n"
	if err := os.WriteFile(filepath.Join(dir, "old.c"), []byte(tu), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(gcc, "-std=c99", "-I"+filepath.Join(corelib, "src", "include"), "-I"+dir,
		"-c", filepath.Join(dir, "old.c"), "-o", filepath.Join(dir, "old.o")).CombinedOutput()
	if err == nil || !strings.Contains(string(out), "which this corelib-c-cpp predates") {
		t.Fatalf("a corelib without SOFAB_OBJECT_DESCR_UNION must fail with the guard's message, got err=%v:\n%s", err, out)
	}
}

// TestProjectSetsNoUnionSwitch: corelib-c-cpp has no union switch, so no
// generated project -- with a union or without -- passes one. The build half
// hands a union-bearing project a sequence-free corelib and expects it to stop
// at a guard naming SOFAB_DISABLE_SEQUENCE_SUPPORT: the generated header's,
// which the .c includes before its descriptors reach the corelib's own.
func TestProjectSetsNoUnionSwitch(t *testing.T) {
	uni := genCProject(t, unionShapeYAML)
	for name, files := range map[string]map[string]string{"union-free": genCProject(t, plainYAML), "union": uni} {
		for _, path := range []string{"Makefile", "CMakeLists.txt"} {
			if strings.Contains(files[path], "_UNION_SUPPORT") || strings.Contains(files[path], "SOFAB_DEFINES") {
				t.Errorf("%s %s must not set a union switch:\n%s", name, path, files[path])
			}
		}
	}

	corelib := os.Getenv("SOFAB_C_CORELIB")
	if corelib == "" {
		t.Skip("set SOFAB_C_CORELIB to a corelib-c-cpp checkout to run the build half")
	}
	if _, err := exec.LookPath("make"); err != nil {
		t.Skip("make not found")
	}
	dir := writeProject(t, uni)
	out, err := exec.Command("make", "-C", dir, "SOFAB_C_CORELIB="+corelib, "CFLAGS=-DSOFAB_DISABLE_SEQUENCE_SUPPORT").CombinedOutput()
	if err == nil || !strings.Contains(string(out), "but the corelib was built with SOFAB_DISABLE_SEQUENCE_SUPPORT") {
		t.Fatalf("a union-bearing project on a sequence-free corelib must stop at the sequence guard, got err=%v:\n%s", err, out)
	}
}

// writeProject writes a generated project into a fresh temp dir.
func writeProject(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for path, content := range files {
		full := filepath.Join(dir, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// unionBlobYAML puts a blob option directly in a union, one level down in a
// struct option, and in a union reached only through an array element.
const unionBlobYAML = `
version: 1
messages:
  m:
    payload:
      u:
        id: 0
        type: union
        oneof:
          s:  { id: 0, type: string, maxlen: 8 }
          bl: { id: 1, type: blob, maxlen: 6 }
          pt: { id: 2, type: struct, fields: { n: { id: 0, type: u8 }, raw: { id: 1, type: blob, maxlen: 3 } } }
      v: { id: 1, type: array, items: { type: union, count: 2, oneof: { i: { id: 0, type: u8 }, b: { id: 1, type: blob, maxlen: 2 } } } }
`

// TestUnionBlobOptionIsSized: corelib-c-cpp forbids a capacity-only BLOB option
// in a union and asserts it in sofab_object_init -- such a blob has nowhere to
// record the length it received, so a shorter payload would keep the previously
// held option's bytes and re-encode them. Every blob a generated descriptor
// carries must be SOFAB_OBJECT_FIELD_BLOB_SIZED. The build half compiles with
// the corelib's asserts ON and decodes one union frame that holds a long string
// and then switches to a one-byte blob: the assert, or the residue, would show.
func TestUnionBlobOptionIsSized(t *testing.T) {
	c := genCFromYAML(t, unionBlobYAML)["m.c"]
	for _, want := range []string{
		"SOFAB_OBJECT_FIELD_BLOB_SIZED(1, message_m_u_t, u.bl.data, u.bl.len),",
		"SOFAB_OBJECT_FIELD_BLOB_SIZED(1, message_m_u_pt_t, raw, raw_len),",
		", u.b.data, u.b.len),",
	} {
		if !strings.Contains(c, want) {
			t.Errorf("source missing %q:\n%s", want, c)
		}
	}
	if strings.Contains(c, "SOFAB_OBJECT_FIELDTYPE_BLOB)") {
		t.Errorf("no descriptor may carry a capacity-only blob, SOFAB_OBJECT_FIELD(..., SOFAB_OBJECT_FIELDTYPE_BLOB):\n%s", c)
	}

	corelib := os.Getenv("SOFAB_C_CORELIB")
	if corelib == "" {
		t.Skip("set SOFAB_C_CORELIB to a corelib-c-cpp checkout to run the build half")
	}
	if _, err := exec.LookPath("make"); err != nil {
		t.Skip("make not found")
	}
	dir := writeProject(t, genCProject(t, unionBlobYAML))
	// No -DNDEBUG: the corelib's asserts are live.
	if out, err := exec.Command("make", "-C", dir, "SOFAB_C_CORELIB="+corelib, strictMakeVar, "CFLAGS=-O0 -g").CombinedOutput(); err != nil {
		t.Fatalf("union-with-blob project build failed:\n%s", out)
	}
	harness := filepath.Join(dir, "harness", "harness")
	run := func(mode string, in []byte) []byte {
		t.Helper()
		cmd := exec.Command(harness, mode)
		cmd.Stdin = strings.NewReader(string(in))
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s failed (a corelib assert aborts here): %v\n%s", mode, err, out)
		}
		return out
	}
	// Two frames of u, back to back: {s:"SECRET"}, then {bl:[255]}. The option
	// received last wins, and it is exactly the one byte it carried.
	frames := append(run("encode", []byte(`{"u":{"s":"SECRET"}}`)), run("encode", []byte(`{"u":{"bl":[255]}}`))...)
	if got := string(run("decode", frames)); !strings.Contains(got, `"u":{"bl":[255]}`) {
		t.Errorf("a blob option after a string option must hold only its own byte:\n%s", got)
	}
	got := string(run("decode", run("encode", []byte(`{"u":{"pt":{"n":1,"raw":[9]}},"v":[{"b":[7,8]},{"i":4}]}`))))
	for _, want := range []string{`"raw":[9]`, `"b":[7,8]`, `"i":4`} {
		if !strings.Contains(got, want) {
			t.Errorf("round trip lost %s:\n%s", want, got)
		}
	}
}
