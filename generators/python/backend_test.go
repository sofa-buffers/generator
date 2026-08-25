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
		// example.yaml has count-bearing native arrays and bounded string/blob
		// fields, so the flat visitor overrides on_field and needs Field/WireType/
		// FixlenSubtype alongside the always-present decode names.
		"from sofab import Decoder, Encoder, Field, FixlenSubtype, SofaDecodeError, SofaIncompleteError, SofaLimitError, Status, Visitor, WireType",
		"@dataclass",
		"class Myfirstmessage:",
		"def serialize(self, e: Encoder)",
		"class _MyfirstmessageVisitor(Visitor):",
		"def decoder(cls) -> _StreamDecoder:",
		"class MyfirstmessageSomeenum(IntEnum):",
		"def to_jsonable(self)",
		"e.write_sequence_begin_lazy(", // every sequence opens lazily (MESSAGE_SPEC S2)
		// The schema count is DECLARED, and the corelib applies it at the count
		// header (generator#100/#216/#406).
		"    def on_schema_bound(self, fid: int, n: int) -> int:",
		"                return 4  # someuintarray: schema count",
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
		// AS CONFIGURED. It used to be raised to barr's schema count (100000) so a
		// schema-bounded field larger than the cap stayed decodable; generated code
		// now declares those fields with d.schema_bounded() instead, so the cap can
		// stay tight around what the schema left open (generator#325).
		"MAX_DYN_ARRAY_COUNT = 65536",
		"MAX_DYN_STRING_LEN = 4096",
		// §6.2.1 keeps the caps OUT of the Decoder: they are applied in on_field,
		// where the schema is known, so they cannot bind a schema-bounded field.
		`raise SofaLimitError("array count %d exceeds max_array_count %d" % (fld.count, MAX_DYN_ARRAY_COUNT))`,
		`raise SofaLimitError("string length %d exceeds max_string_len %d" % (fld.size, MAX_DYN_STRING_LEN))`,
	} {
		if !strings.Contains(mod, want) {
			t.Errorf("message.py missing %q", want)
		}
	}
	if strings.Contains(mod, "MAX_DYN_BLOB_LEN") {
		t.Error("inert blob limit must not be emitted (no unbounded blob)")
	}
	if strings.Contains(mod, "max_array_count=") || strings.Contains(mod, "max_string_len=") {
		t.Error("a Decoder-level cap also binds schema-bounded fields, which §6.2.1 forbids")
	}

	// No keys configured -> the target's finite DEFAULTS, not "unlimited"
	// (§9.5, generator#385). Python is on the server tier, and the caps still
	// stay out of the Decoder: liveness and placement are unchanged, only the
	// values' origin is.
	plain := string(genPy(t, s, map[string]any{})["message.py"])
	for _, want := range []string{
		"MAX_DYN_ARRAY_COUNT = 65536",
		"MAX_DYN_STRING_LEN = 1048576",
		"d = Decoder(visitor=_DynVisitor(o))",
	} {
		if !strings.Contains(plain, want) {
			t.Errorf("default limits missing %q", want)
		}
	}
	if strings.Contains(plain, "MAX_DYN_BLOB_LEN") {
		t.Error("inert blob limit must not be emitted (no unbounded blob)")
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
	// FixlenSubtype rides along: the string/blob ELEMENT bounds name it even though
	// no *field* here is fixlen (generator#246), so the full line is asserted — a
	// prefix match would pass either way and let the missing name through. There is
	// no WireType: this schema has no native array, so nothing compares one.
	if !strings.Contains(mod, "from sofab import Decoder, Encoder, Field, FixlenSubtype, SofaDecodeError, SofaIncompleteError, SofaLimitError, Status, Visitor\n") {
		t.Errorf("message.py needs SofaDecodeError (over-index guard) AND FixlenSubtype (element guard) imported, else NameError at decode:\n%s", mod)
	}
	for _, want := range []string{
		// A VALUE element (string/blob) is bounded at the header, in on_field, so
		// the verdict precedes the payload (§5.2).
		`if fld.id >= 4:`,
		`raise SofaDecodeError("bs: array index above schema capacity 4")`,
		`raise SofaDecodeError("bb: array index above schema capacity 3")`,
		// A STRUCT element opens a scope instead, and no on_field precedes a
		// sequence header — so its bound sits in on_sequence_begin, which is still
		// ahead of every byte of the element.
		`if fid >= 2:`,
		`raise SofaDecodeError("bp: array index above schema capacity 2")`,
	} {
		if !strings.Contains(mod, want) {
			t.Errorf("message.py missing over-index guard %q", want)
		}
	}
	// Dynamic string array keeps every index — no guard raised for it.
	if strings.Contains(mod, `raise SofaDecodeError("ds: array index above schema capacity`) {
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
      barr: { id: 4, type: array, items: { type: blob, maxlen: 5 } }
`
	mod := string(genPy(t, schema(t, src), map[string]any{})["message.py"])

	// (a) The maxlen guard raises SofaDecodeError, so the on-demand import MUST be
	// present even though this schema has no counted native/wrapper array — the
	// import bug this test guards against.
	// Asserted as the FULL line (FixlenSubtype included — this schema has string
	// and blob fields, and a string wrapper element): a prefix match cannot tell a
	// complete import line from a truncated one (generator#246).
	if !strings.Contains(mod, "from sofab import Decoder, Encoder, Field, FixlenSubtype, SofaDecodeError, SofaIncompleteError, SofaLimitError, Status, Visitor\n") {
		t.Errorf("message.py must import SofaDecodeError for the maxlen guard (else NameError at decode):\n%s", mod)
	}

	// Every bound below is DECLARED in on_schema_bound, which the decoder asks at
	// the count/length HEADER -- before a payload byte is read. That is what keeps
	// the verdict INVALID on a message cut right after the length word, where §5.2
	// makes INVALID dominate INCOMPLETE (generator#267 / F-0043, and #377 / F-0062
	// at the element). The §7.3 subtype test moved one hook EARLIER, into
	// on_field, where a header carrying a different fixlen kind at this id is
	// declined as the skipped field it is -- because on_schema_bound is told only
	// the id and the announced length, so a bound it declares would otherwise
	// reach a field that is not this field's value (generator#406).
	for _, want := range []string{
		// (b) scalar string: bound the wire byte length the header declares, never
		// a re-encode of the decoded str (#155).
		`                return 8  # s: schema maxlen`,
		`if fld.subtype != FixlenSubtype.STRING:
                    return False  # s: header is not the declared type -- skip it`,
		// (b) scalar blob: same shape, same reason.
		`                return 8  # b: schema maxlen`,
		`if fld.subtype != FixlenSubtype.BLOB:
                    return False  # b: header is not the declared type -- skip it`,
		// (c) bounded wrapper string element (maxlen 5), at its own array scope --
		// every element shares one declared type, so neither the declaration nor
		// the tag test carries an id test.
		`            return 5  # arr: schema element maxlen`,
		`if fld.subtype != FixlenSubtype.STRING:
                return False  # arr: header is not the declared type -- skip it`,
		// (c) bounded wrapper BLOB element (maxlen 5).
		`            return 5  # barr: schema element maxlen`,
		`if fld.subtype != FixlenSubtype.BLOB:
                return False  # barr: header is not the declared type -- skip it`,
	} {
		if !strings.Contains(mod, want) {
			t.Errorf("message.py missing maxlen declaration %q", want)
		}
	}

	// (d) the string maxlen check must never re-encode the decoded str (#155).
	if strings.Contains(mod, `.encode("utf-8")`) {
		t.Error(`string maxlen check must not re-encode via .encode("utf-8") (#155)`)
	}

	// (e) the unbounded string field declares no bound and is declined by nothing:
	// it is the receiver caps' business, not the schema's.
	if strings.Contains(mod, `# us:`) {
		t.Error("unbounded string must not declare a schema bound")
	}
}

