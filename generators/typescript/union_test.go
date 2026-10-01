package typescript

import (
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/sofa-buffers/generator/internal/ir"
)

// unionSrc has a union whose default option is a struct at a non-zero default
// and NOT its first option, one of every option kind the encode rule treats
// differently (scalar, string, compact array, wrapper array, blob, union, fp32,
// fp32 array, boolean), an option named after the union's own tag, a union whose
// default option is a scalar, an array of unions whose default option is a
// struct, a $defs union used with two default_ids (one type per default_id), a
// union of 64-bit options and a union with a 64-bit array option.
const unionSrc = `version: 1
$defs:
  union:
    Pick:
      n: { id: 0, type: u16, default: 6 }
      t: { id: 1, type: struct, fields: { k: { id: 0, type: u8, default: 2 } } }
messages:
  M:
    payload:
      u:
        id: 0
        type: union
        default_id: 2
        oneof:
          num:   { id: 0, type: u16, default: 5 }
          s:     { id: 1, type: string, maxlen: 8 }
          pt:    { id: 2, type: struct, fields: { x: { id: 0, type: i32, default: 7 }, y: { id: 1, type: i32 } } }
          arr:   { id: 3, type: array, items: { type: u16, count: 4 } }
          strs:  { id: 4, type: array, items: { type: string, count: 3, maxlen: 4 } }
          bl:    { id: 5, type: blob, maxlen: 4 }
          inner: { id: 6, type: union, default_id: 1, oneof: { a: { id: 0, type: u8 }, b: { id: 1, type: i8, default: -2 } } }
          f:     { id: 7, type: fp32, default: 1.5 }
          fa:    { id: 8, type: array, items: { type: fp32, count: 2 } }
          bo:    { id: 9, type: boolean, default: true }
          which: { id: 10, type: u8 }
      z: { id: 1, type: union, default_id: 0, oneof: { a: { id: 0, type: u8 }, b: { id: 1, type: u8, default: 3 } } }
      v: { id: 2, type: array, items: { type: union, count: 3, default_id: 1, oneof: { a: { id: 0, type: u8 }, p: { id: 1, type: struct, fields: { q: { id: 0, type: u8, default: 9 } } } } } }
      pf: { id: 4, type: union, default_id: 1, oneof: { $ref: "#/$defs/union/Pick" } }
      pe: { id: 5, type: array, items: { type: union, count: 3, oneof: { $ref: "#/$defs/union/Pick" } } }
      q: { id: 6, type: union, default_id: 1, oneof: { big: { id: 0, type: u64 }, sig: { id: 1, type: i64 } } }
      la: { id: 7, type: union, default_id: 0, oneof: { n: { id: 0, type: u8 }, ls: { id: 1, type: array, items: { type: u64, count: 2 } } } }
`

func genUnionMode(t *testing.T, mode string) string {
	t.Helper()
	return genTSWith(t, unionSrc, map[string]any{"int64": mode})
}

// tsClass returns the text of `export class <name> {` up to its closing brace.
func tsClass(t *testing.T, src, name string) string {
	t.Helper()
	i := strings.Index(src, "\nexport class "+name+" {")
	if i < 0 {
		t.Fatalf("no class %s in:\n%s", name, src)
	}
	rest := src[i+1:]
	if j := strings.Index(rest, "\n}\n"); j >= 0 {
		return rest[:j+3]
	}
	return rest
}

func mustContain(t *testing.T, what, src string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(src, w) {
			t.Errorf("%s: missing %q in:\n%s", what, w, src)
		}
	}
}

func mustNotContain(t *testing.T, what, src string, bads ...string) {
	t.Helper()
	for _, b := range bads {
		if strings.Contains(src, b) {
			t.Errorf("%s: must not contain %q in:\n%s", what, b, src)
		}
	}
}

