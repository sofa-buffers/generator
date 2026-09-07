// Once a receiver cap has been crossed, a count-less native array must stop
// collecting -- measured on both halves of what that buys, because a
// verdict-only assertion cannot see either one.
//
// This is the answer to generator#511, which asked whether the `if !self.lim`
// at an unbounded array's element store had become redundant since #508 made
// every rejecting branch disarm the fill counter. It has not, and this file is
// the reason it stays: #508 covers the field that TRIPPED the cap (its elements
// never reach the store, because `afill == 0` returns first), but three arms set
// the sticky flag WITHOUT disarming anything, because no fill is armed at them --
// a wrapper element's over-cap INDEX, a nested wrapper element's index, and an
// over-cap string/blob LENGTH. After any of those, a well-formed count-less
// native array later in the same message still arrives with its own fill armed,
// and the store is the only thing standing between it and the destination.
//
// Two properties, and the fallible surfaces show neither:
//
//   1. `decode` -- the infallible, best-effort entry point -- hands the caller
//      the message it filled, whatever the flags say. So "nothing observable
//      turns on the guard" is false on that surface: without it a refused
//      message comes back carrying the fields that arrived after the refusal.
//      `try_decode` and `Decoder::finish` are indifferent, and rows 3 and 4
//      pin that they stay indifferent -- both refuse with LimitExceeded and
//      neither hands back a message (`Decoder`'s is private and `finish`
//      consumes it), which is what makes this a question about `decode` alone.
//   2. A message that has ALREADY been refused goes on COLLECTING ELEMENTS into
//      a later count-less native array for the rest of its bytes. The flags are
//      sticky and surfaced at the end, not an abort channel: the corelib cannot
//      see them and keeps delivering. A repeated matrix row id is the shape
//      where that accumulates inside one message -- a row that repeats APPENDS
//      rather than replacing (generator#509) -- so the tail below goes on
//      growing a destination the decode has already decided to throw away.
//
//      Rows 5 and 6 keep every row at INDEX 0, which is all the store guard can
//      be measured on: it wraps the ELEMENT store alone. What a refused message
//      spends on the CONTAINERS -- the gap fill in array_begin/sequence_begin, a
//      wrapper element's own store, and the growth those drive -- is the block at
//      the bottom of this file (generator#518), measured at the highest index the
//      cap admits, where the two differ by six orders of magnitude.
//
// Both budgets sit BETWEEN the two builds rather than merely above the guarded
// one: measured on corelib-rs 7599f9a, the refused message allocates 8 bytes
// with the guard and 32,104 without it, so the 8 KiB budget separates them and
// would not be met by a build that dropped the test. (It read 104 until
// generator#518 refused the row HEADER too: the 96 bytes were the first row the
// gap fill pushed, which the store guard never covered.)
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

/// A 9-byte string at id 0, one over `max_dyn_string_len` (8): it sets the
/// sticky `lim` flag at the length word and arms no fill of its own.
fn breach() -> Vec<u8> {
    let mut w = Vec::new();
    put_varint(&mut w, (0u64 << 3) | 2);
    put_varint(&mut w, (9u64 << 3) | 2);
    w.extend_from_slice(b"ABCDEFGHI");
    w
}

/// `nums` (id 1), a count-less native array carrying n elements -- well formed,
/// inside the count cap, and delivered after the breach above.
fn nums(n: u64) -> Vec<u8> {
    let mut w = Vec::new();
    put_varint(&mut w, (1u64 << 3) | 3);
    put_varint(&mut w, n);
    for i in 0..n {
        put_varint(&mut w, i + 1);
    }
    w
}

/// One `mat` row (id 2) at index 0 with four u32 elements, repeated k times.
/// A repeated row id appends, so this is the count-less shape whose destination
/// can keep growing within a single message.
fn mat_rows(k: u64) -> Vec<u8> {
    let mut w = Vec::new();
    for _ in 0..k {
        put_varint(&mut w, (2u64 << 3) | 6);
        put_varint(&mut w, (0u64 << 3) | 3);
        put_varint(&mut w, 4);
        for i in 0..4u64 {
            put_varint(&mut w, i + 1);
        }
        put_varint(&mut w, 7);
    }
    w
}

/// Bytes allocated per `try_decode` of `msg`, averaged over a few repetitions so
/// a one-off allocation elsewhere in the process cannot dominate.
fn per_decode(msg: &[u8]) -> usize {
    let reps = 8usize;
    let before = ALLOCATED.load(Ordering::Relaxed);
    for _ in 0..reps {
        let r = Pl::try_decode(msg);
        std::hint::black_box(&r);
    }
    (ALLOCATED.load(Ordering::Relaxed) - before) / reps
}

