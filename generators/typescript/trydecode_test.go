package typescript

import (
	"strings"
	"testing"
)

// reuseSrc has a field of every shape tryDecode treats differently: kept and
// written over (a typed array at the message and in a nested struct, a blob, a
// string, struct, union and typed-row array) and rebuilt as decode() builds it
// (a union option's array, a row of strings, a struct element's own array, and a
// 64-bit array, which is typed only under int64: bigint).
const reuseSrc = `version: 1
messages:
  m:
    payload:
      a:  { id: 0, type: array, items: { type: u16, count: 4 } }
      w:  { id: 1, type: array, items: { type: u64, count: 4 } }
      b:  { id: 2, type: blob, maxlen: 8 }
      sa: { id: 3, type: array, items: { type: string, count: 2, maxlen: 4 } }
      ra: { id: 4, type: array, items: { type: struct, count: 2, fields: { t: { id: 0, type: array, items: { type: u8, count: 2 } } } } }
      ua: { id: 5, type: array, items: { type: union, count: 2, default_id: 0, oneof: { p: { id: 0, type: u8 } } } }
      mx: { id: 6, type: array, items: { type: array, count: 2, items: { type: u8, count: 2 } } }
      mw: { id: 7, type: array, items: { type: array, count: 2, items: { type: string, count: 2, maxlen: 4 } } }
      st: { id: 8, type: struct, fields: { ta: { id: 0, type: array, items: { type: i32, count: 2 } } } }
      un: { id: 9, type: union, default_id: 0, oneof: { xs: { id: 0, type: array, items: { type: u16, count: 2 } } } }
`

// visitorBody is the text of the flat visitor class of raw type name `raw`.
func visitorBody(t *testing.T, mod, raw string) string {
	t.Helper()
	head := "class _" + raw + "__Visitor implements Visitor {"
	start := strings.Index(mod, head)
	if start < 0 {
		t.Fatalf("no visitor %s:\n%s", raw, mod)
	}
	end := strings.Index(mod[start:], "\n}\n")
	return mod[start : start+end]
}

// TestTSTryDecodeEntry: a message carries tryDecode(bytes, out), which keeps
// the destination's storage, resets it, decodes through the corelib's one-shot
// tryDecode (the verdict is its return value, a refusal its throw) and cuts the
// re-armed arrays however the decode ended. A struct, decodable on its own,
// does not: §6.1.1's twin belongs to the message, as in the other backends.
func TestTSTryDecodeEntry(t *testing.T) {
	mod := genTSWith(t, reuseSrc, map[string]any{})
	want := "  static tryDecode(bytes: Uint8Array, out: M): DecodeStatus {\n" +
		"    const v = _M__VisitorCache.get(out);\n" +
		"    v._reuse();\n" +
		"    try {\n" +
		"      return _tryDecode(bytes, v);\n" +
		"    } finally {\n" +
		"      v._end();\n" +
		"    }\n" +
		"  }"
	if !strings.Contains(classBody(t, mod, "M"), want) {
		t.Errorf("message tryDecode missing\n%s\nin:\n%s", want, classBody(t, mod, "M"))
	}
	if !strings.Contains(mod, "tryDecode as _tryDecode") {
		t.Errorf("the corelib's tryDecode must be imported under the alias:\n%s", mod[:strings.Index(mod, "\n")])
	}
	if strings.Contains(classBody(t, mod, "M_St"), "tryDecode") {
		t.Errorf("a struct has no tryDecode:\n%s", classBody(t, mod, "M_St"))
	}
	for _, doc := range []string{
		"   * `out` is reset to its declared defaults first, so the result equals",
		"   * @throws SofabError the bytes were refused: `INVALID_MSG` (malformed, or",
		"   *   holds the fields decoded before the error, never an older message's;",
		// The one consequence of a kept visitor a caller can observe.
		"   * One decode at a time per destination: the decode machinery `out` holds is",
	} {
		if !strings.Contains(mod, doc) {
			t.Errorf("tryDecode doc missing %q", doc)
		}
	}
}

