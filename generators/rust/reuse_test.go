package rust

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
)

// isStdCfg reports whether a backend config selects corelib-rs, the default.
func isStdCfg(cfg map[string]any) bool {
	c, _ := cfg["corelib"].(string)
	return c == "" || c == "rs"
}

// seqFor turns an expected reservation into the one cfg emits. On
// corelib-rs-no-std it is returned unchanged; on corelib-rs a wrapper array on a
// fixed path decodes into a reused destination (generator#755), so
// `reserve_row(&mut PATH, id, B)` becomes `reuse_row(&mut PATH, &mut
// self.lv[LV], id, B)` and `reserve_elem(&mut PATH, id, B)` becomes
// `reuse_elem(&mut PATH, &mut self.lv[LV], id, B, RESET)` -- the same bound and
// the same verdicts, through the corelib's reuse twin. path is the container
// (`self.m.objs`), lv its live slot, reset the element reset reuse_elem takes.
func seqFor(cfg map[string]any, want, path string, lv int, reset string) string {
	if !isStdCfg(cfg) {
		return want
	}
	q := regexp.QuoteMeta(path)
	row := regexp.MustCompile(`reserve_row\(&mut ` + q + `, id, `)
	want = row.ReplaceAllLiteralString(want, fmt.Sprintf("reuse_row(&mut %s, &mut self.lv[%d], id, ", path, lv))
	elem := regexp.MustCompile(`reserve_elem\(&mut ` + q + `, id, (sofab::seq::Bound::\w+\([^)]*\))\)`)
	return elem.ReplaceAllString(want, fmt.Sprintf("reuse_elem(&mut %s, &mut self.lv[%d], id, ${1}, %s)", path, lv, reset))
}

// reuseSrc has every position a reused destination distinguishes: wrapper arrays
// at the root (strings, blobs, structs, unions, native rows, wrapper rows), one
// inside a struct member (still a fixed path), one inside a union option and one
// inside an array element (neither is), and leaves of every storage kind.
const reuseSrc = `
version: 1
messages:
  m:
    payload:
      n:    { id: 0, type: u32, default: 5 }
      s:    { id: 1, type: string, maxlen: 8, default: "ab" }
      b:    { id: 2, type: blob, maxlen: 4, default: "AQI=" }
      a:    { id: 3, type: array, items: { type: u16, count: 4 }, default: [1, 2] }
      ss:   { id: 4, type: array, items: { type: string, count: 3, maxlen: 8 } }
      bs:   { id: 5, type: array, items: { type: blob, count: 3, maxlen: 8 } }
      objs:
        id: 6
        type: array
        items:
          type: struct
          count: 3
          fields:
            k:    { id: 0, type: u8 }
            tags: { id: 1, type: array, items: { type: string, count: 2, maxlen: 4 } }
      us:
        id: 7
        type: array
        items:
          type: union
          count: 2
          oneof:
            p: { id: 0, type: u8 }
            q: { id: 1, type: string, maxlen: 4 }
      mat:  { id: 8, type: array, items: { type: array, count: 2, items: { type: u8, count: 3 } } }
      rows: { id: 9, type: array, items: { type: array, count: 2, items: { type: string, count: 2, maxlen: 4 } } }
      st:
        id: 10
        type: struct
        fields:
          x:    { id: 0, type: i8, default: -1 }
          vals: { id: 1, type: array, items: { type: string, count: 2, maxlen: 4 } }
          in:   { id: 2, type: struct, fields: { y: { id: 0, type: u8 } } }
      u:
        id: 11
        type: union
        oneof:
          d:  { id: 0, type: struct, fields: { z: { id: 0, type: string, maxlen: 4 } } }
          xs: { id: 1, type: array, items: { type: string, count: 2, maxlen: 4 } }
`

var noStdCfgs = []map[string]any{
	{"corelib": "rs-no-std"},
	{"corelib": "rs-no-std", "allow_dynamic": true},
	{"corelib": "rs-no-std", "no_std": false},
	{"corelib": "rs-no-std", "no_std": false, "allow_dynamic": true},
}

