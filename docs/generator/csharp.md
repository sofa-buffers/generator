# C# target — `targets.csharp`

Emits one class per message and named type, against `corelib-cs`.

## Options

| key | type | default | effect |
|---|---|---|---|
| `namespace` | string | `Message` | The `namespace <name>` wrapping the generated classes. |

The generic options apply here too; see the [generic config](README.md).

## `namespace`

Wraps every generated type. Unlike Java's and Kotlin's `package`, it has no
effect on file layout — C# does not tie namespaces to directories, so the output
stays a single `Message.cs` in the output directory whichever namespace you
choose.

`generic.namespace` sets it for every target that has one; this key overrides
that for C# alone. Left unset, this target's own default (`Message`) applies
rather than a generic one — each language keeps its own idiomatic capitalisation.
