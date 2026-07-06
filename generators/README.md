# Backends

Concrete generators live here — one package per target (`c/`, `cpp/`, `rust/`,
`golang/`, `python/`, `java/`, `csharp/`, `typescript/`, plus the non-code
`docs/` target), each implementing the `internal/generator.Backend` contract
(Visitor over the IR + Builder for source construction). Embedded C++ is not a
separate package: it is the `cpp` backend with `corelib: c-cpp`. `docs/` emits
a self-contained HTML reference page instead of source code.

**All 8 language backends are wired and conformance-tested.** Adding a target is purely
additive — a new package here that calls `generator.Register(...)` from its
`init()`, blank-imported by `cmd/sofabgen`. The core imports nothing from this
directory, so dependency arrows point inward (PLAN §8.6).

See `docs/ARCHITECTURE.md` → "How to add a new target language".
