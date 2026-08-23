package generator

// Receiver-side decode caps for schema-UNBOUNDED fields (ARCHITECTURE §9.5,
// CORELIB_PLAN §6.2.1). Every target has a finite default for all three; there
// is no unset state and no unlimited mode. A backend resolves them once per
// run with ServerDynLimits.Resolve(cfg) or ClientDynLimits.Resolve(cfg).
//
// WHAT THESE NUMBERS ARE FOR. They are an amplification barrier, not
// application policy. The attack they close is a ~10-byte message claiming
// count = 2^31, i.e. a 2 GB allocation from nothing; capping the count at 65536
// removes five orders of magnitude from that, which is the whole security win.
// Going much lower buys almost nothing against the DoS while rejecting a great
// deal of ordinary traffic.
//
// WHERE APPLICATION-TIGHT BOUNDS BELONG. In the schema (`count`/`maxlen`),
// where they are wire-visible to both peers and enforced as INVALID on every
// target. A receiver cap is the wrong place to say "my telemetry rows are at
// most 1000 long": it binds only this receiver, the sender cannot see it, and
// its violation is a LimitExceeded policy rejection rather than a statement
// about the format. A field that legitimately needs more than the cap gets an
// explicit schema bound -- that is the escape hatch, and the per-target config
// key is the other one.
//
// THE UNIT TRAP (#228). ArrayCount is an ELEMENT count, never a byte budget.
// 65536 u64 elements is 512 KiB; 65536 elements of a nested object type is
// 65536 x sizeof(element), which the check-then-allocate shape (§9.5 shape A)
// reserves exactly. Read each default with the byte budget stated beside it,
// not as a number on its own.
type DynLimits struct {
	// ArrayCount caps the element count of an array without a schema `count`,
	// and the element INDEX of a dynamic wrapper array (whose length is highest
	// present id + 1, so the index IS the length -- MESSAGE_SPEC §5.1).
	ArrayCount int64
	// StringLen caps the wire byte length of a string without a schema `maxlen`.
	StringLen int64
	// BlobLen caps the wire byte length of a blob without a schema `maxlen`.
	BlobLen int64
}

// ServerDynLimits is the default tier for targets whose typical deployment is a
// server or a native process with a machine-sized memory budget: Go, Java, C#,
// Python, Rust (std), pure C++ (corelib: cpp) and Zig.
//
//   - ArrayCount 65536 -- the value examples/config/sofabgen.yaml already
//     suggested, so not a new precedent. 512 KiB as u64 elements.
//   - StringLen 1 MiB -- a string is text, and a megabyte of it is already far
//     outside any structured-message use. Deliberately an order below the blob
//     cap: the two field kinds carry different things.
//   - BlobLen 4 MiB -- matches gRPC's default max receive message size, i.e. a
//     number operators already recognise as "the one you raise for large
//     payloads". A blob is the opaque-payload type: images, compressed bodies,
//     certificate chains.
var ServerDynLimits = DynLimits{ArrayCount: 65536, StringLen: 1 << 20, BlobLen: 4 << 20}

// ClientDynLimits is one order of magnitude down, for targets that typically
// run inside a browser tab or a phone process: TypeScript, Dart, Kotlin
// Multiplatform. Those have a memory budget well below a server's, and
// TypeScript additionally pays 2x on strings -- a byte cap on the wire becomes
// UTF-16 in memory.
//
// There is deliberately no third, smaller "firmware" tier: the statically
// bounded profiles (C, C++ corelib: c-cpp, Rust no_std) reject an unbounded
// field at schema-validation time, so no field a cap could govern exists there
// and a tier for them would apply to nothing.
var ClientDynLimits = DynLimits{ArrayCount: 16384, StringLen: 256 << 10, BlobLen: 1 << 20}

// Resolve returns the tier's defaults with any configured max_dyn_* key
// substituted in. The `generic:` / per-target override mechanism is the config
// layer's; by the time a backend sees cfg the override is already merged, so
// this only has to distinguish "key present" from "key absent".
func (d DynLimits) Resolve(cfg map[string]any) DynLimits {
	out := d
	if v, ok := dynLimit(cfg, "max_dyn_array_count"); ok {
		out.ArrayCount = v
	}
	if v, ok := dynLimit(cfg, "max_dyn_string_len"); ok {
		out.StringLen = v
	}
	if v, ok := dynLimit(cfg, "max_dyn_blob_len"); ok {
		out.BlobLen = v
	}
	return out
}

// dynLimit reads one integer config key. The value arrives as whatever the
// YAML/JSON decoder produced, hence the width sweep.
func dynLimit(cfg map[string]any, key string) (int64, bool) {
	switch v := cfg[key].(type) {
	case int:
		return int64(v), true
	case int64:
		return v, true
	case uint64:
		return int64(v), true
	case float64:
		return int64(v), true
	}
	return 0, false
}
