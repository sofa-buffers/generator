package typescript

import (
	"strings"
	"testing"
)

// emptyPayloadSrc: a message whose payload declares no fields. A legal schema --
// MESSAGE_SPEC §2 makes the all-default message the empty byte string, so a
// field-less message is the ping -- and the one schema shape this backend could
// not emit at all (generator#738).
const emptyPayloadSrc = "version: 1\nmessages:\n  ping:\n    payload: {}\n"

// TestTSEmptyPayloadEmitsItsDecodeSurface: the visitor and the incremental
// Decoder are emitted for every message, a field-less one included.
//
// They used to be skipped when NO type in the schema had a field -- a guard kept
// from #160, where its only job was to leave an unused `WireType` import out,
// which the rendered-text import scan now decides on its own. By the time the
// flat-visitor surface landed, that guard was suppressing the very visitor and
// Decoder that decode() and decoder() name, so the module did not compile.
func TestTSEmptyPayloadEmitsItsDecodeSurface(t *testing.T) {
	mod := genModule(t, schema(t, emptyPayloadSrc), map[string]any{})
	for _, want := range []string{
		"class _Ping__Visitor implements Visitor {",
		"export class Ping__Decoder {",
		"_decode(bytes, new _Ping__Visitor(o, new PayloadAcc()));",
	} {
		if !strings.Contains(mod, want) {
			t.Errorf("module missing %q:\n%s", want, mod)
		}
	}
}

// TestTSEmptyPayloadHarnessDoesNotPanic: the bench sink folds one cheap scalar of
// the decoded message, and a field-less one has none -- benchSinkField indexed
// fields[0] and took the whole generator down with it.
func TestTSEmptyPayloadHarnessDoesNotPanic(t *testing.T) {
	files, err := (&Backend{}).Generate(schema(t, emptyPayloadSrc), map[string]any{"emit": "project"})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	var harness string
	for _, f := range files {
		if f.Path == "harness.ts" {
			harness = string(f.Content)
		}
	}
	if harness == "" {
		t.Fatal("no harness.ts")
	}
	// The object itself is the only thing left to read, and reading it is what
	// keeps the decode from being elided -- a constant would measure nothing.
	for _, want := range []string{
		"sink ^= Number(M.Ping.decode(wire) !== undefined);",
		"sink ^= Number(_d.finish() !== undefined);",
	} {
		if !strings.Contains(harness, want) {
			t.Errorf("harness missing %q:\n%s", want, harness)
		}
	}
}
