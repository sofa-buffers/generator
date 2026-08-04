// Chunk-invariance check for the incremental decoder (decoder()/feed()).
//
// Replaces the generated JSON harness as src/main.zig in a project built from
// the `probe` schema below (see run.sh), so it links the real generated code
// against the real corelib.
//
// The property: decoding is INDEPENDENT of how the input is split. The same
// bytes must produce the same values whether they arrive in one chunk or one
// byte at a time. Every existing zig gate hands each message over whole, so
// nothing here was reachable before -- a payload delivered whole is never
// reassembled, and reassembly is where the split path differs.
//
// The case that motivated this file (generator#293, Crucible F-0058): the
// generated visitor accumulated a split payload in ONE buffer on the visitor
// and handed the destination a view into it. A second split payload then
// cleared and re-appended that buffer, so the element stored first aliased the
// element stored last; growing past the old capacity reallocated and left the
// earlier slice pointing at the old block, with its own stale length. Two or
// more split payloads are needed to see it, which is why byte-at-a-time feeding
// finds it immediately and a two-way split usually does not.

const std = @import("std");
const message = @import("message.zig");

const Probe = message.Probe;

fn fail(comptime fmt: []const u8, args: anytype) noreturn {
    std.debug.print("FAIL: " ++ fmt ++ "\n", args);
    std.process.exit(1);
}

/// Feed `wire` through decoder()/feed() `n` bytes at a time.
///
/// The chunks are slices of `wire`, which outlives the returned message -- the
/// borrow contract the generated decoder documents (a payload arriving whole
/// inside one chunk is borrowed from it).
fn decodeChunked(alloc: std.mem.Allocator, wire: []const u8, n: usize) !Probe {
    var out: Probe = .{};
    var d = Probe.decoder(&out, alloc);
    var i: usize = 0;
    while (i < wire.len) {
        const end = @min(i + n, wire.len);
        _ = try d.feed(wire[i..end]);
        i = end;
    }
    try d.finish();
    return out;
}

fn expectElems(got: []const []const u8, want: []const []const u8, label: []const u8, n: usize) void {
    if (got.len != want.len) {
        fail("{s}: chunk size {d} produced {d} elements, want {d}", .{ label, n, got.len, want.len });
    }
    for (got, want, 0..) |g, w, idx| {
        // Length is checked on its own: a slice rebased by a reallocation keeps
        // the length it was stored with, so it can read PAST the live bytes --
        // a wrong length is the sharper symptom and deserves its own message.
        if (g.len != w.len) {
            fail("{s}: chunk size {d}, element {d} has length {d}, want {d} (stale length past the live payload?)", .{ label, n, idx, g.len, w.len });
        }
        if (!std.mem.eql(u8, g, w)) {
            fail("{s}: chunk size {d}, element {d} = \"{s}\", want \"{s}\" (aliased onto another element?)", .{ label, n, idx, g, w });
        }
    }
}

/// Run one vector at EVERY chunk size from 1 byte up to the whole message, and
/// require the same values from all of them plus from the contiguous decode().
fn checkAllChunkSizes(
    alloc: std.mem.Allocator,
    label: []const u8,
    wire: []const u8,
    comptime field: []const u8,
    want: []const []const u8,
) !void {
    const whole = try Probe.decode(alloc, wire);
    expectElems(@field(whole, field), want, label, wire.len);

    var n: usize = 1;
    while (n <= wire.len) : (n += 1) {
        const m = try decodeChunked(alloc, wire, n);
        expectElems(@field(m, field), want, label, n);
    }
    std.debug.print("==> {s}: chunk-invariant at all {d} chunk sizes\n", .{ label, wire.len });
}