// TestTSUnionStorage pins the representation: a tag plus ONE typed slot per
// option (never a shared slot, whose V8 representation would generalise to
// Tagged and box every double store), each initialised in the class body --
// scalars, strings and typed arrays at their default, object slots at null --
// with only default_id's object built.
func TestTSUnionStorage(t *testing.T) {
	u := tsClass(t, genUnionMode(t, "bigint"), "M_U")
	mustContain(t, "M_U storage", u,
		"  static readonly NUM_ID = 0;",
		"  static readonly PT_ID = 2;",
		"  static readonly WHICH_ID = 10;",
		"  private __which: number = 2;",
		"  private _num: number = 5;",
		`  private _s: string = "";`,
		"  private _pt: M_U_Pt | null = new M_U_Pt();", // default_id: built
		"  private _arr: Uint16Array = _E_Uint16Array;",
		"  private _strs: string[] | null = null;",
		"  private _bl: Uint8Array | null = null;",
		"  private _inner: M_U_Inner | null = null;",
		"  private _f: number = 1.5;",
		"  private _fFp32Raw: Uint8Array | null = null;",
		"  private _fa: Float32Array = _E_Float32Array;",
		"  private _bo: boolean = true;",
		// The option named after the tag takes the underscore; its slot never
		// lands on `__which`.
		"  private _which_: number = 0;",
		"  get which_(): number {",
		"  get which(): number {\n    return this.__which;\n  }",
	)
	// No product-type member survives: an option is reached through its accessor.
	mustNotContain(t, "M_U storage", u, "  num: number", "  pt: M_U_Pt =", "  s: string =")
	// A union whose default option is a scalar builds nothing at all.
	z := tsClass(t, genUnionMode(t, "bigint"), "M_Z")
	mustContain(t, "M_Z storage", z, "  private __which: number = 0;", "  private _a: number = 0;", "  private _b: number = 3;")
	mustNotContain(t, "M_Z storage", z, "__leave", "| null = new ")
}

// TestTSUnionAccessors pins the API and the select-if-not-held rule: a getter of
// an option not held answers its default and stores nothing; a setter selects;
// mutable<Opt>() switches only when another option is held and never resets a
// held one; a real switch releases what the option left behind held.
func TestTSUnionAccessors(t *testing.T) {
	u := tsClass(t, genUnionMode(t, "bigint"), "M_U")
	mustContain(t, "M_U accessors", u,
		"  get num(): number {\n    return this.__which === 0 ? this._num : 5;\n  }",
		"  set num(v: number) {\n    if (this.__which !== 0) {\n      this.__leave();\n      this.__which = 0;\n    }\n    this._num = v;\n  }",
		"  hasNum(): boolean {\n    return this.__which === 0;\n  }",
		// A getter of an object option not held builds a fresh default and keeps it
		// off the union.
		"  get pt(): M_U_Pt {\n    return this.__which === 2 ? this._pt! : new M_U_Pt();\n  }",
		// Select if not held.
		"  mutablePt(): M_U_Pt {\n    if (this.__which !== 2) {\n      this.__leave();\n      this.__which = 2;\n      this._pt = new M_U_Pt();\n    }\n    return this._pt!;\n  }",
		"  mutableStrs(): string[] {\n    if (this.__which !== 4) {",
		"  mutableInner(): M_U_Inner {",
		// fp32: a new value drops the NaN bytes; the bytes select at the default.
		"  set f(v: number) {\n    if (this.__which !== 7) {\n      this.__leave();\n      this.__which = 7;\n    }\n    this._f = v;\n    this._fFp32Raw = null;\n  }",
		"  get fFp32Raw(): Uint8Array | null {\n    return this.__which === 7 ? this._fFp32Raw : null;\n  }",
		"  set fFp32Raw(b: Uint8Array | null) {\n    if (this.__which !== 7) {\n      this.__leave();\n      this.__which = 7;\n      this._f = 1.5;\n    }\n    this._fFp32Raw = b;\n  }",
		// The release on a real switch: object slots to null, a string to "", a
		// typed array to the shared empty one; numbers and booleans hold nothing.
		"  private __leave(): void {\n    switch (this.__which) {\n      case 1:\n        this._s = \"\";\n        break;\n      case 2:\n        this._pt = null;\n        break;\n      case 3:\n        this._arr = _E_Uint16Array;\n        break;",
		"  clear(): void {\n    this.__leave();\n    this.__which = 2;\n    this._pt = new M_U_Pt();\n  }",
	)
	// A replaced-whole kind (scalar, string, blob, native array) has no mutable
	// accessor: nothing about it is edited in place.
	mustNotContain(t, "M_U accessors", u, "mutableNum(", "mutableS(", "mutableBl(", "mutableArr(", "mutableFa(")
	// A union with no option that holds anything selects with a bare tag store.
	z := tsClass(t, genUnionMode(t, "bigint"), "M_Z")
	mustContain(t, "M_Z accessors", z, "  set b(v: number) {\n    this.__which = 1;\n    this._b = v;\n  }")
}

