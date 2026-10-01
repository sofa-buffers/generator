package c

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// sharedDefsYAML uses every $defs type from two messages, in each shape that
// once defined it twice: a plain struct and enum field, a union split by
// default_id (both variants, in both messages), a bitfield, a $defs struct that
// uses another $defs struct, and a $defs struct as a plain field in one message
// and an array element in the other. m3 uses no $defs type at all.
const sharedDefsYAML = `
version: 1
$defs:
  struct:
    point:
      x: { id: 0, type: i32 }
      c: { id: 1, type: enum, enum: { $ref: '#/$defs/enum/color' } }
    line:
      a:    { id: 0, type: struct, fields: { $ref: '#/$defs/struct/point' } }
      tags: { id: 1, type: array, items: { type: string, maxlen: 4, count: 2 } }
  enum:
    color: { red: 0, green: 1 }
  bitfield:
    flags: { on: { pos: 0 }, off: { pos: 1 } }
  union:
    shape:
      num: { id: 0, type: u8 }
      pt:  { id: 1, type: struct, fields: { $ref: '#/$defs/struct/point' } }
messages:
  m1:
    payload:
      p:  { id: 0, type: struct, fields: { $ref: '#/$defs/struct/point' } }
      c:  { id: 1, type: enum, enum: { $ref: '#/$defs/enum/color' } }
      l:  { id: 2, type: struct, fields: { $ref: '#/$defs/struct/line' } }
      s:  { id: 3, type: union, default_id: 0, oneof: { $ref: '#/$defs/union/shape' } }
      s2: { id: 4, type: union, default_id: 1, oneof: { $ref: '#/$defs/union/shape' } }
      f:  { id: 5, type: bitfield, bits: { $ref: '#/$defs/bitfield/flags' } }
  m2:
    payload:
      ps:  { id: 0, type: array, items: { type: struct, count: 3, fields: { $ref: '#/$defs/struct/point' } } }
      c:   { id: 1, type: enum, enum: { $ref: '#/$defs/enum/color' } }
      l:   { id: 2, type: struct, fields: { $ref: '#/$defs/struct/line' } }
      s:   { id: 3, type: union, default_id: 1, oneof: { $ref: '#/$defs/union/shape' } }
      s2:  { id: 4, type: union, default_id: 0, oneof: { $ref: '#/$defs/union/shape' } }
      f:   { id: 5, type: bitfield, bits: { $ref: '#/$defs/bitfield/flags' } }
      own: { id: 6, type: struct, fields: { z: { id: 0, type: u8 } } }
  m3:
    payload:
      x: { id: 0, type: u8 }
`

// definitionRe matches what defines a name at file scope in generated C: a
// typedef, a descriptor (or any other non-static object), a macro.
var definitionRe = regexp.MustCompile(`(?m)^(?:\} (\w+);|const sofab_object_descr_t (\w+) =|#define (\w+))`)

