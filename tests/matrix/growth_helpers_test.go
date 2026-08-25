package matrix

import (
	"bufio"
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sofa-buffers/generator/internal/generator"
)

// growthHelpers are the corelib entry points that GROW a container from a wire
// count, one per port that had one. Generated code must not call any of them.
//
// ARCHITECTURE §9.5: a decoder allocates from a wire count only after checking
// it, and only for shape B — a sequence/wrapper array, whose length is "highest
// present id + 1" and is therefore not on the wire at all. Every other array
// arrives with its count in front of it, so the count is checked once and the
// container allocated exactly that long: there is no growth left to do, and a
// helper that grows is a second, unchecked allocation path beside the checked
// one.
//
// The call sites went in generator#396; this keeps them gone. It is a sweep over
// EMITTED TEXT rather than over the emitters, because that is the property that
// matters and the only one a restructuring corelib cannot quietly re-establish:
// a helper is reintroduced by a backend emitting its name, whatever the emitter
// looks like (generator#402 item 4).
var growthHelpers = []string{
	"ensureCap",      // corelib-java, corelib-kotlin-mp
	"ARRAY_INIT_CAP", // corelib-java
	"ArrayInitCap",   // corelib-cs
	"allocCapped",    // corelib-rs, corelib-rs-no-std
}

// zigPutGrowing is the one exception, and it is a no-op one. corelib-zig's
// `putGrowing` is still the store on a nested native row, but its growth branch
// is unreachable: the row slice is already `count` long by the time an element
// lands, so `i >= s.len` implies `i >= n` and the helper returns on its first
// line. Reducing it to the bounded put it has become is corelib-zig's half
// (corelib-zig#67); this test pins that no OTHER backend grows and that zig's
// use stays confined to zig, so the day corelib-zig#67 lands the last line here
// goes with it.
const zigPutGrowing = "putGrowing"

func TestGeneratedCodeCallsNoCorelibGrowthHelper(t *testing.T) {
	defs, _ := filepath.Glob("corpus/defs/*.yaml")
	defs = append(defs,
		filepath.Join("..", "..", "examples", "messages", "example.yaml"),
		filepath.Join("..", "..", "examples", "messages", "realworld", "vehicle_telemetry.yaml"))
	if len(defs) < 3 {
		t.Fatal("no defs found")
	}
	modes := []map[string]any{
		{"emit": "sources"},
		{"emit": "project", "timestamp": false},
	}

	for _, def := range defs {
		s, err := buildIR(t, def)
		if err != nil {
			t.Fatalf("%s should validate: %v", def, err)
		}
		for _, lang := range generator.Registered() {
			if fixedOnlyTarget(lang) && hasUnboundedField(s) {
				continue // heapless target cannot size an unbounded field
			}
			b, _ := generator.Lookup(lang)
			for _, cfg := range modes {
				files, err := b.Generate(s, cfg)
				if err != nil {
					continue // generate errors are covered by the other sweeps
				}
				for _, f := range files {
					sc := bufio.NewScanner(bytes.NewReader(f.Content))
					sc.Buffer(make([]byte, 0, 1<<20), 1<<20)
					for n := 1; sc.Scan(); n++ {
						line := sc.Text()
						for _, helper := range growthHelpers {
							if strings.Contains(line, helper) {
								t.Errorf("[%s] %s (%s) line %d calls the corelib growth helper %q — "+
									"a counted array is allocated exactly once, from a count that "+
									"was already checked (ARCHITECTURE §9.5):\n  %s",
									lang, f.Path, filepath.Base(def), n, helper, strings.TrimSpace(line))
							}
						}
						if lang != "zig" && strings.Contains(line, zigPutGrowing) {
							t.Errorf("[%s] %s (%s) line %d calls %q, which is corelib-zig's and is "+
								"a growth path everywhere else:\n  %s",
								lang, f.Path, filepath.Base(def), n, zigPutGrowing, strings.TrimSpace(line))
						}
					}
				}
			}
		}
	}
}