// TestTSUnionEncodeArms pins MESSAGE_SPEC §4.2's encode rule: serialize is one
// switch over the held option; default_id's arm is the ordinary guarded write
// (a struct closes with the dropping end), every other arm is forced -- no
// ≠-default guard, the empty value as its empty payload, a struct/union/wrapper
// option closed with the KEEPING end.
func TestTSUnionEncodeArms(t *testing.T) {
	u := tsClass(t, genUnionMode(t, "bigint"), "M_U")
	mustContain(t, "M_U serialize", u,
		"  serialize(os: OStream): void {\n    switch (this.__which) {",
		// default_id: guarded, dropping end.
		"      case 2: {\n        os.writeSequenceBeginLazy(2);\n        this._pt!.serialize(os);\n        os.writeSequenceEnd();\n        break;\n      }",
		// forced scalar, string, array, blob.
		"      case 0: {\n        os.writeUnsigned(0, this._num);\n        break;\n      }",
		"      case 1: {\n        os.writeString(1, this._s);\n        break;\n      }",
		"      case 3: {\n        os.writeUnsignedArray(3, this._arr);\n        break;\n      }",
		"      case 5: {\n        os.writeBlob(5, this._bl!);\n        break;\n      }",
		"      case 8: {\n        os.writeFp32Array(8, this._fa);\n        break;\n      }",
		"      case 9: {\n        os.writeBoolean(9, this._bo);\n        break;\n      }",
		// forced fp32: no value guard, the raw bytes still re-emit a NaN.
		"      case 7: {\n        if (Number.isNaN(this._f) && this._fFp32Raw !== null && this._fFp32Raw.length === 4) {",
		// forced frames: the keeping end.
		"        os.writeSequenceBeginLazy(6);\n        this._inner!.serialize(os);\n        os.writeSequenceEndKeep();",
		"        os.writeSequenceEndKeep();\n        break;\n      }\n      case 5: {", // strs wrapper
		// isDefault agrees: default_id held AND at its own default.
		"  isDefault(): boolean {\n    return this.__which === 2 && this._pt!.isDefault();\n  }",
	)
	mustNotContain(t, "M_U serialize", u,
		"if (this._num !== 5)", `if (this._s !== "")`, "if (this._arr.length !== 0)",
		"if (this._bl!.length !== 0)", "if (this._bo !== true)", "if (this._f !== 1.5)")
	// A scalar default_id keeps its guard.
	z := tsClass(t, genUnionMode(t, "bigint"), "M_Z")
	mustContain(t, "M_Z serialize", z,
		"      case 0: {\n        if (this._a !== 0) {\n          os.writeUnsigned(0, this._a);\n        }",
		"      case 1: {\n        os.writeUnsigned(1, this._b);")
	// The union FIELD keeps its lazy frame and dropping end: it vanishes exactly
	// when serialize wrote nothing.
	m := tsClass(t, genUnionMode(t, "bigint"), "M")
	mustContain(t, "M serialize", m, "    os.writeSequenceBeginLazy(0);\n    this.u.serialize(os);\n    os.writeSequenceEnd();\n")
}

