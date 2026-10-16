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
		"    const v = new _M__Visitor(out, new PayloadAcc());\n" +
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
	} {
		if !strings.Contains(mod, doc) {
			t.Errorf("tryDecode doc missing %q", doc)
		}
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
		"bigint": {"    this._s0W = this.o.w;\n", "    if (this._s0W !== null) this.o.w = _E_BigUint64Array;\n"},
		"long":   {"    this.o[\"_w\"] = [];\n", ""},
		"number": {"    this.o[\"_w\"] = [];\n", ""},
	} {
		vis := visitorBody(t, genTSWith(t, reuseSrc, map[string]any{"int64": mode}), "M")
		want := "  _reuse(): void {\n" +
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
		wantEnd := "  _end(): void {\n" +
			"    if (this._s0A !== null) this.o.a = _E_Uint16Array;\n" +
			w[1] +
			"    if (this._s0B !== null) this.o.b = new Uint8Array();\n" +
			"    if (this._s1 !== null) this.o.sa = [];\n" +
			"    if (this._s2 !== null) this.o.ra = [];\n" +
			"    if (this._s4 !== null) this.o.ua = [];\n" +
			"    if (this._s6 !== null) this.o.mx = [];\n" +
			"    if (this._s9Ta !== null) this.o.st.ta = _E_Int32Array;\n" +
			"    this._q1?.end();\n" +
			"    this._q2?.end();\n" +
			"    this._q4?.end();\n" +
			"    this._q6?.end();\n" +
			"  }"
		if !strings.Contains(vis, wantEnd) {
			t.Errorf("int64: %s -- _end missing\n%s\nin:\n%s", mode, wantEnd, vis)
		}
		if strings.Contains(vis, "this.o.reset()") {
			t.Errorf("int64: %s -- _reuse must not run reset(): it would replace the storage it keeps", mode)
		}
	}
}

// TestTSReuseArms: each kept storage has its arm, and only those. A typed array
// is the destination when it is the wire count long, zeroed first (a cut-short
// decode leaves zeros, as a new array would); a blob is offered to the corelib's
// takeReusing and released once taken; a wrapper array's collector is
// re-armed over the kept array, with the element's own reset for a struct and
// its clear() for a union; a typed row comes back from staleAt.
func TestTSReuseArms(t *testing.T) {
	mod := genTSWith(t, reuseSrc, map[string]any{})
	vis := visitorBody(t, mod, "M")
	for _, want := range []string{
		"const _s = this._s0A; this._s0A = null; const _d = _s !== null && _s.length === count ? _s.fill(0) : new Uint16Array(count); this.o.a = _d;",
		"const _s = this._s0W; this._s0W = null; const _d = _s !== null && _s.length === count ? _s.fill(0n) : new BigUint64Array(count); this.o.w = _d;",
		"const _s = this._s9Ta; this._s9Ta = null; const _d = _s !== null && _s.length === count ? _s.fill(0) : new Int32Array(count); this.o.st.ta = _d;",
		// The kept buffer is released once the field holds it, so a repeated id
		// (§7.4) never writes its payload into the value an earlier occurrence left.
		"{ const _p = this.a.takeReusing(this._s0B, total, offset, src, start, end); if (_p !== null) { this.o.b = _p; this._s0B = null; } }",
		"const _r = this._s1; this._s1 = null; const _t: string[] = _r ?? []; this.o.sa = _t; this._q1 = new StringSeq(",
		"if (_r !== null) this._q1.reuse();",
		"if (_r !== null) this._q2.reuse(_M_Ra__Reset);",
		"if (_r !== null) this._q4.reuse(_M_Ua__Reset);",
		"const _o = this._re ? this._q6!.staleAt(id) : null; const _r = _o !== null && _o.length === count ? _o.fill(0) : new Uint8Array(count);",
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
