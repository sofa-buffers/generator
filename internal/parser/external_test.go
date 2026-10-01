package parser

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeFiles writes name -> content under a fresh temp dir and returns it.
func writeFiles(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// loadValid loads and hard-gate validates path, failing the test on any error.
func loadValid(t *testing.T, path string) *Document {
	t.Helper()
	doc, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	resolved, err := doc.Resolve()
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if errs := Validate(resolved); errs != nil {
		t.Fatalf("validate: %v", errs)
	}
	return doc
}

func wantErr(t *testing.T, err error, parts ...string) {
	t.Helper()
	if err == nil {
		t.Fatalf("want an error containing %q, got none", parts)
	}
	for _, p := range parts {
		if !strings.Contains(err.Error(), p) {
			t.Errorf("error does not contain %q:\n%v", p, err)
		}
	}
}

func defsOf(doc *Document, cat string) map[string]any {
	defs, _ := doc.Root.(map[string]any)["$defs"].(map[string]any)
	g, _ := defs[cat].(map[string]any)
	return g
}

const pointU8 = "version: 1\n$defs:\n  struct:\n    point: { x: { id: 0, type: u8 } }\n"

// Two files each define a struct `point`, differently: importing both is an
// ambiguous schema, not one struct with the first file's layout.
func TestCrossFileSameNameDifferentDefinitionsRefused(t *testing.T) {
	dir := writeFiles(t, map[string]string{
		"main.yaml": `version: 1
messages:
  m:
    payload:
      a: { id: 0, type: struct, fields: { $ref: 'a.yaml#/$defs/struct/point' } }
      b: { id: 1, type: struct, fields: { $ref: 'b.yaml#/$defs/struct/point' } }
`,
		"a.yaml": pointU8,
		"b.yaml": "version: 1\n$defs:\n  struct:\n    point: { label: { id: 0, type: string, maxlen: 8 } }\n",
	})
	_, err := Load(filepath.Join(dir, "main.yaml"))
	wantErr(t, err,
		`#/messages/m/payload/b/fields: $ref b.yaml#/$defs/struct/point: "point" is already defined by a.yaml#/$defs/struct/point (imported for #/messages/m/payload/a/fields)`,
		"one namespace for messages and $defs")
}

// A local `point` and an imported `point` are two types; the field that refs
// the import must not silently take the local layout.
func TestCrossFileImportClashesWithLocalDefinition(t *testing.T) {
	dir := writeFiles(t, map[string]string{
		"main.yaml": `version: 1
$defs:
  struct:
    point: { z: { id: 0, type: fp64 } }
messages:
  m:
    payload:
      l: { id: 0, type: struct, fields: { $ref: '#/$defs/struct/point' } }
      a: { id: 1, type: struct, fields: { $ref: 'a.yaml#/$defs/struct/point' } }
`,
		"a.yaml": pointU8,
	})
	_, err := Load(filepath.Join(dir, "main.yaml"))
	wantErr(t, err, `#/messages/m/payload/a/fields: $ref a.yaml#/$defs/struct/point: "point" is already defined by #/$defs/struct/point;`)
}

// Rule 3 is by fold and across categories: an imported enum `Point` takes the
// name of a local message `point`.
func TestCrossFileImportClashesByFoldAcrossCategories(t *testing.T) {
	dir := writeFiles(t, map[string]string{
		"main.yaml": `version: 1
messages:
  point:
    payload:
      e: { id: 0, type: enum, enum: { $ref: 'a.yaml#/$defs/enum/Point' }, default: 0 }
`,
		"a.yaml": "version: 1\n$defs:\n  enum:\n    Point: { A: 0 }\n",
	})
	_, err := Load(filepath.Join(dir, "main.yaml"))
	wantErr(t, err, `"Point" is already defined by #/messages/point`)
}

// The same definition reached directly and transitively (through another
// file's definition) is one type.
func TestCrossFileSameDefinitionTwiceIsOneType(t *testing.T) {
	dir := writeFiles(t, map[string]string{
		"main.yaml": `version: 1
messages:
  m:
    payload:
      v: { id: 0, type: struct, fields: { $ref: 'lib/vec.yaml#/$defs/struct/Vec' } }
      s: { id: 1, type: struct, fields: { $ref: 'lib/seg.yaml#/$defs/struct/Seg' } }
`,
		"lib/vec.yaml": "version: 1\n$defs:\n  struct:\n    Vec: { x: { id: 0, type: fp32 } }\n",
		"lib/seg.yaml": `version: 1
$defs:
  struct:
    Seg:
      a: { id: 0, type: struct, fields: { $ref: 'vec.yaml#/$defs/struct/Vec' } }
      b: { id: 1, type: struct, fields: { $ref: 'vec.yaml#/$defs/struct/Vec' } }
`,
	})
	doc := loadValid(t, filepath.Join(dir, "main.yaml"))
	g := defsOf(doc, "struct")
	if len(g) != 2 || g["Vec"] == nil || g["Seg"] == nil {
		t.Fatalf("want exactly Vec and Seg imported, got %v", g)
	}
	if got := doc.Origins["#/$defs/struct/Vec"]; filepath.Base(got.File) != "vec.yaml" {
		t.Errorf("Vec origin = %+v, want lib/vec.yaml", got)
	}
}

// Two different files carrying a structurally identical definition under one
// name are one type: nothing is lost by generating it once.
func TestCrossFileIdenticalDefinitionsMerge(t *testing.T) {
	dir := writeFiles(t, map[string]string{
		"main.yaml": `version: 1
messages:
  m:
    payload:
      a: { id: 0, type: struct, fields: { $ref: 'a.yaml#/$defs/struct/point' } }
      b: { id: 1, type: struct, fields: { $ref: 'b.yaml#/$defs/struct/point' } }
`,
		"a.yaml": pointU8,
		"b.yaml": pointU8,
	})
	doc := loadValid(t, filepath.Join(dir, "main.yaml"))
	if g := defsOf(doc, "struct"); len(g) != 1 {
		t.Fatalf("want one struct point, got %v", g)
	}
}

// Identical bodies whose dependencies differ under one name are not the same
// type: the dependency is refused on its own import.
func TestCrossFileIdenticalBodyDifferentDependencyRefused(t *testing.T) {
	dir := writeFiles(t, map[string]string{
		"main.yaml": `version: 1
messages:
  m:
    payload:
      a: { id: 0, type: struct, fields: { $ref: 'a.yaml#/$defs/struct/Box' } }
      b: { id: 1, type: struct, fields: { $ref: 'b.yaml#/$defs/struct/Box' } }
`,
		"a.yaml": `version: 1
$defs:
  struct:
    Box: { p: { id: 0, type: struct, fields: { $ref: '#/$defs/struct/P' } } }
    P: { x: { id: 0, type: u8 } }
`,
		"b.yaml": `version: 1
$defs:
  struct:
    Box: { p: { id: 0, type: struct, fields: { $ref: '#/$defs/struct/P' } } }
    P: { x: { id: 0, type: u16 } }
`,
	})
	_, err := Load(filepath.Join(dir, "main.yaml"))
	wantErr(t, err, `b.yaml#/$defs/struct/Box/p/fields: $ref #/$defs/struct/P: "P" is already defined by a.yaml#/$defs/struct/P (imported for a.yaml#/$defs/struct/Box/p/fields)`)
}

// The checked-in corpus case: Bounds needs Vec3 through a same-file ref, and
// Vec3 is also referenced directly; with an enum and a bitfield beside them.
func TestCrossFileCorpusTransitive(t *testing.T) {
	doc := loadValid(t, filepath.Join("..", "..", "tests", "matrix", "corpus", "defs", "cross_file.yaml"))
	for cat, want := range map[string][]string{"struct": {"Bounds", "Vec3"}, "enum": {"Unit"}, "bitfield": {"Caps"}} {
		g := defsOf(doc, cat)
		if len(g) != len(want) {
			t.Errorf("%s: got %d definitions, want %v", cat, len(g), want)
		}
		for _, n := range want {
			if g[n] == nil {
				t.Errorf("%s: %s not imported", cat, n)
			}
		}
	}
}

// Files that import each other's definitions terminate.
func TestCrossFileCycleBetweenFiles(t *testing.T) {
	dir := writeFiles(t, map[string]string{
		"main.yaml": `version: 1
messages:
  m:
    payload:
      a: { id: 0, type: struct, fields: { $ref: 'a.yaml#/$defs/struct/A' } }
`,
		"a.yaml": `version: 1
$defs:
  struct:
    A: { b: { id: 0, type: struct, fields: { $ref: 'b.yaml#/$defs/struct/B' } } }
`,
		"b.yaml": `version: 1
$defs:
  struct:
    B: { n: { id: 0, type: u8 } }
    C: { a: { id: 0, type: struct, fields: { $ref: 'a.yaml#/$defs/struct/A' } } }
`,
	})
	doc := loadValid(t, filepath.Join(dir, "main.yaml"))
	if g := defsOf(doc, "struct"); len(g) != 2 {
		t.Fatalf("want A and B, got %v", g)
	}
}

// A validation error inside an imported definition names its real file, not a
// #/$defs pointer the user's file does not have.
func TestImportedDefinitionErrorIsLocatedInItsFile(t *testing.T) {
	dir := writeFiles(t, map[string]string{
		"main.yaml": `version: 1
messages:
  m:
    payload:
      a: { id: 0, type: struct, fields: { $ref: 'lib/a.yaml#/$defs/struct/P' } }
`,
		"lib/a.yaml": "version: 1\n$defs:\n  struct:\n    P: { x: { id: 0, type: u8, default: 999 } }\n",
	})
	doc, err := Load(filepath.Join(dir, "main.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := doc.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	errs := Validate(resolved)
	if errs == nil {
		t.Fatal("want a validation error")
	}
	// The resolved tree reaches the field through the message; the unresolved
	// document's own $defs copy is what Locate maps.
	if got := doc.Locate("#/$defs/struct/P/x"); got != "lib/a.yaml#/$defs/struct/P/x" {
		t.Errorf("Locate = %q", got)
	}
	if got := doc.Locate("#/messages/m/payload/a"); got != "#/messages/m/payload/a" {
		t.Errorf("a local location must stay as it is, got %q", got)
	}
}

func TestCrossFileMissingDefinitionIsLocated(t *testing.T) {
	dir := writeFiles(t, map[string]string{
		"main.yaml": `version: 1
messages:
  m:
    payload:
      a: { id: 0, type: struct, fields: { $ref: 'a.yaml#/$defs/struct/Nope' } }
`,
		"a.yaml": pointU8,
	})
	_, err := Load(filepath.Join(dir, "main.yaml"))
	wantErr(t, err, `#/messages/m/payload/a/fields: $ref a.yaml#/$defs/struct/Nope: no $defs/struct/Nope in a.yaml`)
}