// TestTSUnionLongModes pins the 64-bit options per int64 mode: a Long scalar
// default_id keeps the (low, high) omission test, which must look at `high`
// too; a Long option's setter converts like a struct member's; and a Long[]
// option, whose setter converts (copies), is decoded by assigning an empty
// value and filling the one the union then holds.
func TestTSUnionLongModes(t *testing.T) {
	long := genUnionMode(t, "long")
	q := tsClass(t, long, "M_Q")
	mustContain(t, "M_Q long", q,
		"  private _big: Long = Long.ZERO;",
		"  set sig(v: Long | bigint | number) {\n    this.__which = 1;\n    this._sig = Long.fromValue(v);\n  }",
		"      case 0: {\n        os.writeUnsignedLong(0, this._big);\n        break;\n      }",
		"        if (!(this._sig.low === 0 && this._sig.high === 0)) {\n          os.writeSignedLong(1, this._sig);",
		"    return this.__which === 1 && this._sig.low === 0 && this._sig.high === 0;",
		`        return { "big": this._big.toString(false) };`,
	)
	la := tsClass(t, long, "M_La")
	mustContain(t, "M_La long", la,
		"  private _ls: Long[] | null = null;",
		"  set ls(v: readonly (Long | bigint | number)[]) {",
		"    this._ls = v.map(Long.fromValue);",
		"        os.writeUnsignedArrayLong(1, this._ls!);",
	)
	mustContain(t, "M_La decode", long,
		"this.o.la.ls = []; const _d = this.o.la.ls; this._a",
		"if (kind !== ArrayKind.Unsigned) break; if (count > 2)",
	)
	num := tsClass(t, genUnionMode(t, "number"), "M_Q")
	mustContain(t, "M_Q number", num,
		"  private _sig: number = 0;",
		"      case 0: {\n        os.writeUnsigned(0, this._big);",
		"        if (this._sig !== 0) {\n          os.writeSigned(1, this._sig);",
	)
	big := tsClass(t, genUnionMode(t, "bigint"), "M_Q")
	mustContain(t, "M_Q bigint", big,
		"  private _sig: bigint = 0n;",
		"        if (this._sig !== 0n) {\n          os.writeSigned(1, this._sig);",
	)
}

// TestTSUnionDecodeSwitch pins where decode selects an option (§7.4.1): every
// store goes through the option's accessor, and each sits behind the §7.3 gate
// of its kind -- a native array's after arrayBegin's kind test and bound (the
// header is routed by id alone), a string/blob at the completion store (never
// in fixlenBegin, which is routed by id alone too), a struct/union option by
// mutable<Opt>() at its sequence begin and on every path below it.
func TestTSUnionDecodeSwitch(t *testing.T) {
	mod := genUnionMode(t, "bigint")
	mustContain(t, "decode", mod,
		// scalar: after the width guard, through the setter.
		`throw new SofabError(SofabErrorCode.InvalidMsg, "num: value outside declared width u16"); this.o.u.num = _v; break; }`,
		// compact array: kind test, bound, THEN the setter.
		`case 3: { if (kind !== ArrayKind.Unsigned) break; if (count > 4) throw new SofabError(SofabErrorCode.InvalidMsg, "arr: array count above schema capacity 4"); const _d = new Uint16Array(count); this.o.u.arr = _d;`,
		`case 8: { if (kind !== ArrayKind.Fp32) break; if (count > 2)`,
		// string / blob: the completion store.
		"this.o.u.s = decodeUtf8(src, start, end);",
		"if (_p !== null) this.o.u.bl = _p;",
		// fp32: the setter selects and drops old bytes; a NaN's bytes follow.
		"case 7: { const _u = this.o.u; _u.f = v; if (Number.isNaN(v)) _u.fFp32Raw = fp32RawBytes(bits); break; }",
		// struct/union option: selected at its begin, reached through mutable below.
		"case 2: { this.o.u.mutablePt(); this._c = _M__Loc_u_pt; return true; }",
		"case 6: { this.o.u.mutableInner(); this._c = _M__Loc_u_inner; return true; }",
		"this.o.u.mutablePt().x = _v;",
		"this.o.u.mutableInner().b = _v;",
		// wrapper option: a fresh destination per occurrence (§7.4 replace).
		"case 4: { const _t: string[] = []; this.o.u.strs = _t; this._q",
		// an element union selects through the element's setters.
		"this.o.v[this._ix",
	)
	// fixlenBegin only bounds: no store and no select there.
	fb := mod[strings.Index(mod, "fixlenBegin(id: number"):]
	fb = fb[:strings.Index(fb, "\n  }\n")]
	mustNotContain(t, "fixlenBegin", fb, "this.o.u.s =", "this.o.u.bl =", "__which")
	// The visitor never reaches past the union's API.
	mustNotContain(t, "decode", mod, `this.o.u["_`, "this.o.u._")
}

