package python

import (
	"strings"
	"testing"
)

// The destination table (binding.go) takes a field off the visitor entirely: the
// decoder writes it into a slot and calls nothing. What it may take is decided by
// two rules, and the tests below pin both, because breaking either is silent --
// a width that stops being checked, or a value that lands in the wrong field.

// TestPythonBindsOnlyWhatTheTableCanCarry: rule 1. An entry carries a declared
// width for an ARRAY's elements and none for a scalar, so every kind whose
// declared width is narrower than the 64-bit slot keeps its store in the typed
// hook, where the §7.1 guard runs at the value rather than after the decode.
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
`
	mod := string(genPy(t, schema(t, src), map[string]any{})["message.py"])

	for _, want := range []string{
		// A 64-bit integer, both floats, a boolean (§4.4: no width at all), a
		// string and a blob: nothing to check per value, so the table takes them.
		"    .unsigned(0, at=0, count_at=1)",
		"    .signed(1, at=2, count_at=3)",
		"    .float32(2, at=4, count_at=5)",
		"    .float64(3, at=6, count_at=7)",
		"    .boolean(4, at=8, count_at=9)",
		"    .string(5, at=0, maxlen=8, count_at=10)",
		"    .bytes(6, at=1, maxlen=0, count_at=11)",
		// A COUNTED native array: the destination is `cap` slots, and the declared
		// element width rides the entry, where the decoder applies it at each
		// element.
		"    .unsigned_array(11, at=12, cap=4, count_at=16, elem_max=65535)",
	} {
		if !strings.Contains(mod, want) {
			t.Errorf("message.py missing table row %q:\n%s", want, mod)
		}
	}

	// The narrow kinds keep their guarded store in the typed hook.
	for _, want := range []string{
		`raise SofaDecodeError("narrow: value outside declared width u8")`,
		`raise SofaDecodeError("narrowi: value outside declared width i16")`,
		`raise SofaDecodeError("en: value outside declared enum width")`,
		`raise SofaDecodeError("bf: value outside declared bitfield width")`,
	} {
		if !strings.Contains(mod, want) {
			t.Errorf("a narrow kind must keep its §7.1 guard in the hook, missing %q:\n%s", want, mod)
		}
	}
	for _, gone := range []string{
		".unsigned(7,", ".signed(8,", ".signed(9,", ".unsigned(10,",
		// An array the schema leaves unbounded has no destination to declare: the
		// slots are `cap` wide and the wire may not choose that number (§6.6).
		".unsigned_array(12,",
		// A wrapper array's elements are sequence-framed, per index.
		"(13,",
	} {
		if strings.Contains(mod, gone) {
			t.Errorf("the table must not carry %q:\n%s", gone, mod)
		}
	}
}

// TestPythonBindsNestedScopeOnlyWhenTheWholeTreeIs: rule 2, the one that decides
// where a value lands.
//
// The decoder descends into a bound sequence without telling the visitor
// (corelib-py#146), so while the walk is inside the child the visitor's `_c`
// still names the PARENT. An id the child's table does not name -- an unknown
// one, which is what forward compatibility delivers -- is then offered to the
// visitor under the parent's location. That is safe only when the parent has no
// arms at all, i.e. when it binds everything it declares.
func TestPythonBindsNestedScopeOnlyWhenTheWholeTreeIs(t *testing.T) {
	const whole = `
version: 1
messages:
  M:
    payload:
      when:  { id: 0, type: struct, fields: { sec: { id: 0, type: u64 }, frac: { id: 1, type: fp64 } } }
      where: { id: 1, type: struct, fields: { lat: { id: 0, type: fp64 }, lon: { id: 1, type: fp64 } } }
      name:  { id: 2, type: string, maxlen: 8 }
`
	mod := string(genPy(t, schema(t, whole), map[string]any{})["message.py"])
	// The message's OWN visitor, which is where the dispatch would survive; the
	// two inline structs keep visitors of their own for decoding them standalone.
	vis := mod[strings.Index(mod, "class _MVisitor("):]
	for _, want := range []string{
		// Every field is bindable, so the tree is bound down to the leaves...
		"    .sequence(0, child=_BIND_M_when)",
		"    .sequence(1, child=_BIND_M_where)",
		"_BIND_M_when = (Binding()",
		"        if U[1] != _ABSENT: m.when.sec = U[0]",
		// ...and the message visitor has nothing left to dispatch: every id that
		// can reach it, at any depth, is one it does not declare.
		"                return False",
	} {
		if !strings.Contains(mod, want) {
			t.Errorf("a fully bindable tree must bind its nested scopes, missing %q:\n%s", want, mod)
		}
	}
	for _, gone := range []string{
		// No location exists for a scope the table enters, so nothing can dispatch
		// against it...
		"_L_M_when = ", "_L_M_where = ",
	} {
		if strings.Contains(mod, gone) {
			t.Errorf("a bound scope must leave no location behind: %q:\n%s", gone, mod)
		}
	}
	// ...and the message's visitor keeps no arm for any of it: no typed hook, and
	// no sequence arm to enter a scope the table already entered.
	for _, gone := range []string{
		"    def on_float64(self, fid: int, value: float) -> None:",
		"    def on_unsigned(self, fid: int, value: int) -> None:",
		"    def on_string(self, fid: int, value: str) -> None:",
		"        if c == _L_M:",
	} {
		if strings.Contains(vis, gone) {
			t.Errorf("a bound scope must leave no visitor dispatch behind: %q:\n%s", gone, vis)
		}
	}

	// One wrapper array is enough to stop the descent: the message scope now has
	// an arm of its own, so an unknown id arriving inside a bound child could be
	// mistaken for it.
	const mixed = `
version: 1
messages:
  M:
    payload:
      when:  { id: 0, type: struct, fields: { sec: { id: 0, type: u64 }, frac: { id: 1, type: fp64 } } }
      where: { id: 1, type: struct, fields: { lat: { id: 0, type: fp64 }, lon: { id: 1, type: fp64 } } }
      name:  { id: 2, type: string, maxlen: 8 }
      tags:  { id: 3, type: array, items: { type: string, count: 4 } }
      seq:   { id: 4, type: u64 }
      ratio: { id: 5, type: fp64 }
`
	mod = string(genPy(t, schema(t, mixed), map[string]any{})["message.py"])
	if strings.Contains(mod, ".sequence(") {
		t.Errorf("a scope with an unbindable field must not be descended into:\n%s", mod)
	}
	for _, want := range []string{
		// Its own leaves are still bound...
		"    .string(2, at=0, maxlen=8, count_at=0)",
		// ...and the two structs go back to the visitor, locations and all.
		"_L_M_when = ", "_L_M_where = ",
		"    def on_float64(self, fid: int, value: float) -> None:",
	} {
		if !strings.Contains(mod, want) {
			t.Errorf("a partially bindable message must still bind its own leaves, missing %q:\n%s", want, mod)
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
      b: { id: 1, type: u8 }
      c: { id: 2, type: u16 }
      d: { id: 3, type: i8 }
`
	mod := string(genPy(t, schema(t, src), map[string]any{})["message.py"])
	for _, gone := range []string{"Binding", "_ABSENT", "def scatter", "def destinations"} {
		if strings.Contains(mod, gone) {
			t.Errorf("a class with %d bindable fields must emit no table, found %q:\n%s",
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
		"        if U[10] != _ABSENT: m.somestring = OB[0]",
	} {
		if !strings.Contains(mod, want) {
			t.Errorf("message.py missing %q:\n%s", want, mod)
		}
	}
}
