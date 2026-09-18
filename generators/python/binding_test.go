package python

import (
	"strings"
	"testing"
)

// The destination table (binding.go) takes a field off the visitor entirely: the
// decoder writes it into a slot and calls nothing. What it may take is decided by
// two rules, and the tests below pin both, because breaking either is silent --
// a width that stops being checked, or a value that lands in the wrong field.

// TestPythonBindsOnlyWhatTheTableCanCarry: what is left off the table is left off
// for a SHAPE it has no entry for, never for a rule it cannot apply -- every
// declared width now rides the entry (corelib-py#149) and the decoder checks it at
// the value, where the visitor's guard used to.
func TestPythonBindsOnlyWhatTheTableCanCarry(t *testing.T) {
	const src = `
version: 1
$defs:
  enum:
    Small: { A: { value: 0 }, B: { value: 3 } }
  bitfield:
    Few: { X: { pos: 0 }, Y: { pos: 2 } }
messages:
  M:
    payload:
      wide_u:  { id: 0, type: u64 }
      wide_i:  { id: 1, type: i64 }
      f32:     { id: 2, type: fp32 }
      f64:     { id: 3, type: fp64 }
      flag:    { id: 4, type: boolean }
      text:    { id: 5, type: string, maxlen: 8 }
      raw:     { id: 6, type: blob }
      narrow:  { id: 7, type: u8 }
      narrowi: { id: 8, type: i16 }
      en:      { id: 9, type: enum, enum: { $ref: "#/$defs/enum/Small" } }
      bf:      { id: 10, type: bitfield, bits: { $ref: "#/$defs/bitfield/Few" } }
      counted: { id: 11, type: array, items: { type: u16, count: 4 } }
      dyn:     { id: 12, type: array, items: { type: u16 } }
      wrapped: { id: 13, type: array, items: { type: string, count: 2 } }
      wide:    { id: 14, type: array, items: { type: u8, count: 4096 } }
`
	mod := string(genPy(t, schema(t, src), map[string]any{})["message.py"])

	for _, want := range []string{
		// A 64-bit integer, both floats, a boolean (§4.4: no width at all), a
		// string and a blob: nothing to state, so the entry states nothing.
		"    .unsigned(0, at=0, count_at=1)",
		"    .signed(1, at=2, count_at=3)",
		"    .float32(2, at=4, count_at=5)",
		"    .float64(3, at=6, count_at=7)",
		"    .boolean(4, at=8, count_at=9)",
		"    .string(5, at=0, maxlen=8, count_at=10)",
		"    .bytes(6, at=1, maxlen=0, count_at=11)",
		// Every NARROW width, on the entry: the declared one for an integer, the
		// implied one for an enum (smallest signed type holding every constant)
		// and for a bitfield (smallest unsigned type holding the highest `pos`).
		"    .unsigned(7, at=12, count_at=13, max_value=255)",
		"    .signed(8, at=14, count_at=15, min_value=-32768, max_value=32767)",
		"    .signed(9, at=16, count_at=17, min_value=-128, max_value=127)",
		"    .unsigned(10, at=18, count_at=19, max_value=255)",
		// A COUNTED native array: the destination is `cap` slots, and the element
		// width rides the entry, where the decoder applies it at each element.
		"    .unsigned_array(11, at=20, cap=4, count_at=24, elem_max=65535)",
	} {
		if !strings.Contains(mod, want) {
			t.Errorf("message.py missing table row %q:\n%s", want, mod)
		}
	}

	// A narrow kind no longer carries a guard in the hook: the entry states it and
	// the decoder applies it, so a second comparison would be the two routes to one
	// rule §5.3.1 forbids.
	for _, gone := range []string{
		`raise SofaDecodeError("narrow: value outside declared width u8")`,
		`raise SofaDecodeError("en: value outside declared enum width")`,
		// An array the schema leaves unbounded has no destination to declare: the
		// slots are `cap` wide and the wire may not choose that number (§6.6).
		".unsigned_array(12,",
		// A wrapper array's elements are sequence-framed, per index.
		"(13,",
		// And one whose declared count is past pyBindArrayMax: a table
		// materializes an array twice, so a big one is a measured loss.
		".unsigned_array(14,",
	} {
		if strings.Contains(mod, gone) {
			t.Errorf("the table must not carry %q:\n%s", gone, mod)
		}
	}
}

