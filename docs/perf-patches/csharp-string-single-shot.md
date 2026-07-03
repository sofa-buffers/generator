# C# — string/blob single-shot decode

**Impact:** arena 0.81× → **0.89×**. (This is the smallest, safest fix — a good
first one to land, and the same idea applies to Rust, Java, and any language whose
visitor accumulates string/blob bytes. It is exactly what TypeScript's `ChunkAcc`
optimization did in v0.5.1.)
**Reference diff:** `csharp-string-single-shot.patch`
**Generator file:** `generators/csharp/visitor.go` (~L137–L165)

## Problem

The emitted visitor accumulates every string/blob payload byte-by-byte into a
`List<byte> acc`, then copies it out again:

```csharp
public void String(int id, int total, int offset, byte[] data, int chunkOffset, int chunkLength) {
    for (int _i = 0; _i < chunkLength; _i++) acc.Add(data[chunkOffset + _i]);   // copy 1, per byte
    if (acc.Count < total) return;
    var _s = Encoding.UTF8.GetString(acc.ToArray());                            // copy 2 (+ temp array)
    acc.Clear();
    …
}
```

But the corelib's contiguous decode path hands the visitor the **entire payload in
one chunk** (`offset == 0`, `chunkLength == total`). In that common case the `acc`
buffer is pure overhead: hundreds of bounds-checked `List<byte>.Add` calls plus a
throwaway `ToArray()` per string. (This message has a nested string + a blob + a
5-element string array.)

## Fix

Take the payload straight from the input slice on the single-chunk fast path; keep
the accumulator only as the fallback for genuinely split chunks:

```csharp
public void String(int id, int total, int offset, byte[] data, int chunkOffset, int chunkLength) {
    string _s;
    if (offset == 0 && chunkLength >= total) {
        _s = Encoding.UTF8.GetString(data, chunkOffset, total);   // single-shot: no acc
    } else {
        for (int _i = 0; _i < chunkLength; _i++) acc.Add(data[chunkOffset + _i]);
        if (acc.Count < total) return;
        _s = Encoding.UTF8.GetString(acc.ToArray());
        acc.Clear();
    }
    … (the existing (cur,id) switch that stores _s) …
}
```

Blob is identical with `new byte[total]` + `System.Array.Copy(data, chunkOffset, _b, 0, total)`
on the fast path.

## Where in the generator

`generators/csharp/visitor.go` emits the `String` and `Blob` methods (the `acc.Add`
loop lines shown above, ~L137–L165). Wrap the existing accumulate+extract body in
the `else` branch and prepend the `if (offset == 0 && chunkLength >= total)`
single-shot branch that produces `_s`/`_b` directly. The trailing `(cur,id)` switch
that assigns the value is unchanged — just make it consume the `_s`/`_b` computed by
either branch.

## Generalization / caveats

- **Correctness of the guard:** `offset == 0 && chunkLength >= total` is the exact
  "whole payload, single chunk" condition. When it does not hold (a payload split
  across corelib chunks — large strings, or a chunked input reader), fall through to
  the unchanged accumulator path. Never remove the accumulator; it is the streaming
  correctness path.
- **Language-agnostic:** the same transform belongs in every visitor-based decoder.
  Rust (`from_utf8_lossy(chunk)` vs `acc.extend_from_slice`) and Java (`new String(
  data, off, total, UTF_8)` vs a **synchronized** `ByteArrayOutputStream`) already
  carry it in their patches; C++ reads the pointer directly and never had the
  problem. Emit it uniformly.
- **No wire impact:** decode-only representation change; bytes are unchanged.

## Validate

Regenerate `languages/csharp`, run its bench: `sha256` stays `db362b…`. Then remove
the `single-shot-strings.patch` block from `languages/csharp/setup.sh`.
