// A decoded message OWNS its bytes (CORELIB_PLAN §6.7 / §6.7.1, generator#412).
//
// Replaces the generated JSON harness as src/main.zig in a project built from
// examples/messages/example.yaml (see run.sh), so it links the real generated
// code against the real corelib.
//
// The property: no destination may keep a window into the bytes it was decoded
// from. §6.0 fixes it for `feed` -- a chunk is borrowed only for the duration
// of the call -- and §6.7.1 extends it to the one-shot path, which gets no
// exemption: "decode(buffer) copies too", because a message whose lifetime
// depends on which entry point produced it is exactly the divergence §6.7 ends.
//
// The oracle is destructive, not comparative: decode, then OVERWRITE the buffer
// the bytes came from, then re-encode and diff. Comparing two decoders against
// each other cannot see this -- both would read the same live buffer.
//
// This file caught a real violation. Before generator#412 the zig backend set
// its visitor's `own` flag only in decoder(); decode() left it false and handed
// every payload straight back as a slice of `data`, so scribbling `data` was
// visible in `somestring`, `someblob` and every string/blob array element of a
// message that had already been decoded.
//
// KNOWN REACH -- do not read a pass as "every field is copied":
//
//   * The legs that can fail are the ones whose Zig storage is a SLICE the
//     corelib could hand over unchanged: `somestring`, `someblob`, each element
//     of `somestringarray`/`someblobarray`, the string inside the nested struct,
//     the union's string option, and the `key` of each dynamic map row.
//   * A counted native array (`someuintarray`, `somefloatarray`, ...) is
//     `sofab.FixedArray(T, N)` -- storage inside the message itself, filled
//     element by element from decoded scalars. It cannot alias, so dropping a
//     copy there is not a thing this file can observe.
//   * The scribble reaches only memory the CALLER owns. A corelib that handed
//     out a window into its own reused carry buffer would survive it; that
//     hazard is what tests/conformance/zig/stream_check.zig pins, by feeding
//     one byte at a time so two payloads pass through that buffer in turn.
//
// The scribble byte is 0x41 ('A'), deliberately: with 0xff an aliased string
// makes the re-encode fail UTF-8 validation instead, and the oracle would
// become an error code rather than a byte comparison. A failed re-encode is
// still reported as a failure here -- it is never propagated with `try`.
//
// Built with --release=fast, so an aliasing read never traps: safety checks are
// off and the arena keeps freed pages mapped. Only comparing VALUES catches it.

const std = @import("std");
const message = @import("message.zig");

const M = message.Myfirstmessage;

// Every chunk size is swept, and the sweep must include one at least as large
// as the longest payload: a payload SPLIT across chunks is reassembled into the
// corelib's accumulator and copied out of it whether or not the destination
// wanted to alias, so small chunks alone cannot reach the aliasing branch at
// all. 4096 covers the whole message; 1 covers the other extreme.
const chunk_sizes = [_]usize{ 1, 7, 16, 64, 4096 };

var failures: usize = 0;

fn fail(comptime fmt: []const u8, args: anytype) void {
    std.debug.print("FAIL: " ++ fmt ++ "\n", args);
    failures += 1;
}

/// A message filling every aliasing-capable field kind: string, blob,
/// array<string>, array<blob>, a string nested in a struct, a string in a
/// union, a string in a dynamic wrapper-array row -- plus the native arrays,
/// which are in the sample so the wire has them, not because they can alias.
fn sample() M {
    var m: M = .{};
    m.somestring = "hello world payload";
    m.someblob = &.{ 1, 2, 3, 4, 5 };
    m.somestringarray = &.{ "aaa", "bbbb", "ccccc" };
    m.someblobarray = &.{ &.{ 9, 9 }, &.{8} };
    m.someuintarray = .init(&.{ 9, 8, 7, 6 });
    m.somefloatarray = .init(&.{ 1.5, -2.5, 3.5 });
    m.somestruct.nestedstring = "nested payload";
    m.someunion.option2 = "union payload";
    m.somemap = &.{
        .{ .key = "first key", .value = 1 },
        .{ .key = "second key", .value = 2 },
    };
    return m;
}

/// Re-encode `got` and diff it against `want`. A re-encode that fails is a
/// failure of this check too: encode UTF-8-validates, so a scribbled string
/// destination can come back as an error rather than as different bytes.
fn mustMatch(alloc: std.mem.Allocator, what: []const u8, want: []const u8, got: *const M) void {
    const re = got.encode(alloc) catch |e| {
        fail("{s}: re-encoding the decoded message failed with {any} -- a destination aliased its input", .{ what, e });
        std.debug.print("  somestring = \"{s}\"\n  someblob   = {x}\n", .{ got.somestring, got.someblob });
        return;
    };
    if (!std.mem.eql(u8, want, re)) {
        fail("{s}: a decoded field aliased the buffer it was decoded from", .{what});
        std.debug.print("  want {x}\n  got  {x}\n", .{ want, re });
        std.debug.print("  somestring = \"{s}\"\n  someblob   = {x}\n", .{ got.somestring, got.someblob });
        for (got.somestringarray, 0..) |s, i| std.debug.print("  somestringarray[{d}] = \"{s}\"\n", .{ i, s });
        for (got.someblobarray, 0..) |b, i| std.debug.print("  someblobarray[{d}]   = {x}\n", .{ i, b });
    }
}

pub fn main() !void {
    // An arena: the generated decoder allocates array storage (and, since
    // generator#412, every payload) from the allocator it is handed.
    var arena = std.heap.ArenaAllocator.init(std.heap.page_allocator);
    defer arena.deinit();
    const alloc = arena.allocator();

    const m = sample();
    const want = try m.encode(alloc);

    // ---- 1. one-shot decode(), out of a MUTABLE buffer -------------------
    // §6.7.1: `data` may be reused the moment decode() returns, so the scribble
    // below is a legitimate thing for a caller to do.
    {
        const wire = try alloc.dupe(u8, want);
        var got = M.decode(alloc, wire) catch |e| {
            fail("one-shot decode() failed with {any}", .{e});
            return failExit();
        };
        @memset(wire, 0x41);
        mustMatch(alloc, "one-shot decode()", want, &got);
    }

    // ---- 2. streaming decoder()/feed(), out of ONE reusable scratch ------
    // The sharper case: a real caller refills the same buffer, so every chunk
    // is copied into one scratch that is overwritten the instant feed returns.
    for (chunk_sizes) |size| {
        const scratch = try alloc.alloc(u8, size);
        var out: M = .{};
        var d = M.decoder(&out, alloc);
        var i: usize = 0;
        while (i < want.len) {
            const n = @min(size, want.len - i);
            @memcpy(scratch[0..n], want[i .. i + n]);
            _ = d.feed(scratch[0..n]) catch |e| {
                fail("streaming feed(chunk={d}) failed with {any}", .{ size, e });
                break;
            };
            @memset(scratch, 0x41);
            i += n;
        }
        d.finish() catch |e| {
            fail("streaming finish(chunk={d}) failed with {any}", .{ size, e });
            continue;
        };
        var buf: [64]u8 = undefined;
        mustMatch(alloc, std.fmt.bufPrint(&buf, "streaming feed(chunk={d})", .{size}) catch unreachable, want, &out);
    }

    if (failures > 0) return failExit();
    std.debug.print("decoded message owns its bytes: one-shot + {d} chunk sizes\n", .{chunk_sizes.len});
}

fn failExit() noreturn {
    std.process.exit(1);
}