fn main() {
    // 1. The observable half. A cap crossed by the STRING at id 0, then a legal
    //    count-less array at id 1: the array's elements must not be collected,
    //    because the decode has already refused the message.
    let mut refused = breach();
    refused.extend_from_slice(&nums(3));
    let got = Pl::decode(&refused);
    assert!(
        got.nums.is_empty(),
        "decode() after a crossed cap must not collect a later count-less array; got nums = {:?} \
         (the `if !self.lim` at the element store is gone -- generator#511)",
        got.nums
    );

    // 2. The counterweight. Without the breach the very same array is collected
    //    in full -- a decoder that dropped everything would satisfy row 1.
    let kept = Pl::decode(&nums(3));
    assert_eq!(
        kept.nums,
        vec![1u32, 2, 3],
        "an unrefused count-less array must still be collected"
    );

    // 3. The fallible surface is indifferent, and must stay so: the verdict is
    //    LimitExceeded either way, which is why the guard cannot be judged by it.
    assert!(
        matches!(
            Pl::try_decode(&refused),
            Err(DecodeError::Sofab(sofab::Error::LimitExceeded))
        ),
        "the refused message must be LimitExceeded"
    );
    assert!(Pl::try_decode(&nums(3)).is_ok(), "the control must decode");

    // 4. ...and so is the incremental one. `feed` refuses on the very chunk that
    //    crosses the cap, and `finish` consumes the decoder rather than handing
    //    back what it filled -- so no partially collected message escapes here
    //    whichever way the store is written.
    let mut d = Pl::decoder();
    let mut crossed = false;
    for b in &refused {
        if let Err(e) = d.feed(&[*b]) {
            assert!(
                matches!(e, sofab::Error::LimitExceeded),
                "streaming refusal must be LimitExceeded, got {:?}",
                e
            );
            crossed = true;
            break;
        }
    }
    assert!(crossed, "the incremental decoder must refuse the breach");
    assert!(
        d.finish().is_err(),
        "finish() must refuse after a crossed cap, never hand back the partial message"
    );

    // 5. The effort half. 2000 legal matrix rows behind the same 11-byte breach:
    //    with the guard the refused decode materialises none of them.
    const K: u64 = 2000;
    const BUDGET: usize = 8192;
    let mut tail = breach();
    tail.extend_from_slice(&mat_rows(K));
    let after_breach = per_decode(&tail);
    assert!(
        after_breach < BUDGET,
        "a message already refused must stop filling its containers: {} bytes/decode over {} rows \
         (budget {}) -- generator#511",
        after_breach,
        K,
        BUDGET
    );

    // ...and the counterweight again: the same rows with no breach in front of
    // them are collected, so the budget above measures the guard and not a
    // decoder that stopped working.
    let plain = mat_rows(K);
    let unrefused = per_decode(&plain);
    assert!(
        unrefused > BUDGET,
        "the same rows without a breach must still be collected: {} bytes/decode",
        unrefused
    );

    println!(
        "post-limit fill: refused {} bytes/decode, unrefused {} bytes/decode over {} rows",
        after_breach, unrefused, K
    );
    post_limit_containers();
}

// ---------------------------------------------------------------------------
// generator#518: the containers the guard above does NOT cover.
//
// `if !self.lim` wraps a native array's ELEMENT store. Everything else a refused
// message materialises -- the gap fill in array_begin/sequence_begin, a wrapper
// element's own store, and the container growth those two drive -- ran on for the
// rest of the message's bytes, so a refused decode allocated to the byte what an
// accepted one does. Measured on corelib-rs 7599f9a at max_dyn_array_count 65536,
// one element at index 65535 behind the same 11-byte breach, bytes/try_decode:
//
//                                       refused    refused    no breach
//                                        before      after
//     array<string> element            1,572,875         11    1,572,875
//     array<blob> element              1,572,875         11    1,572,875
//     array<array<string>> row         1,572,970          8    1,572,970
//     native mat row header            1,572,872          8    1,572,888
//     array<struct{u32}> element         262,152          8      262,152
//
// The refused column is a BUDGET, and the only budget these rows assert: it can
// only ever move down. The unrefused half asserts the VALUE instead -- that the
// element still arrives at index 65535 -- and deliberately not a byte count. What
// an ACCEPTED decode spends there is the generator#512 ceiling (cap x sizeof
// slot, verdict Ok), and a test that required it would pin that ceiling as a
// requirement, which ARCHITECTURE §9.5's generator#512 block declines to do.

/// The highest index the configured cap admits: the arm's own over-index reject
/// fires at MAX_DYN_ARRAY_COUNT, so this is the largest gap a WELL-FORMED message
/// can ask a receiver to fill, and the one the ceiling is measured at.
const IDX: u64 = 65535;

