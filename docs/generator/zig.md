# Zig target — `targets.zig`

Emits the generated structs for every message and named type, against
`corelib-zig`.

## Options

This target has none of its own. The generic options — `emit`, `license`,
`max_message_size`, the `max_dyn_*` decode limits — are documented in the
[generic config](README.md) and apply here unchanged.

## Formatting

Every generated file is `zig fmt` output already: `message.zig` and, under
`emit: project`, the harness, `build.zig` and `build.zig.zon`. A
`zig fmt --check` over a tree that holds generated code passes, so generated
files need no exclusion from such a check and never come back reformatted.
`sofabgen` produces that layout itself and does not run `zig`.
