package java

import (
	"strings"
	"testing"
)

// holderYAML declares enums and bitfields in every place the schema language
// allows: $defs, inline on a message field, inline as an array element, inline
// on a union option and inside an inline struct. Big spans the whole 32-bit
// range an enum may declare (the validator caps it there), Wide has a flag at
// bit 63.
const holderYAML = `version: 1
$defs:
  enum:
    Level: { MIN: -128, LOW: -3, OFF: 0, ON: 1, MAX: 127 }
    Big:   { LOWEST: -2147483648, ZERO: 0, HIGHEST: 2147483647 }
  bitfield:
    Perm: { R: { pos: 0 }, W: { pos: 1, default: true, description: "write" }, X: { pos: 3 } }
    Wide: { LO: { pos: 0 }, TOP: { pos: 63 } }
messages:
  probe:
    payload:
      level: { id: 0, type: enum, enum: { $ref: "#/$defs/enum/Level" }, description: "the level" }
      big:   { id: 1, type: enum, enum: { $ref: "#/$defs/enum/Big" } }
      perm:  { id: 2, type: bitfield, bits: { $ref: "#/$defs/bitfield/Perm" } }
      wide:  { id: 3, type: bitfield, bits: { $ref: "#/$defs/bitfield/Wide" }, deprecated: true }
      mode:  { id: 4, type: enum, enum: { idle: 0, run: { value: 1, description: "running" } } }
      levels: { id: 5, type: array, items: { type: enum, count: 4, enum: { $ref: "#/$defs/enum/Level" } } }
      grid:  { id: 6, type: array, items: { type: array, count: 2, items: { type: bitfield, count: 2, bits: { a: { pos: 0 }, b: { pos: 5 } } } } }
      u:     { id: 7, type: union, oneof: { st: { id: 0, type: enum, enum: { none: 0, up: 1, down: -1 } }, n: { id: 1, type: u8 } } }
      s:     { id: 8, type: struct, fields: { fl: { id: 0, type: bitfield, bits: { k: { pos: 2 } } } } }
`

func holderFiles(t *testing.T) map[string]string {
	t.Helper()
	return genJavaFromYAML(t, holderYAML, map[string]any{})
}

const pkgDir = "src/main/java/message/"

// TestJavaEnumHolderConstants: one public final class per enum, uninstantiable,
// one `long` constant per value -- negative values, the 8-bit extremes, and
// the 32-bit extremes, the widest an enum may declare.
func TestJavaEnumHolderConstants(t *testing.T) {
	files := holderFiles(t)
	for file, wants := range map[string][]string{
		"Level.java": {
			"public final class Level {",
			"    private Level() {}",
			"    public static final long MIN = -128L;",
			"    public static final long LOW = -3L;",
			"    public static final long OFF = 0L;",
			"    public static final long ON = 1L;",
			"    public static final long MAX = 127L;",
			"may also hold an in-width value that names no",
		},
		"Big.java": {
			"    public static final long LOWEST = -2147483648L;",
			"    public static final long ZERO = 0L;",
			"    public static final long HIGHEST = 2147483647L;",
		},
	} {
		src, ok := files[pkgDir+file]
		if !ok {
			t.Errorf("no %s", file)
			continue
		}
		for _, w := range wants {
			if !strings.Contains(src, w) {
				t.Errorf("%s lacks %q:\n%s", file, w, src)
			}
		}
		if strings.Contains(src, "import ") {
			t.Errorf("%s imports something; a holder needs nothing:\n%s", file, src)
		}
	}
}

// TestJavaBitfieldHolderMasks: one constant per flag, as its MASK, up to the
// sign bit of the long carrier; a flag's description and default are its doc.
func TestJavaBitfieldHolderMasks(t *testing.T) {
	files := holderFiles(t)
	for file, wants := range map[string][]string{
		"Perm.java": {
			"public final class Perm {",
			"    private Perm() {}",
			"    public static final long R = 1L << 0;",
			"    /** write (default: true) */\n    public static final long W = 1L << 1;",
			"    public static final long X = 1L << 3;",
			"may also carry bits that no flag declares",
		},
		"Wide.java": {
			"    public static final long LO = 1L << 0;",
			"    public static final long TOP = 1L << 63;",
		},
	} {
		src, ok := files[pkgDir+file]
		if !ok {
			t.Errorf("no %s", file)
			continue
		}
		for _, w := range wants {
			if !strings.Contains(src, w) {
				t.Errorf("%s lacks %q:\n%s", file, w, src)
			}
		}
	}
}

// TestJavaInlineEnumBitfieldHolders: an inline declaration -- on a field, as an
// array element (also nested two deep), on a union option, inside an inline
// struct -- gets a holder named by its schema path, like an inline struct does.
func TestJavaInlineEnumBitfieldHolders(t *testing.T) {
	files := holderFiles(t)
	for file, want := range map[string]string{
		"Probe_Mode.java": "    /** running */\n    public static final long run = 1L;",
		"Probe_Grid.java": "    public static final long b = 1L << 5;",
		"Probe_U_St.java": "    public static final long down = -1L;",
		"Probe_S_Fl.java": "    public static final long k = 1L << 2;",
	} {
		src, ok := files[pkgDir+file]
		if !ok {
			var have []string
			for p := range files {
				have = append(have, p)
			}
			t.Errorf("no %s; have %v", file, have)
			continue
		}
		if !strings.Contains(src, want) {
			t.Errorf("%s lacks %q:\n%s", file, want, src)
		}
	}
}