// TestPythonBindsNestedScopeOnlyWhenItsSubtreeIs: the rule that decides where a
// value lands.
//
// The decoder descends into a bound sequence without telling the visitor
// (corelib-py#146), so while the walk is inside the child the visitor's `_c` still
// names the PARENT -- and an id the child's table does not name would be offered
// to it under the parent's location. `Binding(closed=True)` (corelib-py#150) is
// what makes that impossible: a closed table skips what it does not name, sequence
// and all. So a scope may be entered exactly when its whole subtree is bindable,
// which is exactly when its table can be closed.
func TestPythonBindsNestedScopeOnlyWhenItsSubtreeIs(t *testing.T) {
	const src = `
version: 1
messages:
  M:
    payload:
      when:  { id: 0, type: struct, fields: { sec: { id: 0, type: u64 }, frac: { id: 1, type: fp64 } } }
      where: { id: 1, type: struct, fields: { lat: { id: 0, type: fp64 }, lon: { id: 1, type: fp64 } } }
      name:  { id: 2, type: string, maxlen: 8 }
      mixed: { id: 3, type: struct, fields: { k: { id: 0, type: u64 }, tags: { id: 1, type: array, items: { type: string, count: 2 } } } }
      tags:  { id: 4, type: array, items: { type: string, count: 4 } }
`
	mod := string(genPy(t, schema(t, src), map[string]any{})["message.py"])
	for _, want := range []string{
		// Two subtrees are bindable whole, so both are entered and both tables are
		// closed -- which is the promise that nothing inside them needs a visitor.
		"_BIND_M_when = (Binding(closed=True)",
		"_BIND_M_where = (Binding(closed=True)",
		"    .sequence(0, child=_BIND_M_when)",
		"    .sequence(1, child=_BIND_M_where)",
		"        if U[1] != _ABSENT: m.when.sec = U[0]",
		// The message's own table is NOT closed: it still has fields of its own
		// that only the visitor can handle, so an id it does not name has to reach
		// the visitor as before.
		"_BIND_M = (Binding()",
	} {
		if !strings.Contains(mod, want) {
			t.Errorf("a bindable subtree must be entered and closed, missing %q:\n%s", want, mod)
		}
	}
	for _, gone := range []string{
		// `mixed` holds a wrapper array, so its subtree cannot be closed and the
		// table must not descend into it -- an unknown id inside would otherwise
		// be offered to the visitor under the MESSAGE's location.
		"    .sequence(3,",
		"_BIND_M_mixed",
		// ...and the two scopes the table entered leave no dispatch behind.
		"_L_M_when = ", "_L_M_where = ",
	} {
		if strings.Contains(mod, gone) {
			t.Errorf("a scope with an unbindable field must stay on the visitor, found %q:\n%s", gone, mod)
		}
	}
	// `mixed` keeps its location, its own scalar's store and its wrapper array.
	for _, want := range []string{"_L_M_mixed = ", "_L_M_mixed_tags = "} {
		if !strings.Contains(mod, want) {
			t.Errorf("the unbindable subtree must keep its dispatch, missing %q:\n%s", want, mod)
		}
	}
}

// TestPythonClosedTableLeavesNoVisitor: when the table covers every id at every
// depth, the codec skips whatever is not on it -- so the class carries the
// storage, the declaration and the scatter, and not one hook.
func TestPythonClosedTableLeavesNoVisitor(t *testing.T) {
	const src = `
version: 1
messages:
  M:
    payload:
      when: { id: 0, type: struct, fields: { sec: { id: 0, type: u64 }, frac: { id: 1, type: fp64 } } }
      name: { id: 1, type: string, maxlen: 8 }
      n:    { id: 2, type: u8 }
`
	mod := string(genPy(t, schema(t, src), map[string]any{})["message.py"])
	if !strings.Contains(mod, "_BIND_M = (Binding(closed=True)") {
		t.Errorf("a class the table covers whole must close its root table:\n%s", mod)
	}
	vis := mod[strings.Index(mod, "class _MVisitor("):]
	for _, gone := range []string{
		"def on_", "self._c", "self._s", "_L_M",
	} {
		if strings.Contains(vis, gone) {
			t.Errorf("a closed table leaves no dispatch behind, found %q:\n%s", gone, vis)
		}
	}
	for _, want := range []string{"def destinations(self):", "def scatter(self) -> None:"} {
		if !strings.Contains(vis, want) {
			t.Errorf("the handler still declares and scatters, missing %q:\n%s", want, vis)
		}
	}
}

