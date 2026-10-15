package matrix

import (
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/sofa-buffers/generator/internal/config"
	"github.com/sofa-buffers/generator/internal/generator"
	"github.com/sofa-buffers/generator/internal/ir"
)

// updateGolden rewrites the snapshots instead of comparing against them.
//
// The snapshots are a backend's `Generate` output, and regenerating goes
// through this test rather than through the CLI, which calls the same Generate
// this compares. `go run ./cmd/sofabgen` matches it only as long as the run
// formats nothing: for the targets whose canonical formatter is an external
// program (rust, dart, python) a run with --format=auto or --format=require
// pipes the files through it before writing them (generator.Formatter, §8) and
// the result is FORMATTED files this test rejects — on a box that has the
// formatter, and only there. That is one of the reasons --format defaults to
// off: the bytes a plain run writes are the bytes pinned here.
var updateGolden = flag.Bool("update", false,
	"rewrite tests/matrix/testdata/golden* from the backends' Generate output")

// goldenRun is one pinned (schema, backend, config) triple and the directory
// its snapshot lives in. variant is "" for the backend's default run.
type goldenRun struct {
	lang    string
	variant string
	dir     string
	cfg     map[string]any
}

// goldenVariant is one non-default target config pinned beside the default.
type goldenVariant struct {
	lang, name string
	target     map[string]any
}

// goldenSet is one schema, the directory its snapshots live under — one
// subdirectory per run and nothing else — and the config variants pinned for
// it besides every backend's default run.
type goldenSet struct {
	schema   string
	root     string
	variants []goldenVariant
}

// runs is every backend's default run — the effective config the CLI applies
// with no config file, so a golden matches what a plain `sofabgen` writes —
// followed by the set's variants, each in `<lang>-<name>`.
func (s goldenSet) runs() []goldenRun {
	empty := config.Empty()
	var runs []goldenRun
	for _, lang := range generator.Registered() {
		runs = append(runs, goldenRun{lang, "", filepath.Join(s.root, lang), empty.Effective(lang)})
	}
	for _, v := range s.variants {
		cfg := config.Empty()
		cfg.Targets[v.lang] = v.target
		runs = append(runs, goldenRun{v.lang, v.name, filepath.Join(s.root, v.lang+"-"+v.name), cfg.Effective(v.lang)})
	}
	return runs
}

// goldenSets lists every pinned snapshot.
//
// scalars.yaml is the original gate. named_kinds.yaml pins the `enum` and
// `bitfield` output, which scalars.yaml never reached (generator#767), and adds
// one run per config switch that changes that output: the footprint corelibs
// of cpp and rust bind both kinds through other code than their maxspeed
// default, and `allow_dynamic` flips the storage of every enum/bitfield array
// on all four of those configurations (static on the maxspeed default, dynamic
// on the footprint one). A switch belongs here when it changes the emitted
// enum/bitfield shape, and TestGoldenOutput fails a variant whose output equals
// its backend's default run, so one that pins nothing cannot stay listed.
var goldenSets = []goldenSet{
	{
		schema: "corpus/defs/scalars.yaml",
		root:   filepath.Join("testdata", "golden"),
	},
	{
		schema: "corpus/defs/named_kinds.yaml",
		root:   filepath.Join("testdata", "golden_named_kinds"),
		variants: []goldenVariant{
			{"cpp", "static", map[string]any{"allow_dynamic": false}},
			{"cpp", "c-cpp", map[string]any{"corelib": "c-cpp"}},
			{"cpp", "c-cpp-dyn", map[string]any{"corelib": "c-cpp", "allow_dynamic": true}},
			{"rust", "static", map[string]any{"allow_dynamic": false}},
			{"rust", "rs-no-std", map[string]any{"corelib": "rs-no-std"}},
			{"rust", "rs-no-std-dyn", map[string]any{"corelib": "rs-no-std", "allow_dynamic": true}},
		},
	},
}

// generateGolden runs one pinned backend and returns its files keyed by the
// slash-separated path they are pinned under.
func generateGolden(t *testing.T, s *ir.Schema, r goldenRun) map[string][]byte {
	t.Helper()
	b, ok := generator.Lookup(r.lang)
	if !ok {
		t.Fatalf("[%s] backend not registered", r.dir)
	}
	files, err := b.Generate(s, r.cfg)
	if err != nil {
		t.Fatalf("[%s] generate: %v", r.dir, err)
	}
	if len(files) == 0 {
		t.Fatalf("[%s] generated no files", r.dir)
	}
	out := make(map[string][]byte, len(files))
	for _, f := range files {
		out[filepath.ToSlash(f.Path)] = f.Content
	}
	return out
}