// TestTSVisitorCache: the visitor is taken from a module-level cache keyed on the
// destination, so a receive loop builds it once per destination and not once per
// message (generator#775). The memo is the corelib's (VisitorCache,
// corelib-ts#216); what is generated is the one cache per message and the factory
// that names its visitor. decode(bytes) builds a destination of its own per call,
// so it keeps its own `new`, and a struct or union -- neither has tryDecode --
// gets no cache at all.
func TestTSVisitorCache(t *testing.T) {
	mod := genTSWith(t, reuseSrc, map[string]any{})
	for _, want := range []string{
		"const _M__VisitorCache = new VisitorCache<M, _M__Visitor>(\n" +
			"  (o) => new _M__Visitor(o, new PayloadAcc()),\n" +
			");",
		// The cache is declared after the class it builds, and used before it:
		// the factory runs per miss, not at module load.
		"class _M__Visitor implements Visitor {",
	} {
		if !strings.Contains(mod, want) {
			t.Errorf("missing %q in:\n%s", want, mod)
		}
	}
	if !strings.Contains(mod, "VisitorCache,") && !strings.Contains(mod, "VisitorCache }") {
		t.Errorf("VisitorCache must be imported from the corelib:\n%s", mod[:strings.Index(mod, "\n")])
	}
	if i, j := strings.Index(mod, "class _M__Visitor implements Visitor {"), strings.Index(mod, "const _M__VisitorCache ="); i > j {
		t.Errorf("the cache must follow the visitor class it builds (class at %d, cache at %d)", i, j)
	}
	// decode(bytes) has nothing to cache: its destination is new every call.
	if !strings.Contains(classBody(t, mod, "M"), "    _decode(bytes, new _M__Visitor(o, new PayloadAcc()));") {
		t.Errorf("decode must keep building its own visitor:\n%s", classBody(t, mod, "M"))
	}
	for _, raw := range []string{"M_St", "M_Ra", "M_Un"} {
		if strings.Contains(mod, "const _"+raw+"__VisitorCache") {
			t.Errorf("%s has no tryDecode and must have no visitor cache", raw)
		}
	}
	// A field-less message still has tryDecode, so it still has a cache -- and
	// its visitor reads no location, so _reuse must not name one (generator#738).
	empty := genTSWith(t, "version: 1\nmessages:\n  ping:\n    payload: {}\n", map[string]any{})
	if !strings.Contains(empty, "const _Ping__VisitorCache = new VisitorCache<Ping, _Ping__Visitor>(") {
		t.Errorf("a field-less message needs its cache too:\n%s", empty)
	}
	if strings.Contains(empty, "this._c") {
		t.Errorf("a field-less message's visitor tracks no location:\n%s", empty)
	}
}