// TestTSUnionGapFillPerType: an array of unions fills its gaps with a fresh
// instance of ITS element type, so the two types a $defs union splits into (one
// per default_id) each fill with their own default option.
func TestTSUnionGapFillPerType(t *testing.T) {
	mod := genUnionMode(t, "bigint")
	mustContain(t, "gap fill", mod,
		"const _Pick__DefaultN__Make = () => new Pick__DefaultN();",
		"new FramedSeq<Pick__DefaultN>(_t, _Pick__DefaultN__Make, 3,",
		"const _M_V__Make = () => new M_V();",
		"export class Pick__DefaultT {",
		"  private __which: number = 1;\n  private _n: number = 6;\n  private _t: Pick_T | null = new Pick_T();",
	)
	n := tsClass(t, mod, "Pick__DefaultN")
	mustContain(t, "Pick__DefaultN", n, "  private __which: number = 0;", "  private _t: Pick_T | null = null;")
}

// TestTSUnionJSON: exactly ONE member, the held option -- printed even for
// default_id at its default -- and fromJSON selects what it reads.
func TestTSUnionJSON(t *testing.T) {
	u := tsClass(t, genUnionMode(t, "bigint"), "M_U")
	mustContain(t, "M_U JSON", u,
		"  toJSON(): Record<string, unknown> {\n    switch (this.__which) {\n      case 0:\n        return { \"num\": this._num };",
		`        return { "arr": Array.from(this._arr) };`,
		`        return { "inner": this._inner!.toJSON() };`,
		"    }\n    return { \"pt\": this._pt!.toJSON() };\n  }",
		`    if (Object.prototype.hasOwnProperty.call(d, "num")) o.num = d["num"] as number;`,
		`    if (Object.prototype.hasOwnProperty.call(d, "pt")) o.pt = M_U_Pt.fromJSON(d["pt"] as Record<string, unknown>);`,
		`    if (Object.prototype.hasOwnProperty.call(d, "which")) o.which_ = d["which"] as number;`,
	)
}

// TestTSLazySequenceFramingUnion: the keeping closer appears unconditionally
// only in a forced union option's arm; a union FIELD and default_id's frame keep
// the dropping end.
func TestTSLazySequenceFramingUnion(t *testing.T) {
	u := tsClass(t, genUnionMode(t, "bigint"), "M_U")
	// inner (union) and strs (wrapper): forced. pt (struct): default_id.
	if got, want := strings.Count(u, "os.writeSequenceEndKeep();"), 2; got != want {
		t.Errorf("M_U: writeSequenceEndKeep() count = %d, want %d (the two forced sequence options)", got, want)
	}
	if got, want := strings.Count(u, "os.writeSequenceEnd();"), 1; got != want {
		t.Errorf("M_U: writeSequenceEnd() count = %d, want %d (default_id's struct frame)", got, want)
	}
}

// memberDecl matches one member declaration in a class body: a field, a method
// or one half of an accessor pair, with its modifiers.
var memberDecl = regexp.MustCompile(`(?m)^  (?:private |static |readonly )*(get |set )?([A-Za-z_$][\w$]*)\s*[(:=]`)

// duplicateMembers returns every name the class body declares twice: two fields
// or methods of one name, or a field/method beside an accessor of that name
// (TS2300 / TS2717 in tsc). A get/set pair is one member.
func duplicateMembers(cls string) []string {
	plain, acc := map[string]int{}, map[string]map[string]int{}
	for _, m := range memberDecl.FindAllStringSubmatch(cls, -1) {
		if m[1] != "" {
			if acc[m[2]] == nil {
				acc[m[2]] = map[string]int{}
			}
			acc[m[2]][m[1]]++
			continue
		}
		plain[m[2]]++
	}
	var dup []string
	for n, c := range plain {
		if c > 1 || acc[n] != nil {
			dup = append(dup, n)
		}
	}
	for n, k := range acc {
		for _, c := range k {
			if c > 1 {
				dup = append(dup, n)
			}
		}
	}
	sort.Strings(dup)
	return dup
}