// TestPythonArrayElemBound covers generator#267's element position: an array
// element outside its DECLARED WIDTH is INVALID (§7.1) and, established by its
// own bytes, dominates a truncation behind it (§5.2). The `any(... for _v in ...)`
// scan decides an array that arrives and never runs for one that does not, so the
// bound also travels WITH the read — the same place the schema count and a blob's
// maxlen already go.
func TestPythonArrayElemBound(t *testing.T) {
	const src = `
version: 1
messages:
  M:
    payload:
      ua:  { id: 0, type: array, items: { type: u8,  count: 4 } }
      sa:  { id: 1, type: array, items: { type: i16, count: 4 } }
      wa:  { id: 2, type: array, items: { type: u64, count: 4 } }
      da:  { id: 3, type: array, items: { type: u32 } }
      fa:  { id: 4, type: array, items: { type: fp32, count: 4 } }
`
	mod := string(genPy(t, schema(t, src), map[string]any{})["message.py"])

	// The bound is STATED to the decoder at the header, as the (min, max) pair it
	// applies at each element, rather than scanned out of the assembled list.
	for _, want := range []string{
		`return (None, None, 255)`,
		`return (None, -32768, 32767)`,
		// The width is a property of the element TYPE, not of the array length,
		// so a count-less array carries it too.
		`return (None, None, 4294967295)`,
	} {
		if !strings.Contains(mod, want) {
			t.Errorf("message.py missing element bound %q:\n%s", want, mod)
		}
	}
	// u64 spans the value domain the corelib already hands over, so it states no
	// width — and an fp32 array is not offered on_array_begin at all.
	for _, unwanted := range []string{
		`raise SofaDecodeError("wa element:`,
		`raise SofaDecodeError("fa element:`,
		// The verdict is the decoder's, at each element. A post-assembly scan is a
		// second, weaker one: it cannot reject an element a truncation stops the
		// array from ever completing (§5.2 INVALID over INCOMPLETE).
		`for _v in value`,
	} {
		if strings.Contains(mod, unwanted) {
			t.Errorf("a u64/fp32 array must carry no element bound, and no kind a post-assembly scan (%q):\n%s", unwanted, mod)
		}
	}
}

