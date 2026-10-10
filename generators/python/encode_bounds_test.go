package python

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// encodeBoundsSchema declares a bound at every position serialize() writes one:
// narrow integers of both signs, an enum and a bitfield, a string and a blob, a
// native array, a wrapper array of strings and one of blobs, an array of
// structs, an array-of-arrays row, a nested struct, and union options (the
// default one, written under its != default branch, and a forced one, written
// whatever its value). Its unbounded and 64-bit twins state no bound.
const encodeBoundsSchema = `
version: 1
messages:
  M:
    payload:
      u8:   { id: 0, type: u8 }
      u16:  { id: 1, type: u16 }
      u32:  { id: 2, type: u32 }
      u64:  { id: 3, type: u64 }
      i8:   { id: 4, type: i8 }
      i16:  { id: 5, type: i16 }
      i32:  { id: 6, type: i32 }
      i64:  { id: 7, type: i64 }
      col:  { id: 8, type: enum, enum: { RED: 0, GREEN: 1, BLUE: 300 } }
      fl:   { id: 9, type: bitfield, bits: { a: { pos: 0 }, b: { pos: 9 } } }
      s:    { id: 10, type: string, maxlen: 6 }
      us:   { id: 11, type: string }
      b:    { id: 12, type: blob, maxlen: 5 }
      ub:   { id: 13, type: blob }
      au:   { id: 14, type: array, items: { type: u32, count: 3 } }
      uau:  { id: 15, type: array, items: { type: u32 } }
      as:   { id: 16, type: array, items: { type: string, count: 2, maxlen: 3 } }
      ab:   { id: 17, type: array, items: { type: blob, count: 2, maxlen: 4 } }
      ast:  { id: 18, type: array, items: { type: struct, count: 2, fields: { x: { id: 0, type: i8 } } } }
      rows:
        id: 19
        type: array
        items: { type: array, count: 2, items: { type: u16, count: 3 } }
      srows:
        id: 20
        type: array
        items: { type: array, count: 2, items: { type: string, count: 2, maxlen: 2 } }
      n:    { id: 21, type: struct, fields: { arr: { id: 0, type: array, items: { type: u16, count: 2 } } } }
      u:
        id: 22
        type: union
        default_id: 0
        oneof:
          small: { id: 0, type: u8 }
          name:  { id: 1, type: string, maxlen: 4 }
          bl:    { id: 2, type: blob, maxlen: 2 }
          arr:   { id: 3, type: array, items: { type: i16, count: 2 } }
      au8:  { id: 23, type: array, items: { type: u8, count: 3 } }
      ai8:  { id: 24, type: array, items: { type: i8 } }
      aen:  { id: 25, type: array, items: { type: enum, count: 4, enum: { A: 0, B: 1 } } }
      abf:  { id: 26, type: array, items: { type: bitfield, bits: { a: { pos: 0 }, b: { pos: 9 } } } }
      abo:  { id: 27, type: array, items: { type: boolean, count: 2 } }
      af:   { id: 28, type: array, items: { type: fp32, count: 2 } }
      a64:  { id: 29, type: array, items: { type: i64, count: 2 } }
      ua64: { id: 30, type: array, items: { type: u64 } }
      bd:   { id: 31, type: blob, maxlen: 3, default: "AQI=" }
      cd:   { id: 32, type: enum, enum: { X: 0, Y: -2 }, default: -2 }
`