// TestRustNoStdTakesNoReuse: corelib-rs-no-std is the footprint profile and
// does not take the decode into a reused destination (corelib-rs-no-std#133:
// +348 B of flash with heapless, +372-380 B with alloc, for nothing heapless
// storage could keep). Nothing of it may reach that output -- not the API, not a
// live count, not a reset, not a corelib reuse call -- in any of its four builds.
func TestRustNoStdTakesNoReuse(t *testing.T) {
	for _, cfg := range noStdCfgs {
		m := moduleFromYAML(t, reuseSrc, cfg)
		if !strings.Contains(m, "pub fn try_decode(data: &[u8]) -> Result<Self, DecodeError> {") {
			t.Errorf("(%v) try_decode must stay try_decode(data) -> Result<Self, DecodeError>", cfg)
		}
		for _, bad := range []string{"fn reset(", "fn _reset(", "fn _rearm(", "fn _trim(", "reuse_leaf", "reuse_elem", "reuse_row", "lv:", "self.lv[", "out: &mut Self"} {
			if strings.Contains(m, bad) {
				t.Errorf("(%v) the footprint profile must not carry %q", cfg, bad)
			}
		}
	}
}

// TestRustReuseDestination: corelib-rs decodes into the caller's destination
// (generator#755) and keeps the storage it holds.
func TestRustReuseDestination(t *testing.T) {
	for _, cfg := range []map[string]any{{"corelib": "rs"}, {"corelib": "rs", "allow_dynamic": false}} {
		m := moduleFromYAML(t, reuseSrc, cfg)
		static := cfg["allow_dynamic"] == false
		fill := func(s string) string {
			if static {
				return "let _ = " + s + ";"
			}
			return s + ";"
		}
		wantAll(t, cfg, m, "the public API",
			"pub fn try_decode(data: &[u8], out: &mut Self) -> Result<(), DecodeError> {",
			"_M__Decode::try_decode(data, out)",
			"pub fn decode(data: &[u8]) -> Self {",
			"pub fn reset(&mut self) {",
		)
		// reset(): in place, every kind -- a container is cleared and refilled
		// from its default (keeping its buffer), an aggregate reset recursively.
		wantAll(t, cfg, sliceFn(t, block(t, m, "impl M {"), "    pub fn reset("), "the message's reset() restores every default in place",
			"self.n = 5;",
			"self.s.clear();", fill(`self.s.push_str("ab")`),
			"self.b.clear();", fill("self.b.extend_from_slice(&[1, 2])"),
			"self.a.clear();", fill("self.a.extend_from_slice(&[1, 2])"),
			"self.ss.clear();", "self.objs.clear();", "self.mat.clear();", "self.rows.clear();",
			"self.st.reset();",
			"self.u._reset();",
		)
		// A union falls back to its default option in place when it holds it.
		wantAll(t, cfg, sliceFn(t, block(t, m, "impl M_U {"), "    fn _reset("), "a union resets its default option in place",
			"Self::D(v) => { v.reset(); }",
			"_ => *self = Self::default(),",
		)
		// try_decode re-arms, feeds, and cuts the reused arrays whatever the verdict.
		td := sliceFn(t, m, "    pub fn try_decode(data: &[u8], m: &mut M)")
		wantAll(t, cfg, td, "try_decode re-arms the destination and trims after the feed",
			"_rearm(m);", "m: &mut *m,", "lv = v.lv;", "_trim(m, lv);", "Ok(())")
		if strings.Index(td, "_trim(m, lv);") > strings.Index(td, "if invalid {") {
			t.Errorf("(%v) the trim must run before the verdict returns, so a refusal is trimmed too:\n%s", cfg, td)
		}
		// _rearm: reset() minus the reused arrays, walking into the struct member
		// that holds one (st) and resetting the one that does not (st.in) whole.
		rearm := sliceFn(t, m, "    fn _rearm(")
		wantAll(t, cfg, rearm, "_rearm resets everything but the reused arrays",
			"m.n = 5;", "m.s.clear();", "m.st.x = -1;", "m.st.r#in.reset();", "m.u._reset();")
		for _, bad := range []string{"m.ss.", "m.bs.", "m.objs.", "m.us.", "m.mat.", "m.rows.", "m.st.vals.", "m.st.reset()"} {
			if strings.Contains(rearm, bad) {
				t.Errorf("(%v) _rearm must leave a reused array's slots alone, found %q:\n%s", cfg, bad, rearm)
			}
		}
		// _trim covers exactly the fixed-path arrays, in live-slot order.
		trim := sliceFn(t, m, "    fn _trim(")
		wantAll(t, cfg, trim, "_trim cuts every reused array to its live count",
			"fn _trim(m: &mut M, lv: [usize; 7]) {",
			"sofab::seq::SeqVec::truncate(&mut m.ss, lv[0]);",
			"sofab::seq::SeqVec::truncate(&mut m.bs, lv[1]);",
			"sofab::seq::SeqVec::truncate(&mut m.objs, lv[2]);",
			"sofab::seq::SeqVec::truncate(&mut m.us, lv[3]);",
			"sofab::seq::SeqVec::truncate(&mut m.mat, lv[4]);",
			"sofab::seq::SeqVec::truncate(&mut m.rows, lv[5]);",
			"sofab::seq::SeqVec::truncate(&mut m.st.vals, lv[6]);",
		)
		// Seven slots, not ten: the array inside a struct element (objs.tags), the
		// one inside a union option (u.xs) and a wrapper row's own elements are
		// not fixed paths. They keep the reserve_* collectors and are reset with
		// their owner.
		wantAll(t, cfg, m, "an array inside an element or an option is reset with its owner",
			"sofab::seq::reserve_elem(&mut self.m.objs[self._ix0].tags, id,",
			"sofab::seq::reserve_elem(&mut (*self.m.u.xs_mut()), id,",
			"sofab::seq::reserve_elem(&mut self.m.rows[self._ix3], id,",
		)
		// The wrapper starts its live count over instead of clearing (§7.4).
		wantAll(t, cfg, m, "a wrapper occurrence restarts its live count",
			"(_Loc::Root, 4) => { self.lv[0] = 0; _Loc::Root_ss },",
			"(_Loc::Root_st, 1) => { self.lv[6] = 0; _Loc::Root_st_vals },",
		)
		wantAll(t, cfg, m, "the reuse collectors carry the element resets",
			"<M_Objs>::reset)",
			"<M_Us>::_reset)",
			"sofab::seq::reuse_row(&mut self.m.mat, &mut self.lv[4], id, sofab::seq::Bound::Schema(2))",
			"sofab::seq::reuse_row(&mut self.m.rows, &mut self.lv[5], id, sofab::seq::Bound::Schema(2))",
		)
		// The incremental decoder carries the live counts across feeds and trims
		// in finish; decode() trims too (a repeated wrapper restarts its count).
		wantAll(t, cfg, m, "the live counts survive between feeds",
			"lv: [usize; 7],", "lv: self.lv", "self.lv = lv;", "_trim(&mut self.m, self.lv);", "_trim(&mut m, lv);")
	}
}