// TestPythonArrayElemBoundIsPostAssembly pins a KNOWN GAP so it cannot be
// mistaken for a passing guarantee.
//
// The removed pull API took the declared element width as an argument to
// read_*_array, so the corelib rejected an out-of-width element AT that element
// — which kept the verdict INVALID on a message truncated behind it, as §5.2
// requires (generator#267 / Crucible F-0043 width_elem_trunc).
//
// corelib-py's visitor surface has no equivalent: on_*_array hands over a list
// that has already fully arrived, and on_field carries only the count, not the
// values. So the scan can only run on an array that assembles, and a truncated
// array carrying an out-of-width element now reports INCOMPLETE where INVALID is
// owed. The scan below is therefore the whole bound, not a belt-and-braces
// second one — asserted here so the day corelib-py grows an element-width hook
// this test fails and the guard moves back ahead of the payload.
func TestPythonArrayElemBoundIsAtTheHeader(t *testing.T) {
	const src = `
version: 1
messages:
  M:
    payload:
      ua: { id: 0, type: array, items: { type: u8, count: 4 } }
`
	mod := string(genPy(t, schema(t, src), map[string]any{})["message.py"])
	const want = `    def on_unsigned_array(self, fid: int, value: list[int]) -> None:
        c = self._c
        if c == _L_M:
            if fid == 0:
                self._o.ua = value`
	if !strings.Contains(mod, want) {
		t.Errorf("the typed hook must only STORE -- the width was applied per element:\n%s", mod)
	}
	// Both the count and the element width are settled at the header, ahead of the
	// first element: the count is DECLARED in on_schema_bound, which the decoder
	// asks first, and the width is handed to it in on_array_begin rather than
	// checked here.
	const hdr = `    def on_array_begin(self, fid: int, wtype: WireType, count: int):`
	if !strings.Contains(mod, hdr) {
		t.Errorf("an integer array must state its element width in on_array_begin:\n%s", mod)
	}
	if !strings.Contains(mod, "                return 4  # ua: schema count") {
		t.Errorf("the array COUNT bound must be declared in on_schema_bound:\n%s", mod)
	}
	// ...and nowhere else: a second copy of the rule is what generator#406 removed.
	if strings.Contains(mod, "if count > 4:") {
		t.Errorf("the schema count must not be re-checked in on_array_begin:\n%s", mod)
	}
	if !strings.Contains(mod, "                return (None, None, 255)") {
		t.Errorf("the declared element width must be STATED for the decoder to apply:\n%s", mod)
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
		// The bound itself is untouched -- that is all `count` still does. A
		// native array DECLARES it at the header; a wrapper array has no count
		// word on the wire, so its index is bounded in on_field instead.
		"                return 5  # shortDflt: schema count",
		"if fld.id >= 3:",
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

// TestPythonWireTypeGuard pins how MESSAGE_SPEC §7.3 is satisfied on the visitor
// surface (generator#174).
//
// The pull decoder needed an explicit guard: the caller chose which typed reader
// to run, so a header contradicting the schema had to be detected and skipped
// before the wrong reader consumed the wrong byte count and desynchronized the
// stream. The flat visitor inverts that. The CORELIB picks the hook from the
// wire type, so a contradicting field is delivered to a different hook, where its
// (location, id) matches no arm and it falls through — skipped, exactly like an
// unknown id, with no generated code at all. A sequence header at a scalar id
// reaches on_sequence_begin, matches nothing, and is declined with False, which
// makes the corelib skip the whole subtree.
//
// The one place a wire-type test survives is on_field, where a header-time bound
// has to know the header really is the field it bounds — §7.3 again, in the only
// form the visitor still needs it.
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
	// No SofaLimitError: every field in this schema is bounded, so no receiver
	// cap is live and the name would be dead (§9.5, generator#385).
	if !strings.Contains(mod, "from sofab import Decoder, Encoder, Field, FixlenSubtype, SofaDecodeError, SofaIncompleteError, Status, Visitor, WireType") {
		t.Errorf("message.py missing the full decode import line:\n%s", mod)
	}
	for _, want := range []string{
		// Each kind lands in the hook the corelib routes its wire type to, and
		// nowhere else — that routing IS the §7.3 dispatch.
		"    def on_unsigned(self, fid: int, value: int) -> None:",
		"    def on_signed(self, fid: int, value: int) -> None:",
		"    def on_float32(self, fid: int, value: float) -> None:",
		"    def on_float64(self, fid: int, value: float) -> None:",
		"    def on_string(self, fid: int, value: str) -> None:",
		"    def on_bytes(self, fid: int, value: bytes) -> None:",
		"    def on_unsigned_array(self, fid: int, value: list[int]) -> None:",
		"    def on_signed_array(self, fid: int, value: list[int]) -> None:",
		"    def on_float32_array(self, fid: int, value: list[float]) -> None:",
		// A schema-bounded id is DECLINED in on_field when the header is not the
		// declared type: fp32/fp64/string/blob all share FIXLEN, so only the
		// subtype separates them, and a fixlen array shares ARRAY_FIXLEN the same
		// way. §7.3 wins over the schema bound, so the tag is decided first and
		// the bound reaches only a field that survives it.
		"if fld.subtype != FixlenSubtype.STRING:",
		"if fld.subtype != FixlenSubtype.BLOB:",
		"if fld.type != WireType.ARRAY_UNSIGNED:",
		"if fld.type != WireType.ARRAY_SIGNED:",
		"if fld.type != WireType.ARRAY_FIXLEN or fld.subtype != FixlenSubtype.FP32:",
		// on_array_begin is left with the declared element WIDTH alone.
		"    def on_array_begin(self, fid: int, wtype: WireType, count: int):",
		"                return (None, None, 4294967295)",
		// Every bound is one number, in one place.
		"                return 2  # i: schema count",
		"                return 2  # k: schema count",
		// An unmatched sequence is declined, which skips its whole subtree.
		"        return False",
	} {
		if !strings.Contains(mod, want) {
			t.Errorf("message.py missing %q\n%s", want, mod)
		}
	}
	// No skip plumbing survives beyond the §7.3 decline: the corelib does the
	// skipping, generated code only declines the field.
	for _, bad := range []string{"d.skip()", "self._skip"} {
		if strings.Contains(mod, bad) {
			t.Errorf("the visitor must carry no explicit skip plumbing, found %q", bad)
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
	if !strings.Contains(mod, "from sofab import Decoder, Encoder, SofaDecodeError, SofaIncompleteError, Status, Visitor\n") {
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
		// ELEMENT. Under the visitor a subtype is compared where a BOUND has to
		// know the header is really the field it bounds -- so the element carries
		// its maxlen here, which is what puts FixlenSubtype in the body.
		{"bounded wrapper string array", true, `
      tags: { id: 0, type: array, items: { type: string, maxlen: 8 } }
      n:    { id: 1, type: u32 }`},
		{"bounded wrapper blob array", true, `
      parts: { id: 0, type: array, items: { type: blob, maxlen: 8 } }
      n:     { id: 1, type: u32 }`},
		// Nested rows: the bound sits one (or two) levels below the field.
		{"bounded nested string rows", true, `
      rows: { id: 0, type: array, items: { type: array, items: { type: string, maxlen: 4 } } }
      n:    { id: 1, type: u32 }`},
		{"counted nested fp32 rows", true, `
      grid: { id: 0, type: array, items: { type: array, items: { type: fp32, count: 3 } } }
      n:    { id: 1, type: u32 }`},
		{"doubly nested bounded blob rows", true, `
      cube: { id: 0, type: array, items: { type: array, items: { type: array, items: { type: blob, maxlen: 4 } } } }
      n:    { id: 1, type: u32 }`},
		// A bounded string reached through a STRUCT element is bounded inside that
		// struct's own visitor, which the scope walk reaches through the element.
		{"bounded string inside a struct element", true, `
      items: { id: 0, type: array, items: { type: struct, fields: { s: { id: 0, type: string, maxlen: 8 } } } }
      n:     { id: 1, type: u32 }`},
		// Negatives — the import must stay out, or every module carries an unused
		// name (the reason the gate exists at all). UNBOUNDED fixlen fields are
		// negatives now: with no bound there is nothing for on_field to frame, and
		// §7.3 needs no code of its own on the visitor surface.
		// An unbounded string element is a POSITIVE again since every target
		// carries a finite default string cap (§9.5, generator#385): the guard
		// keys on fld.subtype == FixlenSubtype.STRING, so the name is used.
		{"unbounded wrapper string array", true, `
      tags: { id: 0, type: array, items: { type: string } }
      n:    { id: 1, type: u32 }`},
		{"unbounded fp32 rows", false, `
      grid: { id: 0, type: array, items: { type: array, items: { type: fp32 } } }
      n:    { id: 1, type: u32 }`},
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
		// struct elements: gap-fill, then descend into the element the index names
		// -- the index register is what carries it into the element scope, and
		// decoding INTO the object already there gives the §7.4 merge for free.
		"            _t = self._o.objs\n" +
			"            while len(_t) <= fid:\n" +
			"                _t.append(VecObjsElem())\n" +
			"            self._ix1 = fid\n",
		"                self._o.objs[self._ix1].k = value",
		// leaf elements: placed at the index, never appended
		"            _t = self._o.strs\n" +
			"            while len(_t) <= fid:\n" +
			"                _t.append(\"\")\n" +
			"            _t[fid] = value",
		// native matrix rows: gap-fill, then place the row at its index. The row's
		// elements were bounded against the declared u32 width one step earlier,
		// in on_array_begin (§7.1).
		"            _t = self._o.mat\n" +
			"            while len(_t) <= fid:\n" +
			"                _t.append([])\n" +
			"            _t[fid] = value",
		// wrapper rows: the row itself is a scope, and its own elements are placed
		// through the row the outer index register selected
		"            _t = self._o.rows[self._ix5]\n" +
			"            while len(_t) <= fid:\n" +
			"                _t.append(\"\")\n" +
			"            _t[fid] = value",
		// the over-index bound guards every id-keyed fill: at the header for a
		// value element, in on_sequence_begin for one that opens a scope
		"            if fid >= 4:",
		"            if fld.id >= 2:",
		// a count-less array is placed by id like every other, just unbounded
		"            _t = self._o.dyn\n            while len(_t) <= fid:",
	} {
		if !strings.Contains(mod, want) {
			t.Errorf("message.py missing %q:\n%s", want, mod)
		}
	}
	// The defects this replaced: rows appended id-blind, and the N-refill that made
	// the superseded trailing elision lossless.
	for _, bad := range []string{
		"self._o.mat.append(value)",
		"self._o.rows.append(value)",
		"while len(_t) < 4:",
		"while len(_t) < 3:",
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
		// Every element of a row scope shares one declared type, so the row count
		// is declared once for the whole scope, with no index test in front of it.
		"            return 3  # numrows row: schema count",
		"            return 2  # fprows row: schema count",
		// ...and the §7.3 tag test that has to precede it, one hook earlier.
		"if fld.type != WireType.ARRAY_UNSIGNED:",
		"if fld.type != WireType.ARRAY_FIXLEN or fld.subtype != FixlenSubtype.FP32:",
	} {
		if !strings.Contains(mod, want) {
			t.Errorf("message.py missing nested-row count bound %q:\n%s", want, mod)
		}
	}
	// A count-less row is unbounded — no bound invented for it.
	if strings.Contains(mod, "# dynrows row: schema count") {
		t.Errorf("a count-less nested row must declare no bound:\n%s", mod)
	}
	// INVALID must dominate INCOMPLETE (§5.2). The bound is declared in
	// on_schema_bound, which the decoder asks at the row's own count header --
	// before an element is decoded -- so a truncated tail cannot downgrade the
	// verdict. Pinned as "the bound is at the header, not in the value hook": the
	// value hook only ever sees a row that fully arrived.
	onSchemaBound := strings.Index(mod, "    def on_schema_bound(self, fid: int, n: int) -> int:")
	guard := strings.Index(mod, "            return 3  # numrows row: schema count")
	if onSchemaBound < 0 || guard < onSchemaBound {
		t.Errorf("the row count bound must sit inside on_schema_bound (hook=%d guard=%d)", onSchemaBound, guard)
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
	if !strings.Contains(rows, "from sofab import Decoder, Encoder, Field, SofaDecodeError, SofaIncompleteError, SofaLimitError, Status, Visitor, WireType\n") {
		t.Errorf("a nested-row-only §7.3 tag test still needs WireType imported:\n%s", rows)
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
		"                if value > 255:\n" +
			`                    raise SofaDecodeError("a_u8: value outside declared width u8")` +
			"\n                self._o.a_u8 = value",
		"                if value > 4294967295:\n" +
			`                    raise SofaDecodeError("c_u32: value outside declared width u32")` +
			"\n                self._o.c_u32 = value",
		"                if value < -128 or value > 127:\n" +
			`                    raise SofaDecodeError("e_i8: value outside declared width i8")` +
			"\n                self._o.e_i8 = value",
		"                if value < -2147483648 or value > 2147483647:\n" +
			`                    raise SofaDecodeError("g_i32: value outside declared width i32")` +
			"\n                self._o.g_i32 = value",
		// An ARRAY's elements are bounded one step earlier, at the count header: the
		// pair is handed to the decoder, which applies it as each element is read.
		// A scan of the assembled list would come too late to reject an element a
		// truncation stops the array from ever completing (§5.2).
		"                return (None, None, 255)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("message.py missing width guard %q:\n%s", want, got)
		}
	}
	// 64-bit destinations store bare: the whole arm is the assignment.
	for _, want := range []string{
		"            elif fid == 3:\n                self._o.d_u64 = value",
		"            elif fid == 7:\n                self._o.h_i64 = value",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("message.py: a 64-bit destination must store unguarded (%q):\n%s", want, got)
		}
	}
}

// TestPythonCallerOwnedEncodeBuffer: CORELIB_PLAN §5.1 makes every output buffer
// the CALLER's — a corelib allocates nothing and grows nothing — and generated
// code is the caller, so it allocates and hands the storage in. Which shape that
// takes is decided by the SCHEMA, so both arms are pinned here.
//
// A BOUNDED message has a worst case, so one exactly-sized buffer with no sink
// is enough and MAX_SIZE is the schema's own number.
func TestPythonCallerOwnedEncodeBufferBounded(t *testing.T) {
	const src = `
version: 1
messages:
  B:
    payload:
      a: { id: 0, type: array, items: { type: u32, count: 4 } }
`
	got := string(genPy(t, schema(t, src), map[string]any{})["message.py"])
	for _, want := range []string{
		"    MAX_SIZE = 22",
		"        buf = bytearray(B.MAX_SIZE)",
		"        e = Encoder.over_buffer(buf, 0)",
		"        return bytes(memoryview(buf)[: e.bytes_used()])",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("bounded encode must own its buffer, missing %q:\n%s", want, got)
		}
	}
	for _, bad := range []string{
		"Encoder()",      // the corelib installing its own scratch
		"e.getvalue()",   // and handing back storage it grew
		"MAX_SIZE_LIMIT", // a derived size is not an imposed ceiling
		"out.append",     // no sink: the one buffer holds the message
	} {
		if strings.Contains(got, bad) {
			t.Errorf("bounded encode must not contain %q:\n%s", bad, got)
		}
	}
	// The constant must stay a plain class attribute: annotating it would make
	// @dataclass treat it as a field, putting it in __init__ and on the wire.
	if strings.Contains(got, "MAX_SIZE:") {
		t.Error("MAX_SIZE must be unannotated, or @dataclass turns it into a field")
	}
}

// The unbounded arm: no worst case exists, so max_message_size is a CEILING and
// must not size the buffer — a message larger than it is legitimate. The shape is
// a fixed scratch drained into caller-owned storage instead.
func TestPythonCallerOwnedEncodeBufferUnbounded(t *testing.T) {
	const src = `
version: 1
messages:
  U:
    payload:
      a: { id: 0, type: array, items: { type: u32 } }
`
	got := string(genPy(t, schema(t, src), map[string]any{"max_message_size": 512})["message.py"])
	for _, want := range []string{
		"    MAX_SIZE_LIMIT = 512",
		"    MAX_SIZE = MAX_SIZE_LIMIT",
		"        out: list[bytes] = []",
		"        scratch = bytearray(512)",
		"        e.flush()",
		// The sink COPIES. §5.1.6 hands it the installed buffer -- a memoryview
		// over `scratch`, which the encoder goes on writing into -- rather than a
		// snapshot, so appending the view itself would append the same live window
		// each time and every piece would alias the scratch's final contents.
		`        e = Encoder.over_buffer(scratch, 0, lambda _v: out.append(bytes(_v)))`,
		`        return out[0] if len(out) == 1 else b"".join(out)`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("unbounded encode must stream into caller storage, missing %q:\n%s", want, got)
		}
	}
	// The ceiling must never become a buffer size: that would refuse a larger
	// message the caller legitimately built.
	if strings.Contains(got, "bytearray(U.MAX_SIZE)") {
		t.Errorf("an imposed ceiling must not size the encode buffer:\n%s", got)
	}
	if strings.Contains(got, "Encoder()") || strings.Contains(got, "getvalue()") {
		t.Errorf("the corelib must not be left owning the buffer:\n%s", got)
	}
	// The bare `out.append` this replaced kept the corelib's view, so every piece
	// aliased one buffer -- silently correct for a single-drain message and wrong
	// for every message that filled the scratch more than once.
	if strings.Contains(got, "Encoder.over_buffer(scratch, 0, out.append)") {
		t.Errorf("the sink must copy the view it is handed (§5.1.6):\n%s", got)
	}
}