// TestPythonEncodeBoundGuards pins the shape of every encode-side refusal. The
// bound is the schema literal, per field. Where a corelib writer takes the value
// whole, a schema maxlen or count rides that call (write_string_bounded,
// write_bytes_bounded, an array's cap), and a declared width is the writer's name
// (write_u8 .. write_i32 for a narrow integer, enum or bitfield, write_u8_array ..
// write_i64_array for an integer array) -- and the writer refuses with
// SofaArgumentError. A wrapper
// array has no writer that takes it whole, so its count is the one generated
// `if ...: raise SofaArgumentError(...)`, on the length the element loop's
// last-index test already reads. Where the schema states no bound, or the
// bound is the 64-bit range every writer already refuses past, the plain writer
// is called, unchanged.
func TestPythonEncodeBoundGuards(t *testing.T) {
	mod := string(genPy(t, schema(t, encodeBoundsSchema), map[string]any{})["message.py"])
	m := classBody(t, mod, "M")
	for _, want := range []string{
		// Narrow integers: the same numbers the decode side hands the destination
		// table as max_value / min_value.
		"        if self.u8 != 0:\n            e.write_u8(0, self.u8)\n",
		"            e.write_u16(1, self.u16)\n",
		"            e.write_u32(2, self.u32)\n",
		"            e.write_i8(4, self.i8)\n",
		"            e.write_i16(5, self.i16)\n",
		"            e.write_i32(6, self.i32)\n",
		// enum: the smallest SIGNED width holding every constant (300 -> i16);
		// bitfield: the smallest UNSIGNED width holding the highest pos (9 -> u16).
		"            e.write_i16(8, self.col)\n",
		"            e.write_u16(9, self.fl)\n",
		// String and blob: maxlen as the writer's third argument.
		"            e.write_string_bounded(10, self.s, 6)\n",
		"        if self.b != b\"\":\n            e.write_bytes_bounded(12, self.b, 5)\n",
		// Native array: the count as `cap`.
		"        if len(self.au) != 0:\n            e.write_u32_array(14, self.au, 3)\n",
		// Wrapper arrays: the count before the frame opens, each element's maxlen
		// at its write.
		"        _n0 = len(self.as_)\n        if _n0 > 2:\n            raise SofaArgumentError(\"as: array over count 2\")\n        e.write_sequence_begin_lazy(16)",
		"                e.write_string_bounded(_i0, _e0, 3)\n",
		"        _n0 = len(self.ab)\n        if _n0 > 2:\n            raise SofaArgumentError(\"ab: array over count 2\")\n",
		"                e.write_bytes_bounded(_i0, _e0, 4)\n",
		"        _n0 = len(self.ast)\n        if _n0 > 2:\n            raise SofaArgumentError(\"ast: array over count 2\")\n",
		// Array of arrays: the outer count, then each row's own -- a native row as
		// its writer's cap, a string row as a guard plus its elements' maxlen.
		"        _n0 = len(self.rows)\n        if _n0 > 2:\n            raise SofaArgumentError(\"rows: array over count 2\")\n",
		"            if len(_e0) != 0 or _i0 == _n0 - 1:\n                e.write_u16_array(_i0, _e0, 3)\n",
		"            _n1 = len(_e0)\n            if _n1 > 2:\n                raise SofaArgumentError(\"srows: array over count 2\")\n",
		"                    e.write_string_bounded(_i1, _e1, 2)\n",
		// Native arrays of integers, enums and bitfields: the element's width
		// next to the cap ("None" where the schema states no count) -- the same
		// numbers the decode side hands the destination table as elem_min/elem_max.
		"e.write_u8_array(23, self.au8, 3)\n",
		"e.write_i8_array(24, self.ai8, -1)\n",
		"e.write_i8_array(25, self.aen, 4)\n",
		"e.write_u16_array(26, self.abf, -1)\n",
		// No width: a boolean (§4.4), a float, a 64-bit element -- only the cap.
		"e.write_bool_array_bounded(27, self.abo, 2)\n",
		"e.write_float32_array_bounded(28, self.af, 2)\n",
		"e.write_i64_array(29, self.a64, 2)\n",
		// No count, but a u32 element: the width alone.
		"e.write_u32_array(15, self.uau, -1)\n",
	} {
		if !strings.Contains(m, want) {
			t.Errorf("class M missing encode bound:\n%s\n--- in ---\n%s", want, m)
		}
	}
	// No bound, the plain writer: 64-bit integers, an unbounded string and blob, a
	// u64 array without a count.
	for _, want := range []string{
		"e.write_unsigned(3, self.u64)\n",
		"e.write_signed(7, self.i64)\n",
		"e.write_string(11, self.us)\n",
		"e.write_bytes(13, self.ub)\n",
		"e.write_unsigned_array(30, self.ua64)\n",
	} {
		if !strings.Contains(m, want) {
			t.Errorf("class M bounds an unbounded field, want %q:\n%s", want, m)
		}
	}
	// A nested struct's own serialize() carries its bound, so every caller of it
	// is covered -- a struct-array element's member included.
	if n := classBody(t, mod, "M_N"); !strings.Contains(n, "e.write_u16_array(0, self.arr, 2)\n") {
		t.Errorf("nested struct M_N missing its cap:\n%s", n)
	}
	if st := classBody(t, mod, "M_Ast"); !strings.Contains(st, "e.write_i8(0, self.x)\n") {
		t.Errorf("struct array element M_Ast missing its width:\n%s", st)
	}
	// Union options: the default option under its != default branch, a forced
	// option written whatever its value -- the bound rides both.
	u := classBody(t, mod, "M_U")
	for _, want := range []string{
		"            if _v != 0:\n                e.write_u8(0, _v)\n",
		"            e.write_string_bounded(1, _v, 4)\n",
		"            e.write_bytes_bounded(2, _v, 2)\n",
		"            e.write_i16_array(3, _v, 2)\n",
	} {
		if !strings.Contains(u, want) {
			t.Errorf("union M_U missing encode bound:\n%s\n--- in ---\n%s", want, u)
		}
	}
	if !strings.Contains(mod, "SofaArgumentError, SofaDecodeError,") {
		t.Errorf("a module that raises SofaArgumentError must import it:\n%s", mod[:400])
	}
}

