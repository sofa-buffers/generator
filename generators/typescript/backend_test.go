package typescript

import (
	"os"
	"strings"
	"testing"

	"github.com/sofa-buffers/generator/internal/analysis"
	"github.com/sofa-buffers/generator/internal/ir"
	"github.com/sofa-buffers/generator/internal/model"
	"github.com/sofa-buffers/generator/internal/parser"
)

func schema(t *testing.T, src string) *ir.Schema {
	t.Helper()
	doc, err := parser.Parse([]byte(src), "t.yaml")
	if err != nil {
		t.Fatal(err)
	}
	resolved, _ := doc.Resolve()
	if errs := parser.Validate(resolved); errs != nil {
		t.Fatalf("invalid: %v", errs)
	}
	s, err := model.Build(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := analysis.Analyze(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func genTS(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("../../examples/messages/example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	files, err := (&Backend{}).Generate(schema(t, string(b)), map[string]any{})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	for _, f := range files {
		if f.Path == "message.ts" {
			return string(f.Content)
		}
	}
	t.Fatal("no message.ts")
	return ""
}

func TestTSStructural(t *testing.T) {
	mod := genTS(t)
	for _, want := range []string{
		`import { OStream, Cursor } from "@sofa-buffers/corelib";`,
		"export class Myfirstmessage {",
		"marshal(os: OStream): void {",
		"static decode(bytes: Uint8Array): Myfirstmessage {",
		"return Myfirstmessage.decodeFrom(new Cursor(bytes));",
		"static decodeFrom(c: Cursor): Myfirstmessage {",
		"while (c.readHeader()) {",                                      // monomorphic pull loop
		"switch (c.id) {",                                               // one switch per type
		"default: c.skip(c.wire); break;",                               // forward-compat skip
		"o.somestruct = MyfirstmessageSomestruct.decodeFrom(c); break;", // nested message recursion
		`while (c.readHeader()) { const _id = c.id; while (arr.length <= _id) arr.push(""); arr[_id] = c.readString(); }`, // id-aware string-list sequence (MESSAGE_SPEC S2)
		"o.someu64 = c.readUnsigned() as bigint; break;",                                                                  // u64 -> bigint, number-first
		"os.writeSequenceBegin(", // nested framing (marshal unchanged)
		"export enum MyfirstmessageSomeenum {",
	} {
		if !strings.Contains(mod, want) {
			t.Errorf("message.ts missing %q", want)
		}
	}
	// The megamorphic push/visitor decode is gone: no _visitor()/ChunkAcc, no
	// per-field visitor callbacks, no `decode`/`Visitor` import.
	for _, gone := range []string{
		"_visitor()", "ChunkAcc", "type Visitor", "sequenceBegin(",
		"stringListVisitor", "unsigned(id: number, value: bigint)",
	} {
		if strings.Contains(mod, gone) {
			t.Errorf("message.ts should no longer emit %q (push/visitor decode removed)", gone)
		}
	}
	// Fast-encode marshal tidy-up: a leaf string list uses an indexed for (no
	// per-encode closure) rather than .forEach.
	if !strings.Contains(mod, "for (let _i0 = 0; _i0 < this.somestringarray.length; _i0++) {") {
		t.Error("message.ts missing indexed-for string-list marshal (fast-encode)")
	}
}

func TestTSDeterministic(t *testing.T) {
	if genTS(t) != genTS(t) {
		t.Fatal("TS generation not deterministic")
	}
}
