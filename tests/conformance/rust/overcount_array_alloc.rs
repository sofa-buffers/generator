// An array whose count header was REJECTED must not go on collecting the count
// it was rejected for -- measured, not asserted.
//
// This is generator#508, and it is the reason nothing in this suite reached it
// before: `try_decode` returns the right verdict. A forged `count = 200000` on a
// field the schema bounds at four is `InvalidMsg` both before and after the fix,
// so every existing assertion passes either way. Only counting the bytes tells
// the two apart.
//
// What was wrong: `array_begin` arms the fill counter from the UNTRUSTED wire
// count before the schema bound is compared, and `self.inv` is a sticky flag
// surfaced at the END of the decode rather than an abort channel -- the corelib
// cannot see it and keeps delivering. So the reject set the flag, returned, and
// left a fill budget of `count` behind; all 200,000 elements were then pushed
// into a `Vec<u64>` the schema bounds at four. The amplification is
// `sizeof(elem)` over a one-byte varint, so a 1 MB message bought 8 MB of heap.
//
// The fix is that every rejecting branch DISARMS the fill (`self.afill = 0`),
// which routes the elements that follow into the `afill == 0` skip the store
// already opens with. Three shapes are pinned here, because they are three
// separate emitted arms and only the first was ever measured:
//
//   1. the integer count header,
//   2. the fixlen (fp) count header, reached through its own subtype, and
//   3. a MID-ARRAY element that trips its declared width, whose reject leaves the
//      rest of a legitimately-counted array still streaming in.
//
// Each of the three gets a counterweight: a legal message on the same arm must
// still decode, and still hold every value it announced. A decoder that disarmed
// too eagerly would pass every assertion above and be useless.
//
// Every budget here is written to sit BETWEEN the two builds, not merely above
// the fixed one -- a row whose budget also covers the pre-fix reading guards
// nothing. Rows 1 and 2 go from 2,097,152 bytes per decode to 0, and row 3 from
// 524,288 to 262,144 (#505's clamped reserve for a count that was legitimately
// accepted, which stays).
//SOFAB_IMPORT

use std::alloc::{GlobalAlloc, Layout, System};
use std::sync::atomic::{AtomicUsize, Ordering};

static ALLOCATED: AtomicUsize = AtomicUsize::new(0);

/// The system allocator with a running total of every byte handed out. Only
/// growth is counted, for the reason skipped_blob_alloc.rs gives.
struct Counting;

unsafe impl GlobalAlloc for Counting {
    unsafe fn alloc(&self, l: Layout) -> *mut u8 {
        ALLOCATED.fetch_add(l.size(), Ordering::Relaxed);
        System.alloc(l)
    }
    unsafe fn dealloc(&self, p: *mut u8, l: Layout) {
        System.dealloc(p, l)
    }
    unsafe fn realloc(&self, p: *mut u8, l: Layout, new: usize) -> *mut u8 {
        if new > l.size() {
            ALLOCATED.fetch_add(new - l.size(), Ordering::Relaxed);
        }
        System.realloc(p, l, new)
    }
}

#[global_allocator]
static A: Counting = Counting;

fn put_varint(out: &mut Vec<u8>, mut v: u64) {
    while v >= 0x80 {
        out.push((v as u8 & 0x7f) | 0x80);
        v >>= 7;
    }
    out.push(v as u8);
}

/// Bytes allocated per decode of `msg`, averaged over a few repetitions so a
/// one-off allocation elsewhere in the process cannot dominate.
fn per_decode(msg: &[u8]) -> (usize, String) {
    let reps = 8usize;
    let before = ALLOCATED.load(Ordering::Relaxed);
    let mut verdict = String::new();
    for _ in 0..reps {
        let r = Bnd::try_decode(msg);
        // Formatting allocates, so the verdict is only rendered after the count
        // is taken -- keep the loop body free of anything but the decode.
        std::hint::black_box(&r);
        if verdict.is_empty() {
            verdict = match &r {
                Ok(_) => "Ok".into(),
                Err(_) => "Err".into(),
            };
        }
    }
    ((ALLOCATED.load(Ordering::Relaxed) - before) / reps, verdict)
}

