package naming

import (
	"math/rand"
	"strings"
	"testing"
)

func TestIsDeviceStem(t *testing.T) {
	for _, s := range []string{"con", "CON", "Prn", "aux", "nul", "com0", "COM9", "lpt1", "Lpt5"} {
		if !IsDeviceStem(s) {
			t.Errorf("IsDeviceStem(%q) = false, want true", s)
		}
	}
	for _, s := range []string{"conn", "co", "com", "com10", "lpt", "com_1", "nul_", "aux1", "console", "m"} {
		if IsDeviceStem(s) {
			t.Errorf("IsDeviceStem(%q) = true, want false", s)
		}
	}
}

func TestPascal(t *testing.T) {
	for in, want := range map[string]string{
		"vehicle_telemetry": "VehicleTelemetry",
		"fooBar":            "FooBar",
		"m_a":               "MA",
		"mA":                "MA",
		"a_1b":              "A1b",
		"A1b":               "A1b",
		"HTTPServer":        "HTTPServer",
		"x_y_z":             "XYZ",
		"point":             "Point",
	} {
		if got := Pascal(in); got != want {
			t.Errorf("Pascal(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTypeIdent(t *testing.T) {
	for _, c := range []struct {
		path []string
		want string
	}{
		{[]string{"vehicle_telemetry"}, "VehicleTelemetry"},
		{[]string{"m", "a"}, "M_A"},
		{[]string{"m_a"}, "MA"},
		{[]string{"m", "a_b"}, "M_AB"},
		{[]string{"m_a", "b"}, "MA_B"},
		{[]string{"point"}, "Point"},
		{[]string{"struct_point"}, "StructPoint"},
	} {
		if got := TypeIdent(c.path); got != c.want {
			t.Errorf("TypeIdent(%v) = %q, want %q", c.path, got, c.want)
		}
	}
}

// adversarial names: every pair here folds together, or Pascal-cases or
// joins into another one, when the naming rules are ignored.
var seeds = []string{
	"m", "M", "a", "A", "b", "m_a", "mA", "M_a", "ma", "a_b", "aB", "ab", "a_1b", "A1b", "a1b",
	"x_y_z", "xYZ", "HTTPServer", "HttpServer", "http_server", "struct", "point", "struct_point",
	"decoder", "Decoder", "visitor", "default", "elem", "a_elem", "id", "ID", "Id", "new_m", "new",
}

// randomName draws a name the validator accepts: seeds, and random spellings
// over a small alphabet so that near-collisions are frequent.
func randomName(r *rand.Rand) string {
	if r.Intn(3) == 0 {
		return seeds[r.Intn(len(seeds))]
	}
	const letters = "aAbBmM"
	const alnum = "aAbBmM1"
	var b strings.Builder
	b.WriteByte(letters[r.Intn(len(letters))])
	for n := r.Intn(4); n > 0; n-- {
		if r.Intn(3) == 0 {
			b.WriteByte('_')
		}
		b.WriteByte(alnum[r.Intn(len(alnum))])
	}
	return b.String()
}

// randomScope draws up to n names that the validator accepts together in one
// scope: valid spellings with pairwise distinct folds.
func randomScope(r *rand.Rand, n int) []string {
	seen := map[string]bool{}
	var out []string
	for i := 0; i < n*3 && len(out) < n; i++ {
		name := randomName(r)
		if !NameRe.MatchString(name) || seen[Fold(name)] {
			continue
		}
		seen[Fold(name)] = true
		out = append(out, name)
	}
	return out
}

// randomPaths draws a schema-shaped set of type paths: a top-level scope
// (messages and $defs share it), and below each name a few levels of
// member scopes, every scope drawn under the naming rules.
func randomPaths(r *rand.Rand) [][]string {
	var paths [][]string
	var walk func(prefix []string, depth int)
	walk = func(prefix []string, depth int) {
		if depth == 0 {
			return
		}
		for _, name := range randomScope(r, 4) {
			p := append(append([]string(nil), prefix...), name)
			paths = append(paths, p)
			walk(p, depth-1)
		}
	}
	walk(nil, 3)
	return paths
}

// Distinct paths never meet in any encoding, nor through the channels a
// backend adds to them: a role after "__", an escape "_" at the end.
func TestEncodingsAreInjective(t *testing.T) {
	r := rand.New(rand.NewSource(624))
	for round := 0; round < 2000; round++ {
		paths := randomPaths(r)
		enc := map[string]map[string]string{"TypeIdent": {}, "CPath": {}, "Lower": {}}
		idents := map[string]bool{}
		for _, p := range paths {
			key := strings.Join(p, ".")
			ti := TypeIdent(p)
			idents[ti] = true
			for kind, got := range map[string]string{"TypeIdent": ti, "CPath": CPath(p), "Lower": Lower(p)} {
				if prev, dup := enc[kind][got]; dup && prev != key {
					t.Fatalf("round %d: %s(%s) = %s(%s) = %q", round, kind, prev, kind, key, got)
				}
				enc[kind][got] = key
			}
			if strings.Contains(ti, "__") || strings.HasSuffix(ti, "_") || ti[0] < 'A' || ti[0] > 'Z' {
				t.Fatalf("TypeIdent(%v) = %q leaves its channel", p, ti)
			}
			if strings.Contains(CPath(p), "____") {
				t.Fatalf("CPath(%v) = %q has a run longer than its separator", p, CPath(p))
			}
		}
		for ti := range idents {
			for _, other := range []string{ti + "_", ti + "__Decoder", ti + "__New"} {
				if idents[other] {
					t.Fatalf("round %d: %q (derived from %q) is another path's TypeIdent", round, other, ti)
				}
			}
		}
	}
}

// Every name the validator accepts keeps Pascal free of "_" and in its fold,
// which is what TypeIdent's injectivity rests on.
func TestPascalStaysInItsChannel(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	for i := 0; i < 20000; i++ {
		name := randomName(r)
		if !NameRe.MatchString(name) {
			continue
		}
		p := Pascal(name)
		if strings.Contains(p, "_") || p == "" || p[0] < 'A' || p[0] > 'Z' || Fold(p) != Fold(name) {
			t.Fatalf("Pascal(%q) = %q", name, p)
		}
	}
}
