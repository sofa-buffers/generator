package zig

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// namesFiles generates the shared name-collision schema
// (tests/conformance/lib/names.yaml, ARCHITECTURE §8 "Naming") as a project.
func namesFiles(t *testing.T) (msg, harness string) {
	t.Helper()
	src, err := os.ReadFile("../../tests/conformance/lib/names.yaml")
	if err != nil {
		t.Fatal(err)
	}
	files, err := generateYAML(t, string(src), map[string]any{"emit": "project"})
	if err != nil {
		t.Fatalf("names.yaml must generate: %v", err)
	}
	for _, f := range files {
		switch f.Path {
		case "src/message.zig":
			msg = string(f.Content)
		case "src/main.zig":
			harness = string(f.Content)
		}
	}
	return msg, harness
}

var (
	// a declaration at column 0: `pub const X =`, `const X:`, `fn x(`, `var x:`
	topDecl = regexp.MustCompile(`(?m)^(?:pub |export )?(?:const|var|fn) ([A-Za-z_][A-Za-z0-9_]*)\b`)
	// a member of a top-level container, at 4 spaces: a declaration or a field
	memberDecl  = regexp.MustCompile(`(?m)^    (?:pub )?(?:const|var|fn) ([A-Za-z_][A-Za-z0-9_]*)\b`)
	memberField = regexp.MustCompile(`(?m)^    (@"[^"]+"|[A-Za-z_][A-Za-z0-9_]*): `)
)

// containers splits a Zig file into its top-level declarations, each up to the
// next one.
func containers(src string) []string {
	idx := topDecl.FindAllStringIndex(src, -1)
	out := make([]string, len(idx))
	for i, p := range idx {
		end := len(src)
		if i+1 < len(idx) {
			end = idx[i+1][0]
		}
		out[i] = src[p[0]:end]
	}
	return out
}

// TestNamesSchemaDeclaresEachNameOnce generates names.yaml and checks, without a
// toolchain, what Zig would reject: a file-scope name declared twice (in
// message.zig and in the harness), a member declared twice in one container
// (fields and declarations share one namespace), and a member declaration that
// is also a file-scope name -- a reference to it from inside its container is
// then "ambiguous" in Zig, even though the two declarations do not clash.
func TestNamesSchemaDeclaresEachNameOnce(t *testing.T) {
	msg, harness := namesFiles(t)
	for file, src := range map[string]string{"message.zig": msg, "main.zig": harness} {
		top := map[string]bool{}
		for _, m := range topDecl.FindAllStringSubmatch(src, -1) {
			if top[m[1]] {
				t.Errorf("%s: %s is declared twice at file scope", file, m[1])
			}
			top[m[1]] = true
		}
		for _, c := range containers(src) {
			head := strings.SplitN(c, "\n", 2)[0]
			seen := map[string]bool{}
			for _, re := range []*regexp.Regexp{memberDecl, memberField} {
				for _, m := range re.FindAllStringSubmatch(c, -1) {
					n := strings.TrimSuffix(strings.TrimPrefix(m[1], `@"`), `"`)
					if seen[n] {
						t.Errorf("%s: %q declares the member %s twice", file, head, n)
					}
					seen[n] = true
				}
			}
			for _, m := range memberDecl.FindAllStringSubmatch(c, -1) {
				if top[m[1]] {
					t.Errorf("%s: %q declares %s, which is also a file-scope name", file, head, m[1])
				}
			}
		}
	}

	// The spellings of each channel, one example each.
	for _, want := range []string{
		"pub const M = struct {",                // message
		"pub const M_A = struct {",              // inline struct m.a
		"pub const MA = struct {",               // message m_a
		"pub const M_Arr = struct {",            // array element: the field's own path
		"pub const Color = struct {",            // $defs enum, no category prefix
		"pub const M_Visitor = struct {",        // inline enum m.visitor
		"pub const Shape__DefaultPt = union(",   // split variant
		"pub const ShapeDefaultPt = struct {",   // message shape_default_pt
		"pub const ShapeDefault_Pt = struct {",  // the inline option shape_default.pt
		"const _M__Visitor = struct {",          // private visitor
		"pub const Decoder_ = struct {",         // escape: message decoder
		"pub const DecodeError_ = struct {",     // escape: message decode_error
		"pub const Std = struct {",              // std is lower-case, no clash
		"pub fn decoder(out: *Decoder_, alloc:", // role built on the escaped type
		"var v: _Decoder__Visitor = .{",         // private built from the UNESCAPED type
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("message.zig: missing %q", want)
		}
	}
	for _, want := range []string{
		"fn toJson_Decoder(o: *const message.Decoder_,",
		"fn fromJson_Shape__DefaultPt(",
		"export fn run_encode_decode_error() void {",
	} {
		if !strings.Contains(harness, want) {
			t.Errorf("main.zig: missing %q", want)
		}
	}
}