// TestTSUnionPrivateMembersUnreachable: a union's private tag and release
// method are spelled `__which`/`__leave`, which no option slot (`_` + a member
// that starts with a letter) can be. An option `leave` beside a string option --
// so the release method is emitted -- declared `_leave` twice before.
func TestTSUnionPrivateMembersUnreachable(t *testing.T) {
	src := "version: 1\nmessages:\n  m:\n    payload:\n" +
		"      u: { id: 0, type: union, oneof: { leave: { id: 0, type: u8 }, which: { id: 1, type: u8 }," +
		" s: { id: 2, type: string, maxlen: 4 } } }\n"
	u := tsClass(t, genTSWith(t, src, map[string]any{}), "M_U")
	mustContain(t, "M_U", u,
		"  private __which: number = 0;",
		"  private _leave: number = 0;",
		"  private _which_: number = 0;",
		"  private __leave(): void {")
	if dup := duplicateMembers(u); len(dup) != 0 {
		t.Errorf("M_U declares %v twice:\n%s", dup, u)
	}
}

// shapeNames are option names that spell, or are spelled by, a member derived
// from another option: has<Opt>, mutable<Opt>, <opt>Fp32Raw, the bare prefixes
// (raw(`has`) is has(`fp32_raw`) unless `has` is escaped), digits after the
// prefix, Object's hasOwnProperty (has<Opt> of `own_property`), the union's own
// members and its private ones' stems. Their folds are distinct, as the
// validator requires of sibling options.
var shapeNames = []string{
	"x", "hasX", "mutableX", "xFp32Raw", "has", "mutable", "fp32_raw", "hasFp32Raw",
	"mutableFp32Raw", "has9", "mutable1", "has_y", "hash", "mutablez", "own_property",
	"hasOwnProperty", "which", "clear", "leave", "constructor", "toString", "g_fp32_raw",
}

// unionSpellings generates a union of the given options, all of one kind, and
// returns every option's member spellings, keyed by option name.
func unionSpellings(t *testing.T, names []string, kind string) (map[string][]string, string) {
	t.Helper()
	var opts []string
	for i, n := range names {
		opts = append(opts, fmt.Sprintf("%s: { id: %d, %s }", n, i, kind))
	}
	src := "version: 1\nmessages:\n  m:\n    payload:\n      u: { id: 0, type: union, oneof: { " + strings.Join(opts, ", ") + " } }\n"
	s := schema(t, src)
	g := &gen{schema: s}
	for key, nt := range s.Named {
		if nt.Category != ir.CatUnion {
			continue
		}
		got := map[string][]string{}
		for _, o := range g.unionShapeOf(key, nt).opts {
			got[o.orig.Name] = []string{o.prop, o.slot, o.has, o.mutable, o.raw, o.rawSlot}
		}
		return got, genTSWith(t, src, map[string]any{})
	}
	t.Fatal("no union in the schema")
	return nil, ""
}