// TestRustReuseArraysOnly: a message whose every field is a reused array has
// nothing to re-arm -- its live counts start at 0 and _trim cuts the rest -- so
// no _rearm is emitted or called; an empty one would leave its parameter unused,
// which a -D warnings build refuses.
func TestRustReuseArraysOnly(t *testing.T) {
	m := moduleFromYAML(t, `
version: 1
messages:
  m:
    payload:
      a: { id: 0, type: array, items: { type: string, count: 8, maxlen: 16 } }
`, map[string]any{"corelib": "rs"})
	if strings.Contains(m, "_rearm") {
		t.Errorf("an empty _rearm was emitted or called:\n%s", m)
	}
	td := sliceFn(t, m, "    pub fn try_decode(data: &[u8], m: &mut M)")
	if strings.Contains(td, "m.reset();") || !strings.Contains(td, "_trim(m, lv);") {
		t.Errorf("try_decode must rely on the live count and the trim alone:\n%s", td)
	}
}

// TestRustReuseEmitsNoStaticHelper: the element reuse is corelib-rs's
// (ARCHITECTURE §8) -- the generated code names a container, a live count and a
// reset, and holds no loop over slots, no truncation of its own and no reuse
// function. Mechanical check of the CLAUDE.md rule: no for-loop and no
// `.truncate(` outside the corelib call.
func TestRustReuseEmitsNoStaticHelper(t *testing.T) {
	m := moduleFromYAML(t, reuseSrc, map[string]any{"corelib": "rs"})
	vis := m[strings.Index(m, "mod _M__Decode {"):]
	if regexp.MustCompile(`\bfor \w+ in `).MatchString(vis) {
		t.Errorf("the decode module must not loop over slots itself")
	}
	if n, k := strings.Count(vis, "truncate("), strings.Count(vis, "sofab::seq::SeqVec::truncate("); n != k || k == 0 {
		t.Errorf("a truncate outside sofab::seq::SeqVec: %d of %d", n-k, n)
	}
	for _, bad := range []string{"fn _reuse", "fn reuse_", "slot_mut", "grow_to"} {
		if strings.Contains(m, bad) {
			t.Errorf("a static reuse helper was emitted: %q", bad)
		}
	}
}

