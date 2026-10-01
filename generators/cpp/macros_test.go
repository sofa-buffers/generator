package cpp

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

var updateMacros = flag.Bool("update-macros", false, "add every measured header macro missing from macros.go to its list")

// macroProbeHeaders are the standard headers a generated C++ file reaches:
// the message header's own includes and the project harness's.
var macroProbeHeaders = []string{
	"cstdint", "string", "vector", "span", "cstddef", "utility", "variant", "new", "memory",
	"iostream", "sstream", "fstream", "ostream", "cstdio", "cstdlib", "cstring",
}

// macroToolchains are the compilers the list is measured with: the host's
// (glibc) and the embedded one (newlib, plain and nano).
var macroToolchains = []struct {
	cxx   string
	flags []string
}{
	{"g++", nil},
	{"arm-none-eabi-g++", nil},
	{"arm-none-eabi-g++", []string{"--specs=nano.specs"}},
}

var macroIdent = regexp.MustCompile(`^#define ([A-Za-z][A-Za-z0-9]*(?:_[A-Za-z0-9]+)*)[ (]`)

// measureHeaderMacros preprocesses a TU that includes everything a generated
// file reaches, against both corelibs, in -std=c++20 and -std=gnu++20, with
// every toolchain in macroToolchains, and returns every macro whose name a
// schema name or a type identifier can spell -- SOFAB_* and SOFABGEN_* aside,
// which are escaped by prefix.
func measureHeaderMacros(t *testing.T, cppDir, cDir string) map[string]bool {
	t.Helper()
	tmp := t.TempDir()
	var tu strings.Builder
	for _, h := range macroProbeHeaders {
		fmt.Fprintf(&tu, "#include <%s>\n", h)
	}
	tu.WriteString("#include \"sofab/sofab.hpp\"\n#include \"sofab_test_json.h\"\n")
	cppTU := filepath.Join(tmp, "probe_cpp.cpp")
	ccppTU := filepath.Join(tmp, "probe_ccpp.cpp")
	if err := os.WriteFile(cppTU, []byte(tu.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ccppTU, []byte(tu.String()+"#include \"sofab/object.h\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	corelibs := []struct {
		tu   string
		incs []string
	}{
		{cppTU, []string{filepath.Join(cppDir, "include"), filepath.Join(cDir, "test", "shared")}},
		{ccppTU, []string{filepath.Join(cDir, "src", "include"), filepath.Join(cDir, "test", "shared")}},
	}
	got := map[string]bool{}
	for _, tc := range macroToolchains {
		for _, std := range []string{"c++20", "gnu++20"} {
			for _, cl := range corelibs {
				args := append([]string{"-std=" + std, "-dM", "-E"}, tc.flags...)
				for _, inc := range cl.incs {
					args = append(args, "-I"+inc)
				}
				args = append(args, cl.tu)
				var out, stderr bytes.Buffer
				cmd := exec.Command(tc.cxx, args...)
				cmd.Stdout, cmd.Stderr = &out, &stderr
				if err := cmd.Run(); err != nil {
					t.Fatalf("%s %s: %v\n%s", tc.cxx, strings.Join(args, " "), err, stderr.String())
				}
				for _, line := range strings.Split(out.String(), "\n") {
					m := macroIdent.FindStringSubmatch(line)
					if m == nil || sofabPrefixed(m[1]) {
						continue
					}
					got[m[1]] = true
				}
			}
		}
	}
	return got
}

// TestHeaderMacrosAreListed re-measures the macro list: every macro the
// headers define, on glibc and on newlib, must be on it, or a schema name
// spelled like one compiles into whatever the macro expands to. Needs g++,
// arm-none-eabi-g++ and both corelibs (SOFAB_CPP_DIR, SOFAB_C_DIR -- the
// variables run.sh and the generated Makefile read); skipped otherwise.
// -update-macros adds the missing names to macros.go; then regenerate the
// collision schema with -update.
func TestHeaderMacrosAreListed(t *testing.T) {
	for _, tc := range macroToolchains {
		if _, err := exec.LookPath(tc.cxx); err != nil {
			t.Skipf("%s not on PATH", tc.cxx)
		}
	}
	cppDir, cDir := os.Getenv("SOFAB_CPP_DIR"), os.Getenv("SOFAB_C_DIR")
	if cppDir == "" || cDir == "" {
		t.Skip("SOFAB_CPP_DIR and SOFAB_C_DIR must name the corelib-cpp and corelib-c-cpp checkouts")
	}
	measured := measureHeaderMacros(t, cppDir, cDir)
	var missing []string
	for n := range measured {
		if !cppHeaderMacros[n] {
			missing = append(missing, n)
		}
	}
	sort.Strings(missing)
	if len(missing) == 0 {
		return
	}
	if *updateMacros {
		if err := rewriteMacroList(missing); err != nil {
			t.Fatal(err)
		}
		t.Logf("added %d names to macros.go; regenerate reserved.yaml with -update", len(missing))
		return
	}
	t.Errorf("%d header macros are not on the list (run with -update-macros): %s", len(missing), strings.Join(missing, " "))
}

// rewriteMacroList writes the list in macros.go as the sorted union of what it
// holds and add, eight names to a line.
func rewriteMacroList(add []string) error {
	const path = "macros.go"
	src, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	names := append([]string{}, add...)
	for n := range cppHeaderMacros {
		names = append(names, n)
	}
	sort.Strings(names)
	var b strings.Builder
	b.WriteString("const cppHeaderMacroList = `\n")
	for i := 0; i < len(names); i += 8 {
		end := min(i+8, len(names))
		b.WriteString(strings.Join(names[i:end], " ") + "\n")
	}
	b.WriteString("`\n")
	s := string(src)
	start := strings.Index(s, "const cppHeaderMacroList = `")
	if start < 0 {
		return fmt.Errorf("%s: no cppHeaderMacroList", path)
	}
	end := strings.Index(s[start+len("const cppHeaderMacroList = `"):], "`")
	if end < 0 {
		return fmt.Errorf("%s: unterminated cppHeaderMacroList", path)
	}
	end += start + len("const cppHeaderMacroList = `") + 1
	for end < len(s) && s[end] == '\n' {
		end++
	}
	return os.WriteFile(path, []byte(s[:start]+b.String()+s[end:]), 0o644)
}
