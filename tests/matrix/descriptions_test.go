package matrix

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/sofa-buffers/generator/internal/generator"
)

// TestDescriptionsBecomeDocComments verifies that every backend renders the
// message summary and field descriptions from descriptions.yaml as doc comments
// in the generated code, preserving the UTF-8 text byte-for-byte. It checks two
// things per backend: (1) every description/summary/unit fragment appears
// verbatim (UTF-8 pass-through), and (2) each single-line anchor fragment sits on
// a comment line (a comment marker precedes it) — i.e. it is a comment, not code.
// The exact comment syntax (Doxygen ///<, rustdoc ///, godoc //, Python #:/""" ,
// TSDoc/Javadoc /** */, C# /// <summary>) is language-idiomatic so doc generators
// pick it up; per-language compilation of these doc comments is gated by the
// per-language conformance jobs (example.yaml carries descriptions too).
func TestDescriptionsBecomeDocComments(t *testing.T) {
	s, err := buildIR(t, filepath.Join("testdata", "descriptions.yaml"))
	if err != nil {
		t.Fatalf("descriptions.yaml should validate: %v", err)
	}

	// Field descriptions (single-line) + the summary's first line: each must
	// appear AND sit on a comment line in every language.
	commentAnchors := []string{
		"Telemetrie-Paket", // message summary (first line)
		"Außentemperatur in Grad Celsius",
		"速度 — vitesse du véhicule",
		"Δv: Geschwindigkeitsänderung",
		"π-Verhältnis ≈ 3.14159",
		"Bezeichnung des Pakets",
	}
	// Fragments that must appear verbatim (UTF-8 fidelity) but may live on a
	// block-comment / docstring continuation line without a per-line marker.
	presenceOnly := []string{
		"Enthält 温度", "🚗", "(naïve, café, façade)",
		"(unit: °C)", "(unit: km/h)",
	}

	for _, lang := range generator.Registered() {
		t.Run(lang, func(t *testing.T) {
			b, _ := generator.Lookup(lang)
			files, err := b.Generate(s, map[string]any{})
			if err != nil {
				t.Fatalf("generate: %v", err)
			}
			var all strings.Builder
			for _, f := range files {
				all.WriteString(string(f.Content))
				all.WriteByte('\n')
			}
			out := all.String()

			if lang == "docs" {
				// The docs backend renders descriptions as page CONTENT, not code
				// comments: no comment-line requirement, and units get their own
				// table column instead of an "(unit: …)" suffix. Only UTF-8
				// fidelity is checked here.
				for _, frag := range append([]string{"Enthält 温度", "🚗", "(naïve, café, façade)", "°C", "km/h"}, commentAnchors...) {
					if !strings.Contains(out, frag) {
						t.Errorf("missing %q in generated docs (UTF-8 not preserved?)", frag)
					}
				}
				return
			}

			for _, frag := range presenceOnly {
				if !strings.Contains(out, frag) {
					t.Errorf("missing %q in generated output (UTF-8 not preserved?)", frag)
				}
			}
			for _, frag := range commentAnchors {
				line, ok := lineContaining(out, frag)
				if !ok {
					t.Errorf("missing %q in generated output", frag)
					continue
				}
				if !onCommentLine(line, frag) {
					t.Errorf("%q is not on a comment line: %q", frag, strings.TrimSpace(line))
				}
			}
		})
	}
}

// lineContaining returns the first line of s that contains sub.
func lineContaining(s, sub string) (string, bool) {
	for _, line := range strings.Split(s, "\n") {
		if strings.Contains(line, sub) {
			return line, true
		}
	}
	return "", false
}

// onCommentLine reports whether sub sits inside a comment on this line: some
// comment marker must appear before sub. Covers leading (// /// #: """ * /**),
// trailing-member (///< /**< // ), and block-continuation (" * ") forms across
// all target languages.
func onCommentLine(line, sub string) bool {
	pos := strings.Index(line, sub)
	if pos < 0 {
		return false
	}
	prefix := line[:pos]
	for _, marker := range []string{"//", "/*", "#", `"""`, "*"} {
		if strings.Contains(prefix, marker) {
			return true
		}
	}
	return false
}