// TestPythonEncodeBoundImportFollowsUse: SofaArgumentError is imported only where
// a generated guard raises it (a wrapper array's count). Every other bound rides a
// writer, which raises it itself, so a schema without a counted wrapper array
// neither raises nor imports it -- an unused import is a pyflakes finding in the
// user's tree.
func TestPythonEncodeBoundImportFollowsUse(t *testing.T) {
	const src = `
version: 1
messages:
  M:
    payload:
      a: { id: 0, type: u8 }
      b: { id: 1, type: i16 }
      s: { id: 2, type: string, maxlen: 4 }
      l: { id: 3, type: blob, maxlen: 4 }
      r: { id: 4, type: array, items: { type: u8, count: 3 } }
      w: { id: 5, type: array, items: { type: string } }
`
	mod := string(genPy(t, schema(t, src), map[string]any{})["message.py"])
	if strings.Contains(mod, "raise SofaArgumentError(") || strings.Contains(mod, "SofaArgumentError,") ||
		strings.Contains(mod, ", SofaArgumentError\n") {
		t.Errorf("nothing to guard, yet SofaArgumentError is raised or imported:\n%s", mod)
	}
}

// TestPythonEncodeBoundsRefuseAtRuntime runs the guards against the live
// corelib, on both engines (the native one only where it is built, and skipped
// out loud otherwise): every over-bound value raises SofaArgumentError out
// of encode(), and every value exactly at its bound encodes and decodes back.
// The structural test above pins where the guards are; this pins that they fire
// -- including at the positions the shared conformance probe does not reach
// (union options, array rows, blob elements, a struct array's element).
func TestPythonEncodeBoundsRefuseAtRuntime(t *testing.T) {
	corelib := os.Getenv("SOFAB_PY_CORELIB")
	if corelib == "" {
		t.Skip("set SOFAB_PY_CORELIB to a corelib-py checkout")
	}
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not found")
	}
	dir := t.TempDir()
	for path, content := range genPy(t, schema(t, encodeBoundsSchema), map[string]any{}) {
		if err := os.WriteFile(filepath.Join(dir, path), content, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	const driver = `
import sys
import sofab
from sofab import SofaArgumentError
from message import M, M_Ast, M_N, M_U

def at_bound():
    m = M(u8=255, u16=65535, u32=4294967295, i8=-128, i16=32767, i32=-2147483648,
          col=-32768, fl=0xffff, s="ééé", b=b"12345", au=[1, 2, 3],
          as_=["abc", "é"], ab=[b"1234", b""], ast=[M_Ast(x=127), M_Ast(x=-128)],
          rows=[[1, 2, 3], [65535]], srows=[["ab", "é"], ["x"]], n=M_N(arr=[1, 2]),
          au8=[255, 0, 1], ai8=[-128, 127], aen=[127, -128], abf=[0xffff],
          abo=[True, False], af=[1.5, 2.5], a64=[-(1 << 63), (1 << 63) - 1])
    m.u.small = 255
    return m

def over():
    cases = {
        "u8": dict(u8=256), "u16": dict(u16=65536), "u32": dict(u32=1 << 32),
        "i8 high": dict(i8=128), "i8 low": dict(i8=-129), "i16": dict(i16=-32769),
        "i32": dict(i32=1 << 31), "enum": dict(col=32768), "bitfield": dict(fl=0x10000),
        "bitfield negative": dict(fl=-1), "string": dict(s="ééé!"),
        "blob": dict(b=b"123456"), "array": dict(au=[1, 2, 3, 4]),
        "string array count": dict(as_=["a", "b", "c"]),
        "string array element": dict(as_=["abcd"]),
        "string array element midchar": dict(as_=["abé"]),
        "blob array count": dict(ab=[b"", b"", b"x"]),
        "blob array element": dict(ab=[b"12345"]),
        "struct array count": dict(ast=[M_Ast(), M_Ast(), M_Ast()]),
        "struct array element width": dict(ast=[M_Ast(x=200)]),
        "rows count": dict(rows=[[1], [2], [3]]),
        "row count": dict(rows=[[1, 2, 3, 4]]),
        "row count interior": dict(rows=[[1, 2, 3, 4], []]),
        "string rows count": dict(srows=[["a", "b", "c"]]),
        "string row element": dict(srows=[["abc"]]),
        "nested": dict(n=M_N(arr=[1, 2, 3])),
        "u8 array element": dict(au8=[1, 300]),
        "i8 array element": dict(ai8=[-129]),
        "enum array element": dict(aen=[200]),
        "enum array element low": dict(aen=[-129]),
        "bitfield array element": dict(abf=[0x10000]),
        "bitfield array element negative": dict(abf=[-1]),
        "row element width": dict(rows=[[70000]]),
        "bool array count": dict(abo=[True, False, True]),
        "float array count": dict(af=[1.0, 2.0, 3.0]),
        "i64 array count": dict(a64=[1, 2, 3]),
    }
    for name, kw in cases.items():
        yield name, M(**kw)
    for name, sel in (("union default option", lambda u: setattr(u, "small", 256)),
                      ("union string", lambda u: setattr(u, "name", "abcde")),
                      ("union blob", lambda u: setattr(u, "bl", b"123")),
                      ("union array", lambda u: setattr(u, "arr", [1, 2, 3])),
                      ("union array element", lambda u: setattr(u, "arr", [40000]))):
        m = M()
        sel(m.u)
        yield name, m

if sofab.IMPL != sys.argv[1]:
    sys.exit("engine %s, expected %s" % (sofab.IMPL, sys.argv[1]))
m = at_bound()
again = M.decode(m.encode())
if again != m:
    sys.exit("[%s] at-bound value does not round-trip:\n%r\n%r" % (sofab.IMPL, m, again))
bad = []
for name, m in over():
    try:
        out = m.encode()
    except SofaArgumentError:
        continue
    bad.append("%s: encoded %s" % (name, out.hex()))
if bad:
    sys.exit("[%s] not refused:\n  " % sofab.IMPL + "\n  ".join(bad))
print("%s ok" % sofab.IMPL)
`
	if err := os.WriteFile(filepath.Join(dir, "driver.py"), []byte(driver), 0o644); err != nil {
		t.Fatal(err)
	}
	// Each run pins its engine and the driver checks it ran on it: without a
	// built sofab._speedups the "native" run would silently be the pure one.
	env := func(pure string) []string {
		return append(os.Environ(),
			"PYTHONPATH="+filepath.Join(corelib, "src")+string(os.PathListSeparator)+dir,
			"SOFAB_PUREPYTHON="+pure)
	}
	probe := exec.Command(py, "-c", "import sofab; print(sofab.IMPL)")
	probe.Env = env("")
	impl, err := probe.Output()
	if err != nil {
		t.Fatalf("import sofab: %v", err)
	}
	runs := [][2]string{{"1", "python"}}
	if strings.TrimSpace(string(impl)) == "native" {
		runs = append(runs, [2]string{"", "native"})
	} else {
		t.Log("native engine: SKIPPED, sofab._speedups is not built in SOFAB_PY_CORELIB")
	}
	for _, r := range runs {
		cmd := exec.Command(py, filepath.Join(dir, "driver.py"), r[1])
		cmd.Dir = dir
		cmd.Env = env(r[0])
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s engine: %v\n%s", r[1], err, out)
		}
		t.Logf("%s engine: %s", r[1], strings.TrimSpace(string(out)))
	}
}