// committedGolden reads every file under a run's snapshot directory, keyed the
// way generateGolden keys them.
func committedGolden(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		content, err := os.ReadFile(p)
		out[filepath.ToSlash(rel)] = content
		return err
	})
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("[%s] reading goldens: %v", dir, err)
	}
	return out
}

// compareGolden reports every file that drifted, is missing, or is pinned but
// no longer generated. The last one matters as much as the first: a backend
// that stops emitting a file (or renames it) would otherwise leave a stale
// snapshot behind that pins nothing.
func compareGolden(t *testing.T, r goldenRun, got map[string][]byte, why string) {
	t.Helper()
	want := committedGolden(t, r.dir)
	paths := make([]string, 0, len(got))
	for p := range got {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		w, ok := want[p]
		switch {
		case !ok:
			t.Errorf("[%s] missing golden %s (regenerate)", r.dir, p)
		case string(got[p]) != string(w):
			t.Errorf("[%s] %s %s", r.dir, p, why)
		}
	}
	for p := range want {
		if _, ok := got[p]; !ok {
			t.Errorf("[%s] golden %s is no longer generated (regenerate)", r.dir, p)
		}
	}
}

// writeGolden replaces a run's snapshot directory with what was generated, so
// a file the backend stopped emitting goes too.
func writeGolden(t *testing.T, r goldenRun, got map[string][]byte) {
	t.Helper()
	if err := os.RemoveAll(r.dir); err != nil {
		t.Fatalf("[%s] %v", r.dir, err)
	}
	for p, content := range got {
		golden := filepath.Join(r.dir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(golden), 0o755); err != nil {
			t.Fatalf("[%s] %s: %v", r.dir, golden, err)
		}
		if err := os.WriteFile(golden, content, 0o644); err != nil {
			t.Fatalf("[%s] %s: %v", r.dir, golden, err)
		}
	}
}

// TestGoldenOutput is the M8 reproducibility gate: regenerating every pinned
// schema for every pinned backend and config must be byte-identical to the
// committed snapshots under tests/matrix/testdata/golden*/ — no file drifted,
// none missing, none left over, and no directory left over from a run that is
// no longer pinned. A diff here means output drifted — regenerate with:
//
//	go test ./tests/matrix -run TestGoldenOutput -update
func TestGoldenOutput(t *testing.T) {
	for _, set := range goldenSets {
		s, err := buildIR(t, set.schema)
		if err != nil {
			t.Fatalf("%s: %v", set.schema, err)
		}
		listed := map[string]bool{}
		defaults := map[string]map[string][]byte{}
		for _, r := range set.runs() {
			listed[filepath.Base(r.dir)] = true
			got := generateGolden(t, s, r)
			if r.variant == "" {
				defaults[r.lang] = got
			} else if sameFiles(got, defaults[r.lang]) {
				t.Errorf("[%s] generates exactly what the %s default run does: the variant pins nothing", r.dir, r.lang)
			}
			if *updateGolden {
				writeGolden(t, r, got)
				continue
			}
			compareGolden(t, r, got, "drifted from golden (regenerate if intentional)")
		}
		entries, err := os.ReadDir(set.root)
		if err != nil {
			t.Fatalf("%s: %v", set.root, err)
		}
		for _, e := range entries {
			if listed[e.Name()] {
				continue
			}
			stale := filepath.Join(set.root, e.Name())
			if *updateGolden {
				if err := os.RemoveAll(stale); err != nil {
					t.Fatalf("%s: %v", stale, err)
				}
				continue
			}
			t.Errorf("%s belongs to no pinned run (regenerate)", stale)
		}
	}
}

// sameFiles reports whether two generated file sets are byte-identical.
func sameFiles(a, b map[string][]byte) bool {
	if len(a) != len(b) {
		return false
	}
	for p, c := range a {
		if d, ok := b[p]; !ok || string(c) != string(d) {
			return false
		}
	}
	return true
}