// TestTSReuseEmitsNoDeadStatement: _reuse and _end put back only what an arm of
// THIS visitor can have moved. A flat message -- no nested scope, so no arm
// assigns the scope cursor -- gets no cursor store, and a message with no string
// or blob leaf reaches no payload accumulator, so it gets no release. Both would
// be correct and both would be dead: a statement per decode on the entry point
// whose cost is the point (generator#775).
func TestTSReuseEmitsNoDeadStatement(t *testing.T) {
	flat := genTSWith(t, "version: 1\nmessages:\n  flat:\n    payload:\n"+
		"      x: { id: 0, type: u8 }\n"+
		"      s: { id: 1, type: string, maxlen: 4 }\n"+
		"      a: { id: 2, type: array, items: { type: u16, count: 2 } }\n", map[string]any{})
	vis := visitorBody(t, flat, "Flat")
	if !strings.Contains(vis, "this._c") {
		t.Fatalf("a flat message's hooks still COMPARE against the cursor:\n%s", vis)
	}
	if strings.Contains(vis, "this._c = ") {
		t.Errorf("no arm moves the cursor, so _reuse must not put it back:\n%s", vis)
	}
	// Its string field reaches the accumulator, so the release stays.
	if !strings.Contains(vis, "    this.a.release();\n") {
		t.Errorf("a string field reaches the accumulator; _end must release it:\n%s", vis)
	}
	// A payload that lives ONLY in a wrapper array reaches the accumulator too --
	// as the bare `this.a` argument of a StringSeq/BlobSeq, never as `this.a.`, so
	// a substring gate on the latter would leave this message without a release.
	// It is the shape with the most to retain: a blob element's buffer is sized by
	// a `total` the sender declared, up to the receiver cap.
	wrapOnly := genTSWith(t, "version: 1\nmessages:\n  w:\n    payload:\n"+
		"      sa: { id: 0, type: array, items: { type: string, maxlen: 8 } }\n"+
		"      bx: { id: 1, type: array, items: { type: blob } }\n", map[string]any{})
	wv := visitorBody(t, wrapOnly, "W")
	// The HOOKS, not the whole class: _end's own release is spelled `this.a.`.
	if hooks := wv[:strings.Index(wv, "  _reuse(): void {")]; strings.Contains(hooks, "this.a.") {
		t.Fatalf("this schema's payloads go through the collectors only:\n%s", hooks)
	}
	if !strings.Contains(wv, "    this.a.release();\n") {
		t.Errorf("a collector holds the accumulator; _end must release it:\n%s", wv)
	}
	// No string, no blob: nothing in the visitor touches the accumulator.
	noPayload := genTSWith(t, "version: 1\nmessages:\n  nums:\n    payload:\n"+
		"      x: { id: 0, type: u8 }\n"+
		"      a: { id: 1, type: array, items: { type: u16, count: 2 } }\n", map[string]any{})
	if nv := visitorBody(t, noPayload, "Nums"); strings.Contains(nv, "this.a.release()") {
		t.Errorf("no payload field reaches the accumulator, so there is nothing to release:\n%s", nv)
	}
}