// TestJavaEnumBitfieldFieldSeesItsHolder: the field stays a long (so an unnamed
// in-width value is still stored as received) and its javadoc links the holder
// -- for a scalar, an array of either kind at any depth, a union option and a
// struct member -- with @see ahead of @deprecated.
func TestJavaEnumBitfieldFieldSeesItsHolder(t *testing.T) {
	files := holderFiles(t)
	probe := files[pkgDir+"Probe.java"]
	for _, w := range []string{
		"    /**\n     * the level\n     *\n     * @see Level\n     */\n    public long level;",
		"    /** @see Big */\n    public long big;",
		"    /** @see Perm */\n    public long perm = 0x2L;",
		"     * @see Wide\n     *\n     * @deprecated",
		"    /** @see Probe_Mode */\n    public long mode;",
		"     * @see Level\n     */\n    public byte[] levels",
		"     * @see Probe_Grid\n     */\n    public List<",
	} {
		if !strings.Contains(probe, w) {
			t.Errorf("Probe.java lacks %q", w)
		}
	}
	if !strings.Contains(files[pkgDir+"Probe_U.java"], "/** @see Probe_U_St */\n    public long getSt()") {
		t.Errorf("Probe_U.java: the enum option's getter does not link its holder:\n%s", files[pkgDir+"Probe_U.java"])
	}
	if !strings.Contains(files[pkgDir+"Probe_S.java"], "/** @see Probe_S_Fl */\n    public long fl;") {
		t.Errorf("Probe_S.java: the bitfield member does not link its holder:\n%s", files[pkgDir+"Probe_S.java"])
	}
	// No other field gets a link.
	if n := strings.Count(probe, "@see"); n != 7 {
		t.Errorf("Probe.java has %d @see tags, want 7 (one per enum/bitfield field)", n)
	}
}

// collisionHolderYAML: holder and constant names that meet a name the
// generated package already uses -- a type the generated code names (Seq,
// List, the nested Decoder), a Java keyword as a constant, an inline path
// spelled like another message.
const collisionHolderYAML = `version: 1
$defs:
  enum:
    Seq: { class: 0, default: 1, new: 2, ok: 3 }
    Decoder: { A: 0 }
  bitfield:
    List: { int: { pos: 0 }, static: { pos: 1 }, fine: { pos: 2 } }
messages:
  m:
    payload:
      s: { id: 0, type: enum, enum: { $ref: "#/$defs/enum/Seq" } }
      d: { id: 1, type: enum, enum: { $ref: "#/$defs/enum/Decoder" } }
      l: { id: 2, type: bitfield, bits: { $ref: "#/$defs/bitfield/List" } }
      visitor: { id: 3, type: enum, enum: { off: 0, on: 1 } }
  m_visitor: { payload: { x: { id: 0, type: u8 } } }
`

// TestJavaHolderNamesFollowTheNamingContract: a holder takes the type channel
// -- escaped where the package already uses the name, and an inline path never
// meets a message of the same letters -- and a keyword constant is mangled with
// a trailing underscore. The field's @see names the escaped class.
func TestJavaHolderNamesFollowTheNamingContract(t *testing.T) {
	files := genJavaFromYAML(t, collisionHolderYAML, map[string]any{})
	for file, wants := range map[string][]string{
		"Seq_.java": {
			"public final class Seq_ {",
			"    private Seq_() {}",
			"    public static final long class_ = 0L;",
			"    public static final long default_ = 1L;",
			"    public static final long new_ = 2L;",
			"    public static final long ok = 3L;",
		},
		"Decoder_.java": {"public final class Decoder_ {"},
		"List_.java": {
			"public final class List_ {",
			"    public static final long int_ = 1L << 0;",
			"    public static final long static_ = 1L << 1;",
			"    public static final long fine = 1L << 2;",
		},
		"M_Visitor.java": {"public final class M_Visitor {"},
		"MVisitor.java":  {"public class MVisitor {"},
		"M.java":         {"/** @see Seq_ */\n    public long s;", "/** @see Decoder_ */\n    public long d;", "/** @see List_ */\n    public long l;", "/** @see M_Visitor */\n    public long visitor;"},
	} {
		src, ok := files[pkgDir+file]
		if !ok {
			t.Errorf("no %s", file)
			continue
		}
		for _, w := range wants {
			if !strings.Contains(src, w) {
				t.Errorf("%s lacks %q:\n%s", file, w, src)
			}
		}
	}
	for _, bad := range []string{"Seq.java", "List.java", "Decoder.java"} {
		if _, ok := files[pkgDir+bad]; ok {
			t.Errorf("%s exists: it would shadow a name the generated code uses", bad)
		}
	}
}
