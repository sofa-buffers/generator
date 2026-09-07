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
//   2. A message that has ALREADY been refused goes on materialising its
//      containers for the rest of its bytes. The flags are sticky and surfaced
//      at the end, not an abort channel: the corelib cannot see them and keeps
//      delivering. A repeated matrix row id is the shape where that accumulates
//      inside one message -- a row that repeats APPENDS rather than replacing
//      (generator#509) -- so the tail below goes on growing a destination the
//      decode has already decided to throw away.
//
// Both budgets sit BETWEEN the two builds rather than merely above the guarded
// one: measured on corelib-rs 7599f9a, the refused message allocates 106 bytes
// with the guard and 32,106 without it, so the 8 KiB budget separates them and
// would not be met by a build that dropped the test.
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
}