// TestSharedDefsDefinedOnce: across every file of a run, each typedef,
// descriptor and macro is defined exactly once -- a $defs type used by two
// messages lives in the shared $defs files, which both message headers include,
// and a message that uses none does not include them.
func TestSharedDefsDefinedOnce(t *testing.T) {
	files := genCFromYAML(t, sharedDefsYAML)
	seen := map[string][]string{}
	var paths []string
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		for _, m := range definitionRe.FindAllStringSubmatch(files[p], -1) {
			name := m[1] + m[2] + m[3]
			seen[name] = append(seen[name], p)
		}
	}
	for name, where := range seen {
		if len(where) != 1 {
			t.Errorf("%s is defined %d times: %v", name, len(where), where)
		}
	}
	for _, want := range []string{
		"message_point_t", "message_line_t", "message_shape__default_num_t", "message_shape__default_pt_t",
		"message_point__descr", "message_line__descr", "MESSAGE_FLAGS___ON", "MESSAGE_SHAPE___PT__ID",
	} {
		if w := seen[want]; len(w) != 1 || w[0] != "sofab-defs.h" && w[0] != "sofab-defs.c" {
			t.Errorf("%s must be defined in the shared $defs files, is in %v", want, w)
		}
	}
	for _, f := range []string{"m1_sofab.h", "m2_sofab.h"} {
		if !strings.Contains(files[f], `#include "sofab-defs.h"`) {
			t.Errorf("%s must include the shared $defs header", f)
		}
	}
	if strings.Contains(files["m3_sofab.h"], "sofab-defs.h") {
		t.Errorf("m3 uses no $defs type and must not include the shared $defs header")
	}
	// A schema without $defs gets no shared files.
	files = genCFromYAML(t, "version: 1\nmessages:\n  m:\n    payload:\n      p: { id: 0, type: struct, fields: { x: { id: 0, type: u8 } } }\n")
	for p := range files {
		if strings.HasPrefix(p, "sofab-defs.") {
			t.Errorf("a schema without $defs must not emit %s", p)
		}
	}
}

// TestSharedDefsBuildAndRoundTrip: every header of the run in one translation
// unit, every source in one program -- the project harness includes each
// header and links each source -- builds under the strict warning set and
// round-trips both messages. Gated on SOFAB_C_CORELIB + make + gcc.
func TestSharedDefsBuildAndRoundTrip(t *testing.T) {
	corelib := os.Getenv("SOFAB_C_CORELIB")
	if corelib == "" {
		t.Skip("set SOFAB_C_CORELIB to run the shared-$defs build gate")
	}
	for _, tool := range []string{"make", "gcc"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not found", tool)
		}
	}
	dir := t.TempDir()
	files := genCFromYAMLCfg(t, sharedDefsYAML, map[string]any{"emit": "project"})
	for path, content := range files {
		full := filepath.Join(dir, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// A user's TU that includes every header, the shared one last.
	tu := "#include \"m1_sofab.h\"\n#include \"m2_sofab.h\"\n#include \"m3_sofab.h\"\n#include \"sofab-defs.h\"\n" +
		"int tu_use(void) { message_m1_t a; message_m2_t b; message_m1__init(&a); message_m2__init(&b); return (int)(a.p.x + b.ps.len); }\n"
	if err := os.WriteFile(filepath.Join(dir, "generated", "tu.c"), []byte(tu), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("make", "-C", dir, "SOFAB_C_CORELIB="+corelib, strictMakeVar).CombinedOutput(); err != nil {
		t.Fatalf("a schema with shared $defs does not build:\n%s", out)
	}
	harness := filepath.Join(dir, "harness", "harness")
	for msg, in := range map[string]string{
		"m1": `{"p":{"x":5,"c":1},"c":1,"l":{"a":{"x":-2,"c":0},"tags":["ab"]},"s":{"pt":{"x":3,"c":1}},"s2":{"num":4},"f":3}`,
		"m2": `{"ps":[{"x":1,"c":0},{"x":2,"c":1}],"c":1,"l":{"a":{"x":9,"c":1},"tags":["a","bc"]},"s":{"num":7},"s2":{"pt":{"x":-1,"c":0}},"f":2,"own":{"z":6}}`,
	} {
		enc := exec.Command(harness, "encode", msg)
		enc.Stdin = strings.NewReader(in)
		wire, err := enc.Output()
		if err != nil {
			t.Fatalf("%s: encode: %v", msg, err)
		}
		dec := exec.Command(harness, "decode", msg)
		dec.Stdin = strings.NewReader(string(wire))
		out, err := dec.Output()
		if err != nil {
			t.Fatalf("%s: decode: %v", msg, err)
		}
		for _, frag := range []string{`"c":1`, `"tags":[`} {
			if !strings.Contains(string(out), frag) {
				t.Errorf("%s: round trip lost %s: %s", msg, frag, out)
			}
		}
	}
}
