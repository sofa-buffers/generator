package matrix

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/sofa-buffers/generator/internal/generator"
)

// TestEveryHarnessEmitsStreamDecode pins the CHUNKED decode surface in every
// backend's project harness (generator#456).
//
// `decode` is the one-shot, whole-buffer entry point everywhere. It never
// suspends, so nothing in it has to carry parse state across a boundary, while
// the chunked path has to resume mid-varint, mid-payload, mid-array-element and
// mid-sequence. Inside a SKIPPED field that is exactly where a resync bug hides:
// the skip's own length computation and its progress counter both have to
// survive the boundary, and the anchor behind the skip is the only thing that
// notices when they don't.
//
// `streamdecode` is that surface, shaped so one driver can reach it on every
// target: it takes the same raw wire bytes on stdin that `decode` takes, feeds
// them ONE BYTE PER FEED, and prints exactly what `decode` prints.
// tests/conformance/lib/check_vectors_decode.py then replays the shared vectors
// -- whose skip/matrix group puts every wire type in the skipped position --
// through both surfaces against identical expectations, so any difference
// between them is the finding.
//
// The sweep is over EMITTED TEXT and over every registered backend rather than
// per-backend, so a NEW target cannot land without the mode: its harness is the
// thing the conformance runner drives, and a harness missing this mode silently
// halves what the vectors reach.
func TestEveryHarnessEmitsStreamDecode(t *testing.T) {
	// Two definitions, because the heapless C target cannot size an unbounded
	// field and example.yaml has several: scalars.yaml is the fully-bounded one
	// that reaches it. `checked` then asserts no backend fell through BOTH -- a
	// silently narrowed sweep passes while testing less than it claims.
	defs := []string{
		filepath.Join("..", "..", "examples", "messages", "example.yaml"),
		filepath.Join("corpus", "defs", "scalars.yaml"),
	}
	checked := map[string]bool{}
	for _, def := range defs {
		s, err := buildIR(t, def)
		if err != nil {
			t.Fatalf("%s should validate: %v", def, err)
		}
		for _, lang := range generator.Registered() {
			// The docs target renders HTML; it has no harness and no decode surface.
			if lang == "docs" {
				continue
			}
			if fixedOnlyTarget(lang) && hasUnboundedField(s) {
				continue // heapless target cannot size an unbounded field
			}
			b, ok := generator.Lookup(lang)
			if !ok {
				t.Fatalf("%s is registered but not resolvable", lang)
			}
			files, err := b.Generate(s, map[string]any{"emit": "project", "timestamp": false})
			if err != nil {
				t.Fatalf("%s (%s): generate: %v", lang, filepath.Base(def), err)
			}
			checked[lang] = true
			found := false
			for _, f := range files {
				if strings.Contains(string(f.Content), "streamdecode") {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("%s (%s): project mode emits no `streamdecode` harness mode — "+
					"the shared vectors then reach the one-shot decode surface only, "+
					"and no skipped field is ever cut by a chunk boundary",
					lang, filepath.Base(def))
			}
		}
	}
	for _, lang := range generator.Registered() {
		if lang == "docs" {
			continue
		}
		if !checked[lang] {
			t.Errorf("%s was never reached by this sweep — add a definition it can generate", lang)
		}
	}
}

// chunkSizeBackends are the targets whose `streamdecode` also takes a CHUNK SIZE
// as its third argument, and the exact parse each emits (generator#413).
//
// A single split width only shows that the decoder resumed once. Sweeping
// several is what turns that into "where the cut lands does not matter", which
// is the property CORELIB_PLAN §5.2/§6.0 claims and the one a resume bug breaks:
// a half-read varint, a payload accumulator that is not carried, a scope stack
// that unwinds one level too far. `0` feeds the whole message in one call.
//
// Four entries, not eleven, and that is deliberate — generator#413 scopes the
// sweep to the suites that had NO chunked check at all. The other seven already
// run one of their own (go/zig/typescript compare verdicts across splits;
// rust/cpp/kotlin/c feed a streaming round-trip), and their `streamdecode` keeps
// the one-byte default. Adding a target here is what wires it into
// tests/conformance/lib/check_chunk_invariance.py.
var chunkSizeBackends = map[string]string{
	"java":   "int csz = args.length > 2 ? Integer.parseInt(args[2]) : 1;",
	"csharp": "var csz = args.Length > 2 ? int.Parse(args[2]) : 1;",
	"python": "csz = int(sys.argv[3]) if len(sys.argv) > 3 else 1",
	"dart":   "final csz = args.length > 2 ? int.parse(args[2]) : 1;",
}

func TestChunkSizeArgumentWhereTheSweepRuns(t *testing.T) {
	s, err := buildIR(t, filepath.Join("..", "..", "examples", "messages", "example.yaml"))
	if err != nil {
		t.Fatalf("example.yaml should validate: %v", err)
	}
	for lang, want := range chunkSizeBackends {
		b, ok := generator.Lookup(lang)
		if !ok {
			t.Errorf("%s is named here but not registered", lang)
			continue
		}
		files, err := b.Generate(s, map[string]any{"emit": "project", "timestamp": false})
		if err != nil {
			t.Fatalf("%s: generate: %v", lang, err)
		}
		found := false
		for _, f := range files {
			if strings.Contains(string(f.Content), want) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("%s: harness must parse a chunk-size argument (%q) — without it "+
				"check_chunk_invariance.py can only ever feed one split width", lang, want)
		}
	}
}

// TestNoHarnessFormatArtifact catches a `%s` that was meant for the GENERATED
// text but was eaten by the emitter's own Printf: Go writes the leftover verb
// back as `%!s(MISSING)`, which compiles in some targets and merely prints
// garbage in others. The C backend already guarded its own harness this way; the
// streamdecode branches added the same shape to ten more (generator#456).
func TestNoHarnessFormatArtifact(t *testing.T) {
	s, err := buildIR(t, filepath.Join("..", "..", "examples", "messages", "example.yaml"))
	if err != nil {
		t.Fatalf("example.yaml should validate: %v", err)
	}
	for _, lang := range generator.Registered() {
		if fixedOnlyTarget(lang) && hasUnboundedField(s) {
			continue
		}
		b, ok := generator.Lookup(lang)
		if !ok {
			continue
		}
		files, err := b.Generate(s, map[string]any{"emit": "project", "timestamp": false})
		if err != nil {
			continue
		}
		for _, f := range files {
			if strings.Contains(string(f.Content), "%!") {
				t.Errorf("%s: %s carries a Go fmt artifact (%%!...(MISSING)) — "+
					"a literal %% in emitted text must be written %%%%", lang, f.Path)
			}
		}
	}
}