// TestPythonEncodeHandsValuesOverUncoerced pins that serialize() hands every
// integer and blob value to its writer as it is held: no int(...) around an
// integer scalar, element or union option, no per-element int() list built for
// an enum or bitfield array, no bytes(...) around a blob. The writer is what
// takes an IntEnum / IntFlag member as the integer it is and refuses a float;
// a coercion in front of it would truncate the float instead.
func TestPythonEncodeHandsValuesOverUncoerced(t *testing.T) {
	mod := string(genPy(t, schema(t, encodeBoundsSchema), map[string]any{})["message.py"])
	start := strings.Index(mod, "    def serialize(")
	if start < 0 {
		t.Fatal("no serialize() in the module")
	}
	// Every serialize() body, up to the module's visitor classes.
	enc := mod[start:]
	if end := strings.Index(enc, "class _StreamDecoder"); end > 0 {
		enc = enc[:end]
	}
	// An enum's omission test compares with the integer value, never with a
	// member built on every call (in serialize() and in _is_default alike).
	for _, want := range []string{"        if self.col != 0:\n", "        if self.cd != -2:\n",
		"        if not (self.col == 0):\n", "        if not (self.cd == -2):\n"} {
		if !strings.Contains(enc, want) {
			t.Errorf("missing %q", want)
		}
	}
	for _, line := range strings.Split(enc, "\n") {
		if strings.Contains(line, "if ") && strings.Contains(line, "__Enum(") {
			t.Errorf("an omission test builds an enum member per call: %s", strings.TrimSpace(line))
		}
		if strings.Contains(line, "if bytes(") {
			t.Errorf("a blob's omission test copies the value: %s", strings.TrimSpace(line))
		}
		if !strings.Contains(line, "e.write_") {
			continue
		}
		for _, bad := range []string{", int(", ", bytes(", ", [int(", ", float("} {
			if strings.Contains(line, bad) {
				t.Errorf("a writer is handed a coerced value (%q): %s", bad, strings.TrimSpace(line))
			}
		}
	}
}