// A schema whose worst case outgrows an EXPLICITLY configured max_message_size
// cannot fit the transport it was configured for, and that is a generate-time
// error rather than a runtime surprise (the same third leg as
// tests/matrix TestMaxMessageSizeCeiling, here for this backend's plumbing).
func TestPythonMaxMessageSizeCeilingRefusesOversizedSchema(t *testing.T) {
	const src = `
version: 1
messages:
  B:
    payload:
      a: { id: 0, type: array, items: { type: u32, count: 4 } }
`
	if _, err := (&Backend{}).Generate(schema(t, src), map[string]any{"max_message_size": 8}); err == nil {
		t.Error("a worst case above the configured max_message_size must fail generation")
	}
}

// TestPythonSchemaBoundedFieldsOptOutOfTheCap is generator#325: corelib-py
// applies its receiver caps per Decoder, so they also bound the fields whose
// schema declares a count:/maxlen: — which CORELIB_PLAN §6.2.1 forbids, since
// there the schema bound governs and its violation is INVALID, not
// LimitExceeded. The generator used to keep such messages decodable by raising
// the cap to the largest schema bound, which loosened it for the UNBOUNDED
// fields too — the protection §6.2.1 wants kept tight.
//
// on_schema_bound declares the count/length the schema puts on a field, and the
// corelib takes the cap off any field that declares one (§6.2.1). Generated code
// applies the caps itself besides, in the ELSE of the chain whose arms are
// exactly the schema-bounded ids -- so neither half can reach a bounded field.
func TestPythonSchemaBoundedFieldsOptOutOfTheCap(t *testing.T) {
	const src = `
version: 1
messages:
  M:
    payload:
      s:   { id: 1, type: string, maxlen: 8 }
      b:   { id: 2, type: blob, maxlen: 8 }
      arr: { id: 3, type: array, items: { type: u32, count: 4 } }
      sa:  { id: 4, type: array, items: { type: string, count: 3, maxlen: 6 } }
      dyn: { id: 5, type: string }
`
	mod := string(genPy(t, schema(t, src), map[string]any{
		"max_dyn_string_len":  4,
		"max_dyn_array_count": 2,
	})["message.py"])

	// The caps are NOT handed to the Decoder: it knows no schema, so a Decoder cap
	// bounds every field alike -- which is exactly what §6.2.1 forbids.
	for _, bad := range []string{"max_string_len=", "max_array_count=", "max_blob_len="} {
		if strings.Contains(mod, bad) {
			t.Errorf("a Decoder-level cap (%q) also binds schema-bounded fields, which §6.2.1 forbids:\n%s", bad, mod)
		}
	}
	// Instead each scope's cap sits in the ELSE of the chain whose arms are the
	// schema-bounded ids, so a bounded id can never reach it.
	for _, want := range []string{
		`                return 8  # s: schema maxlen`,
		`                return 4  # arr: schema count`,
		`            else:
                if fld.subtype == FixlenSubtype.STRING and fld.size > MAX_DYN_STRING_LEN:
                    raise SofaLimitError("string length %d exceeds max_string_len %d" % (fld.size, MAX_DYN_STRING_LEN))`,
	} {
		if !strings.Contains(mod, want) {
			t.Errorf("message.py missing %q:\n%s", want, mod)
		}
	}
	// Every array in this schema declares a count, so max_dyn_array_count is inert
	// and nothing is emitted for it.
	if strings.Contains(mod, "MAX_DYN_ARRAY_COUNT") {
		t.Errorf("a cap with no unbounded field of its kind must stay inert:\n%s", mod)
	}
	// sa's elements are schema-bounded by their own maxlen, so that scope carries
	// no cap at all -- an array scope dispatches by index, so one bound covers it.
	// Exactly one cap site survives: the root scope's else.
	if got := strings.Count(mod, "MAX_DYN_STRING_LEN)"); got != 1 {
		t.Errorf("expected exactly one cap site (the root scope's else), got %d:\n%s", got, mod)
	}
	// The cap stays at the configured number rather than being raised to 8.
	if !strings.Contains(mod, "MAX_DYN_STRING_LEN = 4") {
		t.Error("the configured cap must be emitted as configured, not raised to the largest schema maxlen")
	}
}