// TestRustReuseHarness: the project harness decodes through the new signature,
// carries the reusedecode verb of check_reused_destination.py, and the bench
// measures the steady state -- on corelib-rs only.
func TestRustReuseHarness(t *testing.T) {
	main := func(cfg map[string]any) string {
		s := exampleSchema(t)
		if !isStdCfg(cfg) {
			s = exampleSchemaBounded(t)
		}
		cfg["emit"] = "project"
		files, err := (&Backend{}).Generate(s, cfg)
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range files {
			if f.Path == "src/main.rs" {
				return string(f.Content)
			}
		}
		t.Fatal("no src/main.rs")
		return ""
	}
	h := main(map[string]any{"corelib": "rs"})
	wantAll(t, nil, h, "the std harness",
		`} else if mode == "reusedecode" {`,
		"let mut obj = message::Myfirstmessage::decode(&held);",
		"pub fn run_decode_myfirstmessage(wire: &[u8], out: &mut message::Myfirstmessage) {",
		"message::Myfirstmessage::try_decode(&wire, &mut out).expect(\"decode\"); // setup",
		"run_decode_myfirstmessage(&wire, &mut out);",
	)
	n := main(map[string]any{"corelib": "rs-no-std"})
	if strings.Contains(n, "reusedecode") || !strings.Contains(n, "pub fn run_decode_myfirstmessage(wire: &[u8]) -> message::Myfirstmessage {") {
		t.Error("the no_std harness must keep its own decode surface")
	}
}

func TestSeqForRewritesOnlyCorelibRs(t *testing.T) {
	in := "if let Err(_e) = sofab::seq::reserve_elem(&mut self.m.objs, id, sofab::seq::Bound::Schema(4)) { x } sofab::seq::reserve_row(&mut self.m.mat, id, sofab::seq::Bound::Schema(2))"
	if got := seqFor(map[string]any{"corelib": "rs-no-std"}, in, "self.m.objs", 3, "<T>::reset"); got != in {
		t.Errorf("rs-no-std must be unchanged, got %q", got)
	}
	got := seqFor(map[string]any{}, in, "self.m.objs", 3, "<T>::reset")
	want := "if let Err(_e) = sofab::seq::reuse_elem(&mut self.m.objs, &mut self.lv[3], id, sofab::seq::Bound::Schema(4), <T>::reset) { x } sofab::seq::reserve_row(&mut self.m.mat, id, sofab::seq::Bound::Schema(2))"
	if got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
	if !strings.Contains(seqFor(map[string]any{}, in, "self.m.mat", 1, ""), "reuse_row(&mut self.m.mat, &mut self.lv[1], id, ") {
		t.Error("reserve_row must become reuse_row on corelib-rs")
	}
}