/// Bytes/decode a refused message is allowed to spend on the tail behind the
/// breach. Two orders of magnitude under the smallest unguarded reading above and
/// three above the guarded ones, so it separates the two builds rather than
/// merely bounding one.
const TAIL_BUDGET: usize = 4096;

/// `strs` (id 3): one string element at index `ix`.
fn str_at(ix: u64) -> Vec<u8> {
    let mut w = Vec::new();
    put_varint(&mut w, (3u64 << 3) | 6);
    put_varint(&mut w, (ix << 3) | 2);
    put_varint(&mut w, (3u64 << 3) | 2);
    w.extend_from_slice(b"abc");
    put_varint(&mut w, 7);
    w
}

/// `blbs` (id 4): one blob element at index `ix`.
fn blob_at(ix: u64) -> Vec<u8> {
    let mut w = Vec::new();
    put_varint(&mut w, (4u64 << 3) | 6);
    put_varint(&mut w, (ix << 3) | 2);
    put_varint(&mut w, (3u64 << 3) | 3);
    w.extend_from_slice(b"abc");
    put_varint(&mut w, 7);
    w
}

/// `objs` (id 5): one struct element at index `ix`, carrying k = 7.
fn obj_at(ix: u64) -> Vec<u8> {
    let mut w = Vec::new();
    put_varint(&mut w, (5u64 << 3) | 6);
    put_varint(&mut w, (ix << 3) | 6);
    put_varint(&mut w, 0);
    put_varint(&mut w, 7);
    put_varint(&mut w, 7);
    put_varint(&mut w, 7);
    w
}

/// `rows` (id 6): one wrapper row (an array<string>) at index `ix`, itself
/// carrying one element -- the two-level shape, where the outer sequence_begin
/// grows and the inner string element grows again.
fn row_at(ix: u64) -> Vec<u8> {
    let mut w = Vec::new();
    put_varint(&mut w, (6u64 << 3) | 6);
    put_varint(&mut w, (ix << 3) | 6);
    put_varint(&mut w, (0u64 << 3) | 2);
    put_varint(&mut w, (2u64 << 3) | 2);
    w.extend_from_slice(b"ab");
    put_varint(&mut w, 7);
    put_varint(&mut w, 7);
    w
}

/// `mat` (id 2): one NATIVE row header at index `ix` with four u32 elements. The
/// elements are already covered by the store guard; the header's gap fill is not.
fn mat_row_at(ix: u64) -> Vec<u8> {
    let mut w = Vec::new();
    put_varint(&mut w, (2u64 << 3) | 6);
    put_varint(&mut w, (ix << 3) | 3);
    put_varint(&mut w, 4);
    for i in 0..4u64 {
        put_varint(&mut w, i + 1);
    }
    put_varint(&mut w, 7);
    w
}

/// One container shape: what the refused message spends on it, that `decode`
/// hands back nothing for it, and that the same tail alone still decodes to the
/// element it carries.
fn shape(name: &str, tail: Vec<u8>, len_of: fn(&Pl) -> usize) {
    let mut refused = breach();
    refused.extend_from_slice(&tail);
    let cost = per_decode(&refused);
    assert!(
        cost < TAIL_BUDGET,
        "a message already refused must not materialise a {}: {} bytes/decode \
         (budget {}) -- generator#518",
        name,
        cost,
        TAIL_BUDGET
    );
    assert!(
        matches!(
            Pl::try_decode(&refused),
            Err(DecodeError::Sofab(sofab::Error::LimitExceeded))
        ),
        "the refused {} message must be LimitExceeded",
        name
    );
    let got = Pl::decode(&refused);
    assert_eq!(
        len_of(&got),
        0,
        "decode() after a crossed cap must hand back no {}",
        name
    );
    // The counterweight, on the VALUE and not on a byte count: without the breach
    // the very same element is placed at its index, so the budget above measures
    // the refusal and not a decoder that stopped collecting.
    let kept = Pl::decode(&tail);
    assert_eq!(
        len_of(&kept),
        IDX as usize + 1,
        "an unrefused {} must still be placed at its index",
        name
    );
    println!("  {:<30} {:>8} bytes/decode refused", name, cost);
}

fn post_limit_containers() {
    println!("post-limit containers (generator#518), one element at index {}:", IDX);
    shape("array<string> element", str_at(IDX), |m| m.strs.len());
    shape("array<blob> element", blob_at(IDX), |m| m.blbs.len());
    shape("array<struct> element", obj_at(IDX), |m| m.objs.len());
    shape("array<array<string>> row", row_at(IDX), |m| m.rows.len());
    shape("native row header", mat_row_at(IDX), |m| m.mat.len());
}
