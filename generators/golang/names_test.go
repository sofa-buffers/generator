package golang

import (
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// namesYAML is the shared name-collision schema (ARCHITECTURE §8, "Naming").
const namesYAML = "../../tests/conformance/lib/names.yaml"

// TestNamesSchemaDeclaresEachNameOnce generates the shared name-collision
// schema and parses every file of the generated package: each package-level
// name -- type, constant, variable, function -- must be declared exactly once
// across the package, and each method once per receiver type. This is the
// guarantee the conformance suite proves by compiling (tests/conformance/go),
// checked here without a toolchain.
func TestNamesSchemaDeclaresEachNameOnce(t *testing.T) {
	for _, emit := range []string{"sources", "project"} {
		files := genGo(t, schemaFromYAMLFile(t, namesYAML), map[string]any{"emit": emit})
		pkg := map[string][]string{} // name -> files declaring it
		methods := map[string][]string{}
		fset := token.NewFileSet()
		for path, src := range files {
			if !strings.HasSuffix(path, ".go") || strings.HasPrefix(path, "harness/") {
				continue
			}
			f, err := parser.ParseFile(fset, path, src, 0)
			if err != nil {
				t.Fatalf("%s: %s does not parse: %v", emit, path, err)
			}
			for _, d := range f.Decls {
				switch d := d.(type) {
				case *ast.FuncDecl:
					if d.Recv == nil {
						pkg[d.Name.Name] = append(pkg[d.Name.Name], path)
						continue
					}
					recv := d.Recv.List[0].Type
					if star, ok := recv.(*ast.StarExpr); ok {
						recv = star.X
					}
					key := recv.(*ast.Ident).Name + "." + d.Name.Name
					methods[key] = append(methods[key], path)
				case *ast.GenDecl:
					for _, s := range d.Specs {
						switch s := s.(type) {
						case *ast.TypeSpec:
							pkg[s.Name.Name] = append(pkg[s.Name.Name], path)
						case *ast.ValueSpec:
							for _, n := range s.Names {
								pkg[n.Name] = append(pkg[n.Name], path)
							}
						}
					}
				}
			}
		}
		if len(pkg) == 0 {
			t.Fatalf("%s: no declarations parsed", emit)
		}
		for _, set := range []map[string][]string{pkg, methods} {
			for n, where := range set {
				if len(where) != 1 {
					sort.Strings(where)
					t.Errorf("%s: %s declared %d times (%s)", emit, n, len(where), strings.Join(where, ", "))
				}
			}
		}
		// A few spellings of the contract, pinned so the test above cannot pass
		// on a schema that no longer reaches them.
		for _, n := range []string{
			"M", "M_A", "MA", "M_Decoder", "MDecoder", "M__New", "NewM", "M__Decode", "M__DecodeFrom",
			"M_Arr", "M_ArrElem", "M_U", "M_U_Default", "M_U_Id__ID", "M_U_Default__ID",
			"Shape__DefaultNum", "Shape__DefaultPt", "Shape_Pt", "Shape_Num__ID", "ShapeDefault", "ShapeDefault_Pt", "ShapeDefaultPt",
			"Color", "Color_Red", "ColorRed", "Flags", "Flags_On", "FlagsOn", "Point", "StructPoint",
			"A_BC", "AB_C", "MaxDynStringLen_", "_M__EncOpts",
		} {
			if len(pkg[n]) != 1 {
				t.Errorf("%s: %s not declared once (%v)", emit, n, pkg[n])
			}
		}
	}
}

// TestNamesSchemaFilesAreBuilt writes the generated package of the shared
// name-collision schema to disk and asks go/build which files it compiles,
// for a Unix and a Windows target: a message file named like `*_test.go` or
// `*_<GOOS>.go` would be silently left out, and one named like a fixed file
// would have overwritten it.
func TestNamesSchemaFilesAreBuilt(t *testing.T) {
	files := genGo(t, schemaFromYAMLFile(t, namesYAML), map[string]any{"emit": "sources"})
	dir := t.TempDir()
	var want []string
	folded := map[string]string{}
	for path, src := range files {
		if prev, dup := folded[strings.ToLower(path)]; dup {
			t.Errorf("%s and %s are one file on a case-insensitive filesystem", prev, path)
		}
		folded[strings.ToLower(path)] = path
		if err := os.WriteFile(filepath.Join(dir, path), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
		want = append(want, path)
	}
	sort.Strings(want)
	for _, target := range [][2]string{{"linux", "amd64"}, {"windows", "arm64"}, {"darwin", "arm64"}} {
		ctx := build.Default
		ctx.GOOS, ctx.GOARCH = target[0], target[1]
		p, err := ctx.ImportDir(dir, 0)
		if err != nil {
			t.Fatalf("%s/%s: %v", target[0], target[1], err)
		}
		got := append([]string(nil), p.GoFiles...)
		sort.Strings(got)
		if strings.Join(got, " ") != strings.Join(want, " ") {
			t.Errorf("%s/%s builds %d of %d files; left out: %v %v",
				target[0], target[1], len(got), len(want), p.IgnoredGoFiles, p.TestGoFiles)
		}
	}
}
