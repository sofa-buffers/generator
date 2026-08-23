# Java target — `targets.java`

Emits one class per message and named type, against `corelib-java`. Java allows
a single public top-level class per file, so every named struct and union gets
its own alongside the message.

## Options

| key | type | default | effect |
|---|---|---|---|
| `package` | string | `message` | The `package <name>;` declaration, and the source-directory layout. |

The generic options apply here too; see the [generic config](README.md).

## `package`

Two effects, and the second is the one that surprises people.

It sets the `package` declaration in every generated file — and it also decides
**where those files are written**. Java requires the directory structure to
mirror the package, so the output is always laid out under
`src/main/java/<package as directories>/`:

| `package` | output path |
|---|---|
| `message` (default) | `src/main/java/message/Telemetry.java` |
| `com.acme.msg` | `src/main/java/com/acme/msg/Telemetry.java` |

This holds in both `emit` modes — `sources` emits the same tree without the
build files. Changing the package changes the paths, so point `output_dir` at
the root of a source tree, not at the package directory itself.