// TestPythonWrapperIndexCap: a DYNAMIC wrapper array's element index is bounded
// by the receiver cap, checked before the list is gap-filled (ARCHITECTURE §9.5,
// generator#387).
//
// A wrapper array carries no count header, so MAX_DYN_ARRAY_COUNT never reached
// it: its elements are keyed by an unbounded varint index and the gap-fill
// extends the list to fid + 1. Gap filling (§5.1) is why the INDEX and not the
// element count is the bound -- two delivered elements at id 0 and id 16383 are
// a 16384-slot list, so the index IS the length.
//
// The category is SofaLimitError, not SofaDecodeError: the bytes are well formed
// and the same message decodes under a looser cap (CORELIB_PLAN §6.2.1).
func TestPythonWrapperIndexCap(t *testing.T) {
	const src = `
version: 1
messages:
  M:
    payload:
      dstrs: { id: 0, type: array, items: { type: string } }
      dblbs: { id: 1, type: array, items: { type: blob } }
      dobjs: { id: 2, type: array, items: { type: struct, fields: { x: { id: 0, type: u32 } } } }
      bstrs: { id: 4, type: array, items: { type: string, count: 4 } }
`
	m := string(genPy(t, schema(t, src), map[string]any{})["message.py"])

	for _, want := range []string{
		// A VALUE element is bounded at the header, in on_field, one step ahead of
		// the payload that would be placed.
		`if fld.id >= MAX_DYN_ARRAY_COUNT:`,
		`raise SofaLimitError("dstrs: array index %d exceeds max_array_count %d" % (fld.id, MAX_DYN_ARRAY_COUNT))`,
		`raise SofaLimitError("dblbs: array index %d exceeds max_array_count %d" % (fld.id, MAX_DYN_ARRAY_COUNT))`,
		// An element that OPENS a scope has no on_field in front of it, so it is
		// bounded in on_sequence_begin instead -- where fid names the index.
		`if fid >= MAX_DYN_ARRAY_COUNT:`,
		`raise SofaLimitError("dobjs: array index %d exceeds max_array_count %d" % (fid, MAX_DYN_ARRAY_COUNT))`,
	} {
		if !strings.Contains(m, want) {
			t.Errorf("message.py missing wrapper index cap %q:\n%s", want, m)
		}
	}
	// The cap governs only what the schema left unbounded (§9.5): a count:N array
	// keeps its own bound and its own category.
	if !strings.Contains(m, `raise SofaDecodeError("bstrs: array index above schema capacity 4")`) {
		t.Errorf("a count:N wrapper array must keep its SofaDecodeError schema bound:\n%s", m)
	}
	if strings.Contains(m, `"bstrs: array index %d exceeds max_array_count`) {
		t.Errorf("a schema-bounded array must not also carry the receiver cap:\n%s", m)
	}
}

