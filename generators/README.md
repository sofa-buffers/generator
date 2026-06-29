# Language backends

Concrete, language-specific generators live here — one package per target
(`c/`, `cpp/`, `cpp_embedded/`, `rust/`, `golang/`, `python/`, `java/`,
`csharp/`, `typescript/`), each implementing the `internal/generator.Backend`
contract (Visitor over the IR + Builder for source construction).

**No backend is wired yet (M0).** The core pipeline (`internal/`) builds and
freezes a language-neutral IR and stops there. Adding a target is purely
additive — a new package here that calls `generator.Register(...)` from its
`init()`, blank-imported by `cmd/sofabgen`. The core imports nothing from this
directory, so dependency arrows point inward (PLAN §8.6).

See `docs/ARCHITECTURE.md` → "How to add a new target language".
