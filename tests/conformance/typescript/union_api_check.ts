// The generated union API, driven directly (generator#608): what check_union.py
// cannot reach through the wire -- the accessors, select-if-not-held, what a
// real switch builds and releases, and clear(). Runs against the project
// generated from `check_union.py --emit-schema`, in every int64 mode (nothing
// below touches a 64-bit option, so the one file typechecks under all three).
import { Uni, Uni_U, Uni_V, Pick__DefaultN, Pick__DefaultT } from "./message.js";

let failed = 0;
function check(ok: boolean, what: string): void {
  if (!ok) {
    process.stderr.write(`FAIL: ${what}\n`);
    failed++;
  }
}
const hex = (b: Uint8Array): string => Buffer.from(b).toString("hex");

const m = new Uni();
// Fresh: default_id (pt) at its own default, nothing written.
check(m.u.which === Uni_U.PT_ID && m.u.hasPt() && m.u.pt.x === 7, "a fresh union holds pt at its default");
check(m.encode().length === 0, "a fresh message encodes to zero bytes");

// A getter of an option that is not held answers its default and stores nothing.
check(m.u.num === 5 && m.u.which === Uni_U.PT_ID, "the getter of a non-held option is its default");
m.u.box.z = 9;
check(m.u.which === Uni_U.PT_ID && !m.u.hasBox() && m.u.box.z === 3, "writing into a detached default selects nothing");

// A setter selects -- and a held option other than default_id is written even
// at its own default.
m.u.num = 5;
check(m.u.which === Uni_U.NUM_ID && m.u.hasNum() && !m.u.hasPt(), "the setter selects");
check(hex(m.encode()) === "06000507", `num at its own default is written: ${hex(m.encode())}`);
check(JSON.stringify(m.u.toJSON()) === '{"num":5}', "JSON carries the held option only");

// mutable<Opt>(): a real switch builds the option at ITS default...
const pt = m.u.mutablePt();
check(m.u.which === Uni_U.PT_ID && pt.x === 7 && pt.y === 0, "mutablePt selects pt at its default");
pt.y = 3;
// ...and a held option is continued, never reset.
check(m.u.mutablePt() === pt && pt.y === 3, "mutablePt of the held option keeps it");
// Away and back: a fresh default, and the object handed out earlier is left as
// it was rather than reset behind the caller's back.
m.u.s = "ab";
check(m.u.which === Uni_U.S_ID && m.u.s === "ab", "the string setter selects");
const pt2 = m.u.mutablePt();
check(pt2 !== pt && pt2.y === 0 && pt.y === 3, "a re-selected struct option is built fresh; the old object is untouched");
check(m.u.s === "", "the getter of a string option left behind is its default");

// A wrapper option, edited in place.
m.u.mutableStrs().push("ab");
check(m.u.hasStrs() && m.u.strs.length === 1 && m.u.strs[0] === "ab", "mutableStrs edits the held list");
check(hex(m.encode()) === "0626021261620707", `strs is written: ${hex(m.encode())}`);
m.u.strs = [];
check(hex(m.encode()) === "06260707", `an empty wrapper option is a present frame: ${hex(m.encode())}`);

// An empty blob option is written as the zero-length payload.
m.u.bl = new Uint8Array();
check(hex(m.encode()) === "062a0307", `an empty blob option is written: ${hex(m.encode())}`);

// fp32: its raw NaN bytes select the option at its default value; a new value
// drops them; another option held reads them as null.
m.u.fFp32Raw = new Uint8Array([1, 0, 0x80, 0x7f]);
check(m.u.which === Uni_U.F_ID && m.u.f === 1.5 && m.u.fFp32Raw !== null, "the raw bytes select f");
m.u.f = 2;
check(m.u.fFp32Raw === null, "a new value drops the raw bytes");
m.u.num = 1;
check(m.u.fFp32Raw === null && m.u.f === 1.5, "a non-held fp32 option reads as its default, no bytes");

// clear(): default_id at its own default.
m.u.clear();
check(m.u.which === Uni_U.PT_ID && m.u.isDefault() && m.u.pt.x === 7, "clear restores pt at its default");
check(m.encode().length === 0, "a cleared message encodes to zero bytes");

// Decode: the last option wins, and nothing of the one it replaced survives.
const d = Uni.decode(new Uint8Array([0x06, 0x00, 0x09, 0x0a, 0x0a, 0x78, 0x07]));
check(d.u.which === Uni_U.S_ID && d.u.s === "x" && d.u.num === 5, "decode: the last option wins");

// Fresh elements hold their own type's default_id: the $defs union split per
// default_id gives each site its own type.
check(new Uni_V().which === Uni_V.S_ID, "an element union holds its default_id");
check(new Pick__DefaultN().which === Pick__DefaultN.N_ID && m.pe.length === 0, "the element site's type holds n");
check(new Pick__DefaultT().which === Pick__DefaultT.T_ID && m.pf.t.k === 2, "the field site's type holds t");

if (failed > 0) {
  process.stderr.write(`union API: ${failed} check(s) failed\n`);
  process.exit(1);
}
process.stdout.write("union API: PASS\n");