// TestGoldenSetsCoverNamedKinds holds the line generator#767 drew: the golden
// sets must pin an `enum` and a `bitfield` at both edges of every declared
// width (MESSAGE_SPEC §1 — the edge that picks a width and the edge it ends
// at), in every position a backend binds them at, from a $defs and an inline
// declaration alike, plus the declarations whose shape is a branch of its own.
// It reads the pinned schemas' IR, so an edit to named_kinds.yaml that drops a
// case fails here instead of leaving the gate as blind as it was before.
func TestGoldenSetsCoverNamedKinds(t *testing.T) {
	have := map[string]bool{}
	// at records the kind ref names as bound at a position.
	at := func(where string, ref *ir.TypeRef) {
		if k := namedKind(ref); k != "" {
			have[k+" as "+where] = true
		}
	}
	// members records every enum/bitfield a struct or union holds, at prefix.
	members := func(prefix string, ref *ir.TypeRef) {
		if ref == nil || ref.Target == nil {
			return
		}
		nt := ref.Target
		for _, f := range nt.Fields {
			role := prefix
			if nt.Category == ir.CatUnion {
				if nt.IsDefaultOption(f) {
					role += " (default option)"
				} else {
					role += " (other option)"
				}
				if nt.Variant != "" {
					at("split union option", f.Ref)
				}
			}
			at(role, f.Ref)
			at(prefix+" array element", f.ElemRef)
		}
	}
	for _, set := range goldenSets {
		s, err := buildIR(t, set.schema)
		if err != nil {
			t.Fatalf("%s: %v", set.schema, err)
		}
		for _, m := range s.Messages {
			for _, f := range m.Fields {
				at("field", f.Ref)
				if f.Deprecated {
					at("deprecated field", f.Ref)
				} else if f.Description != "" {
					at("described field", f.Ref)
				}
				// An enum field carries its default on the field; a bitfield
				// field's default is the set of flags its type defaults on.
				if f.Default != nil || flagDefaulted(f.Ref) {
					at("defaulted field", f.Ref)
				}
				at("array element", f.ElemRef)
				if f.Default != nil {
					at("defaulted array element", f.ElemRef)
				}
				depth := 2
				for e := f.ElemItems; e != nil; e = e.ElemItems {
					at(fmt.Sprintf("depth-%d row element", depth), e.ElemRef)
					depth++
				}
				if f.Ref != nil && f.Ref.Target != nil {
					switch f.Ref.Target.Category {
					case ir.CatStruct:
						members("struct member", f.Ref)
						for _, g := range f.Ref.Target.Fields {
							if g.Ref != nil && g.Ref.Target != nil && g.Ref.Target.Category == ir.CatStruct {
								members("nested struct member", g.Ref)
							}
						}
					case ir.CatUnion:
						members("union option", f.Ref)
					}
				}
				if f.ElemRef != nil && f.ElemRef.Target != nil {
					switch f.ElemRef.Target.Category {
					case ir.CatStruct:
						members("struct-array element member", f.ElemRef)
					case ir.CatUnion:
						members("union-array element option", f.ElemRef)
					}
				}
			}
		}
		for _, key := range s.NamedOrder {
			nt := s.Named[key]
			ref := &ir.TypeRef{Key: nt.Key, Target: nt}
			k := namedKind(ref)
			if k == "" {
				continue
			}
			if nt.Inline {
				have[k+" declared inline"] = true
			} else {
				have[k+" declared in $defs"] = true
			}
			if k == "enum" {
				lo, hi, _ := ir.EnumWidthRange(ref)
				zero, seen := false, map[int64]bool{}
				for _, c := range nt.Consts {
					// The values the width rule turns on: the width's own
					// edges, and one past each narrower width's.
					for _, edge := range []int64{lo, hi, -129, 128, -32769, 32768} {
						if c.Value == edge {
							have[enumEdge(lo, edge)] = true
						}
					}
					zero = zero || c.Value == 0
					if seen[c.Value] {
						have["enum with an alias constant"] = true
					}
					seen[c.Value] = true
					if c.Description != "" {
						have["enum with a described constant"] = true
					}
				}
				if !zero {
					have["enum without a 0 constant"] = true
				}
				continue
			}
			hi, _ := ir.BitfieldWidthMax(ref)
			if len(nt.Flags) == 0 {
				have["bitfield without a flag"] = true
			}
			for _, fl := range nt.Flags {
				have[bitEdge(hi, fl.Pos)] = true
				if fl.HasDefault && !fl.Default {
					have["bitfield flag with an explicit default: false"] = true
				}
				if fl.Default {
					have["bitfield flag defaulted on"] = true
				}
				if fl.Description != "" {
					have["bitfield flag with a description"] = true
				}
			}
		}
	}
	var want []string
	for _, k := range []string{"enum", "bitfield"} {
		for _, w := range []string{"field", "defaulted field", "deprecated field", "described field",
			"array element", "defaulted array element", "depth-2 row element", "depth-3 row element",
			"struct member", "nested struct member", "struct-array element member",
			"union option (default option)", "union option (other option)", "union option array element",
			"union-array element option (default option)", "split union option"} {
			want = append(want, k+" as "+w)
		}
		want = append(want, k+" declared in $defs", k+" declared inline")
	}
	for _, w := range []ir.Kind{ir.KindI8, ir.KindI16, ir.KindI32} {
		lo, hi, _ := ir.NarrowRange(w)
		want = append(want, enumEdge(lo, lo), enumEdge(lo, hi))
	}
	want = append(want, enumEdge(-32768, -129), enumEdge(-32768, 128),
		enumEdge(-2147483648, -32769), enumEdge(-2147483648, 32768))
	for _, e := range []struct {
		hi       uint64
		low, top int64
	}{{0xFF, 0, 7}, {0xFFFF, 8, 15}, {0xFFFFFFFF, 16, 31}, {0, 32, 63}} {
		want = append(want, bitEdge(e.hi, e.low), bitEdge(e.hi, e.top))
	}
	want = append(want, "enum with an alias constant", "enum with a described constant",
		"enum without a 0 constant", "bitfield without a flag",
		"bitfield flag with an explicit default: false", "bitfield flag defaulted on",
		"bitfield flag with a description")
	for _, w := range want {
		if !have[w] {
			t.Errorf("no golden pins: %s", w)
		}
	}
}