// pyProjectCfg is pyProject with the generator config the case under test needs
// (the receiver caps, above all), which pyProject fixes to the defaults.
func pyProjectCfg(t *testing.T, src string, cfg map[string]any) string {
	t.Helper()
	full := map[string]any{"emit": "project"}
	for k, v := range cfg {
		full[k] = v
	}
	dir := t.TempDir()
	for path, content := range genPy(t, schema(t, src), full) {
		if err := os.WriteFile(filepath.Join(dir, path), content, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// TestPythonSchemaBoundIsDeclaredNotCopied is the behaviour half of generator#406:
// the schema's count:/maxlen: is DECLARED to the corelib, in on_schema_bound, and
// the corelib is what applies it — one implementation of the rule, at the
// count/length header, for every route into it (CORELIB_PLAN §5.3.1).
//
// Declaring it is not only tidier, it is the fix for a live defect. A declared
// bound takes the receiver-side cap off the field (§6.2.1: a cap "MUST NOT be
// applied to a field the schema already bounds", and §6.3: LimitExceeded is
// "never raised for a field the schema bounds"). Generated code used to bound a
// native integer array in on_array_begin, which left that id in the ELSE of
// on_field's chain — where the array cap sat. A count:8 array under a configured
// max_dyn_array_count of 4 was therefore rejected at a wire count of 6, a message
// the schema plainly admits.
//
// The other half is what must NOT move with it: §7.3's tag test. A header whose
// wire type — or, for fixlen, whose subtype — is not the one the declared type
// maps to is a SKIPPED field, and the clause is explicit that against a schema
// bound it wins. on_schema_bound is told the id and the announced count/length
// and nothing else, so it cannot tell the two apart; the tag is therefore decided
// one hook earlier, in on_field, which declines such a field outright.
func TestPythonSchemaBoundIsDeclaredNotCopied(t *testing.T) {
	corelib := os.Getenv("SOFAB_PY_CORELIB")
	if corelib == "" {
		t.Skip("set SOFAB_PY_CORELIB to a corelib-py checkout")
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not found")
	}
	dir := pyProjectCfg(t, `
version: 1
messages:
  vec:
    payload:
      s:   { id: 0, type: string, maxlen: 32 }
      arr: { id: 1, type: array, items: { type: u32, count: 8 } }
      ds:  { id: 2, type: string }
      dyn: { id: 3, type: array, items: { type: u32 } }
`, map[string]any{"max_dyn_array_count": 4, "max_dyn_string_len": 8})

	// Wire helpers. A header is id<<3 | wire type; a fixlen word is
	// byte-length<<3 | subtype (string 2, blob 3); an array carries its count
	// straight after the header.
	str := func(id int, n int) []byte { // a fixlen STRING of n 'x' bytes
		out := append([]byte{byte(id<<3 | 2)}, varint(uint64(n<<3|2))...)
		return append(out, bytes.Repeat([]byte("x"), n)...)
	}
	blob := func(id int, n int) []byte { // ...and the same length as a BLOB
		out := append([]byte{byte(id<<3 | 2)}, varint(uint64(n<<3|3))...)
		return append(out, bytes.Repeat([]byte{1}, n)...)
	}
	uarr := func(id int, n int) []byte { // an unsigned array of n one-byte elements
		out := append([]byte{byte(id<<3 | 3)}, varint(uint64(n))...)
		for i := 0; i < n; i++ {
			out = append(out, 1)
		}
		return out
	}
	farr := func(id int, n int) []byte { // an fp32 fixlen ARRAY of n elements
		out := append([]byte{byte(id<<3 | 5)}, varint(uint64(n))...)
		out = append(out, varint(uint64(4<<3|0))...) // fixlen word: 4-byte fp32
		return append(out, make([]byte, 4*n)...)
	}

	for _, c := range []struct {
		what string
		wire []byte
		want string // "" = must decode; otherwise the error class expected
		keep string // a substring the decoded JSON must carry
	}{
		// --- the defect: a schema bound takes the cap off the field (§6.2.1) ---
		{"bounded array over the cap, inside the schema count", uarr(1, 6), "", `"arr": [1, 1, 1, 1, 1, 1]`},
		{"bounded string over the cap, inside the schema maxlen", str(0, 20), "", `"s": "xxxxxxxxxxxxxxxxxxxx"`},
		// ...while the bound itself still bites, as INVALID and not as a cap.
		{"bounded array past its schema count", uarr(1, 9), "SofaDecodeError", ""},
		{"bounded string past its schema maxlen", str(0, 40), "SofaDecodeError", ""},
		// --- and the cap still governs everything the schema left open (§6.3) ---
		{"unbounded array past the cap", uarr(3, 5), "SofaLimitError", ""},
		{"unbounded string past the cap", str(2, 20), "SofaLimitError", ""},
		// --- §7.3 wins over the bound: a mis-tagged header is a skipped field ---
		// Both of these are past the bound the schema puts on that id, so reading
		// the announced length as if it were the declared field's would make them
		// INVALID. They are not the declared field's value at all.
		{"blob at a bounded string's id, past its maxlen", blob(0, 40), "", `"s": ""`},
		{"fp32 array at a bounded u32 array's id, past its count", farr(1, 9), "", `"arr": []`},
	} {
		stderr, ok := pyHarnessTry(t, corelib, dir, "decode", c.wire)
		switch {
		case c.want == "" && !ok:
			t.Errorf("%s: must decode, got:\n%s", c.what, stderr)
		case c.want != "" && ok:
			t.Errorf("%s: must be rejected as %s, it decoded", c.what, c.want)
		case c.want != "" && !strings.Contains(stderr, c.want):
			t.Errorf("%s: must be %s, got:\n%s", c.what, c.want, stderr)
		case c.want == "" && c.keep != "":
			if got := string(pyHarness(t, corelib, dir, "decode", c.wire)); !strings.Contains(got, c.keep) {
				t.Errorf("%s: decoded message must carry %s, got: %s", c.what, c.keep, got)
			}
		}
	}
}

// varint renders n as a base-128 varint, the wire's only integer encoding.
func varint(n uint64) []byte {
	var out []byte
	for n >= 0x80 {
		out = append(out, byte(n)|0x80)
		n >>= 7
	}
	return append(out, byte(n))
}