// TestPythonTableBelowMinimumIsNotEmitted: a table costs an allocation and a
// scatter per decode, so a class binding almost nothing keeps today's shape.
func TestPythonTableBelowMinimumIsNotEmitted(t *testing.T) {
	const src = `
version: 1
messages:
  M:
    payload:
      a: { id: 0, type: u64 }
      b: { id: 1, type: array, items: { type: u32 } }
      c: { id: 2, type: array, items: { type: string, count: 2 } }
`
	mod := string(genPy(t, schema(t, src), map[string]any{})["message.py"])
	for _, gone := range []string{"Binding", "_ABSENT", "def scatter", "def destinations"} {
		if strings.Contains(mod, gone) {
			t.Errorf("a class with %d bindable field must emit no table, found %q:\n%s",
				pyBindMin-2, gone, mod)
		}
	}
	// ...and the one bindable field keeps its store in the hook.
	if !strings.Contains(mod, "                self._o.a = value") {
		t.Errorf("the unbound field must still be stored by the visitor:\n%s", mod)
	}
}

// TestPythonScatterRunsOnlyOnAComplete: the slots are moved onto the message
// when the decode COMPLETES and never before. A decode that raises builds
// nothing, so a refused message cannot leave half a value behind.
func TestPythonScatterRunsOnlyOnAComplete(t *testing.T) {
	mod := string(genPy(t, schemaFile(t, "../../examples/messages/example.yaml"), map[string]any{})["message.py"])
	for _, want := range []string{
		// one-shot: after both refusals
		`        if st is Status.INCOMPLETE:
            raise SofaIncompleteError(d.error or "truncated message")`,
		"        v.scatter()\n        return o",
		// streaming: per feed, gated on the outcome of that feed
		"        st = self._d.feed(chunk)\n        if st is Status.COMPLETE:",
		// the arrival test, and the prefill that makes it mean something
		"_ABSENT = 0xFFFFFFFFFFFFFFFF",
		`_FILL_Myfirstmessage = b"\xff" * (_W_Myfirstmessage * 8)`,
		"        self._w = bytearray(_FILL_Myfirstmessage)",
		"        if U[22] != _ABSENT: m.somestring = OB[0]",
	} {
		if !strings.Contains(mod, want) {
			t.Errorf("message.py missing %q:\n%s", want, mod)
		}
	}
}

// TestPythonScopeNamesAreUnique: a scope is named after the PATH that reaches it,
// and two different paths can spell one name -- a field `a` whose struct has a
// field `b`, beside a sibling field `a_b`. Both the dispatch location and the
// destination table are named from it, and a duplicate is not a cosmetic clash:
// the second module-level assignment wins, so two scopes would share one location
// constant (and one Binding), and one of them would decode into the other's
// destination with no error anywhere.
//
// Measured before the fix, on exactly this schema: `a.b` decoded the sibling's
// values and the sibling decoded nothing -- on both engines, with and without a
// destination table.
func TestPythonScopeNamesAreUnique(t *testing.T) {
	const src = `
version: 1
$defs:
  struct:
    Leaf: { p: { id: 0, type: u64 }, q: { id: 1, type: fp64 } }
    Inner: { b: { id: 0, type: struct, fields: { $ref: '#/$defs/struct/Leaf' } } }
messages:
  M:
    payload:
      a:   { id: 0, type: struct, fields: { $ref: '#/$defs/struct/Inner' } }
      a_b: { id: 1, type: struct, fields: { $ref: '#/$defs/struct/Leaf' } }
      z:   { id: 2, type: u64 }
`
	mod := string(genPy(t, schema(t, src), map[string]any{})["message.py"])
	for _, decl := range []string{"_BIND_M_a_b = ", "_BIND_M_a_b_2 = ", "_L_M_a_b = ", "_L_M_a_b_2 = "} {
		if n := strings.Count(mod, decl); n > 1 {
			t.Errorf("%q is declared %d times -- the later one wins and two scopes share it:\n%s",
				decl, n, mod)
		}
	}
	// Both halves of the pair exist, so the two scopes really are distinguished
	// rather than one of them having been dropped.
	for _, want := range []string{"_BIND_M_a_b = ", "_BIND_M_a_b_2 = "} {
		if !strings.Contains(mod, want) {
			t.Errorf("missing %q -- the colliding scopes must BOTH get a table:\n%s", want, mod)
		}
	}
}