pub fn main() !void {
    // An arena is the allocator the generated decoder documents ("storage comes
    // from `alloc` -- an arena frees everything at once"): nothing in the
    // generated code frees an individual payload, so a leak-checking allocator
    // would report by design. The assertions below are on VALUES and hold
    // whatever the allocator does, which is what makes them a reliable gate --
    // under an arena a rebased read returns stale-but-mapped bytes rather than
    // trapping, so only comparing the content catches it.
    var arena = std.heap.ArenaAllocator.init(std.heap.page_allocator);
    defer arena.deinit();
    const alloc = arena.allocator();

    // string_array (id 200) = ["ab", "cd"].
    //   c6 0c  sequence begin, id 200
    //   02     element id 0, string      12  fixlen: string, length 2   "ab"
    //   0a     element id 1, string      12  fixlen: string, length 2   "cd"
    //   07     sequence end
    try checkAllChunkSizes(
        alloc,
        "string_array [\"ab\",\"cd\"]",
        &.{ 0xc6, 0x0c, 0x02, 0x12, 'a', 'b', 0x0a, 0x12, 'c', 'd', 0x07 },
        "string_array",
        &.{ "ab", "cd" },
    );

    // blob_array (id 201), same shape through the same reassembly helper. Kept
    // as its own vector because the store side differs: a blob is not UTF-8
    // validated, so it reaches setElem by a different path than a string.
    try checkAllChunkSizes(
        alloc,
        "blob_array [\"ab\",\"cd\"]",
        &.{ 0xce, 0x0c, 0x02, 0x13, 'a', 'b', 0x0a, 0x13, 'c', 'd', 0x07 },
        "blob_array",
        &.{ "ab", "cd" },
    );

    // Growth case: a 2-byte element followed by a 60-byte one. The second
    // payload pushes the accumulator past the capacity the first left behind,
    // so assembling in place both overwrites element 0 AND reallocates out from
    // under it -- element 0 then carried 60-byte-buffer content under its own
    // 2-byte length, or pointed into a freed block under a releasing allocator.
    // The plain aliasing vectors above cannot separate those two failures.
    var big: [60]u8 = undefined;
    for (&big, 0..) |*b, i| b.* = 'A' + @as(u8, @intCast(i % 26));
    var grow: std.ArrayList(u8) = .empty;
    defer grow.deinit(alloc);
    try grow.appendSlice(alloc, &.{ 0xc6, 0x0c, 0x02, 0x12, 'a', 'b', 0x0a });
    // fixlen word for a 60-byte string: (60 << 3) | 2 (string subtype) = 482.
    try grow.appendSlice(alloc, &.{ 0xe2, 0x03 });
    try grow.appendSlice(alloc, &big);
    try grow.append(alloc, 0x07);
    try checkAllChunkSizes(
        alloc,
        "string_array [\"ab\", <60 bytes>] (accumulator grows and reallocates)",
        grow.items,
        "string_array",
        &.{ "ab", &big },
    );

    // The same two payloads in the OTHER order, which fails differently and is
    // the sharper of the two. Assembling in place leaves element 0 holding the
    // length it was stored with (60) over a buffer that now holds 2 live bytes,
    // so reading it walks past the payload into spare capacity -- a wrong LENGTH
    // rather than merely wrong content. Small-then-large above cannot show this:
    // there the stale slice is shorter than what replaced it.
    var shrink: std.ArrayList(u8) = .empty;
    defer shrink.deinit(alloc);
    try shrink.appendSlice(alloc, &.{ 0xc6, 0x0c, 0x02, 0xe2, 0x03 });
    try shrink.appendSlice(alloc, &big);
    try shrink.appendSlice(alloc, &.{ 0x0a, 0x12, 'c', 'd', 0x07 });
    try checkAllChunkSizes(
        alloc,
        "string_array [<60 bytes>, \"cd\"] (long element first -- stale length)",
        shrink.items,
        "string_array",
        &.{ &big, "cd" },
    );

    // Control that pins the mechanism as ALIASING rather than a blanket bug in
    // the split path: with only ONE reassembled payload there is no second user
    // of the buffer, and the value was correct even before the fix. If this row
    // ever fails together with the others, the cause is not aliasing.
    try checkAllChunkSizes(
        alloc,
        "string_array [\"ab\"] (single split element -- control)",
        &.{ 0xc6, 0x0c, 0x02, 0x12, 'a', 'b', 0x07 },
        "string_array",
        &.{"ab"},
    );

    std.debug.print("==> chunked decode is chunk-invariant\n", .{});
}
