package python

import (
	"strings"
	"testing"
)

// enumIntSchema puts an enum and a bitfield at every position a field can hold
// one: a scalar (with and without a declared default), an array element, a
// nested array's element, a struct member and a union option.
const enumIntSchema = `
version: 1
messages:
  M:
    payload:
      en:  { id: 0, type: enum, enum: { A: 0, B: 1, C: 2, Z: 10 }, default: 2 }
      ez:  { id: 1, type: enum, enum: { A: 0, B: 1 } }
      bf:  { id: 2, type: bitfield, bits: { A: { pos: 0, default: true }, B: { pos: 1 }, D: { pos: 3, default: true } } }
      aen: { id: 3, type: array, items: { type: enum, count: 4, enum: { A: 0, B: 1, Z: 10 } }, default: [1, 10] }
      abf: { id: 4, type: array, items: { type: bitfield, count: 3, bits: { A: { pos: 0 }, B: { pos: 1 } } } }
      st:
        id: 5
        type: struct
        fields:
          se:  { id: 0, type: enum, enum: { A: 0, B: 1, Z: 10 }, default: 10 }
          sbf: { id: 1, type: bitfield, bits: { A: { pos: 0 }, B: { pos: 1 } } }
      un:
        id: 6
        type: union
        default_id: 0
        oneof:
          ue:  { id: 0, type: enum, enum: { A: 0, B: 1, Z: 10 }, default: 1 }
          ubf: { id: 1, type: bitfield, bits: { A: { pos: 0 }, B: { pos: 1 } } }
      mat: { id: 7, type: array, items: { type: array, count: 2, items: { type: enum, count: 3, enum: { A: 0, B: 1 } } } }
`

// TestPythonEnumAndBitfieldFieldsAreInt pins an enum or bitfield FIELD to `int`
// at every position, with its default as the plain integer.
//
// MESSAGE_SPEC §1 bounds both kinds by their declared WIDTH, not by the constant
// set, so an in-width value no constant names is valid and the decoder stores it
// as the int it is. Annotating the field with the generated IntEnum claimed a
// closed set `E(m.f)` raises ValueError for, and a fresh message held an IntEnum
// member where a decoded one held an int. The class stays as the holder of the
// named constants, which the field's doc points to.
func TestPythonEnumAndBitfieldFieldsAreInt(t *testing.T) {
	mod := string(genPy(t, schema(t, enumIntSchema), map[string]any{})["message.py"])
	for _, want := range []string{
		// scalars: the annotation and the plain-int default
		"    #: Constants: :class:`M_En`.\n    en: int = 2\n",
		"    #: Constants: :class:`M_Ez`.\n    ez: int = 0\n",
		"    #: Constants: :class:`M_Bf`.\n    bf: int = 9\n",
		// arrays, flat and nested
		"    aen: list[int] = field(default_factory=lambda: [1, 10])\n",
		"    abf: list[int] = field(default_factory=list)\n",
		"    #: Constants: :class:`M_Mat`.\n    mat: list[list[int]] = field(default_factory=list)\n",
		// struct members
		"    #: Constants: :class:`M_St_Se`.\n    se: int = 10\n",
		"    #: Constants: :class:`M_St_Sbf`.\n    sbf: int = 0\n",
		// union options: the held default, the getter and the setter
		"    _value: object = 1\n",
		"    def ue(self) -> int:\n",
		"    def ue(self, v: int) -> None:\n",
		"    def ubf(self) -> int:\n",
		"    def ubf(self, v: int) -> None:\n",
		"        return self._value if self._which == 0 else 1\n",
		// the named constants are still there, on the right base
		"class M_En(_IntEnum):\n",
		"class M_Ez(_IntEnum):\n",
		"class M_Aen(_IntEnum):\n",
		"class M_Mat(_IntEnum):\n",
		"class M_St_Se(_IntEnum):\n",
		"class M_Un_Ue(_IntEnum):\n",
		"class M_Bf(_IntFlag):\n",
		"class M_Abf(_IntFlag):\n",
		"class M_St_Sbf(_IntFlag):\n",
		"class M_Un_Ubf(_IntFlag):\n",
		"    Z = 10\n",
		"    D = 1 << 3\n",
	} {
		if !strings.Contains(mod, want) {
			t.Errorf("message.py is missing %q", want)
		}
	}
	// Nothing annotates or builds a value with a holder class.
	for _, holder := range []string{"M_En", "M_Ez", "M_Bf", "M_Aen", "M_Abf", "M_Mat", "M_St_Se", "M_St_Sbf", "M_Un_Ue", "M_Un_Ubf"} {
		uses := strings.ReplaceAll(mod, "class "+holder+"(", "")
		for _, bad := range []string{": " + holder + " =", "[" + holder + "]", "-> " + holder + ":", "v: " + holder + ")", holder + "("} {
			if strings.Contains(uses, bad) {
				t.Errorf("message.py still uses the constant holder %s as a value type (%q)", holder, bad)
			}
		}
	}
	if strings.Contains(mod, "__Enum") {
		t.Error("message.py still emits the enum alias a member default needed")
	}
}