// TestTSReuseKeepsFixedStorage: _reuse moves into a register exactly the
// storage a reset() would replace and a later arm can write into -- the fields of
// the message and of a struct reached through struct fields -- and resets every
// other field as reset() does: a union through clear(), a row of strings and a
// Long[] to a new default. _end gives each field whose register no arm took the
// default reset() would have given it, then cuts every re-armed collector. A
// union option, a struct element's own array, a row of strings and a Long[] are
// not kept: they are rebuilt as decode() builds them.
func TestTSReuseKeepsFixedStorage(t *testing.T) {
	for mode, w := range map[string][2]string{
		"bigint": {
			"    this._s0W = this.o.w;\n",
			"    if (this._s0W !== null) { this.o.w = _E_BigUint64Array; this._s0W = null; this._a0W = _E_BigUint64Array; }\n",
		},
		"long":   {"    this.o[\"_w\"] = [];\n", ""},
		"number": {"    this.o[\"_w\"] = [];\n", ""},
	} {
		vis := visitorBody(t, genTSWith(t, reuseSrc, map[string]any{"int64": mode}), "M")
		want := "  _reuse(): void {\n" +
			// The scope cursor, first: a kept visitor starts every decode at the
			// root, whatever subtree the last one was cut short in.
			"    this._c = _M__Loc;\n" +
			"    this._re = true;\n" +
			"    this._s0A = this.o.a;\n" +
			w[0] +
			"    this._s0B = this.o.b;\n" +
			"    this._s1 = this.o.sa;\n" +
			"    this._s2 = this.o.ra;\n" +
			"    this._s4 = this.o.ua;\n" +
			"    this._s6 = this.o.mx;\n" +
			"    this.o.mw = [];\n" +
			"    this._s9Ta = this.o.st.ta;\n" +
			"    this.o.un.clear();\n" +
			"  }\n"
		if !strings.Contains(vis, want) {
			t.Errorf("int64: %s -- _reuse missing\n%s\nin:\n%s", mode, want, vis)
		}
		// Each arm drops what it just replaced -- the register, and the bulk
		// destination that named the same storage -- and the sweep after them
		// drops every register the arms did not reach: the two collectors that
		// are never re-armed (_q7 rows of strings, _q8 its row), the bulk
		// destination inside an array element (_a3T) and inside the union
		// (_a10Xs), and the payload accumulator. A kept visitor must hold no
		// storage the destination no longer has. A Long[] destination (_a0W, the
		// long/number modes) is NOT dropped: its only empty value is a fresh
		// `[]`, and an allocation per decode is what this entry point exists to
		// avoid.
		wantEnd := "  _end(): void {\n" +
			"    if (this._s0A !== null) { this.o.a = _E_Uint16Array; this._s0A = null; this._a0A = _E_Uint16Array; }\n" +
			w[1] +
			"    if (this._s0B !== null) { this.o.b = new Uint8Array(); this._s0B = null; }\n" +
			"    if (this._s1 !== null) { this.o.sa = []; this._s1 = null; }\n" +
			"    if (this._s2 !== null) { this.o.ra = []; this._s2 = null; }\n" +
			"    if (this._s4 !== null) { this.o.ua = []; this._s4 = null; }\n" +
			"    if (this._s6 !== null) { this.o.mx = []; this._s6 = null; }\n" +
			"    if (this._s9Ta !== null) { this.o.st.ta = _E_Int32Array; this._s9Ta = null; this._a9Ta = _E_Int32Array; }\n" +
			"    this._q1?.end(); this._q1 = null;\n" +
			"    this._q2?.end(); this._q2 = null;\n" +
			"    this._q4?.end(); this._q4 = null;\n" +
			"    this._q6?.end(); this._q6 = null;\n" +
			"    this._row6 = _E_Uint8Array;\n" +
			"    this._q7 = null;\n" +
			"    this._q8 = null;\n" +
			"    this._a3T = _E_Uint8Array;\n" +
			"    this._a10Xs = _E_Uint16Array;\n" +
			"    this.a.release();\n" +
			"  }"
		if strings.Contains(vis, "this._a0W = []") {
			t.Errorf("int64: %s -- a Long[] destination must not be dropped through an allocation:\n%s", mode, vis)
		}
		if !strings.Contains(vis, wantEnd) {
			t.Errorf("int64: %s -- _end missing\n%s\nin:\n%s", mode, wantEnd, vis)
		}
		if strings.Contains(vis, "this.o.reset()") {
			t.Errorf("int64: %s -- _reuse must not run reset(): it would replace the storage it keeps", mode)
		}
	}
}