// namedKind names the named leaf kind ref points at, or "" for any other.
func namedKind(ref *ir.TypeRef) string {
	if ref == nil || ref.Target == nil {
		return ""
	}
	switch ref.Target.Category {
	case ir.CatEnum:
		return "enum"
	case ir.CatBitfield:
		return "bitfield"
	}
	return ""
}

// enumEdge names a constant v in an enum whose declared width starts at lo.
func enumEdge(lo, v int64) string {
	return fmt.Sprintf("enum of the width from %d with a constant %d", lo, v)
}

// bitEdge names a flag at pos in a bitfield whose declared width ends at hi
// (0 = the full u64).
func bitEdge(hi uint64, pos int64) string {
	return fmt.Sprintf("bitfield of the width up to %#x with a flag at %d", hi, pos)
}

// flagDefaulted reports whether ref is a bitfield with a flag defaulted on.
func flagDefaulted(ref *ir.TypeRef) bool {
	if ref == nil || ref.Target == nil || ref.Target.Category != ir.CatBitfield {
		return false
	}
	for _, fl := range ref.Target.Flags {
		if fl.Default {
			return true
		}
	}
	return false
}

// stubFormattersOnPath makes PATH hold nothing but a stub for every external
// formatter a backend could reach for, each of which mangles what it is given
// and records that it ran. Anything that spawns one is then visible twice: in
// the bytes and in the marker file whose path is returned.
func stubFormattersOnPath(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	marker := filepath.Join(dir, "spawned")
	// Every binary a generator.Formatter implementor could reach for
	// (ARCHITECTURE §8 names them), plus npx, which is how a prettier is
	// commonly resolved. A formatter added to a backend belongs in this list
	// the same day, or this test cannot see it spawn.
	for _, tool := range []string{"rustfmt", "dart", "ruff", "prettier", "npx", "gofmt", "zig", "clang-format"} {
		script := "#!/bin/sh\necho " + tool + " >> " + marker + "\necho MANGLED\n"
		if err := os.WriteFile(filepath.Join(dir, tool), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir)
	return marker
}

// TestGoldensDoNotDependOnAnyFormatter pins what the snapshots are worth.
//
// A golden is only a regression detector as long as the same inputs produce the
// same bytes everywhere and forever. An external formatter is neither: rustfmt,
// `dart format` and ruff each change their layout between releases, and none of
// them is installed on every machine. If the snapshots were formatter output, a
// formatter release — or a colleague without the tool — would "move" a golden
// that no backend touched.
//
// They are not: `Generate` is a pure function of (IR, config) and spawns
// nothing, the optional formatter pass lives in the CLI behind `--format`
// (default `off`, §8/§12 gate 10), and `-update` above writes exactly the bytes
// this compares. This test holds that line: with every formatter on PATH
// replaced by a stub that would mangle whatever reached it, every pinned run's
// output still equals the committed golden, and no stub ran.
func TestGoldensDoNotDependOnAnyFormatter(t *testing.T) {
	marker := stubFormattersOnPath(t)
	for _, set := range goldenSets {
		s, err := buildIR(t, set.schema)
		if err != nil {
			t.Fatalf("%s: %v", set.schema, err)
		}
		for _, r := range set.runs() {
			compareGolden(t, r, generateGolden(t, s, r),
				"differs from its golden with formatters stubbed out: "+
					"Generate must not depend on an external tool")
		}
	}
	if ran, err := os.ReadFile(marker); err == nil {
		t.Errorf("Generate spawned an external formatter: %s", ran)
	}
}
