# Java — primitive `long[]/float[]/double[]` instead of boxed `List<Long>`

**Impact:** arena 0.62× → **0.86×**.
**Reference diff:** `java-primitive-arrays.patch` (touches `Example.java` + `Json.java`)
**Generator files:** `generators/java/backend.go`, `generators/java/visitor.go`, `generators/java/project.go`

## Problem

Native integer/fp array fields are emitted as boxed collections:

```java
class ExampleArrays {
    public List<Long> u8 = new ArrayList<>();   // …and List<Float>/List<Double>
}
```

This is the worst allocator of any language:

- **Decode:** every element is `m.arrays.u8.add(value)` — autoboxes `long`→`Long`.
  Most benchmark values are outside the `Long`/`Integer` cache, so each is a fresh
  heap object: **~50 boxing allocations per decode** for this message, plus
  `ArrayList` backing-array growth.
- **Encode:** `marshal` calls `os.writeArrayUnsigned(0, Sbuf.toLongArray(this.u8))`,
  where `Sbuf.toLongArray` allocates a temporary `long[]` and unboxes element-by-
  element — **10 temp arrays + 50 unboxes per encode**.

## Fix

Store fixed-size native arrays as **primitive arrays** and pass them straight to the
`OStream` writers (which already have `long[]`/`float[]`/`double[]` overloads):

```java
class ExampleArrays {
    public long[] u8 = new long[0];   // float[]/double[] for fp arrays
    public void marshal(OStream os) throws IOException {
        if (this.u8 != null && this.u8.length != 0) { os.writeArrayUnsigned(0, this.u8); }
        // … no Sbuf.toLongArray()
    }
}
```

Decode reserves a small backing array and grows it by index as elements arrive
(no box). The `arrayBegin` `count` is **untrusted wire input**, so it is *not* used
for the up-front `new T[count]` allocation — that is an OOM DoS (see #96); it only
caps growth so a valid array still ends exactly right-sized:

```java
private int ai = 0;                          // add to the visitor
private static final int ARRAY_INIT_CAP = 16;
private int acap = 0;                        // declared count = growth ceiling
public void arrayBegin(int id, ArrayKind kind, int count) {
    ai = 0;
    acap = count;
    switch (cur) {
    case 2: switch (id) { case 0: m.arrays.u8 = new long[Math.min(count, ARRAY_INIT_CAP)]; break; /* … */ } break;
    case 3: switch (id) { case 0: m.arrays.nested.fp32 = new float[Math.min(count, ARRAY_INIT_CAP)]; break; /* … */ } break;
    }
}
public void unsigned(int id, long value) {
    switch (cur) {
    case 0: /* scalar direct-assign, unchanged */ break;
    case 2: switch (id) { case 0: m.arrays.u8 = ensureCap(m.arrays.u8, ai, acap); m.arrays.u8[ai++] = value; break; /* … */ } break;
    }
}
// one overload per element base (long/float/double); doubles but never exceeds `cap`:
private static long[] ensureCap(long[] a, int i, int cap) {
    if (i < a.length) return a;
    long n = (long) a.length * 2;
    if (n < i + 1) n = i + 1;
    if (n > cap) n = cap;
    return java.util.Arrays.copyOf(a, (int) n);
}
```

`Json.from`/`Json.to` must be updated to match (see below). The `Sbuf.toLongArray`
family becomes unused (leave it or stop emitting it).

## Where in the generator

- **Field type + default — `generators/java/backend.go`.** The field declaration
  and `marshal` emission live here. Emit `long[]`/`float[]`/`double[]` (default
  `new long[0]` / `new float[0]`) for fixed-size native numeric/fp arrays instead of
  `List<…>` + `new ArrayList<>()`, and drop the `Sbuf.to*Array(...)` wrapper in
  `marshal` — pass the field directly. Change the empty-guard from `!x.isEmpty()` to
  `x != null && x.length != 0`.
- **Decode — `generators/java/visitor.go`.** Add `int ai` / `int acap` fields and a
  static `ensureCap` overload per element base to the emitted visitor. Change the
  array-element case from `m.arrays.u8.add(value)` to
  `m.arrays.u8 = ensureCap(m.arrays.u8, ai, acap); m.arrays.u8[ai++] = value` (and the
  fp handlers likewise), and change the emitted `arrayBegin` from `.clear()` to
  `<field> = new <prim>[Math.min(count, ARRAY_INIT_CAP)]; ai = 0; acap = count`
  (bounded reservation, not `new <prim>[count]` — the wire count is untrusted, #96).
  (Optional, in the same file: swap the `ArrayDeque<Integer> stack` for an unboxed
  `int[]` + index — the reference diff does this; it removes the per-`sequenceBegin`
  `Integer` boxing.)
- **JSON — `generators/java/project.go`.** This emits `Json.from`/`Json.to`. The
  array cases currently emit `.clear(); for (…) { target.add(ev.getAs…()); }`
  (see ~L253–L269) and `for (… i < o.x.size(); …) b.append(o.x.get(i))`. Change:
  - **from:** `JsonArray _a = e.getAsJsonArray(); o.x = new long[_a.size()]; for (int _k=0;_k<o.x.length;_k++) o.x[_k] = _a.get(_k).getAsLong();`
    (`getAsFloat`/`getAsDouble` for fp; `Long.parseUnsignedLong(_a.get(_k).getAsString())` for `u64`).
  - **to:** iterate `o.x.length` and index `o.x[_i0]` instead of `.size()`/`.get()`.

## Generalization / caveats

- **Element type mapping:** the corelib visitor delivers integer array elements
  **widened to 64-bit** (`unsigned/signed(int id, long value)`), so `long[]` is the
  natural primitive for all integer widths (u8…u64) — matching the existing
  `OStream.writeArray{Unsigned,Signed}(int, long[])` overloads. fp arrays → `float[]`
  / `double[]`.
- **Only for fixed/native arrays.** Keep `List<…>` for `string_array`, struct/union
  arrays, and any non-fixed array — those don't have `OStream` primitive overloads
  and aren't the hot allocator.
- **`arrayBegin` count is untrusted, not authoritative** — for a *valid* message the
  corelib delivers exactly `count` elements, but the count is attacker-controlled wire
  input (up to `ARRAY_MAX ≈ 2^31`). Allocating `new long[count]` up front is an
  `OutOfMemoryError` DoS on malformed input (#96). Reserve `min(count, ARRAY_INIT_CAP)`
  and grow on demand, capped at `count`: allocation then tracks elements actually
  delivered (bounded by input size), yet a valid array still ends exactly right-sized.

## Also folded in: string/blob single-shot

The same diff replaces the `ByteArrayOutputStream acc` accumulate on the common
whole-in-one-chunk case with a direct `new String(data, chunkOffset, total, UTF_8)`
(and `Arrays.copyOfRange` for blobs). This is worth doing on its own: `BAOS.write`
/`toByteArray` are **`synchronized`**, so the single-shot path also drops a monitor
enter/exit per string. See `csharp-string-single-shot.md` for the shared rationale.

## Validate

Regenerate `languages/java`, run its bench: `sha256` must stay `db362b…`. Then
remove the `primitive-arrays.patch` block from `languages/java/setup.sh`.
