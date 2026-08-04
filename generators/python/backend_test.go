package python

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sofa-buffers/generator/internal/analysis"
	"github.com/sofa-buffers/generator/internal/ir"
	"github.com/sofa-buffers/generator/internal/model"
	"github.com/sofa-buffers/generator/internal/parser"
)

func schema(t *testing.T, src string) *ir.Schema {
	t.Helper()
	doc, err := parser.Parse([]byte(src), "t.yaml")
	if err != nil {
		t.Fatal(err)
	}
	resolved, _ := doc.Resolve()
	if errs := parser.Validate(resolved); errs != nil {
		t.Fatalf("invalid: %v", errs)
	}
	s, err := model.Build(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := analysis.Analyze(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func schemaFile(t *testing.T, path string) *ir.Schema {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return schema(t, string(b))
}

func genPy(t *testing.T, s *ir.Schema, cfg map[string]any) map[string][]byte {
	t.Helper()
	files, err := (&Backend{}).Generate(s, cfg)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	out := map[string][]byte{}
	for _, f := range files {
		out[f.Path] = f.Content
	}
	return out
}

func TestPythonStructural(t *testing.T) {
	mod := string(genPy(t, schemaFile(t, "../../examples/messages/example.yaml"), map[string]any{})["message.py"])
	for _, want := range []string{
		// example.yaml has count-bearing native arrays, so the over-count guard
		// (generator#100) pulls in SofaDecodeError.
		"from sofab import Encoder, Decoder, SofaDecodeError, WireType",
		"@dataclass",
		"class Myfirstmessage:",
		"def serialize(self, e: Encoder)",
		"def deserialize(self, d: Decoder)",
		"class MyfirstmessageSomeenum(IntEnum):",
		"def to_jsonable(self)",
		"e.write_sequence_begin_lazy(", // every sequence opens lazily (MESSAGE_SPEC S2)
		"if fld.count > 4:",            // over-count scalar array rejected at the count header (generator#100/#216)
		`raise SofaDecodeError("someuintarray: array count above schema capacity 4")`,
	} {
		if !strings.Contains(mod, want) {
			t.Errorf("message.py missing %q", want)
		}
	}
}

// TestPythonLazySequenceFraming: MESSAGE_SPEC §2 omits a sequence-typed FIELD
// whose value equals its declared default instead of framing it empty, so every
// sequence opens with write_sequence_begin_lazy and the CLOSER alone decides
// whether a contentless one survives — and where that choice is made from is the
// whole of the element rule:
//
//	struct/union FIELD    -> write_sequence_end  (decided by the SCHEMA: may vanish)
//	array wrapper FIELD   -> write_sequence_end  (decided by the SCHEMA: may vanish)
//	wrapper-array ELEMENT -> decided by its position in the VALUE, at run time:
//	                         the dropping end in the array's interior, the keeping
//	                         end at the last index
//
// Only the LAST element's presence carries the array's length (§5.1), so only it
// must survive as an empty frame; an interior all-default element is a gap the
// decoder restores from the element default.
func TestPythonLazySequenceFraming(t *testing.T) {
	const src = `
version: 1
messages:
  M:
    payload:
      st:    { id: 0, type: struct, fields: { x: { id: 0, type: i32 } } }
      un:    { id: 1, type: union, oneof: { a: { id: 0, type: i32 } } }
      strs:  { id: 2, type: array, items: { type: string } }
      blobs: { id: 3, type: array, items: { type: blob } }
      objs:  { id: 4, type: array, items: { type: struct, fields: { y: { id: 0, type: i32 } } } }
      deep:  { id: 5, type: array, items: { type: array, items: { type: struct, fields: { z: { id: 0, type: i32 } } } } }
`
	mod := string(genPy(t, schema(t, src), map[string]any{})["message.py"])
	for _, want := range []string{
		// struct FIELD and union FIELD: lazy open, dropping close.
		"        e.write_sequence_begin_lazy(0)\n        self.st.serialize(e)\n        e.write_sequence_end()\n",
		"        e.write_sequence_begin_lazy(1)\n        self.un.serialize(e)\n        e.write_sequence_end()\n",
		// string/blob wrapper FIELD: the leaf elements are omitted when default,
		// and the wrapper itself closes with the dropping end at field level.
		// These two arrays are DYNAMIC, so they are not narrowed and their LAST
		// element is written whatever its value — its presence is what carries the
		// recovered length (MESSAGE_SPEC §2/§5.1).
		"        e.write_sequence_begin_lazy(2)\n        for _i0, _e0 in enumerate(self.strs):\n" +
			"            if _e0 != \"\" or _i0 == len(self.strs) - 1:\n                e.write_string(_i0, _e0)\n        e.write_sequence_end()\n",
		"        e.write_sequence_begin_lazy(3)\n        for _i0, _e0 in enumerate(self.blobs):\n" +
			"            if len(_e0) != 0 or _i0 == len(self.blobs) - 1:\n                e.write_bytes(_i0, bytes(_e0))\n        e.write_sequence_end()\n",
		// struct ELEMENT inside a wrapper array: the closer is picked from the
		// element's index in the value; the wrapper FIELD around it still closes
		// unconditionally with the dropping end.
		"        e.write_sequence_begin_lazy(4)\n        for _i0, _e0 in enumerate(self.objs):\n" +
			"            e.write_sequence_begin_lazy(_i0)\n            _e0.serialize(e)\n" +
			"            if _i0 == len(self.objs) - 1:\n                e.write_sequence_end_keep()\n" +
			"            else:\n                e.write_sequence_end()\n        e.write_sequence_end()\n",
		// array-of-array: the nested ROW is an ELEMENT, as is each struct element
		// inside it -- both take the positional closer; only the depth-0 wrapper is
		// decided statically.
		"        e.write_sequence_begin_lazy(5)\n        for _i0, _e0 in enumerate(self.deep):\n" +
			"            e.write_sequence_begin_lazy(_i0)\n            for _i1, _e1 in enumerate(_e0):\n" +
			"                e.write_sequence_begin_lazy(_i1)\n                _e1.serialize(e)\n" +
			"                if _i1 == len(_e0) - 1:\n                    e.write_sequence_end_keep()\n" +
			"                else:\n                    e.write_sequence_end()\n" +
			"            if _i0 == len(self.deep) - 1:\n                e.write_sequence_end_keep()\n" +
			"            else:\n                e.write_sequence_end()\n" +
			"        e.write_sequence_end()\n",
	} {
		if !strings.Contains(mod, want) {
			t.Errorf("message.py missing lazy framing:\n%s\ngot:\n%s", want, mod)
		}
	}
	// The eager begin is gone from corelib-py: emitting it would be an
	// AttributeError at encode time, not a size regression.
	if strings.Contains(mod, "e.write_sequence_begin(") {
		t.Error("generated code must not call the removed eager write_sequence_begin")
	}
	// An ELEMENT must never take the keeping closer unconditionally: that framed
	// every all-default interior element, which the sparse rule now omits. Each of
	// the three element frames in this schema (objs element, deep row, deep
	// element) is guarded, so every keep is paired with a drop.
	if strings.Contains(mod, "_marshal(e)\n            e.write_sequence_end_keep()") {
		t.Errorf("an array element must not close unconditionally with the keeping end:\n%s", mod)
	}
	if n := strings.Count(mod, "e.write_sequence_end_keep()"); n != 3 {
		t.Errorf("expected 3 write_sequence_end_keep() calls (array ELEMENTS only), got %d", n)
	}
}

// TestPythonDecodeLimits: the max_dyn_* config keys bake receiver-side decode
// limits (generator#102) into the generated module -- named constants at
// module level plus Decoder(max_array_count=..., ...) kwargs in every decode.
// The cap is raised to the largest schema bound of its kind (escape hatch:
// schema-bounded fields stay governed by their own bound), an unset key emits
// nothing, and a key whose kind has no unbounded field is inert.
func TestPythonDecodeLimits(t *testing.T) {
	const src = `
version: 1
messages:
  dyn:
    payload:
      s:    { id: 0, type: string }
      arr:  { id: 1, type: array, items: { type: u64 } }
      barr: { id: 2, type: array, items: { type: i32, count: 100000 } }
`
	s := schema(t, src)
	mod := string(genPy(t, s, map[string]any{
		"max_dyn_array_count": 65536,
		"max_dyn_string_len":  4096,
		"max_dyn_blob_len":    2048, // no unbounded blob in the schema -> inert
	})["message.py"])
	for _, want := range []string{
		"MAX_DYN_ARRAY_COUNT = 100000", // raised to the schema count of barr
		"MAX_DYN_STRING_LEN = 4096",
		"o.deserialize(Decoder(io.BytesIO(data), max_array_count=MAX_DYN_ARRAY_COUNT, max_string_len=MAX_DYN_STRING_LEN))",
	} {
		if !strings.Contains(mod, want) {
			t.Errorf("message.py missing %q", want)
		}
	}
	if strings.Contains(mod, "MAX_DYN_BLOB_LEN") || strings.Contains(mod, "max_blob_len") {
		t.Error("inert blob limit must not be emitted (no unbounded blob)")
	}

	// No limits configured -> byte-identical plumbing-free output.
	plain := string(genPy(t, s, map[string]any{})["message.py"])
	if strings.Contains(plain, "MAX_DYN") || strings.Contains(plain, "max_array_count") {
		t.Error("unset limits must emit no limit plumbing")
	}
	if !strings.Contains(plain, "o.deserialize(Decoder(io.BytesIO(data)))") {
		t.Error("unset limits must leave the plain Decoder call unchanged")
	}
}

// TestPythonMetadataDocs: enum-constant and bitfield-flag descriptions render as
// Sphinx "#:" attribute comments (flags append a "(default: true/false)" note),
// and a deprecated field carries a ".. deprecated::" directive in its doc.
// TestPythonOverIndexWrapperArray: a fixed-count wrapper array (string/blob/
// struct elements) raises SofaDecodeError for an element id >= N before the list
// grows (issue #142 / MESSAGE_SPEC §5.1/§7). A dynamic array keeps every index.
func TestPythonOverIndexWrapperArray(t *testing.T) {
	const src = `
version: 1
messages:
  M:
    payload:
      bs: { id: 0, type: array, items: { type: string, count: 4, maxlen: 16 } }
      bb: { id: 1, type: array, items: { type: blob,   count: 3, maxlen: 16 } }
      bp: { id: 2, type: array, items: { type: struct, count: 2, fields: { x: { id: 0, type: i32 } } } }
      ds: { id: 3, type: array, items: { type: string } }
`
	mod := string(genPy(t, schema(t, src), map[string]any{})["message.py"])
	// The over-index guard raises SofaDecodeError, so the on-demand import MUST be
	// emitted even when the schema has no scalar over-count array (the #100 case) —
	// a wrapper-only schema like this one. Missing it is a NameError at decode time.
	// FixlenSubtype rides along: the string/blob ELEMENT guards name it even though
	// no *field* here is fixlen (generator#246), so the full line is asserted — a
	// prefix match would pass either way and let the missing name through.
	if !strings.Contains(mod, "from sofab import Encoder, Decoder, SofaDecodeError, WireType, FixlenSubtype\n") {
		t.Errorf("message.py needs SofaDecodeError (over-index guard) AND FixlenSubtype (element guard) imported, else NameError at decode:\n%s", mod)
	}
	for _, want := range []string{
		`if _ef0.id >= 4:`,
		`raise SofaDecodeError("self.bs: array index above schema capacity 4")`,
		`raise SofaDecodeError("self.bb: array index above schema capacity 3")`,
		`raise SofaDecodeError("self.bp: array index above schema capacity 2")`,
	} {
		if !strings.Contains(mod, want) {
			t.Errorf("message.py missing over-index guard %q", want)
		}
	}
	// Dynamic string array keeps every index — no guard raised for it.
	if strings.Contains(mod, `raise SofaDecodeError("self.ds: array index above schema capacity`) {
		t.Errorf("dynamic string array must not carry an over-index guard")
	}
}

// TestPythonMaxlenReject: a bounded (maxlen) string/blob whose wire BYTE length
// exceeds its schema maxlen is malformed and MUST raise SofaDecodeError on
// decode — never silently truncated (MESSAGE_SPEC §7.1). Covers scalar fields
// and (dynamic) wrapper-array string elements. The schema below has NO counted
// native array and NO counted wrapper array, so the maxlen guard is the ONLY
// thing pulling in SofaDecodeError — a regression check on the on-demand import
// (a bounded-string-only schema that missed the import would NameError at
// decode).
func TestPythonMaxlenReject(t *testing.T) {
	const src = `
version: 1
messages:
  M:
    payload:
      s:   { id: 0, type: string, maxlen: 8 }
      b:   { id: 1, type: blob,   maxlen: 8 }
      arr: { id: 2, type: array, items: { type: string, maxlen: 5 } }
      us:  { id: 3, type: string }
`
	mod := string(genPy(t, schema(t, src), map[string]any{})["message.py"])

	// (a) The maxlen guard raises SofaDecodeError, so the on-demand import MUST be
	// present even though this schema has no counted native/wrapper array — the
	// import bug this test guards against.
	// Asserted as the FULL line (FixlenSubtype included — this schema has string
	// and blob fields, and a string wrapper element): a prefix match cannot tell a
	// complete import line from a truncated one (generator#246).
	if !strings.Contains(mod, "from sofab import Encoder, Decoder, SofaDecodeError, WireType, FixlenSubtype\n") {
		t.Errorf("message.py must import SofaDecodeError for the maxlen guard (else NameError at decode):\n%s", mod)
	}

	for _, want := range []string{
		// (b) scalar string: bound the wire byte length (non-consuming peek), not a
		// re-encode of the decoded str (#155).
		`if d.fixlen_len() > 8:`,
		`raise SofaDecodeError("s: string byte length above schema maxlen 8")`,
		// (b) scalar blob: byte length of the bytes value.
		`if len(self.b) > 8:`,
		`raise SofaDecodeError("b: blob byte length above schema maxlen 8")`,
		// (c) bounded wrapper string element (maxlen 5): wire byte length peek.
		`if d.fixlen_len() > 5:`,
		`raise SofaDecodeError("self.arr: string element byte length above schema maxlen 5")`,
	} {
		if !strings.Contains(mod, want) {
			t.Errorf("message.py missing maxlen guard %q", want)
		}
	}

	// (d) the string maxlen check must never re-encode the decoded str (#155).
	if strings.Contains(mod, `.encode("utf-8")`) {
		t.Error(`string maxlen check must not re-encode via .encode("utf-8") (#155)`)
	}

	// (e) the unbounded string field carries no maxlen guard.
	if strings.Contains(mod, `raise SofaDecodeError("us:`) {
		t.Error("unbounded string must not raise a maxlen SofaDecodeError")
	}
	if strings.Contains(mod, `raise SofaDecodeError("us:`) {
		t.Error("unbounded string must not raise a maxlen SofaDecodeError")
	}
}

func TestPythonMetadataDocs(t *testing.T) {
	const src = `
version: 1
$defs:
  enum:
    Mode:
      Off:    { value: 0, description: "Node is powered down." }
      Active: { value: 1, description: "Node is sampling and transmitting." }
  bitfield:
    StatusFlags:
      ready:      { pos: 0, default: true, description: "Node has completed initialization." }
      overheated: { pos: 1, description: "Core temperature exceeded the safe threshold." }
messages:
  Telemetry:
    payload:
      legacyId: { id: 0, type: u32, description: "Old identifier retained for backward compatibility.", deprecated: true }
      mode:     { id: 1, type: enum, enum: { $ref: "#/$defs/enum/Mode" } }
      status:   { id: 2, type: bitfield, bits: { $ref: "#/$defs/bitfield/StatusFlags" } }
`
	mod := string(genPy(t, schema(t, src), map[string]any{})["message.py"])
	for _, want := range []string{
		// enum-constant descriptions
		"    #: Node is powered down.\n    OFF = 0",
		"    #: Node is sampling and transmitting.\n    ACTIVE = 1",
		// bitfield flag description + default note (and no-default flag)
		"    #: Node has completed initialization. (default: true)\n    READY = 1 << 0",
		"    #: Core temperature exceeded the safe threshold.\n    OVERHEATED = 1 << 1",
		// deprecated field doc: description then a Sphinx deprecated directive
		"    #: Old identifier retained for backward compatibility.\n    #: .. deprecated::",
	} {
		if !strings.Contains(mod, want) {
			t.Errorf("message.py missing %q", want)
		}
	}
	// A flag without a default must NOT carry a "(default:" note.
	if strings.Contains(mod, "safe threshold. (default:") {
		t.Error("no-default flag must not get a (default:) note")
	}
}

// TestPythonArrayCountIsCapacityNotLength: `count: N` is a CAPACITY, not a
// length (MESSAGE_SPEC §3). It never reaches the wire, the wire count M IS the
// array's length, and nothing that carries that length may be elided. So the whole
// trim-on-encode / fill-on-decode pair is gone -- from both array forms -- and a
// count:N array is generated exactly like a count-less one except for the bound it
// still enforces.
func TestPythonArrayCountIsCapacityNotLength(t *testing.T) {
	const src = `
version: 1
$defs:
  enum:
    Mode: { Off: { value: 0 }, Active: { value: 1 } }
messages:
  T:
    payload:
      fixedU32:  { id: 0, type: array, items: { type: u32, count: 5 } }
      fixedF32:  { id: 1, type: array, items: { type: fp32, count: 2 } }
      fixedBool: { id: 2, type: array, items: { type: boolean, count: 4 } }
      fixedEnum: { id: 3, type: array, items: { type: enum, enum: { $ref: "#/$defs/enum/Mode" }, count: 2 } }
      shortDflt: { id: 4, type: array, items: { type: u32, count: 5 }, default: [1, 2] }
      dynU32:    { id: 5, type: array, items: { type: u32 } }
      fixedStrs: { id: 6, type: array, items: { type: string, count: 3, maxlen: 8 } }
      fixedObjs: { id: 7, type: array, items: { type: struct, count: 3, fields: { k: { id: 0, type: u32 } } } }
`
	mod := string(genPy(t, schema(t, src), map[string]any{})["message.py"])

	for _, want := range []string{
		// Encode writes the value whole: no trim wrapper anywhere.
		"e.write_unsigned_array(0, self.fixedU32)",
		"e.write_float32_array(1, self.fixedF32)",
		"e.write_unsigned_array(2, [1 if _v else 0 for _v in self.fixedBool])",
		"e.write_signed_array(3, [int(_v) for _v in self.fixedEnum])",
		// The omit test is the ordinary != default, against the value as it stands:
		// the EMPTY list when nothing is declared, the declared literal otherwise.
		"if len(self.fixedU32) != 0:",
		"if len(self.dynU32) != 0:",
		"if self.shortDflt != [1, 2]:",
		// A fresh count:N array is the EMPTY list -- both element kinds, so the two
		// forms agree, and neither materializes N elements it never received.
		"fixedU32: list[int] = field(default_factory=list)",
		"fixedBool: list[bool] = field(default_factory=list)",
		"fixedStrs: list[str] = field(default_factory=list)",
		"fixedObjs: list[TFixedObjsElem] = field(default_factory=list)",
		// ...while a declared default is still materialized, exactly as written.
		"shortDflt: list[int] = field(default_factory=lambda: [1, 2])",
		// _is_default has to reach the same verdict as the writer, or it omits a
		// field that is on the wire (or keeps one that is not).
		"if not (len(self.fixedU32) == 0):",
		"if not (self.shortDflt == [1, 2]):",
		"if not (len(self.fixedStrs) == 0):",
		"if not (len(self.fixedObjs) == 0):",
		// The bound itself is untouched -- that is all `count` still does.
		"if fld.count > 5:",
		"if _ef0.id >= 3:",
	} {
		if !strings.Contains(mod, want) {
			t.Errorf("message.py missing %q:\n%s", want, mod)
		}
	}
	// Nothing anywhere may trim on encode, refill on decode, or pad a short default
	// out to N -- and with the helpers unreferenced, `import math` goes with them.
	for _, bad := range []string{
		"_trim_tail", "_trim_tail_float", "_pad_to", "_trim_empty", "_trim_objs",
		"import math",
		// the padded renderings of the two array defaults above
		"[0, 0, 0, 0, 0]", "[1, 2, 0, 0, 0]",
		// a count:N wrapper array materialized to N element defaults
		"for _ in range(3)",
	} {
		if strings.Contains(mod, bad) {
			t.Errorf("message.py still carries the superseded fixed-length machinery %q:\n%s", bad, mod)
		}
	}
}

// The removed helpers are gone from the backend, so no schema can reach them --
// not a count-bearing float array (which used to pull in `import math` for the
// bit-pattern trim), not a count-bearing wrapper array.
func TestPythonNoFixedArrayHelpersWhenUnused(t *testing.T) {
	const src = `
version: 1
messages:
  T:
    payload:
      dynU32: { id: 0, type: array, items: { type: u32 } }
      fixed:  { id: 1, type: array, items: { type: fp64, count: 3 } }
      fstrs:  { id: 2, type: array, items: { type: string, count: 3, maxlen: 4 } }
`
	mod := string(genPy(t, schema(t, src), map[string]any{})["message.py"])
	for _, bad := range []string{"_trim_tail", "_pad_to", "_trim_empty", "_trim_objs", "import math"} {
		if strings.Contains(mod, bad) {
			t.Errorf("message.py must not contain %q:\n%s", bad, mod)
		}
	}
}

func TestPythonSyntaxValid(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not found")
	}
	dir := t.TempDir()
	for path, content := range genPy(t, schemaFile(t, "../../examples/messages/example.yaml"), map[string]any{"emit": "project"}) {
		if err := os.WriteFile(filepath.Join(dir, path), content, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command(py, "-m", "py_compile", filepath.Join(dir, "message.py"), filepath.Join(dir, "harness.py"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generated Python is invalid:\n%s", out)
	}
}

// TestPythonConformance: byte-exact shared-vector conformance against corelib-py
// (no build step — Python just needs the corelib on PYTHONPATH). Gated on
// SOFAB_PY_CORELIB (a corelib-py checkout with src/ + assets/test_vectors.json).
func TestPythonConformance(t *testing.T) {
	corelib := os.Getenv("SOFAB_PY_CORELIB")
	if corelib == "" {
		t.Skip("set SOFAB_PY_CORELIB to a corelib-py checkout")
	}
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not found")
	}
	raw, err := os.ReadFile(filepath.Join(corelib, "assets", "test_vectors.json"))
	if err != nil {
		t.Skipf("no vectors: %v", err)
	}
	var vf struct {
		Vectors []struct {
			Name   string `json:"name"`
			Offset int    `json:"offset"`
			Fields []struct {
				Op    string          `json:"op"`
				ID    int64           `json:"id"`
				Value json.RawMessage `json:"value"`
			} `json:"fields"`
			Serialized struct {
				Hex string `json:"hex"`
			} `json:"serialized"`
		} `json:"vectors"`
	}
	if err := json.Unmarshal(raw, &vf); err != nil {
		t.Fatal(err)
	}

	groups := map[string]string{"unsigned": "u64", "signed": "i64", "fp32": "fp32", "fp64": "fp64", "string": "string"}
	dirs := map[string]string{}
	for op, typ := range groups {
		dirs[op] = g(t, typ)
	}
	pyEnv := append(os.Environ(), "PYTHONPATH="+filepath.Join(corelib, "src"))

	checked := 0
	for _, v := range vf.Vectors {
		if len(v.Fields) != 1 || v.Offset != 0 {
			continue
		}
		f := v.Fields[0]
		dir, ok := dirs[f.Op]
		if !ok || f.ID != 0 {
			continue
		}
		in, ok := scalarJSON(f.Op, string(f.Value))
		if !ok {
			continue
		}
		cmd := exec.Command(py, filepath.Join(dir, "harness.py"), "encode", "vec")
		cmd.Stdin = strings.NewReader(in)
		cmd.Env = pyEnv
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("encode %q: %v", in, err)
		}
		// Sparse-canonical (MESSAGE_SPEC S2): a field equal to its default is
		// omitted, so a default-valued single-field message encodes to empty. The
		// dense per-field vector is still validated for every non-default value.
		got := hex.EncodeToString(out)
		if pyValueIsDefault(f.Op, string(f.Value)) {
			if got != "" {
				t.Errorf("vector %q: default-valued field must be omitted (sparse), got %s", v.Name, got)
			} else {
				checked++
			}
		} else if got != v.Serialized.Hex {
			t.Errorf("vector %q: got %s want %s", v.Name, got, v.Serialized.Hex)
		} else {
			checked++
		}
	}
	t.Logf("Python shared-vector conformance: %d byte-exact", checked)
	if checked == 0 {
		t.Fatal("no vectors checked")
	}
}

// g generates a one-field project into a temp dir and returns it.
func g(t *testing.T, typ string) string {
	t.Helper()
	extra := ""
	if typ == "string" {
		extra = ", maxlen: 4096"
	}
	src := "version: 1\nmessages:\n  vec:\n    payload:\n      a: {id: 0, type: " + typ + extra + "}\n"
	dir := t.TempDir()
	for path, content := range genPy(t, schema(t, src), map[string]any{"emit": "project"}) {
		if err := os.WriteFile(filepath.Join(dir, path), content, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// pyValueIsDefault reports whether a shared-vector scalar value is the type
// default (zero / empty) -- which a sparse-canonical encoder omits.
func pyValueIsDefault(op, rawValue string) bool {
	s := strings.Trim(strings.TrimSpace(rawValue), `"`)
	switch op {
	case "unsigned", "signed":
		return s == "0"
	case "fp32", "fp64":
		return s == "0" || s == "0.0"
	case "string":
		return s == ""
	}
	return false
}

func scalarJSON(op, rawValue string) (string, bool) {
	s := strings.TrimSpace(rawValue)
	switch op {
	case "unsigned", "signed":
		return `{"a":` + strings.Trim(s, `"`) + `}`, true
	case "fp32", "fp64":
		if strings.Contains(s, "inf") {
			return "", false
		}
		return `{"a":` + s + `}`, true
	case "string":
		return `{"a":` + s + `}`, true
	}
	return "", false
}

// TestPythonWireTypeGuard pins the MESSAGE_SPEC §7.3 guard (generator#174): the
// generated dispatch compares each field header's wire type — plus the fixlen
// subtype where the wire type alone is ambiguous — against the type the schema
// declares, and skips the field like an unknown id on a mismatch. Without it the
// schema-typed reader is called for a type the field does not carry, which
// corelib-py correctly rejects with SofaStateError, failing the whole decode.
func TestPythonWireTypeGuard(t *testing.T) {
	s := schema(t, `
version: 1
messages:
  M:
    payload:
      a: { id: 0, type: u8 }
      b: { id: 1, type: i32 }
      c: { id: 2, type: boolean }
      d: { id: 3, type: fp32 }
      e: { id: 4, type: fp64 }
      f: { id: 5, type: string, maxlen: 8 }
      g: { id: 6, type: blob, maxlen: 8 }
      h: { id: 7, type: struct, fields: { x: { id: 0, type: u8 } } }
      i: { id: 8, type: array, items: { type: u32, count: 2 } }
      j: { id: 9, type: array, items: { type: i32, count: 2 } }
      k: { id: 10, type: array, items: { type: fp32, count: 2 } }
      l: { id: 11, type: array, items: { type: string, count: 2, maxlen: 4 } }
`)
	mod := string(genPy(t, s, map[string]any{})["message.py"])
	// FixlenSubtype is referenced by the fixlen guards, so it must be imported.
	if !strings.Contains(mod, "from sofab import Encoder, Decoder, SofaDecodeError, WireType, FixlenSubtype") {
		t.Errorf("message.py missing FixlenSubtype import:\n%s", mod)
	}
	for _, want := range []string{
		// Wire type alone settles the integer/bool kinds...
		"if fld.type != WireType.UNSIGNED:",
		"if fld.type != WireType.SIGNED:",
		// ...but fp32/fp64/string/blob all share FIXLEN, so the subtype decides.
		"if fld.type != WireType.FIXLEN or fld.subtype != FixlenSubtype.FP32:",
		"if fld.type != WireType.FIXLEN or fld.subtype != FixlenSubtype.FP64:",
		"if fld.type != WireType.FIXLEN or fld.subtype != FixlenSubtype.STRING:",
		"if fld.type != WireType.FIXLEN or fld.subtype != FixlenSubtype.BLOB:",
		// Nested messages and composite (wrapper) arrays open a sequence.
		"if fld.type != WireType.SEQUENCE_START:",
		// Native scalar arrays carry the matching ARRAY_* wire type; the fp array
		// shares ARRAY_FIXLEN with the other fixlen arrays, so it too needs the
		// subtype.
		"if fld.type != WireType.ARRAY_UNSIGNED:",
		"if fld.type != WireType.ARRAY_SIGNED:",
		"if fld.type != WireType.ARRAY_FIXLEN or fld.subtype != FixlenSubtype.FP32:",
		// A mismatch skips the field and resumes the loop — never falls through
		// into the reader below it.
		"                    d.skip()\n                    continue",
	} {
		if !strings.Contains(mod, want) {
			t.Errorf("message.py missing wire-type guard %q\n%s", want, mod)
		}
	}
}

// TestPythonNoFixlenSubtypeImportWhenUnused keeps the import line honest: a
// schema with no fixlen-framed field never references FixlenSubtype, so
// importing it would leave an unused name in every generated module.
func TestPythonNoFixlenSubtypeImportWhenUnused(t *testing.T) {
	s := schema(t, `
version: 1
messages:
  M:
    payload:
      a: { id: 0, type: u8 }
      b: { id: 1, type: i32 }
`)
	mod := string(genPy(t, s, map[string]any{})["message.py"])
	if strings.Contains(mod, "FixlenSubtype") {
		t.Errorf("message.py must not import FixlenSubtype when no fixlen field exists:\n%s", mod)
	}
	if !strings.Contains(mod, "from sofab import Encoder, Decoder, WireType") {
		t.Errorf("message.py missing plain import line:\n%s", mod)
	}
}

// TestPythonFixlenSubtypeImportMatchesUse is the durable form of generator#246:
// instead of pinning one import line per shape, it asserts the INVARIANT the gate
// exists for — the module imports FixlenSubtype exactly when its body references
// it. The old gate looked only at field kinds plus one level of NATIVE array
// element, so every wrapper-array element that names a subtype (an array<string>
// element guard, or a nested array<array<fp32>> row) generated a module using a
// name it never imported: NameError at decode time, from decode of any field —
// the module imports fine, so no test that merely compiles it can catch this.
func TestPythonFixlenSubtypeImportMatchesUse(t *testing.T) {
	cases := []struct {
		name string
		want bool // FixlenSubtype expected in the import line
		src  string
	}{
		// The issue's reproduction: the ONLY fixlen use is a wrapper string
		// ELEMENT. Pre-fix this imported nothing and died in _unmarshal.
		{"wrapper string array", true, `
      tags: { id: 0, type: array, items: { type: string } }
      n:    { id: 1, type: u32 }`},
		{"wrapper blob array", true, `
      parts: { id: 0, type: array, items: { type: blob } }
      n:     { id: 1, type: u32 }`},
		// Nested rows: the guard sits one (or two) levels below the field.
		{"nested string rows", true, `
      rows: { id: 0, type: array, items: { type: array, items: { type: string } } }
      n:    { id: 1, type: u32 }`},
		{"nested fp32 rows", true, `
      grid: { id: 0, type: array, items: { type: array, items: { type: fp32 } } }
      n:    { id: 1, type: u32 }`},
		{"doubly nested blob rows", true, `
      cube: { id: 0, type: array, items: { type: array, items: { type: array, items: { type: blob } } } }
      n:    { id: 1, type: u32 }`},
		// A string reached through a STRUCT element is guarded inside the struct's
		// own class, which schemaHasField already walks via the named types.
		{"string inside a struct element", true, `
      items: { id: 0, type: array, items: { type: struct, fields: { s: { id: 0, type: string } } } }
      n:     { id: 1, type: u32 }`},
		// Negatives — the import must stay out, or every module carries an unused
		// name (the reason the gate exists at all).
		{"native integer array", false, `
      a: { id: 0, type: array, items: { type: u32 } }
      n: { id: 1, type: u32 }`},
		{"nested integer rows", false, `
      m: { id: 0, type: array, items: { type: array, items: { type: i32 } } }
      n: { id: 1, type: u32 }`},
		{"struct elements without fixlen", false, `
      items: { id: 0, type: array, items: { type: struct, fields: { x: { id: 0, type: i32 } } } }
      n:     { id: 1, type: u32 }`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mod := string(genPy(t, schema(t, "version: 1\nmessages:\n  M:\n    payload:"+tc.src+"\n"), map[string]any{})["message.py"])
			imp := importLine(t, mod)
			imported := strings.Contains(imp, "FixlenSubtype")
			// The body is what settles it: a reference outside the import line is a
			// NameError unless the name was imported.
			used := strings.Contains(mod[strings.Index(mod, imp)+len(imp):], "FixlenSubtype")
			if used != tc.want {
				t.Fatalf("expected the emitted guards to reference FixlenSubtype=%v; module:\n%s", tc.want, mod)
			}
			if imported != used {
				t.Errorf("import/use mismatch: imported=%v used=%v (imported without use = dead name; used without import = NameError at decode)\n%s",
					imported, used, mod)
			}
		})
	}
}

// importLine returns the module's single `from sofab import ...` line.
func importLine(t *testing.T, mod string) string {
	t.Helper()
	for _, ln := range strings.Split(mod, "\n") {
		if strings.HasPrefix(ln, "from sofab import ") {
			return ln
		}
	}
	t.Fatalf("no corelib import line in:\n%s", mod)
	return ""
}

// MESSAGE_SPEC §2 governs both element kinds with ONE rule, and the rule is
// positional: an element before the last one that equals its element default is
// omitted -- a string/blob leaf simply not written, a sequence element not framed
// either -- while the element at the LAST index is always written, as its value or
// as an empty frame. Only the last index carries the array's length (§5.1); an
// interior gap is restored from the element default and is therefore free.
//
// This holds with or without a declared `count`: a capacity can never restore an
// elided tail.
func TestPythonElementSparsityIsPositional(t *testing.T) {
	const src = `
version: 1
messages:
  vec:
    payload:
      dynstr:   { id: 0, type: array, items: { type: string, maxlen: 8 } }
      dynblob:  { id: 1, type: array, items: { type: blob, maxlen: 8 } }
      fixedstr: { id: 2, type: array, items: { type: string, count: 3, maxlen: 8 } }
      fixedobj: { id: 3, type: array, items: { type: struct, count: 3, fields: { k: { id: 0, type: u32 } } } }
      mat:      { id: 4, type: array, items: { type: array, count: 2, items: { type: u32, count: 3 } } }
      rows:     { id: 5, type: array, items: { type: array, count: 2, items: { type: string, maxlen: 4 } } }
`
	mod := string(genPy(t, schema(t, src), map[string]any{})["message.py"])

	for _, want := range []string{
		// leaf elements: the last index escapes the omit test -- count or no count
		`            if _e0 != "" or _i0 == len(self.dynstr) - 1:`,
		"            if len(_e0) != 0 or _i0 == len(self.dynblob) - 1:",
		`            if _e0 != "" or _i0 == len(self.fixedstr) - 1:`,
		// sequence elements: the same rule, applied through the closer
		"            if _i0 == len(self.fixedobj) - 1:\n                e.write_sequence_end_keep()\n" +
			"            else:\n                e.write_sequence_end()\n",
		// a native row carries no frame of its own, so the rule lands on the write
		"            if len(_e0) != 0 or _i0 == len(self.mat) - 1:\n                e.write_unsigned_array(_i0, _e0)\n",
		// a wrapper row has a frame, so it takes the closer
		"            if _i0 == len(self.rows) - 1:\n                e.write_sequence_end_keep()\n" +
			"            else:\n                e.write_sequence_end()\n",
		// every array loops over the value itself: there is no M to narrow to
		"for _i0, _e0 in enumerate(self.fixedstr):",
		"for _i0, _e0 in enumerate(self.fixedobj):",
	} {
		if !strings.Contains(mod, want) {
			t.Errorf("message.py missing %q:\n%s", want, mod)
		}
	}
	// The predicate follows the writer: every non-empty array now puts at least one
	// element on the wire, so "default" is exactly "empty" for every element kind.
	for _, want := range []string{
		"if not (len(self.dynstr) == 0):",
		"if not (len(self.fixedstr) == 0):",
		"if not (len(self.fixedobj) == 0):",
		"if not (len(self.rows) == 0):",
	} {
		if !strings.Contains(mod, want) {
			t.Errorf("_is_default must be plain emptiness, missing %q:\n%s", want, mod)
		}
	}
	// The two defects this replaced: an unconditional keeping closer on an element,
	// and the fixed-count leaf's missing last-element guard.
	if strings.Contains(mod, "_marshal(e)\n            e.write_sequence_end_keep()") {
		t.Errorf("an array element must not close unconditionally with the keeping end:\n%s", mod)
	}
	if strings.Contains(mod, `            if _e0 != "":`) {
		t.Errorf("a count:N leaf element must take the same last-element guard as a dynamic one:\n%s", mod)
	}
}

// A wrapper array's element id IS the array index (§5.1), so an element is PLACED
// at target[id] after gap-filling from the element default -- never appended. That
// is what restores an interior element the sparse rule omitted; appending would
// shorten the array by the size of every gap and would decode a REOPENED id as a
// second element instead of merging into the first (§7.4).
//
// The row collectors are the ones that had it wrong: a matrix row and a wrapper row
// were both appended id-blind. That was unreachable while every row was framed
// unconditionally, but an interior gap is now ordinary, and an appending collector
// shifts every later row down by one.
//
// The decoded length is highest present id + 1, exact because the last element is
// never elided. Nothing is filled in beyond it: a schema `count` is a capacity, so
// it bounds the id but never adds an element the wire did not carry.
func TestPythonWrapperElementsArePlacedByID(t *testing.T) {
	const src = `
version: 1
messages:
  vec:
    payload:
      objs: { id: 0, type: array, items: { type: struct, count: 4, fields: { k: { id: 0, type: u32 } } } }
      strs: { id: 1, type: array, items: { type: string, count: 3, maxlen: 8 } }
      mat:  { id: 2, type: array, items: { type: array, count: 2, items: { type: u32, count: 3 } } }
      rows: { id: 3, type: array, items: { type: array, count: 2, items: { type: string, maxlen: 4 } } }
      dyn:  { id: 4, type: array, items: { type: struct, fields: { k: { id: 0, type: u32 } } } }
`
	mod := string(genPy(t, schema(t, src), map[string]any{})["message.py"])

	for _, want := range []string{
		// struct elements: place, not append -- and the gap-fill that precedes it
		"                    while len(self.objs) <= _ef0.id:\n" +
			"                        self.objs.append(VecObjsElem())\n" +
			"                    self.objs[_ef0.id].deserialize(d)\n",
		// leaf elements, unchanged: they always got this right
		"                    while len(self.strs) <= _ef0.id:\n" +
			"                        self.strs.append(\"\")\n",
		// native matrix rows: read the row, check the elements against the declared
		// u32 width (§7.1), then place it at its id
		"                    _e0 = d.read_unsigned_array()\n" +
			"                    if any(_v > 4294967295 for _v in _e0):\n" +
			"                        raise SofaDecodeError(\"mat row element: value outside declared width u32\")\n" +
			"                    while len(self.mat) <= _ef0.id:\n" +
			"                        self.mat.append([])\n" +
			"                    self.mat[_ef0.id] = _e0\n",
		// wrapper rows: same again
		"                    while len(self.rows) <= _ef0.id:\n" +
			"                        self.rows.append([])\n" +
			"                    self.rows[_ef0.id] = _e0\n",
		// the over-index guard bounds every id-keyed fill
		"if _ef0.id >= 4:",
		"if _ef0.id >= 2:",
		// a count-less array is placed by id like every other, just unbounded
		"while len(self.dyn) <= _ef0.id:",
	} {
		if !strings.Contains(mod, want) {
			t.Errorf("message.py missing %q:\n%s", want, mod)
		}
	}
	// The defects this replaced: rows appended id-blind, and the N-refill that made
	// the superseded trailing elision lossless.
	for _, bad := range []string{
		"self.mat.append(_e0)",
		"self.rows.append(_e0)",
		"while len(self.objs) < 4:",
		"while len(self.strs) < 3:",
		"_pad_to(",
	} {
		if strings.Contains(mod, bad) {
			t.Errorf("message.py must no longer contain %q:\n%s", bad, mod)
		}
	}
}

// TestPythonWireArraySparsity is the byte-level statement of the whole change,
// executed against corelib-py. Every hex below is a regenerated shared test vector
// (the serialized_sparse form), so these are cross-language byte targets, not this
// backend's opinion:
//
//	array_string_trailing_default          ["a",""]      06020a610a0207
//	array_string_all_default               ["",""]       060a0207
//	array_string_leading_default           ["","x",""]   060a0a78120207
//	array_string_gap                       ["a","","c"]  06020a61120a6307
//	array_struct_interior_default_element  [{1},{},{3}]  06060001071600030707
//	array_struct_all_default_elements      [{},{}]       060e0707
//	array_unsigned_trailing_defaults       [1,2,0,0]     030401020000
//	array_of_string_arrays                 [["a"],[]]    0606020a61070e0707
//
// Every probe declares a `count` deliberately: a capacity must change none of it
// (§3). Round-tripping is exact -- ["a",""], ["a"] and [] are three distinct values
// with three distinct encodings, and nothing is added or dropped on the way back.
func TestPythonWireArraySparsity(t *testing.T) {
	corelib := os.Getenv("SOFAB_PY_CORELIB")
	if corelib == "" {
		t.Skip("set SOFAB_PY_CORELIB to a corelib-py checkout")
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not found")
	}
	for _, probe := range []struct {
		what  string
		def   string
		cases []struct{ in, wantHex, wantJSON string }
	}{
		{"count:N string elements", "{ type: string, count: 4, maxlen: 8 }", []struct{ in, wantHex, wantJSON string }{
			// interior gap at index 1, last element carries a value
			{`["a","","c"]`, "06020a61120a6307", `["a", "", "c"]`},
			// the trailing default IS the last element: written, so length 2 survives
			{`["a",""]`, "06020a610a0207", `["a", ""]`},
			// all-default: the interior one drops, the final one is written alone at
			// id 1 -- this is NOT the empty array
			{`["",""]`, "060a0207", `["", ""]`},
			// leading default leaves a gap, trailing default is written
			{`["","x",""]`, "060a0a78120207", `["", "x", ""]`},
			// no element at all: the wrapper stays contentless and the FIELD is
			// omitted (§2), decoding to the empty array -- length 0, not 4
			{`[]`, "", `[]`},
		}},
		{"count-less string elements", "{ type: string, maxlen: 8 }", []struct{ in, wantHex, wantJSON string }{
			{`["a",""]`, "06020a610a0207", `["a", ""]`},
			{`["",""]`, "060a0207", `["", ""]`},
			{`[]`, "", `[]`},
		}},
		{"count:N struct elements", "{ type: struct, count: 4, fields: { k: { id: 0, type: u32 } } }", []struct{ in, wantHex, wantJSON string }{
			// the interior all-default element is NOT framed: id 1 is a gap, and the
			// array still decodes at length 3 from id 2
			{`[{"k":1},{"k":0},{"k":3}]`, "06060001071600030707", `[{"k": 1}, {"k": 0}, {"k": 3}]`},
			// all-default elements: interior drops, the last keeps its empty frame
			{`[{"k":0},{"k":0}]`, "060e0707", `[{"k": 0}, {"k": 0}]`},
			// a single all-default element is the last one: an empty frame at id 0
			{`[{"k":0}]`, "06060707", `[{"k": 0}]`},
			// only element 0 set, in a count:4 array -- no trailing fill to 4
			{`[{"k":1}]`, "060600010707", `[{"k": 1}]`},
			{`[]`, "", `[]`},
		}},
		{"count:N u32 elements", "{ type: u32, count: 4 }", []struct{ in, wantHex, wantJSON string }{
			// the wire count IS the length: the trailing zeros are part of the value
			{`[1,2,0,0]`, "030401020000", `[1, 2, 0, 0]`},
			{`[1,2]`, "03020102", `[1, 2]`},
			// all-zero at length 4 is a length-4 array: it differs from the empty
			// default, so it stays on the wire (a capacity is not a minimum length)
			{`[0,0,0,0]`, "030400000000", `[0, 0, 0, 0]`},
			// only the EMPTY array is the field's default, and only it is omitted
			{`[]`, "", `[]`},
		}},
		{"wrapper rows", "{ type: array, count: 3, items: { type: string, maxlen: 8 } }", []struct{ in, wantHex, wantJSON string }{
			// the empty row is LAST, so it keeps its frame: length 2 survives
			{`[["a"],[]]`, "0606020a61070e0707", `[["a"], []]`},
			// now the empty row is INTERIOR -- not framed, an id gap, and the decoder
			// restores it as an empty row at index 0
			{`[[],["a"]]`, "060e020a610707", `[[], ["a"]]`},
			{`[["a","b"],[],["c"]]`, "0606020a610a0a620716020a630707", `[["a", "b"], [], ["c"]]`},
		}},
		{"native rows", "{ type: array, count: 3, items: { type: u32, count: 3 } }", []struct{ in, wantHex, wantJSON string }{
			// a native row has no frame: the LAST empty row is written as an empty
			// count-prefixed array at id 1 (0b 00)
			{`[[1],[]]`, "060301010b0007", `[[1], []]`},
			// the interior empty row is dropped, so id 0 is a gap
			{`[[],[1]]`, "060b010107", `[[], [1]]`},
			{`[[1,2],[],[3]]`, "060302010213010307", `[[1, 2], [], [3]]`},
		}},
	} {
		dir := pyProject(t, "version: 1\nmessages:\n  vec:\n    payload:\n"+
			"      arr: { id: 0, type: array, items: "+probe.def+" }\n")
		for _, c := range probe.cases {
			in := `{"arr":` + c.in + `}`
			wire := pyHarness(t, corelib, dir, "encode", []byte(in))
			if got := hex.EncodeToString(wire); got != c.wantHex {
				t.Errorf("%s: encode %s: got %s, want %s", probe.what, in, got, c.wantHex)
				continue
			}
			out := pyHarness(t, corelib, dir, "decode", wire)
			var back struct {
				Arr json.RawMessage `json:"arr"`
			}
			if err := json.Unmarshal(out, &back); err != nil {
				t.Fatalf("%s: decode %s: %v (%s)", probe.what, in, err, out)
			}
			if got := string(back.Arr); got != c.wantJSON {
				t.Errorf("%s: round-trip %s: got %s, want %s", probe.what, in, got, c.wantJSON)
			}
		}
	}
}

// pyProject generates a runnable Python project for src into a temp dir.
func pyProject(t *testing.T, src string) string {
	t.Helper()
	dir := t.TempDir()
	for path, content := range genPy(t, schema(t, src), map[string]any{"emit": "project"}) {
		if err := os.WriteFile(filepath.Join(dir, path), content, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// pyHarness runs the generated harness against corelib-py and returns its stdout.
func pyHarness(t *testing.T, corelib, dir, mode string, in []byte) []byte {
	t.Helper()
	cmd := exec.Command("python3", "harness.py", mode, "vec")
	cmd.Dir = dir
	cmd.Stdin = bytes.NewReader(in)
	cmd.Env = append(os.Environ(), "PYTHONPATH="+filepath.Join(corelib, "src"))
	out, err := cmd.Output()
	if err != nil {
		stderr := ""
		if ee, ok := err.(*exec.ExitError); ok {
			stderr = string(ee.Stderr)
		}
		t.Fatalf("harness %s %q: %v\n%s", mode, in, err, stderr)
	}
	return out
}

// pyHarnessTry runs the generated harness like pyHarness but WITHOUT failing the
// test on a non-zero exit: it returns stderr and whether the run succeeded, so a
// decode that MUST be rejected can be asserted on.
func pyHarnessTry(t *testing.T, corelib, dir, mode string, in []byte) (string, bool) {
	t.Helper()
	cmd := exec.Command("python3", "harness.py", mode, "vec")
	cmd.Dir = dir
	cmd.Stdin = bytes.NewReader(in)
	cmd.Env = append(os.Environ(), "PYTHONPATH="+filepath.Join(corelib, "src"))
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stderr.String(), err == nil
}

// TestPythonNestedNativeRowCountBound: a nested NATIVE row (array<array<u32>>,
// array<array<fp32>>) declares its own `count`, and a wire element count M above
// that capacity N is INVALID (MESSAGE_SPEC §3+§7.1) exactly as for a top-level
// native array — the capacity bounds M, and the message is rejected whole, never
// truncated and never kept.
//
// A row is read through the very same native branch as a top-level array, and
// that branch used to ignore the cap it was handed: the bound was emitted OUTSIDE
// it, once, for the top-level field only, so every nested row was unbounded and
// Python accepted rows every other backend rejects. A row's count header is the
// row ELEMENT's header in the enclosing wrapper loop (`_ef<depth-1>.count`) —
// the only place a row's count can be bounded at all — so the guard belongs with
// the native read, at whatever depth that read happens.
//
// Emitting it before the read also keeps the §5.2 ordering the top-level guard
// already had: a row that is BOTH over-count and truncated is INVALID, not
// INCOMPLETE, because the count is known from the header before any element is
// consumed.
func TestPythonNestedNativeRowCountBound(t *testing.T) {
	const src = `
version: 1
messages:
  M:
    payload:
      numrows: { id: 0, type: array, items: { type: array, count: 2, items: { type: u32, count: 3 } } }
      fprows:  { id: 1, type: array, items: { type: array, count: 2, items: { type: fp32, count: 2 } } }
      dynrows: { id: 2, type: array, items: { type: array, count: 2, items: { type: u32 } } }
`
	mod := string(genPy(t, schema(t, src), map[string]any{})["message.py"])
	for _, want := range []string{
		"if _ef0.count > 3:",
		`raise SofaDecodeError("numrows row: array count above schema capacity 3")`,
		"if _ef0.count > 2:",
		`raise SofaDecodeError("fprows row: array count above schema capacity 2")`,
	} {
		if !strings.Contains(mod, want) {
			t.Errorf("message.py missing nested-row count bound %q:\n%s", want, mod)
		}
	}
	// A count-less row is unbounded — no guard invented for it.
	if strings.Contains(mod, `raise SofaDecodeError("dynrows row: array count`) {
		t.Errorf("a count-less nested row must carry no over-count guard:\n%s", mod)
	}
	// INVALID must dominate INCOMPLETE (§5.2): the guard precedes the read that
	// would otherwise raise SofaIncompleteError on a truncated tail.
	guard := strings.Index(mod, "if _ef0.count > 3:")
	read := strings.Index(mod, "_e0 = d.read_unsigned_array()")
	if guard < 0 || read < 0 || guard > read {
		t.Errorf("the row count guard must precede the row read (guard=%d read=%d)", guard, read)
	}

	// Lockstep with the on-demand import: when the ONLY counted native array in
	// the schema is a nested row (the outer array count-less), SofaDecodeError is
	// still raised and so must still be imported — otherwise the guard is a
	// NameError at decode time.
	rows := string(genPy(t, schema(t, `
version: 1
messages:
  M:
    payload:
      rows: { id: 0, type: array, items: { type: array, items: { type: u32, count: 3 } } }
`), map[string]any{})["message.py"])
	if !strings.Contains(rows, "from sofab import Encoder, Decoder, SofaDecodeError, WireType\n") {
		t.Errorf("a nested-row-only over-count guard still needs SofaDecodeError imported:\n%s", rows)
	}

	// And the behaviour itself, against corelib-py.
	corelib := os.Getenv("SOFAB_PY_CORELIB")
	if corelib == "" {
		t.Skip("set SOFAB_PY_CORELIB to a corelib-py checkout for the decode half")
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not found")
	}
	dir := pyProject(t, "version: 1\nmessages:\n  vec:\n    payload:\n"+
		"      arr: { id: 0, type: array, items: { type: array, count: 2, items: { type: u32, count: 3 } } }\n")
	for _, c := range []struct {
		what   string
		hex    string
		reject bool
	}{
		// 06 seq_begin(id 0) | 03 row(id 0, unsigned array) 03 count | 01 02 03 | 07 end
		{"row at the capacity", "06030301020307", false},
		// ...and one element past it: M = 4 > N = 3 is INVALID.
		{"row one past the capacity", "0603040102030407", true},
		// the row at index 1 (header 0b) is bounded too, not just the first one
		{"second row past the capacity", "060301010b040102030407", true},
		// over-count AND truncated: INVALID wins over INCOMPLETE (§5.2)
		{"row over-count and truncated", "06030401", true},
	} {
		wire, err := hex.DecodeString(c.hex)
		if err != nil {
			t.Fatal(err)
		}
		stderr, ok := pyHarnessTry(t, corelib, dir, "decode", wire)
		if c.reject {
			if ok {
				t.Errorf("%s (%s): decode must be INVALID, it succeeded", c.what, c.hex)
			} else if !strings.Contains(stderr, "SofaDecodeError") {
				t.Errorf("%s (%s): must be INVALID (SofaDecodeError), got:\n%s", c.what, c.hex, stderr)
			}
		} else if !ok {
			t.Errorf("%s (%s): must decode, got:\n%s", c.what, c.hex, stderr)
		}
	}
}

// MESSAGE_SPEC §7.1 + documentation#32 (issue #266, Crucible F-0033 / G-0026):
// the declared integer width is a normative VALIDITY bound. Python's int is
// unbounded, so nothing masked the value here — the defect was that an
// out-of-range value was simply KEPT — and the raise aborts the decode.
func TestPythonDeclaredWidthIsAValidityBound(t *testing.T) {
	const src = `
version: 1
messages:
  W:
    payload:
      a_u8:   { id: 0, type: u8 }
      c_u32:  { id: 2, type: u32 }
      d_u64:  { id: 3, type: u64 }
      e_i8:   { id: 4, type: i8 }
      g_i32:  { id: 6, type: i32 }
      h_i64:  { id: 7, type: i64 }
      arr_u8: { id: 8, type: array, items: { type: u8, count: 4 } }
`
	got := string(genPy(t, schema(t, src), map[string]any{})["message.py"])
	for _, want := range []string{
		"self.a_u8 = d.unsigned()\n                if self.a_u8 > 255:\n" +
			`                    raise SofaDecodeError("a_u8: value outside declared width u8")`,
		"self.c_u32 = d.unsigned()\n                if self.c_u32 > 4294967295:\n" +
			`                    raise SofaDecodeError("c_u32: value outside declared width u32")`,
		"self.e_i8 = d.signed()\n                if self.e_i8 < -128 or self.e_i8 > 127:\n" +
			`                    raise SofaDecodeError("e_i8: value outside declared width i8")`,
		"self.g_i32 = d.signed()\n                if self.g_i32 < -2147483648 or self.g_i32 > 2147483647:\n" +
			`                    raise SofaDecodeError("g_i32: value outside declared width i32")`,
		// The array arrives whole, so one scan over the elements decides it.
		"if any(_v > 255 for _v in self.arr_u8):\n" +
			`                    raise SofaDecodeError("arr_u8 element: value outside declared width u8")`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("message.py missing width guard %q:\n%s", want, got)
		}
	}
	// 64-bit destinations read bare: the next line is the following field's read,
	// not a bound check.
	for _, want := range []string{
		"self.d_u64 = d.unsigned()\n            elif",
		"self.h_i64 = d.signed()\n            elif",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("message.py: a 64-bit destination must read unguarded (%q):\n%s", want, got)
		}
	}
}
