package matrix

import (
	"path/filepath"
	"regexp"
	"testing"

	"github.com/sofa-buffers/generator/internal/generator"
)

// streamEncodeArm matches the verb QUOTED, which is how every backend spells
// its dispatch arm (`mode == "streamencode"`, `strcmp(mode, "streamencode")`,
// `'streamencode'`, `"streamencode" ->`). The usage line and comments name the
// verb bare, so they cannot satisfy it.
var streamEncodeArm = regexp.MustCompile(`["']streamencode["']`)

// TestEveryHarnessEmitsStreamEncode pins the WINDOWED encode surface in every
// backend's project harness (generator#653).
//
// An unbounded one-shot `encode()` drains a fixed scratch into its result, and
// every fixture the suites encode is smaller than that scratch, so the drain
// ran at most once and a bug in it -- wrong slice bounds, a buffer reused
// before it was copied, a lost tail at flush -- passed everything.
//
// `streamencode <Message> <window>` is the encode counterpart of
// `streamdecode`: it takes the same JSON on stdin that `encode` takes, writes
// the message through a sink with an output buffer of `window` bytes (0 = the
// generated one-shot or streaming method with its own scratch) and prints the
// bytes. tests/conformance/lib/check_stream_encode.py then encodes a message of
// tens of KB at several windows and requires every result to equal the one-shot
// bytes.
//
// The sweep is over EMITTED TEXT and over every registered backend rather than
// per-backend, so a NEW target cannot land without the mode: its harness is the
// thing the conformance runner drives, and a harness missing this mode leaves
// the drain path unreached.
func TestEveryHarnessEmitsStreamEncode(t *testing.T) {
	// Two definitions, as in the streamdecode sweep: the heapless C target cannot
	// size an unbounded field, so scalars.yaml is the fully bounded one that
	// reaches it, and `checked` asserts no backend fell through both.
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
			// The docs target renders HTML; it has no harness and no encode surface.
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
				if streamEncodeArm.Match(f.Content) {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("%s (%s): project mode emits no quoted \"streamencode\" dispatch arm in its harness — "+
					"the one-shot encode's drain path then never sees a message larger "+
					"than its scratch, and no sink is driven with a small window",
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