// TestTSReuseArms: each kept storage has its arm, and only those. A typed array
// is the destination when it is the wire count long (a cut-short decode leaves
// zeros past what it wrote, as a new array would -- the decoder zeroes that
// tail, which reuse_storage_check's chain leg pins); a blob is offered to the corelib's
// takeReusing and released once taken; a wrapper array's collector is
// re-armed over the kept array, with the element's own reset for a struct and
// its clear() for a union; a typed row comes back from staleAt.
func TestTSReuseArms(t *testing.T) {
	mod := genTSWith(t, reuseSrc, map[string]any{})
	vis := visitorBody(t, mod, "M")
	for _, want := range []string{
		"const _d = reuseU16(this._s0A, count); this._s0A = null; this.o.a = _d;",
		"const _d = reuseU64(this._s0W, count); this._s0W = null; this.o.w = _d;",
		"const _d = reuseI32(this._s9Ta, count); this._s9Ta = null; this.o.st.ta = _d;",
		// The kept buffer is released once the field holds it, so a repeated id
		// (§7.4) never writes its payload into the value an earlier occurrence left.
		"{ const _p = this.a.takeReusing(this._s0B, total, offset, src, start, end); if (_p !== null) { this.o.b = _p; this._s0B = null; } }",
		"const _r = this._s1; this._s1 = null; const _t: string[] = _r ?? []; this.o.sa = _t; this._q1 = new StringSeq(",
		"if (_r !== null) this._q1.reuse();",
		"if (_r !== null) this._q2.reuse(_M_Ra__Reset);",
		"if (_r !== null) this._q4.reuse(_M_Ua__Reset);",
		"const _r = reuseU8(this._re ? this._q6!.staleAt(id) : null, count);",
		"  private _s1: string[] | null = null;",
		"  private _s0A: Uint16Array | null = null;",
		"  private _s0B: Uint8Array | null = null;",
	} {
		if !strings.Contains(vis, want) {
			t.Errorf("missing %q in:\n%s", want, vis)
		}
	}
	for _, want := range []string{
		"const _M_Ra__Reset = (o: M_Ra): M_Ra => { o.reset(); return o; };",
		"const _M_Ua__Reset = (o: M_Ua): M_Ua => { o.clear(); return o; };",
	} {
		if !strings.Contains(mod, want) {
			t.Errorf("missing element reset %q", want)
		}
	}
	// Rebuilt as decode() builds them: the union option, the struct element's
	// own array, the row of strings.
	for _, want := range []string{
		"const _d = new Uint16Array(count); this.o.un.xs = _d;",
		"const _d = new Uint8Array(count); this.o.ra[this._ix2]!.t = _d;",
		"case 7: { const _t: string[][] = []; this.o.mw = _t; this._q7 = new FramedSeq<string[]>(",
	} {
		if !strings.Contains(vis, want) {
			t.Errorf("missing fresh arm %q in:\n%s", want, vis)
		}
	}
}

// TestTSReuseOnlyInMessageVisitor: a struct's or a union's own visitor serves
// only its decode(bytes), which has nothing to reuse, and is emitted exactly as
// before: no registers, no hooks, no reuse arm.
func TestTSReuseOnlyInMessageVisitor(t *testing.T) {
	mod := genTSWith(t, reuseSrc, map[string]any{})
	for _, raw := range []string{"M_St", "M_Ra", "M_Un"} {
		vis := visitorBody(t, mod, raw)
		for _, no := range []string{"_reuse", "_end", "this._s", "staleAt", ".reuse(", "takeInto"} {
			if strings.Contains(vis, no) {
				t.Errorf("%s visitor must not carry %q:\n%s", raw, no, vis)
			}
		}
	}
}

// TestTSTryDecodeHarnessVerb: the harness reaches tryDecode through its own verb,
// which check_reused_destination.py drives with --verb.
func TestTSTryDecodeHarnessVerb(t *testing.T) {
	files, err := (&Backend{}).Generate(schema(t, reuseSrc), map[string]any{"emit": "project"})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	var h string
	for _, f := range files {
		if f.Path == "harness.ts" {
			h = string(f.Content)
		}
	}
	for _, want := range []string{
		`} else if (mode === "trydecodeinto") {`,
		"      st = cls.tryDecode(new Uint8Array(input), prev as never);",
	} {
		if !strings.Contains(h, want) {
			t.Errorf("harness missing %q", want)
		}
	}
}

// TestTSJSONCopiesStringArrays: toJSON hands out, and fromJSON takes in, a COPY
// of a string array -- like every other element kind, which is converted anyway.
// tryDecode writes the next message into the member array, so an alias either
// way would let a later decode rewrite a JSON value already returned, or the
// caller's own input.
func TestTSJSONCopiesStringArrays(t *testing.T) {
	mod := genTSWith(t, reuseSrc, map[string]any{})
	for _, want := range []string{
		`"sa": this.sa.slice(),`,
		`o.sa = (d["sa"] as string[]).slice();`,
		`"mw": this.mw.map((_x0) => _x0.slice()),`,
		`o.mw = (d["mw"] as unknown[]).map((_x0) => (_x0 as string[]).slice());`,
	} {
		if !strings.Contains(mod, want) {
			t.Errorf("missing %q", want)
		}
	}
}
