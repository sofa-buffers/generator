package cpp

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

// nsDeclRe matches a namespace-level type definition in a generated header.
var nsDeclRe = regexp.MustCompile(`(?m)^(?:struct (\w+) : |enum class (\w+) : |enum (\w+) : )`)

// TestSharedDefsDefinedOnce: on every profile, each namespace-level type is
// defined in exactly one header of the run -- a $defs type used by two messages
// lives in the shared $defs header, which both message headers include, and a
// message that uses none does not include it.
func TestSharedDefsDefinedOnce(t *testing.T) {
	for _, p := range unionProfiles {
		files := unionFiles(t, sharedDefsYAML, p.cfg)
		seen := map[string][]string{}
		var paths []string
		for path := range files {
			if strings.HasSuffix(path, ".hpp") && !strings.HasPrefix(path, "harness/") {
				paths = append(paths, path)
			}
		}
		sort.Strings(paths)
		for _, path := range paths {
			for _, m := range nsDeclRe.FindAllStringSubmatch(files[path], -1) {
				name := m[1] + m[2] + m[3]
				seen[name] = append(seen[name], path)
			}
		}
		for name, where := range seen {
			if len(where) != 1 {
				t.Errorf("%s: %s is defined %d times: %v", p.name, name, len(where), where)
			}
		}
		for _, want := range []string{"Point", "Line", "Color", "Flags", "Shape_default_Num", "Shape_default_Pt"} {
			if w := seen[want]; len(w) != 1 || w[0] != "sofab-defs.hpp" {
				t.Errorf("%s: %s must be defined in the shared $defs header, is in %v", p.name, want, w)
			}
		}
		if w := seen["M2_Own"]; len(w) != 1 || w[0] != "m2.hpp" {
			t.Errorf("%s: an inline type stays in its message's header, M2_Own is in %v", p.name, w)
		}
		for _, f := range []string{"m1.hpp", "m2.hpp"} {
			if !strings.Contains(files[f], `#include "sofab-defs.hpp"`) {
				t.Errorf("%s: %s must include the shared $defs header", p.name, f)
			}
		}
		if strings.Contains(files["m3.hpp"], "sofab-defs.hpp") {
			t.Errorf("%s: m3 uses no $defs type and must not include the shared $defs header", p.name)
		}
	}
}

// TestSharedDefsCompile: on every profile, the harness -- which includes every
// header -- compiles under the strict warning set, and two translation units
// that each include every header link together. Gated on the corelib checkouts
// (SOFAB_CPP_DIR, SOFAB_C_DIR), like the union compile gate.
func TestSharedDefsCompile(t *testing.T) {
	cpp, cc := os.Getenv("SOFAB_CPP_DIR"), os.Getenv("SOFAB_C_DIR")
	if cpp == "" || cc == "" {
		t.Skip("set SOFAB_CPP_DIR and SOFAB_C_DIR to corelib checkouts to run the compile gate")
	}
	gxx, err := exec.LookPath("g++")
	if err != nil {
		t.Skip("g++ not found")
	}
	const tu = `#include "m1.hpp"
#include "m2.hpp"
#include "m3.hpp"
#include "sofab-defs.hpp"
`
	for _, p := range unionProfiles {
		dir := t.TempDir()
		for path, content := range unionFiles(t, sharedDefsYAML, p.cfg) {
			full := filepath.Join(dir, path)
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		for name, body := range map[string]string{
			"a.cpp": tu + "int use_a() { sofabuffers::M1 m; return static_cast<int>(m.p.x); }\n",
			"b.cpp": tu + "int use_b() { sofabuffers::M2 m; return static_cast<int>(m.ps.size()); }\n",
		} {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		inc := "-I" + filepath.Join(cpp, "include")
		if p.cfg["corelib"] == "c-cpp" {
			inc = "-I" + filepath.Join(cc, "src", "include")
		}
		var objs []string
		for _, src := range []string{"harness/main.cpp", "a.cpp", "b.cpp"} {
			obj := filepath.Join(dir, strings.ReplaceAll(src, "/", "_")+".o")
			args := []string{"-std=c++20", "-Wall", "-Wextra", "-Werror", "-c", "-o", obj, inc,
				"-I" + filepath.Join(cc, "test", "shared"), "-I" + dir, filepath.Join(dir, src)}
			if out, err := exec.Command(gxx, args...).CombinedOutput(); err != nil {
				t.Fatalf("%s: %s does not compile with shared $defs:\n%s", p.name, src, out)
			}
			objs = append(objs, obj)
		}
		// A relocatable link resolves nothing external but still refuses a
		// symbol two objects both define.
		args := append([]string{"-r", "-nostdlib", "-o", filepath.Join(dir, "all.o")}, objs...)
		if out, err := exec.Command(gxx, args...).CombinedOutput(); err != nil {
			t.Fatalf("%s: the headers do not link into one program:\n%s", p.name, out)
		}
	}
}