// TestTSUnionMembersAreShapeBased: every member an option derives -- has<Opt>,
// mutable<Opt>, the fp32 raw-bytes pair -- depends on the option's own name
// alone. With every shape-colliding name present at once the spellings are
// pairwise distinct (and distinct from the class's fixed members), and each
// option alone gets exactly the spellings it gets among all the others: adding
// an option `hasX` never renames option `x`'s hasX().
func TestTSUnionMembersAreShapeBased(t *testing.T) {
	fixed := map[string]bool{"__which": true, "__leave": true}
	for _, set := range []map[string]bool{tsClassBody, tsMembers, tsObjectMembers, unionReserved} {
		for n := range set {
			fixed[n] = true
		}
	}
	for _, kind := range []string{
		"type: fp32", // prop, has, raw pair
		"type: struct, fields: { v: { id: 0, type: u8 } }", // prop, has, mutable
	} {
		all, out := unionSpellings(t, shapeNames, kind)
		seen := map[string]string{}
		for _, n := range shapeNames {
			for _, sp := range all[n] {
				if sp == "" {
					continue
				}
				if fixed[sp] {
					t.Errorf("%s: option %s derives the fixed member %s", kind, n, sp)
				}
				if prev, ok := seen[sp]; ok {
					t.Errorf("%s: options %s and %s both derive %s", kind, prev, n, sp)
				}
				seen[sp] = n
			}
			alone, _ := unionSpellings(t, []string{n}, kind)
			if !reflect.DeepEqual(alone[n], all[n]) {
				t.Errorf("%s: option %s alone is %v, among the others %v", kind, n, alone[n], all[n])
			}
		}
		if dup := duplicateMembers(tsClass(t, out, "M_U")); len(dup) != 0 {
			t.Errorf("%s: M_U declares %v twice", kind, dup)
		}
	}
	// The plain spellings, pinned: a derived member is never escaped by a
	// sibling; the option with a derived member's shape is.
	fp, _ := unionSpellings(t, shapeNames, "type: fp32")
	st, _ := unionSpellings(t, shapeNames, "type: struct, fields: { v: { id: 0, type: u8 } }")
	for _, c := range []struct{ got, want string }{
		{fp["x"][2], "hasX"}, {st["x"][3], "mutableX"}, {fp["x"][4], "xFp32Raw"}, {fp["x"][5], "_xFp32Raw"},
		{fp["hasX"][0], "hasX_"}, {fp["mutableX"][0], "mutableX_"}, {fp["xFp32Raw"][0], "xFp32Raw_"},
		{fp["has"][0], "has_"}, {fp["has"][4], "has_Fp32Raw"}, {fp["fp32_raw"][2], "hasFp32Raw"},
		{fp["hash"][0], "hash"}, {fp["mutablez"][0], "mutablez"}, {fp["has_y"][0], "has_y"},
		{fp["has9"][0], "has9_"}, {fp["own_property"][2], "hasOwnProperty__"},
		{fp["hasOwnProperty"][0], "hasOwnProperty_"}, {fp["which"][1], "_which_"}, {fp["leave"][1], "_leave"},
		{fp["toString"][4], "toString_Fp32Raw"},
	} {
		if c.got != c.want {
			t.Errorf("spelling %q, want %q", c.got, c.want)
		}
	}
}

// TestTSStructFp32CompanionIsShapeBased: a struct field's raw-bytes companion is
// its member plus "Fp32Raw", and a field that ENDS with "Fp32Raw" is escaped
// whether or not the field it would shadow exists, so neither renames the other.
func TestTSStructFp32CompanionIsShapeBased(t *testing.T) {
	both := genTSWith(t, "version: 1\nmessages:\n  m:\n    payload:\n"+
		"      f: { id: 0, type: fp32 }\n      fFp32Raw: { id: 1, type: u8 }\n      toString: { id: 2, type: fp32 }\n", map[string]any{})
	alone := genTSWith(t, "version: 1\nmessages:\n  m:\n    payload:\n"+
		"      fFp32Raw: { id: 1, type: u8 }\n", map[string]any{})
	mustContain(t, "f + fFp32Raw", both,
		"  fFp32Raw_: number = 0;",
		"  fFp32Raw: Uint8Array | null = null;",
		"this.o.fFp32Raw = Number.isNaN(v)",
		"  toString_Fp32Raw: Uint8Array | null = null;")
	mustContain(t, "fFp32Raw alone", alone, "  fFp32Raw_: number = 0;")
	if dup := duplicateMembers(tsClass(t, both, "M")); len(dup) != 0 {
		t.Errorf("M declares %v twice", dup)
	}
}

// TestTSFromJSONIgnoresInheritedKeys: fromJSON tests a key with an own-property
// check, so a field or option named after an Object.prototype member is not
// "present" in every JSON object.
func TestTSFromJSONIgnoresInheritedKeys(t *testing.T) {
	out := genTSWith(t, "version: 1\nmessages:\n  m:\n    payload:\n"+
		"      constructor: { id: 0, type: u8 }\n      u: { id: 1, type: union, oneof: { valueOf: { id: 0, type: u8 } } }\n", map[string]any{})
	mustContain(t, "fromJSON", out,
		`if (Object.prototype.hasOwnProperty.call(d, "constructor")) o.constructor_ = `,
		`if (Object.prototype.hasOwnProperty.call(d, "valueOf")) o.valueOf_ = `)
	if strings.Contains(out, " in d)") {
		t.Errorf("fromJSON still tests a key with `in`, which sees Object.prototype")
	}
}

func ptr(v int64) *int64 { return &v }