// TestPythonEncodeRefusesNonInteger runs serialize() against the live corelib on
// both engines: a float in any integer position -- a narrow or 64-bit scalar, an
// enum or bitfield, an element of every integer array kind, a struct array's
// member, a union option -- is refused with SofaArgumentError instead of being
// truncated, and an int in a blob position is refused instead of becoming that
// many zero bytes. bool, IntEnum and IntFlag values, and bytearray / memoryview
// blobs, still encode to exactly the bytes their plain equivalents do.
//
// A blob is a byte string -- bytes, bytearray or a 1-D memoryview of format
// 'B' -- and the corelib's blob writers refuse anything else. That keeps two
// things true without a bytes(...) copy per field: maxlen is checked in bytes
// (a buffer of wider items, whose len() counts items, is refused rather than
// let through over its bound), and the writer takes exactly the values the
// omission test (self.b != <default literal>) compares by content, so a list,
// tuple or array.array equal to the default is neither omitted nor written.
// That agreement holds for the values the writer takes. The omission test makes
// no type test, so a memoryview of ANOTHER format, which compares items rather
// than bytes, is omitted as the default when its items equal the default's
// bytes -- even past maxlen (bd: H items 1, 2 are four bytes at maxlen 3). That
// is pinned below; a per-field type test would cost every encode.
//
// A float EQUAL to the default (u8=0.0) takes the omission branch: the field
// compares equal to its default, so it is omitted exactly as 0 is -- the value
// on the wire is the one the caller holds, nothing is truncated. Refusing it
// would cost every field a type test on the hot path.
func TestPythonEncodeRefusesNonInteger(t *testing.T) {
	corelib := os.Getenv("SOFAB_PY_CORELIB")
	if corelib == "" {
		t.Skip("set SOFAB_PY_CORELIB to a corelib-py checkout")
	}
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not found")
	}
	dir := t.TempDir()
	for path, content := range genPy(t, schema(t, encodeBoundsSchema), map[string]any{}) {
		if err := os.WriteFile(filepath.Join(dir, path), content, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	const driver = `
import array
import sys
import sofab
from sofab import SofaArgumentError
from message import M, M_Abf, M_Aen, M_Ast, M_Cd, M_Col, M_Fl

if sofab.IMPL != sys.argv[1]:
    sys.exit("engine %s, expected %s" % (sofab.IMPL, sys.argv[1]))

def with_union(**sel):
    m = M()
    for k, v in sel.items():
        setattr(m.u, k, v)
    return m

refused = {
    "u8 255.9": M(u8=255.9), "u8 1.0": M(u8=1.0), "u16": M(u16=1.5),
    "u32": M(u32=2.0), "u64": M(u64=1.5), "i8": M(i8=-1.5), "i16": M(i16=3.0),
    "i32": M(i32=-0.5), "i64": M(i64=-2.0), "enum": M(col=1.0), "bitfield": M(fl=1.0),
    "u8 array": M(au8=[1, 1.0]), "i8 array": M(ai8=[1.5]),
    "u32 array uncounted": M(uau=[2.0]), "i64 array": M(a64=[1.0]),
    "u64 array plain writer": M(ua64=[1.0]),
    "enum array": M(aen=[1.0]), "bitfield array": M(abf=[1.0]),
    "array row element": M(rows=[[1.0]]), "struct array member": M(ast=[M_Ast(x=1.5)]),
    "nested struct array": M(n=type(M().n)(arr=[1.0])),
    "union default option": with_union(small=1.5),
    "union forced array option": with_union(arr=[1.0]),
}
bad = []
for name, m in refused.items():
    try:
        out = m.encode()
    except SofaArgumentError:
        continue
    bad.append("%s: encoded %s" % (name, out.hex()))

# Not a byte string where a blob belongs: refused, never converted. An int is
# not that many zero bytes; a buffer of wider items is not measured in items
# (each of these is over its maxlen in bytes); a list, tuple or array.array is
# not a payload, whether or not it holds the default's values.
not_bytes = {
    "blob int": M(ub=3), "bounded blob int": M(b=3), "blob element int": M(ab=[3]),
    "interior empty-list blob element": M(ab=[[], b"x"]),
    "array H over maxlen": M(b=array.array("H", [1, 2, 3])),
    "memoryview H over maxlen": M(b=memoryview(b"abcdef").cast("H")),
    "blob element array I over maxlen": M(ab=[array.array("I", [1, 2])]),
    "union blob option array H over maxlen": with_union(bl=array.array("H", [1, 2])),
    "empty list": M(ub=[]), "empty array H": M(ub=array.array("H")),
    "list blob": M(ub=[1, 2]), "default as list": M(bd=[1, 2]),
    "default as tuple": M(bd=(1, 2)), "default as array B": M(bd=array.array("B", [1, 2])),
    "memoryview H off the default": M(bd=memoryview(b"\x01\x00\x03\x00").cast("H")),
}
for name, m in not_bytes.items():
    try:
        out = m.encode()
    except SofaArgumentError:
        continue
    except Exception as exc:
        bad.append("%s: raised %r, not SofaArgumentError" % (name, exc))
        continue
    bad.append("%s: encoded %s" % (name, out.hex()))

same = [
    ("bool", M(u8=True), M(u8=1)),
    ("IntEnum", M(col=M_Col.BLUE), M(col=300)),
    ("IntFlag", M(fl=M_Fl.A | M_Fl.B), M(fl=0x201)),
    ("IntEnum array", M(aen=[M_Aen.B, M_Aen.A]), M(aen=[1, 0])),
    ("IntFlag array", M(abf=[M_Abf.B, M_Abf.A | M_Abf.B]), M(abf=[0x200, 0x201])),
    ("bytearray blob", M(ub=bytearray(b"ab"), b=bytearray(b"c")), M(ub=b"ab", b=b"c")),
    ("memoryview blob", M(ub=memoryview(b"ab"), ab=[memoryview(b"x"), bytearray()]), M(ub=b"ab", ab=[b"x", b""])),
    ("empty bytearray is the default", M(ub=bytearray(), b=memoryview(b"")), M()),
    ("float equal to the default", M(u8=0.0, i64=0.0), M()),
    ("blob default as bytearray", M(bd=bytearray(b"\x01\x02")), M()),
    ("blob default as memoryview", M(bd=memoryview(b"\x01\x02")), M()),
    ("blob off its default", M(bd=bytearray(b"\x01")), M(bd=b"\x01")),
    ("enum at its non-zero default", M(cd=M_Cd.Y), M()),
    ("enum int equal to the default", M(cd=-2), M()),
    ("enum off its default", M(cd=M_Cd.X), M(cd=0)),
    # Pinned, not endorsed: a memoryview of another format compares ITEMS, so
    # one whose items equal the default's bytes is omitted as the default
    # although it holds four bytes, past maxlen 3; with other items it reaches
    # the writer and is refused (not_bytes above).
    ("memoryview H with the default's items", M(bd=memoryview(b"\x01\x00\x02\x00").cast("H")), M()),
]
for name, a, b in same:
    try:
        ea, eb = a.encode(), b.encode()
    except Exception as exc:
        bad.append("%s: raised %r" % (name, exc))
        continue
    if ea != eb:
        bad.append("%s: %s != %s" % (name, ea.hex(), eb.hex()))
again = M.decode(M(col=M_Col.BLUE, fl=M_Fl.B, aen=[M_Aen.B]).encode())
if again.col != 300 or again.fl != 0x200 or again.aen != [1] or again.cd is not M_Cd.Y:
    bad.append("enum/bitfield round trip: %r" % again)
if bad:
    sys.exit("[%s]\n  " % sofab.IMPL + "\n  ".join(bad))
print("%s ok" % sofab.IMPL)
`
	if err := os.WriteFile(filepath.Join(dir, "driver.py"), []byte(driver), 0o644); err != nil {
		t.Fatal(err)
	}
	env := func(pure string) []string {
		return append(os.Environ(),
			"PYTHONPATH="+filepath.Join(corelib, "src")+string(os.PathListSeparator)+dir,
			"SOFAB_PUREPYTHON="+pure)
	}
	probe := exec.Command(py, "-c", "import sofab; print(sofab.IMPL)")
	probe.Env = env("")
	impl, err := probe.Output()
	if err != nil {
		t.Fatalf("import sofab: %v", err)
	}
	runs := [][2]string{{"1", "python"}}
	if strings.TrimSpace(string(impl)) == "native" {
		runs = append(runs, [2]string{"", "native"})
	} else {
		t.Log("native engine: SKIPPED, sofab._speedups is not built in SOFAB_PY_CORELIB")
	}
	for _, r := range runs {
		cmd := exec.Command(py, filepath.Join(dir, "driver.py"), r[1])
		cmd.Dir = dir
		cmd.Env = env(r[0])
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s engine: %v\n%s", r[1], err, out)
		}
		t.Logf("%s engine: %s", r[1], strings.TrimSpace(string(out)))
	}
}
