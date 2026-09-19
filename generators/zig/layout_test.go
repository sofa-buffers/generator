package zig

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// layoutCases are one-line shapes the emitters produce, each paired with what
// `zig fmt` 0.16 makes of it (TestLayoutMatchesZigFmt re-derives the right-hand
// side from zig itself wherever zig is installed).
var layoutCases = []struct{ name, in, want string }{
	{"multi-statement block",
		"    if (self.askip > 0) { self.askip -= 1; return; }",
		"    if (self.askip > 0) {\n        self.askip -= 1;\n        return;\n    }"},
	{"catch block inside parens",
		"    const p = (self.acc.push(a) catch { self.inv = true; return null; }) orelse return null;",
		"    const p = (self.acc.push(a) catch {\n        self.inv = true;\n        return null;\n    }) orelse return null;"},
	{"capture block, single statement",
		"    dec.finish() catch |fe| { fin = @errorName(fe); };",
		"    dec.finish() catch |fe| {\n        fin = @errorName(fe);\n    };"},
	{"if / else if / else chain in a prong",
		"        0 => if (id >= 3) { self.inv = true; } else if (total > 8) { self.inv = true; } else { const c = t() orelse return; self.m.s = c; },",
		"        0 => if (id >= 3) {\n            self.inv = true;\n        } else if (total > 8) {\n            self.inv = true;\n        } else {\n            const c = t() orelse return;\n            self.m.s = c;\n        },"},
	{"prong holding a block that holds a nested if",
		"        .root_x => { if (self.afill != 0) { self.afill -= 1; if (value > 255) { self.inv = true; return; } self.m.x.push(@intCast(value), &self.inv); } },",
		"        .root_x => {\n            if (self.afill != 0) {\n                self.afill -= 1;\n                if (value > 255) {\n                    self.inv = true;\n                    return;\n                }\n                self.m.x.push(@intCast(value), &self.inv);\n            }\n        },"},
	{"statement after a block",
		"    { if (count > 3) { self.inv = true; return; } self.m.b.clear(); }",
		"    {\n        if (count > 3) {\n            self.inv = true;\n            return;\n        }\n        self.m.b.clear();\n    }"},
	{"switch without a trailing comma, labeled block prong",
		"    for (a0.items, 0..) |it0, k0| s0[k0] = switch (it0) { .array => |a1| blk1: { const s1 = f(a1); break :blk1 s1; }, else => &.{} };",
		"    for (a0.items, 0..) |it0, k0| s0[k0] = switch (it0) {\n        .array => |a1| blk1: {\n            const s1 = f(a1);\n            break :blk1 s1;\n        },\n        else => &.{},\n    };"},
	{"switch with a trailing comma and a multi-item prong",
		"        .root => switch (id) { 0, 1 => if (total > 5) return error.X, else => {}, },",
		"        .root => switch (id) {\n            0, 1 => if (total > 5) return error.X,\n            else => {},\n        },"},
	{"braces and semicolons inside literals and comments stay put",
		`    if (x) { s = "{ a; }"; c = '{'; } // { b; }`,
		"    if (x) {\n        s = \"{ a; }\";\n        c = '{';\n    } // { b; }"},
	{"initializers and containers are left alone",
		"    const v: T = .{ .m = &m, .a = [_]u8{b}, .k = enum(u8) { a, b }.a, .s = struct { x: u8 }{ .x = 1 } };",
		"    const v: T = .{ .m = &m, .a = [_]u8{b}, .k = enum(u8) { a, b }.a, .s = struct { x: u8 }{ .x = 1 } };"},
	{"empty blocks are left alone",
		"    if (x) {} else {}",
		"    if (x) {} else {}"},
	{"comment line",
		"    // if (x) { a; b; }",
		"    // if (x) { a; b; }"},
}

func TestLayoutZig(t *testing.T) {
	for _, c := range layoutCases {
		if got := layoutZig(c.in); got != c.want {
			t.Errorf("%s:\n got:\n%s\nwant:\n%s", c.name, got, c.want)
		}
		// Only whitespace may move -- plus the comma zig fmt gives a switch's
		// last prong: the token stream is otherwise unchanged.
		if a, b := tokensOf(c.in), tokensOf(layoutZig(c.in)); a != b {
			t.Errorf("%s: layout changed a token", c.name)
		}
	}
}

func tokensOf(s string) string {
	return strings.ReplaceAll(strings.Join(strings.Fields(s), ""), ",}", "}")
}

// TestLayoutMatchesZigFmt holds the expectations above to zig fmt itself, so
// they cannot encode a guess about its rules. A case is wrapped in a function
// body, and a switch prong (indented two levels) in a switch inside one.
func TestLayoutMatchesZigFmt(t *testing.T) {
	zig, err := exec.LookPath("zig")
	if err != nil {
		t.Skip("zig not installed; tests/conformance/zig/run.sh holds generated code to zig fmt")
	}
	for _, c := range layoutCases {
		src := "fn f() void {\n" + c.want + "\n}\n"
		if strings.HasPrefix(c.in, "        ") {
			src = "fn f() void {\n    switch (q) {\n" + c.want + "\n    }\n}\n"
		}
		p := filepath.Join(t.TempDir(), "case.zig")
		if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
		if out, err := exec.Command(zig, "fmt", "--check", p).CombinedOutput(); err != nil {
			t.Errorf("%s: expectation is not zig fmt output (%v):\n%s%s", c.name, err, out, src)
		}
	}
}

// The pass is a fixed point on real output, and it moves nothing but
// whitespace: laying out an already laid-out file changes nothing, and the
// example project's files hold the same tokens either way.
func TestLayoutZigIsAFixedPointOnGeneratedFiles(t *testing.T) {
	for path, src := range exampleFiles(t, map[string]any{"emit": "project"}) {
		if !strings.HasSuffix(path, ".zig") {
			continue
		}
		if again := layoutZig(src); again != src {
			t.Errorf("%s: emitted file is not in layout form (layoutZig changed it again)", path)
		}
		if flat(src) != flat(layoutZig(src)) {
			t.Errorf("%s: layout changed a token", path)
		}
	}
}