fn main() {
    // The schema's bounds. Both arrays are declared with four elements; the wire
    // announces fifty thousand times that.
    const BOUND: usize = 4;
    const N: u64 = 200_000;

    // 1. The integer count header. id 0, T_VARINTARRAY_UNSIGNED (wire type 3),
    //    N one-byte varint elements to match the announced count exactly -- so
    //    the message is well formed in every respect except the bound it breaks.
    let mut ints = Vec::new();
    put_varint(&mut ints, (0u64 << 3) | 3);
    put_varint(&mut ints, N);
    for _ in 0..N {
        ints.push(1u8);
    }

    // 2. The fixlen twin: id 1, T_FIXLENARRAY (wire type 5), the count word, the
    //    mandatory fixlen_word (width 8, subtype fp64) and then N doubles.
    let mut fps = Vec::new();
    put_varint(&mut fps, (1u64 << 3) | 5);
    put_varint(&mut fps, N);
    put_varint(&mut fps, (8 << 3) | 1);
    for _ in 0..N {
        fps.extend_from_slice(&1.0f64.to_le_bytes());
    }

    // 3. The mid-array width trip: id 2 is `array<u32>, count: 100000`, so a
    //    header of 100,000 is INSIDE the bound and is accepted. The first element
    //    is 2^32, which breaches the declared u32 width -- and the 99,999 that
    //    follow were still being collected after it.
    const WIDE: u64 = 100_000;
    let mut trip = Vec::new();
    put_varint(&mut trip, (2u64 << 3) | 3);
    put_varint(&mut trip, WIDE);
    put_varint(&mut trip, 4294967296);
    for _ in 0..WIDE - 1 {
        trip.push(1u8);
    }

    for (what, msg, elem, budget) in [
        ("an over-count integer array", &ints, 8usize, BOUND * 8 * 4),
        ("an over-count fixlen array", &fps, 8, BOUND * 8 * 4),
        // The width trip is measured against the count the header was ALLOWED,
        // not against the schema bound: #505 pre-sizes an accepted count, so the
        // reserve for 100,000 u32 (clamped at the receiver's ceiling of 65,536) is
        // legitimate and stays. What must not happen is filling it.
        //
        // The budget is that legitimate reserve plus a page, NOT twice it: filling
        // past the reserve costs one Vec doubling and nothing more, so the whole
        // pre-fix reading is 65_536 * 4 * 2 = 524,288 and a budget written that way
        // passes on the broken build too. It reads 262,144 after the fix and
        // 524,288 before it, and 266,240 is the only place to stand between them.
        ("a mid-array width trip", &trip, 4, 65_536 * 4 + 4_096),
    ] {
        let (per, verdict) = per_decode(msg);
        println!(
            "    {what}: {} wire bytes -> {verdict}, {per} bytes allocated per decode",
            msg.len()
        );
        assert_eq!(verdict, "Err", "{what} must be refused");
        assert!(
            per <= budget,
            "{what} kept filling after its reject: {per} bytes allocated from a \
             {}-byte message (budget {budget}); the announced count is worth {} bytes",
            msg.len(),
            N as usize * elem
        );
    }

    // The counterweight: a legal message still decodes, and still holds its
    // values. Disarming on a reject must not disarm on an accept. All THREE arms
    // above get one, because an over-eager disarm in any of them would otherwise
    // pass every assertion so far and be useless.
    let mut good = Vec::new();
    put_varint(&mut good, (0u64 << 3) | 3);
    put_varint(&mut good, BOUND as u64);
    for v in 0..BOUND as u64 {
        put_varint(&mut good, v + 10);
    }
    let m = Bnd::try_decode(&good).expect("a message inside the bound must decode");
    assert_eq!(m.arr, vec![10u64, 11, 12, 13], "the accept path must still fill");
    println!("    a legal {BOUND}-element array still decodes to {:?}", m.arr);

    // The fixlen arm's counterweight: a bound-EXACT fp64 array, four elements at
    // the declared capacity of four.
    let mut goodfx = Vec::new();
    put_varint(&mut goodfx, (1u64 << 3) | 5);
    put_varint(&mut goodfx, BOUND as u64);
    put_varint(&mut goodfx, (8 << 3) | 1);
    for v in 0..BOUND {
        goodfx.extend_from_slice(&(v as f64 + 0.5).to_le_bytes());
    }
    let mf = Bnd::try_decode(&goodfx).expect("a bound-exact fixlen array must decode");
    assert_eq!(
        mf.fx,
        vec![0.5f64, 1.5, 2.5, 3.5],
        "the fixlen accept path must still fill"
    );
    println!("    a bound-exact fixlen array still decodes to {:?}", mf.fx);

    // The width-guard arm's counterweight: a fully legal `wide` fill, every one of
    // its 100,000 elements inside the declared u32 width. This is the arm the
    // `arrayWidthGuard` rewrite touched, so it is the one where an over-eager
    // disarm would be cheapest to write and hardest to see.
    let mut goodwide = Vec::new();
    put_varint(&mut goodwide, (2u64 << 3) | 3);
    put_varint(&mut goodwide, WIDE);
    for v in 0..WIDE {
        put_varint(&mut goodwide, v % 4294967296);
    }
    let mw = Bnd::try_decode(&goodwide).expect("a legal wide array must decode");
    assert_eq!(mw.wide.len(), WIDE as usize, "the width-guarded arm must fill to the end");
    assert_eq!(mw.wide[0], 0, "first element");
    assert_eq!(
        mw.wide[WIDE as usize - 1],
        (WIDE - 1) as u32,
        "last element -- a disarm that fired mid-array would truncate here"
    );
    println!("    a legal {WIDE}-element u32 array still decodes to its full length");

    // ...and so must a message whose FIRST occurrence of the id was rejected: the
    // disarm is per-array, re-armed by the next array_begin, not a latch that
    // switches the field off for the rest of the message. (The verdict stays
    // InvalidMsg -- the flag is sticky -- so this is checked by re-decoding the
    // good message after a bad one, on the same shape.)
    let (_, first) = per_decode(&ints);
    assert_eq!(first, "Err");
    let m2 = Bnd::try_decode(&good).expect("a rejected decode must not poison the next one");
    assert_eq!(m2.arr, vec![10u64, 11, 12, 13]);
    println!("    a decoder that rejected one message still decodes the next");
}
